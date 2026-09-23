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

// ManifestState is the sealing state of a Job upload manifest.
type ManifestState string

const (
	ManifestOpen       ManifestState = "Open"
	ManifestCompleting ManifestState = "Completing"
	ManifestCompleted  ManifestState = "Completed"
	ManifestFailed     ManifestState = "Failed"
)

// Formal release states reported by Artifact Manager.
type ReleaseState string

const (
	ReleaseCreating ReleaseState = "Creating"
	ReleasePrepared ReleaseState = "Prepared"
	ReleaseReady    ReleaseState = "Ready"
	ReleaseFailed   ReleaseState = "Failed"
	ReleaseDeleting ReleaseState = "Deleting"
)

type ManifestReference struct {
	JobName string `json:"jobName"`
	JobUID  string `json:"jobUID"`
}

type CreateRepositoryRequest struct {
	RepositoryUID     string              `json:"repositoryUID"`
	RepositoryName    string              `json:"repositoryName"`
	Project           string              `json:"project"`
	BuildName         string              `json:"buildName"`
	TargetOS          string              `json:"targetOS"`
	TargetArch        string              `json:"targetArch"`
	BaseRepositoryUID string              `json:"baseRepositoryUID,omitempty"`
	Manifests         []ManifestReference `json:"manifests"`
}

