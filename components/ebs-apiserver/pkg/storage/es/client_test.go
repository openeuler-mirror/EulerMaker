package es

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
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

func TestEnsureIndicesOnlyCreatesESPrimaryResources(t *testing.T) {
	created := map[string]bool{}
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.Method {
		case http.MethodGet:
			return response(http.StatusOK, `{}`), nil
		case http.MethodHead:
			return response(http.StatusNotFound, ``), nil
		case http.MethodPut:
			created[strings.TrimPrefix(req.URL.Path, "/")] = true
			return response(http.StatusOK, `{"acknowledged":true}`), nil
		default:
			t.Fatalf("unexpected method %s", req.Method)
			return nil, nil
		}
	})}
	client := NewClientForTesting("http://elasticsearch", httpClient)
	if err := client.ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := client.ensureIndices(); err != nil {
		t.Fatalf("ensure indices: %v", err)
	}
	for _, index := range []string{"ebs-projects", "ebs-snapshots", "ebs-builds", "ebs-buildinfos", "ebs-rpmrepos", "ebs-buildresources"} {
		if !created[index] {
			t.Errorf("index %s was not created", index)
		}
	}
	if created["ebs-jobs"] || created["ebs-runners"] {
		t.Fatalf("etcd resources must not have ES indices: %#v", created)
	}
}

func TestEnsureIAMIndices(t *testing.T) {
	created := map[string]bool{}
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodHead {
			return response(http.StatusNotFound, ``), nil
		}
		if req.Method == http.MethodPut {
			created[strings.TrimPrefix(req.URL.Path, "/")] = true
			return response(http.StatusOK, `{"acknowledged":true}`), nil
		}
		t.Fatalf("unexpected method %s", req.Method)
		return nil, nil
	})}
	client := NewClientForTesting("http://elasticsearch", httpClient)
	if err := client.EnsureIAMIndices(); err != nil {
		t.Fatalf("ensure IAM indices: %v", err)
	}
	for _, index := range []string{"ebs-users", "ebs-machineaccounts"} {
		if !created[index] {
			t.Errorf("index %s was not created", index)
		}
	}
	if created["ebs-projects"] {
		t.Fatalf("core index unexpectedly created: %#v", created)
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
