package handler

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/klog/v2"

	"ebs-gateway/internal/iam"
	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/limit"
	"ebs-gateway/internal/middleware"
	"ebs-gateway/internal/oauth"
	"ebs-gateway/internal/policy"
	"ebs-gateway/internal/upstream"
)

type Dependencies struct {
	Upstream        *upstream.Client
	Tokens          *identity.Tokens
	Now             func() time.Time
	Limits          *limit.Buckets
	BodyLimit       int64
	OAuthConfigFile string
}

// Handler holds the shared dependencies for Gateway request handlers.
type Handler struct {
	OAuth                 *oauth.Handler
	upstream              *upstream.Client
	tokens                *identity.Tokens
	now                   func() time.Time
	limits                *limit.Buckets
	iam                   *iam.Client
	auth                  *middleware.Auth
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
	config, err := oauth.Load(deps.OAuthConfigFile)
	if err != nil {
		return nil, err
	}
	iamClient := iam.New(deps.Upstream)
	return &Handler{
		OAuth:                 oauth.New(config, iamClient, deps.Tokens, deps.Now),
		upstream:              deps.Upstream,
		tokens:                deps.Tokens,
		limits:                deps.Limits,
		now:                   deps.Now,
		iam:                   iamClient,
		bodyLimit:             deps.BodyLimit,
		auth:                  middleware.NewAuth(deps.Tokens, iamClient, deps.Now),
		registerIP:            limit.New(5.0/60.0, 5, deps.Now),
		registrationUserLimit: limit.New(3.0/60.0, 3, deps.Now),
		loginLimit:            limit.New(5.0/60.0, 5, deps.Now),
		machineLimit:          limit.New(5.0/60.0, 5, deps.Now),
		publicLimit:           limit.New(20, 40, deps.Now),
		policy:                policy.New(deps.Upstream),
		runnerWatches:         make(map[string]int),
	}, nil
}

func (a *Handler) limitAuthenticated(c *gin.Context) {
	user := middleware.Principal(c)
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
	user := middleware.Principal(c)
	subject := user.Subject
	if subject == "" {
		subject = "anonymous"
	}
	klog.InfoS("HTTP request", "method", method, "path", path, "status", c.Writer.Status(),
		"response_bytes", c.Writer.Size(), "latency_ms", a.now().Sub(started).Milliseconds(),
		"client_ip", c.ClientIP(), "user", subject, "request_id", c.Writer.Header().Get("X-Request-ID"),
		"service_project", c.GetString("serviceProject"), "service_permission", c.GetString("servicePermission"),
		"service_authorization", c.GetString("serviceAuthorization"))
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
	return Middleware{
		a.auth.Authenticate,
		a.auth.ResolveUser,
		a.limitAuthenticated,
		a.audit,
		a.requireAdmin,
	}
}

type Endpoints struct {
	RegisterUser    gin.HandlerFunc
	Login           gin.HandlerFunc
	MachineToken    gin.HandlerFunc
	CheckToken      gin.HandlerFunc
	ChangePassword  gin.HandlerFunc
	RegisterMachine gin.HandlerFunc
	GetUser         gin.HandlerFunc
	ListUsers       gin.HandlerFunc
}

func (a *Handler) Endpoints() Endpoints {
	return Endpoints{
		a.registerUser,
		a.login,
		a.machineToken,
		a.checkToken,
		a.changePassword,
		a.registerMachine,
		a.getOrdinaryUser,
		a.listOrdinaryUsers,
	}
}
