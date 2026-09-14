package gitserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	ebsv1 "ebs-api/ebs/v1"
)

type ErrorKind string

const (
	ErrorValidation ErrorKind = "Validation"
	ErrorNotFound   ErrorKind = "NotFound"
	ErrorTemporary  ErrorKind = "Temporary"
	ErrorPermanent  ErrorKind = "Permanent"
)

type Error struct {
	Operation string
	Kind      ErrorKind
	Err       error
}

func (e *Error) Error() string {
	return fmt.Sprintf("git-server %s failed (%s): %v", e.Operation, e.Kind, e.Err)
}
func (e *Error) Unwrap() error { return e.Err }

type SyncCheckResult struct {
	Synced   bool
	CloneURL string
}

type Config struct {
	Address  string
	Timeout  time.Duration
	Retries  int
	CacheTTL time.Duration
}

type Client struct {
	address string
	http    *http.Client
	retries int
	ttl     time.Duration
	mu      sync.Mutex
	cache   map[string]statusCache
}

type statusCache struct {
	response repositoryResponse
	expires  time.Time
}

type repositoryRequest struct {
	OriginURL string `json:"origin_url"`
}

type repositoryResponse struct {
	CloneURL string           `json:"clone_url,omitempty"`
	SyncTime *time.Time       `json:"sync_time,omitempty"`
	Error    *repositoryError `json:"error,omitempty"`
}

type repositoryError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type commandRequest struct {
	Repo    string   `json:"repo"`
	Command []string `json:"command"`
}

