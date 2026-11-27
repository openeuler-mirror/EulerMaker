package gateway

import (
	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/middleware"
	"ebs-gateway/internal/policy"
)

const updateObjectKey = "ebs-gateway.authorizedUpdateObject"

// authorizeResource adapts an explicit policy rule to the resource request.
func (a *Handler) authorizeResource(rule policy.Rule) gin.HandlerFunc {
	return func(c *gin.Context) {
		who := middleware.Principal(c)
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
		result, err := a.policy.Authorize(c.Request.Context(), who, request, rule)
		if err != nil {
			c.AbortWithStatusJSON(policyErrorStatus(err), gin.H{"error": err.Error()})
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

// updateObject returns the full object already read during update authorization.
func updateObject(c *gin.Context) []byte {
	value, _ := c.Get(updateObjectKey)
	raw, _ := value.([]byte)
	return raw
}
