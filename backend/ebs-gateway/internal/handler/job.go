package handler

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/policy"
)

func (a *Handler) forwardJobResource(c *gin.Context, who identity.Principal, route policy.Route) {
	if who.Type == identity.RunnerType && (route.Method == http.MethodGet || route.Method == http.MethodHead) {
		if !a.runnerOwnsJob(c, who.Runner) {
			return
		}
	}
	a.forwardRequest(c, who, route)
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
		if c.Request.Method == http.MethodHead {
			c.Status(http.StatusNotFound)
		} else {
			c.JSON(http.StatusNotFound, gin.H{"error": "Job not found"})
		}
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