type commandResponse struct {
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	ExitCode        int    `json:"exit_code"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
}

type errorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

var (
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	refPattern    = regexp.MustCompile(`^[A-Za-z0-9._/][A-Za-z0-9._/-]*$`)
)

func New(config Config) (*Client, error) {
	if config.Address == "" || config.Timeout <= 0 || config.Retries < 0 || config.CacheTTL <= 0 {
		return nil, fmt.Errorf("git-server address, positive timeout/cache TTL and non-negative retries are required")
	}
	parsed, err := url.Parse(config.Address)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("invalid git-server address %q", config.Address)
	}
	return &Client{address: strings.TrimRight(config.Address, "/"), http: &http.Client{Timeout: config.Timeout}, retries: config.Retries, ttl: config.CacheTTL, cache: make(map[string]statusCache)}, nil
}

func (c *Client) PublishSyncTask(ctx context.Context, originURL string) error {
	if err := validateOrigin(originURL); err != nil {
		return gitError("sync", ErrorValidation, err)
	}
	_, err := c.do(ctx, "sync", "/api/v1/repo/sync", repositoryRequest{OriginURL: originURL}, http.StatusAccepted)
	return err
}

func (c *Client) CheckSynced(ctx context.Context, originURL string, baseline time.Time) (SyncCheckResult, error) {
	if err := validateOrigin(originURL); err != nil {
		return SyncCheckResult{}, gitError("status", ErrorValidation, err)
	}
	response, cached := c.cachedStatus(originURL)
	if !cached {
		body, err := c.do(ctx, "status", "/api/v1/repo/status", repositoryRequest{OriginURL: originURL}, http.StatusOK)
		if err != nil {
			var serverErr *Error
			if errors.As(err, &serverErr) && serverErr.Kind == ErrorNotFound {
				return SyncCheckResult{}, nil
			}
			return SyncCheckResult{}, err
		}
		if err := json.Unmarshal(body, &response); err != nil {
			return SyncCheckResult{}, gitError("status", ErrorTemporary, fmt.Errorf("decode response: %w", err))
		}
		c.storeStatus(originURL, response)
	}
	if response.Error != nil {
		kind := ErrorValidation
		if response.Error.Retryable {
			kind = ErrorTemporary
		}
		return SyncCheckResult{}, gitError("status", kind, fmt.Errorf("repository synchronization failed: %s", response.Error.Code))
	}
	if response.SyncTime == nil || response.SyncTime.Before(baseline) {
		return SyncCheckResult{}, nil
	}
	if response.CloneURL == "" {
		return SyncCheckResult{}, gitError("status", ErrorTemporary, fmt.Errorf("synced response lacks clone_url"))
	}
	return SyncCheckResult{Synced: true, CloneURL: response.CloneURL}, nil
}

func (c *Client) ResolveCommit(ctx context.Context, originURL string, ref ebsv1.GitRef) (string, error) {
	if err := validateOrigin(originURL); err != nil {
		return "", gitError("resolve", ErrorValidation, err)
	}
	if err := validateRef(ref); err != nil {
		return "", gitError("resolve", ErrorValidation, err)
	}
	value := "refs/heads/" + ref.Value
	if ref.Type == ebsv1.GitRefTag {
		value = "refs/tags/" + ref.Value + "^{commit}"
	}
	body, err := c.do(ctx, "resolve", "/command", commandRequest{Repo: originURL, Command: []string{"git-rev-parse", "--verify", value}}, http.StatusOK)
	if err != nil {
		return "", err
	}
	var response commandResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return "", gitError("resolve", ErrorTemporary, fmt.Errorf("decode response: %w", err))
	}
	commit := strings.TrimSpace(response.Stdout)
	if response.ExitCode != 0 || response.StdoutTruncated || !commitPattern.MatchString(commit) {
		return "", gitError("resolve", ErrorTemporary, fmt.Errorf("invalid command success response"))
	}
	return commit, nil
}

func (c *Client) do(ctx context.Context, operation, path string, request any, success int) ([]byte, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, gitError(operation, ErrorValidation, err)
	}
	var last error
	for attempt := 0; attempt <= c.retries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.address+path, bytes.NewReader(payload))
		if err != nil {
			return nil, gitError(operation, ErrorValidation, err)
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			last = gitError(operation, ErrorTemporary, err)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		_ = response.Body.Close()
		if readErr != nil {
			last = gitError(operation, ErrorTemporary, readErr)
			continue
		}
		if response.StatusCode == success {
			return body, nil
		}
		classified := classifyResponse(operation, response.StatusCode, body)
		if classified.Kind != ErrorTemporary {
			return nil, classified
		}
		last = classified
	}
	return nil, last
}

func classifyResponse(operation string, status int, body []byte) *Error {
	var response errorResponse
	_ = json.Unmarshal(body, &response)
	message := response.Code
	if response.Message != "" {
		message += ": " + response.Message
	}
	if message == "" {
		message = http.StatusText(status)
	}
	kind := ErrorPermanent
	switch {
	case operation == "status" && status == http.StatusNotFound:
		kind = ErrorNotFound
	case operation == "resolve" && status == http.StatusUnprocessableEntity:
		kind = ErrorNotFound
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		kind = ErrorValidation
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500:
		kind = ErrorTemporary
	}
	return gitError(operation, kind, fmt.Errorf("HTTP %d: %s", status, message))
}

func gitError(operation string, kind ErrorKind, err error) *Error {
	return &Error{Operation: operation, Kind: kind, Err: err}
}

func validateOrigin(value string) error {
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("origin URL is invalid")
	}
	return nil
}

func validateRef(ref ebsv1.GitRef) error {
	if ref.Type != ebsv1.GitRefBranch && ref.Type != ebsv1.GitRefTag {
		return fmt.Errorf("ref type must be Branch or Tag")
	}
	if !refPattern.MatchString(ref.Value) || strings.HasPrefix(ref.Value, "-") || strings.Contains(ref.Value, "..") || strings.Contains(ref.Value, "/./") || strings.Contains(ref.Value, "/../") {
		return fmt.Errorf("ref value is invalid")
	}
	return nil
}

func (c *Client) cachedStatus(key string) (repositoryResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.cache[key]
	if !ok || time.Now().After(item.expires) {
		delete(c.cache, key)
		return repositoryResponse{}, false
	}
	return item.response, true
}

func (c *Client) storeStatus(key string, response repositoryResponse) {
	c.mu.Lock()
	c.cache[key] = statusCache{response: response, expires: time.Now().Add(c.ttl)}
	c.mu.Unlock()
}
