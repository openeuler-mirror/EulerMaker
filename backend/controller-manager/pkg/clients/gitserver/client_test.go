package gitserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
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
		return response(http.StatusOK, `{"clone_path":"/git/example.com/repo.git","sync_time":"2026-09-14T00:00:00Z"}`), nil
	})

	result, err := client.CheckSynced(context.Background(), "https://example.com/repo.git", syncTime.Add(-time.Second))
	if err != nil || !result.Synced || result.CloneURL != "git-server:///git/example.com/repo.git" {
		t.Fatalf("unexpected first result %#v, err=%v", result, err)
	}
	result, err = client.CheckSynced(context.Background(), "git@EXAMPLE.com:repo", syncTime.Add(time.Second))
	if err != nil || result.Synced || requests != 1 {
		t.Fatalf("cache/baseline mismatch: result=%#v requests=%d err=%v", result, requests, err)
	}
}

func TestCheckSyncedClonePath(t *testing.T) {
	for _, key := range []string{"example.com/team/repo.git", "example.com/team/repo%2Bname.git", "example.com/team/仓库.git"} {
		t.Run(key, func(t *testing.T) {
			clonePath := (&url.URL{Path: "/git/" + key}).EscapedPath()
			client := newTestClient(t, func(*http.Request) (*http.Response, error) {
				return response(http.StatusOK, `{"clone_path":"`+clonePath+`","sync_time":"2026-09-14T00:00:00Z"}`), nil
			})
			got, err := client.CheckSynced(context.Background(), "https://example.com/repo.git", time.Time{})
			if err != nil || !got.Synced || got.CloneURL != "git-server://"+clonePath {
				t.Fatalf("result=%+v error=%v", got, err)
			}
			parsed, err := url.Parse(got.CloneURL)
			if err != nil || parsed.Scheme != "git-server" || parsed.Host != "" || parsed.Path != "/git/"+key {
				t.Fatalf("logical URL changed repository key: %s, %v", got.CloneURL, err)
			}
		})
	}
}

func TestCheckSyncedReadinessAndPathContract(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		wantError bool
	}{
		{name: "awaiting first sync", body: `{}`},
		{name: "missing path after sync", body: `{"sync_time":"2026-09-14T00:00:00Z"}`, wantError: true},
		{name: "absolute address", body: `{"sync_time":"2026-09-14T00:00:00Z","clone_path":"https://git/git/repo.git"}`, wantError: true},
		{name: "traversal", body: `{"sync_time":"2026-09-14T00:00:00Z","clone_path":"/git/%2e%2e/repo.git"}`, wantError: true},
		{name: "query", body: `{"sync_time":"2026-09-14T00:00:00Z","clone_path":"/git/repo.git?token=x"}`, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(t, func(*http.Request) (*http.Response, error) {
				return response(http.StatusOK, tc.body), nil
			})
			got, err := client.CheckSynced(context.Background(), "https://example.com/repo.git", time.Time{})
			if tc.wantError {
				assertKind(t, err, ErrorTemporary)
			} else if err != nil || got.Synced || got.CloneURL != "" {
				t.Fatalf("result=%+v error=%v", got, err)
			}
		})
	}
}

func TestRepositoryFailureIsPackageValidationError(t *testing.T) {
	client := newTestClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{"error":{"code":"InvalidRepositoryURL","message":"invalid","retryable":false}}`), nil
	})

	_, err := client.CheckSynced(context.Background(), "https://example.com/repo.git", time.Time{})
	assertKind(t, err, ErrorValidation)
}

func TestResolveCommitID(t *testing.T) {
	const commitID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	client := newTestClient(t, func(request *http.Request) (*http.Response, error) {
		var command commandRequest
		if err := json.NewDecoder(request.Body).Decode(&command); err != nil {
			t.Fatal(err)
		}
		want := []string{"git-rev-parse", "--verify", commitID + "^{commit}"}
		if !reflect.DeepEqual(command.Command, want) {
			t.Fatalf("command=%v", command.Command)
		}
		return response(http.StatusOK, `{"stdout":"`+commitID+`\n","exit_code":0}`), nil
	})
	got, err := client.ResolveCommitID(context.Background(), "https://example.com/repo.git", commitID)
	if err != nil || got != commitID {
		t.Fatalf("commit=%q err=%v", got, err)
	}
	missing := newTestClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusUnprocessableEntity, `{"code":"CommandFailed"}`), nil
	})
	_, err = missing.ResolveCommitID(context.Background(), "https://example.com/repo.git", commitID)
	assertKind(t, err, ErrorNotFound)
}

func TestResolveFullRefClassifiesMissingRef(t *testing.T) {
	client := newTestClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusUnprocessableEntity, `{"code":"CommandFailed","message":"unknown revision"}`), nil
	})

	_, err := client.ResolveFullRef(context.Background(), "https://example.com/repo.git", "refs/heads/main")
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
