package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/mutation"
	"ebs-gateway/internal/policy"
)

type resourceRoute struct {
	resource    string
	projectKey  string
	nameKey     string
	subresource string
}

const apiPrefix = "/apis/ebs/v1"

func (r resourceRoute) at(c *gin.Context) policy.Route {
	return policy.Route{
		Resource: r.resource, Project: c.Param(r.projectKey),
		Name: c.Param(r.nameKey), Subresource: r.subresource,
		Method: c.Request.Method,
	}
}

// ResourceHandlers returns the guard and final handler for one registered resource path.
func (a *Handler) ResourceHandlers(resource, projectKey, nameKey, subresource string) (gin.HandlerFunc, gin.HandlerFunc) {
	route := resourceRoute{resource: resource, projectKey: projectKey, nameKey: nameKey, subresource: subresource}
	return a.resourceGuard(route), a.forwardResource(route)
}

func (a *Handler) resourceGuard(route resourceRoute) gin.HandlerFunc {
	return func(c *gin.Context) {
		resolved := route.at(c)
		public := policy.IsPublicRead(resolved) ||
			(resolved.Resource == "configs" && resolved.Name != "" && (resolved.Method == http.MethodGet || resolved.Method == http.MethodHead))
		if policy.HasWatch(c.Request.URL.Query()) {
			public = false
		}
		header := c.GetHeader("Authorization")
		var who identity.Principal
		if header == "" && public {
			c.Set("anonymous", true)
		} else {
			var ok bool
			who, ok = a.verifyBearer(c, header)
			if !ok {
				return
			}
			if !a.confirmUser(c, who) {
				return
			}
			c.Set(principalKey, who)
		}
		if who.Type == identity.RunnerType && resolved.Resource != "configs" {
			public = false
		}
		if !allowedResourceMethod(resolved) {
			c.Header("Allow", allowedResourceVerbs(resolved))
			c.AbortWithStatus(http.StatusMethodNotAllowed)
			return
		}
		if policy.HasWatch(c.Request.URL.Query()) && (resolved.Resource == "configs" || resolved.Resource == "scripts") {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "watch is not supported for this resource"})
			return
		}
		if public {
			if !a.publicLimit.Allow(c.ClientIP()) {
				c.Header("Retry-After", "1")
				c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "too many requests"})
				return
			}
			if resolved.Name == "" && resolved.Subresource == "" {
				query := c.Request.URL.Query()
				limit, err := strconv.Atoi(query.Get("limit"))
				if err != nil || limit < 1 || limit > 100 || len(query["limit"]) != 1 {
					query.Set("limit", "100")
					c.Request.URL.RawQuery = query.Encode()
				}
			}
			c.Set("public", true)
		} else {
			if !a.limits.Allow(who.Subject + "/" + c.ClientIP()) {
				c.Header("Retry-After", "1")
				c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "too many requests"})
				return
			}
			if err := a.policy.Authorize(c.Request.Context(), who, resolved); err != nil {
				status := http.StatusForbidden
				if classified, ok := err.(*policy.Error); ok {
					status = classified.Status
				}
				c.AbortWithStatusJSON(status, gin.H{"error": err.Error()})
				return
			}
			if who.Type == identity.RunnerType && resolved.Resource == "runners" && resolved.Subresource == "jobs" {
				if !validRunnerJobsQuery(c.Request.URL.Query()) {
					c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "unsupported Runner Jobs query"})
					return
				}
			}
		}
		c.Next()
	}
}

func (a *Handler) verifyBearer(c *gin.Context, header string) (identity.Principal, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || parts[0] != "Bearer" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return identity.Principal{}, false
	}
	who, err := a.tokens.Verify(parts[1], a.now())
	if err != nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return identity.Principal{}, false
	}
	return who, true
}

func (a *Handler) confirmUser(c *gin.Context, who identity.Principal) bool {
	if !who.IsUser() {
		return true
	}
	user, status, err := a.iam.GetUser(c.Request.Context(), who.Subject)
	if err != nil || (status != http.StatusOK && status != http.StatusNotFound) {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "user service unavailable"})
		return false
	}
	if status == http.StatusNotFound || user.Name != who.Subject || !user.Enabled || len(user.Scopes) != 1 || user.Scopes[0] != who.Scope {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return false
	}
	return true
}

