package rpmrepo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Repository materialization states reported by Artifact Manager.
type RepositoryState string

const (
	RepositoryCreating RepositoryState = "Creating"
	RepositoryReady    RepositoryState = "Ready"
	RepositoryFailed   RepositoryState = "Failed"
	RepositoryDeleting RepositoryState = "Deleting"
)

// Formal release states reported by Artifact Manager.
type ReleaseState string

const (
	ReleaseCreating ReleaseState = "Creating"
	ReleaseReady    ReleaseState = "Ready"
	ReleaseFailed   ReleaseState = "Failed"
	ReleaseDeleting ReleaseState = "Deleting"
)

type ManifestReference struct {
	JobName string `json:"jobName"`
}

type CreateRepositoryRequest struct {
	RepositoryUID     string              `json:"repositoryUID"`
	RepositoryName    string              `json:"repositoryName"`
	Project           string              `json:"-"`
	BuildName         string              `json:"-"`
	TargetArch        string              `json:"targetArch"`
	BaseRepositoryUID string              `json:"baseRepositoryUID,omitempty"`
	Manifests         []ManifestReference `json:"manifests"`
}

type FailureInfo struct {
	Code      string    `json:"code"`
	Message   string    `json:"message"`
	Retryable bool      `json:"retryable"`
	JobName   string    `json:"jobName,omitempty"`
	Time      time.Time `json:"time"`
}

type RepositoryResponse struct {
	RepositoryUID    string          `json:"repositoryUID"`
	State            RepositoryState `json:"state"`
	Attempt          int             `json:"attempt"`
	PollAfterSeconds int             `json:"pollAfterSeconds,omitempty"`
	ContentURL       string          `json:"contentURL,omitempty"`
	Failure          *FailureInfo    `json:"failure,omitempty"`
	CreatedAt        time.Time       `json:"createdAt"`
	UpdatedAt        time.Time       `json:"updatedAt"`
	CompletedAt      *time.Time      `json:"completedAt,omitempty"`
}

type CreateReleaseRequest struct {
	BuildName           string   `json:"-"`
	Project             string   `json:"-"`
	TargetArch          string   `json:"targetArch"`
	SourceRepositoryUID string   `json:"sourceRepositoryUID"`
	ExcludeSpecs        []string `json:"excludeSpecs,omitempty"`
}

type ReleaseResponse struct {
	BuildName        string       `json:"buildName"`
	State            ReleaseState `json:"state"`
	PollAfterSeconds int          `json:"pollAfterSeconds,omitempty"`
	ContentURL       string       `json:"contentURL,omitempty"`
	Failure          *FailureInfo `json:"failure,omitempty"`
	CreatedAt        time.Time    `json:"createdAt"`
	UpdatedAt        time.Time    `json:"updatedAt"`
	CompletedAt      *time.Time   `json:"completedAt,omitempty"`
}

// ArtifactManagerClient talks to the Artifact Manager control API. It keeps the stable error codes, the
// retryable flag and Retry-After so callers can classify a response without parsing messages.
type ArtifactManagerClient interface {
	SubmitRepository(ctx context.Context, req CreateRepositoryRequest) (RepositoryResponse, error)
	GetRepository(ctx context.Context, project, buildName, repositoryUID string) (RepositoryResponse, error)
	SubmitRelease(ctx context.Context, req CreateReleaseRequest) (ReleaseResponse, error)
	GetRelease(ctx context.Context, project, buildName string) (ReleaseResponse, error)
	DeleteBuildReleases(ctx context.Context, project, buildName string) (bool, error)
	DeleteBuildRepositories(ctx context.Context, project, buildName string) (bool, error)
}

type artifactErrorKind string

const (
	artifactNotFound  artifactErrorKind = "NotFound"
	artifactDeleting  artifactErrorKind = "Deleting"
	artifactRetryable artifactErrorKind = "Retryable"
	artifactPermanent artifactErrorKind = "Permanent"
)

// artifactError is the classified result of one Artifact Manager call.
type artifactError struct {
	operation  string
	kind       artifactErrorKind
	code       string
	statusCode int
	retryAfter time.Duration
	err        error
}

func (e *artifactError) Error() string {
	return fmt.Sprintf("artifact-manager %s failed (%s/%s status=%d): %v", e.operation, e.kind, e.code, e.statusCode, e.err)
}

func (e *artifactError) Unwrap() error { return e.err }

func isArtifactNotFound(err error) bool {
	var target *artifactError
	return errors.As(err, &target) && target.kind == artifactNotFound
}

