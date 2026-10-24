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

func TestOldJobsReadRoutes(t *testing.T) {
	api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.HasPrefix(r.URL.Path, "/apis/ebs/v1/projects/project/oldjobs") {
			t.Fatalf("upstream=%s", r.URL)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"apiVersion":"ebs/v1","kind":"JobList","items":[]}`))}, nil
	}))
	router := api.Router()
	for _, path := range []string{"/apis/ebs/v1/projects/project/oldjobs?includeFields=metadata&fieldSelector=status.phase=Succeeded", "/apis/ebs/v1/projects/project/oldjobs/job"} {
		r := httptest.NewRecorder()
		router.ServeHTTP(r, httptest.NewRequest("GET", path, nil))
		if r.Code != 200 {
			t.Fatalf("path=%s status=%d body=%s", path, r.Code, r.Body)
		}
	}
	token, err := api.tokens.Issue("runner", "runner", identity.RunnerType, "", time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/apis/ebs/v1/projects/project/oldjobs/job", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("runner status=%d body=%s", w.Code, w.Body)
	}
	for _, route := range router.Routes() {
		if strings.Contains(route.Path, "oldjobs") && (route.Method != "GET" || strings.HasSuffix(route.Path, "/status") || !strings.Contains(route.Path, "/projects/:project/")) {
			t.Fatalf("unexpected route=%+v", route)
		}
		if strings.HasSuffix(route.Path, "/archive") {
			t.Fatalf("archive exposed: %+v", route)
		}
	}
}

func TestWatchRouteAllowlist(t *testing.T) {
	forwarded := 0
	api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{}`
		if r.URL.Path == "/apis/iam.ebs/v1/users/admin" {
			body = `{"metadata":{"name":"admin"},"spec":{"enabled":true,"scopes":["ebs:admin"]}}`
		} else {
			forwarded++
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	adminToken, err := api.tokens.Issue("admin", "", identity.UserType, identity.AdminScope, time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	runnerToken, err := api.tokens.Issue("runner-1", "runner-1", identity.RunnerType, "", time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	router := api.Router()
	cases := []struct {
		path, token string
		status      int
	}{
		{"/projects/team/jobs?watch=true", adminToken, http.StatusOK},
		{"/runners", adminToken, http.StatusOK},
		{"/runners?watch=false", adminToken, http.StatusOK},
		{"/runners/runner-1/jobs?watch=true", adminToken, http.StatusOK},
		{"/runners/runner-1/jobs?watch=true", runnerToken, http.StatusOK},
		{"/runners?watch=true", runnerToken, http.StatusForbidden},
		{"/runners/runner-2/jobs?watch=true", runnerToken, http.StatusForbidden},
		{"/projects/team/jobs?watch=true", runnerToken, http.StatusForbidden},
		{"/projects/team/jobs?watch=true", "", http.StatusUnauthorized},
		{"/projects?watch=false", adminToken, http.StatusOK},
	}
	for _, path := range []string{
		"/projects", "/projects/team", "/projects/team/builds",
		"/projects/team/snapshots", "/projects/team/buildinfos", "/projects/team/rpmrepos",
		"/projects/team/oldjobs", "/projects/team/buildgraphs", "/configs", "/scripts",
		"/projects/team/jobs/job", "/projects/team/jobs/job/status",
		"/runners", "/runners/runner-1", "/runners/runner-1/status",
	} {
		for _, query := range []string{"watch=true", "watch=1", "watch=false&watch=true"} {
			cases = append(cases, struct {
				path, token string
				status      int
			}{path + "?" + query, adminToken, http.StatusForbidden})
		}
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			forwarded = 0
			r := httptest.NewRequest(http.MethodGet, "/apis/ebs/v1"+tc.path, nil)
			if tc.token != "" {
				r.Header.Set("Authorization", "Bearer "+tc.token)
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.status, w.Body)
			}
			wantForwarded := 0
			if tc.status == http.StatusOK {
				wantForwarded = 1
			}
			if forwarded != wantForwarded {
				t.Fatalf("forwarded=%d want=%d", forwarded, wantForwarded)
			}
		})
	}
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

func TestRunnerServiceEndpoints(t *testing.T) {
	content := `artifactManagers:
  default:
    internalURL: http://artifact-manager:8081
    runnerInternalURL: http://127.0.0.1:8081
    runnerExternalURL: http://artifacts.example.com:8081
  art-01:
    internalURL: http://artifact-manager.art-01:8081
    runnerInternalURL: http://artifact-manager.art-01:8081
    runnerExternalURL: https://artifacts.example.com
gitServer:
  internalURL: http://git-server:8080
  runnerInternalURL: http://git.internal:8084
  runnerExternalURL: https://git.example.com/prefix
`
	config, err := json.Marshal(map[string]any{
		"metadata": map[string]string{"name": "services"},
		"spec":     map[string]string{"visibility": "OpsOnly", "content": content},
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	api := newTestAPI(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/apis/iam.ebs/v1/users/alice" {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{"metadata":{"name":"alice"},"spec":{"enabled":true,"scopes":["ebs:user"]}}`))}, nil
		}
		calls++
		if request.URL.Path != "/apis/ebs/v1/configs/services" || request.Method != http.MethodGet ||
			request.Header.Get("Authorization") != "" {
			t.Errorf("unexpected upstream request: %s %s", request.Method, request.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(config)))}, nil
	}))
	for _, runner := range []string{"runner-a", "runner-b"} {
		token, err := api.tokens.Issue(runner, runner, identity.RunnerType, "", time.Hour, api.now())
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodGet, "/apis/ebs/v1/service-endpoints", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		api.Router().ServeHTTP(response, request)
		if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("runner=%s status=%d body=%s", runner, response.Code, response.Body.String())
		}
		type endpoint struct {
			RunnerInternalURL string `json:"runnerInternalURL"`
			RunnerExternalURL string `json:"runnerExternalURL"`
		}
		var endpoints struct {
			ArtifactManagers map[string]endpoint `json:"artifactManagers"`
			GitServer        endpoint            `json:"gitServer"`
		}
		decoder := json.NewDecoder(response.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&endpoints); err != nil {
			t.Fatal(err)
		}
		if len(endpoints.ArtifactManagers) != 2 || endpoints.ArtifactManagers["default"].RunnerExternalURL != "http://artifacts.example.com:8081" ||
			endpoints.ArtifactManagers["default"].RunnerInternalURL != "http://127.0.0.1:8081" ||
			endpoints.ArtifactManagers["art-01"].RunnerInternalURL != "http://artifact-manager.art-01:8081" ||
			endpoints.ArtifactManagers["art-01"].RunnerExternalURL != "https://artifacts.example.com" ||
			endpoints.GitServer.RunnerInternalURL != "http://git.internal:8084" ||
			endpoints.GitServer.RunnerExternalURL != "https://git.example.com/prefix" {
			t.Fatalf("unexpected Runner projection: %+v", endpoints)
		}
	}
	if calls != 2 {
		t.Fatalf("upstream reads=%d, want one per request", calls)
	}
	request := httptest.NewRequest(http.MethodGet, "/apis/ebs/v1/service-endpoints", nil)
	response := httptest.NewRecorder()
	api.Router().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || calls != 2 {
		t.Fatalf("anonymous status=%d upstream reads=%d", response.Code, calls)
	}
	userToken, err := api.tokens.Issue("alice", "", identity.UserType, identity.UserScope, time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodGet, "/apis/ebs/v1/service-endpoints", nil)
	request.Header.Set("Authorization", "Bearer "+userToken)
	response = httptest.NewRecorder()
	api.Router().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || calls != 2 {
		t.Fatalf("user status=%d Config reads=%d", response.Code, calls)
	}
}

