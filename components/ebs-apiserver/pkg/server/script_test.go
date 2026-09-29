package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/storage/es"
)

// Exercise routing, validation, resourceVersion and generation through the real
// ES store. The transport replaces only Elasticsearch.
func TestScriptHTTP(t *testing.T) {
	var document json.RawMessage
	var seq int64
	writes := 0
	client := es.NewClientForTesting("http://es", &http.Client{Transport: confTransport(func(r *http.Request) (*http.Response, error) {
		code := 200
		var body any
		if r.URL.Path == "/ebs-scripts/_pit" || r.URL.Path == "/_pit" || r.URL.Path == "/_search" {
			body = map[string]any{"id": "pit"}
			if r.URL.Path == "/_search" {
				body = map[string]any{"hits": map[string]any{"total": map[string]any{"value": 1}, "hits": []any{map[string]any{"_id": "rpmbuild", "_seq_no": seq, "_primary_term": 1, "_source": document}}}}
			}
			data, _ := json.Marshal(body)
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data))}, nil
		}
		if r.URL.Path != "/ebs-scripts/_doc/rpmbuild" {
			t.Fatalf("unexpected ES path %s", r.URL.Path)
		}
		switch r.Method {
		case "GET":
			if document == nil {
				code = 404
				body = map[string]any{"found": false}
			} else {
				body = map[string]any{"_id": "rpmbuild", "_seq_no": seq, "_primary_term": 1, "_source": document}
			}
		case "PUT":
			if r.URL.Query().Get("op_type") == "create" && document != nil {
				code = 409
				body = map[string]any{"error": "exists"}
				break
			}
			document, _ = io.ReadAll(r.Body)
			writes++
			seq++
			body = map[string]any{"_seq_no": seq, "_primary_term": 1}
		default:
			t.Fatalf("unexpected ES method %s", r.Method)
		}
		data, _ := json.Marshal(body)
		return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data))}, nil
	})})
	handler, err := configScriptTestHandler(map[string]rest.Storage{"scripts": newScriptStore(client)})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body, content string, want int) *ebsv1.Script {
		t.Helper()
		req := httptest.NewRequest(method, "/apis/ebs/v1"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", content)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, rec.Code, want, rec.Body.String())
		}
		out := new(ebsv1.Script)
		if want < 300 {
			if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	call("GET", "/scripts/rpmbuild", "", "application/json", 404)
	obj := call("POST", "/scripts", `{"apiVersion":"ebs/v1","kind":"Script","metadata":{"name":"rpmbuild"},"spec":{"content":"#!/bin/sh\necho first\n"}}`, "application/json", 201)
	if obj.ResourceVersion == "" || obj.Generation != 1 || obj.UID == "" {
		t.Fatalf("missing metadata %+v", obj.ObjectMeta)
	}
	original := obj.DeepCopy()
	listResponse := httptest.NewRecorder()
	handler.ServeHTTP(listResponse, httptest.NewRequest("GET", "/apis/ebs/v1/scripts?limit=100", nil))
	var list ebsv1.ScriptList
	if err := json.Unmarshal(listResponse.Body.Bytes(), &list); err != nil || listResponse.Code != 200 || list.Kind != "ScriptList" || len(list.Items) != 1 || list.Items[0].Name != "rpmbuild" {
		t.Fatalf("invalid list: %s, error=%v", listResponse.Body.String(), err)
	}
	call("POST", "/scripts", `{"metadata":{"name":"rpmbuild"},"spec":{"content":"#!/bin/sh\necho first\n"}}`, "application/json", 409)
	call("PUT", "/scripts/rpmbuild", `{"metadata":{"name":"rpmbuild"},"spec":{"content":"#!/bin/sh\necho first\n"}}`, "application/json", 409)
	obj.Spec.Content = "#!/bin/sh\necho updated\n"
	data, _ := json.Marshal(obj)
	obj = call("PUT", "/scripts/rpmbuild", string(data), "application/json", 200)
	if obj.Generation != 2 {
		t.Fatalf("generation=%d", obj.Generation)
	}
	data, _ = json.Marshal(original)
	call("PUT", "/scripts/rpmbuild", string(data), "application/json", 409)
	obj = call("PATCH", "/scripts/rpmbuild", `{"spec":{"content":"#!/bin/sh\necho patched\n"}}`, "application/merge-patch+json", 200)
	if obj.Spec.Content != "#!/bin/sh\necho patched\n" {
		t.Fatal("merge patch not applied")
	}
	call("PATCH", "/scripts/rpmbuild", `[{"op":"replace","path":"/spec/content","value":"#!/bin/bash\necho jsonpatch\n"}]`, "application/json-patch+json", 200)
	call("PATCH", "/scripts/rpmbuild", `{"metadata":{"resourceVersion":"stale"}}`, "application/merge-patch+json", 409)
	call("PATCH", "/scripts/rpmbuild", `{"spec":{"content":"missing shebang"}}`, "application/merge-patch+json", 422)
	call("HEAD", "/scripts/rpmbuild", "", "application/json", 405)
	call("HEAD", "/scripts", "", "application/json", 405)
	call("POST", "/scripts", `{"metadata":{"name":""},"spec":{"content":"#!/bin/sh\n"}}`, "application/json", 422)
	call("POST", "/scripts", `{"metadata":{"name":"INVALID"},"spec":{"content":"#!/bin/sh\n"}}`, "application/json", 422)
	call("PATCH", "/scripts/rpmbuild", `{"metadata":{"name":"other"}}`, "application/merge-patch+json", 400)
	call("PATCH", "/scripts/rpmbuild", `{"spec":{"content":null}}`, "application/merge-patch+json", 422)
	call("GET", "/scripts/rpmbuild/status", "", "application/json", 404)
	call("GET", "/projects/demo/scripts", "", "application/json", 404)
	call("GET", "/scripts?fieldSelector=status.phase=Active", "", "application/json", 400)
	call("POST", "/scripts", `{} {}`, "application/json", 400)
	if writes != 4 {
		t.Fatalf("rejected requests persisted data: writes=%d", writes)
	}
}

func TestScriptDelete(t *testing.T) {
	for _, name := range []string{"custom", "rpmbuild"} {
		t.Run(name, func(t *testing.T) { testScriptDelete(t, name) })
	}
}

func testScriptDelete(t *testing.T, name string) {
	t.Helper()
	var document json.RawMessage
	var seq int64
	deletes := 0
	client := es.NewClientForTesting("http://es", &http.Client{Transport: confTransport(func(r *http.Request) (*http.Response, error) {
		code := 200
		var body any
		if r.URL.Path != "/ebs-scripts/_doc/"+name {
			t.Fatalf("unexpected ES path %s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			if document == nil {
				code, body = 404, map[string]any{"found": false}
			} else {
				body = map[string]any{"_id": name, "_seq_no": seq, "_primary_term": 1, "_source": document}
			}
		case http.MethodPut:
			document, _ = io.ReadAll(r.Body)
			seq++
			body = map[string]any{"_seq_no": seq, "_primary_term": 1}
		case http.MethodDelete:
			deletes++
			document = nil
			body = map[string]any{"result": "deleted"}
		default:
			t.Fatalf("unexpected ES method %s", r.Method)
		}
		data, _ := json.Marshal(body)
		return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data))}, nil
	})})
	handler, err := configScriptTestHandler(map[string]rest.Storage{"scripts": newScriptStore(client)})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string, want int) *ebsv1.Script {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/apis/ebs/v1"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, rec.Code, want, rec.Body.String())
		}
		obj := new(ebsv1.Script)
		if want < 300 {
			if err := json.Unmarshal(rec.Body.Bytes(), obj); err != nil {
				t.Fatal(err)
			}
		}
		return obj
	}
	obj := call("POST", "/scripts", `{"metadata":{"name":"`+name+`"},"spec":{"content":"#!/bin/sh\necho custom\n"}}`, 201)
	call("DELETE", "/scripts/"+name, `{"preconditions":{"uid":"wrong","resourceVersion":"`+obj.ResourceVersion+`"}}`, 409)
	call("DELETE", "/scripts/"+name, `{"preconditions":{"uid":"`+string(obj.UID)+`","resourceVersion":"wrong"}}`, 409)
	if deletes != 0 {
		t.Fatalf("failed deletes reached ES: %d", deletes)
	}
	call("DELETE", "/scripts/"+name, `{}`, 200)
	if deletes != 1 || document != nil {
		t.Fatalf("delete not persisted: deletes=%d", deletes)
	}
	call("GET", "/scripts/"+name, "", 404)
}
