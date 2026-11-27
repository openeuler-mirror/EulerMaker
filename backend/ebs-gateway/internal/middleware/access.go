package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/identity"
)

// AdmitIdentity enforces route-level identity access after authentication.
func AdmitIdentity(routes *RoutePolicies) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.FullPath()
		if path == "" {
			c.Next() // Preserve the router's handling of unregistered paths.
			return
		}
		who := Principal(c)
		rule, exists := routes.rules[RouteKey{Method: c.Request.Method, Path: path}]
		if !exists || rule.Identities&principalIdentity(who) == 0 {
			if who.Type == identity.ServiceType {
				c.Set("serviceProject", c.Param("project"))
				c.Set("serviceAuthorization", "denied")
			}
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "identity is not allowed for this route"})
			return
		}
		c.Next()
	}

}

func principalIdentity(who identity.Principal) RouteIdentities {
	if who.Type == "" && who.Subject == "" {
		return AllowAnonymous
	}
	if who.Subject == "" {
		return 0
	}
	switch who.Type {
	case identity.UserType:
		switch who.Scope {
		case identity.UserScope:
			return AllowUser
		case identity.OpsScope:
			return AllowOps
		case identity.AdminScope:
			return AllowAdmin
		}
	case identity.RunnerType:
		return AllowRunner
	case identity.ServiceType:
		return AllowService
	}
	return 0
}
