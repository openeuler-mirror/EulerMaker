package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Identity struct {
	Subject string
	Tenant  string
	Scopes  []string
}

func (i Identity) IsSystem() bool {
	for _, scope := range i.Scopes {
		if scope == "ebs:system" {
			return true
		}
	}
	return false
}

func (i Identity) ScopeHeader() string {
	return strings.Join(i.Scopes, ",")
}

type jwtClaims struct {
	Subject string   `json:"sub"`
	Tenant  string   `json:"tenant"`
	Scopes  []string `json:"scopes"`
	Exp     int64    `json:"exp"`
}

func authenticate(r *http.Request, secret string, now time.Time) (Identity, error) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return Identity{}, errors.New("missing bearer token")
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return Identity{}, errors.New("invalid authorization header")
	}
	return parseJWT(strings.TrimSpace(strings.TrimPrefix(auth, prefix)), secret, now)
}

func parseJWT(token, secret string, now time.Time) (Identity, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Identity{}, errors.New("invalid jwt format")
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Identity{}, fmt.Errorf("decode jwt header: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return Identity{}, fmt.Errorf("parse jwt header: %w", err)
	}
	if header.Alg != "HS256" {
		return Identity{}, fmt.Errorf("unsupported jwt alg %q", header.Alg)
	}

	signingInput := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(signingInput))
	wantSig := mac.Sum(nil)
	gotSig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Identity{}, fmt.Errorf("decode jwt signature: %w", err)
	}
	if !hmac.Equal(gotSig, wantSig) {
		return Identity{}, errors.New("invalid jwt signature")
	}

	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Identity{}, fmt.Errorf("decode jwt claims: %w", err)
	}
	claims, err := decodeClaims(claimsBytes)
	if err != nil {
		return Identity{}, err
	}
	if claims.Exp == 0 || now.Unix() >= claims.Exp {
		return Identity{}, errors.New("jwt expired")
	}
	if claims.Subject == "" {
		return Identity{}, errors.New("jwt sub is required")
	}
	if claims.Tenant == "" {
		return Identity{}, errors.New("jwt tenant is required")
	}
	return Identity{Subject: claims.Subject, Tenant: claims.Tenant, Scopes: claims.Scopes}, nil
}

func decodeClaims(data []byte) (jwtClaims, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return jwtClaims{}, fmt.Errorf("parse jwt claims: %w", err)
	}

	claims := jwtClaims{}
	if v, ok := raw["sub"].(string); ok {
		claims.Subject = v
	}
	if v, ok := raw["tenant"].(string); ok {
		claims.Tenant = v
	}
	switch exp := raw["exp"].(type) {
	case float64:
		claims.Exp = int64(exp)
	case string:
		parsed, err := strconv.ParseInt(exp, 10, 64)
		if err != nil {
			return jwtClaims{}, fmt.Errorf("parse exp: %w", err)
		}
		claims.Exp = parsed
	}
	if scopes, ok := raw["scopes"].([]any); ok {
		for _, item := range scopes {
			if scope, ok := item.(string); ok {
				claims.Scopes = append(claims.Scopes, scope)
			}
		}
	}
	return claims, nil
}