func TestRunnerServiceEndpointsRejectsInvalidConfig(t *testing.T) {
	api := newTestAPI(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := `{"metadata":{"name":"services"},"spec":{"visibility":"OpsOnly","content":"artifactManagers:\n  art-01:\n    internalURL: http://artifact-manager.art-01:8081\n    secret: hidden\n"}}`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	token, err := api.tokens.Issue("runner-a", "runner-a", identity.RunnerType, "", time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/apis/ebs/v1/service-endpoints", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	api.Router().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "hidden") ||
		response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestRunnerServiceEndpointsRefresh(t *testing.T) {
	upstreamStatus := http.StatusOK
	internalURL := "http://runner-internal:8081"
	content := func() string {
		return "artifactManagers:\n  default:\n    internalURL: http://management:8081\n" +
			"    runnerInternalURL: " + internalURL + "\ngitServer:\n  internalURL: http://git-server:8080\n" +
			"  runnerInternalURL: " + internalURL + "\n"
	}
	api := newTestAPI(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		body, err := json.Marshal(map[string]any{
			"metadata": map[string]string{"name": "services"},
			"spec":     map[string]string{"visibility": "OpsOnly", "content": content()},
		})
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: upstreamStatus, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	}))
	token, err := api.tokens.Issue("runner-a", "runner-a", identity.RunnerType, "", time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, address string
		upstreamCode  int
		wantCode      int
	}{
		{"initial", "http://runner-internal:8081", http.StatusOK, http.StatusOK},
		{"updated", "https://new-endpoint:8443/prefix", http.StatusOK, http.StatusOK},
		{"unavailable", "", http.StatusNotFound, http.StatusServiceUnavailable},
		{"recovered", "http://restored:8081", http.StatusOK, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			internalURL, upstreamStatus = tc.address, tc.upstreamCode
			request := httptest.NewRequest(http.MethodGet, "/apis/ebs/v1/service-endpoints", nil)
			request.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			api.Router().ServeHTTP(response, request)
			if response.Code != tc.wantCode || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if tc.wantCode == http.StatusOK {
				var body struct {
					ArtifactManagers map[string]map[string]string `json:"artifactManagers"`
					GitServer        map[string]string            `json:"gitServer"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body.ArtifactManagers["default"]["runnerInternalURL"] != tc.address ||
					body.GitServer["runnerInternalURL"] != tc.address {
					t.Fatalf("unexpected refreshed endpoint: %s", response.Body.String())
				}
			} else if strings.Contains(response.Body.String(), "artifactManagers") || strings.Contains(response.Body.String(), "gitServer") {
				t.Fatal("configuration failure returned cached endpoints")
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
			body = `{"apiVersion":"iam.ebs/v1","kind":"UserList","metadata":{"continue":"next-page","resourceVersion":"42"},"items":[{"metadata":{"name":"admin"},"spec":{"scopes":["ebs:admin"]}},{"metadata":{"name":"alice"},"spec":{"scopes":["ebs:user"],"displayName":"Alice"}}]}`
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
	for _, field := range []string{`"apiVersion":"iam.ebs/v1"`, `"kind":"UserList"`, `"continue":"next-page"`, `"resourceVersion":"42"`, `"displayName":"Alice"`} {
		if !strings.Contains(response.Body.String(), field) {
			t.Fatalf("missing %s in response: %s", field, response.Body.String())
		}
	}
}

func TestRunnerStatusPatchBecomesValidatedPUT(t *testing.T) {
	old := `{"apiVersion":"ebs/v1","kind":"Runner","metadata":{"name":"runner-1","resourceVersion":"7"},"spec":{"instanceID":"constant","type":"ct"},"status":{"phase":"Offline"}}`
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
			if !strings.Contains(string(body), `"phase":"Online"`) || !strings.Contains(string(body), `"instanceID":"constant"`) {
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

func TestRunnerJobReadPreservesNotFound(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			path := "/apis/ebs/v1/projects/team/jobs/deleted-job"
			calls := 0
			api := newTestAPI(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.Method != http.MethodGet || request.URL.Path != path {
					t.Fatalf("unexpected upstream request: %s %s", request.Method, request.URL.Path)
				}
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"reason":"NotFound"}`)),
				}, nil
			}))
			token, err := api.tokens.Issue("runner-1", "runner-1", identity.RunnerType, "", time.Hour, api.now())
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(method, path, nil)
			request.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			api.Router().ServeHTTP(response, request)
			if response.Code != http.StatusNotFound || calls != 1 {
				t.Fatalf("status=%d calls=%d body=%q", response.Code, calls, response.Body.String())
			}
			if method == http.MethodGet {
				var body struct {
					Error string `json:"error"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error != "Job not found" {
					t.Fatalf("unexpected response: %s, error=%v", response.Body.String(), err)
				}
			} else if response.Body.Len() != 0 {
				t.Fatalf("HEAD response contains a body: %q", response.Body.String())
			}
		})
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

func TestRunnerJobsIdentityIsCheckedBeforeProxy(t *testing.T) {
	forwarded := 0
	api := newTestAPI(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		forwarded++
		if request.URL.Path != "/apis/ebs/v1/runners/runner-1/jobs" {
			t.Errorf("unexpected upstream path: %s", request.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"items":[]}`))}, nil
	}))
	token, err := api.tokens.Issue("runner-1", "runner-1", identity.RunnerType, "", time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path string
		want int
	}{
		{path: "/apis/ebs/v1/runners/runner-1/jobs", want: http.StatusOK},
		{path: "/apis/ebs/v1/runners/runner-2/jobs", want: http.StatusForbidden},
	} {
		request := httptest.NewRequest(http.MethodGet, test.path, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		api.Router().ServeHTTP(response, request)
		if response.Code != test.want {
			t.Errorf("GET %s: status=%d, want %d, body=%q", test.path, response.Code, test.want, response.Body.String())
		}
	}
	if forwarded != 1 {
		t.Fatalf("forwarded %d requests, want 1", forwarded)
	}
}

func TestControllerResourcesRegisterOnlyGet(t *testing.T) {
	api := newTestAPI(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("route registration must not call upstream")
		return nil, nil
	}))
	routes := api.Router().Routes()
	for _, resource := range []string{"snapshots", "buildinfos", "buildgraphs", "rpmrepos"} {
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
