package es

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestExists(t *testing.T) {
	query := map[string]interface{}{"term": map[string]interface{}{"data.status.phase": "Success"}}
	for _, tc := range []struct {
		name   string
		status int
		body   string
		exists bool
		err    bool
	}{
		{"match", 200, `{"hits":{"hits":[{"_id":"project/build"}]}}`, true, false},
		{"empty", 200, `{"hits":{"hits":[]}}`, false, false},
		{"unavailable", 503, `{"error":"unavailable"}`, false, true},
		{"timeout", 200, `{"timed_out":true,"hits":{"hits":[]}}`, false, true},
		{"partial", 200, `{"_shards":{"failed":1},"hits":{"hits":[]}}`, false, true},
		{"malformed", 200, `{`, false, true},
		{"missing hits", 200, `{}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			client := NewClientForTesting("http://elasticsearch", &http.Client{Transport: roundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					requests++
					if req.Method != http.MethodPost || req.URL.Path != "/ebs-builds/_search" ||
						req.URL.Query().Get("allow_partial_search_results") != "false" {
						t.Fatalf("unexpected request %s %s", req.Method, req.URL)
					}
					var body map[string]interface{}
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					want := map[string]interface{}{
						"query": query, "size": float64(1), "_source": false, "track_total_hits": false,
					}
					if !reflect.DeepEqual(body, want) {
						t.Fatalf("request body = %#v, want %#v", body, want)
					}
					return response(tc.status, tc.body), nil
				})})
			exists, err := client.Exists(context.Background(), "build", query)
			if exists != tc.exists || (err != nil) != tc.err {
				t.Fatalf("Exists = %t, %v; want %t, error=%t", exists, err, tc.exists, tc.err)
			}
			if tc.status == 503 && !IsStatus(err, 503) {
				t.Fatalf("HTTP status not preserved: %v", err)
			}
			if requests != 1 {
				t.Fatalf("requests = %d, want 1", requests)
			}
		})
	}
}

func TestCreateUsesCreateOnlyAndRefresh(t *testing.T) {
	var requestPath string
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requestPath = req.URL.RequestURI()
		if req.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", req.Method)
		}
		return response(http.StatusOK, `{"_seq_no":7,"_primary_term":3}`), nil
	})}
	client := NewClientForTesting("http://elasticsearch", httpClient)
	version, err := client.Create(context.Background(), "build", "project-a/build-a", Document{
		APIVersion: "ebs/v1", Kind: "Build", DocumentID: "project-a/build-a",
		Data: json.RawMessage(`{"kind":"Build"}`),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if version.SeqNo != 7 || version.PrimaryTerm != 3 {
		t.Fatalf("unexpected version: %#v", version)
	}
	if !strings.HasPrefix(requestPath, "/ebs-builds/_doc/project-a%2Fbuild-a?") {
		t.Fatalf("document path was not escaped: %s", requestPath)
	}
	if !strings.Contains(requestPath, "op_type=create") || !strings.Contains(requestPath, "refresh=wait_for") {
		t.Fatalf("missing create query options: %s", requestPath)
	}
}

func TestSearchPITSortsByCreationTimeAndDocumentID(t *testing.T) {
	searchAfter := []json.RawMessage{json.RawMessage(`"2026-09-05T08:00:00Z"`), json.RawMessage(`"project-a/build-a"`)}
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.Path != "/_search" {
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
		}
		var body struct {
			Sort        []map[string]map[string]string `json:"sort"`
			SearchAfter []json.RawMessage              `json:"search_after"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if len(body.Sort) != 2 || body.Sort[0]["metadata.creationTimestamp"]["order"] != "desc" || body.Sort[0]["metadata.creationTimestamp"]["missing"] != "_last" {
			t.Fatalf("unexpected primary sort: %#v", body.Sort)
		}
		if body.Sort[1]["documentID"]["order"] != "desc" {
			t.Fatalf("unexpected tie-break sort: %#v", body.Sort)
		}
		if len(body.SearchAfter) != 2 || string(body.SearchAfter[0]) != string(searchAfter[0]) || string(body.SearchAfter[1]) != string(searchAfter[1]) {
			t.Fatalf("unexpected search_after: %s", body.SearchAfter)
		}
		return response(http.StatusOK, `{"pit_id":"pit-next","hits":{"total":{"value":0},"hits":[]}}`), nil
	})}
	client := NewClientForTesting("http://elasticsearch", httpClient)
	result, err := client.SearchPIT(context.Background(), "pit", "1m", map[string]interface{}{"match_all": map[string]interface{}{}}, 10, searchAfter)
	if err != nil {
		t.Fatalf("search PIT: %v", err)
	}
	if result.PITID != "pit-next" {
		t.Fatalf("PIT ID = %q, want pit-next", result.PITID)
	}
}

