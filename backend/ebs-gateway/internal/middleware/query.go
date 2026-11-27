package middleware

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/policy"
)

// PrepareQuery validates Watch support and normalizes pagination for ordinary GET lists.
func PrepareQuery(routes *RoutePolicies) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.FullPath()
		if path == "" {
			c.Next()
			return
		}
		options := routes.rules[RouteKey{Method: c.Request.Method, Path: path}].Options
		query := c.Request.URL.Query()
		if policy.HasWatch(query) {
			if !options.Watch {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "watch is not supported for this route"})
				return
			}
			c.Next()
			return
		}
		if maximum := options.ListLimit; c.Request.Method == http.MethodGet && maximum > 0 {
			limit, err := strconv.Atoi(query.Get("limit"))
			if err != nil || limit < 1 || limit > maximum || len(query["limit"]) != 1 {
				query.Set("limit", strconv.Itoa(maximum))
				c.Request.URL.RawQuery = query.Encode()
			}
		}
		c.Next()
	}
}
