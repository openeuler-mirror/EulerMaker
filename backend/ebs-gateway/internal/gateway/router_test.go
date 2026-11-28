package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/klog/v2"

	ebsv1 "ebs-api/ebs/v1"
	iamv1 "ebs-api/iam/v1"
	"ebs-gateway/internal/auth"
	"ebs-gateway/internal/iamclient"
	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/limit"
	"ebs-gateway/internal/middleware"
	"ebs-gateway/internal/oauth"
	"ebs-gateway/internal/policy"
	"ebs-gateway/internal/upstream"
)

type testAPI struct {
	*Handler
	authHandler  *auth.Handler
	oauthHandler *oauth.Handler
	tokens       *identity.Tokens
	now          func() time.Time
}

func TestPersonalCredentialRoutes(t *testing.T) {
	var audit bytes.Buffer
	state := klog.CaptureState()
	t.Cleanup(state.Restore)
	loggingFlags := flag.NewFlagSet("logging", flag.ContinueOnError)
	klog.InitFlags(loggingFlags)
	for name, value := range map[string]string{"logtostderr": "false", "alsologtostderr": "false", "stderrthreshold": "FATAL", "one_output": "true"} {
		if err := loggingFlags.Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	klog.SetOutput(&audit)
	id := strings.Repeat("a", 32)
	pat := "ebs_pat_v1.YWxpY2U." + id + "." + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	now := time.Unix(1790000000, 0).UTC()
	scope, revoked, unavailable := "ebs:admin", false, false
	lookups, operations := 0, 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" {
			t.Fatal("caller token leaked upstream")
		}
		status, body := 200, any(map[string]any{})
		metadata := map[string]any{"id": id, "name": "CLI", "createdAt": now, "expiresAt": now.Add(90 * 24 * time.Hour)}
		switch r.URL.Path {
		case "/internal/iam/v1/users/authenticate":
			lookups++
			var input map[string]string
			if json.NewDecoder(r.Body).Decode(&input) != nil || input["method"] != "credential" || input["token"] != pat {
				t.Fatal("unexpected credential authentication request")
			}
			body = map[string]any{"authenticated": true, "username": "alice", "uid": "alice-uid", "scopes": []string{scope}, "credentialID": id, "expiresAt": now.Add(90 * 24 * time.Hour)}
			if revoked {
				status, body = 401, map[string]string{"code": "InvalidCredential", "error": "PRIVATE"}
			}
			if unavailable {
				status, body = 503, map[string]string{"code": "CredentialServiceUnavailable", "error": "PRIVATE"}
			}
		case "/apis/iam.ebs/v1/users/alice", "/apis/iam.ebs/v1/users/bob":
			lookups++
			name := strings.TrimPrefix(r.URL.Path, "/apis/iam.ebs/v1/users/")
			body = map[string]any{"metadata": map[string]string{"name": name}, "spec": map[string]any{"enabled": true, "scopes": []string{scope}}}
		case "/apis/iam.ebs/v1/machineaccounts/service":
			body = map[string]any{"metadata": map[string]string{"name": "service"}, "spec": map[string]string{"type": "service"}}
		case "/internal/iam/v1/users/alice/credentials":
			operations++
			if r.Method == "POST" {
				status, body = 201, metadata
				metadata["token"] = pat
			} else {
				metadata["secretHash"] = "PRIVATE"
				metadata["token"] = "PRIVATE"
				body = map[string]any{"items": []any{metadata}}
			}
		case "/internal/iam/v1/users/alice/credentials/" + id:
			operations++
			revoked, status = true, 204
		case "/internal/iam/v1/users/bob/credentials/" + id:
			operations++
			status = 204 // Deleting an absent credential in Bob's scope must not revoke Alice's.
		case "/apis/ebs/v1/projects":
			operations++
			body = map[string]any{"items": []any{}}
		default:
			t.Fatalf("unexpected upstream operation %s %s", r.Method, r.URL.Path)
		}
		data, _ := json.Marshal(body)
		if status == 204 {
			data = nil
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(data))}, nil
	})
	api := newTestAPIWithConfig(t, transport, 200, oauth.Config{PublicBaseURL: "https://ebs.example.com"})
	router := api.Router()
	jwt, _ := api.tokens.Issue("alice", identity.UserType, identity.AdminScope, time.Hour, now)
	bobJWT, _ := api.tokens.Issue("bob", identity.UserType, identity.AdminScope, time.Hour, now)
	runner, _ := api.tokens.Issue("runner", identity.RunnerType, "", time.Hour, now)
	service, _ := api.tokens.Issue("service", identity.ServiceType, "", time.Hour, now)
	call := func(method, path, token, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != want || strings.Contains(rec.Body.String(), "PRIVATE") {
			t.Fatalf("%s %s: status=%d want=%d", method, path, rec.Code, want)
		}
		return rec
	}
	created := call("POST", "/auth/credentials", jwt, `{"name":"CLI"}`, 201)
	if !strings.Contains(created.Body.String(), pat) || created.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing one-time token or no-store header")
	}
	before := lookups
	listed := call("GET", "/auth/credentials", pat, "", 200)
	if lookups != before+1 || strings.Contains(listed.Body.String(), "token") || strings.Contains(listed.Body.String(), "secretHash") {
		t.Fatal("PAT queried User twice or list exposed secret fields")
	}
	for _, currentScope := range []string{"ebs:admin", "ebs:user", "ebs:ops"} {
		scope = currentScope
		checked := call("POST", "/auth/check", pat, "", 200)
		if !strings.Contains(checked.Body.String(), currentScope) || !strings.Contains(checked.Body.String(), now.Add(90*24*time.Hour).Format(time.RFC3339)) {
			t.Fatal("check did not use current IAM role and PAT expiry")
		}
	}
	scope = "ebs:admin"
	before = operations
	for _, tc := range []struct {
		method, path, token, body string
		status                    int
	}{
		{"POST", "/auth/credentials", pat, `{"name":"derived"}`, 403},
		{"PUT", "/auth/users/alice/password", pat, `{}`, 403},
		{"PUT", "/apis/iam.ebs/v1/users/bob", pat, `{}`, 403},
		{"PATCH", "/apis/iam.ebs/v1/users/bob", pat, `{}`, 403},
		{"POST", "/auth/credentials", runner, `{}`, 403},
		{"GET", "/auth/credentials", service, "", 403},
		{"DELETE", "/auth/credentials/" + id, runner, "", 403},
		{"GET", "/auth/credentials", "", "", 401},
		{"GET", "/auth/credentials?watch=true", pat, "", 403},
		{"PUT", "/auth/credentials", jwt, `{}`, 404},
		{"PATCH", "/auth/credentials/" + id, jwt, `{}`, 404},
		{"GET", "/auth/credentials", pat, `{}`, 400},
		{"DELETE", "/auth/credentials/" + id, pat, `{}`, 400},
		{"DELETE", "/auth/credentials/invalid", pat, "", 400},
		{"POST", "/auth/credentials", jwt, `{"name":"CLI","username":"bob"}`, 400},
		{"POST", "/auth/credentials", jwt, `{"name":"CLI","scopes":["ebs:admin"]}`, 400},
		{"POST", "/auth/credentials", jwt, `{}` + strings.Repeat(" ", 64<<10), 400},
		{"GET", "/apis/ebs/v1/service-endpoints", pat, "", 403},
	} {
		call(tc.method, tc.path, tc.token, tc.body, tc.status)
	}
	if operations != before {
		t.Fatal("denied request reached a business operation")
	}
	call("DELETE", "/auth/credentials/"+id, bobJWT, "", 204)
	call("POST", "/auth/check", pat, "", 200)
	call("GET", "/apis/ebs/v1/projects", pat, "", 200)
	call("DELETE", "/auth/credentials/"+id, pat, "", 204)
	call("POST", "/auth/check", pat, "", 401)
	call("GET", "/apis/ebs/v1/projects", pat, "", 401)
	call("GET", "/apis/ebs/v1/projects/demo/jobs?watch=true", pat, "", 401)
	call("DELETE", "/auth/credentials/"+id, jwt, "", 204)
	unavailable = true
	call("GET", "/apis/ebs/v1/projects", pat, "", 503)
	before = lookups
	call("GET", "/apis/ebs/v1/projects", "ebs_pat_v2.bad", "", 401)
	if before != lookups {
		t.Fatal("invalid PAT format reached IAM")
	}
	klog.Flush()
	if strings.Contains(audit.String(), pat) || strings.Contains(audit.String(), "PRIVATE") || !strings.Contains(audit.String(), `authentication_method="credential"`) {
		t.Fatal("audit leaked credentials or omitted authentication method")
	}
}

func TestPersonalCredentialWatchRevocation(t *testing.T) {
	var revoked atomic.Bool
	id := strings.Repeat("c", 32)
	pat := "ebs_pat_v1.YWxpY2U." + id + "." + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	now := time.Unix(1790000000, 0)
	readEnd, writeEnd := io.Pipe()
	defer readEnd.Close()
	defer writeEnd.Close()
	api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status, body := 200, ""
		switch r.URL.Path {
		case "/internal/iam/v1/users/authenticate":
			if revoked.Load() {
				status, body = 401, `{"code":"InvalidCredential"}`
			} else {
				data, _ := json.Marshal(map[string]any{"authenticated": true, "username": "alice", "uid": "uid", "scopes": []string{"ebs:user"}, "credentialID": id, "expiresAt": now.Add(90 * 24 * time.Hour)})
				body = string(data)
			}
		case "/apis/ebs/v1/projects/demo":
			body = `{"metadata":{"name":"demo","labels":{"ebs.io/owner-user":"alice"}}}`
		case "/apis/ebs/v1/projects/demo/jobs":
			if r.URL.Query().Get("watch") != "true" || r.Header.Get("Authorization") != "" {
				t.Error("invalid forwarded Watch")
			}
			go func() {
				defer writeEnd.Close()
				_, _ = io.WriteString(writeEnd, "{\"type\":\"ADDED\"}\n")
				revoked.Store(true)
				_, _ = io.WriteString(writeEnd, "{\"type\":\"MODIFIED\"}\n")
			}()
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: readEnd}, nil
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	router := api.Router()
	for _, want := range []int{200, 401} {
		req := httptest.NewRequest("GET", "/apis/ebs/v1/projects/demo/jobs?watch=true", nil)
		req.Header.Set("Authorization", "Bearer "+pat)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != want || (want == 200 && !strings.Contains(rec.Body.String(), "MODIFIED")) {
			t.Fatalf("Watch status=%d want=%d", rec.Code, want)
		}
	}
}

func TestPersonalCredentialSharesUserRateLimit(t *testing.T) {
	now := time.Unix(1790000000, 0)
	id := strings.Repeat("b", 32)
	pat := "ebs_pat_v1.YWxpY2U." + id + "." + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	api := newTestAPIWithConfig(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"metadata":{"name":"alice"},"spec":{"enabled":true,"scopes":["ebs:user"]}}`
		if r.URL.Path == "/internal/iam/v1/users/authenticate" {
			data, _ := json.Marshal(map[string]any{"authenticated": true, "username": "alice", "uid": "uid", "scopes": []string{"ebs:user"}, "credentialID": id, "expiresAt": now.Add(90 * 24 * time.Hour)})
			body = string(data)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	}), 1, oauth.Config{})
	jwt, _ := api.tokens.Issue("alice", identity.UserType, identity.UserScope, time.Hour, now)
	router := api.Router()
	for i, token := range []string{jwt, pat} {
		req := httptest.NewRequest("POST", "/auth/check", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if (i == 0 && rec.Code != 200) || (i == 1 && rec.Code != 429) {
			t.Fatalf("request %d status=%d", i, rec.Code)
		}
	}
}

func TestAuthenticationMode(t *testing.T) {
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("disabled authentication called IAM")
		return nil, nil
	})
	router := newTestAPI(t, transport).Router()
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/auth/oauth/community/login", 404},
		{"POST", "/auth/oauth/community/callback", 404},
		{"POST", "/auth/oauth/register", 404},
	} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))
		if response.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.path, response.Code, response.Body.String())
		}
	}
	for _, enabled := range []bool{false, true} {
		var cfg oauth.Config
		if enabled {
			cfg.PublicBaseURL = "https://ebs.example.com"
			cfg.Provider.ID = "community"
		}
		router := newTestAPIWithConfig(t, transport, 200, cfg).Router()
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/auth/providers", nil))
		var providers struct {
			PasswordLoginEnabled bool `json:"passwordLoginEnabled"`
			RegistrationEnabled  bool `json:"registrationEnabled"`
			Providers            []struct {
				ID string `json:"id"`
			} `json:"providers"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &providers); err != nil {
			t.Fatal(err)
		}
		if response.Code != 200 || providers.PasswordLoginEnabled != !enabled || providers.RegistrationEnabled != !enabled {
			t.Fatalf("OAuth enabled=%v: %s", enabled, response.Body.String())
		}
		if enabled && (len(providers.Providers) != 1 || providers.Providers[0].ID != "community") || !enabled && len(providers.Providers) != 0 {
			t.Fatalf("OAuth enabled=%v: unexpected providers %v", enabled, providers.Providers)
		}
		for _, path := range []string{"/auth/login", "/auth/register"} {
			response = httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
			want := http.StatusBadRequest
			if enabled {
				want = http.StatusForbidden
			}
			if response.Code != want {
				t.Fatalf("OAuth enabled=%v %s: %d %s", enabled, path, response.Code, response.Body.String())
			}
		}
	}
}

func (a *testAPI) Router() *gin.Engine { return NewRouter(a.Handler, a.authHandler, a.oauthHandler) }

