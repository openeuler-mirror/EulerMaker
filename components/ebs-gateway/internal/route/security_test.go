package route

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ebs-gateway/internal/identity"
)

func TestResourceAuthorizationBoundaries(t *testing.T) {
	api := newTestAPI(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body string
		switch request.URL.Path {
		case "/apis/iam.ebs/v1/users/alice":
			body = `{"metadata":{"name":"alice"},"spec":{"enabled":true,"scopes":["ebs:user"]}}`
		case "/apis/ebs/v1/projects/team":
			body = `{"metadata":{"name":"team","labels":{"ebs.io/owner-user":"alice"}}}`
		case "/apis/ebs/v1/projects/team/jobs":
			if request.URL.Query().Get("limit") != "100" {
				t.Errorf("public collection was not bounded: %q", request.URL.RawQuery)
			}
			body = `{"items":[]}`
		default:
			t.Errorf("unexpected upstream request: %s", request.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	userToken, err := api.tokens.Issue("alice", "", identity.UserScope, time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	runnerToken, err := api.tokens.Issue("runner-1", "runner-1", identity.RunnerScope, time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, method, path, token string
		status                    int
	}{
		{"anonymous public list", http.MethodGet, "/apis/ebs/v1/projects/team/jobs", "", http.StatusOK},
		{"anonymous watch", http.MethodGet, "/apis/ebs/v1/projects/team/jobs?watch=true", "", http.StatusUnauthorized},
		{"invalid bearer cannot become anonymous", http.MethodGet, "/apis/ebs/v1/projects/team/jobs", "bad", http.StatusUnauthorized},
		{"runner not public business reader", http.MethodGet, "/apis/ebs/v1/projects/team/jobs", runnerToken, http.StatusForbidden},
		{"unknown anonymous path", http.MethodGet, "/apis/ebs/v1/secrets", "", http.StatusUnauthorized},
		{"unknown authenticated path", http.MethodGet, "/apis/ebs/v1/secrets", userToken, http.StatusNotFound},
		{"abort wrong method", http.MethodGet, "/apis/ebs/v1/projects/team/jobs/job-1/abort", userToken, http.StatusMethodNotAllowed},
		{"collection update", http.MethodPut, "/apis/ebs/v1/projects/team/jobs", userToken, http.StatusMethodNotAllowed},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			if test.token != "" {
				request.Header.Set("Authorization", "Bearer "+test.token)
			}
			response := httptest.NewRecorder()
			api.Router().ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status=%d, want %d, body=%q", response.Code, test.status, response.Body.String())
			}
		})
	}
}

func TestAdminAndSystemCannotModifyProject(t *testing.T) {
	api := newTestAPI(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Path != "/apis/iam.ebs/v1/users/admin" {
			t.Fatalf("unexpected Project write or lookup: %s %s", request.Method, request.URL.Path)
		}
		body := `{"metadata":{"name":"admin"},"spec":{"enabled":true,"scopes":["ebs:admin"]}}`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	for _, test := range []struct {
		name, subject string
		scope         identity.Scope
		method, path  string
	}{
		{"admin update", "admin", identity.AdminScope, http.MethodPut, "/apis/ebs/v1/projects/team"},
		{"admin delete", "admin", identity.AdminScope, http.MethodDelete, "/apis/ebs/v1/projects/team"},
		{"system status update", "system", identity.SystemScope, http.MethodPatch, "/apis/ebs/v1/projects/team/status"},
	} {
		t.Run(test.name, func(t *testing.T) {
			token, err := api.tokens.Issue(test.subject, "", test.scope, time.Hour, api.now())
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(test.method, test.path, nil)
			request.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			api.Router().ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("status=%d, want 403", response.Code)
			}
		})
	}
}
