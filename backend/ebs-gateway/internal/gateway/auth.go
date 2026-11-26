package gateway

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	iamv1 "ebs-api/iam/v1"
	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/middleware"
	"ebs-gateway/internal/mutation"
)

var dnsLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func (a *Handler) readJSON(c *gin.Context, result any) error {
	if media := strings.TrimSpace(strings.Split(c.GetHeader("Content-Type"), ";")[0]); media != "application/json" {
		return fmt.Errorf("Content-Type must be application/json")
	}
	defer c.Request.Body.Close()
	data, err := io.ReadAll(io.LimitReader(c.Request.Body, a.bodyLimit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > a.bodyLimit {
		return fmt.Errorf("request body too large")
	}
	if _, err := mutation.ParseObject(data); err != nil {
		return fmt.Errorf("invalid JSON request: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil {
		return err
	}
	var remainder any
	if err := decoder.Decode(&remainder); err != io.EOF {
		return fmt.Errorf("request body must contain one JSON object")
	}
	return nil
}

func validUsername(name string) bool     { return len(name) <= 63 && dnsLabel.MatchString(name) }
func validPassword(password string) bool { return len(password) >= 12 && len(password) <= 128 }

// registerUser creates a local password account when local registration is enabled.
func (a *Handler) registerUser(c *gin.Context) {
	if !a.localAuthEnabled {
		c.JSON(http.StatusForbidden, gin.H{"error": "password registration disabled"})
		return
	}
	var input struct {
		Username    string `json:"username"`
		Password    string `json:"password"`
		DisplayName string `json:"displayName"`
		Email       string `json:"email"`
	}
	if err := a.readJSON(c, &input); err != nil || !validUsername(input.Username) || !validPassword(input.Password) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid registration request"})
		return
	}
	if input.Email != "" {
		address, err := mail.ParseAddress(input.Email)
		if err != nil || address.Address != input.Email {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid email"})
			return
		}
	}
	ip := c.ClientIP()
	if !a.registerIP.Allow(ip) || !a.registrationUserLimit.Allow(input.Username+"/"+ip) {
		middleware.AbortRateLimited(c, 60, "too many requests")
		return
	}
	status, err := a.iam.RegisterUser(c.Request.Context(), input)
	if err != nil || status >= 500 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "registration unavailable"})
		return
	}
	if status != http.StatusCreated {
		c.JSON(status, gin.H{"error": "registration failed"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"username": input.Username})
}

// login exchanges local user credentials for an access token.
func (a *Handler) login(c *gin.Context) {
	if !a.localAuthEnabled {
		c.JSON(http.StatusForbidden, gin.H{"error": "password login disabled"})
		return
	}
	if !a.loginIP.Allow(c.ClientIP()) {
		middleware.AbortRateLimited(c, 60, "too many requests")
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := a.readJSON(c, &input); err != nil || !validUsername(input.Username) || input.Password == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid login request"})
		return
	}
	if !a.loginLimit.Allow(input.Username + "/" + c.ClientIP()) {
		middleware.AbortRateLimited(c, 60, "too many requests")
		return
	}
	authenticated, status, err := a.iam.AuthenticateUser(c.Request.Context(), input.Username, input.Password)
	if err != nil || status >= 500 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "authentication unavailable"})
		return
	}
	if !authenticated {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	user, status, err := a.iam.GetUser(c.Request.Context(), input.Username)
	if err != nil || status >= 500 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "user service unavailable"})
		return
	}
	if status != http.StatusOK || user.Name != input.Username || !user.Enabled || len(user.Scopes) != 1 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	scope := user.Scopes[0]
	if scope != identity.UserScope && scope != identity.OpsScope && scope != identity.AdminScope {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	token, err := a.tokens.Issue(input.Username, identity.UserType, scope, 24*time.Hour, a.now())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "token unavailable"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"token": token, "tokenType": "Bearer", "expiresIn": 86400})
}