func (a *Handler) forwardResource(route resourceRoute) gin.HandlerFunc {
	return func(c *gin.Context) {
		resolved := route.at(c)
		if route.resource == "configs" && c.Param("name") != "" && (c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead) {
			a.serveConfig(c)
			return
		}
		who := principal(c)
		if who.Type == identity.RunnerType && route.resource == "runners" && route.subresource == "jobs" && c.Query("watch") == "true" {
			if !a.acquireRunnerWatch(who.Runner) {
				c.Header("Retry-After", "5")
				c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many Runner watches"})
				return
			}
			defer a.releaseRunnerWatch(who.Runner)
		}
		if who.Type == identity.RunnerType && route.resource == "jobs" && (c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead) {
			if !a.runnerOwnsJob(c, who.Runner) {
				return
			}
		}
		if c.Request.Method == http.MethodPost && route.subresource == "" {
			if !a.prepareCreate(c, who, resolved) {
				return
			}
		}
		if (c.Request.Method == http.MethodPost && route.subresource == "abort") || c.Request.Method == http.MethodDelete {
			if !a.limitWriteBody(c) {
				return
			}
		}
		if c.Request.Method == http.MethodPut || c.Request.Method == http.MethodPatch {
			if !a.prepareUpdate(c, who, resolved) {
				return
			}
		}
		public, _ := c.Get("public")
		a.upstream.Forward(c, who.Subject, string(who.Type), string(who.Scope), public == true)
	}
}

func (a *Handler) limitWriteBody(c *gin.Context) bool {
	defer c.Request.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, a.bodyLimit+1))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unable to read request"})
		return false
	}
	if int64(len(raw)) > a.bodyLimit {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large"})
		return false
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
	c.Request.ContentLength = int64(len(raw))
	return true
}

func allowedResourceMethod(route policy.Route) bool {
	method := route.Method
	if policy.IsReadOnlyProjectResource(route.Resource) {
		return method == http.MethodGet && (route.Subresource == "" || (route.Subresource == "status" && route.Name != ""))
	}
	switch route.Subresource {
	case "abort":
		return route.Name != "" && method == http.MethodPost
	case "jobs":
		return route.Name != "" && method == http.MethodGet
	case "status":
		return route.Name != "" && (method == http.MethodGet || method == http.MethodHead || method == http.MethodPut || method == http.MethodPatch)
	case "":
		if route.Name == "" {
			return method == http.MethodGet || method == http.MethodHead || method == http.MethodPost
		}
		return method == http.MethodGet || method == http.MethodHead || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete
	default:
		return false
	}
}

func allowedResourceVerbs(route policy.Route) string {
	if policy.IsReadOnlyProjectResource(route.Resource) {
		return "GET"
	}
	switch route.Subresource {
	case "abort":
		return "POST"
	case "jobs":
		return "GET"
	case "status":
		return "GET, HEAD, PUT, PATCH"
	default:
		if route.Name == "" {
			return "GET, HEAD, POST"
		}
		return "GET, HEAD, PUT, PATCH, DELETE"
	}
}