func isArtifactDeleting(err error) bool {
	var target *artifactError
	return errors.As(err, &target) && target.kind == artifactDeleting
}

func artifactRetryAfter(err error) time.Duration {
	var target *artifactError
	if errors.As(err, &target) && target.kind == artifactRetryable {
		return target.retryAfter
	}
	return 0
}

// isArtifactRetryable reports whether the failure only says "try the same request again later".
func isArtifactRetryable(err error) bool {
	var target *artifactError
	if !errors.As(err, &target) {
		return false
	}
	return target.kind == artifactRetryable || target.kind == artifactNotFound
}

// artifactErrorCode returns the stable error code when the response carried one.
func artifactErrorCode(err error) string {
	var target *artifactError
	if errors.As(err, &target) {
		return target.code
	}
	return ""
}

type apiErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type httpArtifactClient struct {
	base   *url.URL
	client *http.Client
}

// maxResponseBytes bounds one control API response; repository responses exclude RPM metadata.
const maxResponseBytes = 8 << 20

// newArtifactManagerClient validates the configured address and builds the HTTP client used for every control
// call. The HTTP client never retries on its own: one controller decision maps to exactly one request.
func newArtifactManagerClient(address string, timeout time.Duration) (ArtifactManagerClient, error) {
	if strings.TrimSpace(address) == "" {
		return nil, fmt.Errorf("artifact manager address is required")
	}
	parsed, err := url.Parse(address)
	if err != nil {
		return nil, fmt.Errorf("parse artifact manager address %q: %w", address, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("artifact manager address %q must use http or https", address)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("artifact manager address %q must include a host", address)
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("artifact manager timeout must be positive")
	}
	return &httpArtifactClient{base: parsed, client: &http.Client{Timeout: timeout}}, nil
}

func (c *httpArtifactClient) SubmitRepository(ctx context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
	var response RepositoryResponse
	if req.Project == "" || req.BuildName == "" || len(req.Manifests) == 0 {
		return response, &artifactError{operation: "submit-repository", kind: artifactPermanent, code: "InvalidRepositoryRequest", statusCode: http.StatusUnprocessableEntity,
			err: fmt.Errorf("repository request requires project, build name and at least one manifest")}
	}
	err := c.call(ctx, http.MethodPost, buildPath(req.Project, req.BuildName)+"/repositories", req, &response)
	return response, err
}

func (c *httpArtifactClient) GetRepository(ctx context.Context, project, buildName, repositoryUID string) (RepositoryResponse, error) {
	var response RepositoryResponse
	if project == "" || buildName == "" || repositoryUID == "" {
		return response, &artifactError{operation: "get-repository", kind: artifactPermanent, code: "InvalidRepositoryRequest", statusCode: http.StatusUnprocessableEntity,
			err: fmt.Errorf("project, build name and repository UID are required")}
	}
	err := c.call(ctx, http.MethodGet, buildPath(project, buildName)+"/repositories/"+url.PathEscape(repositoryUID), nil, &response)
	return response, err
}

func (c *httpArtifactClient) SubmitRelease(ctx context.Context, req CreateReleaseRequest) (ReleaseResponse, error) {
	var response ReleaseResponse
	if req.Project == "" || req.BuildName == "" || req.SourceRepositoryUID == "" {
		return response, &artifactError{operation: "submit-release", kind: artifactPermanent, code: "InvalidRequest", statusCode: http.StatusUnprocessableEntity,
			err: fmt.Errorf("release request requires project, build name and source repository UID")}
	}
	err := c.call(ctx, http.MethodPost, buildPath(req.Project, req.BuildName)+"/releases", req, &response)
	return response, err
}

func (c *httpArtifactClient) GetRelease(ctx context.Context, project, buildName string) (ReleaseResponse, error) {
	var response ReleaseResponse
	if project == "" || buildName == "" {
		return response, &artifactError{operation: "get-release", kind: artifactPermanent, code: "InvalidRequest", statusCode: http.StatusBadRequest,
			err: fmt.Errorf("project and build name are required")}
	}
	err := c.call(ctx, http.MethodGet, buildPath(project, buildName)+"/releases", nil, &response)
	return response, err
}

// DeleteBuildRepositories confirms completion only on 204; a missing route is not a deleted repository.
func (c *httpArtifactClient) DeleteBuildRepositories(ctx context.Context, project, buildName string) (bool, error) {
	return c.deleteBuildResource(ctx, project, buildName, "repositories")
}

func (c *httpArtifactClient) DeleteBuildReleases(ctx context.Context, project, buildName string) (bool, error) {
	return c.deleteBuildResource(ctx, project, buildName, "releases")
}

func buildPath(project, buildName string) string {
	return "/internal/v1/projects/" + url.PathEscape(project) + "/builds/" + url.PathEscape(buildName)
}

func (c *httpArtifactClient) deleteBuildResource(ctx context.Context, project, buildName, resource string) (bool, error) {
	if project == "" || buildName == "" {
		return false, fmt.Errorf("%s cleanup requires project and build name", resource)
	}
	path := buildPath(project, buildName) + "/" + resource
	status, err := c.callStatus(ctx, http.MethodDelete, path, nil, nil)
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusNoContent:
		return true, nil
	case http.StatusAccepted:
		return false, nil
	default:
		return false, fmt.Errorf("unexpected %s cleanup status %d", resource, status)
	}
}

