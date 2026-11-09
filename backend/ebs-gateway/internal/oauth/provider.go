package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coreos/go-oidc/v3/oidc"

	"ebs-gateway/internal/iam"
	"ebs-gateway/internal/mutation"
)

type provider struct {
	config      Config
	client      *http.Client
	now         func() time.Time
	mu          sync.Mutex
	lastAttempt time.Time
	ready       bool
	endpoints   ProviderConfig
	verifier    *oidc.IDTokenVerifier
}

// guardedTransport enforces trusted origins and response limits, including discovery and JWKS calls.
type guardedTransport struct {
	base   http.RoundTripper
	config ProviderConfig
}

func (t guardedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.config.allowedURL(req.URL.String()) {
		return nil, fmt.Errorf("untrusted OAuth endpoint")
	}
	response, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, fmt.Errorf("invalid OAuth response size")
	}
	response.Body = io.NopCloser(bytes.NewReader(data))
	return response, nil
}

func newProvider(config Config, now func() time.Time, transport http.RoundTripper) *provider {
	if transport == nil {
		transport = http.DefaultTransport.(*http.Transport).Clone()
	}
	return &provider{config: config, now: now, client: &http.Client{
		Timeout: 10 * time.Second, Transport: guardedTransport{transport, config.Provider},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (p *provider) ensure(ctx context.Context) (ProviderConfig, *oidc.IDTokenVerifier, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ready {
		return p.endpoints, p.verifier, nil
	}
	if !p.lastAttempt.IsZero() && p.now().Sub(p.lastAttempt) < 30*time.Second {
		return ProviderConfig{}, nil, failure(503, "OAuthUnavailable")
	}
	p.lastAttempt = p.now()
	endpoints := p.config.Provider
	if endpoints.Protocol == "oidc" && endpoints.AuthorizationURL == "" {
		var discovery struct {
			Issuer           string   `json:"issuer"`
			AuthorizationURL string   `json:"authorization_endpoint"`
			TokenURL         string   `json:"token_endpoint"`
			JWKSURL          string   `json:"jwks_uri"`
			UserInfoURL      string   `json:"userinfo_endpoint"`
			PKCE             []string `json:"code_challenge_methods_supported"`
			Algorithms       []string `json:"id_token_signing_alg_values_supported"`
			AuthMethods      []string `json:"token_endpoint_auth_methods_supported"`
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(endpoints.Issuer, "/")+"/.well-known/openid-configuration", nil)
		if err := p.request(req, &discovery); err != nil {
			return ProviderConfig{}, nil, failure(503, "OAuthUnavailable")
		}
		if discovery.Issuer != endpoints.Issuer || !slices.Contains(discovery.PKCE, "S256") ||
			(len(discovery.AuthMethods) > 0 && !slices.Contains(discovery.AuthMethods, endpoints.TokenEndpointAuthMethod)) {
			return ProviderConfig{}, nil, failure(503, "OAuthUnavailable")
		}
		for _, alg := range endpoints.SigningAlgorithms {
			if !slices.Contains(discovery.Algorithms, alg) {
				return ProviderConfig{}, nil, failure(503, "OAuthUnavailable")
			}
		}
		endpoints.AuthorizationURL, endpoints.TokenURL, endpoints.JWKSURL, endpoints.UserInfoURL = discovery.AuthorizationURL, discovery.TokenURL, discovery.JWKSURL, discovery.UserInfoURL
	}
	for _, raw := range []string{endpoints.AuthorizationURL, endpoints.TokenURL} {
		if !endpoints.allowedURL(raw) {
			return ProviderConfig{}, nil, failure(503, "OAuthUnavailable")
		}
	}
	if endpoints.UserInfoURL != "" && !endpoints.allowedURL(endpoints.UserInfoURL) {
		return ProviderConfig{}, nil, failure(503, "OAuthUnavailable")
	}
	if endpoints.Protocol == "oidc" {
		if !endpoints.allowedURL(endpoints.JWKSURL) {
			return ProviderConfig{}, nil, failure(503, "OAuthUnavailable")
		}
		jwksClient := *p.client
		jwksClient.Transport = &keyTransport{base: p.client.Transport, now: p.now}
		keys := oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), &jwksClient), endpoints.JWKSURL)
		p.verifier = oidc.NewVerifier(endpoints.Issuer, keys, &oidc.Config{
			ClientID: endpoints.ClientID, SupportedSigningAlgs: endpoints.SigningAlgorithms,
			// Check expiration, nbf and iat together below with a uniform 60-second tolerance.
			SkipExpiryCheck: true,
		})
	}
	p.endpoints, p.ready = endpoints, true
	return p.endpoints, p.verifier, nil
}

// keyTransport merges and limits JWKS refreshes even for repeated unknown key IDs.
type keyTransport struct {
	base http.RoundTripper
	now  func() time.Time
	mu   sync.Mutex
	last time.Time
	data []byte
	err  error
}

