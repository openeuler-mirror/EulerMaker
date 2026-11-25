package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/middleware"
	"ebs-gateway/internal/policy"
)

const (
	apiPrefix         = "/apis/ebs/v1"
	resourceAccessKey = "ebs-gateway.resourceAccess"
)

type resourceCheck uint8

const (
	noPendingCheck resourceCheck = iota
	runnerJobsListCheck
	runnerJobReadCheck
)

type resourceAccess struct {
	route   policy.Route
	pending resourceCheck
}

// ResourceGuard authorizes the route and records any check still required before forwarding.
func (a *Handler) ResourceGuard(resource, projectKey, nameKey, subresource string) gin.HandlerFunc {
	return func(c *gin.Context) {
		resolved := policy.Route{
			Resource:    resource,
			Project:     c.Param(projectKey),
			Name:        c.Param(nameKey),
			Subresource: subresource,
			Method:      c.Request.Method,
		}
		watch := policy.HasWatch(c.Request.URL.Query())
		allowAnonymous := !watch && (policy.IsPublicRead(resolved) || isNamedConfigRead(resolved))
		if !a.authenticateResource(c, allowAnonymous) {
			return
		}
		who := middleware.Principal(c)
		if who.Type == identity.ServiceType {
			prepareServiceAudit(c, resolved)
		}
		if watch && !supportsWatch(resolved) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "watch is not supported for this route"})
			return
		}
		if allowAnonymous && canUsePublicRead(who, resolved) {
			if !a.preparePublicRead(c, resolved) {
				return
			}
		} else if !a.authorizeResource(c, who, resolved) {
			return
		}
		c.Set(resourceAccessKey, &resourceAccess{route: resolved, pending: requiredResourceCheck(resolved)})
		c.Next()
	}
}

func requiredResourceCheck(route policy.Route) resourceCheck {
	switch {
	case route.Resource == "runners" && route.Subresource == "jobs" && route.Method == http.MethodGet:
		return runnerJobsListCheck
	case route.Resource == "jobs" && route.Name != "" && route.Subresource == "" &&
		(route.Method == http.MethodGet || route.Method == http.MethodHead):
		return runnerJobReadCheck
	default:
		return noPendingCheck
	}
}

func requireResourceCheck(c *gin.Context, expected resourceCheck) *resourceAccess {
	value, _ := c.Get(resourceAccessKey)
	access, ok := value.(*resourceAccess)
	if !ok || access == nil || access.pending != expected {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "invalid resource guard chain"})
		return nil
	}
	return access
}

func supportsWatch(route policy.Route) bool {
	if route.Method != http.MethodGet {
		return false
	}
	switch route.Resource {
	case "jobs":
		return route.Project != "" && route.Name == "" && route.Subresource == ""
	case "runners":
		return route.Project == "" && route.Name != "" && route.Subresource == "jobs"
	default:
		return false
	}
}

func canUsePublicRead(who identity.Principal, route policy.Route) bool {
	switch who.Type {
	case identity.ServiceType:
		return false
	case identity.RunnerType:
		return isNamedConfigRead(route)
	default:
		return true
	}
}

func (a *Handler) authenticateResource(c *gin.Context, allowAnonymous bool) bool {
	if c.GetHeader("Authorization") == "" && allowAnonymous {
		c.Set("anonymous", true)
		return true
	}
	return a.auth.VerifyAccess(c)
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

// ForwardResource only forwards after all required guards have passed.
func (a *Handler) ForwardResource(c *gin.Context) {
	access := requireResourceCheck(c, noPendingCheck)
	if access == nil {
		return
	}
	route := access.route
	if isNamedConfigRead(route) {
		a.serveConfig(c)
		return
	}
	a.forwardRequest(c, middleware.Principal(c), route)
}
