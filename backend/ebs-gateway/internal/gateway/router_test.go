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
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/klog/v2"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-gateway/internal/iamclient"
	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/limit"
	"ebs-gateway/internal/middleware"
	"ebs-gateway/internal/oauth"
	"ebs-gateway/internal/upstream"
)

func TestLoginIPLimitAcrossUsernames(t *testing.T) {
	calls := 0
	api := newTestAPI(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	}))
	router := api.Router()
	for i := 0; i < 21; i++ {
		request := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(fmt.Sprintf(`{"username":"user-%d","password":"wrong"}`, i)))
		request.Header.Set("Content-Type", "application/json")
		request.RemoteAddr = "192.0.2.1:1234"
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		want := http.StatusUnauthorized
		if i == 20 {
			want = http.StatusTooManyRequests
		}
		if recorder.Code != want {
			t.Fatalf("request %d: status=%d body=%s", i, recorder.Code, recorder.Body)
		}
		if i == 20 && recorder.Header().Get("Retry-After") == "" {
			t.Fatal("missing Retry-After")
		}
	}
	if calls != 20 {
		t.Fatalf("throttled login reached IAM: calls=%d", calls)
	}
}

type testAPI struct {
	*Handler
	oauthHandler *oauth.Handler
	tokens       *identity.Tokens
	now          func() time.Time
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

func (a *testAPI) Router() *gin.Engine { return NewRouter(a.Handler, a.oauthHandler) }

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
	for _, route := range router.Routes() {
		if route.Path != "/healthz" && !middleware.HasRoutePolicy(route.Method, route.Path) {
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
		"/apis/ebs/v1/scripts":                                 "get post",
		"/apis/ebs/v1/scripts/{name}":                          "get put patch delete",
		"/apis/ebs/v1/runners/{runner}/jobs":                   "get",
		"/apis/ebs/v1/service-endpoints":                       "get",
		"/apis/iam.ebs/v1/users":                         "get",
		"/apis/iam.ebs/v1/machineaccounts":               "get",
		"/apis/iam.ebs/v1/machineaccounts/{name}":        "get delete put patch",
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
		Upstream: client, IAM: accounts, Tokens: tokens, Now: now,
		Limits: limit.New(100, burst, now), BodyLimit: 1 << 20, LocalAuthEnabled: cfg.LocalAuthenticationEnabled(),
		IPLimits: limit.New(1000, 2000, now),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &testAPI{Handler: api, oauthHandler: oauth.New(cfg, accounts, tokens, now), tokens: tokens, now: now}
}

func TestMachineTokenAccountType(t *testing.T) {
	for _, tc := range []struct {
		name, typeField, body      string
		upstreamStatus, wantStatus int
	}{
		{"runner", `,"type":"runner"`, `{"runner":"runner-1"}`, 200, 200},
		{"service", `,"type":"service"`, `{}`, 200, 200},
		{"service runner field", `,"type":"service"`, `{"runner":"runner-1"}`, 200, 400},
		{"service null runner", `,"type":"service"`, `{"runner":null}`, 200, 400},
		{"service empty runner", `,"type":"service"`, `{"runner":""}`, 200, 400},
		{"caller type", `,"type":"service"`, `{"type":"user"}`, 200, 400},
		{"caller scope", `,"type":"service"`, `{"scopes":["ebs:admin"]}`, 200, 400},
		{"runner missing name", `,"type":"runner"`, `{}`, 200, 400},
		{"missing type", "", `{}`, 200, 503},
		{"unknown type", `,"type":"unknown"`, `{}`, 200, 503},
		{"invalid credentials", "", `{}`, 401, 401},
		{"upstream unavailable", "", `{}`, 503, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodPost || r.URL.Path != "/internal/iam/v1/machineaccounts/authenticate" {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
				}
				body := `{"authenticated":true,"name":"worker","tokenTTLSeconds":3600` + tc.typeField + `}`
				return &http.Response{StatusCode: tc.upstreamStatus, Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			req := httptest.NewRequest(http.MethodPost, "/auth/machine-token", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.SetBasicAuth("worker", "secret")
			rec := httptest.NewRecorder()
			api.Router().ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
			var result struct {
				AccessToken string
				ExpiresIn   int
				TokenType   string
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if tc.wantStatus != http.StatusOK {
				if result.AccessToken != "" {
					t.Fatal("rejected account received a token")
				}
				return
			}
			who, err := api.tokens.Verify(result.AccessToken, api.now())
			if err != nil || who.Scope != "" || result.ExpiresIn != 3600 || result.TokenType != "Bearer" || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("token principal=%+v expiresIn=%d error=%v", who, result.ExpiresIn, err)
			}
			if tc.name == "service" {
				if who.Type != identity.ServiceType || who.Subject != "worker" {
					t.Fatalf("service principal=%+v", who)
				}
			} else if who.Type != identity.RunnerType || who.Subject != "runner-1" {
				t.Fatalf("runner principal=%+v", who)
			}
		})
	}
}

func TestMachineTokenLifetimeChangesAffectNewTokensOnly(t *testing.T) {
	for _, typ := range []string{"runner", "service"} {
		t.Run(typ, func(t *testing.T) {
			ttl := 7200
			api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodPost || r.URL.Path != "/internal/iam/v1/machineaccounts/authenticate" {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
				}
				body, _ := json.Marshal(map[string]any{"authenticated": true, "name": "worker", "type": typ, "tokenTTLSeconds": ttl})
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))}, nil
			}))
			exchange := func() string {
				body := `{}`
				if typ == "runner" {
					body = `{"runner":"runner-1"}`
				}
				req := httptest.NewRequest(http.MethodPost, "/auth/machine-token", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.SetBasicAuth("worker", "secret")
				res := httptest.NewRecorder()
				api.Router().ServeHTTP(res, req)
				var result struct {
					AccessToken string
					ExpiresIn   int
				}
				if err := json.Unmarshal(res.Body.Bytes(), &result); err != nil || res.Code != 200 || result.ExpiresIn != ttl {
					t.Fatalf("status=%d body=%s err=%v", res.Code, res.Body, err)
				}
				return result.AccessToken
			}
			oldToken := exchange()
			oldPrincipal, err := api.tokens.Verify(oldToken, api.now())
			if err != nil {
				t.Fatal(err)
			}
			ttl = 300
			newToken := exchange()
			newPrincipal, err := api.tokens.Verify(newToken, api.now())
			if err != nil {
				t.Fatal(err)
			}
			if oldPrincipal.ExpiresAt.Sub(newPrincipal.ExpiresAt) != 6900*time.Second {
				t.Fatal("new token did not use the updated lifetime")
			}
			stillValid, err := api.tokens.Verify(oldToken, api.now().Add(600*time.Second))
			if err != nil || !stillValid.ExpiresAt.Equal(oldPrincipal.ExpiresAt) {
				t.Fatalf("old token expiry changed: %v", err)
			}
			if _, err := api.tokens.Verify(newToken, api.now().Add(600*time.Second)); err == nil {
				t.Fatal("new token outlived its configured lifetime")
			}
		})
	}
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
		{"invalid project", `{"metadata":{"name":"service"},"spec":{"type":"service","projects":["*"]}}`, 200, 503},
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
		{"PATCH", "/apis/ebs/v1/projects/team/status"},
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
		{"namespace mismatch", "POST", "/apis/ebs/v1/projects/team/builds", "application/json", `{"metadata":{"name":"b","namespace":"other"}}`, 400, false},
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
					status, body = http.StatusCreated, test.body
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
			api.Router().ServeHTTP(rec, request)
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