func TestSourceFilterIsSentForFilteredReads(t *testing.T) {
	requests := 0
	client := NewClientForTesting("http://elasticsearch", &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		switch req.URL.EscapedPath() {
		case "/ebs-builds/_doc/project-a%2Fbuild-a":
			if req.URL.Query().Get("_source_includes") != "data.apiVersion,data.kind,data.metadata,data.status.phase" || req.URL.Query().Get("_source_excludes") != "data.spec" {
				t.Fatalf("unexpected GET source filter: %s", req.URL.RawQuery)
			}
			return response(http.StatusOK, `{"_id":"project-a/build-a","_seq_no":1,"_primary_term":1,"_source":{"data":{"apiVersion":"ebs/v1","kind":"Build","metadata":{"name":"build-a"}}}}`), nil
		case "/_search":
			var body struct {
				Source SourceFilter `json:"_source"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if strings.Join(body.Source.Includes, ",") != "data.apiVersion,data.kind,data.metadata,data.status.phase" || strings.Join(body.Source.Excludes, ",") != "data.spec" {
				t.Fatalf("unexpected search source filter: %#v", body.Source)
			}
			return response(http.StatusOK, `{"hits":{"total":{"value":0},"hits":[]}}`), nil
		default:
			t.Fatalf("unexpected path %s", req.URL.Path)
			return nil, nil
		}
	})})
	filter := SourceFilter{Includes: []string{"data.apiVersion", "data.kind", "data.metadata", "data.status.phase"}, Excludes: []string{"data.spec"}}
	if _, err := client.GetWithSourceFilter(context.Background(), "build", "project-a/build-a", filter); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SearchPITWithSourceFilter(context.Background(), "pit", "1m", map[string]interface{}{"match_all": map[string]interface{}{}}, 10, nil, filter); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
}

func TestCoordinationCASAndRealtimeGET(t *testing.T) {
	calls := 0
	c := NewClientForTesting("http://es", &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if !strings.HasPrefix(r.URL.Path, "/ebs-build-target-claims/_doc/") {
			t.Fatal(r.URL)
		}
		if r.Method == http.MethodGet {
			if r.URL.Query().Get("realtime") == "false" {
				t.Fatal("GET must be realtime")
			}
			return response(200, `{"_id":"target","_seq_no":7,"_primary_term":2,"_source":{}}`), nil
		}
		if r.URL.Query().Get("if_seq_no") != "7" || r.URL.Query().Get("if_primary_term") != "2" {
			t.Fatal("missing CAS version", r.URL)
		}
		return response(200, `{"_seq_no":8,"_primary_term":2}`), nil
	})})
	ctx := context.Background()
	if _, err := c.Get(ctx, "buildclaim", "target"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Update(ctx, "buildclaim", "target", Document{}, 7, 2); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "buildclaim", "target", 7, 2); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatal(calls)
	}
}

func TestWriteMissingVersionIsUnknown(t *testing.T) {
	c := NewClientForTesting("http://es", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response(201, `{}`), nil })})
	if _, err := c.Create(context.Background(), "buildclaim", "id", Document{}); err == nil {
		t.Fatal("malformed write response accepted")
	}
}

func TestDocumentTypesRoundTrip(t *testing.T) {
	base := Document{
		APIVersion: "ebs/v1",
		Kind:       "Job",
		DocumentID: "project/job",
		Metadata:   Metadata{Name: "job", Namespace: "project"},
		Data:       json.RawMessage(`{"metadata":{"name":"job"}}`),
	}
	for _, test := range []struct {
		resource string
		document any
		fields   map[string]any
	}{
		{"build", base, nil},
		{"oldjob", base, nil},
		{
			"user",
			IAMDocument{
				Document:         base,
				Credential:       json.RawMessage(`{"passwordHash":"hash"}`),
				ExternalIdentity: &ExternalIdentity{Issuer: "issuer", Subject: "subject", LinkedAt: "2026-11-07T00:00:00Z"},
			},
			map[string]any{
				"credential":       map[string]any{"passwordHash": "hash"},
				"externalIdentity": map[string]any{"issuer": "issuer", "subject": "subject", "linkedAt": "2026-11-07T00:00:00Z"},
			},
		},
	} {
		t.Run(test.resource, func(t *testing.T) {
			want := map[string]any{
				"apiVersion": base.APIVersion,
				"kind":       base.Kind,
				"documentID": base.DocumentID,
				"metadata":   map[string]any{"name": "job", "namespace": "project"},
				"data":       map[string]any{"metadata": map[string]any{"name": "job"}},
			}
			for name, value := range test.fields {
				want[name] = value
			}
			var stored json.RawMessage
			client := NewClientForTesting("http://es", &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodPut {
					if err := json.NewDecoder(req.Body).Decode(&stored); err != nil {
						t.Fatal(err)
					}
					var got map[string]any
					if err := json.Unmarshal(stored, &got); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("stored document = %v, want %v", got, want)
					}
					return response(201, `{"_seq_no":1,"_primary_term":1}`), nil
				}
				if req.Method == http.MethodGet {
					return response(200, `{"_seq_no":1,"_primary_term":1,"_source":`+string(stored)+`}`), nil
				}
				return response(200, `{"hits":{"total":{"value":1},"hits":[{"_source":`+string(stored)+`}]}}`), nil
			})})
			ctx := context.Background()
			if _, err := client.Create(ctx, test.resource, base.DocumentID, test.document); err != nil {
				t.Fatal(err)
			}
			hit, err := client.Get(ctx, test.resource, base.DocumentID)
			if err != nil {
				t.Fatal(err)
			}
			target := reflect.New(reflect.TypeOf(test.document))
			if err := hit.Decode(target.Interface()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(target.Elem().Interface(), test.document) {
				t.Fatalf("decoded document = %+v, want %+v", target.Elem().Interface(), test.document)
			}
			if _, err := client.Update(ctx, test.resource, base.DocumentID, target.Interface(), hit.SeqNo, hit.PrimaryTerm); err != nil {
				t.Fatal(err)
			}
			result, err := client.SearchPIT(ctx, "pit", "1m", map[string]any{"match_all": map[string]any{}}, 10, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Hits) != 1 {
				t.Fatalf("search hits = %d, want 1", len(result.Hits))
			}
			if err := result.Hits[0].Decode(target.Interface()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(target.Elem().Interface(), test.document) {
				t.Fatalf("search document = %+v, want %+v", target.Elem().Interface(), test.document)
			}
		})
	}
}