func (t *keyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last.IsZero() || t.now().Sub(t.last) >= 30*time.Second {
		t.last = t.now()
		response, err := t.base.RoundTrip(req)
		t.data, t.err = nil, err
		if err == nil {
			t.data, t.err = io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != 200 {
				t.err = fmt.Errorf("JWKS unavailable")
			}
		}
	}
	if t.err != nil {
		return nil, t.err
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(t.data)), Request: req}, nil
}

func (p *provider) request(req *http.Request, output any) error {
	response, err := p.client.Do(req)
	if err != nil {
		return failure(503, "OAuthUnavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode >= 500 || response.StatusCode == 429 {
			return failure(503, "OAuthUnavailable")
		}
		return failure(401, "OAuthAuthenticationFailed")
	}
	data, err := io.ReadAll(response.Body)
	if err != nil || !utf8.Valid(data) {
		return failure(503, "OAuthUnavailable")
	}
	if _, err := mutation.ParseObject(data); err != nil {
		return failure(401, "OAuthAuthenticationFailed")
	}
	if json.Unmarshal(data, output) != nil {
		return failure(401, "OAuthAuthenticationFailed")
	}
	return nil
}

type claims struct {
	Subject         string `json:"sub"`
	Name            string `json:"name"`
	Email           string `json:"email"`
	EmailVerified   bool   `json:"email_verified"`
	AuthorizedParty string `json:"azp"`
	IssuedAt        int64  `json:"iat"`
	NotBefore       int64  `json:"nbf"`
}

func (p *provider) exchange(ctx context.Context, code string, state browserContext, responseIssuer string) (iam.ExternalIdentity, profile, error) {
	var identity iam.ExternalIdentity
	var info claims
	e, verifier, err := p.ensure(ctx)
	if err != nil {
		return identity, profile{}, err
	}
	if responseIssuer != "" && responseIssuer != e.Issuer {
		return identity, profile{}, failure(401, "OAuthAuthenticationFailed")
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {p.config.PublicBaseURL + "/auth/callback"}, "code_verifier": {state.Verifier}}
	if e.TokenEndpointAuthMethod == "client_secret_post" {
		form.Set("client_id", e.ClientID)
		form.Set("client_secret", p.config.Provider.ClientSecret)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.TokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if e.TokenEndpointAuthMethod == "client_secret_basic" {
		req.SetBasicAuth(url.QueryEscape(e.ClientID), url.QueryEscape(p.config.Provider.ClientSecret))
	}
	var token struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
		TokenType   string `json:"token_type"`
		Error       string `json:"error"`
	}
	if err := p.request(req, &token); err != nil {
		return identity, profile{}, err
	}
	if token.Error != "" || token.AccessToken == "" || !strings.EqualFold(token.TokenType, "Bearer") {
		return identity, profile{}, failure(401, "OAuthAuthenticationFailed")
	}
	if e.Protocol == "oidc" {
		id, err := verifier.Verify(ctx, token.IDToken)
		if err != nil {
			return identity, profile{}, failure(401, "OAuthAuthenticationFailed")
		}
		if id.Claims(&info) != nil || !matches(id.Nonce, state.NonceHash) || id.Expiry.IsZero() || p.now().After(id.Expiry.Add(time.Minute)) ||
			info.IssuedAt <= 0 || info.IssuedAt > p.now().Add(time.Minute).Unix() || info.NotBefore > p.now().Add(time.Minute).Unix() ||
			(info.AuthorizedParty != "" && info.AuthorizedParty != e.ClientID) || (len(id.Audience) > 1 && info.AuthorizedParty != e.ClientID) {
			return identity, profile{}, failure(401, "OAuthAuthenticationFailed")
		}
		if id.AccessTokenHash != "" && id.VerifyAccessToken(token.AccessToken) != nil {
			return identity, profile{}, failure(401, "OAuthAuthenticationFailed")
		}
	}
	if e.UserInfoURL != "" {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.UserInfoURL, nil)
		req.Header.Set("Authorization", "Bearer "+token.AccessToken)
		var user claims
		if err := p.request(req, &user); err != nil {
			return identity, profile{}, err
		}
		if e.Protocol == "oidc" && user.Subject != info.Subject {
			return identity, profile{}, failure(401, "OAuthAuthenticationFailed")
		}
		info = user
	}
	identity = iam.ExternalIdentity{Issuer: e.Issuer, Subject: info.Subject}
	if !validIdentity(identity) {
		return identity, profile{}, failure(401, "OAuthAuthenticationFailed")
	}
	result := profile{DisplayName: info.Name}
	if info.EmailVerified {
		result.Email = info.Email
	}
	return identity, result, nil
}

func validIdentity(identity iam.ExternalIdentity) bool {
	valid := func(value string, max int) bool {
		return value != "" && len(value) <= max && utf8.ValidString(value) && !strings.ContainsFunc(value, unicode.IsControl)
	}
	return valid(identity.Issuer, 2048) && valid(identity.Subject, 1024)
}
