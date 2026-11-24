package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/middleware"
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
