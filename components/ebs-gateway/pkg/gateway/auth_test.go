package gateway

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
