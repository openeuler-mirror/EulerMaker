package gateway

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/middleware"
	"ebs-gateway/internal/policy"
)

// runnerJobReadGuard verifies assignment before a Runner reads a named Job.
func (a *Handler) runnerJobReadGuard(c *gin.Context) {
	who := middleware.Principal(c)
	if who.Type == identity.RunnerType && !a.runnerOwnsJob(c, who.Subject) {
		c.Abort()
		return
	}
	c.Next()
}

func (a *Handler) runnerOwnsJob(c *gin.Context, runner string) bool {
	response, err := a.upstream.Do(c.Request.Context(), http.MethodGet, c.Request.URL.Path, nil, nil)
	if err != nil || response == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Job lookup unavailable"})
		return false
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		// Runner recovery needs to distinguish deleted Jobs from denied access.
		c.JSON(http.StatusNotFound, gin.H{"error": "Job not found"})
		return false
	}
	if response.StatusCode != http.StatusOK {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Job lookup unavailable"})
		return false
	}
	var object struct {
		Status struct {
			Runner string `json:"runner"`
		} `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, a.bodyLimit+1)).Decode(&object); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid Job response"})
		return false
	}
	if object.Status.Runner != runner {
		c.JSON(http.StatusForbidden, gin.H{"error": "Job is not assigned to Runner"})
		return false
	}
	return true
}

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
