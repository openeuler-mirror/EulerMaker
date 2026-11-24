package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/limit"
)

// LimitAuthenticated applies the shared per-identity and client-IP request limit.
func LimitAuthenticated(limits *limit.Buckets) gin.HandlerFunc {
	return func(c *gin.Context) {
		user := Principal(c)
		if !limits.Allow(user.Subject + "/" + c.ClientIP()) {
			c.Header("Retry-After", "1")
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "too many requests"})
			return
		}
		c.Next()
	}
}
