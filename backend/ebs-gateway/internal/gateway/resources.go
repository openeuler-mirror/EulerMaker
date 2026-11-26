package gateway

import (
	"context"
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

// These modes select the public-read path; other callers go through authorization.
type accessMode uint8

const (
	authenticatedOnly accessMode = iota
	publicRead
)

type resourceRule func(context.Context, identity.Principal, policy.Route) error

func (a *Handler) scriptGuard(c *gin.Context) {
	route := policy.Route{Resource: "scripts", Name: c.Param("name"), Method: c.Request.Method}
	a.checkResourceAccess(c, route, authenticatedOnly, policy.AuthorizeScript, noPendingCheck)
}

// resourceGuard authorizes the route and records any check still required before forwarding.
func (a *Handler) resourceGuard(resource, projectKey, nameKey, subresource string) gin.HandlerFunc {
	return func(c *gin.Context) {
		resolved := policy.Route{
			Resource:    resource,
			Project:     c.Param(projectKey),
			Name:        c.Param(nameKey),
			Subresource: subresource,
			Method:      c.Request.Method,
		}
		mode := authenticatedOnly
		if policy.IsPublicRead(resolved) {
			mode = publicRead
		}
		a.checkResourceAccess(c, resolved, mode, a.policy.Authorize, requiredResourceCheck(resolved))
	}
}

func requiredResourceCheck(route policy.Route) resourceCheck {
	switch {
	case route.Resource == "runners" && route.Subresource == "jobs" && route.Method == http.MethodGet:
		return runnerJobsListCheck
	case route.Resource == "jobs" && route.Name != "" && route.Subresource == "" && route.Method == http.MethodGet:
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

func (a *Handler) checkResourceAccess(c *gin.Context, route policy.Route, mode accessMode, rule resourceRule, pending resourceCheck) {
	watch := policy.HasWatch(c.Request.URL.Query())
	allowPublic := !watch && mode != authenticatedOnly
	if !allowPublic || c.GetHeader("Authorization") != "" {
		if !a.auth.VerifyAccess(c) {
			return
		}
	}
	who := middleware.Principal(c)
	if who.Type == identity.ServiceType {
		prepareServiceAudit(c, route)
	}
	if allowPublic && (who.Type == "" || who.IsUser()) {
		if !a.preparePublicRead(c, route) {
			return
		}
	} else if !a.authorizeResource(c, who, route, rule) {
		return
	}
	c.Set(resourceAccessKey, &resourceAccess{route: route, pending: pending})
	c.Next()
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
		middleware.AbortRateLimited(c, 1, "too many requests")
		return false
	}
	if route.Resource != "build-targets" && route.Name == "" && route.Subresource == "" {
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

func (a *Handler) authorizeResource(c *gin.Context, who identity.Principal, route policy.Route, rule resourceRule) bool {
	if !middleware.AllowAuthenticated(c, a.limits) {
		return false
	}
	if err := rule(c.Request.Context(), who, route); err != nil {
		c.AbortWithStatusJSON(policyErrorStatus(err), gin.H{"error": err.Error()})
		return false
	}
	if who.Type == identity.ServiceType {
		c.Set("serviceAuthorization", "allowed")
	}
	return true
}

func policyErrorStatus(err error) int {
	if classified, ok := err.(*policy.Error); ok {
		return classified.Status
	}
	return http.StatusForbidden
}

// forwardResource only forwards after all required guards have passed.
func (a *Handler) forwardResource(c *gin.Context) {
	access := requireResourceCheck(c, noPendingCheck)
	if access == nil {
		return
	}
	a.forwardRequest(c, middleware.Principal(c), access.route)
}
