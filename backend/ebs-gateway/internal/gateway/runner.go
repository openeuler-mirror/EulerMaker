package gateway

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/middleware"
	"ebs-gateway/internal/policy"
)

func (a *Handler) prepareRunnerCreate(c *gin.Context) {
	route := policy.Route{Resource: "runners", Method: http.MethodPost}
	if !a.prepareCreate(c, middleware.Principal(c), route) {
		c.Abort()
		return
	}
	c.Next()
}

func (a *Handler) prepareRunnerUpdate(c *gin.Context) {
	route := policy.Route{
		Resource: "runners",
		Name:     c.Param("runner"),
		Method:   c.Request.Method,
	}
	if !a.prepareUpdate(c, middleware.Principal(c), route) {
		c.Abort()
		return
	}
	c.Next()
}

// prepareRunnerJobs validates assignment queries and holds the watch slot until forwarding ends.
func (a *Handler) prepareRunnerJobs(c *gin.Context) {
	who := middleware.Principal(c)
	if !validRunnerJobsQuery(c.Request.URL.Query()) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "unsupported Runner Jobs query"})
		return
	}
	if c.Query("watch") == "true" {
		if !a.acquireRunnerWatch(who.Subject) {
			middleware.AbortRateLimited(c, 5, "too many Runner watches")
			return
		}
		defer a.releaseRunnerWatch(who.Subject)
	}
	c.Next()
}

func validRunnerJobsQuery(query url.Values) bool {
	for name, values := range query {
		if len(values) != 1 {
			return false
		}
		value := values[0]
		switch name {
		case "watch", "allowWatchBookmarks":
			if value != "true" && value != "false" {
				return false
			}
		case "timeoutSeconds":
			seconds, err := strconv.Atoi(value)
			if err != nil || seconds < 1 || seconds > 300 {
				return false
			}
		case "resourceVersionMatch":
			if value != "Exact" && value != "NotOlderThan" {
				return false
			}
		case "limit":
			limit, err := strconv.Atoi(value)
			if err != nil || limit < 1 || limit > 500 {
				return false
			}
		case "resourceVersion", "continue", "labelSelector", "includeFields", "excludeFields":
			continue
		default:
			return false
		}
	}
	return true
}

func (a *Handler) acquireRunnerWatch(name string) bool {
	a.runnerWatchMu.Lock()
	defer a.runnerWatchMu.Unlock()
	if a.runnerWatches.Has(name) {
		return false
	}
	a.runnerWatches.Insert(name)
	return true
}

func (a *Handler) releaseRunnerWatch(name string) {
	a.runnerWatchMu.Lock()
	defer a.runnerWatchMu.Unlock()
	a.runnerWatches.Delete(name)
}