func TestPublicOpenAPIRoutes(t *testing.T) {
	api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("document requested upstream %s", r.URL)
		return nil, nil
	}))
	router := api.Router()
	var document struct {
		Paths map[string]map[string]struct {
			Parameters []struct {
				Name string `json:"name"`
			} `json:"parameters"`
		} `json:"paths"`
	}
	var body string
	for _, authorization := range []string{"", "Bearer expired"} {
		request := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
		request.Header.Set("Authorization", authorization)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != 200 || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") || response.Header().Get("Cache-Control") != "no-cache" {
			t.Fatalf("document response=%d %v", response.Code, response.Header())
		}
		if body != "" && response.Body.String() != body {
			t.Fatal("document varies by identity")
		}
		body = response.Body.String()
	}
	if err := json.Unmarshal([]byte(body), &document); err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	rules := registeredRoutePolicies(api)
	for _, route := range router.Routes() {
		if _, ok := rules[middleware.RouteKey{Method: route.Method, Path: route.Path}]; route.Path != "/healthz" && route.Path != "/auth/check" && !ok {
			t.Fatalf("route has no identity admission policy: %s %s", route.Method, route.Path)
		}
		if route.Method == http.MethodHead && strings.HasPrefix(route.Path, "/apis/") {
			t.Fatalf("management route unexpectedly supports HEAD: %s", route.Path)
		}
		segments := strings.Split(route.Path, "/")
		for i, segment := range segments {
			if strings.HasPrefix(segment, ":") {
				segments[i] = "{" + segment[1:] + "}"
			}
		}
		path := strings.Join(segments, "/")
		registered[route.Method+" "+path] = true
		if _, ok := document.Paths[path][strings.ToLower(route.Method)]; !ok && path != "/healthz" && path != "/openapi.json" {
			t.Fatalf("undocumented Gateway route: %s %s", route.Method, path)
		}
	}
	for path, methods := range document.Paths {
		for method, operation := range methods {
			if !registered[strings.ToUpper(method)+" "+path] {
				t.Fatalf("undispatched operation: %s %s", method, path)
			}
			for _, parameter := range operation.Parameters {
				if parameter.Name != "watch" {
					continue
				}
				if method != "get" || (path != "/apis/ebs/v1/projects/{project}/jobs" && path != "/apis/ebs/v1/runners/{runner}/jobs") {
					t.Fatalf("unsupported documented watch: %s %s", method, path)
				}
			}
		}
	}
	for path, methods := range map[string]string{
		"/auth/login": "post", "/auth/register": "post", "/auth/machine-token": "post", "/auth/check": "post",
		"/auth/users/{name}/password": "put", "/auth/machineaccounts": "post",
		"/apis/ebs/v1/projects/{project}/builds":               "get post",
		"/apis/ebs/v1/projects/{project}/builds/{name}":        "get delete",
		"/apis/ebs/v1/projects/{project}/builds/{name}/status": "get",
		"/apis/ebs/v1/projects/{project}/builds/{name}/abort":  "post",
		"/apis/ebs/v1/projects/{project}/jobs":                 "get",
		"/apis/ebs/v1/projects/{project}/jobs/{name}":          "get",
		"/apis/ebs/v1/projects/{project}/jobs/{name}/status":   "get patch",
		"/apis/ebs/v1/projects/{project}/jobs/{name}/abort":    "post",
		"/apis/ebs/v1/scripts":                                 "get post",
		"/apis/ebs/v1/scripts/{name}":                          "get put patch delete",
		"/apis/ebs/v1/runners/{runner}/jobs":                   "get",
		"/apis/ebs/v1/service-endpoints":                       "get",
		"/apis/iam.ebs/v1/users":                               "get",
		"/apis/iam.ebs/v1/machineaccounts":                     "get",
		"/apis/iam.ebs/v1/machineaccounts/{name}":              "get delete put patch",
	} {
		if len(document.Paths[path]) != len(strings.Fields(methods)) {
			t.Fatalf("methods for %s: %v", path, document.Paths[path])
		}
		for _, method := range strings.Fields(methods) {
			if _, ok := document.Paths[path][method]; !ok {
				t.Fatalf("missing %s %s", method, path)
			}
		}
	}
	for _, resource := range []string{"snapshots", "buildinfos", "buildgraphs", "rpmrepos", "oldjobs"} {
		for _, suffix := range []string{"", "/{name}", "/{name}/status"} {
			if resource == "oldjobs" && suffix == "/{name}/status" {
				continue
			}
			path := "/apis/ebs/v1/projects/{project}/" + resource + suffix
			if _, ok := document.Paths[path]["get"]; !ok || len(document.Paths[path]) != 1 {
				t.Fatalf("read-only contract for %s: %v", path, document.Paths[path])
			}
		}
	}
}

func newTestAPI(t *testing.T, transport http.RoundTripper) *testAPI {
	t.Helper()
	return newTestAPIWithConfig(t, transport, 200, oauth.Config{})
}

func newTestAPIWithConfig(t *testing.T, transport http.RoundTripper, burst int, cfg oauth.Config) *testAPI {
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
	accounts := iamclient.New(client)
	api, err := NewHandler(Dependencies{
		Upstream: client, Auth: middleware.NewAuth(tokens, accounts, now), Now: now,
		Limits: limit.New(100, burst, now), BodyLimit: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	authHandler, err := auth.New(auth.Options{IAM: accounts, Tokens: tokens, Now: now, LocalAuthEnabled: cfg.PublicBaseURL == ""})
	if err != nil {
		t.Fatal(err)
	}
	oauthHandler, err := oauth.New(oauth.Options{Config: cfg, IAM: accounts, Tokens: tokens, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return &testAPI{Handler: api, authHandler: authHandler, oauthHandler: oauthHandler, tokens: tokens, now: now}
}

func TestServiceTokenChecksCurrentAccount(t *testing.T) {
	status := http.StatusOK
	account := `{"metadata":{"name":"service","uid":"uid-1"},"spec":{"type":"service"}}`
	lookups := 0
	api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		lookups++
		if r.Method != http.MethodGet || r.URL.Path != "/apis/iam.ebs/v1/machineaccounts/service" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(account))}, nil
	}))
	token, err := api.tokens.Issue("service", identity.ServiceType, "", time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	router := api.Router()
	for _, tc := range []struct {
		name, body   string
		status, want int
	}{
		{"current", account, 200, 200},
		{"name only", `{"metadata":{"name":"service"},"spec":{"type":"service"}}`, 200, 200},
		{"deleted", `{}`, 404, 401},
		{"recreated", `{"metadata":{"name":"service","uid":"uid-2"},"spec":{"type":"service"}}`, 200, 200},
		{"wrong name", `{"metadata":{"name":"other","uid":"uid-1"},"spec":{"type":"service"}}`, 200, 401},
		{"wrong type", `{"metadata":{"name":"service","uid":"uid-1"},"spec":{"type":"runner"}}`, 200, 401},
		{"unavailable", `{}`, 503, 503},
		{"malformed", `{}`, 200, 503},
		{"malformed projects", `{"metadata":{"name":"service"},"spec":{"type":"service","projects":"team"}}`, 200, 503},
		{"invalid project", `{"metadata":{"name":"service"},"spec":{"type":"service","projects":["demo-*"]}}`, 200, 503},
		{"mixed wildcard scope", `{"metadata":{"name":"service"},"spec":{"type":"service","projects":["demo","*"],"permissions":["builds:create"]}}`, 200, 503},
		{"duplicate project", `{"metadata":{"name":"service"},"spec":{"type":"service","projects":["team","team"]}}`, 200, 503},
		{"unknown permission", `{"metadata":{"name":"service"},"spec":{"type":"service","permissions":["projects:update","projects:delete"]}}`, 200, 503},
		{"duplicate permission", `{"metadata":{"name":"service"},"spec":{"type":"service","permissions":["projects:update","projects:update"]}}`, 200, 503},
		{"malformed permissions", `{"metadata":{"name":"service"},"spec":{"type":"service","permissions":{}}}`, 200, 503},
		{"recovered", account, 200, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, account = tc.status, tc.body
			before := lookups
			req := httptest.NewRequest(http.MethodPost, "/auth/check", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tc.want || lookups != before+1 {
				t.Fatalf("status=%d lookups=%d body=%s", rec.Code, lookups-before, rec.Body)
			}
			if tc.want == http.StatusOK {
				var result struct {
					Authenticated bool
					Identity      struct {
						Type, Name string
						Scopes     []string
					}
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || !result.Authenticated || result.Identity.Type != "service" || result.Identity.Name != "service" || result.Identity.Scopes == nil || len(result.Identity.Scopes) != 0 {
					t.Fatalf("check response=%s err=%v", rec.Body, err)
				}
			}
		})
	}
}

func TestRegisterRoutesMiddlewareOrder(t *testing.T) {
	for _, reject := range []bool{false, true} {
		var calls []string
		definitions := []routeDefinition{{
			Method:         http.MethodGet,
			Path:           "/test/:name",
			Authentication: middleware.AuthenticationNone,
			Middlewares: []gin.HandlerFunc{
				func(c *gin.Context) {
					calls = append(calls, "first:"+c.Param("name"))
					c.Next()
					calls = append(calls, "after")
				},
				func(c *gin.Context) {
					calls = append(calls, "second")
					if reject {
						c.AbortWithStatus(http.StatusForbidden)
						return
					}
					c.Next()
				},
			},
			Handler: func(c *gin.Context) {
				calls = append(calls, "handler")
				c.Status(http.StatusNoContent)
			},
		}}
		router := gin.New()
		registerRoutes(router, make(map[middleware.RouteKey]middleware.RoutePolicy), definitions)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/test/item", nil))
		want := []string{"first:item", "second", "handler", "after"}
		status := http.StatusNoContent
		if reject {
			want = []string{"first:item", "second", "after"}
			status = http.StatusForbidden
		}
		if response.Code != status || !reflect.DeepEqual(calls, want) {
			t.Fatalf("reject=%v status=%d calls=%v, want %d %v", reject, response.Code, calls, status, want)
		}
	}
}

func TestRouteRegistrationRejectsDuplicateAcrossGroups(t *testing.T) {
	router := gin.New()
	rules := make(map[middleware.RouteKey]middleware.RoutePolicy)
	routes := []routeDefinition{{
		Method:         http.MethodGet,
		Path:           "/test",
		Authentication: middleware.AuthenticationNone,
		Handler:        func(c *gin.Context) { c.Status(http.StatusNoContent) },
	}}
	registerRoutes(router, rules, routes)
	defer func() {
		value := recover()
		if value == nil || !strings.Contains(fmt.Sprint(value), "duplicate Gateway route: GET /test") {
			t.Fatalf("unexpected panic: %v", value)
		}
	}()
	registerRoutes(router, rules, routes)
}

func TestRouteRegistrationValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*routeDefinition)
		want   string
	}{
		{"valid", func(r *routeDefinition) {}, ""},
		{"anonymous default", func(r *routeDefinition) {
			r.Authentication = middleware.AuthenticationNone
			r.Authorization.Identities = 0
		}, ""},
		{"missing access", func(r *routeDefinition) { r.Authorization.Identities = 0 }, "identities"},
		{"unknown auth", func(r *routeDefinition) { r.Authentication = 99 }, "authentication mode"},
		{"required anonymous", func(r *routeDefinition) { r.Authorization.Identities |= policy.AllowAnonymous }, "required authentication"},
		{"optional without anonymous", func(r *routeDefinition) { r.Authentication = middleware.AuthenticationOptional }, "optional authentication"},
		{"no auth with users", func(r *routeDefinition) { r.Authentication = middleware.AuthenticationNone }, "unauthenticated routes"},
		{"unknown identity", func(r *routeDefinition) { r.Authorization.Identities = 128 }, "identities"},
		{"unknown ownership", func(r *routeDefinition) { r.Authorization.ObjectAccess = "unknown" }, "object access"},
		{"missing service permission", func(r *routeDefinition) { r.Authorization.Identities = policy.AllowService }, "ServicePermission"},
		{"invalid service permission", func(r *routeDefinition) {
			r.Authorization.Identities = policy.AllowService
			r.Authorization.ServicePermission = "unknown"
		}, "ServicePermission"},
		{"unused service permission", func(r *routeDefinition) { r.Authorization.ServicePermission = "builds:create" }, "allowed Service"},
		{"missing project parameter", func(r *routeDefinition) { r.Authorization.ObjectAccess = policy.ProjectOwner }, ":project"},
		{"missing runner parameter", func(r *routeDefinition) { r.Authorization.ObjectAccess = policy.RunnerSelf }, ":runner"},
		{"missing job parameters", func(r *routeDefinition) { r.Authorization.ObjectAccess = policy.AssignedJob }, ":project and :name"},
		{"unused watch permission", func(r *routeDefinition) { r.Authorization.WatchAccess = policy.ProjectMember }, "Watch access requires"},
		{"missing service project", func(r *routeDefinition) {
			r.Authorization.Identities = policy.AllowService
			r.Authorization.ServicePermission = "builds:create"
		}, "service permission requires"},
		{"partial anonymous rule", func(r *routeDefinition) {
			r.Authentication = middleware.AuthenticationNone
			r.Authorization = policy.Rule{ObjectAccess: policy.ProjectMember}
		}, "identities"},
		{"negative limit", func(r *routeDefinition) { r.Options.ListLimit = -1 }, "list limit"},
		{"write pagination", func(r *routeDefinition) { r.Method = http.MethodPost; r.Options.ListLimit = 100 }, "list limit"},
		{"write watch", func(r *routeDefinition) { r.Method = http.MethodPost; r.Options.Watch = true }, "Watch requires"},
		{"unauthenticated watch", func(r *routeDefinition) {
			r.Options.Watch = true
			r.Authentication = middleware.AuthenticationNone
			r.Authorization.Identities = policy.AllowAnonymous
		}, "Watch requires"},
		{"missing handler", func(r *routeDefinition) { r.Handler = nil }, "handler"},
		{"nil middleware", func(r *routeDefinition) { r.Middlewares = []gin.HandlerFunc{nil} }, "nil Gateway middleware"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route := routeDefinition{
				Method:        http.MethodGet,
				Path:          "/test",
				Authorization: policy.Rule{Identities: policy.AllowUsers},
				Handler:       func(c *gin.Context) { c.Status(http.StatusNoContent) },
			}
			tc.change(&route)
			defer func() {
				value := recover()
				if tc.want == "" {
					if value != nil {
						t.Fatalf("unexpected panic: %v", value)
					}
				} else if value == nil || !strings.Contains(fmt.Sprint(value), tc.want) ||
					!strings.Contains(fmt.Sprint(value), route.Method+" "+route.Path) {
					t.Fatalf("panic=%v, want route and %q", value, tc.want)
				}
			}()
			registerRoutes(gin.New(), make(map[middleware.RouteKey]middleware.RoutePolicy), []routeDefinition{route})
		})
	}
}