// machineToken exchanges machine credentials for a Runner or Service token.
func (a *Handler) machineToken(c *gin.Context) {
	clientID, secret, ok := c.Request.BasicAuth()
	if !ok || !validUsername(clientID) || secret == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	var input struct {
		Runner json.RawMessage `json:"runner"`
	}
	if err := a.readJSON(c, &input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid token request"})
		return
	}
	if !a.machineLimit.Allow(clientID + "/" + c.ClientIP()) {
		middleware.AbortRateLimited(c, 60, "too many requests")
		return
	}
	spec, authenticated, status, err := a.iam.AuthenticateMachine(c.Request.Context(), clientID, secret)
	if err != nil || status >= 500 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "authentication unavailable"})
		return
	}
	if !authenticated {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	ttl := spec.TokenTTLSeconds
	if ttl < 300 || ttl > 86400 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "invalid authentication response"})
		return
	}
	var token string
	if spec.Type == iamv1.MachineAccountService {
		if len(input.Runner) != 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "service token request must be empty"})
			return
		}
		token, err = a.tokens.Issue(spec.Name, identity.ServiceType, "", time.Duration(ttl)*time.Second, a.now())
	} else {
		var runner string
		if err := json.Unmarshal(input.Runner, &runner); err != nil || !validUsername(runner) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid runner"})
			return
		}
		token, err = a.tokens.Issue(runner, identity.RunnerType, "", time.Duration(ttl)*time.Second, a.now())
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "token unavailable"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"accessToken": token, "tokenType": "Bearer", "expiresIn": ttl})
}

// checkToken returns the identity verified by the authentication middleware.
func (a *Handler) checkToken(c *gin.Context) {
	defer c.Request.Body.Close()
	data, err := io.ReadAll(io.LimitReader(c.Request.Body, 1))
	if err != nil || len(data) != 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "request body must be empty"})
		return
	}
	user := middleware.Principal(c)
	name := user.Subject
	scopes := make([]identity.Scope, 0, 1)
	if user.Scope != "" {
		scopes = append(scopes, user.Scope)
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"authenticated": true,
		"identity": gin.H{
			"type":   user.Type,
			"name":   name,
			"scopes": scopes,
		},
		"expiresAt": user.ExpiresAt,
	})
}

// changePassword updates the authenticated user's password after verifying the current password.
func (a *Handler) changePassword(c *gin.Context) {
	user := middleware.Principal(c)
	if c.Param("name") != user.Subject {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}
	var input struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if err := a.readJSON(c, &input); err != nil || input.CurrentPassword == "" || !validPassword(input.NewPassword) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid password request"})
		return
	}
	authenticated, status, err := a.iam.AuthenticateUser(c.Request.Context(), user.Subject, input.CurrentPassword)
	if err != nil || status >= 500 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "authentication unavailable"})
		return
	}
	if !authenticated {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	status, err = a.iam.SetPassword(c.Request.Context(), user.Subject, input.NewPassword)
	if err != nil || status >= 500 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "password update unavailable"})
		return
	}
	if status != http.StatusNoContent && status != http.StatusOK {
		c.JSON(status, gin.H{"error": "password update failed"})
		return
	}
	c.Status(http.StatusNoContent)
}

// registerMachine registers a machine account after global Admin admission.
func (a *Handler) registerMachine(c *gin.Context) {
	var input struct {
		Name            string                           `json:"name"`
		ClientSecret    string                           `json:"clientSecret"`
		TokenTTLSeconds int                              `json:"tokenTTLSeconds"`
		Type            json.RawMessage                  `json:"type,omitempty"`
		Projects        []string                         `json:"projects,omitempty"`
		Permissions     []iamv1.MachineAccountPermission `json:"permissions,omitempty"`
	}
	if err := a.readJSON(c, &input); err != nil || !validUsername(input.Name) || len(input.ClientSecret) > 256 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid machine account"})
		return
	}
	secret, err := base64.RawURLEncoding.DecodeString(input.ClientSecret)
	if err != nil || len(secret) < 32 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid machine account"})
		return
	}
	if input.TokenTTLSeconds == 0 {
		input.TokenTTLSeconds = 3600
	}
	if input.TokenTTLSeconds < 300 || input.TokenTTLSeconds > 86400 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid token lifetime"})
		return
	}
	status, err := a.iam.RegisterMachine(c.Request.Context(), input)
	if err != nil || status >= 500 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "machine registration unavailable"})
		return
	}
	if status != http.StatusCreated {
		c.JSON(status, gin.H{"error": "machine registration failed"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"name": input.Name})
}
