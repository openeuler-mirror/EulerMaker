package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestFieldProjectionGETAndLIST(t *testing.T) {
	tests := []struct {
		name, path, body string
		want             map[string]interface{}
	}{
		{
			name: "GET includes and excludes nested fields",
			path: "/apis/ebs/v1/projects/demo/builds/build-a?includeFields=metadata.name,status.phase,status.stage&excludeFields=status.stage",
			body: `{"apiVersion":"ebs/v1","kind":"Build","metadata":{"name":"build-a","uid":"uid-a"},"spec":{"packages":["a"]},"status":{"phase":"Processing","stage":"build"}}`,
			want: map[string]interface{}{"apiVersion": "ebs/v1", "kind": "Build", "metadata": map[string]interface{}{"name": "build-a"}, "status": map[string]interface{}{"phase": "Processing"}},
		},
		{
			name: "LIST keeps pagination metadata and projects every item",
			path: "/apis/ebs/v1/projects/demo/jobs?limit=1&includeFields=metadata.name,status.phase",
			body: `{"apiVersion":"ebs/v1","kind":"JobList","metadata":{"continue":"next-page","remainingItemCount":5},"items":[{"metadata":{"name":"job-a","uid":"uid-a"},"status":{"phase":"Running","runner":"node-a"}},{"metadata":{"name":"job-b"},"status":{"phase":"Pending"}}]}`,
			want: map[string]interface{}{"apiVersion": "ebs/v1", "kind": "JobList", "metadata": map[string]interface{}{"continue": "next-page", "remainingItemCount": float64(5)}, "items": []interface{}{
				map[string]interface{}{"metadata": map[string]interface{}{"name": "job-a"}, "status": map[string]interface{}{"phase": "Running"}},
				map[string]interface{}{"metadata": map[string]interface{}{"name": "job-b"}, "status": map[string]interface{}{"phase": "Pending"}},
			}},
		},
		{
			name: "exclude only",
			path: "/apis/ebs/v1/configs/default?excludeFields=spec.content,metadata.annotations",
			body: `{"apiVersion":"ebs/v1","kind":"Config","metadata":{"name":"default","annotations":{"secret":"value"}},"spec":{"content":"large","visibility":"Public"}}`,
			want: map[string]interface{}{"apiVersion": "ebs/v1", "kind": "Config", "metadata": map[string]interface{}{"name": "default"}, "spec": map[string]interface{}{"visibility": "Public"}},
		},
		{
			name: "nested array fields",
			path: "/apis/ebs/v1/projects/demo?includeFields=spec.packageRepos.name",
			body: `{"apiVersion":"ebs/v1","kind":"Project","metadata":{"name":"demo"},"spec":{"packageRepos":[{"name":"gcc","url":"https://example.test/gcc"},{"name":"bash","url":"https://example.test/bash"}]}}`,
			want: map[string]interface{}{"apiVersion": "ebs/v1", "kind": "Project", "spec": map[string]interface{}{"packageRepos": []interface{}{map[string]interface{}{"name": "gcc"}, map[string]interface{}{"name": "bash"}}}},
		},
		{
			name: "empty list retains metadata",
			path: "/apis/ebs/v1/jobs?includeFields=metadata.name",
			body: `{"apiVersion":"ebs/v1","kind":"JobList","metadata":{"continue":""},"items":null}`,
			want: map[string]interface{}{"apiVersion": "ebs/v1", "kind": "JobList", "metadata": map[string]interface{}{"continue": ""}, "items": nil},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := withFieldProjection(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			}))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			var actual map[string]interface{}
			if err := json.Unmarshal(response.Body.Bytes(), &actual); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actual, tt.want) {
				t.Fatalf("response = %#v, want %#v", actual, tt.want)
			}
		})
	}
}

func TestFieldProjectionRejectsInvalidRequests(t *testing.T) {
	handler := withFieldProjection(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"ebs/v1","kind":"Job","metadata":{"name":"job-a"}}`))
	}))
	for _, path := range []string{
		"/apis/ebs/v1/jobs/job-a?includeFields=metadata..name",
		"/apis/ebs/v1/jobs?watch=true&includeFields=metadata.name",
		"/apis/ebs/v1/jobs?excludeFields=",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", path, response.Code)
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/apis/ebs/v1/jobs?includeFields=metadata.name", nil))
	if response.Code != http.StatusBadRequest {
		t.Errorf("POST status = %d, want 400", response.Code)
	}
}

func TestFieldProjectionPreservesUpstreamError(t *testing.T) {
	handler := withFieldProjection(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"Status","message":"missing"}`))
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/apis/ebs/v1/jobs/missing?includeFields=metadata.name", nil))
	if response.Code != http.StatusNotFound || response.Body.String() != `{"kind":"Status","message":"missing"}` {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}
