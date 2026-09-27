package policy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/upstream"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProjectPermissions(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/apis/ebs/v1/projects/sample" {
			t.Errorf("unexpected project lookup %s", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"metadata":{"name":"sample","labels":{"ebs.io/owner-user":"alice","ebs.io/member-user.bob":"true"}}}`))}, nil
	})
	client, err := upstream.New("https://api.example", transport)
	if err != nil {
		t.Fatal(err)
	}
	a := New(client)
	cases := []struct {
		name  string
		who   identity.Principal
		route Route
		allow bool
	}{
		{"owner delete", identity.Principal{Subject: "alice", Scope: identity.UserScope}, Route{Resource: "jobs", Project: "sample", Name: "job", Method: http.MethodDelete}, true},
		{"member update", identity.Principal{Subject: "bob", Scope: identity.UserScope}, Route{Resource: "jobs", Project: "sample", Name: "job", Method: http.MethodPut}, true},
		{"member delete", identity.Principal{Subject: "bob", Scope: identity.UserScope}, Route{Resource: "jobs", Project: "sample", Name: "job", Method: http.MethodDelete}, false},
		{"admin abort without membership", identity.Principal{Subject: "root", Scope: identity.AdminScope}, Route{Resource: "jobs", Project: "sample", Name: "job", Subresource: "abort", Method: http.MethodPost}, false},
		{"owner abort", identity.Principal{Subject: "alice", Scope: identity.UserScope}, Route{Resource: "jobs", Project: "sample", Name: "job", Subresource: "abort", Method: http.MethodPost}, true},
		{"Build update", identity.Principal{Subject: "alice", Scope: identity.UserScope}, Route{Resource: "builds", Project: "sample", Name: "build", Method: http.MethodPatch}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allowed := a.Authorize(context.Background(), tc.who, tc.route) == nil
			if allowed != tc.allow {
				t.Fatalf("allowed=%v want %v", allowed, tc.allow)
			}
		})
	}
}

func TestPublicRead(t *testing.T) {
	if !IsPublicRead(Route{Resource: "jobs", Project: "sample", Method: http.MethodGet}) {
		t.Fatal("Project-scoped Job list should be public")
	}
	if IsPublicRead(Route{Resource: "runners", Method: http.MethodGet}) {
		t.Fatal("Runner list must not be public")
	}
	if !HasWatch(map[string][]string{"watch": {"false", "true"}}) {
		t.Fatal("repeated watch parameter bypassed check")
	}
}
