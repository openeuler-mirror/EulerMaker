package middleware

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/limit"
)

// LimitIP bounds requests before token verification and account lookups.
func LimitIP(limits *limit.Buckets) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !limits.Allow(c.ClientIP()) {
			AbortRateLimited(c, 1, "too many requests")
			return
		}
		c.Next()
	}
}

// LimitIdentity uses separate keys for authenticated identities and anonymous clients.
func LimitIdentity(limits *limit.Buckets) gin.HandlerFunc {
	return func(c *gin.Context) {
		who := Principal(c)
		key := "anonymous/" + c.ClientIP()
		if who.Type != "" {
			key = string(who.Type) + "/" + who.Subject + "/" + c.ClientIP()
		}
		if !limits.Allow(key) {
			AbortRateLimited(c, 1, "too many requests")
			return
		}
		c.Next()
	}
}

// AbortRateLimited ends the request with a 429 and the caller's retry delay.
func AbortRateLimited(c *gin.Context, retryAfterSeconds int, message string) {
	c.Header("Retry-After", strconv.Itoa(retryAfterSeconds))
	c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": message})
}
