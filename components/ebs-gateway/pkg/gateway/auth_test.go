package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPublicTokenCheckReturnsValidatedIdentity(t *testing.T) {
	gw := newTestGateway(t, http.NotFoundHandler(), 100, 200)
	now := time.Unix(1790000000, 0)
	gw.now = func() time.Time { return now }
	token, expires, err := gw.tokens.issueRunner("runner-1", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/auth/check", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	gw.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("check: %d %s", w.Code, w.Body.String())
	}
	var response struct {
		Authenticated bool `json:"authenticated"`
		Identity      struct {
			Type, Name string
			Scopes     []string
		} `json:"identity"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Authenticated || response.Identity.Type != "runner" || response.Identity.Name != "runner-1" || response.ExpiresAt.Unix() != expires {
		t.Fatalf("unexpected response: %#v", response)
	}

	userToken, _, _ := gw.tokens.issueUser("alice", now)
	r = httptest.NewRequest(http.MethodPost, "/auth/check", nil)
	r.Header.Set("Authorization", "Bearer "+userToken)
	w = httptest.NewRecorder()
	gw.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("user token status: %d", w.Code)
	}
}

func TestPublicTokenCheckRejectsRequestBody(t *testing.T) {
	gw := newTestGateway(t, http.NotFoundHandler(), 100, 200)
	now := time.Unix(1790000000, 0)
	gw.now = func() time.Time { return now }
	token, _, err := gw.tokens.issueRunner("runner-1", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/auth/check", bytes.NewBufferString(`{}`))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	gw.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("body status: %d", w.Code)
	}
}

func TestTokenCheckDoesNotExposeLegacyInternalPath(t *testing.T) {
	gw := newTestGateway(t, http.NotFoundHandler(), 100, 200)
	r := httptest.NewRequest(http.MethodPost, "/internal/auth/v1/check", nil)
	w := httptest.NewRecorder()
	gw.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("legacy internal path status: %d", w.Code)
	}
}

func TestTokenCheckDoesNotExposeVersionedPath(t *testing.T) {
	gw := newTestGateway(t, http.NotFoundHandler(), 100, 200)
	r := httptest.NewRequest(http.MethodPost, "/auth/v1/check", nil)
	w := httptest.NewRecorder()
	gw.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("versioned path status: %d", w.Code)
	}
}

func TestTokenManagerLoadsSecretAndIssuesUserToken(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	path := filepath.Join(t.TempDir(), "jwt-secret")
	data := []byte(base64.StdEncoding.EncodeToString(key))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	manager, err := newTokenManager(Config{
		JWTSecretFile: path,
	})
	if err != nil {
		t.Fatalf("load token manager: %v", err)
	}
	now := time.Unix(1790000000, 0)
	token, expiresAt, err := manager.issueUser("alice", now)
	if err != nil {
		t.Fatalf("issue user token: %v", err)
	}
	if expiresAt != now.Add(time.Hour).Unix() {
		t.Fatalf("unexpected expiry %d", expiresAt)
	}
	identity, err := manager.parse(token, now)
	if err != nil {
		t.Fatalf("parse issued token: %v", err)
	}
	if identity.Subject != "alice" || len(identity.Scopes) != 1 || identity.Scopes[0] != "ebs:user" {
		t.Fatalf("unexpected identity: %#v", identity)
	}
}

func TestTokenManagerRejectsShortKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jwt-secret")
	data := []byte(base64.StdEncoding.EncodeToString([]byte("too-short")))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	if _, err := newTokenManager(Config{JWTSecretFile: path}); err == nil {
		t.Fatal("expected short key to be rejected")
	}
}

func TestTokenManagerAcceptsOnlyUnifiedAdminScope(t *testing.T) {
	manager := &tokenManager{key: []byte("0123456789abcdef0123456789abcdef")}
	now := time.Unix(1790000000, 0)
	base := jwtClaims{
		Subject: "admin", Issuer: jwtIssuer, Audience: jwtAudience,
		IssuedAt: now.Unix(), NotBefore: now.Unix(), Exp: now.Add(time.Hour).Unix(), JTI: "admin-token",
	}
	base.Scopes = []string{"ebs:admin"}
	if err := manager.validateClaims(base, now); err != nil {
		t.Fatalf("expected unified admin scope to be valid: %v", err)
	}
	for _, scopes := range [][]string{{"ebs:system", "ebs:admin"}} {
		claims := base
		claims.Scopes = scopes
		if err := manager.validateClaims(claims, now); err == nil {
			t.Fatalf("expected scopes %#v to be rejected", scopes)
		}
	}
}

func TestTokenManagerAcceptsStandaloneOpsScope(t *testing.T) {
	manager := &tokenManager{key: []byte("0123456789abcdef0123456789abcdef")}
	now := time.Unix(1790000000, 0)
	claims := jwtClaims{Subject: "operator", Scopes: []string{"ebs:ops"}, Issuer: jwtIssuer, Audience: jwtAudience, IssuedAt: now.Unix(), NotBefore: now.Unix(), Exp: now.Add(time.Hour).Unix(), JTI: "ops-token"}
	if err := manager.validateClaims(claims, now); err != nil {
		t.Fatalf("ops scope rejected: %v", err)
	}
	claims.Scopes = []string{"ebs:user", "ebs:ops"}
	if err := manager.validateClaims(claims, now); err == nil {
		t.Fatal("combined user and ops scopes were accepted")
	}
}