func TestServiceTokenRejectsOperationsOutsideAllowlist(t *testing.T) {
	api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Path != "/apis/iam.ebs/v1/machineaccounts/service" {
			t.Fatalf("service request reached resource or user API: %s %s", r.Method, r.URL)
		}
		body := `{"metadata":{"name":"service"},"spec":{"type":"service","projects":["team"],"permissions":["projects:update","builds:create","builds:abort"]}}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	token, err := api.tokens.Issue("service", identity.ServiceType, "", time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	router := api.Router()
	for _, tc := range []struct{ method, path string }{
		{"GET", "/apis/ebs/v1/projects"},
		{"GET", "/apis/ebs/v1/projects/team"},
		{"POST", "/apis/ebs/v1/projects"},
		{"GET", "/apis/ebs/v1/projects/team/builds"},
		{"DELETE", "/apis/ebs/v1/projects/team/builds/build-1"},
		{"POST", "/apis/ebs/v1/projects/team/jobs/job-1/abort"},
		{"GET", "/apis/ebs/v1/configs/services"},
		{"POST", "/apis/ebs/v1/scripts"},
		{"GET", "/apis/ebs/v1/runners/service/jobs?watch=true"},
		{"GET", "/apis/ebs/v1/service-endpoints"},
		{"GET", "/apis/iam.ebs/v1/machineaccounts"},
		{"PATCH", "/apis/iam.ebs/v1/machineaccounts/service"},
		{"PUT", "/apis/iam.ebs/v1/machineaccounts/service"},
		{"POST", "/auth/machineaccounts"},
		{"PUT", "/auth/users/service/password"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
		})
	}
}

func TestServiceTokenAllowedWrites(t *testing.T) {
	const project = `{"apiVersion":"ebs/v1","kind":"Project","metadata":{"name":"team","uid":"project-uid","resourceVersion":"v1:1:1","labels":{"ebs.io/owner-user":"owner"}},"spec":{"description":"old"}}`
	for _, test := range []struct {
		name, method, path, media, body string
		status                          int
		forward                         bool
	}{
		{"project put", "PUT", "/apis/ebs/v1/projects/team", "application/json", strings.Replace(project, "old", "new", 1), 200, true},
		{"project merge patch", "PATCH", "/apis/ebs/v1/projects/team", "application/merge-patch+json", `{"spec":{"description":"new"}}`, 200, true},
		{"project json patch", "PATCH", "/apis/ebs/v1/projects/team", "application/json-patch+json", `[{"op":"replace","path":"/spec/description","value":"new"}]`, 200, true},
		{"owner immutable", "PATCH", "/apis/ebs/v1/projects/team", "application/merge-patch+json", `{"metadata":{"labels":{"ebs.io/owner-user":"service"}}}`, 403, false},
		{"purpose protected", "PATCH", "/apis/ebs/v1/projects/team", "application/merge-patch+json", `{"metadata":{"labels":{"ebs.io/build-purpose":"gating"}}}`, 403, false},
		{"uid protected", "PATCH", "/apis/ebs/v1/projects/team", "application/merge-patch+json", `{"metadata":{"uid":"another"}}`, 403, false},
		{"stale version", "PATCH", "/apis/ebs/v1/projects/team", "application/json-patch+json", `[{"op":"test","path":"/metadata/resourceVersion","value":"v1:0:1"}]`, 409, false},
		{"build create", "POST", "/apis/ebs/v1/projects/team/builds", "application/json", `{"metadata":{"name":"b","namespace":"team"},"spec":{"type":"single","arch":"aarch64","packages":["pkg"]}}`, 201, true},
		{"build abort", "POST", "/apis/ebs/v1/projects/team/builds/b/abort", "application/json", `{}`, 200, true},
		{"namespace mismatch", "POST", "/apis/ebs/v1/projects/team/builds", "application/json", `{"metadata":{"name":"b","namespace":"other"}}`, 400, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			forwarded := false
			lookups := 0
			api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				status, body := http.StatusOK, project
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/apis/iam.ebs/v1/machineaccounts/service":
					lookups++
					body = `{"metadata":{"name":"service"},"spec":{"type":"service","projects":["team"],"permissions":["projects:update","builds:create","builds:abort"]}}`
				case r.Method == http.MethodGet && r.URL.Path == "/apis/ebs/v1/projects/team":
				case r.Method == http.MethodPut && r.URL.Path == "/apis/ebs/v1/projects/team":
					forwarded = true
					var obj struct {
						Metadata struct {
							UID, ResourceVersion string
							Labels               map[string]string
						}
						Spec struct{ Description string }
					}
					if err := json.NewDecoder(r.Body).Decode(&obj); err != nil {
						t.Fatal(err)
					}
					if obj.Spec.Description != "new" || obj.Metadata.UID != "project-uid" ||
						obj.Metadata.ResourceVersion != "v1:1:1" || obj.Metadata.Labels[ebsv1.ProjectOwnerUserLabel] != "owner" {
						t.Fatalf("forwarded project = %+v", obj)
					}
				case r.Method == http.MethodPost && r.URL.Path == "/apis/ebs/v1/projects/team/builds":
					forwarded = true
					raw, err := io.ReadAll(r.Body)
					if err != nil || string(raw) != test.body {
						t.Fatalf("create body=%s err=%v", raw, err)
					}
					status, body = test.status, test.body
				case r.Method == http.MethodPost && r.URL.Path == "/apis/ebs/v1/projects/team/builds/b/abort":
					forwarded = true
					raw, err := io.ReadAll(r.Body)
					if err != nil || string(raw) != test.body {
						t.Fatalf("abort body=%s err=%v", raw, err)
					}
					body = `{"metadata":{"name":"b","namespace":"team"},"status":{"phase":"Aborted"}}`
				default:
					t.Fatalf("unexpected upstream request: %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Authorization") != "" {
					t.Fatal("caller token forwarded to apiserver")
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			token, err := api.tokens.Issue("service", identity.ServiceType, "", time.Hour, api.now())
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Content-Type", test.media)
			rec := httptest.NewRecorder()
			router := api.Router()
			router.ServeHTTP(rec, request)
			if rec.Code != test.status || forwarded != test.forward || lookups != 1 {
				t.Fatalf("status=%d forwarded=%v lookups=%d body=%s", rec.Code, forwarded, lookups, rec.Body)
			}
		})
	}
}

func TestServiceTokenUsesCurrentGrants(t *testing.T) {
	var audit bytes.Buffer
	state := klog.CaptureState()
	t.Cleanup(state.Restore)
	loggingFlags := flag.NewFlagSet("logging", flag.ContinueOnError)
	klog.InitFlags(loggingFlags)
	for name, value := range map[string]string{"logtostderr": "false", "alsologtostderr": "false", "stderrthreshold": "FATAL", "one_output": "true"} {
		if err := loggingFlags.Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	klog.SetOutput(&audit)
	var projects, permissions []string
	lookups, writes := 0, 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status, body := http.StatusOK, `{}`
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/iam.ebs/v1/machineaccounts/service":
			lookups++
			raw, err := json.Marshal(map[string]any{
				"metadata": map[string]string{"name": "service"},
				"spec":     map[string]any{"type": "service", "projects": projects, "permissions": permissions},
			})
			if err != nil {
				t.Fatal(err)
			}
			body = string(raw)
		case r.Method == http.MethodPost && r.URL.Path == "/apis/ebs/v1/projects/team/builds/b/abort":
			writes++
		default:
			t.Fatalf("unexpected upstream request: %s %s", r.Method, r.URL)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	api := newTestAPI(t, transport)
	token, err := api.tokens.Issue("service", identity.ServiceType, "", time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	routers := []*gin.Engine{api.Router(), newTestAPI(t, transport).Router()}
	for _, tc := range []struct {
		name                  string
		projects, permissions []string
		want                  int
	}{
		{"empty", nil, nil, 403},
		{"granted abort only", []string{"team"}, []string{"builds:abort"}, 200},
		{"all projects", []string{"*"}, []string{"builds:abort"}, 200},
		{"all projects without permission", []string{"*"}, nil, 403},
		{"wildcard revoked", []string{"other"}, []string{"builds:abort"}, 403},
		{"revoked permission", []string{"team"}, []string{"builds:create"}, 403},
		{"cross project", []string{"other"}, []string{"builds:abort"}, 403},
		{"no projects", nil, []string{"builds:abort"}, 403},
		{"no permissions", []string{"team"}, nil, 403},
		{"restored", []string{"team", "other"}, []string{"builds:abort"}, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projects, permissions = tc.projects, tc.permissions
			for _, router := range routers {
				audit.Reset()
				beforeLookups, beforeWrites := lookups, writes
				req := httptest.NewRequest(http.MethodPost, "/apis/ebs/v1/projects/team/builds/b/abort", strings.NewReader(`{"reason":"private-abort-reason"}`))
				req.Header.Set("Authorization", "Bearer "+token)
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				if rec.Code != tc.want || lookups != beforeLookups+1 || (writes == beforeWrites+1) != (tc.want == 200) {
					t.Fatalf("status=%d lookups=%d writes=%d body=%s", rec.Code, lookups-beforeLookups, writes-beforeWrites, rec.Body)
				}
				decision := "denied"
				if tc.want == http.StatusOK {
					decision = "allowed"
				}
				klog.Flush()
				for _, field := range []string{`user="service"`, `service_project="team"`, `service_permission="builds:abort"`, `service_authorization="` + decision + `"`} {
					if !strings.Contains(audit.String(), field) {
						t.Fatalf("audit missing %s: %s", field, audit.String())
					}
				}
				if strings.Contains(audit.String(), token) || strings.Contains(audit.String(), "private-abort-reason") {
					t.Fatal("audit contains token or request body")
				}
			}
		})
	}
}

func TestMachineAccountUpdateForwarding(t *testing.T) {
	const path = "/apis/iam.ebs/v1/machineaccounts/automation"
	const original = `{"metadata":{"name":"automation","uid":"id","resourceVersion":"v1:3:1"},"spec":{"type":"service","tokenTTLSeconds":3600}}`
	for _, tc := range []struct {
		name, method, media, body string
		scope                     identity.Scope
		status                    int
	}{
		{"put", "PUT", "application/json", original, identity.AdminScope, 200},
		{"merge patch", "PATCH", "application/merge-patch+json", `{"metadata":{"uid":"id","resourceVersion":"v1:3:1"},"spec":{"projects":["demo"]}}`, identity.AdminScope, 200},
		{"json patch", "PATCH", "application/json-patch+json", `[{"op":"test","path":"/metadata/resourceVersion","value":"v1:3:1"},{"op":"replace","path":"/spec/tokenTTLSeconds","value":7200}]`, identity.AdminScope, 200},
		{"validation failure", "PATCH", "application/merge-patch+json", `{"spec":{"type":"unknown"}}`, identity.AdminScope, 422},
		{"version conflict", "PUT", "application/json", original, identity.AdminScope, 409},
		{"missing account", "PATCH", "application/merge-patch+json", "{}", identity.AdminScope, 404},
		{"malformed body", "PATCH", "application/merge-patch+json", "{", identity.AdminScope, 400},
		{"ordinary user", "PATCH", "application/merge-patch+json", "{}", identity.UserScope, 403},
		{"ops user", "PUT", "application/json", original, identity.OpsScope, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			result := `{"upstream":"response"}`
			api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodGet && r.URL.Path == "/apis/iam.ebs/v1/users/admin" {
					body := `{"metadata":{"name":"admin"},"spec":{"enabled":true,"scopes":["` + string(tc.scope) + `"]}}`
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
				}
				if tc.scope != identity.AdminScope {
					t.Fatal("non-admin request reached upstream")
				}
				writes++
				raw, err := io.ReadAll(r.Body)
				if err != nil || r.Method != tc.method || r.URL.RequestURI() != path+"?fieldValidation=Strict" ||
					r.Header.Get("Content-Type") != tc.media || string(raw) != tc.body {
					t.Fatalf("request changed: %s %s Content-Type=%q body=%s err=%v", r.Method, r.URL, r.Header.Get("Content-Type"), raw, err)
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(result))}, nil
			}))
			token, err := api.tokens.Issue("admin", identity.UserType, tc.scope, time.Hour, api.now())
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(tc.method, path+"?fieldValidation=Strict", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", tc.media)
			res := httptest.NewRecorder()
			api.Router().ServeHTTP(res, req)
			if res.Code != tc.status {
				t.Fatalf("status=%d, want %d: %s", res.Code, tc.status, res.Body)
			}
			if tc.scope == identity.AdminScope {
				if writes != 1 || res.Body.String() != result {
					t.Fatalf("writes=%d body=%s", writes, res.Body)
				}
			} else if writes != 0 {
				t.Fatalf("unauthorized writes=%d", writes)
			}
		})
	}
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
	token, err := api.tokens.Issue("runner", identity.RunnerType, "", time.Hour, api.now())
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

func TestAuthenticatedRoutesShareLimit(t *testing.T) {
	api := newTestAPIWithConfig(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/apis/ebs/v1/runners/runner-1/jobs" && r.URL.Path != "/apis/ebs/v1/scripts/rpmbuild" {
			t.Fatalf("unexpected upstream request: %s", r.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"items":[]}`))}, nil
	}), 2, oauth.Config{})
	token, err := api.tokens.Issue("runner-1", identity.RunnerType, "", time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	router := api.Router()
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/apis/ebs/v1/scripts/rpmbuild", http.StatusOK},
		{http.MethodGet, "/apis/ebs/v1/runners/runner-1/jobs", http.StatusOK},
		{http.MethodGet, "/apis/ebs/v1/build-targets", http.StatusTooManyRequests},
		{http.MethodGet, "/apis/ebs/v1/configs?watch=true", http.StatusForbidden},
		{http.MethodGet, "/apis/ebs/v1/scripts/rpmbuild?watch=true", http.StatusForbidden},
		{http.MethodGet, "/apis/ebs/v1/configs", http.StatusTooManyRequests},
		{http.MethodPost, "/apis/ebs/v1/scripts", http.StatusTooManyRequests},
		{http.MethodPost, "/auth/check", http.StatusTooManyRequests},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		if res.Code != tc.status || (tc.status == http.StatusTooManyRequests && res.Header().Get("Retry-After") != "1") {
			t.Fatalf("%s: status=%d Retry-After=%q", tc.path, res.Code, res.Header().Get("Retry-After"))
		}
	}
}

