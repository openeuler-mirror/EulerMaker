//go:build ignore

package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/mail"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	jsonpatch "github.com/evanphx/json-patch"
)

const (
	apiPrefix           = "/apis/ebs/v1"
	ownerUserLabel      = "ebs.io/owner-user"
	memberUserLabelBase = "ebs.io/member-user."
	projectTypeLabel    = "project.ebs.io/type"
)

type Gateway struct {
	cfg                 Config
	upstream            *url.URL
	client              *http.Client
	proxy               *httputil.ReverseProxy
	limiter             *RateLimiter
	publicLimiter       *RateLimiter
	registerIPLimiter   *RateLimiter
	registerUserLimiter *RateLimiter
	watchMu             sync.Mutex
	activeRunnerWatches map[string]int
	tokens              *tokenManager
	now                 func() time.Time
	transport           http.RoundTripper
}

func NewServer(cfg Config) (*http.Server, error) {
	gw, err := NewGateway(cfg)
	if err != nil {
		return nil, err
	}
	return &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.Port),
		Handler:           gw,
		ReadHeaderTimeout: 10 * time.Second,
	}, nil
}

func NewGateway(cfg Config) (*Gateway, error) {
	upstream, err := url.Parse(cfg.APIServerAddr)
	if err != nil {
		return nil, fmt.Errorf("parse apiserver address: %w", err)
	}
	if upstream.Scheme == "" || upstream.Host == "" {
		return nil, fmt.Errorf("apiserver address must include scheme and host")
	}
	transport, err := cfg.HTTPTransport()
	if err != nil {
		return nil, err
	}
	tokens, err := newTokenManager(cfg)
	if err != nil {
		return nil, err
	}

	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.Transport = transport
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}
	proxy.ModifyResponse = func(response *http.Response) error {
		if response.Request != nil && isMarkedPublicRead(response.Request) {
			sanitizePublicResponseHeaders(response.Header)
		}
		return nil
	}

	gw := &Gateway{
		cfg:                 cfg,
		upstream:            upstream,
		client:              &http.Client{Transport: transport},
		proxy:               proxy,
		limiter:             NewRateLimiter(cfg.RateLimitPerSec, cfg.RateLimitBurst),
		publicLimiter:       NewRateLimiter(cfg.PublicRateLimitPerSec, cfg.PublicRateLimitBurst),
		registerIPLimiter:   NewRateLimiter(5.0/60.0, 5),
		registerUserLimiter: NewRateLimiter(3.0/60.0, 3),
		activeRunnerWatches: make(map[string]int),
		tokens:              tokens,
		now:                 time.Now,
		transport:           transport,
	}
	return gw, nil
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := g.now()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	var ident Identity

	defer func() {
		log.Printf(
			"method=%s path=%s query=%q status=%d latency_ms=%d client_ip=%s user=%s user_agent=%q",
			r.Method,
			r.URL.Path,
			r.URL.RawQuery,
			rec.status,
			g.now().Sub(start).Milliseconds(),
			clientIP(r),
			ident.Subject,
			r.UserAgent(),
		)
	}()

	if r.URL.Path == "/healthz" {
		rec.WriteHeader(http.StatusOK)
		_, _ = rec.Write([]byte("ok\n"))
		return
	}
	if r.URL.Path == "/auth/login" {
		g.handleLogin(rec, r)
		return
	}
	if r.URL.Path == "/auth/register" {
		g.handleRegister(rec, r)
		return
	}
	if r.URL.Path == "/auth/runner-token" {
		g.handleRunnerToken(rec, r)
		return
	}
	if r.URL.Path == "/auth/check" {
		g.handleTokenCheck(rec, r)
		return
	}
	isPasswordRoute := strings.HasPrefix(r.URL.Path, "/auth/users/") && strings.HasSuffix(r.URL.Path, "/password")
	isMachineCreate := r.URL.Path == "/auth/machineaccounts"
	isIAMRoute := strings.HasPrefix(r.URL.Path, "/apis/iam.ebs/v1/")
	isBusinessRoute := strings.HasPrefix(r.URL.Path, apiPrefix+"/") || r.URL.Path == apiPrefix
	if !isPasswordRoute && !isMachineCreate && !isIAMRoute && !isBusinessRoute {
		http.NotFound(rec, r)
		return
	}
	if isBusinessRoute && isInternalGlobalAPIPath(r.URL.Path) {
		http.NotFound(rec, r)
		return
	}
	if isBusinessRoute && !hasAuthorizationHeader(r) {
		if !isPublicReadRoute(r) {
			http.Error(rec, "unauthorized", http.StatusUnauthorized)
			return
		}
		ident.Subject = "anonymous"
		if !g.publicLimiter.Allow(clientIP(r)) {
			rec.Header().Set("Retry-After", "1")
			http.Error(rec, "too many requests", http.StatusTooManyRequests)
			return
		}
		if name, ok := configReadName(r); ok {
			g.serveConfigRead(rec, r, ident, name)
			return
		}
		g.servePublicRead(rec, r)
		return
	}

	authIdent, err := authenticate(r, g.tokens, g.now())
	if err != nil {
		http.Error(rec, "unauthorized", http.StatusUnauthorized)
		return
	}
	ident = authIdent
	if ident.IsUser() {
		if resolveErr := g.resolveUser(r.Context(), ident.Subject); resolveErr != nil {
			http.Error(rec, resolveErr.message, resolveErr.status)
			return
		}
	}
	if ident.IsAdmin() {
		if resolveErr := g.resolveAdmin(r.Context(), ident.Subject); resolveErr != nil {
			http.Error(rec, resolveErr.message, resolveErr.status)
			return
		}
	}
	if ident.IsOps() {
		if resolveErr := g.resolveOps(r.Context(), ident.Subject); resolveErr != nil {
			http.Error(rec, resolveErr.message, resolveErr.status)
			return
		}
	}

	limitKey := ident.Subject + "/" + clientIP(r)
	if !g.limiter.Allow(limitKey) {
		rec.Header().Set("Retry-After", "1")
		http.Error(rec, "too many requests", http.StatusTooManyRequests)
		return
	}
	if isBusinessRoute && isPublicReadRoute(r) {
		if name, ok := configReadName(r); ok {
			g.serveConfigRead(rec, r, ident, name)
			return
		}
		g.servePublicRead(rec, r)
		return
	}
	if isPasswordRoute {
		g.handlePasswordChange(rec, r, ident)
		return
	}
	if isMachineCreate {
		g.handleMachineAccountCreate(rec, r, ident)
		return
	}
	if isIAMRoute {
		g.handleIAMAPI(rec, r, ident)
		return
	}

	decision, err := g.authorizeAndPrepare(r.Context(), r, ident)
	if err != nil {
		http.Error(rec, err.Error(), http.StatusForbidden)
		return
	}
	injectIdentityHeaders(r, ident)
	if decision.handle != nil {
		decision.handle(rec, r)
		return
	}

	g.proxy.ServeHTTP(rec, r)
}