func TestRegisterMachineAccountConfigurationForwarding(t *testing.T) {
	for _, typeField := range []string{"", `,"type":"runner"`, `,"type":"service"`,
		`,"type":"service","projects":["team","other"],"permissions":["projects:update","builds:create","builds:abort"]`,
	} {
		t.Run(typeField, func(t *testing.T) {
			registered := false
			api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := `{"metadata":{"name":"admin"},"spec":{"enabled":true,"scopes":["ebs:admin"]}}`
				status := http.StatusOK
				if r.URL.Path == "/internal/iam/v1/machineaccounts/register" {
					registered = true
					var input map[string]any
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Fatal(err)
					}
					var want map[string]any
					_ = json.Unmarshal([]byte(`{"name":"worker"`+typeField+`}`), &want)
					if input["type"] != want["type"] || input["name"] != "worker" || input["tokenTTLSeconds"] != float64(3600) {
						t.Fatalf("forwarded input=%v", input)
					}
					for _, field := range []string{"projects", "permissions"} {
						if !reflect.DeepEqual(input[field], want[field]) {
							t.Fatalf("forwarded %s=%v, want %v", field, input[field], want[field])
						}
					}
					body, status = `{"name":"worker"}`, http.StatusCreated
				} else if r.URL.Path != "/apis/iam.ebs/v1/users/admin" {
					t.Fatalf("unexpected request %s", r.URL)
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			token, err := api.tokens.Issue("admin", identity.UserType, identity.AdminScope, time.Hour, api.now())
			if err != nil {
				t.Fatal(err)
			}
			secret := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
			req := httptest.NewRequest(http.MethodPost, "/auth/machineaccounts", strings.NewReader(`{"name":"worker","clientSecret":"`+secret+`"`+typeField+`}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			api.Router().ServeHTTP(rec, req)
			if rec.Code != http.StatusCreated || !registered {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
		})
	}
}

func TestMachineAccountGrantUpdates(t *testing.T) {
	const path = "/apis/iam.ebs/v1/machineaccounts/automation"
	const original = `{"apiVersion":"iam.ebs/v1","kind":"MachineAccount","metadata":{"name":"automation","uid":"id","resourceVersion":"v1:3:1"},"spec":{"type":"service","tokenTTLSeconds":3600,"projects":["demo"],"permissions":["projects:update"]}}`
	for _, tc := range []struct {
		name, method, media, body string
		scope                     identity.Scope
		accountType               string
		status                    int
	}{
		{"merge patch", "PATCH", "application/merge-patch+json", `{"metadata":{"uid":"id","resourceVersion":"v1:3:1"},"spec":{"projects":["other"],"permissions":["builds:abort"]}}`, identity.AdminScope, "service", 200},
		{"put", "PUT", "application/json", strings.ReplaceAll(original, "projects:update", "builds:abort"), identity.AdminScope, "service", 200},
		{"json patch", "PATCH", "application/json-patch+json", `[{"op":"test","path":"/metadata/resourceVersion","value":"v1:3:1"},{"op":"replace","path":"/spec/permissions","value":[]}]`, identity.AdminScope, "service", 200},
		{"service TTL", "PATCH", "application/merge-patch+json", `{"spec":{"tokenTTLSeconds":7200}}`, identity.AdminScope, "service", 200},
		{"type", "PATCH", "application/merge-patch+json", `{"spec":{"type":"runner"}}`, identity.AdminScope, "service", 403},
		{"metadata", "PATCH", "application/merge-patch+json", `{"metadata":{"labels":{"x":"y"}}}`, identity.AdminScope, "service", 403},
		{"credential", "PATCH", "application/merge-patch+json", `{"credential":{"secretHash":"changed"}}`, identity.AdminScope, "service", 403},
		{"stale", "PATCH", "application/merge-patch+json", `{"metadata":{"resourceVersion":"v1:2:1"}}`, identity.AdminScope, "service", 409},
		{"replaced", "PATCH", "application/merge-patch+json", `{"metadata":{"uid":"other"}}`, identity.AdminScope, "service", 409},
		{"runner TTL", "PATCH", "application/merge-patch+json", `{"spec":{"tokenTTLSeconds":7200}}`, identity.AdminScope, "runner", 200},
		{"runner grants", "PATCH", "application/merge-patch+json", `{"spec":{"permissions":["builds:abort"]}}`, identity.AdminScope, "runner", 403},
		{"ordinary user", "PATCH", "application/merge-patch+json", `{}`, identity.UserScope, "service", 403},
		{"ops user", "PATCH", "application/merge-patch+json", `{}`, identity.OpsScope, "service", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := strings.ReplaceAll(original, `"type":"service"`, `"type":"`+tc.accountType+`"`)
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/apis/iam.ebs/v1/users/admin":
					body = `{"metadata":{"name":"admin"},"spec":{"enabled":true,"scopes":["` + string(tc.scope) + `"]}}`
				case r.Method == http.MethodGet && r.URL.Path == path:
				case r.Method == http.MethodPut && r.URL.Path == path:
					writes++
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						t.Fatal(err)
					}
					body = string(raw)
					wantTTL := `"tokenTTLSeconds":3600`
					if strings.Contains(tc.name, "TTL") {
						wantTTL = `"tokenTTLSeconds":7200`
					}
					if !strings.Contains(body, wantTTL) || !strings.Contains(body, `"resourceVersion":"v1:3:1"`) {
						t.Fatalf("unexpected update: %s", body)
					}
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			token, err := api.tokens.Issue("admin", identity.UserType, tc.scope, time.Hour, api.now())
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(tc.method, path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", tc.media)
			res := httptest.NewRecorder()
			api.Router().ServeHTTP(res, req)
			if res.Code != tc.status || (writes == 1) != (tc.status == 200) {
				t.Fatalf("status=%d writes=%d body=%s", res.Code, writes, res.Body)
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

func TestConfigAndScriptServiceAuthorizationAudit(t *testing.T) {
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
	router := gin.New()
	var decision string
	router.Use(func(c *gin.Context) {
		c.Next()
		decision = c.GetString("serviceAuthorization")
	})
	router.Use(middleware.ValidateWatchRoute, middleware.LimitIP(api.ipLimits), api.auth.AuthenticateRequest,
		middleware.LimitIdentity(api.limits), middleware.AdmitIdentity)
	registerConfigs(router, api.Handler)
	registerScripts(router, api.Handler)
	for _, tc := range []struct {
		query, audit string
		status       int
	}{
		{"", "denied", http.StatusForbidden},
		{"?watch=true", "", http.StatusForbidden},
		{"", "", http.StatusTooManyRequests},
	} {
		for _, resource := range []string{"configs", "scripts"} {
			req := httptest.NewRequest(http.MethodPost, "/apis/ebs/v1/"+resource+tc.query, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			res := httptest.NewRecorder()
			router.ServeHTTP(res, req)
			if res.Code != tc.status || decision != tc.audit {
				t.Errorf("%s: status=%d audit=%q, want %d/%q", req.URL, res.Code, decision, tc.status, tc.audit)
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

func TestLoginAndCheck(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/internal/iam/v1/users/authenticate":
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

func TestCheckTokenUsesScopesForUserPermissions(t *testing.T) {
	for _, scope := range []identity.Scope{identity.UserScope, identity.OpsScope, identity.AdminScope} {
		t.Run(string(scope), func(t *testing.T) {
			api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.Path != "/apis/iam.ebs/v1/users/alice" {
					t.Fatalf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
				}
				body := fmt.Sprintf(`{"metadata":{"name":"alice"},"spec":{"enabled":true,"scopes":[%q]}}`, scope)
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			token, err := api.tokens.Issue("alice", identity.UserType, scope, time.Hour, api.now())
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
	token, err := api.tokens.Issue("runner-a", identity.RunnerType, "", time.Hour, api.now())
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
		response.Identity.Type != identity.RunnerType || response.Identity.Scopes == nil || len(response.Identity.Scopes) != 0 {
		t.Fatalf("unexpected token check response: status=%d response=%+v", recorder.Code, response)
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
	api.ipLimits = limit.New(1, 1, api.now)
	api.limits = limit.New(1, 1, api.now)
	router := api.Router()
	for _, exhausted := range []bool{false, true} {
		if exhausted {
			if !api.ipLimits.Allow("192.0.2.1") || !api.limits.Allow("anonymous/192.0.2.1") {
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
		if response.Code != http.StatusTooManyRequests {
			t.Fatalf("%s %s bypassed business limits: %d", methodPath.method, methodPath.path, response.Code)
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

func TestGlobalIPLimitBeforeAuthentication(t *testing.T) {
	lookups, forwards := 0, 0
	api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"items":[]}`
		if r.URL.Path == "/apis/iam.ebs/v1/users/alice" {
			lookups++
			body = `{"metadata":{"name":"alice"},"spec":{"enabled":true,"scopes":["ebs:user"]}}`
		} else {
			forwards++
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	api.ipLimits = limit.New(1, 1, api.now)
	token, err := api.tokens.Issue("alice", identity.UserType, identity.UserScope, time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	router := api.Router()
	for _, tc := range []struct {
		path, address, token string
		status               int
		lookups, forwards    int
	}{
		{"/apis/ebs/v1/projects", "192.0.2.1:1234", "invalid", 401, 0, 0},
		{"/apis/ebs/v1/projects", "192.0.2.1:1234", token, 429, 0, 0},
		{"/auth/providers", "192.0.2.1:1234", "", 429, 0, 0},
		{"/unknown", "192.0.2.1:1234", "", 429, 0, 0},
		{"/healthz", "192.0.2.1:1234", "invalid", 200, 0, 0},
		{"/apis/ebs/v1/projects?watch=true", "192.0.2.1:1234", token, 403, 0, 0},
		{"/apis/ebs/v1/projects", "192.0.2.2:1234", token, 200, 1, 1},
	} {
		request := httptest.NewRequest(http.MethodGet, tc.path, nil)
		request.RemoteAddr = tc.address
		if tc.token != "" {
			request.Header.Set("Authorization", "Bearer "+tc.token)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != tc.status || lookups != tc.lookups || forwards != tc.forwards {
			t.Fatalf("%s: status=%d lookups=%d forwards=%d, want %d/%d/%d", tc.path, response.Code, lookups, forwards, tc.status, tc.lookups, tc.forwards)
		}
		if tc.status == 429 && response.Header().Get("Retry-After") != "1" {
			t.Fatal("missing retry delay")
		}
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
		{"GET", "/scripts/custom", "service", 200},
		{"GET", "/scripts", "service", 200},
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
	token, err := api.tokens.Issue("admin", identity.UserType, identity.AdminScope, time.Hour, api.now())
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
	token, err := api.tokens.Issue("admin", identity.UserType, identity.AdminScope, time.Hour, api.now())
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
			if request.Header.Get("Content-Type") != "application/json" || request.ContentLength != int64(len(body)) || request.Header.Get("Content-Length") != "" {
				t.Errorf("invalid rewritten headers=%v content length=%d, body length=%d", request.Header, request.ContentLength, len(body))
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
		default:
			t.Fatalf("unexpected method %s", request.Method)
			return nil, nil
		}
	}))
	token, err := api.tokens.Issue("runner-1", identity.RunnerType, "", time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPatch, "/apis/ebs/v1/runners/runner-1/status", strings.NewReader(`{"status":{"phase":"Online"}}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/merge-patch+json")
	request.Header.Set("Content-Length", fmt.Sprint(request.ContentLength))
	response := httptest.NewRecorder()
	api.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK || writes != 1 {
		t.Fatalf("status=%d writes=%d body=%q", response.Code, writes, response.Body.String())
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
		{"anonymous project write", http.MethodPatch, "/apis/ebs/v1/projects/team", "", http.StatusUnauthorized},
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
		{"build update", http.MethodPut, "/apis/ebs/v1/projects/team/builds/b", userToken, http.StatusNotFound},
		{"build patch", http.MethodPatch, "/apis/ebs/v1/projects/team/builds/b", userToken, http.StatusNotFound},
		{"build status update", http.MethodPut, "/apis/ebs/v1/projects/team/builds/b/status", userToken, http.StatusNotFound},
		{"build status patch", http.MethodPatch, "/apis/ebs/v1/projects/team/builds/b/status", userToken, http.StatusNotFound},
		{"anonymous unsupported method", http.MethodPut, "/apis/ebs/v1/projects/team/jobs", "", http.StatusUnauthorized},
		{"IAM create is not registered", http.MethodPost, "/apis/iam.ebs/v1/users", userToken, http.StatusNotFound},

		{"watch on job creation", http.MethodPost, "/apis/ebs/v1/projects/team/jobs?watch=true", userToken, http.StatusForbidden},
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

func TestForwardResourceRequiresMatchingGuards(t *testing.T) {
	api := newTestAPI(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("incomplete guard chain reached upstream")
		return nil, nil
	}))
	token, err := api.tokens.Issue("runner-1", identity.RunnerType, "", time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	jobs := api.publicReadGuard("jobs", "")
	for _, tc := range []struct {
		name, method, pattern, path string
		guards                      []gin.HandlerFunc
	}{
		{"no guards", "GET", "/projects", "/projects", nil},
		{"private resource marked public", "GET", "/configs/:name", "/configs/example", []gin.HandlerFunc{api.publicReadGuard("configs", "")}},
		{"write marked public", "POST", "/projects", "/projects", []gin.HandlerFunc{api.publicReadGuard("projects", "")}},
		{"missing job read", "GET", "/:project/jobs/:name", "/team/jobs/j", []gin.HandlerFunc{jobs}},
		{"read guard before authorization", "GET", "/:project/jobs/:name", "/team/jobs/j", []gin.HandlerFunc{api.runnerJobReadGuard, jobs}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := gin.New()
			router.Use(api.auth.AuthenticateRequest, func(c *gin.Context) {
				who := middleware.Principal(c)
				if who.Type != identity.RunnerType || who.Subject != "runner-1" {
					t.Fatalf("unexpected authenticated identity: %+v", who)
				}
				c.Next()
			})
			router.Handle(tc.method, tc.pattern, append(tc.guards, api.forwardResource)...)
			request := httptest.NewRequest(tc.method, tc.path, nil)
			request.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("status=%d, want 500: %s", response.Code, response.Body)
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
