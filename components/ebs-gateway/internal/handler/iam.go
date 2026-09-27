package handler

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/policy"
)

func (a *Handler) IAM(resource string) gin.HandlerFunc { return a.serveIAM(resource) }

func (a *Handler) serveIAM(resource string) gin.HandlerFunc {
	return func(c *gin.Context) {
		method := c.Request.Method
		name := c.Param("name")
		if policy.HasWatch(c.Request.URL.Query()) {
			c.Status(http.StatusForbidden)
			return
		}
		if method == http.MethodPost || ((method == http.MethodPut || method == http.MethodPatch) && resource == "machineaccounts") {
			c.Header("Allow", "GET, HEAD, DELETE")
			c.Status(http.StatusMethodNotAllowed)
			return
		}
		if name == "" && method != http.MethodGet && method != http.MethodHead {
			c.Status(http.StatusMethodNotAllowed)
			return
		}
		if method != http.MethodGet && method != http.MethodHead && method != http.MethodPut && method != http.MethodPatch && method != http.MethodDelete {
			c.Status(http.StatusMethodNotAllowed)
			return
		}
		route := policy.Route{Resource: resource, Name: name, Method: method}
		if err := a.policy.Authorize(c.Request.Context(), principal(c), route); err != nil {
			c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
			return
		}
		if resource == "users" && name == "" {
			a.listOrdinaryUsers(c)
			return
		}
		if resource == "users" && (method == http.MethodGet || method == http.MethodHead) {
			a.getOrdinaryUser(c)
			return
		}
		if resource == "users" && method == http.MethodDelete {
			if !a.ordinaryUserObject(c) {
				return
			}
		}
		if method == http.MethodPut || method == http.MethodPatch {
			if !a.prepareUpdate(c, principal(c), route) {
				return
			}
		}
		a.upstream.Forward(c, principal(c).Subject, string(identity.AdminScope), false)
	}
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
	var object struct {
		Spec struct {
			Scopes []identity.Scope `json:"scopes"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(data, &object); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid User response"})
		return
	}
	if len(object.Spec.Scopes) != 1 || object.Spec.Scopes[0] == identity.AdminScope {
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
	var object struct {
		Spec struct {
			Scopes []identity.Scope `json:"scopes"`
		} `json:"spec"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, a.bodyLimit+1)).Decode(&object); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid User response"})
		return false
	}
	if len(object.Spec.Scopes) != 1 || object.Spec.Scopes[0] == identity.AdminScope {
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
	var result map[string]any
	if err := json.NewDecoder(io.LimitReader(response.Body, a.bodyLimit+1)).Decode(&result); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid User list response"})
		return
	}
	items, ok := result["items"].([]any)
	if !ok {
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid User list response"})
		return
	}
	filtered := make([]any, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		spec, _ := object["spec"].(map[string]any)
		scopes, _ := spec["scopes"].([]any)
		if len(scopes) == 1 && scopes[0] == string(identity.AdminScope) {
			continue
		}
		filtered = append(filtered, item)
	}
	result["items"] = filtered
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	c.JSON(http.StatusOK, result)
}
