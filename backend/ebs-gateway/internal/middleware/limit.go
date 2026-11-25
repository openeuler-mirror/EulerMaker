package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/limit"
)

// LimitAuthenticated applies the shared per-identity and client-IP request limit.
func LimitAuthenticated(limits *limit.Buckets) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !AllowAuthenticated(c, limits) {
			return
		}
		c.Next()
	}
}

// AllowAuthenticated consumes one request from the same bucket used by LimitAuthenticated.
func AllowAuthenticated(c *gin.Context, limits *limit.Buckets) bool {
	if limits.Allow(Principal(c).Subject + "/" + c.ClientIP()) {
		return true
	}
	c.Header("Retry-After", "1")
	c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "too many requests"})
	return false
}
