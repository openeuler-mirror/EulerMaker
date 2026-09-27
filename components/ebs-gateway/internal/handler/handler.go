package handler

import (
	"fmt"
	"log"
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

func (a *Handler) audit(c *gin.Context) {
	started := a.now()
	method := c.Request.Method
	path := c.Request.URL.Path
	c.Next()
	user := principal(c)
	subject := user.Subject
	if subject == "" {
		subject = "anonymous"
	}
	log.Printf("method=%q path=%q status=%d response_bytes=%d latency_ms=%d client_ip=%q user=%q request_id=%q", method, path, c.Writer.Status(), c.Writer.Size(), a.now().Sub(started).Milliseconds(), c.ClientIP(), subject, c.Writer.Header().Get("X-Request-ID"))
}

// Middleware and Endpoints expose HTTP handlers for route registration.
// Authorization decisions and request processing remain owned by Handler.
type Middleware struct {
	Authenticate       gin.HandlerFunc
	ResolveUser        gin.HandlerFunc
	LimitAuthenticated gin.HandlerFunc
	Audit              gin.HandlerFunc
	RequireAdmin       gin.HandlerFunc
}

func (a *Handler) Middleware() Middleware {
	return Middleware{a.authenticate, a.resolveUser, a.limitAuthenticated, a.audit, a.requireAdmin}
}

type Endpoints struct {
	RegisterUser    gin.HandlerFunc
	Login           gin.HandlerFunc
	RunnerToken     gin.HandlerFunc
	CheckToken      gin.HandlerFunc
	ChangePassword  gin.HandlerFunc
	RegisterMachine gin.HandlerFunc
	GetUser         gin.HandlerFunc
	ListUsers       gin.HandlerFunc
}

func (a *Handler) Endpoints() Endpoints {
	return Endpoints{a.registerUser, a.login, a.runnerToken, a.checkToken, a.changePassword,
		a.registerMachine, a.getOrdinaryUser, a.listOrdinaryUsers}
}
