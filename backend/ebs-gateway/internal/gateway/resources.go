package gateway

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/middleware"
	"ebs-gateway/internal/policy"
)

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
		c.Next()
	}
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
