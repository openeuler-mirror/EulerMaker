package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/middleware"
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
		Resource:    r.resource,
		Project:     c.Param(r.projectKey),
		Name:        c.Param(r.nameKey),
		Subresource: r.subresource,
		Method:      c.Request.Method,
	}
}

// ResourceHandlers returns the guard and final handler for one registered resource path.
func (a *Handler) ResourceHandlers(resource, projectKey, nameKey, subresource string) (gin.HandlerFunc, gin.HandlerFunc) {
	route := resourceRoute{
		resource:    resource,
		projectKey:  projectKey,
		nameKey:     nameKey,
		subresource: subresource,
	}
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
		if !a.authenticateResource(c, public) {
			return
		}
		who := middleware.Principal(c)
		if who.Type == identity.ServiceType || (who.Type == identity.RunnerType && resolved.Resource != "configs") {
			public = false
		}
		if who.Type == identity.ServiceType {
			prepareServiceAudit(c, resolved)
		}
		if !validateResourceRoute(c, resolved) {
			return
		}
		if public {
			if !a.preparePublicRead(c, resolved) {
				return
			}
		} else if !a.authorizeResource(c, who, resolved) {
			return
		}
		c.Next()
	}
}

func (a *Handler) authenticateResource(c *gin.Context, public bool) bool {
	if c.GetHeader("Authorization") == "" && public {
		c.Set("anonymous", true)
		return true
	}
	if !a.auth.Verify(c) {
		return false
	}
	return a.auth.ConfirmUser(c, middleware.Principal(c))
}

func prepareServiceAudit(c *gin.Context, route policy.Route) {
	project, permission := policy.ServiceOperation(route)
	if project == "" {
		project = route.Project
		if route.Resource == "projects" {
			project = route.Name
		}
	}
	c.Set("serviceProject", project)
	c.Set("servicePermission", string(permission))
	c.Set("serviceAuthorization", "denied")
}

func validateResourceRoute(c *gin.Context, route policy.Route) bool {
	switch route.Resource {
	case "projects":
		return validateProjectRoute(c, route)
	case "runners":
		return validateRunnerRoute(c, route)
	case "builds":
		return validateBuildRoute(c, route)
	case "jobs":
		return validateJobRoute(c, route)
	case "configs":
		return validateConfigRoute(c, route)
	case "scripts":
		return validateScriptRoute(c, route)
	case "snapshots", "buildinfos", "buildgraphs", "rpmrepos":
		return validateReadOnlyRoute(c, route)
	case "oldjobs":
		return validateOldJobRoute(c, route)
	default:
		return validateRouteMethods(c, route, false)
	}
}

func validateProjectRoute(c *gin.Context, route policy.Route) bool {
	switch {
	case route.Name == "" && route.Subresource == "":
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodHead, http.MethodPost)
	case route.Name != "" && (route.Subresource == "" || route.Subresource == "status"):
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch)
	default:
		return validateRouteMethods(c, route, false)
	}
}

func validateRunnerRoute(c *gin.Context, route policy.Route) bool {
	switch {
	case route.Name == "" && route.Subresource == "":
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodHead, http.MethodPost)
	case route.Name != "" && route.Subresource == "":
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete)
	case route.Name != "" && route.Subresource == "status":
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch)
	case route.Name != "" && route.Subresource == "jobs":
		return validateRouteMethods(c, route, route.Project == "", http.MethodGet)
	default:
		return validateRouteMethods(c, route, false)
	}
}

func validateBuildRoute(c *gin.Context, route policy.Route) bool {
	switch {
	case route.Name == "" && route.Subresource == "":
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodHead, http.MethodPost)
	case route.Name != "" && route.Subresource == "":
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete)
	case route.Name != "" && route.Subresource == "status":
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch)
	case route.Name != "" && route.Subresource == "abort":
		return validateRouteMethods(c, route, false, http.MethodPost)
	default:
		return validateRouteMethods(c, route, false)
	}
}

func validateJobRoute(c *gin.Context, route policy.Route) bool {
	switch {
	case route.Name == "" && route.Subresource == "":
		return validateRouteMethods(c, route, route.Project != "" && route.Method == http.MethodGet,
			http.MethodGet, http.MethodHead, http.MethodPost)
	case route.Name != "" && route.Subresource == "":
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete)
	case route.Name != "" && route.Subresource == "status":
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch)
	case route.Name != "" && route.Subresource == "abort":
		return validateRouteMethods(c, route, false, http.MethodPost)
	default:
		return validateRouteMethods(c, route, false)
	}
}

func validateConfigRoute(c *gin.Context, route policy.Route) bool {
	switch {
	case route.Subresource != "":
		return validateRouteMethods(c, route, false)
	case route.Name == "":
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodHead, http.MethodPost)
	default:
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete)
	}
}

func validateScriptRoute(c *gin.Context, route policy.Route) bool {
	switch {
	case route.Subresource != "":
		return validateRouteMethods(c, route, false)
	case route.Name == "":
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodPost)
	default:
		return validateRouteMethods(c, route, false, http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete)
	}
}

func validateReadOnlyRoute(c *gin.Context, route policy.Route) bool {
	if route.Subresource == "" || (route.Name != "" && route.Subresource == "status") {
		return validateRouteMethods(c, route, false, http.MethodGet)
	}
	return validateRouteMethods(c, route, false)
}

