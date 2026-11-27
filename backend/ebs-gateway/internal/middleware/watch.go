package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/policy"
)

// ValidateWatchRoute allows Watch only on explicitly registered GET list routes.
func ValidateWatchRoute(routes *RoutePolicies) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.FullPath()
		if path == "" || !policy.HasWatch(c.Request.URL.Query()) {
			c.Next()
			return
		}
		if routes.rules[RouteKey{Method: c.Request.Method, Path: path}].Watch {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "watch is not supported for this route"})
	}

}
