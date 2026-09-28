package route

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/handler"
	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/limit"
	"ebs-gateway/internal/upstream"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type testAPI struct {
	*handler.Handler
	tokens *identity.Tokens
	now    func() time.Time
}

func (a *testAPI) Router() *gin.Engine { return New(a.Handler) }

func newTestAPI(t *testing.T, transport http.RoundTripper) *testAPI {
	t.Helper()
	client, err := upstream.New("https://api.example", transport)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := identity.NewTokens(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32))))
	if err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return time.Unix(1790000000, 0) }
	api, err := handler.New(handler.Dependencies{Upstream: client, Tokens: tokens, Now: now, Limits: limit.New(100, 200, now), BodyLimit: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return &testAPI{Handler: api, tokens: tokens, now: now}
}

func TestLoginAndCheck(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/internal/iam/v1/authenticate":
			body = `{"authenticated":true,"username":"alice"}`
		case "/apis/iam.ebs/v1/users/alice":
			body = `{"metadata":{"name":"alice"},"spec":{"enabled":true,"scopes":["ebs:user"]}}`
		default:
			t.Errorf("unexpected IAM request %s", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	api := newTestAPI(t, transport)
	router := api.Router()
	login := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"username":"alice","password":"correct-password"}`))
	login.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, login)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("login failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&result); err != nil || result.Token == "" {
		t.Fatalf("missing login token: %v", err)
	}
	check := httptest.NewRequest(http.MethodPost, "/auth/check", nil)
	check.Header.Set("Authorization", "Bearer "+result.Token)
	checkRecorder := httptest.NewRecorder()
	router.ServeHTTP(checkRecorder, check)
	if checkRecorder.Code != http.StatusOK {
		t.Fatalf("token check failed: status=%d body=%s", checkRecorder.Code, checkRecorder.Body.String())
	}
	var checked struct {
		Authenticated bool `json:"authenticated"`
		Identity      struct {
			Type   string           `json:"type"`
			Name   string           `json:"name"`
			Scopes []identity.Scope `json:"scopes"`
		} `json:"identity"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if err := json.NewDecoder(checkRecorder.Body).Decode(&checked); err != nil {
		t.Fatal(err)
	}
	if !checked.Authenticated || checked.Identity.Type != "user" || checked.Identity.Name != "alice" ||
		len(checked.Identity.Scopes) != 1 || checked.Identity.Scopes[0] != identity.UserScope || checked.ExpiresAt.IsZero() {
		t.Fatalf("unexpected token check response: %+v", checked)
	}
}

func TestCheckTokenUsesScopesForUserPermissions(t *testing.T) {
	api := newTestAPI(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("token check must not call upstream")
		return nil, nil
	}))
	for _, scope := range []identity.Scope{identity.UserScope, identity.OpsScope, identity.AdminScope} {
		t.Run(string(scope), func(t *testing.T) {
			token, err := api.tokens.Issue("alice", "", identity.UserType, scope, time.Hour, api.now())
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/auth/check", nil)
			request.Header.Set("Authorization", "Bearer "+token)
			recorder := httptest.NewRecorder()
			api.Router().ServeHTTP(recorder, request)
			var response struct {
				Identity struct {
					Type   string           `json:"type"`
					Scopes []identity.Scope `json:"scopes"`
				} `json:"identity"`
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("token check failed: status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
				t.Fatal(err)
			}
			if response.Identity.Type != "user" || len(response.Identity.Scopes) != 1 || response.Identity.Scopes[0] != scope {
				t.Fatalf("unexpected token check response: %+v", response)
			}
		})
	}
}

func TestCheckTokenMachineIdentityHasNoUserScopes(t *testing.T) {
	api := newTestAPI(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("token check must not call upstream")
		return nil, nil
	}))
	for _, test := range []struct {
		kind   identity.Type
		runner string
	}{
		{identity.RunnerType, "runner-a"},
	} {
		t.Run(string(test.kind), func(t *testing.T) {
			token, err := api.tokens.Issue("runner-a", test.runner, test.kind, "", time.Hour, api.now())
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/auth/check", nil)
			request.Header.Set("Authorization", "Bearer "+token)
			recorder := httptest.NewRecorder()
			api.Router().ServeHTTP(recorder, request)
			var response struct {
				Identity struct {
					Type   identity.Type    `json:"type"`
					Scopes []identity.Scope `json:"scopes"`
				} `json:"identity"`
			}
			if recorder.Code != http.StatusOK || json.NewDecoder(recorder.Body).Decode(&response) != nil ||
				response.Identity.Type != test.kind || response.Identity.Scopes == nil || len(response.Identity.Scopes) != 0 {
				t.Fatalf("unexpected token check response: status=%d response=%+v", recorder.Code, response)
			}
		})
	}
}

