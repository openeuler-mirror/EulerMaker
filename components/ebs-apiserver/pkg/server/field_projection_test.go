package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"ebs-apiserver/pkg/storage/es"
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
			name: "exclude BuildInfo install statuses",
			path: "/apis/ebs/v1/projects/demo/buildinfos/build-a?excludeFields=status.specStatus.install",
			body: `{"apiVersion":"ebs/v1","kind":"BuildInfo","status":{"phase":"Processing","specStatus":{"build":{"gcc":{"status":"Succeeded"}},"install":{"gcc":{"status":"Failed"}},"dispatchCount":{"gcc":1}}}}`,
			want: map[string]interface{}{"apiVersion": "ebs/v1", "kind": "BuildInfo", "status": map[string]interface{}{"phase": "Processing", "specStatus": map[string]interface{}{"build": map[string]interface{}{"gcc": map[string]interface{}{"status": "Succeeded"}}, "dispatchCount": map[string]interface{}{"gcc": float64(1)}}}},
		},
		{
			name: "include BuildInfo detail fields",
			path: "/apis/ebs/v1/projects/demo/buildinfos/build-a?includeFields=status.phase,status.failedPackages,status.conditions,status.specStatus.build",
			body: `{"apiVersion":"ebs/v1","kind":"BuildInfo","metadata":{"name":"build-a"},"status":{"phase":"Completed","failedPackages":["gcc"],"conditions":[{"type":"PartialFailure"}],"specStatus":{"build":{"gcc":{"status":"Failed"}},"install":{"gcc":{"status":"Succeeded"}},"dispatchCount":{"gcc":1}},"dcg":{"gcc":{}}}}`,
			want: map[string]interface{}{"apiVersion": "ebs/v1", "kind": "BuildInfo", "status": map[string]interface{}{"phase": "Completed", "failedPackages": []interface{}{"gcc"}, "conditions": []interface{}{map[string]interface{}{"type": "PartialFailure"}}, "specStatus": map[string]interface{}{"build": map[string]interface{}{"gcc": map[string]interface{}{"status": "Failed"}}}}},
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

func TestFieldProjectionHandlesCompressedUpstream(t *testing.T) {
	const payload = `{"apiVersion":"ebs/v1","kind":"BuildInfo","status":{"phase":"Completed","specStatus":{"build":{"gcc":{"status":"Succeeded"}},"install":{"gcc":{"status":"Failed"}}}}}`
	for _, forceGzip := range []bool{false, true} {
		t.Run(map[bool]string{false: "disable intermediate compression", true: "decode unexpected gzip"}[forceGzip], func(t *testing.T) {
			handler := withFieldProjection(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if !forceGzip && r.Header.Get("Accept-Encoding") != "" {
					t.Fatalf("intermediate request still accepts compression: %q", r.Header.Get("Accept-Encoding"))
				}
				if !forceGzip {
					_, _ = w.Write([]byte(payload))
					return
				}
				w.Header().Set("Content-Encoding", "gzip")
				var compressed bytes.Buffer
				writer := gzip.NewWriter(&compressed)
				if _, err := writer.Write([]byte(payload)); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				_, _ = w.Write(compressed.Bytes())
			}))
			request := httptest.NewRequest(http.MethodGet, "/apis/ebs/v1/projects/demo/buildinfos/build-a?includeFields=status.phase,status.specStatus.build", nil)
			request.Header.Set("Accept-Encoding", "gzip")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK || response.Header().Get("Content-Encoding") != "" {
				t.Fatalf("status=%d encoding=%q body=%s", response.Code, response.Header().Get("Content-Encoding"), response.Body.String())
			}
			var actual map[string]interface{}
			if err := json.Unmarshal(response.Body.Bytes(), &actual); err != nil {
				t.Fatal(err)
			}
			status := actual["status"].(map[string]interface{})
			if status["phase"] != "Completed" || status["specStatus"].(map[string]interface{})["install"] != nil {
				t.Fatalf("unexpected projected status: %#v", status)
			}
		})
	}
}

func TestFieldProjectionSourceFilter(t *testing.T) {
	tests := []struct {
		path string
		want es.SourceFilter
		push bool
	}{
		{path: "/apis/ebs/v1/builds?excludeFields=spec", want: es.SourceFilter{Excludes: []string{"data.spec"}}, push: true},
		{path: "/apis/ebs/v1/projects/demo/buildinfos/build-a?excludeFields=status.specStatus.install", want: es.SourceFilter{Excludes: []string{"data.status.specStatus.install"}}, push: true},
		{path: "/apis/ebs/v1/builds?includeFields=metadata.name,status.phase,status.stage", want: es.SourceFilter{Includes: []string{"data.apiVersion", "data.kind", "data.metadata", "data.status.phase", "data.status.stage"}}, push: true},
		{path: "/apis/ebs/v1/projects/demo/buildinfos/build-a?includeFields=status.phase,status.failedPackages,status.conditions,status.specStatus.build", want: es.SourceFilter{Includes: []string{"data.apiVersion", "data.kind", "data.metadata", "data.status.conditions", "data.status.failedPackages", "data.status.phase", "data.status.specStatus.build"}}, push: true},
		{path: "/apis/ebs/v1/projects?includeFields=spec.packageRepos.name", push: false},
		{path: "/apis/ebs/v1/projects?excludeFields=spec.packageRepos.name", push: false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			projection, _, err := parseFieldProjection(req)
			if err != nil {
				t.Fatal(err)
			}
			got, push := projection.sourceFilter()
			if push != tt.push || (push && !reflect.DeepEqual(got, tt.want)) {
				t.Fatalf("filter = %#v, push = %t; want %#v, %t", got, push, tt.want, tt.push)
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
