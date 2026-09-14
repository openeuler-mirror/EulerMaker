package gitserver

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	ebsv1 "ebs-api/ebs/v1"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestCheckSyncedUsesCacheAndBaseline(t *testing.T) {
	syncTime := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	requests := 0
	client := newTestClient(t, func(request *http.Request) (*http.Response, error) {
		requests++
		if request.URL.Path != "/api/v1/repo/status" {
			t.Fatalf("unexpected path %q", request.URL.Path)
		}
		return response(http.StatusOK, `{"clone_url":"git://git/repositories/https/example.com/repo.git","sync_time":"2026-09-14T00:00:00Z"}`), nil
	})

	result, err := client.CheckSynced(context.Background(), "https://example.com/repo.git", syncTime.Add(-time.Second))
	if err != nil || !result.Synced || result.CloneURL == "" {
		t.Fatalf("unexpected first result %#v, err=%v", result, err)
	}
	result, err = client.CheckSynced(context.Background(), "https://example.com/repo.git", syncTime.Add(time.Second))
	if err != nil || result.Synced || requests != 1 {
		t.Fatalf("cache/baseline mismatch: result=%#v requests=%d err=%v", result, requests, err)
	}
}

func TestRepositoryFailureIsPackageValidationError(t *testing.T) {
	client := newTestClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{"error":{"code":"InvalidRepositoryURL","message":"invalid","retryable":false}}`), nil
	})

	_, err := client.CheckSynced(context.Background(), "https://example.com/repo.git", time.Time{})
	assertKind(t, err, ErrorValidation)
}

func TestResolveCommitClassifiesMissingRef(t *testing.T) {
	client := newTestClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusUnprocessableEntity, `{"code":"CommandFailed","message":"unknown revision"}`), nil
	})

	_, err := client.ResolveCommit(context.Background(), "https://example.com/repo.git", ebsv1.GitRef{Type: ebsv1.GitRefBranch, Value: "main"})
	assertKind(t, err, ErrorNotFound)
}

func newTestClient(t *testing.T, transport roundTripFunc) *Client {
	t.Helper()
	client, err := New(Config{Address: "http://git-server", Timeout: time.Second, CacheTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	client.http.Transport = transport
	return client
}

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func assertKind(t *testing.T, err error, expected ErrorKind) {
	t.Helper()
	gitErr, ok := err.(*Error)
	if !ok || gitErr.Kind != expected {
		t.Fatalf("expected %s error, got %#v", expected, err)
	}
}
