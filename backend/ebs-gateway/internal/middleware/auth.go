package middleware

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	iamv1 "ebs-api/iam/v1"
	"ebs-gateway/internal/iamclient"
	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/policy"
)

// Auth verifies tokens and resolves their current account identity.
type Auth struct {
	tokens *identity.Tokens
	iam    *iamclient.Client
	now    func() time.Time
}

func NewAuth(tokens *identity.Tokens, client *iamclient.Client, now func() time.Time) *Auth {
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

// Authenticate resolves identity once, defaulting to required authentication for unlisted routes.
func Authenticate(routes *RoutePolicies, auth *Auth) gin.HandlerFunc {
	return func(c *gin.Context) {
		mode := routes.authentication(c.Request.Method, c.FullPath())
		if mode == AuthenticationNone || (mode == AuthenticationOptional &&
			!policy.HasWatch(c.Request.URL.Query()) && c.GetHeader("Authorization") == "") {
			c.Next()
			return
		}
		if !auth.verifyAccess(c) {
			return
		}
		c.Next()
	}
}

func (a *Auth) verifyToken(header string) (identity.Principal, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || parts[0] != "Bearer" {
		return identity.Principal{}, false
	}
	who, err := a.tokens.Verify(parts[1], a.now())
	return who, err == nil
}

func (a *Auth) verifyAccess(c *gin.Context) bool {
	parts := strings.Fields(c.GetHeader("Authorization"))
	if len(parts) == 2 && parts[0] == "Bearer" && strings.HasPrefix(parts[1], "ebs_pat_") {
		c.Header("Cache-Control", "no-store")
		who, err := a.iam.AuthenticateUserCredential(c.Request.Context(), parts[1], a.now())
		if err != nil {
			failure, ok := err.(*iamclient.CredentialError)
			if !ok {
				failure = iamclient.CredentialUnavailable()
			}
			c.AbortWithStatusJSON(failure.Status, gin.H{"code": failure.Code, "error": failure.Code})
			return false
		}
		c.Set(principalKey, who)
		return true
	}
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
	if status == http.StatusNotFound || user.Name != who.Subject || user.Spec.Enabled == nil || !*user.Spec.Enabled || len(user.Spec.Scopes) != 1 || identity.Scope(user.Spec.Scopes[0]) != who.Scope {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return false
	}
	return true
}
