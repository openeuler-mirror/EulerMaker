package handler

import (
	"fmt"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

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
	loginIP               *limit.Buckets
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
		loginIP:               limit.New(20.0/60.0, 20, deps.Now),
		machineLimit:          limit.New(5.0/60.0, 5, deps.Now),
		publicLimit:           limit.New(20, 40, deps.Now),
		policy:                policy.New(deps.Upstream),
		runnerWatches:         make(map[string]int),
	}, nil
}

// Middleware and Endpoints expose HTTP handlers for route registration.
type Middleware struct {
	Authenticate       gin.HandlerFunc
	ResolveUser        gin.HandlerFunc
	LimitAuthenticated gin.HandlerFunc
	Audit              gin.HandlerFunc
	RequireAdmin       gin.HandlerFunc
}

func (a *Handler) Middleware() Middleware {
	return Middleware{
		Authenticate:       a.auth.Authenticate,
		ResolveUser:        a.auth.ResolveUser,
		LimitAuthenticated: middleware.LimitAuthenticated(a.limits),
		Audit:              middleware.Audit(a.now),
		RequireAdmin:       middleware.RequireAdmin,
	}
}

type Endpoints struct {
	RegisterUser    gin.HandlerFunc
	Login           gin.HandlerFunc
	MachineToken    gin.HandlerFunc
	CheckToken      gin.HandlerFunc
	ChangePassword  gin.HandlerFunc
	RegisterMachine gin.HandlerFunc
}

func (a *Handler) Endpoints() Endpoints {
	return Endpoints{
		RegisterUser:    a.registerUser,
		Login:           a.login,
		MachineToken:    a.machineToken,
		CheckToken:      a.checkToken,
		ChangePassword:  a.changePassword,
		RegisterMachine: a.registerMachine,
	}
}
