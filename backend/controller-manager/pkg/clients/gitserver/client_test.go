package gitserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
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
	result, err = client.CheckSynced(context.Background(), "git@EXAMPLE.com:repo", syncTime.Add(time.Second))
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

func TestResolveFullRefCommand(t *testing.T) {
	for _, ref := range []string{"refs/merge-requests/501/head", "refs/heads/main", "refs/tags/v1", "refs/heads/版本+1"} {
		t.Run(ref, func(t *testing.T) {
			origin := "https://example.com/repo.git"
			requests := 0
			client := newTestClient(t, func(request *http.Request) (*http.Response, error) {
				requests++
				var command commandRequest
				if err := json.NewDecoder(request.Body).Decode(&command); err != nil {
					t.Fatal(err)
				}
				want := []string{"git-rev-parse", "--verify", ref + "^{commit}"}
				if request.Method != http.MethodPost || request.URL.Path != "/command" ||
					command.Repo != origin || !reflect.DeepEqual(command.Command, want) {
					t.Fatalf("request=%s %s command=%+v", request.Method, request.URL.Path, command)
				}
				return response(http.StatusOK, `{"stdout":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n","exit_code":0}`), nil
			})
			for i := 0; i < 2; i++ {
				commit, err := client.ResolveFullRef(context.Background(), origin, ref)
				if err != nil || commit != strings.Repeat("a", 40) {
					t.Fatalf("commit=%q err=%v", commit, err)
				}
			}
			if requests != 2 {
				t.Fatalf("resolve requests=%d, want 2", requests)
			}
		})
	}
}

func TestResolveFullRefErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		kind   ErrorKind
	}{
		{"missing", http.StatusUnprocessableEntity, `{"code":"CommandFailed"}`, ErrorNotFound},
		{"unavailable", http.StatusServiceUnavailable, `{"code":"Unavailable"}`, ErrorTemporary},
		{"forbidden", http.StatusForbidden, `{"code":"Forbidden"}`, ErrorPermanent},
		{"invalid response", http.StatusOK, `{"stdout":"not-a-commit","exit_code":0}`, ErrorTemporary},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(t, func(*http.Request) (*http.Response, error) {
				return response(tc.status, tc.body), nil
			})
			_, err := client.ResolveFullRef(context.Background(), "https://example.com/repo.git", "refs/merge-requests/501/head")
			assertKind(t, err, tc.kind)
		})
	}
}

func TestResolveFullRefValidatesLiteralRef(t *testing.T) {
	client := newTestClient(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid ref reached HTTP transport")
		return nil, nil
	})
	for _, ref := range []string{"main", "refs/heads/main~1", "refs/heads/main^{commit}", "refs/heads/a..b", "refs/heads/a.lock"} {
		_, err := client.ResolveFullRef(context.Background(), "https://example.com/repo.git", ref)
		assertKind(t, err, ErrorValidation)
	}
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
