package gateway

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/middleware"
	"ebs-gateway/internal/mutation"
	"ebs-gateway/internal/policy"
)

func (a *Handler) prepareProjectCreate(c *gin.Context) {
	route := policy.Route{Resource: "projects", Method: c.Request.Method}
	if !a.prepareCreate(c, middleware.Principal(c), route) {
		c.Abort()
		return
	}
	c.Next()
}

func (a *Handler) prepareProjectUpdate(c *gin.Context) {
	route := policy.Route{
		Resource: "projects",
		Name:     c.Param("project"),
		Method:   c.Request.Method,
	}
	if !a.prepareUpdate(c, middleware.Principal(c), route) {
		c.Abort()
		return
	}
	c.Next()
}

func (a *Handler) readWriteBody(c *gin.Context) ([]byte, bool) {
	defer c.Request.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, a.bodyLimit+1))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unable to read request"})
		return nil, false
	}
	if int64(len(raw)) > a.bodyLimit {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large"})
		return nil, false
	}
	return raw, true
}

func (a *Handler) prepareCreate(c *gin.Context, who identity.Principal, route policy.Route) bool {
	raw, ok := a.readWriteBody(c)
	if !ok {
		return false
	}
	if media := strings.TrimSpace(strings.Split(c.GetHeader("Content-Type"), ";")[0]); media != "application/json" {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": "application/json required"})
		return false
	}
	encoded, err := a.policy.PrepareCreate(c.Request.Context(), who, route, raw)
	if err != nil {
		c.JSON(policyErrorStatus(err), gin.H{"error": err.Error()})
		return false
	}
	setJSONBody(c.Request, encoded)
	return true
}

func (a *Handler) prepareUpdate(c *gin.Context, who identity.Principal, route policy.Route) bool {
	old := updateObject(c)
	if old == nil {
		response, err := a.upstream.Do(c.Request.Context(), http.MethodGet, c.Request.URL.Path, nil, nil)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "unable to read current object"})
			return false
		}
		defer response.Body.Close()
		if response.StatusCode == http.StatusNotFound {
			c.Status(http.StatusNotFound)
			return false
		}
		if response.StatusCode != http.StatusOK {
			c.JSON(http.StatusBadGateway, gin.H{"error": "unable to read current object"})
			return false
		}
		old, err = io.ReadAll(io.LimitReader(response.Body, a.bodyLimit+1))
		if err != nil || int64(len(old)) > a.bodyLimit {
			c.JSON(http.StatusBadGateway, gin.H{"error": "current object exceeds body limit"})
			return false
		}
	}
	incoming, ok := a.readWriteBody(c)
	if !ok {
		return false
	}
	encoded, previous, candidate, err := mutation.Prepare(old, incoming, c.Request.Method, c.GetHeader("Content-Type"), a.bodyLimit)
	if err != nil {
		status := http.StatusBadRequest
		if classified, ok := err.(*mutation.Error); ok {
			status = classified.Status
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return false
	}
	if err := a.policy.ValidateUpdate(c.Request.Context(), who, route, previous, candidate); err != nil {
		c.JSON(policyErrorStatus(err), gin.H{"error": err.Error()})
		return false
	}
	c.Request.Method = http.MethodPut
	setJSONBody(c.Request, encoded)
	return true
}

func setJSONBody(request *http.Request, encoded []byte) {
	request.Body = io.NopCloser(bytes.NewReader(encoded))
	request.ContentLength = int64(len(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Del("Content-Length")
}

func policyErrorStatus(err error) int {
	if classified, ok := err.(*policy.Error); ok {
		return classified.Status
	}
	return http.StatusForbidden
}
