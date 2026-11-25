package handler

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/middleware"
)

// RunnerJobsListGuard checks Runner list queries and holds the watch slot until forwarding ends.
func (a *Handler) RunnerJobsListGuard(c *gin.Context) {
	access := requireResourceCheck(c, runnerJobsListCheck)
	if access == nil {
		return
	}
	who := middleware.Principal(c)
	if who.Type == identity.RunnerType {
		if !validRunnerJobsQuery(c.Request.URL.Query()) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "unsupported Runner Jobs query"})
			return
		}
		if c.Query("watch") == "true" {
			if !a.acquireRunnerWatch(who.Runner) {
				c.Header("Retry-After", "5")
				c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "too many Runner watches"})
				return
			}
			defer a.releaseRunnerWatch(who.Runner)
		}
	}
	access.pending = noPendingCheck
	c.Next()
}

func validRunnerJobsQuery(query url.Values) bool {
	for name, values := range query {
		switch name {
		case "watch", "allowWatchBookmarks":
			if len(values) != 1 || (values[0] != "true" && values[0] != "false") {
				return false
			}
		case "timeoutSeconds":
			if len(values) != 1 {
				return false
			}
			seconds, err := strconv.Atoi(values[0])
			if err != nil || seconds < 1 || seconds > 300 {
				return false
			}
		case "resourceVersion":
			if len(values) != 1 {
				return false
			}
		case "resourceVersionMatch":
			if len(values) != 1 || (values[0] != "Exact" && values[0] != "NotOlderThan") {
				return false
			}
		case "limit":
			if len(values) != 1 {
				return false
			}
			limit, err := strconv.Atoi(values[0])
			if err != nil || limit < 1 || limit > 500 {
				return false
			}
		case "continue", "labelSelector", "includeFields", "excludeFields":
			if len(values) != 1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (a *Handler) acquireRunnerWatch(name string) bool {
	a.runnerWatchMu.Lock()
	defer a.runnerWatchMu.Unlock()
	if a.runnerWatches[name] >= 1 {
		return false
	}
	a.runnerWatches[name]++
	return true
}

func (a *Handler) releaseRunnerWatch(name string) {
	a.runnerWatchMu.Lock()
	defer a.runnerWatchMu.Unlock()
	delete(a.runnerWatches, name)
}
