package oauth

import (
	"encoding/base64"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ebs-gateway/internal/iam"
)

func TestCookieRotationAndPurpose(t *testing.T) {
	f := newFixture(t)
	cookie, _ := f.login(t)
	req := httptest.NewRequest("POST", "/", nil)
	req.AddCookie(cookie)
	old, err := f.h.readCookie(req, "login")
	if err != nil || old.Verifier == "" {
		t.Fatal(err)
	}
	f.h.config.ContextKeys.Keys["new-key"] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	f.h.config.ContextKeys.ActiveKeyID = "new-key"
	if _, err := f.h.readCookie(req, "login"); err != nil {
		t.Fatal("old key failed during rotation", err)
	}
	fresh, err := f.h.encodeCookie(old)
	if err != nil || !strings.Contains(fresh.Value, ".new-key.") {
		t.Fatal(err)
	}
	cookie.Name = cookieName("register")
	req = httptest.NewRequest("POST", "/", nil)
	req.AddCookie(cookie)
	if _, err := f.h.readCookie(req, "register"); err == nil {
		t.Fatal("accepted wrong purpose")
	}
	cookie.Name = cookieName("login")
	req = httptest.NewRequest("POST", "/", nil)
	req.AddCookie(cookie)
	delete(f.h.config.ContextKeys.Keys, "key-1")
	if _, err := f.h.readCookie(req, "login"); err == nil {
		t.Fatal("accepted retired key")
	}
}

func TestCookieAuthenticationConfigChanges(t *testing.T) {
	f := newFixture(t)
	cookie, _ := f.login(t)
	req := httptest.NewRequest("POST", "/", nil)
	req.AddCookie(cookie)
	original := f.h.config
	for _, tc := range []struct {
		name   string
		change func(*Config)
	}{
		{"client ID", func(c *Config) { c.Provider.ClientID = "another-client" }},
		{"client secret", func(c *Config) { c.Provider.ClientSecret = "rotated-secret" }},
		{"issuer", func(c *Config) { c.Provider.Issuer = "https://other.example.com" }},
		{"protocol", func(c *Config) { c.Provider.Protocol = "oauth2" }},
		{"adapter", func(c *Config) { c.Provider.Adapter = "userinfo-sub" }},
		{"token authentication", func(c *Config) { c.Provider.TokenEndpointAuthMethod = "client_secret_post" }},
		{"authorization endpoint", func(c *Config) { c.Provider.AuthorizationURL = "https://id.example.com/authorize" }},
		{"token endpoint", func(c *Config) { c.Provider.TokenURL = "https://id.example.com/token" }},
		{"JWKS endpoint", func(c *Config) { c.Provider.JWKSURL = "https://id.example.com/keys" }},
		{"userinfo endpoint", func(c *Config) { c.Provider.UserInfoURL = "https://id.example.com/user" }},
		{"scopes", func(c *Config) { c.Provider.Scopes = []string{"openid", "email"} }},
		{"endpoint origins", func(c *Config) { c.Provider.AllowedEndpointOrigins = []string{"https://other.example.com"} }},
		{"signing algorithms", func(c *Config) { c.Provider.SigningAlgorithms = []string{"ES256"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.h.config = original
			tc.change(&f.h.config)
			_, err := f.h.readCookie(req, "login")
			var apiErr *apiError
			if !errors.As(err, &apiErr) || apiErr.code != "OAuthConfigChanged" {
				t.Fatalf("config change: %v", err)
			}
		})
	}
	f.h.config = original
	if _, err := f.h.readCookie(req, "login"); err != nil {
		t.Fatal("identical configuration must accept the context", err)
	}
}

func TestCookiePayloadLimit(t *testing.T) {
	f := newFixture(t)
	state := browserContext{Purpose: "register", Provider: "community", ConfigFingerprint: f.h.config.fingerprint(), IssuedAt: f.now.Unix(), ExpiresAt: f.now.Add(time.Minute).Unix(), ReturnTo: "/",
		RegistrationID: randomValue(), Identity: &iam.ExternalIdentity{Issuer: "issuer", Subject: "subject"}, Profile: profile{DisplayName: strings.Repeat("x", 5000)}}
	cookie, err := f.h.encodeCookie(state)
	if err != nil || len(cookie.String()) > 3800 {
		t.Fatal("optional profile was not dropped", err)
	}
	req := httptest.NewRequest("POST", "/", nil)
	req.AddCookie(cookie)
	decoded, err := f.h.readCookie(req, "register")
	if err != nil || decoded.Profile != (profile{}) || decoded.Identity.Subject != "subject" {
		t.Fatal("identity changed", err)
	}
	state.Identity.Issuer, state.Identity.Subject = strings.Repeat("a", 2048), strings.Repeat("b", 1024)
	_, err = f.h.encodeCookie(state)
	var failure *apiError
	if !errors.As(err, &failure) || failure.code != "OAuthContextTooLarge" {
		t.Fatalf("size failure=%v", err)
	}
}
