package es

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func decodeMapping(t *testing.T, raw string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestCompareMappingNode(t *testing.T) {
	for _, tc := range []struct {
		name, current, desired, patch string
		kinds                         []string
	}{
		{
			name:    "new object with children",
			current: `{"dynamic":"strict"}`,
			desired: `{"dynamic":"strict","properties":{"data":{"type":"object","dynamic":false,"properties":{"name":{"type":"keyword"}}}}}`,
			patch:   `{"properties":{"data":{"type":"object","dynamic":false,"properties":{"name":{"type":"keyword"}}}}}`,
			kinds:   []string{"Add"},
		},
		{
			name:    "nested child preserves parent type",
			current: `{"properties":{"labels":{"type":"nested","properties":{"key":{"type":"keyword"}}}}}`,
			desired: `{"properties":{"labels":{"type":"nested","properties":{"key":{"type":"keyword"},"value":{"type":"keyword"}}}}}`,
			patch:   `{"properties":{"labels":{"type":"nested","properties":{"value":{"type":"keyword"}}}}}`,
			kinds:   []string{"Add"},
		},
		{
			name:    "implicit object and inherited dynamic",
			current: `{"dynamic":"strict","properties":{"data":{"properties":{"name":{"type":"keyword","index":true,"doc_values":true}}}}}`,
			desired: `{"dynamic":"strict","properties":{"data":{"type":"object","enabled":true,"dynamic":"strict","properties":{"name":{"type":"keyword"}}}}}`,
		},
		{
			name:    "boolean dynamic forms",
			current: `{"dynamic":"false","properties":{"data":{"type":"object","dynamic":"false"}}}`,
			desired: `{"dynamic":false,"properties":{"data":{"type":"object"}}}`,
		},
		{
			name:    "legacy field retained",
			current: `{"properties":{"archive":{"type":"object","enabled":false}}}`,
			desired: `{}`,
			kinds:   []string{"Legacy"},
		},
		{
			name:    "field type conflict",
			current: `{"properties":{"name":{"type":"keyword"}}}`,
			desired: `{"properties":{"name":{"type":"date"}}}`,
			kinds:   []string{"Conflict"},
		},
		{
			name:    "object versus nested",
			current: `{"properties":{"labels":{"type":"object"}}}`,
			desired: `{"properties":{"labels":{"type":"nested"}}}`,
			kinds:   []string{"Conflict"},
		},
		{
			name:    "root parameter",
			current: `{"dynamic":true}`,
			desired: `{"dynamic":"strict"}`,
			kinds:   []string{"Conflict"},
		},
		{
			name:    "enabled is not silently changed",
			current: `{"properties":{"data":{"type":"object","enabled":false}}}`,
			desired: `{"properties":{"data":{"type":"object"}}}`,
			kinds:   []string{"Conflict"},
		},
		{
			name:    "unknown parameter requires review",
			current: `{"properties":{"name":{"type":"keyword","ignore_above":256}}}`,
			desired: `{"properties":{"name":{"type":"keyword"}}}`,
			kinds:   []string{"Conflict"},
		},
		{
			name:    "multi-field requires migration",
			current: `{"properties":{"name":{"type":"text"}}}`,
			desired: `{"properties":{"name":{"type":"text","fields":{"raw":{"type":"keyword"}}}}}`,
			kinds:   []string{"Conflict"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var changes []MappingChange
			patch := compareMappingNode("", decodeMapping(t, tc.current), decodeMapping(t, tc.desired), true, true, &changes)
			var kinds []string
			for _, change := range changes {
				kinds = append(kinds, change.Kind)
			}
			if !reflect.DeepEqual(kinds, tc.kinds) {
				t.Fatalf("changes=%+v want kinds=%v", changes, tc.kinds)
			}
			var expected map[string]any
			if tc.patch != "" {
				expected = decodeMapping(t, tc.patch)
			}
			if !reflect.DeepEqual(patch, expected) {
				t.Fatalf("patch=%v want=%v", patch, expected)
			}
		})
	}
}

type mappingFixture struct {
	t        *testing.T
	mappings map[string]map[string]any
	aliases  map[string]string
	writes   []string
	hook     func(*http.Request) (*http.Response, error, bool)
}

func newMappingFixture(t *testing.T, resources ...string) (*Client, *mappingFixture) {
	t.Helper()
	f := &mappingFixture{t: t, mappings: map[string]map[string]any{}, aliases: map[string]string{}}
	for _, resource := range resources {
		index := resourceIndex(resource) + "-v7"
		f.aliases[resourceIndex(resource)] = index
		f.mappings[index] = mappingObject(decodeMapping(t, mappingForResource(resource))["mappings"])
	}
	return NewClientForTesting("http://es", &http.Client{Transport: roundTripFunc(f.request)}), f
}

func mappingResponse(value any) *http.Response {
	data, _ := json.Marshal(value)
	return response(http.StatusOK, string(data))
}

func (f *mappingFixture) request(req *http.Request) (*http.Response, error) {
	if f.hook != nil {
		if resp, err, handled := f.hook(req); handled {
			return resp, err
		}
	}
	if alias, ok := strings.CutPrefix(req.URL.Path, "/_alias/"); ok && req.Method == http.MethodGet {
		index, exists := f.aliases[alias]
		if !exists {
			return response(404, `{"error":"alias [`+alias+`] missing","status":404}`), nil
		}
		return mappingResponse(map[string]any{index: map[string]any{
			"aliases": map[string]any{alias: map[string]any{"is_write_index": true}},
		}}), nil
	}
	index := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/"), "/_mapping")
	current, exists := f.mappings[index]
	if !strings.HasSuffix(req.URL.Path, "/_mapping") || !exists {
		f.t.Fatalf("unexpected request: %s %s", req.Method, req.URL)
	}
	if req.Method == http.MethodPut {
		f.writes = append(f.writes, index)
		var patch map[string]any
		if err := json.NewDecoder(req.Body).Decode(&patch); err != nil {
			f.t.Fatal(err)
		}
		if len(patch) != 1 || patch["properties"] == nil {
			f.t.Fatalf("not a field-only patch: %v", patch)
		}
		mergeMapping(current, patch)
		return mappingResponse(map[string]bool{"acknowledged": true}), nil
	}
	if req.Method != http.MethodGet {
		f.t.Fatalf("unexpected method: %s", req.Method)
	}
	return mappingResponse(map[string]any{index: map[string]any{"mappings": current}}), nil
}

func mergeMapping(dst, patch map[string]any) {
	for key, value := range patch {
		if object := mappingObject(value); object != nil && mappingObject(dst[key]) != nil {
			mergeMapping(mappingObject(dst[key]), object)
		} else {
			dst[key] = value
		}
	}
}

func (f *mappingFixture) removeField(resource, name string) {
	delete(mappingObject(f.mappings[f.aliases[resourceIndex(resource)]]["properties"]), name)
}

func TestUpdateMappings(t *testing.T) {
	client, f := newMappingFixture(t, "user", "oldjob")
	f.removeField("user", "externalIdentity")
	oldjob := f.mappings[f.aliases[resourceIndex("oldjob")]]
	mappingObject(oldjob["properties"])["archive"] = map[string]any{"type": "object", "enabled": false}
	report, err := client.UpdateMappings(context.Background(), []string{"user", "oldjob", "user"}, false)
	if err != nil || !report.HasAdditions() || len(f.writes) != 0 || len(report.Results) != 2 || report.Results[0].Resource != "oldjob" {
		t.Fatalf("preview=%+v err=%v writes=%v", report, err, f.writes)
	}
	if report.Results[0].Changes[0].Kind != "Legacy" || report.Results[1].Index != "ebs-users-v7" {
		t.Fatalf("preview=%+v", report)
	}
	report, err = client.UpdateMappings(context.Background(), []string{"user", "oldjob"}, true)
	if err != nil || report.Results[1].Status != "Updated" || len(f.writes) != 1 {
		t.Fatalf("apply=%+v err=%v writes=%v", report, err, f.writes)
	}
	report, err = client.UpdateMappings(context.Background(), []string{"user", "oldjob"}, true)
	if err != nil || report.HasAdditions() || len(f.writes) != 1 || mappingObject(oldjob["properties"])["archive"] == nil {
		t.Fatalf("repeat=%+v err=%v writes=%v", report, err, f.writes)
	}
}

func TestUpdateMappingsSelectionAndPreflight(t *testing.T) {
	for _, tc := range []struct {
		name      string
		selection []string
		conflict  bool
		wantError bool
	}{
		{name: "all known resources"},
		{name: "explicit missing", selection: []string{"user", "oldjob"}, wantError: true},
		{name: "unknown resource", selection: []string{"users"}, wantError: true},
		{name: "explicit empty", selection: []string{}, wantError: true},
		{name: "late preflight conflict", selection: []string{"oldjob", "user"}, conflict: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, f := newMappingFixture(t, "user")
			if tc.conflict {
				index := "ebs-oldjobs-v7"
				f.aliases["ebs-oldjobs"] = index
				f.mappings[index] = mappingObject(decodeMapping(t, oldJobIndexMapping)["mappings"])
				f.removeField("oldjob", "kind")
				f.mappings["ebs-users-v7"]["dynamic"] = true
			}
			report, err := client.UpdateMappings(context.Background(), tc.selection, true)
			if (err != nil) != tc.wantError || len(f.writes) != 0 {
				t.Fatalf("report=%+v err=%v writes=%v", report, err, f.writes)
			}
			if tc.selection == nil && len(report.Results) != len(indices) {
				t.Fatalf("checked %d resources, want %d", len(report.Results), len(indices))
			}
			if tc.conflict && report.Results[0].Status != "NotAttempted" {
				t.Fatalf("report=%+v", report)
			}
		})
	}
}

