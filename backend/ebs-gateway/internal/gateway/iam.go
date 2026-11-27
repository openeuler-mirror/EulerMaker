package gateway

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	iamv1 "ebs-api/iam/v1"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/middleware"
	"ebs-gateway/internal/policy"
)

func (a *Handler) prepareUserUpdate(c *gin.Context) {
	route := policy.Route{Resource: "users", Name: c.Param("name"), Method: c.Request.Method}
	if !a.prepareUpdate(c, middleware.Principal(c), route) {
		c.Abort()
		return
	}
	c.Next()
}

func (a *Handler) protectAdminUser(c *gin.Context) {
	response, err := a.upstream.Do(c.Request.Context(), http.MethodGet, c.Request.URL.Path+"?includeFields=spec.scopes", nil, nil)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": "User unavailable"})
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		c.AbortWithStatus(response.StatusCode)
		return
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, a.bodyLimit+1))
	if err != nil || int64(len(data)) > a.bodyLimit {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": "invalid User response"})
		return
	}
	var object iamv1.User
	if err := json.Unmarshal(data, &object); err != nil {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": "invalid User response"})
		return
	}
	if len(object.Spec.Scopes) != 1 || object.Spec.Scopes[0] == string(identity.AdminScope) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Admin User is protected"})
		return
	}
	c.Next()
}
