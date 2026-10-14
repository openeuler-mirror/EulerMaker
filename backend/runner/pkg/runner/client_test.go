package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestClientGetScript(t *testing.T) {
	client := newTestClient(t, func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.Path != apiPrefix+"/scripts/rpmbuild" {
			t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
		}
		return response(http.StatusOK, `{"apiVersion":"ebs/v1","kind":"Script","metadata":{"name":"rpmbuild","uid":"uid-1","resourceVersion":"rv-1"},"spec":{"content":"#!/bin/sh\necho ok\n"}}`), nil
	})
	script, err := client.GetScript(context.Background(), "rpmbuild")
	if err != nil || script.Metadata.ResourceVersion != "rv-1" || script.Spec.Content == "" {
		t.Fatalf("GetScript = %+v, %v", script, err)
	}
}

func TestClientPatchRunnerStatus(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotContentType string
	var gotBody struct {
		Status   RunnerStatus `json:"status"`
		Metadata ObjectMeta   `json:"metadata"`
	}
	client := newTestClient(t, func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			return response(200, `{"metadata":{"resourceVersion":"7"},"status":{"phase":"Online"}}`), nil
		}
		gotMethod = req.Method
		gotPath = req.URL.RequestURI()
		gotAuth = req.Header.Get("Authorization")
		gotContentType = req.Header.Get("Content-Type")
		if err := json.NewDecoder(req.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		return response(200, `{}`), nil
	})

	err := client.PatchRunnerStatus(context.Background(), "runner-a", RunnerStatus{Phase: "Online"})
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
	if gotBody.Status.Phase != "Online" || gotBody.Metadata.ResourceVersion != "7" {
		t.Fatalf("unexpected body: %#v", gotBody)
	}
}

func TestClientCreateRunnerOmitsStatus(t *testing.T) {
	client := newTestClient(t, func(req *http.Request) (*http.Response, error) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if _, exists := body["status"]; exists {
			t.Fatalf("create body contains status: %s", body["status"])
		}
		var spec RunnerSpec
		if err := json.Unmarshal(body["spec"], &spec); err != nil {
			t.Fatalf("decode spec: %v", err)
		}
		if spec.InstanceID != testRunnerInstanceID {
			t.Fatalf("instanceId = %q", spec.InstanceID)
		}
		return response(http.StatusCreated, `{}`), nil
	})
	err := client.CreateRunner(context.Background(), RunnerResource{
		TypeMeta: TypeMeta{APIVersion: "ebs/v1", Kind: "Runner"},
		Metadata: ObjectMeta{Name: "runner-a", Labels: map[string]string{
			"ebs.io/runner-type": "ct", "ebs.io/runner-arch": "x86_64",
		}},
		Spec: RunnerSpec{InstanceID: testRunnerInstanceID, Type: "ct", Arch: "x86_64"},
	})
	if err != nil {
		t.Fatalf("create runner: %v", err)
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
		Spec:     RunnerSpec{Type: "ct", Arch: "x86_64", Unschedulable: true},
		Status:   RunnerStatus{Phase: "Online"},
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
		return response(200, `{"metadata":{"resourceVersion":"10"},"items":[{"metadata":{"name":"job-a","namespace":"project-a"},"spec":{"priority":100}}]}`), nil
	})
	list, err := client.ListAssignedJobs(context.Background(), "runner-a")
	if err != nil || list.Metadata.ResourceVersion != "10" || len(list.Items) != 1 || list.Items[0].Spec.Priority != 100 {
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

func TestClientRefreshesAndRetriesUnauthorizedOnce(t *testing.T) {
	tokens := &testTokenSource{token: "token-a", refreshed: "token-b"}
	calls := 0
	client, err := NewClient("https://gateway.example", tokens, &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			if req.Header.Get("Authorization") != "Bearer token-a" {
				t.Fatalf("first auth = %q", req.Header.Get("Authorization"))
			}
			return response(http.StatusUnauthorized, "unauthorized"), nil
		}
		if req.Header.Get("Authorization") != "Bearer token-b" {
			t.Fatalf("retry auth = %q", req.Header.Get("Authorization"))
		}
		return response(http.StatusUnauthorized, "still unauthorized"), nil
	})})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	err = client.PatchRunnerStatus(context.Background(), "runner-a", RunnerStatus{Phase: "Online"})
	if err == nil {
		t.Fatal("expected unauthorized error")
	}
	if calls != 2 || tokens.refreshCalls != 1 {
		t.Fatalf("calls=%d refreshes=%d", calls, tokens.refreshCalls)
	}
}

func newTestClient(t *testing.T, fn roundTripFunc) *Client {
	t.Helper()
	client, err := NewClient("https://gateway.example", &testTokenSource{token: "token-a", refreshed: "token-b"}, &http.Client{Transport: fn})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client
}

type testTokenSource struct {
	token        string
	refreshed    string
	refreshCalls int
}

func (s *testTokenSource) Token(context.Context) (string, error) { return s.token, nil }

func (s *testTokenSource) RefreshAfterUnauthorized(_ context.Context, rejected string) (string, error) {
	s.refreshCalls++
	if rejected == s.token {
		s.token = s.refreshed
	}
	return s.token, nil
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

func TestClientStatusPreservesEviction(t *testing.T) {
	for _, reported := range []string{"Online", "Offline"} {
		t.Run(reported, func(t *testing.T) {
			client := newTestClient(t, func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet {
					return response(200, `{"metadata":{"resourceVersion":"9"},"status":{"phase":"Evicted"}}`), nil
				}
				var body struct {
					Metadata ObjectMeta   `json:"metadata"`
					Status   RunnerStatus `json:"status"`
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body.Status.Phase != "Evicted" || body.Metadata.ResourceVersion != "9" {
					t.Fatalf("eviction overwritten: %+v", body)
				}
				return response(409, `{"message":"conflict"}`), nil
			})
			if err := client.PatchRunnerStatus(context.Background(), "r", RunnerStatus{Phase: reported}); err == nil {
				t.Fatal("concurrent status conflict must propagate instead of retrying a stale status")
			}
		})
	}
}