func (c *httpArtifactClient) call(ctx context.Context, method, path string, body any, out any) error {
	_, err := c.callStatus(ctx, method, path, body, out)
	return err
}

func (c *httpArtifactClient) callStatus(ctx context.Context, method, path string, body any, out any) (int, error) {
	operation := operationFor(path, method)
	target := *c.base
	target.Path = strings.TrimSuffix(target.Path, "/") + strings.SplitN(path, "?", 2)[0]
	target.RawQuery = ""
	if idx := strings.Index(path, "?"); idx >= 0 {
		target.RawQuery = path[idx+1:]
	}
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, &artifactError{operation: operation, kind: artifactPermanent, code: "InvalidRequest", err: err}
		}
		payload = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), payload)
	if err != nil {
		return 0, &artifactError{operation: operation, kind: artifactPermanent, code: "InvalidRequest", err: err}
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, &artifactError{operation: operation, kind: artifactRetryable, code: "RequestCanceled", err: ctxErr}
		}
		return 0, &artifactError{operation: operation, kind: artifactRetryable, code: "TransportError", err: err}
	}
	defer func() { _ = response.Body.Close() }()
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if readErr != nil {
		return response.StatusCode, &artifactError{operation: operation, kind: artifactRetryable, code: "UnreadableResponse", statusCode: response.StatusCode, err: readErr}
	}
	if len(raw) > maxResponseBytes {
		return response.StatusCode, &artifactError{operation: operation, kind: artifactRetryable, code: "ResponseTooLarge", statusCode: response.StatusCode,
			err: fmt.Errorf("response exceeded %d bytes", maxResponseBytes)}
	}
	if response.StatusCode/100 == 2 {
		if out == nil {
			return response.StatusCode, nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return response.StatusCode, &artifactError{operation: operation, kind: artifactRetryable, code: "UnparsableResponse", statusCode: response.StatusCode, err: err}
		}
		return response.StatusCode, nil
	}
	return response.StatusCode, classifyArtifactFailure(operation, response, raw)
}

func classifyArtifactFailure(operation string, response *http.Response, raw []byte) error {
	var body apiErrorBody
	_ = json.Unmarshal(raw, &body)
	retryAfter := parseRetryAfter(response.Header.Get("Retry-After"))
	failure := &artifactError{
		operation:  operation,
		code:       body.Code,
		statusCode: response.StatusCode,
		retryAfter: retryAfter,
		err:        fmt.Errorf("status %d: %s", response.StatusCode, strings.TrimSpace(body.Message)),
	}
	switch response.StatusCode {
	case http.StatusNotFound:
		failure.kind = artifactNotFound
	case http.StatusConflict:
		if body.Code == "RepositoryDeleting" || body.Code == "ReleaseDeleting" {
			failure.kind = artifactDeleting
		} else {
			failure.kind = artifactPermanent
		}
	case http.StatusGone, http.StatusUnprocessableEntity, http.StatusBadRequest:
		failure.kind = artifactPermanent
	case http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusRequestTimeout:
		failure.kind = artifactRetryable
	default:
		if response.StatusCode >= 500 {
			failure.kind = artifactRetryable
		} else {
			failure.kind = artifactPermanent
		}
	}
	return failure
}

func parseRetryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}
	return 0
}

func operationFor(path, method string) string {
	switch {
	case method == http.MethodDelete && strings.HasSuffix(path, "/repositories"):
		return "delete-build-repositories"
	case method == http.MethodDelete && strings.HasSuffix(path, "/releases"):
		return "delete-build-releases"
	case strings.HasSuffix(path, "/releases"):
		if method == http.MethodPost {
			return "submit-release"
		}
		return "get-release"
	case strings.Contains(path, "/repositories"):
		if method == http.MethodPost {
			return "submit-repository"
		}
		return "get-repository"
	}
	return strings.ToLower(method)
}
