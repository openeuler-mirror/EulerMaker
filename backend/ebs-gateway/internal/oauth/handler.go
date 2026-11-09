package oauth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/iam"
	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/mutation"
)

type accountClient interface {
	AuthenticateOAuth(context.Context, iam.ExternalIdentity) (iam.OAuthAccount, error)
	RegisterOAuth(context.Context, iam.ExternalIdentity, string, string, string) (iam.OAuthAccount, error)
	GetUser(context.Context, string) (iam.User, int, error)
}

type Handler struct {
	config   Config
	accounts accountClient
	tokens   *identity.Tokens
	now      func() time.Time
	provider *provider
	limits   rateLimits
}

func New(config Config, accounts accountClient, tokens *identity.Tokens, now func() time.Time) *Handler {
	if now == nil {
		now = time.Now
	}
	return &Handler{config: config, accounts: accounts, tokens: tokens, now: now,
		provider: newProvider(config, now, nil), limits: rateLimits{entries: make(map[string]rateEntry)}}
}

// Run warms discovery and retries temporary provider failures without blocking other routes.
func (h *Handler) Run(ctx context.Context) {
	if h.LocalAuthenticationEnabled() {
		return
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, _, _ = h.provider.ensure(attempt)
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (h *Handler) LocalAuthenticationEnabled() bool { return h.config.PublicBaseURL == "" }

type apiError struct {
	status int
	code   string
}

func (e *apiError) Error() string           { return e.code }
func failure(status int, code string) error { return &apiError{status, code} }

func respondError(c *gin.Context, err error) {
	status, code := 503, "OAuthUnavailable"
	var local *apiError
	var remote *iam.OAuthError
	if errors.As(err, &local) {
		status, code = local.status, local.code
	} else if errors.As(err, &remote) {
		status, code = remote.Status, remote.Code
	}
	c.AbortWithStatusJSON(status, gin.H{"code": code, "error": code})
}

func (h *Handler) Providers(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	providers := []gin.H{}
	if !h.LocalAuthenticationEnabled() {
		providers = append(providers, gin.H{"id": h.config.Provider.ID, "loginURL": "/auth/oauth/" + h.config.Provider.ID + "/login"})
	}
	c.JSON(200, gin.H{"passwordLoginEnabled": h.LocalAuthenticationEnabled(), "registrationEnabled": h.LocalAuthenticationEnabled(), "providers": providers})
}

func (h *Handler) begin(c *gin.Context, login bool) bool {
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	if h.LocalAuthenticationEnabled() || (c.Param("provider") != "" && c.Param("provider") != h.config.Provider.ID) {
		respondError(c, failure(404, "ProviderUnavailable"))
		return false
	}
	max := 30
	key := "request/" + c.ClientIP()
	if login {
		max = 10
		key = "login/" + c.ClientIP()
	}
	if !h.limits.allow(key, max, h.now()) {
		c.Header("Retry-After", "60")
		respondError(c, failure(429, "RateLimited"))
		return false
	}
	if !login {
		origins := c.Request.Header.Values("Origin")
		if len(origins) != 1 || origins[0] != h.config.PublicBaseURL {
			respondError(c, failure(403, "BrowserBindingInvalid"))
			return false
		}
	}
	return true
}

func readInput(c *gin.Context, allowed ...string) (map[string]string, error) {
	defer c.Request.Body.Close()
	if strings.TrimSpace(strings.Split(c.GetHeader("Content-Type"), ";")[0]) != "application/json" {
		return nil, failure(400, "InvalidRequest")
	}
	data, err := io.ReadAll(io.LimitReader(c.Request.Body, (16<<10)+1))
	if err != nil || len(data) > 16<<10 || !utf8.Valid(data) {
		return nil, failure(400, "InvalidRequest")
	}
	object, err := mutation.ParseObject(data)
	if err != nil {
		return nil, failure(400, "InvalidRequest")
	}
	result := map[string]string{}
	for key, value := range object {
		found := false
		for _, name := range allowed {
			if name == key {
				found = true
				break
			}
		}
		text, ok := value.(string)
		if !found || !ok {
			return nil, failure(400, "InvalidRequest")
		}
		result[key] = text
	}
	return result, nil
}

func (h *Handler) Login(c *gin.Context) {
	if !h.begin(c, true) {
		return
	}
	query, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil || len(query) > 1 || len(query["returnTo"]) > 1 {
		respondError(c, failure(400, "InvalidRequest"))
		return
	}
	for key := range query {
		if key != "returnTo" {
			respondError(c, failure(400, "InvalidRequest"))
			return
		}
	}
	returnTo := "/"
	if values, ok := query["returnTo"]; ok {
		returnTo = values[0]
	}
	if !validReturnTo(returnTo) {
		respondError(c, failure(400, "InvalidRequest"))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	endpoints, _, err := h.provider.ensure(ctx)
	if err != nil {
		respondError(c, err)
		return
	}
	state, nonce, verifier := h.config.Provider.ID+"."+randomValue(), randomValue(), randomValue()
	value := browserContext{Purpose: "login", Provider: h.config.Provider.ID, ConfigFingerprint: h.config.fingerprint(),
		IssuedAt: h.now().Unix(), ExpiresAt: h.now().Add(contextTTL).Unix(), ReturnTo: returnTo,
		StateHash: digest(state), NonceHash: digest(nonce), Verifier: verifier}
	cookie, err := h.encodeCookie(value)
	if err != nil {
		respondError(c, err)
		return
	}
	endpoint, _ := url.Parse(endpoints.AuthorizationURL)
	params := endpoint.Query()
	params.Set("response_type", "code")
	params.Set("response_mode", "query")
	params.Set("client_id", endpoints.ClientID)
	params.Set("redirect_uri", h.config.PublicBaseURL+"/auth/callback")
	params.Set("scope", strings.Join(endpoints.Scopes, " "))
	params.Set("state", state)
	params.Set("code_challenge_method", "S256")
	params.Set("code_challenge", digest(verifier))
	if endpoints.Protocol == "oidc" {
		params.Set("nonce", nonce)
	}
	endpoint.RawQuery = params.Encode()
	http.SetCookie(c.Writer, cookie)
	clearCookie(c.Writer, "register")
	c.Redirect(http.StatusFound, endpoint.String())
}

func (h *Handler) Callback(c *gin.Context) {
	if !h.begin(c, false) {
		return
	}
	input, err := readInput(c, "code", "state", "error", "iss")
	if err != nil {
		respondError(c, err)
		return
	}
	_, hasCode := input["code"]
	_, hasError := input["error"]
	if input["state"] == "" || hasCode == hasError || (hasCode && input["code"] == "") || (hasError && input["error"] == "") {
		respondError(c, failure(400, "InvalidRequest"))
		return
	}
	state, err := h.readCookie(c.Request, "login")
	if err != nil {
		respondError(c, err)
		return
	}
	if !matches(input["state"], state.StateHash) {
		respondError(c, failure(403, "BrowserBindingInvalid"))
		return
	}
	clearCookie(c.Writer, "login")
	if issuer, exists := input["iss"]; exists && issuer != h.config.Provider.Issuer {
		respondError(c, failure(401, "OAuthAuthenticationFailed"))
		return
	}
	if hasError {
		if input["error"] == "access_denied" {
			respondError(c, failure(400, "OAuthCancelled"))
		} else {
			respondError(c, failure(401, "OAuthAuthenticationFailed"))
		}
		return
	}
	ctx, cancel := requestContext(c, state)
	defer cancel()
	external, profile, err := h.provider.exchange(ctx, input["code"], state, input["iss"])
	if err != nil {
		respondError(c, err)
		return
	}
	account, err := h.accounts.AuthenticateOAuth(ctx, external)
	if err != nil {
		respondError(c, err)
		return
	}
	if account.Authenticated {
		h.loginAccount(c, ctx, state, account)
		return
	}
	if !account.RegistrationRequired {
		respondError(c, failure(503, "OAuthUnavailable"))
		return
	}
	state.Purpose, state.RegistrationID, state.Identity, state.Profile = "register", randomValue(), &external, profile
	state.StateHash, state.NonceHash, state.Verifier = "", "", ""
	cookie, err := h.encodeCookie(state)
	if err != nil {
		respondError(c, err)
		return
	}
	http.SetCookie(c.Writer, cookie)
	c.JSON(200, gin.H{"registrationRequired": true, "registrationID": state.RegistrationID, "expiresAt": time.Unix(state.ExpiresAt, 0).UTC(), "profile": state.Profile})
}

func requestContext(c *gin.Context, state browserContext) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(30 * time.Second)
	if expiry := time.Unix(state.ExpiresAt, 0); expiry.Before(deadline) {
		deadline = expiry
	}
	return context.WithDeadline(c.Request.Context(), deadline)
}

func (h *Handler) registration(c *gin.Context, id string) (browserContext, error) {
	state, err := h.readCookie(c.Request, "register")
	if err != nil {
		return state, err
	}
	if id == "" || !matches(id, digest(state.RegistrationID)) {
		return state, failure(403, "BrowserBindingInvalid")
	}
	if !h.limits.allow("registration/"+id, 10, h.now()) {
		c.Header("Retry-After", "60")
		return state, failure(429, "RateLimited")
	}
	return state, nil
}

func (h *Handler) Register(c *gin.Context) {
	if !h.begin(c, false) {
		return
	}
	input, err := readInput(c, "registrationID", "action", "username", "displayName", "email")
	if err != nil {
		respondError(c, err)
		return
	}
	if !dnsLabel.MatchString(input["username"]) || (input["action"] != "create" && input["action"] != "confirm") {
		respondError(c, failure(400, "InvalidRequest"))
		return
	}
	if input["action"] == "confirm" {
		for _, key := range []string{"displayName", "email"} {
			if _, exists := input[key]; exists {
				respondError(c, failure(400, "InvalidRequest"))
				return
			}
		}
	}
	if input["email"] != "" {
		address, err := mail.ParseAddress(input["email"])
		if err != nil || address.Address != input["email"] {
			respondError(c, failure(400, "InvalidRequest"))
			return
		}
	}
	state, err := h.registration(c, input["registrationID"])
	if err != nil {
		respondError(c, err)
		return
	}
	ctx, cancel := requestContext(c, state)
	defer cancel()
	var created iam.OAuthAccount
	if input["action"] == "create" {
		created, err = h.accounts.RegisterOAuth(ctx, *state.Identity, input["username"], input["displayName"], input["email"])
		if err != nil {
			respondError(c, err)
			return
		}
		if created.Username != input["username"] || created.UID == "" {
			respondError(c, failure(503, "OAuthUnavailable"))
			return
		}
	}
	account, err := h.accounts.AuthenticateOAuth(ctx, *state.Identity)
	if err != nil {
		respondError(c, err)
		return
	}
	if account.RegistrationRequired {
		respondError(c, failure(409, "RegistrationInProgress"))
		return
	}
	if !account.Authenticated {
		respondError(c, failure(503, "OAuthUnavailable"))
		return
	}
	if account.Username != input["username"] {
		respondError(c, failure(409, "IdentityAlreadyLinked"))
		return
	}
	if created.UID != "" && account.UID != created.UID {
		respondError(c, failure(500, "IdentityStateInvalid"))
		return
	}
	h.loginAccount(c, ctx, state, account)
}

func (h *Handler) loginAccount(c *gin.Context, ctx context.Context, state browserContext, account iam.OAuthAccount) {
	user, status, err := h.accounts.GetUser(ctx, account.Username)
	if err != nil || status >= 500 {
		respondError(c, failure(503, "OAuthUnavailable"))
		return
	}
	if status != 200 || account.UID == "" || user.UID != account.UID || user.Name != account.Username || !user.Enabled || len(user.Scopes) != 1 {
		respondError(c, failure(401, "AuthenticationFailed"))
		return
	}
	scope := user.Scopes[0]
	if scope != identity.UserScope && scope != identity.OpsScope && scope != identity.AdminScope {
		respondError(c, failure(401, "AuthenticationFailed"))
		return
	}
	if !h.now().Before(time.Unix(state.ExpiresAt, 0)) {
		respondError(c, failure(410, "ContextExpired"))
		return
	}
	if ctx.Err() != nil {
		respondError(c, failure(503, "OAuthUnavailable"))
		return
	}
	token, err := h.tokens.Issue(user.Name, "", identity.UserType, scope, 24*time.Hour, h.now())
	if err != nil {
		respondError(c, err)
		return
	}
	clearCookie(c.Writer, "register")
	c.JSON(200, gin.H{"token": token, "tokenType": "Bearer", "expiresIn": 86400, "returnTo": state.ReturnTo})
}

type rateEntry struct {
	started time.Time
	count   int
}
type rateLimits struct {
	mu      sync.Mutex
	entries map[string]rateEntry
}

func (l *rateLimits) allow(key string, max int, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.entries[key]
	if now.Sub(entry.started) >= time.Minute {
		entry = rateEntry{started: now}
	}
	if entry.count >= max {
		return false
	}
	if _, exists := l.entries[key]; !exists && len(l.entries) >= 4096 {
		for key, old := range l.entries {
			if now.Sub(old.started) >= time.Minute {
				delete(l.entries, key)
			}
		}
		if len(l.entries) >= 4096 {
			return false
		}
	}
	entry.count++
	l.entries[key] = entry
	return true
}
