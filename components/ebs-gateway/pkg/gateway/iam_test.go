package gateway

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAdminLoginIssuesStandaloneAdminToken(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/iam/v1/authenticate" {
			t.Fatalf("unexpected upstream path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"authenticated":true,"username":"admin"}`)
	}), 100, 200)
	now := time.Unix(1790000000, 0)
	gw.now = func() time.Time { return now }
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"username":"admin","password":"admin-password-123"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	identity, err := gw.tokens.parse(response.Token, now)
	if err != nil {
		t.Fatalf("parse admin token: %v", err)
	}
	if identity.Subject != "admin" || !identity.IsAdmin() || identity.IsSystem() || len(identity.Scopes) != 1 {
		t.Fatalf("unexpected admin identity: %#v", identity)
	}
}

func TestRunnerTokenExchangeIssuesScopedToken(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/iam/v1/machineaccounts/runner-site-a/authenticate" {
			t.Fatalf("unexpected upstream request %s %s", r.Method, r.URL.Path)
		}
		var input map[string]string
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatalf("decode machine authentication: %v", err)
		}
		if input["clientSecret"] != "machine-secret" {
			t.Fatalf("unexpected machine credential payload: %#v", input)
		}
		_, _ = io.WriteString(w, `{"authenticated":true,"name":"runner-site-a","tokenTTLSeconds":7200}`)
	}), 100, 200)
	now := time.Unix(1790000000, 0)
	gw.now = func() time.Time { return now }

	req := httptest.NewRequest(http.MethodPost, "/auth/runner-token", strings.NewReader(`{"runner":"runner-001"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("runner-site-a", "machine-secret")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		AccessToken string `json:"accessToken"`
		TokenType   string `json:"tokenType"`
		ExpiresIn   int64  `json:"expiresIn"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	identity, err := gw.tokens.parse(response.AccessToken, now)
	if err != nil {
		t.Fatalf("parse runner token: %v", err)
	}
	if response.TokenType != "Bearer" || response.ExpiresIn != 7200 || identity.Subject != "runner-001" || identity.Runner != "runner-001" || !identity.IsRunner() {
		t.Fatalf("unexpected token response or identity: %#v %#v", response, identity)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("expected no-store response")
	}
}

func TestRunnerTokenExchangeRejectsInvalidCredential(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid", http.StatusUnauthorized)
	}), 100, 200)
	req := httptest.NewRequest(http.MethodPost, "/auth/runner-token", strings.NewReader(`{"runner":"runner-001"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("runner-site-a", "wrong-secret")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMachineAccountCreateValidatesAndForwards(t *testing.T) {
	secret := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/iam/v1/machineaccounts/register" {
			t.Fatalf("unexpected upstream request %s %s", r.Method, r.URL.Path)
		}
		var input struct {
			Name            string `json:"name"`
			ClientSecret    string `json:"clientSecret"`
			TokenTTLSeconds int64  `json:"tokenTTLSeconds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatalf("decode registration: %v", err)
		}
		if input.Name != "runner-site-a" || input.ClientSecret != secret || input.TokenTTLSeconds != 3600 {
			t.Fatalf("unexpected registration payload: %#v", input)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"name":"runner-site-a"}`)
	}), 100, 200)

	body := `{"name":"runner-site-a","clientSecret":"` + secret + `"}`
	req := authenticatedRequest(t, http.MethodPost, "/auth/machineaccounts", strings.NewReader(body), adminClaims())
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMachineAccountCreateRejectsInvalidInputAndScope(t *testing.T) {
	secret := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	var hits int
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}), 100, 200)
	tests := []struct {
		claims jwtClaims
		body   string
		status int
	}{
		{systemClaims(), `{"name":"runner-site-a","clientSecret":"` + secret + `"}`, http.StatusForbidden},
		{adminClaims(), `{"name":"Runner","clientSecret":"` + secret + `"}`, http.StatusBadRequest},
		{adminClaims(), `{"name":"runner-site-a","clientSecret":"short"}`, http.StatusBadRequest},
		{adminClaims(), `{"name":"runner-site-a","clientSecret":"` + secret + `","tokenTTLSeconds":299}`, http.StatusBadRequest},
		{adminClaims(), `{"name":"runner-site-a","clientSecret":"` + secret + `","unknown":true}`, http.StatusBadRequest},
	}
	for _, test := range tests {
		req := authenticatedRequest(t, http.MethodPost, "/auth/machineaccounts", strings.NewReader(test.body), test.claims)
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, req)
		if rec.Code != test.status {
			t.Fatalf("body %s: expected %d, got %d: %s", test.body, test.status, rec.Code, rec.Body.String())
		}
	}
	if hits != 0 {
		t.Fatalf("invalid requests reached upstream %d times", hits)
	}
}

func TestMachineAccountAPIRequiresAdminAndLimitsVerbs(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-EBS-Scopes") != "ebs:admin" {
			t.Fatalf("missing trusted machine admin scopes: %q", r.Header.Get("X-EBS-Scopes"))
		}
		w.WriteHeader(http.StatusOK)
	}), 100, 200)

	req := authenticatedRequest(t, http.MethodGet, "/apis/iam.ebs/v1/machineaccounts/runner-site-a", nil, systemClaims())
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected system-only token to be forbidden, got %d", rec.Code)
	}

	req = authenticatedRequest(t, http.MethodGet, "/apis/iam.ebs/v1/machineaccounts/runner-site-a", nil, adminClaims())
	rec = httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected machine admin GET 200, got %d", rec.Code)
	}

	req = authenticatedRequest(t, http.MethodPatch, "/apis/iam.ebs/v1/machineaccounts/runner-site-a", strings.NewReader(`{}`), adminClaims())
	rec = httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected PATCH 405, got %d", rec.Code)
	}
}

func TestPasswordChangeVerifiesCurrentPassword(t *testing.T) {
	var calls []string
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/internal/iam/v1/authenticate":
			_, _ = io.WriteString(w, `{"authenticated":true,"username":"alice"}`)
		case "/internal/iam/v1/users/alice/password":
			var input map[string]string
			_ = json.NewDecoder(r.Body).Decode(&input)
			if input["password"] != "new-password-123" {
				t.Fatalf("unexpected new password payload: %#v", input)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected upstream path %s", r.URL.Path)
		}
	}), 100, 200)
	req := authenticatedRequest(t, http.MethodPut, "/auth/users/alice/password", strings.NewReader(`{"currentPassword":"old-password-123","newPassword":"new-password-123"}`), userClaims("alice"))
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(calls) != 2 || calls[0] != "POST /internal/iam/v1/authenticate" || calls[1] != "PUT /internal/iam/v1/users/alice/password" {
		t.Fatalf("unexpected password flow: %#v", calls)
	}
}

func TestUserAPIRequiresAdmin(t *testing.T) {
	gw := newTestGateway(t, http.NotFoundHandler(), 100, 200)
	for _, claims := range []jwtClaims{userClaims("alice"), systemClaims()} {
		req := authenticatedRequest(t, http.MethodGet, "/apis/iam.ebs/v1/users/alice", nil, claims)
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected User API 403, got %d", rec.Code)
		}
	}
}

func TestAdminUserListFiltersAdministrators(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/apis/iam.ebs/v1/users" {
			t.Fatalf("unexpected upstream request %s %s", r.Method, r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"apiVersion":"iam.ebs/v1","kind":"UserList","items":[{"metadata":{"name":"charlie"},"spec":{"enabled":true}},{"metadata":{"name":"root-admin"},"spec":{"enabled":true,"admin":true}}]}`)
	}), 100, 200)
	req := authenticatedRequest(t, http.MethodGet, "/apis/iam.ebs/v1/users", nil, adminClaims())
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].Metadata.Name != "charlie" {
		t.Fatalf("administrator was not filtered: %#v", list.Items)
	}
}

