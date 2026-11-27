package gateway

import (
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"

	"ebs-gateway/internal/iamclient"
	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/limit"
	"ebs-gateway/internal/middleware"
	"ebs-gateway/internal/policy"
	"ebs-gateway/internal/upstream"
)

type Dependencies struct {
	Upstream         *upstream.Client
	Tokens           *identity.Tokens
	Now              func() time.Time
	Limits           *limit.Buckets
	BodyLimit        int64
	IAM              *iamclient.Client
	LocalAuthEnabled bool
}

// Handler holds the shared dependencies for Gateway request handlers.
type Handler struct {
	localAuthEnabled      bool
	upstream              *upstream.Client
	tokens                *identity.Tokens
	now                   func() time.Time
	limits                *limit.Buckets
	iam                   *iamclient.Client
	auth                  *middleware.Auth
	bodyLimit             int64
	registerIP            *limit.Buckets
	registrationUserLimit *limit.Buckets
	loginLimit            *limit.Buckets
	loginIP               *limit.Buckets
	machineLimit          *limit.Buckets
	policy                *policy.Authorizer
	runnerWatchMu         sync.Mutex
	runnerWatches         sets.Set[string]
}

func NewHandler(deps Dependencies) (*Handler, error) {
	if deps.Upstream == nil || deps.IAM == nil || deps.Tokens == nil || deps.Limits == nil {
		return nil, fmt.Errorf("gateway dependencies are required")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.BodyLimit <= 0 {
		return nil, fmt.Errorf("request body limit must be positive")
	}
	return &Handler{
		localAuthEnabled:      deps.LocalAuthEnabled,
		upstream:              deps.Upstream,
		tokens:                deps.Tokens,
		limits:                deps.Limits,
		now:                   deps.Now,
		iam:                   deps.IAM,
		bodyLimit:             deps.BodyLimit,
		auth:                  middleware.NewAuth(deps.Tokens, deps.IAM, deps.Now),
		registerIP:            limit.New(5.0/60.0, 5, deps.Now),
		registrationUserLimit: limit.New(3.0/60.0, 3, deps.Now),
		loginLimit:            limit.New(5.0/60.0, 5, deps.Now),
		loginIP:               limit.New(20.0/60.0, 20, deps.Now),
		machineLimit:          limit.New(5.0/60.0, 5, deps.Now),
		policy:                policy.New(deps.Upstream),
		runnerWatches:         sets.New[string](),
	}, nil
}