func validateOldJobRoute(c *gin.Context, route policy.Route) bool {
	if route.Subresource == "" {
		return validateRouteMethods(c, route, false, http.MethodGet)
	}
	return validateRouteMethods(c, route, false)
}

func validateRouteMethods(c *gin.Context, route policy.Route, watchAllowed bool, methods ...string) bool {
	if !slices.Contains(methods, route.Method) {
		c.Header("Allow", strings.Join(methods, ", "))
		c.AbortWithStatus(http.StatusMethodNotAllowed)
		return false
	}
	if policy.HasWatch(c.Request.URL.Query()) && !watchAllowed {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "watch is not supported for this route"})
		return false
	}
	return true
}

func (a *Handler) preparePublicRead(c *gin.Context, route policy.Route) bool {
	if !a.publicLimit.Allow(c.ClientIP()) {
		c.Header("Retry-After", "1")
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "too many requests"})
		return false
	}
	if route.Name == "" && route.Subresource == "" {
		query := c.Request.URL.Query()
		limit, err := strconv.Atoi(query.Get("limit"))
		if err != nil || limit < 1 || limit > 100 || len(query["limit"]) != 1 {
			query.Set("limit", "100")
			c.Request.URL.RawQuery = query.Encode()
		}
	}
	c.Set("public", true)
	return true
}

func (a *Handler) authorizeResource(c *gin.Context, who identity.Principal, route policy.Route) bool {
	if !a.limits.Allow(who.Subject + "/" + c.ClientIP()) {
		c.Header("Retry-After", "1")
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "too many requests"})
		return false
	}
	if err := a.policy.Authorize(c.Request.Context(), who, route); err != nil {
		status := http.StatusForbidden
		if classified, ok := err.(*policy.Error); ok {
			status = classified.Status
		}
		c.AbortWithStatusJSON(status, gin.H{"error": err.Error()})
		return false
	}
	if who.Type == identity.ServiceType {
		c.Set("serviceAuthorization", "allowed")
	}
	return true
}

func (a *Handler) forwardResource(route resourceRoute) gin.HandlerFunc {
	return func(c *gin.Context) {
		resolved := route.at(c)
		who := middleware.Principal(c)
		switch resolved.Resource {
		case "configs":
			if resolved.Name != "" && (resolved.Method == http.MethodGet || resolved.Method == http.MethodHead) {
				a.serveConfig(c)
				return
			}
		case "runners":
			a.forwardRunnerResource(c, who, resolved)
			return
		case "jobs":
			a.forwardJobResource(c, who, resolved)
			return
		}
		a.forwardRequest(c, who, resolved)
	}
}

func (a *Handler) forwardRunnerResource(c *gin.Context, who identity.Principal, route policy.Route) {
	if who.Type == identity.RunnerType && route.Subresource == "jobs" {
		if !validateRunnerJobsRequest(c) {
			return
		}
		if c.Query("watch") == "true" {
			if !a.acquireRunnerWatch(who.Runner) {
				c.Header("Retry-After", "5")
				c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many Runner watches"})
				return
			}
			defer a.releaseRunnerWatch(who.Runner)
		}
	}
	a.forwardRequest(c, who, route)
}

func (a *Handler) forwardJobResource(c *gin.Context, who identity.Principal, route policy.Route) {
	if who.Type == identity.RunnerType && (route.Method == http.MethodGet || route.Method == http.MethodHead) {
		if !a.runnerOwnsJob(c, who.Runner) {
			return
		}
	}
	a.forwardRequest(c, who, route)
}

func (a *Handler) forwardRequest(c *gin.Context, who identity.Principal, route policy.Route) {
	if route.Method == http.MethodPost && route.Subresource == "" {
		if !a.prepareCreate(c, who, route) {
			return
		}
	}
	if (route.Method == http.MethodPost && route.Subresource == "abort") || route.Method == http.MethodDelete {
		if !a.limitWriteBody(c) {
			return
		}
	}
	if route.Method == http.MethodPut || route.Method == http.MethodPatch {
		if !a.prepareUpdate(c, who, route) {
			return
		}
	}
	a.upstream.Forward(c, c.GetBool("public"))
}

func validateRunnerJobsRequest(c *gin.Context) bool {
	if !validRunnerJobsQuery(c.Request.URL.Query()) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "unsupported Runner Jobs query"})
		return false
	}
	return true
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
		case "continue", "labelSelector", "includeFields", "excludeFields":
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
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Job lookup unavailable"})
		return false
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		// Runner recovery needs to distinguish deleted Jobs from denied access.
		if c.Request.Method == http.MethodHead {
			c.Status(http.StatusNotFound)
		} else {
			c.JSON(http.StatusNotFound, gin.H{"error": "Job not found"})
		}
		return false
	}
	if response.StatusCode != http.StatusOK {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Job lookup unavailable"})
		return false
	}
	var object struct {
		Status struct {
			Runner string `json:"runner"`
		} `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, a.bodyLimit+1)).Decode(&object); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid Job response"})
		return false
	}
	if object.Status.Runner != runner {
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
	if object.Spec.Visibility != "Public" && !middleware.Principal(c).IsPrivileged() {
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