func TestUpdateMappingsRecovery(t *testing.T) {
	for _, mode := range []string{"lost response", "unconfirmed", "concurrent addition", "alias changed", "alias changed after write", "confirmation read failed", "new conflict", "partial failure"} {
		t.Run(mode, func(t *testing.T) {
			client, f := newMappingFixture(t, "oldjob", "script", "user")
			for _, resource := range []string{"oldjob", "script", "user"} {
				f.removeField(resource, "kind")
			}
			aliasReads := 0
			f.hook = func(req *http.Request) (*http.Response, error, bool) {
				if req.URL.Path == "/_alias/ebs-oldjobs" {
					aliasReads++
					if aliasReads == 3 && mode == "alias changed after write" {
						f.aliases["ebs-oldjobs"] = "ebs-oldjobs-v8"
					}
					if aliasReads == 3 && mode == "confirmation read failed" {
						return response(503, `{"error":"read unavailable"}`), nil, true
					}
					if aliasReads == 2 {
						switch mode {
						case "alias changed":
							f.aliases["ebs-oldjobs"] = "ebs-oldjobs-v8"
						case "new conflict":
							f.mappings["ebs-oldjobs-v7"]["dynamic"] = true
						case "concurrent addition":
							mappingObject(f.mappings["ebs-oldjobs-v7"]["properties"])["kind"] = map[string]any{"type": "keyword"}
						}
					}
				}
				if req.Method != http.MethodPut {
					return nil, nil, false
				}
				if mode == "partial failure" && req.URL.Path == "/ebs-scripts-v7/_mapping" {
					return response(403, `{"error":"secret upstream diagnostic"}`), nil, true
				}
				if req.URL.Path == "/ebs-oldjobs-v7/_mapping" && (mode == "lost response" || mode == "unconfirmed") {
					f.writes = append(f.writes, "ebs-oldjobs-v7")
					if mode == "lost response" {
						mappingObject(f.mappings["ebs-oldjobs-v7"]["properties"])["kind"] = map[string]any{"type": "keyword"}
					}
					return nil, io.ErrUnexpectedEOF, true
				}
				return nil, nil, false
			}
			report, err := client.UpdateMappings(context.Background(), []string{"oldjob", "script", "user"}, true)
			switch mode {
			case "lost response", "concurrent addition":
				if err != nil || report.Results[2].Status != "Updated" {
					t.Fatalf("report=%+v err=%v", report, err)
				}
			case "partial failure":
				if err == nil || report.Results[0].Status != "Updated" || report.Results[1].Status != "Failed" || report.Results[2].Status != "NotAttempted" {
					t.Fatalf("report=%+v err=%v", report, err)
				}
				if strings.Contains(err.Error(), "secret") {
					t.Fatal("error leaked upstream body")
				}
			default:
				if err == nil || report.Results[1].Status != "NotAttempted" {
					t.Fatalf("report=%+v err=%v", report, err)
				}
				written := mode == "unconfirmed" || mode == "alias changed after write" || mode == "confirmation read failed"
				if written && (report.Results[0].Status != "Unconfirmed" || len(f.writes) != 1) {
					t.Fatalf("report=%+v writes=%v", report, f.writes)
				}
				if !written && len(f.writes) != 0 {
					t.Fatalf("wrote after a concurrent conflict: %v", f.writes)
				}
			}
		})
	}
}

