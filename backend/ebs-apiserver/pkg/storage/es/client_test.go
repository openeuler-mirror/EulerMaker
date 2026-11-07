package es

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

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

func TestAPIKeyAuthenticatesPingAndAliasCheck(t *testing.T) {
	var methods []string
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		methods = append(methods, req.Method)
		if got := req.Header.Get("Authorization"); got != "ApiKey encoded-key" {
			t.Errorf("Authorization = %q, want API key", got)
		}
		return response(http.StatusOK, `{}`), nil
	})}
	client := NewClientForTesting("http://elasticsearch", httpClient)
	client.apiKey = "encoded-key"
	if err := client.ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := client.ensureResourceIndices([]string{"project"}); err != nil {
		t.Fatalf("ensure index: %v", err)
	}
	if len(methods) != 2 || methods[0] != http.MethodGet || methods[1] != http.MethodHead {
		t.Fatalf("request methods = %v, want GET and HEAD", methods)
	}
}

func TestEnsureIndicesDoesNotCreateWhenAliasExists(t *testing.T) {
	putRequests := 0
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodHead && strings.HasPrefix(req.URL.Path, "/_alias/") {
			return response(http.StatusOK, `{}`), nil
		}
		if req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/_alias/") {
			alias := strings.TrimPrefix(req.URL.Path, "/_alias/")
			return response(http.StatusOK, `{"index":{"aliases":{"`+alias+`":{"is_write_index":true}}}}`), nil
		}
		if req.Method == http.MethodPut {
			putRequests++
		}
		return response(http.StatusInternalServerError, `{}`), nil
	})}
	client := NewClientForTesting("http://elasticsearch", httpClient)
	if err := client.ensureIndices(); err != nil {
		t.Fatalf("ensure indices: %v", err)
	}
	if putRequests != 0 {
		t.Fatalf("created %d indices despite existing aliases", putRequests)
	}
}

func TestEnsureIAMIndices(t *testing.T) {
	created := map[string]bool{}
	mappings := map[string]string{}
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodHead {
			return response(http.StatusNotFound, ``), nil
		}
		if req.Method == http.MethodPut {
			index := strings.TrimPrefix(req.URL.Path, "/")
			created[index] = true
			body, _ := io.ReadAll(req.Body)
			mappings[index] = string(body)
			return response(http.StatusOK, `{"acknowledged":true}`), nil
		}
		if req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/_mapping") {
			index := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/"), "/_mapping") + "-v1"
			return response(http.StatusOK, `{"`+index+`":`+mappings[index]+`}`), nil
		}
		t.Fatalf("unexpected method %s", req.Method)
		return nil, nil
	})}
	client := NewClientForTesting("http://elasticsearch", httpClient)
	if err := client.EnsureIAMIndices(); err != nil {
		t.Fatalf("ensure IAM indices: %v", err)
	}
	for _, index := range []string{"ebs-users-v1", "ebs-machineaccounts-v1", "ebs-oauth-identity-claims-v1"} {
		if !created[index] {
			t.Errorf("index %s was not created", index)
		}
	}
	if created["ebs-projects-v1"] {
		t.Fatalf("core index unexpectedly created: %#v", created)
	}
	for index, mapping := range mappings {
		if index == "ebs-oauth-identity-claims-v1" {
			continue
		}
		if !strings.Contains(mapping, `"credential"`) {
			t.Errorf("IAM index %s does not contain credential mapping: %s", index, mapping)
		}
		if strings.Contains(mapping, `"phase"`) || strings.Contains(mapping, `"stage"`) {
			t.Errorf("IAM index %s unexpectedly maps status fields: %s", index, mapping)
		}
	}
}

func TestHTTPErrorStatus(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return response(http.StatusNotFound, "missing"), nil
	})}
	client := NewClientForTesting("http://elasticsearch", httpClient)
	_, err := client.Get(context.Background(), "project", "missing")
	if !IsStatus(err, http.StatusNotFound) {
		t.Fatalf("expected 404 HTTPError, got %v", err)
	}
}

func TestCoordinationAliasValidation(t *testing.T) {
	for _, body := range []string{`{}`, `{"a":{"aliases":{}},"b":{"aliases":{}}}`, `{"a":{"aliases":{"ebs-build-target-claims":{"is_write_index":false}}}}`} {
		c := NewClientForTesting("http://es", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response(200, body), nil })})
		if err := c.validateCoordinationAlias("ebs-build-target-claims"); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

func TestCoordinationIndexConcurrentInitialization(t *testing.T) {
	c := NewClientForTesting("http://es", &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.Method {
		case http.MethodHead:
			return response(404, ""), nil
		case http.MethodPut:
			return response(400, `{"error":{"type":"resource_already_exists_exception"}}`), nil
		default:
			return response(200, `{"ebs-build-target-claims-v1":{"aliases":{"ebs-build-target-claims":{"is_write_index":true}}}}`), nil
		}
	})})
	if err := c.ensureResourceIndices([]string{"buildclaim"}); err != nil {
		t.Fatal(err)
	}
}
