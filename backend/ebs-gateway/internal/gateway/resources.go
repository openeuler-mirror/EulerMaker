package gateway

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/middleware"
	"ebs-gateway/internal/policy"
)

const resourceAccessKey = "ebs-gateway.resourceAccess"

type resourceCheck uint8

const (
	noPendingCheck resourceCheck = iota
	runnerJobReadCheck
)

type resourceAccess struct {
	pending resourceCheck
}

func (a *Handler) projectGuard(subresource string) gin.HandlerFunc {
	return func(c *gin.Context) {
		a.authorizeAuthenticatedRoute(c, policy.Route{
			Resource:    "projects",
			Name:        c.Param("project"),
			Subresource: subresource,
			Method:      c.Request.Method,
		})
	}
}

func (a *Handler) publicReadGuard(resource, subresource string) gin.HandlerFunc {
	return func(c *gin.Context) {
		route := policy.Route{
			Resource:    resource,
			Subresource: subresource,
			Method:      c.Request.Method,
		}
		if resource == "projects" {
			route.Name = c.Param("project")
		} else {
			route.Project = c.Param("project")
			route.Name = c.Param("name")
		}
		if !policy.IsPublicRead(route) {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "invalid public read route"})
			return
		}
		watch := policy.HasWatch(c.Request.URL.Query())
		who := middleware.Principal(c)
		if watch || (who.Type != "" && !who.IsUser()) {
			a.authorizeAuthenticatedRoute(c, route)
			return
		}
		preparePublicRead(c, route)
		allowResource(c, route)
	}
}

func (a *Handler) projectScopedGuard(resource, subresource string) gin.HandlerFunc {
	return func(c *gin.Context) {
		a.authorizeAuthenticatedRoute(c, policy.Route{
			Resource:    resource,
			Project:     c.Param("project"),
			Name:        c.Param("name"),
			Subresource: subresource,
			Method:      c.Request.Method,
		})
	}
}

func requiredResourceCheck(route policy.Route) resourceCheck {
	switch {
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

func (a *Handler) authorizeAuthenticatedRoute(c *gin.Context, route policy.Route) {
	who := middleware.Principal(c)
	if who.Type == "" {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "missing authenticated principal"})
		return
	}
	if who.Type == identity.ServiceType {
		prepareServiceAudit(c, route)
	}
	if a.authorizeResource(c, who, route) {
		allowResource(c, route)
	}
}

func allowResource(c *gin.Context, route policy.Route) {
	c.Set(resourceAccessKey, &resourceAccess{pending: requiredResourceCheck(route)})
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

func preparePublicRead(c *gin.Context, route policy.Route) {
	if route.Name == "" && route.Subresource == "" {
		query := c.Request.URL.Query()
		limit, err := strconv.Atoi(query.Get("limit"))
		if err != nil || limit < 1 || limit > 100 || len(query["limit"]) != 1 {
			query.Set("limit", "100")
			c.Request.URL.RawQuery = query.Encode()
		}
	}
}

func (a *Handler) authorizeResource(c *gin.Context, who identity.Principal, route policy.Route) bool {
	if err := a.policy.Authorize(c.Request.Context(), who, route); err != nil {
		if who.Type == identity.ServiceType {
			c.Set("serviceAuthorization", "denied")
		}
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
	a.upstream.Forward(c)
}
