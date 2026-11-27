package gateway

import (
	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/middleware"
	"ebs-gateway/internal/policy"
)

func (a *Handler) prepareJobStatusUpdate(c *gin.Context) {
	route := policy.Route{
		Resource:    "jobs",
		Project:     c.Param("project"),
		Name:        c.Param("name"),
		Subresource: "status",
		Method:      c.Request.Method,
	}
	if !a.prepareUpdate(c, middleware.Principal(c), route) {
		c.Abort()
		return
	}
	c.Next()
}
