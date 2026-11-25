package handler

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

func (a *Handler) Users(c *gin.Context) {
	route, ok := a.authorizeIAM(c, "users")
	if !ok {
		return
	}
	switch route.Method {
	case http.MethodGet, http.MethodHead:
		if route.Name == "" {
			a.listOrdinaryUsers(c)
		} else {
			a.getOrdinaryUser(c)
		}
		return
	case http.MethodDelete:
		if !a.ordinaryUserObject(c) {
			return
		}
	}
	a.forwardIAM(c, route)
}

func (a *Handler) MachineAccounts(c *gin.Context) {
	route, ok := a.authorizeIAM(c, "machineaccounts")
	if !ok {
		return
	}
	a.forwardIAM(c, route)
}

func (a *Handler) authorizeIAM(c *gin.Context, resource string) (policy.Route, bool) {
	route := policy.Route{
		Resource: resource,
		Name:     c.Param("name"),
		Method:   c.Request.Method,
	}
	if policy.HasWatch(c.Request.URL.Query()) {
		c.Status(http.StatusForbidden)
		return route, false
	}
	if err := a.policy.Authorize(c.Request.Context(), middleware.Principal(c), route); err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return route, false
	}
	return route, true
}

func (a *Handler) forwardIAM(c *gin.Context, route policy.Route) {
	if route.Method == http.MethodPut || route.Method == http.MethodPatch {
		if !a.prepareUpdate(c, middleware.Principal(c), route) {
			return
		}
	}
	a.upstream.Forward(c, false)
}

func (a *Handler) getOrdinaryUser(c *gin.Context) {
	response, err := a.upstream.Do(c.Request.Context(), http.MethodGet, c.Request.URL.Path, nil, nil)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "User unavailable"})
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		c.Status(response.StatusCode)
		return
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, a.bodyLimit+1))
	if err != nil || int64(len(data)) > a.bodyLimit {
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid User response"})
		return
	}
	var object iamv1.User
	if err := json.Unmarshal(data, &object); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid User response"})
		return
	}
	if len(object.Spec.Scopes) != 1 || object.Spec.Scopes[0] == string(identity.AdminScope) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Admin User is protected"})
		return
	}
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	c.Data(http.StatusOK, "application/json", data)
}

func (a *Handler) ordinaryUserObject(c *gin.Context) bool {
	response, err := a.upstream.Do(c.Request.Context(), http.MethodGet, c.Request.URL.Path, nil, nil)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "User unavailable"})
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		c.Status(response.StatusCode)
		return false
	}
	var object iamv1.User
	if err := json.NewDecoder(io.LimitReader(response.Body, a.bodyLimit+1)).Decode(&object); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid User response"})
		return false
	}
	if len(object.Spec.Scopes) != 1 || object.Spec.Scopes[0] == string(identity.AdminScope) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Admin User is protected"})
		return false
	}
	return true
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
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	c.JSON(http.StatusOK, result)
}
