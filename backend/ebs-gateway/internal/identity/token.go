package identity

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	iamv1 "ebs-api/iam/v1"
)

const (
	issuer    = "ebs-gateway"
	audience  = "ebs-api"
	maxAge    = 24 * time.Hour
	clockSkew = 30 * time.Second
)

type Scope string

const (
	UserScope  Scope = "ebs:user"
	OpsScope   Scope = "ebs:ops"
	AdminScope Scope = "ebs:admin"
)

type Type string

const (
	UserType    Type = "user"
	RunnerType  Type = "runner"
	ServiceType Type = "service"
)

type AuthenticationMethod string

const (
	AuthenticationJWT        AuthenticationMethod = "jwt"
	AuthenticationCredential AuthenticationMethod = "credential"
)

type Principal struct {
	AuthenticationMethod AuthenticationMethod
	CredentialID         string
	Type                 Type
	Subject              string
	Scope                Scope
	ID                   string
	ExpiresAt            time.Time
	// ServiceGrants comes from the current account lookup, never from JWT claims.
	ServiceGrants *ServiceGrants `json:"-"`
}

type ServiceGrants struct {
	Projects    []string
	Permissions []iamv1.MachineAccountPermission
}

func (p Principal) IsUser() bool {
	return p.Type == UserType
}

func (p Principal) IsPrivileged() bool {
	return p.Type == UserType && (p.Scope == OpsScope || p.Scope == AdminScope)
}

type claims struct {
	Type      Type    `json:"type"`
	Subject   string  `json:"sub"`
	Scopes    []Scope `json:"scopes"`
	Issuer    string  `json:"iss"`
	Audience  string  `json:"aud"`
	IssuedAt  int64   `json:"iat"`
	NotBefore int64   `json:"nbf"`
	ExpiresAt int64   `json:"exp"`
	ID        string  `json:"jti"`
}

type Tokens struct {
	secret []byte
}

func NewTokens(base64Secret string) (*Tokens, error) {
	secret, err := base64.StdEncoding.DecodeString(strings.TrimSpace(base64Secret))
	if err != nil || len(secret) < 32 {
		return nil, errors.New("JWT secret must be base64 and contain at least 32 bytes")
	}
	return &Tokens{secret: secret}, nil
}

func (t *Tokens) Issue(subject string, kind Type, scope Scope, ttl time.Duration, now time.Time) (string, error) {
	if subject == "" || ttl <= 0 || ttl > maxAge || !validIdentity(kind, scope) {
		return "", errors.New("invalid token identity or lifetime")
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate token ID: %w", err)
	}
	scopes := make([]Scope, 0, 1)
	if scope != "" {
		scopes = append(scopes, scope)
	}
	c := claims{
		Type:      kind,
		Subject:   subject,
		Scopes:    scopes,
		Issuer:    issuer,
		Audience:  audience,
		IssuedAt:  now.Unix(),
		NotBefore: now.Unix(),
		ExpiresAt: now.Add(ttl).Unix(),
		ID:        hex.EncodeToString(random),
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode token: %w", err)
	}
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(t.signature(unsigned)), nil
}

func (t *Tokens) Verify(token string, now time.Time) (Principal, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Principal{}, errors.New("invalid token")
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Principal{}, errors.New("invalid token header")
	}
	var fields struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
	}
	if err := json.Unmarshal(header, &fields); err != nil || fields.Algorithm != "HS256" || fields.Type != "JWT" {
		return Principal{}, errors.New("invalid token header")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(signature, t.signature(parts[0]+"."+parts[1])) {
		return Principal{}, errors.New("invalid token signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Principal{}, errors.New("invalid token claims")
	}
	var c claims
	if err := json.Unmarshal(payload, &c); err != nil || !validClaims(c, now) {
		return Principal{}, errors.New("invalid token claims")
	}
	principal := Principal{
		AuthenticationMethod: AuthenticationJWT,
		Type:                 c.Type,
		Subject:              c.Subject,
		ID:                   c.ID,
		ExpiresAt:            time.Unix(c.ExpiresAt, 0),
	}
	if len(c.Scopes) == 1 {
		principal.Scope = c.Scopes[0]
	}
	return principal, nil
}

// ParsePersonalToken validates the wire format, not the caller's identity.
func ParsePersonalToken(token string) (string, string, error) {
	invalid := errors.New("invalid personal credential")
	if len(token) > 512 {
		return "", "", invalid
	}
	parts := strings.Split(token, ".")
	if len(parts) != 4 || parts[0] != "ebs_pat_v1" {
		return "", "", invalid
	}
	username, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || base64.RawURLEncoding.EncodeToString(username) != parts[1] || len(validation.IsDNS1123Label(string(username))) != 0 {
		return "", "", invalid
	}
	id, err := hex.DecodeString(parts[2])
	if err != nil || len(id) != 16 || hex.EncodeToString(id) != parts[2] {
		return "", "", invalid
	}
	secret, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil || len(secret) != 32 || base64.RawURLEncoding.EncodeToString(secret) != parts[3] {
		return "", "", invalid
	}
	return string(username), parts[2], nil
}

func (t *Tokens) signature(message string) []byte {
	mac := hmac.New(sha256.New, t.secret)
	_, _ = mac.Write([]byte(message))
	return mac.Sum(nil)
}

func validClaims(c claims, now time.Time) bool {
	if c.Subject == "" || c.ID == "" || c.Issuer != issuer || c.Audience != audience || c.Scopes == nil || len(c.Scopes) > 1 {
		return false
	}
	var scope Scope
	if c.Type != UserType && len(c.Scopes) != 0 {
		return false
	}
	if len(c.Scopes) == 1 {
		scope = c.Scopes[0]
	}
	if !validIdentity(c.Type, scope) {
		return false
	}
	if c.IssuedAt <= 0 || c.NotBefore <= 0 || c.ExpiresAt <= c.IssuedAt || c.ExpiresAt <= c.NotBefore {
		return false
	}
	current := now.Unix()
	skew := int64(clockSkew / time.Second)
	return c.IssuedAt <= current+skew && c.NotBefore <= current+skew && c.ExpiresAt > current-skew && c.ExpiresAt-c.IssuedAt <= int64(maxAge/time.Second)
}

func validIdentity(kind Type, scope Scope) bool {
	switch kind {
	case ServiceType, RunnerType:
		return scope == ""
	case UserType:
		return scope == UserScope || scope == OpsScope || scope == AdminScope
	default:
		return false
	}
}