func TestIAMRouteGuards(t *testing.T) {
	for _, tc := range []struct {
		scope identity.Scope
		query string
		want  int
	}{
		{identity.UserScope, "", http.StatusForbidden},
		{identity.OpsScope, "", http.StatusForbidden},
		{identity.AdminScope, "?watch=false&watch=true", http.StatusForbidden},
		{identity.AdminScope, "?watch=false", http.StatusOK},
	} {
		t.Run(string(tc.scope)+tc.query, func(t *testing.T) {
			api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := `{"items":[]}`
				if r.URL.Path == "/apis/iam.ebs/v1/users/operator" {
					body = fmt.Sprintf(`{"metadata":{"name":"operator"},"spec":{"enabled":true,"scopes":[%q]}}`, tc.scope)
				} else if tc.want != http.StatusOK {
					t.Fatalf("denied request reached upstream: %s", r.URL)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			token, err := api.tokens.Issue("operator", identity.UserType, tc.scope, time.Hour, api.now())
			if err != nil {
				t.Fatal(err)
			}
			router := api.Router()
			for _, resource := range []string{"users", "machineaccounts"} {
				req := httptest.NewRequest(http.MethodGet, "/apis/iam.ebs/v1/"+resource+tc.query, nil)
				req.Header.Set("Authorization", "Bearer "+token)
				res := httptest.NewRecorder()
				router.ServeHTTP(res, req)
				if res.Code != tc.want {
					t.Errorf("%s: status=%d, want %d", resource, res.Code, tc.want)
				}
				if tc.scope == identity.AdminScope && tc.want == http.StatusForbidden &&
					res.Body.String() != `{"error":"watch is not supported for this route"}` {
					t.Errorf("%s: unexpected Watch denial: %s", resource, res.Body)
				}
			}
		})
	}
}

func TestConfigAndScriptServiceRejectionOrder(t *testing.T) {
	api := newTestAPIWithConfig(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/apis/iam.ebs/v1/machineaccounts/service" {
			t.Fatalf("denied request reached upstream: %s", r.URL)
		}
		body := `{"metadata":{"name":"service"},"spec":{"type":"service"}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	}), 2, oauth.Config{})
	token, err := api.tokens.Issue("service", identity.ServiceType, "", time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	router := api.Router()
	for _, tc := range []struct {
		query  string
		status int
	}{
		{"", http.StatusForbidden},
		{"?watch=true", http.StatusForbidden},
		{"", http.StatusTooManyRequests},
	} {
		for _, resource := range []string{"configs", "scripts"} {
			req := httptest.NewRequest(http.MethodPost, "/apis/ebs/v1/"+resource+tc.query, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			res := httptest.NewRecorder()
			router.ServeHTTP(res, req)
			if res.Code != tc.status {
				t.Errorf("%s: status=%d, want %d", req.URL, res.Code, tc.status)
			}
		}
	}
}

func TestWatchRouteAllowlist(t *testing.T) {
	forwarded := 0
	api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{}`
		if r.URL.Path == "/apis/iam.ebs/v1/users/admin" {
			body = `{"metadata":{"name":"admin"},"spec":{"enabled":true,"scopes":["ebs:admin"]}}`
		} else if r.URL.Path == "/apis/ebs/v1/projects/team" {
			body = `{"metadata":{"name":"team","labels":{"ebs.io/owner-user":"admin"}}}`
		} else {
			forwarded++
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	adminToken, err := api.tokens.Issue("admin", identity.UserType, identity.AdminScope, time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	runnerToken, err := api.tokens.Issue("runner-1", identity.RunnerType, "", time.Hour, api.now())
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
		{"/runners/runner-1/jobs?watch=true", adminToken, http.StatusForbidden},
		{"/runners/runner-1/jobs?watch=true", runnerToken, http.StatusOK},
		{"/runners?watch=true", runnerToken, http.StatusForbidden},
		{"/runners/runner-2/jobs?watch=true", runnerToken, http.StatusForbidden},
		{"/projects/team/jobs?watch=true", runnerToken, http.StatusForbidden},
		{"/projects/team/jobs?watch=true", "", http.StatusUnauthorized},
		{"/runners/runner-1/jobs?watch=true", "", http.StatusUnauthorized},
		{"/projects/team/jobs?watch=true", "invalid", http.StatusUnauthorized},
		{"/runners/runner-1/jobs?watch=true", "invalid", http.StatusUnauthorized},
		{"/projects?watch=true", "", http.StatusForbidden},
		{"/projects?watch=true", "invalid", http.StatusForbidden},
		{"/unknown?watch=true", "", http.StatusUnauthorized},
		{"/unknown?watch=true", adminToken, http.StatusNotFound},
		{"/projects?watch=false", adminToken, http.StatusOK},
	}
	for _, path := range []string{
		"/projects", "/projects/team", "/projects/team/builds",
		"/projects/team/snapshots", "/projects/team/buildinfos", "/projects/team/rpmrepos",
		"/projects/team/oldjobs", "/projects/team/buildgraphs", "/configs", "/scripts",
		"/projects/team/jobs/job", "/projects/team/jobs/job/status",
		"/runners", "/runners/runner-1", "/runners/runner-1/status",
		"/build-targets", "/service-endpoints",
	} {
		cases = append(cases, struct {
			path, token string
			status      int
		}{path + "?watch=true", adminToken, http.StatusForbidden})
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

func TestAuthenticatedRequestsRequireCurrentUser(t *testing.T) {
	for _, tc := range []struct {
		name, body      string
		iamStatus, want int
	}{
		{"disabled", `{"metadata":{"name":"alice"},"spec":{"enabled":false,"scopes":["ebs:user"]}}`, http.StatusOK, http.StatusForbidden},
		{"role changed", `{"metadata":{"name":"alice"},"spec":{"enabled":true,"scopes":["ebs:ops"]}}`, http.StatusOK, http.StatusForbidden},
		{"deleted", "", http.StatusNotFound, http.StatusForbidden},
		{"IAM unavailable", "", http.StatusServiceUnavailable, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookups := 0
			api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.Path != "/apis/iam.ebs/v1/users/alice" {
					t.Fatalf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
				}
				lookups++
				return &http.Response{StatusCode: tc.iamStatus, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}))
			token, err := api.tokens.Issue("alice", identity.UserType, identity.UserScope, time.Hour, api.now())
			if err != nil {
				t.Fatal(err)
			}
			router := api.Router()
			for _, endpoint := range []struct{ method, path string }{
				{http.MethodGet, "/apis/iam.ebs/v1/users"},
				{http.MethodGet, "/apis/ebs/v1/projects"},
				{http.MethodPost, "/auth/check"},
			} {
				before := lookups
				req := httptest.NewRequest(endpoint.method, endpoint.path, nil)
				req.Header.Set("Authorization", "Bearer "+token)
				res := httptest.NewRecorder()
				router.ServeHTTP(res, req)
				if res.Code != tc.want || lookups != before+1 {
					t.Errorf("%s %s: status=%d lookups=%d, want %d and one User lookup", endpoint.method, endpoint.path, res.Code, lookups-before, tc.want)
				}
			}
		})
	}
}

func TestBuildTargets(t *testing.T) {
	for _, tc := range []struct {
		name, content, want string
		status              int
	}{
		{"sorted projection", "targets: {z: {arches: {x86_64: {image: hidden}, aarch64: {image: hidden}}}, a: {arches: {arm: {image: hidden}}}}",
			`{"items":[{"distro":"a","arches":["arm"]},{"distro":"z","arches":["aarch64","x86_64"]}]}`, 200},
		{"empty", "targets: {}", `{"items":[]}`, 200},
		{"invalid", "targets: [hidden]", "", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/apis/iam.ebs/v1/users/user" {
					body := `{"metadata":{"name":"user"},"spec":{"enabled":true,"scopes":["ebs:user"]}}`
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
				}
				if r.Method != http.MethodGet || r.URL.RequestURI() != "/apis/ebs/v1/configs/build-target" {
					t.Fatalf("unexpected upstream request: %s %s", r.Method, r.URL)
				}
				body, _ := json.Marshal(map[string]any{"metadata": map[string]string{"name": "build-target"}, "spec": map[string]string{"content": tc.content}})
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))}, nil
			}))
			api.limits = limit.New(1, 1, api.now)
			res := httptest.NewRecorder()
			token, err := api.tokens.Issue("user", identity.UserType, identity.UserScope, time.Hour, api.now())
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "/apis/ebs/v1/build-targets", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			router := api.Router()
			router.ServeHTTP(res, req)
			if res.Code != tc.status || (tc.want != "" && res.Body.String() != tc.want) || strings.Contains(res.Body.String(), "hidden") {
				t.Fatalf("status=%d response=%s", res.Code, res.Body)
			}
			res = httptest.NewRecorder()
			req = httptest.NewRequest(http.MethodGet, "/apis/ebs/v1/build-targets", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			router.ServeHTTP(res, req)
			if res.Code != http.StatusTooManyRequests || res.Header().Get("Retry-After") != "1" {
				t.Fatalf("authenticated limit: status=%d Retry-After=%q", res.Code, res.Header().Get("Retry-After"))
			}
		})
	}
}

func TestServiceEndpointsRejectQuery(t *testing.T) {
	api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("rejected query reached upstream: %s", r.URL)
		return nil, nil
	}))
	token, err := api.tokens.Issue("runner", identity.RunnerType, "", time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	router := api.Router()
	for _, query := range []string{"watch=false", "project=demo"} {
		t.Run(query, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/apis/ebs/v1/service-endpoints?"+query, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			res := httptest.NewRecorder()
			router.ServeHTTP(res, req)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("%s: status=%d, want 400", req.URL, res.Code)
			}
		})
	}
}

