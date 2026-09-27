package handler

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/iam"
	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/limit"
	"ebs-gateway/internal/policy"
	"ebs-gateway/internal/upstream"
)

type Dependencies struct {
	Upstream  *upstream.Client
	Tokens    *identity.Tokens
	Now       func() time.Time
	Limits    *limit.Buckets
	BodyLimit int64
}

// Handler holds the shared dependencies for Gateway request handlers.
type Handler struct {
	upstream              *upstream.Client
	tokens                *identity.Tokens
	now                   func() time.Time
	limits                *limit.Buckets
	iam                   *iam.Client
	bodyLimit             int64
	registerIP            *limit.Buckets
	registrationUserLimit *limit.Buckets
	loginLimit            *limit.Buckets
	machineLimit          *limit.Buckets
	publicLimit           *limit.Buckets
	policy                *policy.Authorizer
	runnerWatchMu         sync.Mutex
	runnerWatches         map[string]int
}

func New(deps Dependencies) (*Handler, error) {
	if deps.Upstream == nil || deps.Tokens == nil || deps.Limits == nil {
		return nil, fmt.Errorf("gateway dependencies are required")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.BodyLimit <= 0 {
		return nil, fmt.Errorf("request body limit must be positive")
	}
	return &Handler{
		upstream: deps.Upstream, tokens: deps.Tokens, limits: deps.Limits, now: deps.Now,
		iam: iam.New(deps.Upstream), bodyLimit: deps.BodyLimit,
		registerIP:            limit.New(5.0/60.0, 5, deps.Now),
		registrationUserLimit: limit.New(3.0/60.0, 3, deps.Now),
		loginLimit:            limit.New(5.0/60.0, 5, deps.Now),
		machineLimit:          limit.New(5.0/60.0, 5, deps.Now),
		publicLimit:           limit.New(20, 40, deps.Now),
		policy:                policy.New(deps.Upstream),
		runnerWatches:         make(map[string]int),
	}, nil
}

const principalKey = "principal"

func principal(c *gin.Context) identity.Principal {
	value, exists := c.Get(principalKey)
	if !exists {
		return identity.Principal{}
	}
	return value.(identity.Principal)
}

func (a *Handler) authenticate(c *gin.Context) {
	who, ok := a.verifyBearer(c, c.GetHeader("Authorization"))
	if !ok {
		return
	}
	c.Set(principalKey, who)
	c.Next()
}

func (a *Handler) resolveUser(c *gin.Context) {
	if !a.confirmUser(c, principal(c)) {
		return
	}
	c.Next()
}

func (a *Handler) limitAuthenticated(c *gin.Context) {
	user := principal(c)
	if !a.limits.Allow(user.Subject + "/" + c.ClientIP()) {
		c.Header("Retry-After", "1")
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "too many requests"})
		return
	}
	c.Next()
}
