package gateway

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	jwtIssuer    = "ebs-gateway"
	jwtAudience  = "ebs-api"
	jwtTTL       = time.Hour
	jwtMaxTTL    = 24 * time.Hour
	jwtClockSkew = 30 * time.Second
)

type Identity struct {
	Subject string
	Runner  string
	Scopes  []string
	JTI     string
}

func (i Identity) IsSystem() bool { return i.hasScope("ebs:system") }
func (i Identity) IsUser() bool   { return i.hasScope("ebs:user") }
func (i Identity) IsRunner() bool { return i.hasScope("ebs:runner") }
func (i Identity) IsAdmin() bool  { return i.hasScope("ebs:admin") }
func (i Identity) hasScope(want string) bool {
	for _, scope := range i.Scopes {
		if scope == want {
			return true
		}
	}
	return false
}
func (i Identity) ScopeHeader() string { return strings.Join(i.Scopes, ",") }

type jwtClaims struct {
	Subject   string   `json:"sub"`
	Runner    string   `json:"runner,omitempty"`
	Scopes    []string `json:"scopes"`
	Issuer    string   `json:"iss"`
	Audience  string   `json:"aud"`
	IssuedAt  int64    `json:"iat"`
	NotBefore int64    `json:"nbf"`
	Exp       int64    `json:"exp"`
	JTI       string   `json:"jti"`
}

type tokenManager struct {
	key []byte
}

func newTokenManager(cfg Config) (*tokenManager, error) {
	data, err := os.ReadFile(cfg.JWTSecretFile)
	if err != nil {
		return nil, fmt.Errorf("read jwt secret file: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("decode jwt secret: %w", err)
	}
	if len(key) < 32 {
		return nil, errors.New("jwt secret must contain at least 32 bytes")
	}
	return &tokenManager{key: key}, nil
}

func (m *tokenManager) issueUser(subject string, now time.Time) (string, int64, error) {
	return m.issue(subject, "", []string{"ebs:user"}, now, jwtTTL)
}

func (m *tokenManager) issueAdmin(subject string, now time.Time) (string, int64, error) {
	return m.issue(subject, "", []string{"ebs:admin"}, now, jwtTTL)
}

func (m *tokenManager) issueRunner(runner string, now time.Time, ttl time.Duration) (string, int64, error) {
	if ttl < 5*time.Minute || ttl > jwtMaxTTL {
		return "", 0, errors.New("invalid runner token ttl")
	}
	return m.issue(runner, runner, []string{"ebs:runner"}, now, ttl)
}

func (m *tokenManager) issue(subject, runner string, scopes []string, now time.Time, ttl time.Duration) (string, int64, error) {
	jtiBytes := make([]byte, 16)
	if _, err := rand.Read(jtiBytes); err != nil {
		return "", 0, fmt.Errorf("generate jti: %w", err)
	}
	expires := now.Add(ttl).Unix()
	claims := jwtClaims{Subject: subject, Runner: runner, Scopes: scopes, Issuer: jwtIssuer, Audience: jwtAudience, IssuedAt: now.Unix(), NotBefore: now.Unix(), Exp: expires, JTI: hex.EncodeToString(jtiBytes)}
	token, err := m.sign(claims)
	return token, expires, err
}

func (m *tokenManager) sign(claims jwtClaims) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, m.key)
	_, _ = mac.Write([]byte(input))
	return input + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func authenticate(r *http.Request, manager *tokenManager, now time.Time) (Identity, error) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return Identity{}, errors.New("missing bearer token")
	}
	parts := strings.Fields(auth)
	if len(parts) != 2 || parts[0] != "Bearer" {
		return Identity{}, errors.New("invalid authorization header")
	}
	return manager.parse(parts[1], now)
}

func (m *tokenManager) parse(token string, now time.Time) (Identity, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Identity{}, errors.New("invalid jwt format")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Identity{}, errors.New("invalid jwt header")
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil || header.Alg != "HS256" || header.Typ != "JWT" {
		return Identity{}, errors.New("invalid jwt header")
	}
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Identity{}, errors.New("invalid jwt signature")
	}
	mac := hmac.New(sha256.New, m.key)
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(got, mac.Sum(nil)) {
		return Identity{}, errors.New("invalid jwt signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Identity{}, errors.New("invalid jwt claims")
	}
	var claims jwtClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Identity{}, errors.New("invalid jwt claims")
	}
	if err := m.validateClaims(claims, now); err != nil {
		return Identity{}, err
	}
	return Identity{Subject: claims.Subject, Runner: claims.Runner, Scopes: claims.Scopes, JTI: claims.JTI}, nil
}

func (m *tokenManager) validateClaims(c jwtClaims, now time.Time) error {
	if c.Subject == "" || c.JTI == "" || c.Issuer != jwtIssuer || c.Audience != jwtAudience {
		return errors.New("invalid jwt claims")
	}
	if c.IssuedAt == 0 || c.NotBefore == 0 || c.Exp == 0 || c.Exp <= c.IssuedAt || c.Exp <= c.NotBefore {
		return errors.New("invalid jwt time claims")
	}
	skew := int64(jwtClockSkew / time.Second)
	current := now.Unix()
	if c.IssuedAt > current+skew || c.NotBefore > current+skew || c.Exp <= current-skew || time.Duration(c.Exp-c.IssuedAt)*time.Second > jwtMaxTTL {
		return errors.New("invalid jwt time claims")
	}
	scopes := make(map[string]struct{}, len(c.Scopes))
	for _, scope := range c.Scopes {
		if scope == "" {
			return errors.New("invalid jwt scopes")
		}
		if _, duplicate := scopes[scope]; duplicate {
			return errors.New("invalid jwt scopes")
		}
		scopes[scope] = struct{}{}
	}
	_, user := scopes["ebs:user"]
	_, runner := scopes["ebs:runner"]
	_, system := scopes["ebs:system"]
	_, admin := scopes["ebs:admin"]
	if len(scopes) == 1 && user && c.Runner == "" {
		return nil
	}
	if len(scopes) == 1 && runner && c.Runner != "" && c.Runner == c.Subject {
		return nil
	}
	if len(scopes) == 1 && system && c.Runner == "" {
		return nil
	}
	if len(scopes) == 1 && admin && c.Runner == "" {
		return nil
	}
	return errors.New("invalid jwt scopes")
}