func (g *Gateway) handleTokenCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1))
	if err != nil || len(body) != 0 {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	ident, err := authenticate(r, g.tokens, g.now())
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !g.limiter.Allow(ident.Subject + "/" + clientIP(r)) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	identityType := "service"
	name := ident.Subject
	if ident.IsRunner() {
		identityType = "runner"
		name = ident.Runner
	} else if ident.IsUser() {
		identityType = "user"
	} else if ident.IsAdmin() {
		identityType = "admin"
	} else if ident.IsOps() {
		identityType = "ops"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"authenticated": true,
		"identity":      map[string]any{"type": identityType, "name": name, "scopes": ident.Scopes},
		"expiresAt":     ident.ExpiresAt,
	})
}

type gatewayHTTPError struct {
	status  int
	message string
}

func (g *Gateway) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if contentType := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]); contentType != "application/json" {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, g.cfg.MaxRequestBodyBytes)
	var input struct {
		Username    string `json:"username"`
		Password    string `json:"password"`
		DisplayName string `json:"displayName"`
		Email       string `json:"email"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid registration request", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF || !validRegistrationInput(input.Username, input.Password, input.Email) {
		http.Error(w, "invalid registration request", http.StatusBadRequest)
		return
	}
	ip := clientIP(r)
	if !g.registerIPLimiter.Allow(ip) || !g.registerUserLimiter.Allow(input.Username+"/"+ip) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	payload, err := json.Marshal(input)
	if err != nil {
		http.Error(w, "invalid registration request", http.StatusBadRequest)
		return
	}
	body, status, headers, err := g.upstreamRequest(r.Context(), http.MethodPost, "/internal/iam/v1/users/register", bytes.NewReader(payload), http.Header{"Content-Type": []string{"application/json"}})
	if err != nil || status >= 500 {
		http.Error(w, "registration service unavailable", http.StatusServiceUnavailable)
		return
	}
	if status == http.StatusConflict {
		http.Error(w, "username already exists", http.StatusConflict)
		return
	}
	if status < 200 || status >= 300 {
		http.Error(w, "invalid registration request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	for _, value := range headers.Values("Cache-Control") {
		w.Header().Add("Cache-Control", value)
	}
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(body)
}

func validRegistrationInput(username, password, email string) bool {
	if !isDNS1123Label(username) {
		return false
	}
	if n := utf8.RuneCountInString(password); n < 12 || n > 128 {
		return false
	}
	if email != "" {
		address, err := mail.ParseAddress(email)
		if err != nil || address.Address != email {
			return false
		}
	}
	return true
}

func (g *Gateway) handleRunnerToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	clientID, clientSecret, ok := r.BasicAuth()
	if !ok || !isDNS1123Label(clientID) || clientSecret == "" || len(clientSecret) > 256 {
		w.Header().Set("WWW-Authenticate", `Basic realm="runner-token"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var input struct {
		Runner string `json:"runner"`
	}
	if status, err := g.decodeJSONRequest(w, r, &input); err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	if !isDNS1123Label(input.Runner) {
		http.Error(w, "invalid runner token request", http.StatusBadRequest)
		return
	}
	key := "runner-token/" + clientID + "/" + clientIP(r)
	if !g.limiter.Allow(key) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	payload, _ := json.Marshal(map[string]string{"clientSecret": clientSecret})
	body, status, _, err := g.upstreamRequest(r.Context(), http.MethodPost, "/internal/iam/v1/machineaccounts/"+url.PathEscape(clientID)+"/authenticate", bytes.NewReader(payload), http.Header{"Content-Type": []string{"application/json"}})
	if err != nil || status >= 500 {
		http.Error(w, "authentication service unavailable", http.StatusServiceUnavailable)
		return
	}
	if status < 200 || status >= 300 {
		w.Header().Set("WWW-Authenticate", `Basic realm="runner-token"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var result struct {
		Authenticated   bool   `json:"authenticated"`
		Name            string `json:"name"`
		TokenTTLSeconds int64  `json:"tokenTTLSeconds"`
	}
	if json.Unmarshal(body, &result) != nil || !result.Authenticated || result.Name != clientID || result.TokenTTLSeconds < 300 || result.TokenTTLSeconds > 86400 {
		http.Error(w, "authentication service unavailable", http.StatusServiceUnavailable)
		return
	}
	issuedAt := g.now()
	token, expiresAt, err := g.tokens.issueRunner(input.Runner, issuedAt, time.Duration(result.TokenTTLSeconds)*time.Second)
	if err != nil {
		http.Error(w, "unable to issue token", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": token, "tokenType": "Bearer", "expiresIn": expiresAt - issuedAt.Unix()})
}

func (g *Gateway) handleMachineAccountCreate(w http.ResponseWriter, r *http.Request, ident Identity) {
	if !ident.IsAdmin() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var input struct {
		Name            string `json:"name"`
		ClientSecret    string `json:"clientSecret"`
		TokenTTLSeconds int64  `json:"tokenTTLSeconds"`
	}
	if status, err := g.decodeJSONRequest(w, r, &input); err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	if input.TokenTTLSeconds == 0 {
		input.TokenTTLSeconds = 3600
	}
	if !isDNS1123Label(input.Name) || !validClientSecret(input.ClientSecret) || input.TokenTTLSeconds < 300 || input.TokenTTLSeconds > 86400 {
		http.Error(w, "invalid machine account request", http.StatusBadRequest)
		return
	}
	payload, _ := json.Marshal(input)
	_, status, _, err := g.upstreamRequest(r.Context(), http.MethodPost, "/internal/iam/v1/machineaccounts/register", bytes.NewReader(payload), http.Header{"Content-Type": []string{"application/json"}})
	if err != nil || status >= 500 {
		http.Error(w, "registration service unavailable", http.StatusServiceUnavailable)
		return
	}
	if status == http.StatusConflict {
		http.Error(w, "machine account already exists", http.StatusConflict)
		return
	}
	if status < 200 || status >= 300 {
		http.Error(w, "invalid machine account request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"name": input.Name})
}

func validClientSecret(secret string) bool {
	if secret == "" || len(secret) > 256 || strings.Contains(secret, "=") {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(secret)
	return err == nil && len(decoded) >= 32
}

func (g *Gateway) handlePasswordChange(w http.ResponseWriter, r *http.Request, ident Identity) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", http.MethodPut)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/auth/users/"), "/password")
	if (!ident.IsUser() && !ident.IsAdmin() && !ident.IsOps()) || !isDNS1123Label(name) || name != ident.Subject {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var input struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if status, err := g.decodeJSONRequest(w, r, &input); err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	if n := utf8.RuneCountInString(input.CurrentPassword); n < 1 || n > 128 {
		http.Error(w, "invalid password request", http.StatusBadRequest)
		return
	}
	if n := utf8.RuneCountInString(input.NewPassword); n < 12 || n > 128 {
		http.Error(w, "invalid password request", http.StatusBadRequest)
		return
	}
	payload, _ := json.Marshal(map[string]string{"username": name, "password": input.CurrentPassword})
	body, status, _, err := g.upstreamRequest(r.Context(), http.MethodPost, "/internal/iam/v1/authenticate", bytes.NewReader(payload), http.Header{"Content-Type": []string{"application/json"}})
	if err != nil || status >= 500 {
		http.Error(w, "password service unavailable", http.StatusServiceUnavailable)
		return
	}
	if status < 200 || status >= 300 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var authResult struct {
		Authenticated bool   `json:"authenticated"`
		Username      string `json:"username"`
	}
	if json.Unmarshal(body, &authResult) != nil || !authResult.Authenticated || authResult.Username != name {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	payload, _ = json.Marshal(map[string]string{"password": input.NewPassword})
	_, status, _, err = g.upstreamRequest(r.Context(), http.MethodPut, "/internal/iam/v1/users/"+url.PathEscape(name)+"/password", bytes.NewReader(payload), http.Header{"Content-Type": []string{"application/json"}})
	if err != nil || status >= 500 {
		http.Error(w, "password service unavailable", http.StatusServiceUnavailable)
		return
	}
	if status == http.StatusNotFound {
		http.Error(w, "user is not allowed", http.StatusForbidden)
		return
	}
	if status < 200 || status >= 300 {
		http.Error(w, "invalid password request", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (g *Gateway) handleIAMAPI(w http.ResponseWriter, r *http.Request, ident Identity) {
	const machinePrefix = "/apis/iam.ebs/v1/machineaccounts"
	const userPrefix = "/apis/iam.ebs/v1/users"
	if r.URL.Path == userPrefix || strings.HasPrefix(r.URL.Path, userPrefix+"/") {
		g.handleUserAPI(w, r, ident)
		return
	}
	if r.URL.Path != machinePrefix && !strings.HasPrefix(r.URL.Path, machinePrefix+"/") {
		http.NotFound(w, r)
		return
	}
	if !ident.IsAdmin() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, machinePrefix)
	if name == "" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
	} else {
		name = strings.TrimPrefix(name, "/")
		if !isDNS1123Label(name) || strings.Contains(name, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodDelete {
			w.Header().Set("Allow", http.MethodGet+", "+http.MethodDelete)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
	}
	injectIdentityHeaders(r, ident)
	g.proxy.ServeHTTP(w, r)
}

func (g *Gateway) handleUserAPI(w http.ResponseWriter, r *http.Request, ident Identity) {
	const userPrefix = "/apis/iam.ebs/v1/users"
	if !ident.IsAdmin() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	suffix := strings.TrimPrefix(r.URL.Path, userPrefix)
	if suffix == "" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		g.handleOrdinaryUserList(w, r)
		return
	}
	name := strings.TrimPrefix(suffix, "/")
	if !isDNS1123Label(name) || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPut && r.Method != http.MethodPatch && r.Method != http.MethodDelete {
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPut+", "+http.MethodPatch+", "+http.MethodDelete)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	oldData, status, header, err := g.upstreamRequest(r.Context(), http.MethodGet, userPrefix+"/"+url.PathEscape(name), nil, nil)
	if err != nil {
		http.Error(w, "user service unavailable", http.StatusServiceUnavailable)
		return
	}
	if status < 200 || status >= 300 {
		writeUpstreamResponse(w, status, header, oldData)
		return
	}
	oldObject, err := decodeObject(oldData)
	if err != nil {
		http.Error(w, "invalid upstream user", http.StatusBadGateway)
		return
	}
	if userObjectIsAdmin(oldObject) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeUpstreamResponse(w, status, header, oldData)
	case http.MethodDelete:
		g.deleteOrdinaryUser(w, r, name, oldObject)
	case http.MethodPut, http.MethodPatch:
		if err := g.prepareOrdinaryUserUpdate(r, name, oldData, oldObject); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		injectIdentityHeaders(r, ident)
		g.proxy.ServeHTTP(w, r)
	}
}

func (g *Gateway) handleOrdinaryUserList(w http.ResponseWriter, r *http.Request) {
	body, status, header, err := g.upstreamRequest(r.Context(), http.MethodGet, r.URL.RequestURI(), nil, nil)
	if err != nil {
		http.Error(w, "user service unavailable", http.StatusServiceUnavailable)
		return
	}
	if status < 200 || status >= 300 {
		writeUpstreamResponse(w, status, header, body)
		return
	}
	var list map[string]any
	if err := json.Unmarshal(body, &list); err != nil {
		http.Error(w, "invalid upstream user list", http.StatusBadGateway)
		return
	}
	items, ok := list["items"].([]any)
	if !ok {
		http.Error(w, "invalid upstream user list", http.StatusBadGateway)
		return
	}
	filtered := make([]any, 0, len(items))
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if ok && !userObjectIsAdmin(obj) {
			filtered = append(filtered, item)
		}
	}
	list["items"] = filtered
	data, err := json.Marshal(list)
	if err != nil {
		http.Error(w, "encode user list", http.StatusInternalServerError)
		return
	}
	header.Del("Content-Length")
	header.Set("Content-Type", "application/json")
	writeUpstreamResponse(w, http.StatusOK, header, data)
}

func userObjectIsAdmin(obj map[string]any) bool {
	spec, _ := obj["spec"].(map[string]any)
	scopes, _ := spec["scopes"].([]any)
	return len(scopes) == 1 && scopes[0] == "ebs:admin"
}

func (g *Gateway) prepareOrdinaryUserUpdate(r *http.Request, name string, oldData []byte, oldObject map[string]any) error {
	data, err := readAndRestoreBody(r, g.cfg.MaxRequestBodyBytes)
	if err != nil {
		return err
	}
	var candidateData []byte
	if r.Method == http.MethodPut {
		if strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]) != "application/json" {
			return errors.New("unsupported user update type")
		}
		candidateData = data
	} else {
		contentType := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])
		switch contentType {
		case "application/merge-patch+json":
			candidateData, err = jsonpatch.MergePatch(oldData, data)
		case "application/json-patch+json":
			var patch jsonpatch.Patch
			patch, err = jsonpatch.DecodePatch(data)
			if err == nil {
				candidateData, err = patch.Apply(oldData)
			}
		default:
			return errors.New("unsupported user patch type")
		}
		if err != nil {
			return errors.New("invalid user patch")
		}
	}
	if int64(len(candidateData)) > g.cfg.MaxRequestBodyBytes {
		return errors.New("user update too large")
	}
	candidate, err := decodeObject(candidateData)
	if err != nil {
		return err
	}
	if err := validateOrdinaryUserCandidate(name, oldObject, candidate); err != nil {
		return err
	}
	if r.Method == http.MethodPatch {
		oldMeta, _ := oldObject["metadata"].(map[string]any)
		meta, _ := candidate["metadata"].(map[string]any)
		if meta == nil {
			return errors.New("user metadata is required")
		}
		if value, exists := meta["resourceVersion"]; exists && !reflect.DeepEqual(value, oldMeta["resourceVersion"]) {
			return errors.New("user resourceVersion conflict")
		}
		meta["resourceVersion"] = oldMeta["resourceVersion"]
		candidateData, err = json.Marshal(candidate)
		if err != nil {
			return err
		}
		r.Method = http.MethodPut
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Del("Content-Length")
	r.ContentLength = int64(len(candidateData))
	r.Body = io.NopCloser(bytes.NewReader(candidateData))
	return nil
}

func validateOrdinaryUserCandidate(name string, oldObject, candidate map[string]any) error {
	for key := range candidate {
		if key != "apiVersion" && key != "kind" && key != "metadata" && key != "spec" {
			return errors.New("unsupported user field")
		}
	}
	if fmt.Sprint(candidate["apiVersion"]) != "iam.ebs/v1" || fmt.Sprint(candidate["kind"]) != "User" {
		return errors.New("invalid user type metadata")
	}
	oldMeta, _ := oldObject["metadata"].(map[string]any)
	meta, _ := candidate["metadata"].(map[string]any)
	if meta == nil || fmt.Sprint(meta["name"]) != name || fmt.Sprint(oldMeta["name"]) != name {
		return errors.New("user identity mismatch")
	}
	for _, key := range []string{"name", "namespace", "uid", "creationTimestamp", "deletionTimestamp", "deletionGracePeriodSeconds", "generation", "managedFields", "finalizers", "ownerReferences"} {
		if !reflect.DeepEqual(oldMeta[key], meta[key]) {
			return errors.New("user protected metadata changed")
		}
	}
	if fmt.Sprint(meta["resourceVersion"]) == "" || !reflect.DeepEqual(oldMeta["resourceVersion"], meta["resourceVersion"]) {
		return errors.New("user resourceVersion conflict")
	}
	spec, ok := candidate["spec"].(map[string]any)
	if !ok {
		return errors.New("user spec is required")
	}
	for key := range spec {
		if key != "enabled" && key != "scopes" && key != "displayName" && key != "email" {
			return errors.New("unsupported user spec field")
		}
	}
	if userObjectIsAdmin(candidate) || userObjectIsAdmin(oldObject) {
		return errors.New("admin users cannot be managed")
	}
	return nil
}

func (g *Gateway) deleteOrdinaryUser(w http.ResponseWriter, r *http.Request, name string, oldObject map[string]any) {
	meta, _ := oldObject["metadata"].(map[string]any)
	uid, _ := meta["uid"].(string)
	resourceVersion, _ := meta["resourceVersion"].(string)
	if uid == "" || resourceVersion == "" {
		http.Error(w, "invalid upstream user", http.StatusBadGateway)
		return
	}
	payload, _ := json.Marshal(map[string]any{"apiVersion": "meta.k8s.io/v1", "kind": "DeleteOptions", "preconditions": map[string]string{"uid": uid, "resourceVersion": resourceVersion}})
	body, status, header, err := g.upstreamRequest(r.Context(), http.MethodDelete, "/apis/iam.ebs/v1/users/"+url.PathEscape(name), bytes.NewReader(payload), http.Header{"Content-Type": []string{"application/json"}})
	if err != nil {
		http.Error(w, "user service unavailable", http.StatusServiceUnavailable)
		return
	}
	writeUpstreamResponse(w, status, header, body)
}

func writeUpstreamResponse(w http.ResponseWriter, status int, header http.Header, body []byte) {
	copyResponseHeaders(w.Header(), header)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (g *Gateway) decodeJSONRequest(w http.ResponseWriter, r *http.Request, out any) (int, error) {
	if contentType := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]); contentType != "application/json" {
		return http.StatusUnsupportedMediaType, errors.New("unsupported media type")
	}
	r.Body = http.MaxBytesReader(w, r.Body, g.cfg.MaxRequestBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return http.StatusRequestEntityTooLarge, errors.New("request body too large")
		}
		return http.StatusBadRequest, errors.New("invalid request")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return http.StatusBadRequest, errors.New("invalid request")
	}
	return 0, nil
}

func isDNS1123Label(value string) bool {
	if len(value) == 0 || len(value) > 63 || !isLowerAlphanumeric(value[0]) || !isLowerAlphanumeric(value[len(value)-1]) {
		return false
	}
	for i := 1; i < len(value)-1; i++ {
		if value[i] != '-' && !isLowerAlphanumeric(value[i]) {
			return false
		}
	}
	return true
}

func isLowerAlphanumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= '0' && value <= '9'
}

func (g *Gateway) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if contentType := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]); contentType != "application/json" {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, g.cfg.MaxRequestBodyBytes)
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid login request", http.StatusBadRequest)
		return
	}
	if !isDNS1123Label(input.Username) || input.Password == "" || utf8.RuneCountInString(input.Password) > 128 {
		http.Error(w, "invalid login request", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "invalid login request", http.StatusBadRequest)
		return
	}
	username := input.Username
	if !g.limiter.Allow("login/" + username + "/" + clientIP(r)) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	payload, _ := json.Marshal(map[string]string{"username": username, "password": input.Password})
	authBody, status, _, err := g.upstreamRequest(r.Context(), http.MethodPost, "/internal/iam/v1/authenticate", bytes.NewReader(payload), http.Header{"Content-Type": []string{"application/json"}})
	if err != nil || status >= 500 {
		http.Error(w, "authentication service unavailable", http.StatusServiceUnavailable)
		return
	}
	if status < 200 || status >= 300 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var authResult struct {
		Authenticated bool   `json:"authenticated"`
		Username      string `json:"username"`
	}
	if json.Unmarshal(authBody, &authResult) != nil || !authResult.Authenticated || authResult.Username != username {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	user, resolveErr := g.getUser(r.Context(), username)
	if resolveErr != nil {
		if resolveErr.status >= 500 {
			http.Error(w, "authentication service unavailable", http.StatusServiceUnavailable)
		} else {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
		return
	}
	if !user.Enabled {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	issuedAt := g.now()
	var token string
	var expiresAt int64
	switch user.Scope {
	case "ebs:admin":
		token, expiresAt, err = g.tokens.issueAdmin(username, issuedAt)
	case "ebs:ops":
		token, expiresAt, err = g.tokens.issueOps(username, issuedAt)
	case "ebs:user":
		token, expiresAt, err = g.tokens.issueUser(username, issuedAt)
	default:
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err != nil {
		http.Error(w, "unable to issue token", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"token": token, "tokenType": "Bearer", "expiresIn": expiresAt - issuedAt.Unix()})
}

func (g *Gateway) resolveUser(ctx context.Context, username string) *gatewayHTTPError {
	user, resolveErr := g.getUser(ctx, username)
	if resolveErr != nil {
		return resolveErr
	}
	if !user.Enabled || user.Scope != "ebs:user" {
		return &gatewayHTTPError{status: http.StatusForbidden, message: "user is not allowed"}
	}
	return nil
}

func (g *Gateway) resolveProjectUser(ctx context.Context, username string) *gatewayHTTPError {
	user, resolveErr := g.getUser(ctx, username)
	if resolveErr != nil {
		return resolveErr
	}
	if !user.Enabled || (user.Scope != "ebs:user" && user.Scope != "ebs:ops") {
		return &gatewayHTTPError{status: http.StatusForbidden, message: "project user is not allowed"}
	}
	return nil
}

func (g *Gateway) resolveAdmin(ctx context.Context, username string) *gatewayHTTPError {
	user, resolveErr := g.getUser(ctx, username)
	if resolveErr != nil {
		return resolveErr
	}
	if !user.Enabled || user.Scope != "ebs:admin" {
		return &gatewayHTTPError{status: http.StatusForbidden, message: "admin is not allowed"}
	}
	return nil
}

func (g *Gateway) resolveOps(ctx context.Context, username string) *gatewayHTTPError {
	user, resolveErr := g.getUser(ctx, username)
	if resolveErr != nil {
		return resolveErr
	}
	if !user.Enabled || user.Scope != "ebs:ops" {
		return &gatewayHTTPError{status: http.StatusForbidden, message: "ops user is not allowed"}
	}
	return nil
}

type userInfo struct {
	Name    string
	Enabled bool
	Scope   string
}

func (g *Gateway) getUser(ctx context.Context, username string) (userInfo, *gatewayHTTPError) {
	path := "/apis/iam.ebs/v1/users/" + url.PathEscape(username)
	body, status, _, err := g.upstreamRequest(ctx, http.MethodGet, path, nil, nil)
	if err != nil || status >= 500 {
		return userInfo{}, &gatewayHTTPError{status: http.StatusServiceUnavailable, message: "user service unavailable"}
	}
	if status < 200 || status >= 300 {
		return userInfo{}, &gatewayHTTPError{status: http.StatusForbidden, message: "user is not allowed"}
	}
	var user struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Enabled bool     `json:"enabled"`
			Scopes  []string `json:"scopes"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &user); err != nil {
		return userInfo{}, &gatewayHTTPError{status: http.StatusForbidden, message: "user is not allowed"}
	}
	if user.Metadata.Name != username {
		return userInfo{}, &gatewayHTTPError{status: http.StatusForbidden, message: "user is not allowed"}
	}
	if len(user.Spec.Scopes) != 1 {
		return userInfo{}, &gatewayHTTPError{status: http.StatusForbidden, message: "user is not allowed"}
	}
	return userInfo{Name: user.Metadata.Name, Enabled: user.Spec.Enabled, Scope: user.Spec.Scopes[0]}, nil
}

func (g *Gateway) authorizeRunner(ctx context.Context, r *http.Request, ident Identity, route routeInfo) (authzDecision, error) {
	if ident.Runner == "" || ident.Subject != ident.Runner {
		return authzDecision{}, fmt.Errorf("runner identity mismatch")
	}
	if route.resource == "runners" {
		if route.name == "" {
			if r.Method != http.MethodPost {
				return authzDecision{}, fmt.Errorf("runner collection access denied")
			}
			if err := g.validateRunnerCreate(r, ident.Runner); err != nil {
				return authzDecision{}, err
			}
			return authzDecision{}, nil
		}
		if route.name != ident.Runner {
			return authzDecision{}, fmt.Errorf("runner access denied")
		}
		if len(route.rest) == 1 && route.rest[0] == "jobs" {
			return g.authorizeRunnerJobs(r, ident)
		}
		if len(route.rest) == 1 && route.rest[0] == "status" {
			if r.Method != http.MethodPut && r.Method != http.MethodPatch {
				return authzDecision{}, fmt.Errorf("runner status method denied")
			}
			if err := g.validateRunnerStatusBody(r); err != nil {
				return authzDecision{}, err
			}
			return authzDecision{}, nil
		}
		if len(route.rest) != 0 {
			return authzDecision{}, fmt.Errorf("runner subresource denied")