func TestAdminCannotReadOrPromoteAdministrator(t *testing.T) {
	var putCalls int
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/iam.ebs/v1/users/root-admin":
			_, _ = io.WriteString(w, `{"apiVersion":"iam.ebs/v1","kind":"User","metadata":{"name":"root-admin","uid":"u1","resourceVersion":"v1:1:1"},"spec":{"enabled":true,"admin":true}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/iam.ebs/v1/users/charlie":
			_, _ = io.WriteString(w, `{"apiVersion":"iam.ebs/v1","kind":"User","metadata":{"name":"charlie","uid":"u2","resourceVersion":"v1:1:1"},"spec":{"enabled":true}}`)
		case r.Method == http.MethodPut:
			putCalls++
		default:
			t.Fatalf("unexpected upstream request %s %s", r.Method, r.URL.Path)
		}
	}), 100, 200)

	req := authenticatedRequest(t, http.MethodGet, "/apis/iam.ebs/v1/users/root-admin", nil, adminClaims())
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected administrator GET 403, got %d", rec.Code)
	}

	req = authenticatedRequest(t, http.MethodPatch, "/apis/iam.ebs/v1/users/charlie", strings.NewReader(`{"spec":{"admin":true}}`), adminClaims())
	req.Header.Set("Content-Type", "application/merge-patch+json")
	rec = httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected promotion PATCH 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if putCalls != 0 {
		t.Fatalf("promotion reached upstream PUT")
	}
}

func TestAdminCanPatchOrdinaryUser(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `{"apiVersion":"iam.ebs/v1","kind":"User","metadata":{"name":"charlie","uid":"u2","resourceVersion":"v1:1:1"},"spec":{"enabled":true,"displayName":"Charlie"}}`)
		case http.MethodPut:
			var user map[string]any
			if err := json.NewDecoder(r.Body).Decode(&user); err != nil {
				t.Fatalf("decode candidate: %v", err)
			}
			spec, _ := user["spec"].(map[string]any)
			if spec["enabled"] != false || userObjectIsAdmin(user) {
				t.Fatalf("unexpected candidate: %#v", user)
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(user)
		default:
			t.Fatalf("unexpected upstream method %s", r.Method)
		}
	}), 100, 200)
	req := authenticatedRequest(t, http.MethodPatch, "/apis/iam.ebs/v1/users/charlie", strings.NewReader(`{"spec":{"enabled":false}}`), adminClaims())
	req.Header.Set("Content-Type", "application/merge-patch+json")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func adminClaims() jwtClaims {
	claims := systemClaims()
	claims.Subject = "admin"
	claims.Scopes = []string{"ebs:admin"}
	claims.JTI = "admin-test-token"
	return claims
}
