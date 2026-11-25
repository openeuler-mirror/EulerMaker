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
	return &Auth{
		tokens: tokens,
		iam:    client,
		now:    now,
	}
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

// Authenticate verifies the token and, for user identities, the current IAM account.
func (a *Auth) Authenticate(c *gin.Context) {
	if !a.VerifyAccess(c) {
		return
	}
	c.Next()
}

func (a *Auth) verifyToken(c *gin.Context) bool {
	parts := strings.Fields(c.GetHeader("Authorization"))
	if len(parts) != 2 || parts[0] != "Bearer" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return false
	}
	who, err := a.tokens.Verify(parts[1], a.now())
	if err != nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return false
	}
	if who.Type == identity.ServiceType {
		account, status, err := a.iam.GetMachine(c.Request.Context(), who.Subject)
		if err != nil || (status != http.StatusOK && status != http.StatusNotFound) {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "machine account service unavailable"})
			return false
		}
		if status == http.StatusNotFound || account.Name != who.Subject || account.Spec.Type != iamv1.MachineAccountService {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return false
		}
		who.ServiceGrants = &identity.ServiceGrants{
			Projects:    account.Spec.Projects,
			Permissions: account.Spec.Permissions,
		}
	}
	c.Set(principalKey, who)
	return true
}

// VerifyAccess authenticates a request without advancing its middleware chain.
func (a *Auth) VerifyAccess(c *gin.Context) bool {
	if !a.verifyToken(c) {
		return false
	}
	who := Principal(c)
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

// RequireAdmin restricts the route chain to administrators.
func RequireAdmin(c *gin.Context) {
	if Principal(c).Scope != identity.AdminScope {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}
	c.Next()
}