func TestUpdateMappingsAliasAndReadErrors(t *testing.T) {
	for _, body := range []string{
		`{"a":{"aliases":{"ebs-users":{"is_write_index":true}}},"b":{"aliases":{"ebs-users":{"is_write_index":true}}}}`,
		`{"a":{"aliases":{"ebs-users":{"is_write_index":false}}}}`,
		`not JSON`,
	} {
		client, f := newMappingFixture(t, "user")
		f.hook = func(req *http.Request) (*http.Response, error, bool) { return response(200, body), nil, true }
		if _, err := client.UpdateMappings(context.Background(), []string{"user"}, true); err == nil || len(f.writes) != 0 {
			t.Fatalf("body=%s err=%v writes=%v", body, err, f.writes)
		}
	}
	client, f := newMappingFixture(t, "user")
	f.hook = func(req *http.Request) (*http.Response, error, bool) {
		return response(403, `{"error":"sensitive"}`), nil, true
	}
	if _, err := client.UpdateMappings(context.Background(), nil, true); err == nil {
		t.Fatal("authorization failure treated as missing aliases")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.hook = func(req *http.Request) (*http.Response, error, bool) {
		if !errors.Is(req.Context().Err(), context.Canceled) {
			t.Fatal("request is not bound to command context")
		}
		return nil, req.Context().Err(), true
	}
	if _, err := client.UpdateMappings(ctx, []string{"user"}, false); err == nil {
		t.Fatal("cancellation ignored")
	}
}
