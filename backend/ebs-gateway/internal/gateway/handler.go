package gateway

import (
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"

	"ebs-gateway/internal/limit"
	"ebs-gateway/internal/middleware"
	"ebs-gateway/internal/policy"
	"ebs-gateway/internal/upstream"
)

type Dependencies struct {
	Auth      *middleware.Auth
	Upstream  *upstream.Client
	Now       func() time.Time
	Limits    *limit.Buckets
	BodyLimit int64
}

// Handler holds the shared dependencies for Gateway request handlers.
type Handler struct {
	upstream      *upstream.Client
	now           func() time.Time
	limits        *limit.Buckets
	auth          *middleware.Auth
	bodyLimit     int64
	policy        *policy.Authorizer
	runnerWatchMu sync.Mutex
	runnerWatches sets.Set[string]
}

func NewHandler(deps Dependencies) (*Handler, error) {
	if deps.Upstream == nil || deps.Auth == nil || deps.Limits == nil {
		return nil, fmt.Errorf("gateway dependencies are required")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.BodyLimit <= 0 {
		return nil, fmt.Errorf("request body limit must be positive")
	}
	return &Handler{
		upstream:      deps.Upstream,
		limits:        deps.Limits,
		now:           deps.Now,
		bodyLimit:     deps.BodyLimit,
		auth:          deps.Auth,
		policy:        policy.New(deps.Upstream),
		runnerWatches: sets.New[string](),
	}, nil
}
