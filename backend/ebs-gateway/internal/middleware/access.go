package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/policy"
)

// AdmitIdentity checks allowed identities without business resource authorization.
func AdmitIdentity(allowed policy.RouteIdentities) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !allowed.Allows(Principal(c)) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "identity is not allowed for this route"})
			return
		}
		c.Next()
	}
}

// Authorize adapts the policy decision to HTTP responses, audit fields and request-local data.
func Authorize(routes *RoutePolicies, authorizer *policy.Authorizer) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.FullPath()
		if path == "" {
			c.Next()
			return
		}
		who := Principal(c)
		rule := routes.rules[RouteKey{Method: c.Request.Method, Path: path}].Access.Authorization
		request := policy.Request{
			Method:  c.Request.Method,
			Project: c.Param("project"),
			Runner:  c.Param("runner"),
			Path:    c.Request.URL.Path,
			Watch:   policy.HasWatch(c.Request.URL.Query()),
		}
		if who.Type == identity.ServiceType {
			c.Set("serviceProject", request.Project)
			c.Set("servicePermission", string(rule.ServicePermission))
			c.Set("serviceAuthorization", "denied")
		}
		result, err := authorizer.Authorize(c.Request.Context(), who, request, rule)
		if err != nil {
			status := http.StatusForbidden
			if classified, ok := err.(*policy.Error); ok {
				status = classified.Status
			}
			c.AbortWithStatusJSON(status, gin.H{"error": err.Error()})
			return
		}
		if who.Type == identity.ServiceType {
			c.Set("serviceAuthorization", "allowed")
		}
		if result.UpdateObject != nil {
			c.Set(updateObjectKey, result.UpdateObject)
		}
		c.Next()
	}
}

const updateObjectKey = "ebs-gateway.authorizedUpdateObject"

// UpdateObject returns the full object already read during update authorization.
func UpdateObject(c *gin.Context) []byte {
	value, _ := c.Get(updateObjectKey)
	raw, _ := value.([]byte)
	return raw
}