func TestRegisterRejectsUnknownFields(t *testing.T) {
	api := newTestAPI(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid registration reached IAM")
		return nil, nil
	}))
	request := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(`{"username":"alice","password":"valid-password","scopes":["ebs:admin"]}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	api.Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status %d", recorder.Code)
	}
}

func TestConfigVisibilityUsesOneUpstreamResponse(t *testing.T) {
	for _, tc := range []struct {
		visibility string
		status     int
	}{
		{"Public", http.StatusOK},
		{"OpsOnly", http.StatusForbidden},
	} {
		t.Run(tc.visibility, func(t *testing.T) {
			calls := 0
			api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path != "/apis/ebs/v1/configs/example" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				body := `{"metadata":{"name":"example"},"spec":{"visibility":"` + tc.visibility + `","content":"hello"}}`
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			recorder := httptest.NewRecorder()
			api.Router().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/apis/ebs/v1/configs/example", nil))
			if calls != 1 || recorder.Code != tc.status {
				t.Fatalf("calls=%d status=%d body=%q", calls, recorder.Code, recorder.Body.String())
			}
			if tc.status == http.StatusForbidden && strings.Contains(recorder.Body.String(), "hello") {
				t.Fatal("OpsOnly content leaked")
			}
		})
	}
}

func TestAdminUserGetUsesCheckedResponse(t *testing.T) {
	calls := 0
	api := newTestAPI(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/apis/iam.ebs/v1/users/admin":
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"metadata":{"name":"admin"},"spec":{"enabled":true,"scopes":["ebs:admin"]}}`))}, nil
		case "/apis/iam.ebs/v1/users/alice":
			calls++
			scope := "ebs:user"
			if calls > 1 {
				scope = "ebs:admin"
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"metadata":{"name":"alice"},"spec":{"enabled":true,"scopes":["` + scope + `"]}}`))}, nil
		default:
			t.Fatalf("unexpected upstream path %s", request.URL.Path)
			return nil, nil
		}
	}))
	token, err := api.tokens.Issue("admin", "", identity.UserType, identity.AdminScope, time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/apis/iam.ebs/v1/users/alice", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	api.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK || calls != 1 || strings.Contains(response.Body.String(), "ebs:admin") {
		t.Fatalf("status=%d calls=%d body=%q", response.Code, calls, response.Body.String())
	}
}

func TestUserListOmitsAdminAccounts(t *testing.T) {
	api := newTestAPI(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body string
		switch request.URL.Path {
		case "/apis/iam.ebs/v1/users/admin":
			body = `{"metadata":{"name":"admin"},"spec":{"enabled":true,"scopes":["ebs:admin"]}}`
		case "/apis/iam.ebs/v1/users":
			body = `{"items":[{"metadata":{"name":"admin"},"spec":{"scopes":["ebs:admin"]}},{"metadata":{"name":"alice"},"spec":{"scopes":["ebs:user"]}}]}`
		default:
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	token, err := api.tokens.Issue("admin", "", identity.UserType, identity.AdminScope, time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/apis/iam.ebs/v1/users", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	api.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"name":"admin"`) || !strings.Contains(response.Body.String(), `"name":"alice"`) {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestRunnerStatusPatchBecomesValidatedPUT(t *testing.T) {
	old := `{"apiVersion":"ebs/v1","kind":"Runner","metadata":{"name":"runner-1","resourceVersion":"7"},"spec":{"instanceId":"constant","type":"ct"},"status":{"phase":"Offline"}}`
	writes := 0
	api := newTestAPI(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/apis/ebs/v1/runners/runner-1/status" {
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
		switch request.Method {
		case http.MethodGet:
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(old))}, nil
		case http.MethodPut:
			writes++
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), `"phase":"Online"`) || !strings.Contains(string(body), `"instanceId":"constant"`) {
				t.Errorf("invalid complete object forwarded: %s", body)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
		default:
			t.Fatalf("unexpected method %s", request.Method)
			return nil, nil
		}
	}))
	token, err := api.tokens.Issue("runner-1", "runner-1", identity.RunnerType, "", time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPatch, "/apis/ebs/v1/runners/runner-1/status", strings.NewReader(`{"status":{"phase":"Online"}}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/merge-patch+json")
	response := httptest.NewRecorder()
	api.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK || writes != 1 {
		t.Fatalf("status=%d writes=%d body=%q", response.Code, writes, response.Body.String())
	}
}

func TestRunnerCannotUpdateUnassignedJob(t *testing.T) {
	old := `{"apiVersion":"ebs/v1","kind":"Job","metadata":{"name":"job-1","namespace":"team","resourceVersion":"7"},"spec":{},"status":{"runner":"runner-2","phase":"Running"}}`
	api := newTestAPI(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet {
			t.Fatalf("unassigned Job was written with %s", request.Method)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(old))}, nil
	}))
	token, err := api.tokens.Issue("runner-1", "runner-1", identity.RunnerType, "", time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPatch, "/apis/ebs/v1/projects/team/jobs/job-1/status", strings.NewReader(`{"status":{"phase":"Succeeded"}}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/merge-patch+json")
	response := httptest.NewRecorder()
	api.Router().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

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
	userToken, err := api.tokens.Issue("alice", "", identity.UserType, identity.UserScope, time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	runnerToken, err := api.tokens.Issue("runner-1", "runner-1", identity.RunnerType, "", time.Hour, api.now())
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

func TestControllerResourcesRegisterOnlyGet(t *testing.T) {
	api := newTestAPI(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("route registration must not call upstream")
		return nil, nil
	}))
	routes := api.Router().Routes()
	for _, resource := range []string{"snapshots", "buildinfos", "rpmrepos"} {
		prefix := "/apis/ebs/v1/projects/:project/" + resource
		seen := map[string]bool{}
		for _, route := range routes {
			if route.Path != prefix && route.Path != prefix+"/:name" && route.Path != prefix+"/:name/status" {
				continue
			}
			if route.Method != http.MethodGet {
				t.Errorf("%s %s is registered, want GET only", route.Method, route.Path)
			}
			seen[route.Path] = true
		}
		for _, path := range []string{prefix, prefix + "/:name", prefix + "/:name/status"} {
			if !seen[path] {
				t.Errorf("GET %s is not registered", path)
			}
		}
	}
}

func TestAdminCannotModifyProject(t *testing.T) {
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
	} {
		t.Run(test.name, func(t *testing.T) {
			token, err := api.tokens.Issue(test.subject, "", identity.UserType, test.scope, time.Hour, api.now())
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
