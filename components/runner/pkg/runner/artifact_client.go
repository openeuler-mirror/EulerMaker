package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type ArtifactClient struct {
	baseURL    *url.URL
	httpClient *http.Client
	tokens     TokenSource
}

type LogStatus struct {
	Stream         string    `json:"stream"`
	State          string    `json:"state"`
	NextSequence   int64     `json:"nextSequence"`
	CommittedBytes int64     `json:"committedBytes"`
	ArtifactID     string    `json:"artifactID,omitempty"`
	FinalSize      *int64    `json:"finalSize,omitempty"`
	FinalSHA256    string    `json:"finalSHA256,omitempty"`
	UpdatedAt      time.Time `json:"updatedAt,omitempty"`
}

type AppendLogResult struct {
	Stream           string `json:"stream"`
	AcceptedSequence int64  `json:"acceptedSequence"`
	NextSequence     int64  `json:"nextSequence"`
	CommittedBytes   int64  `json:"committedBytes"`
}

type CompleteLogInput struct {
	JobUID       string `json:"jobUID"`
	Stream       string `json:"stream"`
	LastSequence int64  `json:"lastSequence"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
}

type CompletedLog struct {
	State        string `json:"state"`
	ArtifactID   string `json:"artifactID"`
	RelativePath string `json:"relativePath"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
}

type ArtifactAPIError struct {
	StatusCode int
	Code       string
	Message    string
	Retryable  bool
	RetryAfter time.Duration
	Details    map[string]any
}

func (e ArtifactAPIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("artifact-manager returned HTTP %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("artifact-manager returned %s: %s", e.Code, e.Message)
}

func NewArtifactClient(address string, tokens TokenSource, httpClient *http.Client) (*ArtifactClient, error) {
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("artifact-manager must be an http or https URL with scheme and host")
	}
	if tokens == nil || httpClient == nil {
		return nil, fmt.Errorf("token source and HTTP client are required")
	}
	return &ArtifactClient{baseURL: u, tokens: tokens, httpClient: httpClient}, nil
}

func (c *ArtifactClient) LogStatus(ctx context.Context, project, job, uid string) (LogStatus, error) {
	var out LogStatus
	q := url.Values{"jobUID": {uid}, "stream": {"combined"}}
	err := c.do(ctx, http.MethodGet, c.jobPath(project, job)+"/logs/status?"+q.Encode(), nil, nil, &out)
	return out, err
}

func (c *ArtifactClient) AppendLog(ctx context.Context, project, job, uid string, sequence int64, sum string, data []byte) (AppendLogResult, error) {
	headers := make(http.Header)
	headers.Set("Content-Type", "application/octet-stream")
	headers.Set("X-Job-UID", uid)
	headers.Set("X-Log-Stream", "combined")
	headers.Set("X-Log-Sequence", strconv.FormatInt(sequence, 10))
	headers.Set("X-Content-SHA256", sum)
	var out AppendLogResult
	err := c.do(ctx, http.MethodPost, c.jobPath(project, job)+"/logs/chunks", data, headers, &out)
	return out, err
}

func (c *ArtifactClient) CompleteLog(ctx context.Context, project, job, key string, input CompleteLogInput) (CompletedLog, error) {
	data, err := json.Marshal(input)
	if err != nil {
		return CompletedLog{}, err
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("Idempotency-Key", key)
	var out CompletedLog
	err = c.do(ctx, http.MethodPost, c.jobPath(project, job)+"/logs/complete", data, headers, &out)
	return out, err
}

func (c *ArtifactClient) jobPath(project, job string) string {
	return "/artifacts/v1/projects/" + url.PathEscape(project) + "/jobs/" + url.PathEscape(job)
}

func (c *ArtifactClient) do(ctx context.Context, method, path string, body []byte, headers http.Header, out any) error {
	makeRequest := func(token string) (*http.Request, error) {
		u := *c.baseURL
		parsed, err := url.Parse(path)
		if err != nil {
			return nil, err
		}
		u.Path = singleJoiningSlash(c.baseURL.Path, parsed.Path)
		u.RawQuery = parsed.RawQuery
		req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header = headers.Clone()
		if req.Header == nil {
			req.Header = make(http.Header)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		return req, nil
	}
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return fmt.Errorf("get runner token: %w", err)
	}
	req, err := makeRequest(token)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		_ = resp.Body.Close()
		token, err = c.tokens.RefreshAfterUnauthorized(ctx, token)
		if err != nil {
			return fmt.Errorf("refresh runner token: %w", err)
		}
		req, err = makeRequest(token)
		if err != nil {
			return err
		}
		resp, err = c.httpClient.Do(req)
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return artifactResponseError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode artifact-manager response: %w", err)
	}
	return nil
}

func artifactResponseError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var envelope struct {
		Code, Message string
		Retryable     bool
		Details       map[string]any
	}
	_ = json.Unmarshal(data, &envelope)
	if envelope.Message == "" {
		envelope.Message = strings.TrimSpace(string(data))
	}
	if envelope.Message == "" {
		envelope.Message = resp.Status
	}
	e := ArtifactAPIError{StatusCode: resp.StatusCode, Code: envelope.Code, Message: envelope.Message, Retryable: envelope.Retryable, Details: envelope.Details}
	if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
		e.RetryAfter = time.Duration(seconds) * time.Second
	}
	return e
}
