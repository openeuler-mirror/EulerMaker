package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/identity"
)

// AdmitIdentity enforces route-level identity access after authentication.
func AdmitIdentity(c *gin.Context) {
	path := c.FullPath()
	if path == "" {
		c.Next() // Preserve the router's handling of unregistered paths.
		return
	}
	who := Principal(c)
	rule, exists := routePolicies[routeKey{Method: c.Request.Method, Path: path}]
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

func principalIdentity(who identity.Principal) routeIdentities {
	if who.Type == "" && who.Subject == "" {
		return allowAnonymous
	}
	if who.Subject == "" {
		return 0
	}
	switch who.Type {
	case identity.UserType:
		switch who.Scope {
		case identity.UserScope:
			return allowUser
		case identity.OpsScope:
			return allowOps
		case identity.AdminScope:
			return allowAdmin
		}
	case identity.RunnerType:
		return allowRunner
	case identity.ServiceType:
		return allowService
	}
	return 0
}