func validRunnerJobsQuery(query url.Values) bool {
	for name, values := range query {
		switch name {
		case "watch", "allowWatchBookmarks":
			if len(values) != 1 || (values[0] != "true" && values[0] != "false") {
				return false
			}
		case "timeoutSeconds":
			if len(values) != 1 {
				return false
			}
			seconds, err := strconv.Atoi(values[0])
			if err != nil || seconds < 1 || seconds > 300 {
				return false
			}
		case "resourceVersion":
			if len(values) != 1 {
				return false
			}
		case "resourceVersionMatch":
			if len(values) != 1 || (values[0] != "Exact" && values[0] != "NotOlderThan") {
				return false
			}
		case "limit":
			if len(values) != 1 {
				return false
			}
			limit, err := strconv.Atoi(values[0])
			if err != nil || limit < 1 || limit > 500 {
				return false
			}
		case "continue", "labelSelector":
			if len(values) != 1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (a *Handler) acquireRunnerWatch(name string) bool {
	a.runnerWatchMu.Lock()
	defer a.runnerWatchMu.Unlock()
	if a.runnerWatches[name] >= 1 {
		return false
	}
	a.runnerWatches[name]++
	return true
}

func (a *Handler) releaseRunnerWatch(name string) {
	a.runnerWatchMu.Lock()
	defer a.runnerWatchMu.Unlock()
	delete(a.runnerWatches, name)
}

func (a *Handler) runnerOwnsJob(c *gin.Context, runner string) bool {
	response, err := a.upstream.Do(c.Request.Context(), http.MethodGet, c.Request.URL.Path, nil, nil)
	if err != nil || response == nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Job assignment could not be confirmed"})
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		c.JSON(http.StatusForbidden, gin.H{"error": "Job assignment could not be confirmed"})
		return false
	}
	var object struct {
		Status struct {
			Runner string `json:"runner"`
		} `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, a.bodyLimit+1)).Decode(&object); err != nil || object.Status.Runner != runner {
		c.JSON(http.StatusForbidden, gin.H{"error": "Job is not assigned to Runner"})
		return false
	}
	return true
}

func (a *Handler) prepareCreate(c *gin.Context, who identity.Principal, route policy.Route) bool {
	defer c.Request.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, a.bodyLimit+1))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unable to read request"})
		return false
	}
	if int64(len(raw)) > a.bodyLimit {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large"})
		return false
	}
	if media := strings.TrimSpace(strings.Split(c.GetHeader("Content-Type"), ";")[0]); media != "application/json" {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": "application/json required"})
		return false
	}
	encoded, err := a.policy.PrepareCreate(c.Request.Context(), who, route, raw)
	if err != nil {
		status := http.StatusForbidden
		if classified, ok := err.(*policy.Error); ok {
			status = classified.Status
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return false
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(encoded))
	c.Request.ContentLength = int64(len(encoded))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Del("Content-Length")
	return true
}

func (a *Handler) prepareUpdate(c *gin.Context, who identity.Principal, route policy.Route) bool {
	if route.Name == "" {
		c.JSON(http.StatusMethodNotAllowed, gin.H{"error": "object name required"})
		return false
	}
	response, err := a.upstream.Do(c.Request.Context(), http.MethodGet, c.Request.URL.Path, nil, nil)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "unable to read current object"})
		return false
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		c.Status(http.StatusNotFound)
		return false
	}
	if response.StatusCode != http.StatusOK {
		c.JSON(http.StatusBadGateway, gin.H{"error": "unable to read current object"})
		return false
	}
	old, err := io.ReadAll(io.LimitReader(response.Body, a.bodyLimit+1))
	if err != nil || int64(len(old)) > a.bodyLimit {
		c.JSON(http.StatusBadGateway, gin.H{"error": "current object exceeds body limit"})
		return false
	}
	defer c.Request.Body.Close()
	incoming, err := io.ReadAll(io.LimitReader(c.Request.Body, a.bodyLimit+1))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unable to read request"})
		return false
	}
	encoded, previous, candidate, err := mutation.Prepare(old, incoming, c.Request.Method, c.GetHeader("Content-Type"), a.bodyLimit)
	if err != nil {
		status := http.StatusBadRequest
		if classified, ok := err.(*mutation.Error); ok {
			status = classified.Status
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return false
	}
	if err := a.policy.ValidateUpdate(c.Request.Context(), who, route, previous, candidate); err != nil {
		status := http.StatusForbidden
		if classified, ok := err.(*policy.Error); ok {
			status = classified.Status
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return false
	}
	c.Request.Method = http.MethodPut
	c.Request.Body = io.NopCloser(bytes.NewReader(encoded))
	c.Request.ContentLength = int64(len(encoded))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Del("Content-Length")
	return true
}

// serveConfig examines visibility and serves the same upstream response. For
// HEAD it obtains a GET internally so that visibility can still be checked.
func (a *Handler) serveConfig(c *gin.Context) {
	name := c.Param("name")
	response, err := a.upstream.Do(c.Request.Context(), http.MethodGet, apiPrefix+"/configs/"+url.PathEscape(name), nil, nil)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Config unavailable"})
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		c.Status(response.StatusCode)
		return
	}
	spool, err := os.CreateTemp("", "ebs-gateway-config-*")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Config unavailable"})
		return
	}
	defer os.Remove(spool.Name())
	defer spool.Close()
	if _, err := io.Copy(spool, response.Body); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Config unavailable"})
		return
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Config unavailable"})
		return
	}
	var object struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Visibility string `json:"visibility"`
		} `json:"spec"`
	}
	if err := json.NewDecoder(spool).Decode(&object); err != nil || object.Metadata.Name != name {
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid Config response"})
		return
	}
	if object.Spec.Visibility != "Public" && !principal(c).IsPrivileged() {
		c.JSON(http.StatusForbidden, gin.H{"error": "Config access denied"})
		return
	}
	if response.Header.Get("Content-Type") != "" {
		c.Header("Content-Type", response.Header.Get("Content-Type"))
	}
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Config unavailable"})
		return
	}
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, spool)
}
