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

func (a *Handler) users(c *gin.Context) {
	switch c.Request.Method {
	case http.MethodGet:
		if c.Param("name") == "" {
			a.listOrdinaryUsers(c)
			return
		}
		data, ok := a.readOrdinaryUser(c)
		if !ok {
			return
		}
		c.Data(http.StatusOK, "application/json", data)
		return
	case http.MethodDelete:
		if _, ok := a.readOrdinaryUser(c); !ok {
			return
		}
	}
	a.forwardIAM(c, "users")
}

func (a *Handler) machineAccounts(c *gin.Context) {
	a.forwardIAM(c, "machineaccounts")
}

func (a *Handler) forwardIAM(c *gin.Context, resource string) {
	if method := c.Request.Method; method == http.MethodPut || method == http.MethodPatch {
		route := policy.Route{Resource: resource, Name: c.Param("name"), Method: method}
		if !a.prepareUpdate(c, middleware.Principal(c), route) {
			return
		}
	}
	a.upstream.Forward(c)
}

func (a *Handler) readOrdinaryUser(c *gin.Context) ([]byte, bool) {
	response, err := a.upstream.Do(c.Request.Context(), http.MethodGet, c.Request.URL.Path, nil, nil)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "User unavailable"})
		return nil, false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		c.Status(response.StatusCode)
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, a.bodyLimit+1))
	if err != nil || int64(len(data)) > a.bodyLimit {
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid User response"})
		return nil, false
	}
	var object iamv1.User
	if err := json.Unmarshal(data, &object); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid User response"})
		return nil, false
	}
	if len(object.Spec.Scopes) != 1 || object.Spec.Scopes[0] == string(identity.AdminScope) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Admin User is protected"})
		return nil, false
	}
	return data, true
}

func (a *Handler) listOrdinaryUsers(c *gin.Context) {
	response, err := a.upstream.Do(c.Request.Context(), http.MethodGet, c.Request.URL.RequestURI(), nil, nil)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "User list unavailable"})
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		c.Status(response.StatusCode)
		return
	}
	var result map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(response.Body, a.bodyLimit+1)).Decode(&result); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid User list response"})
		return
	}
	var items []json.RawMessage
	if raw, ok := result["items"]; !ok || string(raw) == "null" || json.Unmarshal(raw, &items) != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid User list response"})
		return
	}
	filtered := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		var object iamv1.User
		if string(item) == "null" || json.Unmarshal(item, &object) != nil {
			continue
		}
		scopes := object.Spec.Scopes
		if len(scopes) == 1 && scopes[0] == string(identity.AdminScope) {
			continue
		}
		filtered = append(filtered, item)
	}
	result["items"], err = json.Marshal(filtered)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid User list response"})
		return
	}
	c.JSON(http.StatusOK, result)
}
