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

// Authenticate advances the middleware chain only after VerifyAccess succeeds.
func (a *Auth) Authenticate(c *gin.Context) {
	if !a.VerifyAccess(c) {
		return
	}
	c.Next()
}

func (a *Auth) verifyToken(header string) (identity.Principal, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || parts[0] != "Bearer" {
		return identity.Principal{}, false
	}
	who, err := a.tokens.Verify(parts[1], a.now())
	return who, err == nil
}

// VerifyAccess authenticates a request without advancing its middleware chain.
func (a *Auth) VerifyAccess(c *gin.Context) bool {
	who, ok := a.verifyToken(c.GetHeader("Authorization"))
	if !ok {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return false
	}
	switch who.Type {
	case identity.UserType:
		// Keep the signed user identity available to audit even if IAM rejects access.
		c.Set(principalKey, who)
		return a.verifyUser(c, who)
	case identity.ServiceType:
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
	case identity.RunnerType:
		// Runner tokens require no IAM lookup.
	default:
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return false
	}
	c.Set(principalKey, who)
	return true
}

func (a *Auth) verifyUser(c *gin.Context, who identity.Principal) bool {
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
