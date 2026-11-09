package oauth

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func testConfig(issuer string) Config {
	return Config{PublicBaseURL: "https://ebs.example.com",
		ContextKeys: keyRing{ActiveKeyID: "key-1", Keys: map[string]string{"key-1": base64.StdEncoding.EncodeToString(make([]byte, 32))}},
		Provider: ProviderConfig{ID: "community", Protocol: "oidc", Issuer: issuer,
			ClientID: "client", ClientSecret: "secret", TokenEndpointAuthMethod: "client_secret_basic", Scopes: []string{"openid", "profile", "email"},
			AllowedEndpointOrigins: []string{issuer}, SigningAlgorithms: []string{"RS256"}},
	}
}

func TestLoadConfigurationAndKeys(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig("https://id.example.com")
	data, _ := yaml.Marshal(cfg)
	path := filepath.Join(dir, "oauth.yaml")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil || loaded.Provider.ClientSecret != "secret" || loaded.ContextKeys.ActiveKeyID != "key-1" || loaded.fingerprint() != cfg.fingerprint() || loaded.PublicBaseURL != cfg.PublicBaseURL {
		t.Fatalf("load: %v", err)
	}
	if err := os.WriteFile(path, append(data, []byte("unexpected: true\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("accepted unknown configuration")
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("empty configuration must fail startup")
	}
	loaded, err = Load("")
	if err != nil || loaded.PublicBaseURL != "" {
		t.Fatal("default authentication changed")
	}
}

func TestConfigurationFingerprint(t *testing.T) {
	cfg := testConfig("https://id.example.com")
	original := cfg.fingerprint()
	if original != testConfig("https://id.example.com").fingerprint() {
		t.Fatal("identical configurations must have the same fingerprint")
	}
	cfg.ContextKeys.Keys["key-2"] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	cfg.ContextKeys.ActiveKeyID = "key-2"
	if cfg.fingerprint() != original {
		t.Fatal("Cookie key rotation must preserve the configuration fingerprint")
	}
	cfg.Provider.ClientSecret = "new-secret"
	if cfg.fingerprint() == original {
		t.Fatal("client secret changes must change the fingerprint")
	}
	cfg.Provider.ClientSecret = "secret"
	cfg.PublicBaseURL = "https://other.example.com"
	if cfg.fingerprint() == original {
		t.Fatal("public origin changes must change the fingerprint")
	}
}

func TestProviderConfigurationBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Config)
	}{
		{"HTTP public URL", func(c *Config) { c.PublicBaseURL = "http://ebs.example.com" }},
		{"partial endpoints", func(c *Config) { c.Provider.TokenURL = "https://id.example.com/token" }},
		{"untrusted endpoint", func(c *Config) {
			c.Provider.AuthorizationURL = "https://other.example.com/auth"
			c.Provider.TokenURL = "https://id.example.com/token"
			c.Provider.JWKSURL = "https://id.example.com/keys"
		}},
		{"unsafe algorithm", func(c *Config) { c.Provider.SigningAlgorithms = []string{"HS256"} }},
		{"missing PKCE adapter", func(c *Config) { c.Provider.Protocol = "oauth2"; c.Provider.Adapter = "username" }},
		{"missing openid", func(c *Config) { c.Provider.Scopes = []string{"profile"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig("https://id.example.com")
			tc.change(&c)
			if c.validate() == nil {
				t.Fatal("accepted invalid provider config")
			}
		})
	}
}

func TestReturnToBusinessRoutes(t *testing.T) {
	for _, path := range []string{"/", "/projects", "/projects/demo?tab=builds", "/operations", "/settings", "/admin/users"} {
		if !validReturnTo(path) {
			t.Errorf("rejected %q", path)
		}
	}
	for _, path := range []string{"//evil.example", "/\\evil", "https://evil.example", "/auth/callback", "/login", "/unknown", "/projects/../login", "/%2fevil", "/projects\n"} {
		if validReturnTo(path) {
			t.Errorf("accepted %q", path)
		}
	}
}

func TestSecretConfigurationValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Config)
	}{
		{"missing client secret", func(c *Config) { c.Provider.ClientSecret = "" }},
		{"missing active key", func(c *Config) { c.ContextKeys.ActiveKeyID = "missing" }},
		{"short key", func(c *Config) { c.ContextKeys.Keys["key-1"] = base64.StdEncoding.EncodeToString(make([]byte, 16)) }},
		{"invalid key encoding", func(c *Config) { c.ContextKeys.Keys["key-1"] = strings.Repeat("!", 44) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig("https://id.example.com")
			tc.change(&c)
			data, err := yaml.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "oauth.yaml")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("accepted unusable secret configuration")
			}
		})
	}
}

func TestConfigurationErrorRedactsSecretValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth.yaml")
	if err := os.WriteFile(path, []byte("contextKeys: sensitive-cookie-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || strings.Contains(err.Error(), "sensitive-cookie-key") {
		t.Fatalf("configuration error must not contain secret values: %v", err)
	}
}
