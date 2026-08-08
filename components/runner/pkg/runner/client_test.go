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

func TestClientUpdateRunnerUsesRestrictedMergePatch(t *testing.T) {
	client := newTestClient(t, func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPatch || req.URL.Path != apiPrefix+"/runners/runner-a" {
			t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
		}
		if req.Header.Get("Content-Type") != "application/merge-patch+json" {
			t.Fatalf("content type = %s", req.Header.Get("Content-Type"))
		}
		var patch map[string]any
		if err := json.NewDecoder(req.Body).Decode(&patch); err != nil {
			t.Fatalf("decode patch: %v", err)
		}
		if _, exists := patch["status"]; exists {
			t.Fatalf("runner update included status: %#v", patch)
		}
		return response(http.StatusOK, `{}`), nil
	})
	err := client.UpdateRunner(context.Background(), RunnerResource{
		Metadata: ObjectMeta{Name: "runner-a"},
		Spec:     RunnerSpec{Type: "ct", Arch: "x86_64", Hostname: "runner-a", Unschedulable: true},
		Status:   RunnerStatus{Phase: "Running"},
	})
	if err != nil {
		t.Fatalf("update runner: %v", err)
	}
}

func TestClientListAssignedJobs(t *testing.T) {
	client := newTestClient(t, func(req *http.Request) (*http.Response, error) {
		if req.URL.RequestURI() != apiPrefix+"/runners/runner-a/jobs" {
			t.Fatalf("path = %s", req.URL.RequestURI())
		}
		return response(200, `{"metadata":{"resourceVersion":"10"},"items":[{"metadata":{"name":"job-a","namespace":"project-a"}}]}`), nil
	})
	list, err := client.ListAssignedJobs(context.Background(), "runner-a")
	if err != nil || list.Metadata.ResourceVersion != "10" || len(list.Items) != 1 {
		t.Fatalf("unexpected list: %#v err=%v", list, err)
	}
}

func TestClientWatchAssignedJobsDecodesLineDelimitedEvents(t *testing.T) {
	client := newTestClient(t, func(req *http.Request) (*http.Response, error) {
		want := apiPrefix + "/runners/runner-a/jobs?allowWatchBookmarks=true&resourceVersion=10&timeoutSeconds=300&watch=true"
		if req.URL.RequestURI() != want {
			t.Fatalf("path = %s", req.URL.RequestURI())
		}
		body := `{"type":"ADDED","object":{"metadata":{"name":"job-a","namespace":"project-a","resourceVersion":"11"},"status":{"runner":"runner-a","phase":"Running"}}}` + "\n" +
			`{"type":"MODIFIED","object":{"metadata":{"name":"job-b","namespace":"project-a","resourceVersion":"12"}}}` + "\n"
		return response(200, body), nil
	})

	events, errs := client.WatchAssignedJobs(context.Background(), "runner-a", "10")
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

func TestClientWatchAssignedJobsReturnsWatchError(t *testing.T) {
	client := newTestClient(t, func(req *http.Request) (*http.Response, error) {
		return response(200, `{"type":"ERROR","object":{"code":410,"reason":"Gone","message":"resource version expired"}}`+"\n"), nil
	})
	events, errs := client.WatchAssignedJobs(context.Background(), "runner-a", "10")
	if _, ok := <-events; ok {
		t.Fatal("unexpected watch event")
	}
	err := <-errs
	statusErr, ok := err.(StatusError)
	if !ok || statusErr.Code != 410 {
		t.Fatalf("expected 410 StatusError, got %#v", err)
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
