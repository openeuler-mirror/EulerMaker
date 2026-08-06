package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	apiPrefix           = "/apis/ebs/v1"
	ownerUserLabel      = "ebs.io/owner-user"
	memberUserLabelBase = "ebs.io/member-user."
)

type Gateway struct {
	cfg       Config
	upstream  *url.URL
	client    *http.Client
	proxy     *httputil.ReverseProxy
	limiter   *RateLimiter
	tokens    *tokenManager
	now       func() time.Time
	transport http.RoundTripper
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
	if cfg.MaxRequestBodyBytes == 0 {
		cfg.MaxRequestBodyBytes = 1048576
	}

	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.Transport = transport
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}

	gw := &Gateway{
		cfg:       cfg,
		upstream:  upstream,
		client:    &http.Client{Transport: transport},
		proxy:     proxy,
		limiter:   NewRateLimiter(cfg.RateLimitPerSec, cfg.RateLimitBurst),
		tokens:    tokens,
		now:       time.Now,
		transport: transport,
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
	if !strings.HasPrefix(r.URL.Path, apiPrefix+"/") && r.URL.Path != apiPrefix {
		http.NotFound(rec, r)
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

	limitKey := ident.Subject + "/" + clientIP(r)
	if !g.limiter.Allow(limitKey) {
		http.Error(rec, "too many requests", http.StatusTooManyRequests)
		return
	}

	decision, err := g.authorizeAndPrepare(r.Context(), r, ident)
	if err != nil {
		http.Error(rec, err.Error(), http.StatusForbidden)
		return
	}
	if decision.handle != nil {
		decision.handle(rec, r)
		return
	}

	injectIdentityHeaders(r, ident)
	g.proxy.ServeHTTP(rec, r)
}

type gatewayHTTPError struct {
	status  int
	message string
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
	if strings.TrimSpace(input.Username) == "" || input.Password == "" {
		http.Error(w, "invalid login request", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "invalid login request", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(input.Username)
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
	if resolveErr := g.resolveUser(r.Context(), username); resolveErr != nil {
		if resolveErr.status >= 500 {
			http.Error(w, "authentication service unavailable", http.StatusServiceUnavailable)
		} else {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
		return
	}
	issuedAt := g.now()
	token, expiresAt, err := g.tokens.issueUser(username, issuedAt)
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
	path := "/apis/iam.ebs/v1/users/" + url.PathEscape(username)
	body, status, _, err := g.upstreamRequest(ctx, http.MethodGet, path, nil, nil)
	if err != nil || status >= 500 {
		return &gatewayHTTPError{status: http.StatusServiceUnavailable, message: "user service unavailable"}
	}
	if status < 200 || status >= 300 {
		return &gatewayHTTPError{status: http.StatusForbidden, message: "user is not allowed"}
	}
	var user struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Enabled bool `json:"enabled"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &user); err != nil {
		return &gatewayHTTPError{status: http.StatusForbidden, message: "user is not allowed"}
	}
	if user.Metadata.Name != username || !user.Spec.Enabled {
		return &gatewayHTTPError{status: http.StatusForbidden, message: "user is not allowed"}
	}
	return nil
}

type authzDecision struct {
	handle http.HandlerFunc
}

func (g *Gateway) authorizeAndPrepare(ctx context.Context, r *http.Request, ident Identity) (authzDecision, error) {
	if ident.IsSystem() {
		route := parseRoute(r.URL.Path)
		if route.resource == "projects" && route.project == "" && r.Method == http.MethodPost {
			if err := g.validateSystemProjectOwner(ctx, r); err != nil {
				return authzDecision{}, err
			}
		}
		injectIdentityHeaders(r, ident)
		return authzDecision{}, nil
	}

	route := parseRoute(r.URL.Path)
	if route.resource == "" {
		return authzDecision{}, fmt.Errorf("unsupported ebs api path")
	}

	if route.resource == "runners" {
		return authzDecision{}, fmt.Errorf("runner api requires system scope")
	}

	if route.project == "" && isProjectScopedResource(route.resource) {
		return authzDecision{}, fmt.Errorf("global %s api requires system scope", route.resource)
	}

	if route.resource == "projects" && route.project == "" {
		return g.handleProjectCollection(ctx, r, ident)
	}

	if route.resource == "projects" && route.project != "" {
		project, err := g.getProject(ctx, route.project)
		if err != nil {
			return authzDecision{}, err
		}
		if !projectAllowsUser(project, ident.Subject) {
			return authzDecision{}, fmt.Errorf("project access denied")
		}
		if project.Labels[ownerUserLabel] != ident.Subject && r.Method != http.MethodGet && r.Method != http.MethodHead {
			return authzDecision{}, fmt.Errorf("only project owner can modify project")
		}
		if isProjectObjectWrite(r.Method, route) {
			if err := g.protectProjectAccessLabels(r, ident, project); err != nil {
				return authzDecision{}, err
			}
		}
		return authzDecision{}, nil
	}

	if route.project != "" {
		project, err := g.getProject(ctx, route.project)
		if err != nil {
			return authzDecision{}, err
		}
		if !projectAllowsUser(project, ident.Subject) {
			return authzDecision{}, fmt.Errorf("project access denied")
		}
		if project.Labels[ownerUserLabel] != ident.Subject && r.Method == http.MethodDelete {
			return authzDecision{}, fmt.Errorf("project member cannot delete resources")
		}
		return authzDecision{}, nil
	}

	return authzDecision{}, fmt.Errorf("access denied")
}

func (g *Gateway) handleProjectCollection(ctx context.Context, r *http.Request, ident Identity) (authzDecision, error) {
	switch r.Method {
	case http.MethodPost:
		if err := injectProjectOwnerLabel(r, ident.Subject); err != nil {
			return authzDecision{}, err
		}
		return authzDecision{}, nil
	case http.MethodGet:
		if r.URL.Query().Get("watch") == "true" {
			return authzDecision{}, fmt.Errorf("project watch requires system scope")
		}
		return authzDecision{handle: func(w http.ResponseWriter, req *http.Request) {
			g.handleFilteredProjectList(ctx, w, req, ident)
		}}, nil
	default:
		return authzDecision{}, fmt.Errorf("project collection method %s is not allowed", r.Method)
	}
}

func (g *Gateway) handleFilteredProjectList(ctx context.Context, w http.ResponseWriter, r *http.Request, ident Identity) {
	body, status, header, err := g.upstreamRequest(ctx, http.MethodGet, r.URL.RequestURI(), nil, nil)
	if err != nil {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	copyResponseHeaders(w.Header(), header)
	if status < 200 || status >= 300 {
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}

	var list map[string]any
	if err := json.Unmarshal(body, &list); err != nil {
		http.Error(w, "invalid upstream project list", http.StatusBadGateway)
		return
	}
	items, _ := list["items"].([]any)
	filtered := make([]any, 0, len(items))
	for _, item := range items {
		project, ok := projectFromAny(item)
		if ok && projectAllowsUser(project, ident.Subject) {
			filtered = append(filtered, item)
		}
	}
	list["items"] = filtered
	if meta, ok := list["metadata"].(map[string]any); ok {
		delete(meta, "continue")
		meta["remainingItemCount"] = int64(0)
	}
	data, err := json.Marshal(list)
	if err != nil {
		http.Error(w, "encode filtered project list", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Del("Content-Length")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (g *Gateway) getProject(ctx context.Context, name string) (projectInfo, error) {
	escaped := url.PathEscape(name)
	body, status, _, err := g.upstreamRequest(ctx, http.MethodGet, apiPrefix+"/projects/"+escaped, nil, nil)
	if err != nil {
		return projectInfo{}, fmt.Errorf("read project: %w", err)
	}
	if status < 200 || status >= 300 {
		return projectInfo{}, fmt.Errorf("project access denied")
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return projectInfo{}, fmt.Errorf("parse project: %w", err)
	}
	project, ok := projectFromAny(raw)
	if !ok {
		return projectInfo{}, fmt.Errorf("parse project metadata")
	}
	return project, nil
}

func (g *Gateway) upstreamRequest(ctx context.Context, method, requestURI string, body io.Reader, header http.Header) ([]byte, int, http.Header, error) {
	u := *g.upstream
	parsed, err := url.Parse(requestURI)
	if err != nil {
		return nil, 0, nil, err
	}
	u.Path = singleJoiningSlash(g.upstream.Path, parsed.Path)
	u.RawQuery = parsed.RawQuery

	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, 0, nil, err
	}
	for k, values := range header {
		for _, value := range values {
			req.Header.Add(k, value)
		}
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, nil, err
	}
	return data, resp.StatusCode, resp.Header.Clone(), nil
}

func injectIdentityHeaders(r *http.Request, ident Identity) {
	for key := range r.Header {
		if strings.HasPrefix(strings.ToLower(key), "x-ebs-") {
			r.Header.Del(key)
		}
	}
	r.Header.Set("X-EBS-User", ident.Subject)
	r.Header.Set("X-EBS-Scopes", ident.ScopeHeader())
}

func injectProjectOwnerLabel(r *http.Request, username string) error {
	if !methodHasBody(r.Method) {
		return nil
	}
	obj, err := readJSONObject(r)
	if err != nil {
		return err
	}
	labels := ensureLabels(obj)
	labels[ownerUserLabel] = username
	return writeJSONObject(r, obj)
}

func (g *Gateway) validateSystemProjectOwner(ctx context.Context, r *http.Request) error {
	obj, err := readJSONObject(r)
	if err != nil {
		return err
	}
	labels := ensureLabels(obj)
	owner, ok := labels[ownerUserLabel].(string)
	owner = strings.TrimSpace(owner)
	if !ok || owner == "" {
		return fmt.Errorf("system project create requires owner user label")
	}
	if resolveErr := g.resolveUser(ctx, owner); resolveErr != nil {
		return fmt.Errorf("project owner user is not allowed")
	}
	return writeJSONObject(r, obj)
}

func (g *Gateway) protectProjectAccessLabels(r *http.Request, ident Identity, old projectInfo) error {
	if !methodHasBody(r.Method) {
		return nil
	}
	if r.Method == http.MethodPatch {
		return g.protectProjectPatchAccessLabels(r, ident, old)
	}

	obj, err := readJSONObject(r)
	if err != nil {
		return err
	}
	labels := ensureLabels(obj)
	oldOwner := old.Labels[ownerUserLabel]
	newOwner := labels[ownerUserLabel]
	if newOwner != oldOwner {
		return fmt.Errorf("owner user label is immutable")
	}
	if ident.Subject != oldOwner && accessLabelsChanged(labels, old.Labels) {
		return fmt.Errorf("only project owner can modify project access labels")
	}
	for label, value := range labels {
		if strings.HasPrefix(label, memberUserLabelBase) && fmt.Sprint(value) == "true" && old.Labels[label] != "true" {
			if resolveErr := g.resolveUser(r.Context(), strings.TrimPrefix(label, memberUserLabelBase)); resolveErr != nil {
				return fmt.Errorf("project member user is not allowed")
			}
		}
	}
	return writeJSONObject(r, obj)
}

func (g *Gateway) protectProjectPatchAccessLabels(r *http.Request, ident Identity, old projectInfo) error {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(data))
	r.ContentLength = int64(len(data))

	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(contentType, "json-patch+json") {
		var ops []map[string]any
		if err := json.Unmarshal(data, &ops); err != nil {
			return fmt.Errorf("parse project patch: %w", err)
		}
		for _, op := range ops {
			path, _ := op["path"].(string)
			label := labelNameFromJSONPatchPath(path)
			if label == "" {
				continue
			}
			if label == ownerUserLabel {
				return fmt.Errorf("owner user label is immutable")
			}
			if strings.HasPrefix(label, memberUserLabelBase) && ident.Subject != old.Labels[ownerUserLabel] {
				return fmt.Errorf("only project owner can modify project member labels")
			}
			if strings.HasPrefix(label, memberUserLabelBase) && old.Labels[label] != "true" && fmt.Sprint(op["value"]) == "true" {
				if resolveErr := g.resolveUser(r.Context(), strings.TrimPrefix(label, memberUserLabelBase)); resolveErr != nil {
					return fmt.Errorf("project member user is not allowed")
				}
			}
		}
		return nil
	}

	var patch map[string]any
	if err := json.Unmarshal(data, &patch); err != nil {
		return fmt.Errorf("parse project patch: %w", err)
	}
	labels := labelsFromObject(patch)
	for label, value := range labels {
		if label == ownerUserLabel {
			if value != old.Labels[ownerUserLabel] {
				return fmt.Errorf("owner user label is immutable")
			}
		}
		if strings.HasPrefix(label, memberUserLabelBase) && ident.Subject != old.Labels[ownerUserLabel] {
			return fmt.Errorf("only project owner can modify project member labels")
		}
		if strings.HasPrefix(label, memberUserLabelBase) && old.Labels[label] != "true" && value == "true" {
			if resolveErr := g.resolveUser(r.Context(), strings.TrimPrefix(label, memberUserLabelBase)); resolveErr != nil {
				return fmt.Errorf("project member user is not allowed")
			}
		}
	}
	return nil
}

func readJSONObject(r *http.Request) (map[string]any, error) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	_ = r.Body.Close()
	var obj map[string]any
	if len(bytes.TrimSpace(data)) == 0 {
		obj = map[string]any{}
	} else if err := json.Unmarshal(data, &obj); err != nil {
		return nil, fmt.Errorf("parse json body: %w", err)
	}
	return obj, nil
}

func writeJSONObject(r *http.Request, obj map[string]any) error {
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	r.ContentLength = int64(len(data))
	r.Header.Set("Content-Type", "application/json")
	return nil
}

func ensureLabels(obj map[string]any) map[string]any {
	meta, ok := obj["metadata"].(map[string]any)
	if !ok {
		meta = map[string]any{}
		obj["metadata"] = meta
	}
	labels, ok := meta["labels"].(map[string]any)
	if !ok {
		labels = map[string]any{}
		meta["labels"] = labels
	}
	return labels
}

func labelsFromObject(obj map[string]any) map[string]string {
	meta, ok := obj["metadata"].(map[string]any)
	if !ok {
		return nil
	}
	rawLabels, ok := meta["labels"].(map[string]any)
	if !ok {
		return nil
	}
	labels := make(map[string]string, len(rawLabels))
	for key, value := range rawLabels {
		if value == nil {
			labels[key] = ""
			continue
		}
		labels[key] = fmt.Sprint(value)
	}
	return labels
}

func accessLabelsChanged(newLabels map[string]any, oldLabels map[string]string) bool {
	keys := map[string]struct{}{}
	for key := range oldLabels {
		if key == ownerUserLabel || strings.HasPrefix(key, memberUserLabelBase) {
			keys[key] = struct{}{}
		}
	}
	for key := range newLabels {
		if key == ownerUserLabel || strings.HasPrefix(key, memberUserLabelBase) {
			keys[key] = struct{}{}
		}
	}
	for key := range keys {
		if fmt.Sprint(newLabels[key]) != oldLabels[key] {
			return true
		}
	}
	return false
}

type routeInfo struct {
	resource string
	project  string
	name     string
	rest     []string
}

func parseRoute(path string) routeInfo {
	rel := strings.TrimPrefix(path, apiPrefix)
	rel = strings.Trim(rel, "/")
	if rel == "" {
		return routeInfo{}
	}
	parts := strings.Split(rel, "/")
	if len(parts) == 0 {
		return routeInfo{}
	}
	if parts[0] == "projects" && len(parts) >= 3 && isProjectScopedResource(parts[2]) {
		route := routeInfo{resource: parts[2], project: parts[1]}
		if len(parts) >= 4 {
			route.name = parts[3]
		}
		if len(parts) > 4 {
			route.rest = parts[4:]
		}
		return route
	}
	route := routeInfo{resource: parts[0]}
	if len(parts) >= 2 {
		route.project = parts[1]
		route.name = parts[1]
	}
	if len(parts) > 2 {
		route.rest = parts[2:]
	}
	return route
}

func isProjectScopedResource(resource string) bool {
	return resource == "snapshots" || resource == "builds" || resource == "buildinfos" || resource == "rpmrepos" || resource == "jobs"
}

func isProjectObjectWrite(method string, route routeInfo) bool {
	if route.resource != "projects" || route.project == "" || len(route.rest) > 0 {
		return false
	}
	return method == http.MethodPut || method == http.MethodPatch
}

func methodHasBody(method string) bool {
	return method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch
}

type projectInfo struct {
	Name   string
	Labels map[string]string
}

func projectFromAny(value any) (projectInfo, bool) {
	obj, ok := value.(map[string]any)
	if !ok {
		return projectInfo{}, false
	}
	meta, ok := obj["metadata"].(map[string]any)
	if !ok {
		return projectInfo{}, false
	}
	project := projectInfo{Labels: map[string]string{}}
	if name, ok := meta["name"].(string); ok {
		project.Name = name
	}
	if rawLabels, ok := meta["labels"].(map[string]any); ok {
		for key, value := range rawLabels {
			project.Labels[key] = fmt.Sprint(value)
		}
	}
	return project, true
}

func projectAllowsUser(project projectInfo, username string) bool {
	if project.Labels[ownerUserLabel] == username {
		return true
	}
	return project.Labels[memberUserLabelBase+username] == "true"
}

func labelNameFromJSONPatchPath(path string) string {
	const prefix = "/metadata/labels/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	encoded := strings.TrimPrefix(path, prefix)
	encoded = strings.ReplaceAll(encoded, "~1", "/")
	encoded = strings.ReplaceAll(encoded, "~0", "~")
	return encoded
}

func clientIP(r *http.Request) string {
	if forwardedFor := r.Header.Get("X-Forwarded-For"); forwardedFor != "" {
		parts := strings.Split(forwardedFor, ",")
		return strings.TrimSpace(parts[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func copyResponseHeaders(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	default:
		return a + b
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
