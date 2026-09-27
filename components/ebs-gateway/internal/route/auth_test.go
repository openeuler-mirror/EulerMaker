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
