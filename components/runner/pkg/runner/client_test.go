package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestClientPatchRunnerStatus(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotContentType string
	var gotBody map[string]RunnerStatus
	client := newTestClient(t, func(req *http.Request) (*http.Response, error) {
		gotMethod = req.Method
		gotPath = req.URL.RequestURI()
		gotAuth = req.Header.Get("Authorization")
		gotContentType = req.Header.Get("Content-Type")
		if err := json.NewDecoder(req.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		return response(200, `{}`), nil
	})

	err := client.PatchRunnerStatus(context.Background(), "runner-a", RunnerStatus{Phase: "Idle"})
	if err != nil {
		t.Fatalf("patch runner status: %v", err)
	}
	if gotMethod != http.MethodPatch {
		t.Fatalf("method = %s", gotMethod)
	}
	if gotPath != apiPrefix+"/runners/runner-a/status" {
		t.Fatalf("path = %s", gotPath)
	}
	if gotAuth != "Bearer token-a" {
		t.Fatalf("auth = %s", gotAuth)
	}
	if gotContentType != "application/merge-patch+json" {
		t.Fatalf("content type = %s", gotContentType)
	}
	if gotBody["status"].Phase != "Idle" {
		t.Fatalf("unexpected body: %#v", gotBody)
	}
}

func TestClientWatchJobsDecodesLineDelimitedEvents(t *testing.T) {
	client := newTestClient(t, func(req *http.Request) (*http.Response, error) {
		if req.URL.RequestURI() != apiPrefix+"/jobs?resourceVersion=10&watch=true" {
			t.Fatalf("path = %s", req.URL.RequestURI())
		}
		body := `{"type":"ADDED","object":{"metadata":{"name":"job-a","namespace":"project-a","resourceVersion":"11"},"status":{"runner":"runner-a","phase":"Running"}}}` + "\n" +
			`{"type":"MODIFIED","object":{"metadata":{"name":"job-b","namespace":"project-a","resourceVersion":"12"}}}` + "\n"
		return response(200, body), nil
	})

	events, errs := client.WatchJobs(context.Background(), "10")
	first := <-events
	second := <-events
	if first.Object.Metadata.Name != "job-a" || first.Object.Status.Runner != "runner-a" {
		t.Fatalf("unexpected first event: %#v", first)
	}
	if second.Object.Metadata.ResourceVersion != "12" {
		t.Fatalf("unexpected second event: %#v", second)
	}
	if err := <-errs; err != nil {
		t.Fatalf("watch error: %v", err)
	}
}

func newTestClient(t *testing.T, fn roundTripFunc) *Client {
	t.Helper()
	client, err := NewClient("https://gateway.example", "token-a", &http.Client{Transport: fn})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
