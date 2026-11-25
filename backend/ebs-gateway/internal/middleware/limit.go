package middleware

import (
	"net/http"
	"strconv"

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
	AbortRateLimited(c, 1, "too many requests")
	return false
}

// AbortRateLimited ends the request with a 429 and the caller's retry delay.
func AbortRateLimited(c *gin.Context, retryAfterSeconds int, message string) {
	c.Header("Retry-After", strconv.Itoa(retryAfterSeconds))
	c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": message})
}
