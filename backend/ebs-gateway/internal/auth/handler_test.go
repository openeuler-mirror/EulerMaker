package auth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/iamclient"
	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/middleware"
	"ebs-gateway/internal/upstream"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type testAPI struct {
	*Handler
	auth *middleware.Auth
}

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
	accounts := iamclient.New(client)
	handler, err := New(Dependencies{
		IAM: accounts, Tokens: tokens, Now: now, BodyLimit: 1 << 20, LocalAuthEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &testAPI{Handler: handler, auth: middleware.NewAuth(tokens, accounts, now)}
}

func (a *testAPI) Router() *gin.Engine {
	router := gin.New()
	router.Use(a.auth.AuthenticateRequest, middleware.AdmitIdentity)
	router.POST("/auth/register", a.RegisterUser)
	router.POST("/auth/login", a.Login)
	router.POST("/auth/machine-token", a.MachineToken)
	router.POST("/auth/check", a.CheckToken)
	router.POST("/auth/machineaccounts", a.RegisterMachine)
	router.PUT("/auth/users/:name/password", a.ChangePassword)
	return router
}

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

func TestLoginAndCheck(t *testing.T) {
	userSpec := `{"enabled":true,"scopes":["ebs:user"]}`
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/internal/iam/v1/users/authenticate":
			body = `{"authenticated":true,"username":"alice"}`
		case "/apis/iam.ebs/v1/users/alice":
			body = `{"metadata":{"name":"alice"},"spec":` + userSpec + `}`
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
	for _, spec := range []string{
		`{"enabled":false,"scopes":["ebs:user"]}`,
		`{"enabled":null,"scopes":["ebs:user"]}`,
		`{"scopes":["ebs:user"]}`,
	} {
		userSpec = spec
		login := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"username":"alice","password":"correct-password"}`))
		login.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, login)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("inactive user login: spec=%s status=%d body=%s", spec, response.Code, response.Body)
		}
		check := httptest.NewRequest(http.MethodPost, "/auth/check", nil)
		check.Header.Set("Authorization", "Bearer "+result.Token)
		response = httptest.NewRecorder()
		router.ServeHTTP(response, check)
		if response.Code != http.StatusForbidden {
			t.Fatalf("inactive user token check: spec=%s status=%d body=%s", spec, response.Code, response.Body)
		}
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

func TestNew(t *testing.T) {
	client, err := upstream.New("https://api.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := identity.NewTokens(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32))))
	if err != nil {
		t.Fatal(err)
	}
	deps := Dependencies{IAM: iamclient.New(client), Tokens: tokens, BodyLimit: 1 << 20, LocalAuthEnabled: true}
	handler, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	if handler.iam != deps.IAM || handler.tokens != tokens || handler.now == nil || !handler.localAuthEnabled ||
		handler.registerIP == nil || handler.registrationUserLimit == nil || handler.loginIP == nil ||
		handler.loginLimit == nil || handler.machineLimit == nil {
		t.Fatal("authentication dependencies were not initialized")
	}
	for _, invalid := range []Dependencies{
		{Tokens: tokens, BodyLimit: 1},
		{IAM: deps.IAM, BodyLimit: 1},
		{IAM: deps.IAM, Tokens: tokens},
	} {
		if _, err := New(invalid); err == nil {
			t.Fatal("invalid dependencies accepted")
		}
	}
}
