//go:build ignore

package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/mail"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	jsonpatch "github.com/evanphx/json-patch"
)

const (
	apiPrefix           = "/apis/ebs/v1"
	ownerUserLabel      = "ebs.io/owner-user"
	memberUserLabelBase = "ebs.io/member-user."
	projectTypeLabel    = "project.ebs.io/type"
)

type Gateway struct {
	cfg                 Config
	upstream            *url.URL
	client              *http.Client
	proxy               *httputil.ReverseProxy
	limiter             *RateLimiter
	publicLimiter       *RateLimiter
	registerIPLimiter   *RateLimiter
	registerUserLimiter *RateLimiter
	watchMu             sync.Mutex
	activeRunnerWatches map[string]int
	tokens              *tokenManager
	now                 func() time.Time
	transport           http.RoundTripper
}

func NewServer(cfg Config) (*http.Server, error) {
	gw, err := NewGateway(cfg)
	if err != nil {
		return nil, err
	}
	return &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.Port),
		Handler:           gw,
		ReadHeaderTimeout: 10 * time.Second,
	}, nil
}

func NewGateway(cfg Config) (*Gateway, error) {
	upstream, err := url.Parse(cfg.APIServerAddr)
	if err != nil {
		return nil, fmt.Errorf("parse apiserver address: %w", err)
