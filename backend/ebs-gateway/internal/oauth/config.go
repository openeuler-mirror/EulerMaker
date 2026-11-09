package oauth

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	PublicBaseURL string         `yaml:"publicBaseURL"`
	ContextKeys   keyRing        `yaml:"contextKeys"`
	Provider      ProviderConfig `yaml:"provider"`
}

type ProviderConfig struct {
	ID                      string   `yaml:"id"`
	Protocol                string   `yaml:"protocol"`
	Issuer                  string   `yaml:"issuer"`
	ClientID                string   `yaml:"clientID"`
	ClientSecret            string   `yaml:"clientSecret"`
	TokenEndpointAuthMethod string   `yaml:"tokenEndpointAuthMethod"`
	Scopes                  []string `yaml:"scopes"`
	AllowedEndpointOrigins  []string `yaml:"allowedEndpointOrigins"`
	SigningAlgorithms       []string `yaml:"signingAlgorithms"`
	AuthorizationURL        string   `yaml:"authorizationURL"`
	TokenURL                string   `yaml:"tokenURL"`
	JWKSURL                 string   `yaml:"jwksURL"`
	UserInfoURL             string   `yaml:"userInfoURL"`
	Adapter                 string   `yaml:"adapter"`
}

type keyRing struct {
	ActiveKeyID string            `yaml:"activeKeyID"`
	Keys        map[string]string `yaml:"keys"`
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// Load validates local configuration; an empty path returns the disabled zero configuration.
func Load(path string) (Config, error) {
	var cfg Config
	if path == "" {
		return cfg, nil
	}
	if err := readYAML(path, &cfg); err != nil {
		return cfg, err
	}
	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	if strings.TrimSpace(cfg.Provider.ClientSecret) == "" {
		return cfg, fmt.Errorf("empty OAuth client secret")
	}
	if cfg.ContextKeys.ActiveKeyID == "" || cfg.ContextKeys.Keys[cfg.ContextKeys.ActiveKeyID] == "" {
		return cfg, fmt.Errorf("missing active OAuth Cookie key")
	}
	for id, encoded := range cfg.ContextKeys.Keys {
		key, err := base64.StdEncoding.DecodeString(encoded)
		if !dnsLabel.MatchString(id) || err != nil || len(key) != 32 {
			return cfg, fmt.Errorf("invalid OAuth Cookie key")
		}
	}
	return cfg, nil
}

func readYAML(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		// YAML errors can include input values; this file contains credentials.
		return fmt.Errorf("invalid OAuth YAML")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("OAuth YAML must contain one document")
	}
	return nil
}

func (c Config) validate() error {
	if !validOrigin(c.PublicBaseURL) {
		return fmt.Errorf("publicBaseURL must be an HTTPS origin")
	}
	p := c.Provider
	if !dnsLabel.MatchString(p.ID) || p.ClientID == "" || p.Issuer == "" {
		return fmt.Errorf("OAuth provider identity and clientID are required")
	}
	if p.TokenEndpointAuthMethod != "client_secret_basic" && p.TokenEndpointAuthMethod != "client_secret_post" {
		return fmt.Errorf("unsupported tokenEndpointAuthMethod")
	}
	if len(p.AllowedEndpointOrigins) == 0 {
		return fmt.Errorf("allowedEndpointOrigins is required")
	}
	for _, origin := range p.AllowedEndpointOrigins {
		if !validOrigin(origin) {
			return fmt.Errorf("invalid allowed endpoint origin")
		}
	}
	for _, scope := range p.Scopes {
		if scope == "" || strings.ContainsAny(scope, " \t\r\n") {
			return fmt.Errorf("invalid OAuth scope")
		}
	}
	if p.Protocol == "oidc" {
		issuer, err := url.Parse(p.Issuer)
		if err != nil || issuer.Scheme != "https" || issuer.Hostname() == "" || issuer.User != nil || issuer.RawQuery != "" || issuer.ForceQuery || issuer.Fragment != "" {
			return fmt.Errorf("invalid OIDC issuer")
		}
		if !slices.Contains(p.Scopes, "openid") || len(p.SigningAlgorithms) == 0 || p.Adapter != "" {
			return fmt.Errorf("OIDC requires openid scope and signingAlgorithms, without an adapter")
		}
		for _, alg := range p.SigningAlgorithms {
			if !slices.Contains([]string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA"}, alg) {
				return fmt.Errorf("unsupported OIDC signing algorithm")
			}
		}
		fixed := p.AuthorizationURL != "" || p.TokenURL != "" || p.JWKSURL != "" || p.UserInfoURL != ""
		if fixed && (p.AuthorizationURL == "" || p.TokenURL == "" || p.JWKSURL == "") {
			return fmt.Errorf("incomplete fixed OIDC endpoints")
		}
		if !fixed && !p.allowedURL(strings.TrimSuffix(p.Issuer, "/")+"/.well-known/openid-configuration") {
			return fmt.Errorf("untrusted OIDC discovery URL")
		}
	} else if p.Protocol == "oauth2" {
		if p.Adapter != "userinfo-sub" || p.AuthorizationURL == "" || p.TokenURL == "" || p.UserInfoURL == "" || p.JWKSURL != "" {
			return fmt.Errorf("OAuth2 requires the userinfo-sub adapter and fixed endpoints")
		}
	} else {
		return fmt.Errorf("unsupported OAuth protocol")
	}
	for _, raw := range []string{p.AuthorizationURL, p.TokenURL, p.JWKSURL, p.UserInfoURL} {
		if raw != "" && !p.allowedURL(raw) {
			return fmt.Errorf("untrusted OAuth endpoint")
		}
	}
	return nil
}

// fingerprint binds browser contexts to authentication settings, excluding rotatable Cookie keys.
func (c Config) fingerprint() string {
	data, _ := json.Marshal(struct {
		PublicBaseURL string
		Provider      ProviderConfig
	}{c.PublicBaseURL, c.Provider})
	sum := sha256.Sum256(data)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func validOrigin(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && !strings.ContainsAny(raw, "\\\r\n")
}

func (p ProviderConfig) allowedURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Fragment == "" &&
		!strings.ContainsAny(raw, "\\\r\n") && slices.Contains(p.AllowedEndpointOrigins, u.Scheme+"://"+u.Host)
}

// returnTo only accepts routes owned by the frontend, never authentication pages.
func validReturnTo(raw string) bool {
	if raw == "" || strings.ContainsAny(raw, "\\\r\n\x00") || strings.Contains(raw, "%") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return false
	}
	switch u.Path {
	case "/", "/projects", "/operations", "/runners", "/settings", "/api-docs", "/admin", "/admin/users", "/admin/machineaccounts":
		return true
	}
	return strings.HasPrefix(u.Path, "/projects/") && dnsLabel.MatchString(strings.TrimPrefix(u.Path, "/projects/"))
}