func TestGlobalIdentityAndAnonymousLimits(t *testing.T) {
	lookups := 0
	api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/apis/iam.ebs/v1/users/alice" {
			lookups++
			body := `{"metadata":{"name":"alice"},"spec":{"enabled":true,"scopes":["ebs:user"]}}`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		if r.URL.Path != "/apis/ebs/v1/projects" && r.URL.Path != "/apis/ebs/v1/scripts" {
			t.Fatalf("unexpected upstream request: %s", r.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"items":[]}`))}, nil
	}))
	api.limits = limit.New(1, 2, api.now)
	token, err := api.tokens.Issue("alice", identity.UserType, identity.UserScope, time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	router := api.Router()
	for _, tc := range []struct {
		method, path  string
		authenticated bool
		want          int
	}{
		{"GET", "/apis/ebs/v1/projects", false, 200},
		{"GET", "/openapi.json", false, 200},
		{"GET", "/auth/providers", false, 429},
		{"GET", "/healthz", false, 200},
		{"GET", "/apis/ebs/v1/projects", true, 200},
		{"GET", "/apis/ebs/v1/scripts", true, 200},
		{"POST", "/auth/check", true, 429},
		{"GET", "/apis/ebs/v1/projects", true, 429},
	} {
		request := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.authenticated {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		before := lookups
		res := httptest.NewRecorder()
		router.ServeHTTP(res, request)
		if res.Code != tc.want {
			t.Fatalf("%s: status=%d, want %d", tc.path, res.Code, tc.want)
		}
		wantLookups := 0
		if tc.authenticated {
			wantLookups = 1
		}
		if lookups-before != wantLookups {
			t.Fatalf("%s: IAM lookups=%d, want %d", tc.path, lookups-before, wantLookups)
		}
		if tc.want == 429 && res.Header().Get("Retry-After") != "1" {
			t.Fatal("missing retry delay")
		}
	}
}

func TestRegistrationUsesGlobalLimit(t *testing.T) {
	calls := 0
	api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/iam/v1/users/register" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
		}
		calls++
		return &http.Response{StatusCode: http.StatusConflict, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	}))
	api.limits = limit.New(1, 4, api.now)
	router := api.Router()
	for i := 0; i < 5; i++ {
		request := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(`{"username":"alice","password":"valid-password"}`))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		want := http.StatusConflict
		if i == 4 {
			want = http.StatusTooManyRequests
			if response.Header().Get("Retry-After") != "1" {
				t.Fatal("missing global-limit retry delay")
			}
		}
		if response.Code != want {
			t.Fatalf("request %d: status=%d want=%d body=%s", i, response.Code, want, response.Body)
		}
	}
	if calls != 4 {
		t.Fatalf("IAM calls=%d, want 4", calls)
	}
}

func TestHealthAndBusinessMiddlewareChains(t *testing.T) {
	var audit bytes.Buffer
	state := klog.CaptureState()
	t.Cleanup(state.Restore)
	loggingFlags := flag.NewFlagSet("logging", flag.ContinueOnError)
	klog.InitFlags(loggingFlags)
	for name, value := range map[string]string{"logtostderr": "false", "alsologtostderr": "false", "stderrthreshold": "FATAL", "one_output": "true"} {
		if err := loggingFlags.Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	klog.SetOutput(&audit)
	lookups := 0
	api := newTestAPI(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		lookups++
		panic("test account lookup panic")
	}))
	api.limits = limit.New(1, 1, api.now)
	router := api.Router()
	for _, exhausted := range []bool{false, true} {
		if exhausted {
			if !api.limits.Allow("anonymous/192.0.2.1") {
				t.Fatal("health requests consumed rate limit allowance")
			}
		}
		for _, path := range []string{"/healthz", "/healthz?watch=true"} {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.RemoteAddr = "192.0.2.1:1234"
			request.Header.Set("Authorization", "Bearer invalid")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			klog.Flush()
			if response.Code != http.StatusOK || response.Body.String() != "ok\n" || audit.Len() != 0 || lookups != 0 {
				t.Fatalf("probe entered business chain: status=%d body=%q audit=%q lookups=%d", response.Code, response.Body, audit.String(), lookups)
			}
			if response.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
				t.Fatalf("unexpected health response content type: %q", response.Header().Get("Content-Type"))
			}
		}
	}
	for _, methodPath := range []struct{ method, path string }{
		{http.MethodPost, "/healthz"},
		{http.MethodGet, "/healthz/"},
	} {
		request := httptest.NewRequest(methodPath.method, methodPath.path, nil)
		request.RemoteAddr = "192.0.2.1:1234"
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s bypassed business authentication: %d", methodPath.method, methodPath.path, response.Code)
		}
	}

	audit.Reset()
	token, err := api.tokens.Issue("alice", identity.UserType, identity.UserScope, time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/apis/ebs/v1/projects", nil)
	request.RemoteAddr = "192.0.2.2:1234"
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	klog.Flush()
	if response.Code != http.StatusInternalServerError || lookups != 1 {
		t.Fatalf("business panic not recovered: status=%d lookups=%d", response.Code, lookups)
	}
	for _, field := range []string{`"HTTP request"`, `path="/apis/ebs/v1/projects"`, "status=500"} {
		if !strings.Contains(audit.String(), field) {
			t.Fatalf("panic audit missing %s: %s", field, audit.String())
		}
	}
	if strings.Count(audit.String(), `"HTTP request"`) != 1 || strings.Contains(audit.String(), token) {
		t.Fatalf("unexpected panic audit: %s", audit.String())
	}
}

func TestConfigAndScriptForwarding(t *testing.T) {
	for _, resource := range []string{"configs", "scripts"} {
		t.Run(resource, func(t *testing.T) {
			testResourceForwarding(t, resource)
		})
	}
}

func testResourceForwarding(t *testing.T, resource string) {
	t.Helper()
	path := "/apis/ebs/v1/" + resource
	const object = `{"metadata":{"name":"custom"},"spec":{}}`
	for _, tc := range []struct {
		name, method, suffix, media, payload string
		status                               int
		upstream                             []string
	}{
		{"list with query", "GET", "?limit=3", "", "", 200, []string{"GET " + path + "?limit=3"}},
		{"create", "POST", "", "application/json", object, 200, []string{"POST " + path}},
		{"create preserves formatting", "POST", "", "application/json", " \n" + object + "\n", 200, []string{"POST " + path}},
		{"create exceeds Gateway body limit", "POST", "", "application/json", `{"metadata":{"name":"custom"},"spec":{"content":"` + strings.Repeat("x", 257) + `"}}`, 200, []string{"POST " + path}},
		{"invalid create rejected upstream", "POST", "", "application/json", "{", 400, []string{"POST " + path}},
		{"create media type rejected upstream", "POST", "", "text/plain", object, 415, []string{"POST " + path}},
		{"replace", "PUT", "/custom", "application/json", " \n" + object + "\n", 200, []string{"PUT " + path + "/custom"}},
		{"merge patch", "PATCH", "/custom", "application/merge-patch+json", `{"spec":{"content":"new"}}`, 200, []string{"PATCH " + path + "/custom"}},
		{"JSON patch", "PATCH", "/custom", "application/json-patch+json", `[{"op":"replace","path":"/spec/content","value":"new"}]`, 200, []string{"PATCH " + path + "/custom"}},
		{"large update", "PUT", "/custom", "application/json", `{"metadata":{"name":"custom"},"spec":{"content":"` + strings.Repeat("x", 257) + `"}}`, 200, []string{"PUT " + path + "/custom"}},
		{"invalid patch rejected upstream", "PATCH", "/custom", "application/merge-patch+json", "{", 400, []string{"PATCH " + path + "/custom"}},
		{"conflict from upstream", "PUT", "/custom", "application/json", object, 409, []string{"PUT " + path + "/custom"}},
		{"delete options", "DELETE", "/custom", "application/json", `{"kind":"DeleteOptions","apiVersion":"v1","preconditions":{"resourceVersion":"v1:1:1"}}`, 200, []string{"DELETE " + path + "/custom"}},
		{"large delete", "DELETE", "/custom", "application/json", strings.Repeat(" ", 257) + "{}", 200, []string{"DELETE " + path + "/custom"}},
		{"delete rejected upstream", "DELETE", "/custom", "application/json", "{", 400, []string{"DELETE " + path + "/custom"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := object
				status := http.StatusOK
				if r.URL.Path == "/apis/iam.ebs/v1/users/ops" {
					body = `{"metadata":{"name":"ops"},"spec":{"enabled":true,"scopes":["ebs:ops"]}}`
				} else {
					calls = append(calls, r.Method+" "+r.URL.RequestURI())
					if r.Header.Get("Authorization") != "" {
						t.Fatal("caller token forwarded")
					}
					if r.Method != http.MethodGet {
						raw, err := io.ReadAll(r.Body)
						if err != nil || string(raw) != tc.payload || r.Header.Get("Content-Type") != tc.media {
							t.Fatalf("write request was modified: body=%q media=%q error=%v", raw, r.Header.Get("Content-Type"), err)
						}
						status = tc.status
						if status != http.StatusOK {
							body = `{"error":"rejected by apiserver"}`
						}
					}
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			api.bodyLimit = 256
			token, err := api.tokens.Issue("ops", identity.UserType, identity.OpsScope, time.Hour, api.now())
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(tc.method, path+tc.suffix, strings.NewReader(tc.payload))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", tc.media)
			res := httptest.NewRecorder()
			api.Router().ServeHTTP(res, req)
			if res.Code != tc.status || !reflect.DeepEqual(calls, tc.upstream) {
				t.Fatalf("status=%d calls=%v, want %d/%v", res.Code, calls, tc.status, tc.upstream)
			}
			if len(tc.upstream) != 0 && tc.status != http.StatusOK && res.Body.String() != `{"error":"rejected by apiserver"}` {
				t.Fatalf("upstream error response was modified: %s", res.Body)
			}
		})
	}
}

func TestConfigAndScriptRouteGuards(t *testing.T) {
	for _, tc := range []struct {
		method, path, caller string
		status               int
	}{
		{"GET", "/build-targets", "user", 200},
		{"GET", "/build-targets", "ops", 200},
		{"GET", "/build-targets", "admin", 200},
		{"GET", "/build-targets", "", 401},
		{"GET", "/build-targets", "invalid", 401},
		{"GET", "/build-targets", "runner", 403},
		{"GET", "/build-targets", "service", 403},
		{"GET", "/build-targets?watch=true", "", 403},
		{"GET", "/build-targets?watch=true", "user", 403},
		{"GET", "/configs/example", "invalid", 401},
		{"GET", "/configs/build-target", "", 401},
		{"GET", "/configs/build-target", "user", 403},
		{"GET", "/configs/build-target", "runner", 403},
		{"GET", "/configs/build-target", "service", 403},
		{"GET", "/configs/example?watch=true", "", 403},
		{"GET", "/configs/example?watch=true", "ops", 403},
		{"GET", "/configs/example", "ops", 200},
		{"GET", "/configs/example", "admin", 200},
		{"GET", "/configs", "", 401},
		{"GET", "/configs", "user", 403},
		{"GET", "/configs", "ops", 200},
		{"PATCH", "/configs/build-target", "ops", 200},
		{"DELETE", "/configs/build-target", "admin", 403},
		{"DELETE", "/configs/build-resource", "admin", 403},
		{"DELETE", "/configs/services", "ops", 403},
		{"DELETE", "/configs/services", "admin", 403},
		{"DELETE", "/configs/custom", "ops", 200},
		{"GET", "/scripts", "", 401},
		{"GET", "/scripts/rpmbuild", "", 401},
		{"GET", "/scripts/rpmbuild", "invalid", 401},
		{"GET", "/scripts/rpmbuild?watch=true", "invalid", 403},
		{"GET", "/scripts/rpmbuild?watch=true", "", 403},
		{"GET", "/scripts/rpmbuild?watch=true", "runner", 403},
		{"GET", "/scripts/rpmbuild", "runner", 200},
		{"GET", "/scripts", "runner", 200},
		{"GET", "/scripts", "user", 200},
		{"GET", "/scripts/custom", "user", 200},
		{"GET", "/scripts", "ops", 200},
		{"GET", "/scripts/custom", "ops", 200},
		{"GET", "/scripts", "admin", 200},
		{"GET", "/scripts/custom", "admin", 200},
		{"GET", "/scripts/custom", "service", 403},
		{"GET", "/scripts", "service", 403},
		{"POST", "/scripts", "", 401},
		{"POST", "/scripts", "user", 403},
		{"POST", "/scripts", "runner", 403},
		{"POST", "/scripts", "service", 403},
		{"PUT", "/scripts/custom", "user", 403},
		{"PATCH", "/scripts/custom", "runner", 403},
		{"DELETE", "/scripts/custom", "runner", 403},
		{"DELETE", "/scripts/custom", "user", 403},
		{"DELETE", "/scripts/custom", "ops", 200},
	} {
		t.Run(tc.method+" "+tc.path+"/"+tc.caller, func(t *testing.T) {
			calls := 0
			api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := "{}"
				if r.URL.Path == "/apis/iam.ebs/v1/users/"+tc.caller {
					body = fmt.Sprintf(`{"metadata":{"name":%q},"spec":{"enabled":true,"scopes":["ebs:%s"]}}`, tc.caller, tc.caller)
				} else if r.URL.Path == "/apis/iam.ebs/v1/machineaccounts/service" {
					body = `{"metadata":{"name":"service"},"spec":{"type":"service"}}`
				} else {
					calls++
					if r.URL.Path == "/apis/ebs/v1/configs/build-target" {
						body = `{"metadata":{"name":"build-target"},"spec":{"content":"targets: {}"}}`
					}
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			var payload io.Reader
			if tc.method == http.MethodPatch {
				payload = strings.NewReader(`{"spec":{"content":"targets: {}\n"}}`)
			}
			req := httptest.NewRequest(tc.method, "/apis/ebs/v1"+tc.path, payload)
			if payload != nil {
				req.Header.Set("Content-Type", "application/merge-patch+json")
			}
			switch tc.caller {
			case "":
			case "invalid":
				req.Header.Set("Authorization", "Bearer invalid")
			default:
				typ, scope := identity.UserType, identity.Scope("ebs:"+tc.caller)
				if tc.caller == "runner" {
					typ, scope = identity.RunnerType, ""
				} else if tc.caller == "service" {
					typ, scope = identity.ServiceType, ""
				}
				token, err := api.tokens.Issue(tc.caller, typ, scope, time.Hour, api.now())
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+token)
			}
			res := httptest.NewRecorder()
			api.Router().ServeHTTP(res, req)
			wantCalls := 0
			if tc.status == http.StatusOK {
				wantCalls = 1
			}
			if res.Code != tc.status || calls != wantCalls {
				t.Fatalf("status=%d upstream calls=%d, want %d/%d", res.Code, calls, tc.status, wantCalls)
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
		"spec":     map[string]string{"content": content},
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
		token, err := api.tokens.Issue(runner, identity.RunnerType, "", time.Hour, api.now())
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
	userToken, err := api.tokens.Issue("alice", identity.UserType, identity.UserScope, time.Hour, api.now())
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
		body := `{"metadata":{"name":"services"},"spec":{"content":"artifactManagers:\n  art-01:\n    internalURL: http://artifact-manager.art-01:8081\n    secret: hidden\n"}}`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	token, err := api.tokens.Issue("runner-a", identity.RunnerType, "", time.Hour, api.now())
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
			"spec":     map[string]string{"content": content()},
		})
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: upstreamStatus, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	}))
	token, err := api.tokens.Issue("runner-a", identity.RunnerType, "", time.Hour, api.now())
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

func TestAdminUserGetDirectForwarding(t *testing.T) {
	calls := 0
	api := newTestAPI(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/apis/iam.ebs/v1/users/admin":
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"metadata":{"name":"admin"},"spec":{"enabled":true,"scopes":["ebs:admin"]}}`))}, nil
		case "/apis/iam.ebs/v1/users/alice":
			calls++
			scope := "ebs:admin"
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"metadata":{"name":"alice"},"spec":{"enabled":true,"scopes":["` + scope + `"]}}`))}, nil
		default:
			t.Fatalf("unexpected upstream path %s", request.URL.Path)
			return nil, nil
		}
	}))
	token, err := api.tokens.Issue("admin", identity.UserType, identity.AdminScope, time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/apis/iam.ebs/v1/users/alice", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	api.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK || calls != 1 || !strings.Contains(response.Body.String(), "ebs:admin") {
		t.Fatalf("status=%d calls=%d body=%q", response.Code, calls, response.Body.String())
	}
}

func TestUserListIncludesAdminAccounts(t *testing.T) {
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
	token, err := api.tokens.Issue("admin", identity.UserType, identity.AdminScope, time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/apis/iam.ebs/v1/users", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	api.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"name":"admin"`) || !strings.Contains(response.Body.String(), `"name":"alice"`) {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	for _, field := range []string{`"apiVersion":"iam.ebs/v1"`, `"kind":"UserList"`, `"continue":"next-page"`, `"resourceVersion":"42"`, `"displayName":"Alice"`} {
		if !strings.Contains(response.Body.String(), field) {
			t.Fatalf("missing %s in response: %s", field, response.Body.String())
		}
	}
}

