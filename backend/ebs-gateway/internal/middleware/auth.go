package middleware

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	iamv1 "ebs-api/iam/v1"
	"ebs-gateway/internal/iam"
	"ebs-gateway/internal/identity"
)

// Auth verifies tokens and resolves their current account identity.
type Auth struct {
	tokens *identity.Tokens
	iam    *iam.Client
	now    func() time.Time
}

func NewAuth(tokens *identity.Tokens, client *iam.Client, now func() time.Time) *Auth {
	return &Auth{tokens: tokens, iam: client, now: now}
}

const principalKey = "principal"

// Principal returns the authenticated identity, or its zero value for anonymous requests.
func Principal(c *gin.Context) identity.Principal {
	value, exists := c.Get(principalKey)
	if !exists {
		return identity.Principal{}
	}
	return value.(identity.Principal)
}

// Authenticate requires a valid Bearer token before continuing the route chain.
func (a *Auth) Authenticate(c *gin.Context) {
	if !a.Verify(c) {
		return
	}
	c.Next()
}

// Verify stores the verified identity for routes that also support anonymous access.
func (a *Auth) Verify(c *gin.Context) bool {
	who, ok := a.verifyBearer(c, c.GetHeader("Authorization"))
	if !ok {
		return false
	}
	c.Set(principalKey, who)
	return true
}

// ResolveUser checks resource-access identity before continuing the route chain.
func (a *Auth) ResolveUser(c *gin.Context) {
	if !a.ConfirmUser(c, Principal(c)) {
		return
	}
	c.Next()
}

func (a *Auth) verifyBearer(c *gin.Context, header string) (identity.Principal, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || parts[0] != "Bearer" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return identity.Principal{}, false
	}
	who, err := a.tokens.Verify(parts[1], a.now())
	if err != nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return identity.Principal{}, false
	}
	if who.Type == identity.ServiceType {
		account, status, err := a.iam.GetMachine(c.Request.Context(), who.Subject)
		if err != nil || (status != http.StatusOK && status != http.StatusNotFound) {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "machine account service unavailable"})
			return identity.Principal{}, false
		}
		if status == http.StatusNotFound || account.Name != who.Subject || account.Spec.Type != iamv1.MachineAccountService {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return identity.Principal{}, false
		}
	}
	return who, true
}

// ConfirmUser validates the current user; service resource access is not enabled.
func (a *Auth) ConfirmUser(c *gin.Context, who identity.Principal) bool {
	if who.Type == identity.ServiceType {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "service resource access is not enabled"})
		return false
	}
	if !who.IsUser() {
		return true
	}
	user, status, err := a.iam.GetUser(c.Request.Context(), who.Subject)
	if err != nil || (status != http.StatusOK && status != http.StatusNotFound) {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "user service unavailable"})
		return false
	}
	if status == http.StatusNotFound || user.Name != who.Subject || !user.Enabled || len(user.Scopes) != 1 || user.Scopes[0] != who.Scope {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return false
	}
	return true
}