type FailureInfo struct {
	Code      string    `json:"code"`
	Message   string    `json:"message"`
	Retryable bool      `json:"retryable"`
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

type ManifestFile struct {
	ArtifactID   string `json:"artifactID"`
	RelativePath string `json:"relativePath"`
	Category     string `json:"category"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
	Required     bool   `json:"required"`
}

type JobUploadManifest struct {
	Project     string         `json:"project"`
	JobName     string         `json:"jobName"`
	JobUID      string         `json:"jobUID"`
	Files       []ManifestFile `json:"files"`
	Digest      string         `json:"digest,omitempty"`
	State       ManifestState  `json:"state"`
	Failure     *FailureInfo   `json:"failure,omitempty"`
	CreatedAt   time.Time      `json:"createdAt"`
	UpdatedAt   time.Time      `json:"updatedAt"`
	CompletedAt *time.Time     `json:"completedAt,omitempty"`
}

type CreateReleaseRequest struct {
	BuildName           string   `json:"buildName"`
	Project             string   `json:"project"`
	TargetOS            string   `json:"targetOS"`
	TargetArch          string   `json:"targetArch"`
	SourceRepositoryUID string   `json:"sourceRepositoryUID"`
	ExcludeSpecs        []string `json:"excludeSpecs,omitempty"`
}

type ReleaseResponse struct {
	BuildName        string       `json:"buildName"`
	State            ReleaseState `json:"state"`
	Attempt          int          `json:"attempt"`
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
	GetRepository(ctx context.Context, repositoryUID string) (RepositoryResponse, error)
	GetJobManifest(ctx context.Context, project, jobName, jobUID string) (JobUploadManifest, error)
	SubmitRelease(ctx context.Context, req CreateReleaseRequest) (ReleaseResponse, error)
	GetRelease(ctx context.Context, buildName string) (ReleaseResponse, error)
	ActivateRelease(ctx context.Context, buildName string) (ReleaseResponse, error)
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

// maxResponseBytes bounds one API response, including Job manifests. Repository status responses do not
// include the repository's RPM metadata.
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
	if len(req.Manifests) == 0 {
		return response, &artifactError{operation: "submit-repository", kind: artifactPermanent, code: "InvalidRepositoryRequest", statusCode: http.StatusUnprocessableEntity,
			err: fmt.Errorf("repository request must reference at least one manifest")}
	}
	err := c.call(ctx, http.MethodPost, "/internal/v1/repositories", req, &response)
	return response, err
}

func (c *httpArtifactClient) GetRepository(ctx context.Context, repositoryUID string) (RepositoryResponse, error) {
	var response RepositoryResponse
	if repositoryUID == "" {
		return response, &artifactError{operation: "get-repository", kind: artifactPermanent, code: "InvalidRepositoryRequest", statusCode: http.StatusUnprocessableEntity,
			err: fmt.Errorf("repository UID is required")}
	}
	err := c.call(ctx, http.MethodGet, "/internal/v1/repositories/"+url.PathEscape(repositoryUID), nil, &response)
	return response, err
}

func (c *httpArtifactClient) GetJobManifest(ctx context.Context, project, jobName, jobUID string) (JobUploadManifest, error) {
	var manifest JobUploadManifest
	if project == "" || jobName == "" || jobUID == "" {
		return manifest, &artifactError{operation: "get-manifest", kind: artifactPermanent, code: "InvalidRequest", statusCode: http.StatusBadRequest,
			err: fmt.Errorf("project, job name and job UID are required")}
	}
	path := "/artifacts/v1/projects/" + url.PathEscape(project) + "/jobs/" + url.PathEscape(jobName) + "/manifest?jobUID=" + url.QueryEscape(jobUID)
	err := c.call(ctx, http.MethodGet, path, nil, &manifest)
	return manifest, err
}

func (c *httpArtifactClient) SubmitRelease(ctx context.Context, req CreateReleaseRequest) (ReleaseResponse, error) {
	var response ReleaseResponse
	if req.BuildName == "" || req.SourceRepositoryUID == "" {
		return response, &artifactError{operation: "submit-release", kind: artifactPermanent, code: "InvalidRequest", statusCode: http.StatusUnprocessableEntity,
			err: fmt.Errorf("release request requires build name and source repository UID")}
	}
	err := c.call(ctx, http.MethodPost, "/internal/v1/releases", req, &response)
	return response, err
}

func (c *httpArtifactClient) GetRelease(ctx context.Context, buildName string) (ReleaseResponse, error) {
	var response ReleaseResponse
	if buildName == "" {
		return response, &artifactError{operation: "get-release", kind: artifactPermanent, code: "InvalidRequest", statusCode: http.StatusBadRequest,
			err: fmt.Errorf("build name is required")}
	}
	err := c.call(ctx, http.MethodGet, "/internal/v1/releases/"+url.PathEscape(buildName), nil, &response)
	return response, err
}

func (c *httpArtifactClient) ActivateRelease(ctx context.Context, buildName string) (ReleaseResponse, error) {
	var response ReleaseResponse
	if buildName == "" {
		return response, &artifactError{operation: "activate-release", kind: artifactPermanent, code: "InvalidRequest", statusCode: http.StatusBadRequest,
			err: fmt.Errorf("build name is required")}
	}
	err := c.call(ctx, http.MethodPost, "/internal/v1/releases/"+url.PathEscape(buildName)+"/activate", nil, &response)
	return response, err
}

func (c *httpArtifactClient) call(ctx context.Context, method, path string, body any, out any) error {
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
			return &artifactError{operation: operation, kind: artifactPermanent, code: "InvalidRequest", err: err}
		}
		payload = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), payload)
	if err != nil {
		return &artifactError{operation: operation, kind: artifactPermanent, code: "InvalidRequest", err: err}
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return &artifactError{operation: operation, kind: artifactRetryable, code: "RequestCanceled", err: ctxErr}
		}
		return &artifactError{operation: operation, kind: artifactRetryable, code: "TransportError", err: err}
	}
	defer func() { _ = response.Body.Close() }()
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if readErr != nil {
		return &artifactError{operation: operation, kind: artifactRetryable, code: "UnreadableResponse", statusCode: response.StatusCode, err: readErr}
	}
	if len(raw) > maxResponseBytes {
		return &artifactError{operation: operation, kind: artifactRetryable, code: "ResponseTooLarge", statusCode: response.StatusCode,
			err: fmt.Errorf("response exceeded %d bytes", maxResponseBytes)}
	}
	if response.StatusCode/100 == 2 {
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return &artifactError{operation: operation, kind: artifactRetryable, code: "UnparsableResponse", statusCode: response.StatusCode, err: err}
		}
		return nil
	}
	return classifyArtifactFailure(operation, response, raw)
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
	case strings.HasSuffix(path, "/activate"):
		return "activate-release"
	case strings.Contains(path, "/internal/v1/repositories"):
		if method == http.MethodPost {
			return "submit-repository"
		}
		return "get-repository"
	case strings.Contains(path, "/internal/v1/releases"):
		if method == http.MethodPost {
			return "submit-release"
		}
		return "get-release"
	case strings.Contains(path, "/manifest"):
		return "get-manifest"
	}
	return strings.ToLower(method)
}