func TestRunnerJobReadPreservesNotFound(t *testing.T) {
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
	token, err := api.tokens.Issue("runner-1", identity.RunnerType, "", time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	api.Router().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || calls != 1 {
		t.Fatalf("status=%d calls=%d body=%q", response.Code, calls, response.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error != "Job not found" {
		t.Fatalf("unexpected response: %s, error=%v", response.Body.String(), err)
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
	token, err := api.tokens.Issue("runner-1", identity.RunnerType, "", time.Hour, api.now())
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
		case "/apis/ebs/v1/projects/team/rpmrepos/repo",
			"/apis/ebs/v1/projects/team/builds/item",
			"/apis/ebs/v1/projects/team/builds/item/status",
			"/apis/ebs/v1/projects/team/jobs/item/status",
			"/apis/ebs/v1/projects/team/snapshots/item",
			"/apis/ebs/v1/projects/team/oldjobs/item",
			"/apis/ebs/v1/projects/team/buildinfos/item",
			"/apis/ebs/v1/projects/team/buildgraphs/item",
			"/apis/ebs/v1/projects/team/rpmrepos/repo/status",
			"/apis/ebs/v1/projects/team/snapshots/item/status",
			"/apis/ebs/v1/projects/team/buildinfos/item/status",
			"/apis/ebs/v1/projects/team/buildgraphs/item/status":
			body = `{"status":{"phase":"Ready"}}`
		case "/apis/ebs/v1/projects/team/jobs", "/apis/ebs/v1/projects/team/rpmrepos",
			"/apis/ebs/v1/projects/team/builds",
			"/apis/ebs/v1/projects/team/snapshots", "/apis/ebs/v1/projects/team/oldjobs",
			"/apis/ebs/v1/projects/team/buildinfos", "/apis/ebs/v1/projects/team/buildgraphs":
			if request.URL.Query().Get("limit") != "100" {
				t.Errorf("public collection was not bounded: %q", request.URL.RawQuery)
			}
			body = `{"items":[]}`
		default:
			t.Errorf("unexpected upstream request: %s", request.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	userToken, err := api.tokens.Issue("alice", identity.UserType, identity.UserScope, time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	runnerToken, err := api.tokens.Issue("runner-1", identity.RunnerType, "", time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, method, path, token string
		status                    int
	}{
		{"anonymous public list", http.MethodGet, "/apis/ebs/v1/projects/team/jobs", "", http.StatusOK},
		{"anonymous project read", http.MethodGet, "/apis/ebs/v1/projects/team", "", http.StatusOK},
		{"authenticated project read", http.MethodGet, "/apis/ebs/v1/projects/team", userToken, http.StatusOK},
		{"anonymous rpmrepo status", http.MethodGet, "/apis/ebs/v1/projects/team/rpmrepos/repo/status", "", http.StatusOK},
		{"authenticated rpmrepo status", http.MethodGet, "/apis/ebs/v1/projects/team/rpmrepos/repo/status", userToken, http.StatusOK},
		{"invalid rpmrepo bearer", http.MethodGet, "/apis/ebs/v1/projects/team/rpmrepos/repo/status", "bad", http.StatusUnauthorized},
		{"runner rpmrepo status", http.MethodGet, "/apis/ebs/v1/projects/team/rpmrepos/repo/status", runnerToken, http.StatusForbidden},
		{"rpmrepo status watch", http.MethodGet, "/apis/ebs/v1/projects/team/rpmrepos/repo/status?watch=true", userToken, http.StatusForbidden},
		{"authenticated rpmrepo list", http.MethodGet, "/apis/ebs/v1/projects/team/rpmrepos", userToken, http.StatusOK},
		{"authenticated rpmrepo detail", http.MethodGet, "/apis/ebs/v1/projects/team/rpmrepos/repo", userToken, http.StatusOK},
		{"invalid rpmrepo list bearer", http.MethodGet, "/apis/ebs/v1/projects/team/rpmrepos", "bad", http.StatusUnauthorized},
		{"invalid rpmrepo detail bearer", http.MethodGet, "/apis/ebs/v1/projects/team/rpmrepos/repo", "bad", http.StatusUnauthorized},
		{"runner rpmrepo list", http.MethodGet, "/apis/ebs/v1/projects/team/rpmrepos", runnerToken, http.StatusForbidden},
		{"runner rpmrepo detail", http.MethodGet, "/apis/ebs/v1/projects/team/rpmrepos/repo", runnerToken, http.StatusForbidden},
		{"rpmrepo list watch", http.MethodGet, "/apis/ebs/v1/projects/team/rpmrepos?watch=true", userToken, http.StatusForbidden},
		{"rpmrepo detail watch", http.MethodGet, "/apis/ebs/v1/projects/team/rpmrepos/repo?watch=true", userToken, http.StatusForbidden},
		{"anonymous snapshot status", http.MethodGet, "/apis/ebs/v1/projects/team/snapshots/item/status", "", http.StatusOK},
		{"authenticated snapshot status", http.MethodGet, "/apis/ebs/v1/projects/team/snapshots/item/status", userToken, http.StatusOK},
		{"invalid snapshot bearer", http.MethodGet, "/apis/ebs/v1/projects/team/snapshots/item/status", "bad", http.StatusUnauthorized},
		{"runner snapshot status", http.MethodGet, "/apis/ebs/v1/projects/team/snapshots/item/status", runnerToken, http.StatusForbidden},
		{"snapshot status watch", http.MethodGet, "/apis/ebs/v1/projects/team/snapshots/item/status?watch=true", userToken, http.StatusForbidden},
		{"anonymous buildinfo status", http.MethodGet, "/apis/ebs/v1/projects/team/buildinfos/item/status", "", http.StatusOK},
		{"authenticated buildinfo status", http.MethodGet, "/apis/ebs/v1/projects/team/buildinfos/item/status", userToken, http.StatusOK},
		{"invalid buildinfo bearer", http.MethodGet, "/apis/ebs/v1/projects/team/buildinfos/item/status", "bad", http.StatusUnauthorized},
		{"runner buildinfo status", http.MethodGet, "/apis/ebs/v1/projects/team/buildinfos/item/status", runnerToken, http.StatusForbidden},
		{"buildinfo status watch", http.MethodGet, "/apis/ebs/v1/projects/team/buildinfos/item/status?watch=true", userToken, http.StatusForbidden},
		{"anonymous buildgraph status", http.MethodGet, "/apis/ebs/v1/projects/team/buildgraphs/item/status", "", http.StatusOK},
		{"authenticated buildgraph status", http.MethodGet, "/apis/ebs/v1/projects/team/buildgraphs/item/status", userToken, http.StatusOK},
		{"invalid buildgraph bearer", http.MethodGet, "/apis/ebs/v1/projects/team/buildgraphs/item/status", "bad", http.StatusUnauthorized},
		{"runner buildgraph status", http.MethodGet, "/apis/ebs/v1/projects/team/buildgraphs/item/status", runnerToken, http.StatusForbidden},
		{"buildgraph status watch", http.MethodGet, "/apis/ebs/v1/projects/team/buildgraphs/item/status?watch=true", userToken, http.StatusForbidden},
		{"anonymous project write", http.MethodPatch, "/apis/ebs/v1/projects/team", "", http.StatusUnauthorized},
		{"authenticated build list", http.MethodGet, "/apis/ebs/v1/projects/team/builds", userToken, http.StatusOK},
		{"authenticated build detail", http.MethodGet, "/apis/ebs/v1/projects/team/builds/item", userToken, http.StatusOK},
		{"invalid build list bearer", http.MethodGet, "/apis/ebs/v1/projects/team/builds", "bad", http.StatusUnauthorized},
		{"invalid build detail bearer", http.MethodGet, "/apis/ebs/v1/projects/team/builds/item", "bad", http.StatusUnauthorized},
		{"runner build list", http.MethodGet, "/apis/ebs/v1/projects/team/builds", runnerToken, http.StatusForbidden},
		{"runner build detail", http.MethodGet, "/apis/ebs/v1/projects/team/builds/item", runnerToken, http.StatusForbidden},
		{"build list watch", http.MethodGet, "/apis/ebs/v1/projects/team/builds?watch=true", userToken, http.StatusForbidden},
		{"build detail watch", http.MethodGet, "/apis/ebs/v1/projects/team/builds/item?watch=true", userToken, http.StatusForbidden},
		{"authenticated build status", http.MethodGet, "/apis/ebs/v1/projects/team/builds/item/status", userToken, http.StatusOK},
		{"invalid build status bearer", http.MethodGet, "/apis/ebs/v1/projects/team/builds/item/status", "bad", http.StatusUnauthorized},
		{"runner build status", http.MethodGet, "/apis/ebs/v1/projects/team/builds/item/status", runnerToken, http.StatusForbidden},
		{"build status watch", http.MethodGet, "/apis/ebs/v1/projects/team/builds/item/status?watch=true", userToken, http.StatusForbidden},
		{"anonymous job status", http.MethodGet, "/apis/ebs/v1/projects/team/jobs/item/status", "", http.StatusOK},
		{"authenticated job status", http.MethodGet, "/apis/ebs/v1/projects/team/jobs/item/status", userToken, http.StatusOK},
		{"invalid job status bearer", http.MethodGet, "/apis/ebs/v1/projects/team/jobs/item/status", "bad", http.StatusUnauthorized},
		{"runner job status read", http.MethodGet, "/apis/ebs/v1/projects/team/jobs/item/status", runnerToken, http.StatusForbidden},
		{"job status read watch", http.MethodGet, "/apis/ebs/v1/projects/team/jobs/item/status?watch=true", userToken, http.StatusForbidden},
		{"snapshots list authenticated", http.MethodGet, "/apis/ebs/v1/projects/team/snapshots", userToken, http.StatusOK},
		{"snapshots list invalid bearer", http.MethodGet, "/apis/ebs/v1/projects/team/snapshots", "bad", http.StatusUnauthorized},
		{"snapshots list runner", http.MethodGet, "/apis/ebs/v1/projects/team/snapshots", runnerToken, http.StatusForbidden},
		{"snapshots list watch", http.MethodGet, "/apis/ebs/v1/projects/team/snapshots?watch=true", userToken, http.StatusForbidden},
		{"snapshots detail authenticated", http.MethodGet, "/apis/ebs/v1/projects/team/snapshots/item", userToken, http.StatusOK},
		{"snapshots detail invalid bearer", http.MethodGet, "/apis/ebs/v1/projects/team/snapshots/item", "bad", http.StatusUnauthorized},
		{"snapshots detail runner", http.MethodGet, "/apis/ebs/v1/projects/team/snapshots/item", runnerToken, http.StatusForbidden},
		{"snapshots detail watch", http.MethodGet, "/apis/ebs/v1/projects/team/snapshots/item?watch=true", userToken, http.StatusForbidden},
		{"oldjobs list authenticated", http.MethodGet, "/apis/ebs/v1/projects/team/oldjobs", userToken, http.StatusOK},
		{"oldjobs list invalid bearer", http.MethodGet, "/apis/ebs/v1/projects/team/oldjobs", "bad", http.StatusUnauthorized},
		{"oldjobs list runner", http.MethodGet, "/apis/ebs/v1/projects/team/oldjobs", runnerToken, http.StatusForbidden},
		{"oldjobs list watch", http.MethodGet, "/apis/ebs/v1/projects/team/oldjobs?watch=true", userToken, http.StatusForbidden},
		{"oldjobs detail authenticated", http.MethodGet, "/apis/ebs/v1/projects/team/oldjobs/item", userToken, http.StatusOK},
		{"oldjobs detail invalid bearer", http.MethodGet, "/apis/ebs/v1/projects/team/oldjobs/item", "bad", http.StatusUnauthorized},
		{"oldjobs detail runner", http.MethodGet, "/apis/ebs/v1/projects/team/oldjobs/item", runnerToken, http.StatusForbidden},
		{"oldjobs detail watch", http.MethodGet, "/apis/ebs/v1/projects/team/oldjobs/item?watch=true", userToken, http.StatusForbidden},
		{"buildinfos list authenticated", http.MethodGet, "/apis/ebs/v1/projects/team/buildinfos", userToken, http.StatusOK},
		{"buildinfos list invalid bearer", http.MethodGet, "/apis/ebs/v1/projects/team/buildinfos", "bad", http.StatusUnauthorized},
		{"buildinfos list runner", http.MethodGet, "/apis/ebs/v1/projects/team/buildinfos", runnerToken, http.StatusForbidden},
		{"buildinfos list watch", http.MethodGet, "/apis/ebs/v1/projects/team/buildinfos?watch=true", userToken, http.StatusForbidden},
		{"buildinfos detail authenticated", http.MethodGet, "/apis/ebs/v1/projects/team/buildinfos/item", userToken, http.StatusOK},
		{"buildinfos detail invalid bearer", http.MethodGet, "/apis/ebs/v1/projects/team/buildinfos/item", "bad", http.StatusUnauthorized},
		{"buildinfos detail runner", http.MethodGet, "/apis/ebs/v1/projects/team/buildinfos/item", runnerToken, http.StatusForbidden},
		{"buildinfos detail watch", http.MethodGet, "/apis/ebs/v1/projects/team/buildinfos/item?watch=true", userToken, http.StatusForbidden},
		{"buildgraphs list authenticated", http.MethodGet, "/apis/ebs/v1/projects/team/buildgraphs", userToken, http.StatusOK},
		{"buildgraphs list invalid bearer", http.MethodGet, "/apis/ebs/v1/projects/team/buildgraphs", "bad", http.StatusUnauthorized},
		{"buildgraphs list runner", http.MethodGet, "/apis/ebs/v1/projects/team/buildgraphs", runnerToken, http.StatusForbidden},
		{"buildgraphs list watch", http.MethodGet, "/apis/ebs/v1/projects/team/buildgraphs?watch=true", userToken, http.StatusForbidden},
		{"buildgraphs detail authenticated", http.MethodGet, "/apis/ebs/v1/projects/team/buildgraphs/item", userToken, http.StatusOK},
		{"buildgraphs detail invalid bearer", http.MethodGet, "/apis/ebs/v1/projects/team/buildgraphs/item", "bad", http.StatusUnauthorized},
		{"buildgraphs detail runner", http.MethodGet, "/apis/ebs/v1/projects/team/buildgraphs/item", runnerToken, http.StatusForbidden},
		{"buildgraphs detail watch", http.MethodGet, "/apis/ebs/v1/projects/team/buildgraphs/item?watch=true", userToken, http.StatusForbidden},
		{"anonymous project creation", http.MethodPost, "/apis/ebs/v1/projects", "", http.StatusUnauthorized},
		{"runner project creation", http.MethodPost, "/apis/ebs/v1/projects", runnerToken, http.StatusForbidden},
		{"anonymous build creation", http.MethodPost, "/apis/ebs/v1/projects/team/builds", "", http.StatusUnauthorized},
		{"invalid job status write", http.MethodPatch, "/apis/ebs/v1/projects/team/jobs/j/status", "bad", http.StatusUnauthorized},
		{"anonymous runner list", http.MethodGet, "/apis/ebs/v1/runners", "", http.StatusUnauthorized},
		{"anonymous runner jobs watch", http.MethodGet, "/apis/ebs/v1/runners/runner-1/jobs?watch=true", "", http.StatusUnauthorized},
		{"invalid project bearer cannot become anonymous", http.MethodGet, "/apis/ebs/v1/projects/team", "bad", http.StatusUnauthorized},
		{"runner cannot read project", http.MethodGet, "/apis/ebs/v1/projects/team", runnerToken, http.StatusForbidden},
		{"anonymous project watch", http.MethodGet, "/apis/ebs/v1/projects?watch=true", "", http.StatusForbidden},
		{"authenticated project watch", http.MethodGet, "/apis/ebs/v1/projects?watch=true", userToken, http.StatusForbidden},
		{"anonymous watch", http.MethodGet, "/apis/ebs/v1/projects/team/jobs?watch=true", "", http.StatusUnauthorized},
		{"invalid bearer cannot become anonymous", http.MethodGet, "/apis/ebs/v1/projects/team/jobs", "bad", http.StatusUnauthorized},
		{"runner not public business reader", http.MethodGet, "/apis/ebs/v1/projects/team/jobs", runnerToken, http.StatusForbidden},
		{"unknown anonymous path", http.MethodGet, "/apis/ebs/v1/secrets", "", http.StatusUnauthorized},
		{"unknown authenticated path", http.MethodGet, "/apis/ebs/v1/secrets", userToken, http.StatusNotFound},
		{"abort wrong method", http.MethodGet, "/apis/ebs/v1/projects/team/jobs/job-1/abort", userToken, http.StatusNotFound},
		{"collection update", http.MethodPut, "/apis/ebs/v1/projects/team/jobs", userToken, http.StatusNotFound},
		{"job update", http.MethodPut, "/apis/ebs/v1/projects/team/jobs/j", userToken, http.StatusNotFound},
		{"job patch", http.MethodPatch, "/apis/ebs/v1/projects/team/jobs/j", userToken, http.StatusNotFound},
		{"runner job update", http.MethodPut, "/apis/ebs/v1/projects/team/jobs/j", runnerToken, http.StatusNotFound},
		{"runner job status put", http.MethodPut, "/apis/ebs/v1/projects/team/jobs/j/status", runnerToken, http.StatusNotFound},
		{"job deletion", http.MethodDelete, "/apis/ebs/v1/projects/team/jobs/j", userToken, http.StatusNotFound},
		{"runner job patch", http.MethodPatch, "/apis/ebs/v1/projects/team/jobs/j", runnerToken, http.StatusNotFound},
		{"build update", http.MethodPut, "/apis/ebs/v1/projects/team/builds/b", userToken, http.StatusNotFound},
		{"build patch", http.MethodPatch, "/apis/ebs/v1/projects/team/builds/b", userToken, http.StatusNotFound},
		{"build status update", http.MethodPut, "/apis/ebs/v1/projects/team/builds/b/status", userToken, http.StatusNotFound},
		{"build status patch", http.MethodPatch, "/apis/ebs/v1/projects/team/builds/b/status", userToken, http.StatusNotFound},
		{"project status read", http.MethodGet, "/apis/ebs/v1/projects/team/status", userToken, http.StatusNotFound},
		{"project status update", http.MethodPut, "/apis/ebs/v1/projects/team/status", userToken, http.StatusNotFound},
		{"project status patch", http.MethodPatch, "/apis/ebs/v1/projects/team/status", userToken, http.StatusNotFound},
		{"anonymous unsupported method", http.MethodPut, "/apis/ebs/v1/projects/team/jobs", "", http.StatusUnauthorized},
		{"IAM create is not registered", http.MethodPost, "/apis/iam.ebs/v1/users", userToken, http.StatusNotFound},

		{"job creation", http.MethodPost, "/apis/ebs/v1/projects/team/jobs", userToken, http.StatusNotFound},
		{"watch on job creation", http.MethodPost, "/apis/ebs/v1/projects/team/jobs?watch=true", userToken, http.StatusNotFound},
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

func TestAdminCannotModifyAnotherOwnersProject(t *testing.T) {
	api := newTestAPI(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodGet && request.URL.Path == "/apis/ebs/v1/projects/team" {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"metadata":{"name":"team","labels":{"ebs.io/owner-user":"alice"}}}`))}, nil
		}
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
	} {
		t.Run(test.name, func(t *testing.T) {
			token, err := api.tokens.Issue(test.subject, identity.UserType, test.scope, time.Hour, api.now())
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

func TestAdminOwnerProjectPurposePatch(t *testing.T) {
	for _, test := range []struct {
		name, body string
		want       int
	}{
		{"purpose", `{"metadata":{"labels":{"ebs.io/build-purpose":"gating"}}}`, http.StatusOK},
		{"spec", `{"spec":{"description":"changed"}}`, http.StatusForbidden},
		{"owner", `{"metadata":{"labels":{"ebs.io/owner-user":"alice"}}}`, http.StatusForbidden},
		{"other label", `{"metadata":{"labels":{"note":"changed"}}}`, http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			writes := 0
			api := newTestAPI(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				body := `{"apiVersion":"ebs/v1","kind":"Project","metadata":{"name":"team","resourceVersion":"1","labels":{"ebs.io/owner-user":"admin"}},"spec":{"description":"original"}}`
				switch {
				case request.Method == http.MethodGet && request.URL.Path == "/apis/iam.ebs/v1/users/admin":
					body = `{"metadata":{"name":"admin"},"spec":{"enabled":true,"scopes":["ebs:admin"]}}`
				case request.Method == http.MethodGet && request.URL.Path == "/apis/ebs/v1/projects/team":
				case request.Method == http.MethodPut && request.URL.Path == "/apis/ebs/v1/projects/team":
					writes++
					data, err := io.ReadAll(request.Body)
					if err != nil || !strings.Contains(string(data), `"ebs.io/build-purpose":"gating"`) {
						t.Fatalf("forwarded body=%s error=%v", data, err)
					}
					body = string(data)
				default:
					t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			token, err := api.tokens.Issue("admin", identity.UserType, identity.AdminScope, time.Hour, api.now())
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPatch, "/apis/ebs/v1/projects/team", strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Content-Type", "application/merge-patch+json")
			response := httptest.NewRecorder()
			api.Router().ServeHTTP(response, request)
			if response.Code != test.want || (writes == 1) != (test.want == http.StatusOK) {
				t.Fatalf("status=%d writes=%d body=%s", response.Code, writes, response.Body.String())
			}
		})
	}
}

func TestBuildCreateChain(t *testing.T) {
	const path = "/apis/ebs/v1/projects/team/builds"
	const body = "{\n  \"metadata\": {\"name\": \"build-1\"}, \"spec\": {\"arch\": \"aarch64\"}\n}"
	for _, tc := range []struct {
		name, subject string
		scope         identity.Scope
		want          int
	}{
		{"owner", "alice", identity.UserScope, 201},
		{"member", "bob", identity.UserScope, 201},
		{"ops member", "bob", identity.OpsScope, 201},
		{"admin owner", "alice", identity.AdminScope, 201},
		{"admin member", "bob", identity.AdminScope, 201},
		{"admin outsider", "admin", identity.AdminScope, 403},
		{"outsider", "outsider", identity.UserScope, 403},
		{"ops outsider", "ops", identity.OpsScope, 403},
		{"upstream invalid object", "alice", identity.UserScope, 422},
		{"upstream body limit", "alice", identity.UserScope, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forwarded := false
			api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				response, status := "{}", http.StatusOK
				switch r.URL.Path {
				case "/apis/iam.ebs/v1/users/" + tc.subject:
					response = `{"metadata":{"name":"` + tc.subject + `"},"spec":{"enabled":true,"scopes":["` + string(tc.scope) + `"]}}`
				case "/apis/ebs/v1/projects/team":
					response = `{"metadata":{"name":"team","labels":{"ebs.io/owner-user":"alice","ebs.io/member-user.bob":"true"}}}`
				case path:
					forwarded = true
					raw, err := io.ReadAll(r.Body)
					if err != nil || string(raw) != body || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
						t.Fatalf("request changed: method=%s body=%s err=%v", r.Method, raw, err)
					}
					status = tc.want
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
			}))
			api.bodyLimit = 8
			token, err := api.tokens.Issue(tc.subject, identity.UserType, tc.scope, time.Hour, api.now())
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			api.Router().ServeHTTP(response, request)
			if response.Code != tc.want || forwarded != (tc.want != http.StatusForbidden) {
				t.Fatalf("status=%d forwarded=%v body=%s", response.Code, forwarded, response.Body)
			}
		})
	}
}

func TestJobListRoute(t *testing.T) {
	for _, tc := range []struct {
		name, subject, query, limit string
		want                        int
	}{
		{"anonymous list", "", "", "100", 200},
		{"public user list", "outsider", "limit=200", "100", 200},
		{"watch owner", "alice", "watch=true&limit=500", "500", 200},
		{"watch member", "bob", "watch=true", "", 200},
		{"watch admin outsider", "admin", "watch=true", "", 403},
		{"watch admin member", "admin-member", "watch=true", "", 200},
		{"watch outsider", "outsider", "watch=true", "", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := identity.UserScope
			if strings.HasPrefix(tc.subject, "admin") {
				scope = identity.AdminScope
			}
			forwarded := false
			api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := `{"items":[]}`
				switch r.URL.Path {
				case "/apis/iam.ebs/v1/users/" + tc.subject:
					body = `{"metadata":{"name":"` + tc.subject + `"},"spec":{"enabled":true,"scopes":["` + string(scope) + `"]}}`
				case "/apis/ebs/v1/projects/team":
					if tc.query == "" || !strings.Contains(tc.query, "watch=true") {
						t.Fatal("unexpected Project authorization lookup")
					}
					body = `{"metadata":{"name":"team","labels":{"ebs.io/owner-user":"alice","ebs.io/member-user.bob":"true","ebs.io/member-user.admin-member":"true"}}}`
				case "/apis/ebs/v1/projects/team/jobs":
					forwarded = true
					if r.URL.Query().Get("limit") != tc.limit {
						t.Fatalf("unexpected query: %s", r.URL.RawQuery)
					}
				default:
					t.Fatalf("unexpected request: %s", r.URL)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			router := api.Router()
			request := httptest.NewRequest(http.MethodGet, "/apis/ebs/v1/projects/team/jobs?"+tc.query, nil)
			if tc.subject != "" {
				token, err := api.tokens.Issue(tc.subject, identity.UserType, scope, time.Hour, api.now())
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Authorization", "Bearer "+token)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != tc.want || forwarded != (tc.want == http.StatusOK) {
				t.Fatalf("status=%d forwarded=%v body=%s", response.Code, forwarded, response.Body)
			}
		})
	}
}

func TestBuildActionChains(t *testing.T) {
	body := `{"preconditions":{"uid":"build-uid","resourceVersion":"7"}}`
	for _, tc := range []struct {
		name, method, subject string
		scope                 identity.Scope
		want                  int
	}{
		{"abort owner", http.MethodPost, "alice", identity.UserScope, 200},
		{"abort member", http.MethodPost, "bob", identity.UserScope, 200},
		{"abort ops owner", http.MethodPost, "alice", identity.OpsScope, 200},
		{"abort admin owner", http.MethodPost, "alice", identity.AdminScope, 200},
		{"abort admin member", http.MethodPost, "bob", identity.AdminScope, 200},
		{"abort admin outsider", http.MethodPost, "admin", identity.AdminScope, 403},
		{"abort outsider", http.MethodPost, "outsider", identity.UserScope, 403},
		{"abort ops outsider", http.MethodPost, "ops", identity.OpsScope, 403},
		{"delete owner", http.MethodDelete, "alice", identity.UserScope, 200},
		{"delete ops owner", http.MethodDelete, "alice", identity.OpsScope, 200},
		{"delete admin owner", http.MethodDelete, "alice", identity.AdminScope, 200},
		{"delete member", http.MethodDelete, "bob", identity.UserScope, 403},
		{"delete admin outsider", http.MethodDelete, "admin", identity.AdminScope, 403},
		{"delete version conflict", http.MethodDelete, "alice", identity.UserScope, 409},
		{"delete phase rejected", http.MethodDelete, "alice", identity.UserScope, 422},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := "/apis/ebs/v1/projects/team/builds/build-1"
			if tc.method == http.MethodPost {
				path += "/abort"
			}
			forwarded := false
			api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				response := `{}`
				status := http.StatusOK
				switch r.URL.Path {
				case "/apis/iam.ebs/v1/users/" + tc.subject:
					response = `{"metadata":{"name":"` + tc.subject + `"},"spec":{"enabled":true,"scopes":["` + string(tc.scope) + `"]}}`
				case "/apis/ebs/v1/projects/team":
					response = `{"metadata":{"name":"team","labels":{"ebs.io/owner-user":"alice","ebs.io/member-user.bob":"true"}}}`
				case path:
					forwarded = true
					raw, err := io.ReadAll(r.Body)
					if err != nil || string(raw) != body || r.Method != tc.method || r.Header.Get("Content-Type") != "application/json" {
						t.Fatalf("request changed: method=%s body=%s err=%v", r.Method, raw, err)
					}
					status = tc.want
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
			}))
			api.bodyLimit = 8
			router := api.Router()
			token, err := api.tokens.Issue(tc.subject, identity.UserType, tc.scope, time.Hour, api.now())
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(tc.method, path, strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != tc.want || forwarded != (tc.want != http.StatusForbidden) {
				t.Fatalf("status=%d forwarded=%v body=%s", response.Code, forwarded, response.Body)
			}
		})
	}
}

func TestPublicReadForwarding(t *testing.T) {
	for _, tc := range []struct {
		name, path, query, wantQuery string
	}{
		{"list default", "/apis/ebs/v1/projects", "", "limit=100"},
		{"list explicit", "/apis/ebs/v1/projects", "limit=20&labelSelector=team", "limit=20&labelSelector=team"},
		{"list upper bound", "/apis/ebs/v1/projects", "limit=101", "limit=100"},
		{"list invalid", "/apis/ebs/v1/projects", "limit=invalid", "limit=100"},
		{"list zero", "/apis/ebs/v1/projects", "limit=0", "limit=100"},
		{"list repeated", "/apis/ebs/v1/projects", "limit=10&limit=20", "limit=100"},
		{"object", "/apis/ebs/v1/projects/team", "", ""},
		{"project status projection", "/apis/ebs/v1/projects/team", "includeFields=status", "includeFields=status"},
		{"build list default", "/apis/ebs/v1/projects/team/builds", "", "limit=100"},
		{"build list explicit", "/apis/ebs/v1/projects/team/builds", "limit=20", "limit=20"},
		{"build list upper bound", "/apis/ebs/v1/projects/team/builds", "limit=101", "limit=100"},
		{"build detail", "/apis/ebs/v1/projects/team/builds/item", "includeFields=status", "includeFields=status"},
		{"build status", "/apis/ebs/v1/projects/team/builds/item/status", "includeFields=status", "includeFields=status"},
		{"job status", "/apis/ebs/v1/projects/team/jobs/item/status", "includeFields=status", "includeFields=status"},
		{"job detail", "/apis/ebs/v1/projects/team/jobs/item", "includeFields=status", "includeFields=status"},
		{"snapshots list default", "/apis/ebs/v1/projects/team/snapshots", "", "limit=100"},
		{"snapshots list explicit", "/apis/ebs/v1/projects/team/snapshots", "limit=20", "limit=20"},
		{"snapshots list upper bound", "/apis/ebs/v1/projects/team/snapshots", "limit=101", "limit=100"},
		{"snapshots detail", "/apis/ebs/v1/projects/team/snapshots/item", "includeFields=status", "includeFields=status"},
		{"oldjobs list default", "/apis/ebs/v1/projects/team/oldjobs", "", "limit=100"},
		{"oldjobs list explicit", "/apis/ebs/v1/projects/team/oldjobs", "limit=20", "limit=20"},
		{"oldjobs list upper bound", "/apis/ebs/v1/projects/team/oldjobs", "limit=101", "limit=100"},
		{"oldjobs detail", "/apis/ebs/v1/projects/team/oldjobs/item", "includeFields=status", "includeFields=status"},
		{"rpmrepo status", "/apis/ebs/v1/projects/team/rpmrepos/repo/status", "", ""},
		{"rpmrepo list default", "/apis/ebs/v1/projects/team/rpmrepos", "", "limit=100"},
		{"rpmrepo list explicit", "/apis/ebs/v1/projects/team/rpmrepos", "limit=20", "limit=20"},
		{"rpmrepo list upper bound", "/apis/ebs/v1/projects/team/rpmrepos", "limit=101", "limit=100"},
		{"rpmrepo detail", "/apis/ebs/v1/projects/team/rpmrepos/repo", "includeFields=status", "includeFields=status"},
		{"rpmrepo status query", "/apis/ebs/v1/projects/team/rpmrepos/repo/status", "includeFields=status", "includeFields=status"},
		{"snapshot status", "/apis/ebs/v1/projects/team/snapshots/item/status", "includeFields=status", "includeFields=status"},
		{"buildinfo status", "/apis/ebs/v1/projects/team/buildinfos/item/status", "includeFields=status", "includeFields=status"},
		{"buildgraph status", "/apis/ebs/v1/projects/team/buildgraphs/item/status", "includeFields=status", "includeFields=status"},
		{"buildinfo list default", "/apis/ebs/v1/projects/team/buildinfos", "", "limit=100"},
		{"buildinfo list explicit", "/apis/ebs/v1/projects/team/buildinfos", "limit=20", "limit=20"},
		{"buildinfo list upper bound", "/apis/ebs/v1/projects/team/buildinfos", "limit=101", "limit=100"},
		{"buildinfo detail", "/apis/ebs/v1/projects/team/buildinfos/item", "includeFields=status", "includeFields=status"},
		{"buildgraph list default", "/apis/ebs/v1/projects/team/buildgraphs", "", "limit=100"},
		{"buildgraph list explicit", "/apis/ebs/v1/projects/team/buildgraphs", "limit=20", "limit=20"},
		{"buildgraph list upper bound", "/apis/ebs/v1/projects/team/buildgraphs", "limit=101", "limit=100"},
		{"buildgraph detail", "/apis/ebs/v1/projects/team/buildgraphs/item", "includeFields=status", "includeFields=status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			api := newTestAPI(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.Method != http.MethodGet || request.URL.Path != tc.path || request.URL.RawQuery != tc.wantQuery {
					t.Errorf("unexpected upstream request: %s %s", request.Method, request.URL)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header: http.Header{
						"Content-Type": {"application/json"},
						"Retry-After":  {"5"},
						"Warning":      {`299 ebs-apiserver "example warning"`},
					},
					Body:    io.NopCloser(strings.NewReader(`{"items":[]}`)),
					Request: request,
				}, nil
			}))
			path := tc.path
			if tc.query != "" {
				path += "?" + tc.query
			}
			response := httptest.NewRecorder()
			api.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			if response.Code != http.StatusOK || calls != 1 {
				t.Fatalf("status=%d upstream calls=%d body=%s", response.Code, calls, response.Body)
			}
			if response.Header().Get("Retry-After") != "5" ||
				response.Header().Get("Warning") != `299 ebs-apiserver "example warning"` ||
				response.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("upstream response headers changed: %v", response.Header())
			}
		})
	}
}

func TestAuthCheckDoesNotGrantBusinessAccess(t *testing.T) {
	api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Path != "/apis/iam.ebs/v1/machineaccounts/service" {
			t.Fatalf("unexpected resource access: %s %s", r.Method, r.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"metadata":{"name":"service"},"spec":{"type":"service"}}`))}, nil
	}))
	token, err := api.tokens.Issue("service", identity.ServiceType, "", time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	router := api.Router()
	for _, tc := range []struct {
		method, path string
		token        string
		want         int
	}{
		{http.MethodPost, "/auth/check", "", http.StatusUnauthorized},
		{http.MethodPost, "/auth/check", "invalid", http.StatusUnauthorized},
		{http.MethodPost, "/auth/check", token, http.StatusOK},
		{http.MethodPost, "/apis/ebs/v1/projects/team/builds", token, http.StatusForbidden},
		{http.MethodPost, "/auth/check?watch=true", token, http.StatusForbidden},
		{http.MethodGet, "/auth/check", token, http.StatusNotFound},
	} {
		request := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.token != "" {
			request.Header.Set("Authorization", "Bearer "+tc.token)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != tc.want {
			t.Fatalf("%s %s: status=%d, want %d: %s", tc.method, tc.path, response.Code, tc.want, response.Body)
		}
	}
}

func TestAuthenticationFailureDoesNotConsumeIdentityQuota(t *testing.T) {
	lookups, forwards := 0, 0
	api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"items":[]}`
		if r.URL.Path == "/apis/iam.ebs/v1/users/alice" {
			lookups++
			body = `{"metadata":{"name":"alice"},"spec":{"enabled":true,"scopes":["ebs:user"]}}`
		} else {
			forwards++
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	api.limits = limit.New(1, 1, api.now)
	token, err := api.tokens.Issue("alice", identity.UserType, identity.UserScope, time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	router := api.Router()
	for _, tc := range []struct {
		token                     string
		status, lookups, forwards int
	}{
		{"invalid", http.StatusUnauthorized, 0, 0},
		{"invalid", http.StatusUnauthorized, 0, 0},
		{token, http.StatusOK, 1, 1},
		{token, http.StatusTooManyRequests, 2, 1},
	} {
		request := httptest.NewRequest(http.MethodGet, "/apis/ebs/v1/projects", nil)
		request.RemoteAddr = "192.0.2.1:1234"
		request.Header.Set("Authorization", "Bearer "+tc.token)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != tc.status || lookups != tc.lookups || forwards != tc.forwards {
			t.Fatalf("status=%d lookups=%d forwards=%d", response.Code, lookups, forwards)
		}
	}
}

func TestProjectWriteChain(t *testing.T) {
	const old = `{"metadata":{"name":"team","resourceVersion":"1","labels":{"ebs.io/owner-user":"alice"}},"spec":{},"status":{}}`
	for _, tc := range []struct {
		name, method, suffix, body string
		want                       int
	}{
		{"create", http.MethodPost, "", `{"metadata":{"name":"team"}}`, http.StatusOK},
		{"update", http.MethodPut, "/team", old, http.StatusOK},
		{"patch", http.MethodPatch, "/team", `{"spec":{"description":"changed"}}`, http.StatusOK},
		{"invalid create body", http.MethodPost, "", "{", http.StatusBadRequest},
		{"invalid update body", http.MethodPatch, "/team", "{", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			api := newTestAPI(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				body := old
				if request.Method == http.MethodGet && request.URL.Path == "/apis/iam.ebs/v1/users/alice" {
					body = `{"metadata":{"name":"alice"},"spec":{"enabled":true,"scopes":["ebs:user"]}}`
				} else if request.Method != http.MethodGet {
					writes++
					wantMethod := http.MethodPut
					if tc.method == http.MethodPost {
						wantMethod = http.MethodPost
					}
					data, err := io.ReadAll(request.Body)
					if err != nil || request.Method != wantMethod || !strings.Contains(string(data), `"ebs.io/owner-user":"alice"`) {
						t.Fatalf("unexpected forwarded request: %s body=%s err=%v", request.Method, data, err)
					}
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			router := api.Router()
			token, err := api.tokens.Issue("alice", identity.UserType, identity.UserScope, time.Hour, api.now())
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(tc.method, "/apis/ebs/v1/projects"+tc.suffix, strings.NewReader(tc.body))
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Content-Type", "application/json")
			if tc.method == http.MethodPatch {
				request.Header.Set("Content-Type", "application/merge-patch+json")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != tc.want || (writes == 1) != (tc.want == http.StatusOK) || writes > 1 {
				t.Fatalf("status=%d writes=%d body=%s", response.Code, writes, response.Body)
			}
		})
	}
}

func TestRouteDefinitionAccess(t *testing.T) {
	api := newTestAPI(t, nil)
	rules := registeredRoutePolicies(api)
	for _, tc := range []struct {
		method, path, allowed string
	}{
		{http.MethodGet, "/apis/ebs/v1/configs", "ops,admin"},
		{http.MethodPost, "/apis/ebs/v1/configs", "ops,admin"},
		{http.MethodGet, "/apis/ebs/v1/scripts", "user,ops,admin,runner"},
		{http.MethodPut, "/apis/ebs/v1/scripts/:name", "ops,admin"},
		{http.MethodGet, "/apis/iam.ebs/v1/users", "admin"},
		{http.MethodPost, "/auth/machineaccounts", "admin"},
		{http.MethodPut, "/auth/users/:name/password", "user,ops,admin"},
		{http.MethodGet, "/apis/ebs/v1/build-targets", "user,ops,admin"},
		{http.MethodGet, "/apis/ebs/v1/service-endpoints", "runner"},
		{http.MethodGet, "/apis/ebs/v1/runners", "ops,admin"},
		{http.MethodPost, "/apis/ebs/v1/runners", "runner"},
		{http.MethodGet, "/apis/ebs/v1/runners/:runner", "ops,admin,runner"},
		{http.MethodGet, "/apis/ebs/v1/runners/:runner/jobs", "runner"},
		{http.MethodGet, "/apis/ebs/v1/runners/:runner/status", "ops,admin"},
		{http.MethodPatch, "/apis/ebs/v1/runners/:runner/status", "ops,admin,runner"},
		{http.MethodDelete, "/apis/ebs/v1/runners/:runner", "ops,admin"},
		{http.MethodGet, "/apis/ebs/v1/projects/:project", "anonymous,user,ops,admin"},
		{http.MethodPost, "/apis/ebs/v1/projects", "user,ops,admin"},
		{http.MethodPut, "/apis/ebs/v1/projects/:project", "user,ops,admin,service"},
		{http.MethodPatch, "/apis/ebs/v1/projects/:project/status", ""},
		{http.MethodPost, "/apis/ebs/v1/projects/:project/builds", "user,ops,admin,service"},
		{http.MethodPost, "/apis/ebs/v1/projects/:project/builds/:name/abort", "user,ops,admin,service"},
		{http.MethodDelete, "/apis/ebs/v1/projects/:project/builds/:name", "user,ops,admin"},
		{http.MethodGet, "/apis/ebs/v1/projects/:project/jobs", "anonymous,user,ops,admin"},
		{http.MethodGet, "/apis/ebs/v1/projects/:project/jobs/:name", "anonymous,user,ops,admin,runner"},
		{http.MethodGet, "/apis/ebs/v1/projects/:project/jobs/:name/status", "anonymous,user,ops,admin"},
		{http.MethodPatch, "/apis/ebs/v1/projects/:project/jobs/:name/status", "runner"},
		{http.MethodPut, "/apis/ebs/v1/projects/:project/jobs/:name/status", ""},
		{http.MethodPut, "/apis/ebs/v1/projects/:project/jobs/:name", ""},
		{http.MethodPatch, "/apis/ebs/v1/projects/:project/jobs/:name", ""},
		{http.MethodGet, "/apis/ebs/v1/scripts/:name", "user,ops,admin,runner"},
		{http.MethodGet, "/apis/ebs/v1/projects/:project/status", ""},
		{http.MethodPut, "/apis/ebs/v1/projects/:project/status", ""},
		{http.MethodPost, "/apis/ebs/v1/projects/:project/jobs", ""},
		{http.MethodDelete, "/apis/ebs/v1/projects/:project/jobs/:name", ""},
		{http.MethodGet, "/apis/ebs/v1/new-resource", ""},
		{http.MethodPost, "/apis/ebs/v1/build-targets", ""},
	} {

		var want policy.RouteIdentities
		for _, name := range strings.Split(tc.allowed, ",") {
			want |= map[string]policy.RouteIdentities{
				"anonymous": policy.AllowAnonymous,
				"user":      policy.AllowUser,
				"ops":       policy.AllowOps,
				"admin":     policy.AllowAdmin,
				"runner":    policy.AllowRunner,
				"service":   policy.AllowService,
			}[name]
		}
		key := middleware.RouteKey{Method: tc.method, Path: tc.path}
		if got := rules[key].Access.Authorization.Identities; got != want {
			t.Errorf("%s %s: identities=%v, want %v", tc.method, tc.path, got, want)
		}
	}
}

func TestListRouteLimits(t *testing.T) {
	api := newTestAPI(t, nil)
	expected := map[string]int{
		"/apis/ebs/v1/projects":                      100,
		"/apis/ebs/v1/projects/:project/builds":      100,
		"/apis/ebs/v1/projects/:project/jobs":        100,
		"/apis/ebs/v1/projects/:project/oldjobs":     100,
		"/apis/ebs/v1/projects/:project/snapshots":   100,
		"/apis/ebs/v1/projects/:project/buildinfos":  100,
		"/apis/ebs/v1/projects/:project/buildgraphs": 100,
		"/apis/ebs/v1/projects/:project/rpmrepos":    100,
	}
	for key, route := range registeredRoutePolicies(api) {
		want := 0
		if key.Method == http.MethodGet {
			want = expected[key.Path]
			delete(expected, key.Path)
		}
		if route.Options.ListLimit != want {
			t.Errorf("%s %s: limit=%d, want %d", key.Method, key.Path, route.Options.ListLimit, want)
		}
	}
	if len(expected) != 0 {
		t.Fatalf("missing list routes: %v", expected)
	}
}

func TestServiceRoutePermissions(t *testing.T) {
	api := newTestAPI(t, nil)
	expected := map[middleware.RouteKey]policy.Rule{
		{Method: http.MethodPut, Path: "/apis/ebs/v1/projects/:project"}:                     {ServicePermission: iamv1.MachinePermissionProjectsUpdate},
		{Method: http.MethodPatch, Path: "/apis/ebs/v1/projects/:project"}:                   {ServicePermission: iamv1.MachinePermissionProjectsUpdate},
		{Method: http.MethodPost, Path: "/apis/ebs/v1/projects/:project/builds"}:             {ServicePermission: iamv1.MachinePermissionBuildsCreate},
		{Method: http.MethodPost, Path: "/apis/ebs/v1/projects/:project/builds/:name/abort"}: {ServicePermission: iamv1.MachinePermissionBuildsAbort},
	}
	for key, route := range registeredRoutePolicies(api) {
		want, exists := expected[key]
		if (route.Access.Authorization.Identities&policy.AllowService != 0) != exists ||
			route.Access.Authorization.ServicePermission != want.ServicePermission {
			t.Fatalf("unexpected service access: %+v %+v", key, route.Access.Authorization)
		}
		delete(expected, key)
	}
	if len(expected) != 0 {
		t.Fatalf("missing service routes: %v", expected)
	}
}

func TestAuthenticationNoneDefault(t *testing.T) {
	definitions := []routeDefinition{{
		Method:         http.MethodGet,
		Path:           "/public",
		Authentication: middleware.AuthenticationNone,
		Handler:        func(c *gin.Context) { c.Status(http.StatusNoContent) },
	}}
	rules := make(map[middleware.RouteKey]middleware.RoutePolicy)
	registerRoutes(gin.New(), rules, definitions)
	rule := rules[middleware.RouteKey{Method: http.MethodGet, Path: "/public"}]
	if rule.Access.Authorization.Identities != policy.AllowAnonymous {
		t.Fatalf("missing anonymous default: %+v", rule)
	}
	if definitions[0].Authorization != (policy.Rule{}) {
		t.Fatal("registration mutated the route definition")
	}
}

func registeredRoutePolicies(api *testAPI) map[middleware.RouteKey]middleware.RoutePolicy {
	router := gin.New()
	rules := make(map[middleware.RouteKey]middleware.RoutePolicy)
	registerAuthRoutes(router, rules, api.authHandler)
	registerOAuthRoutes(router, rules, api.oauthHandler)
	registerForwardRoutes(router, rules, api.Handler)
	registerOtherRoutes(router, rules, api.Handler)
	return rules
}
