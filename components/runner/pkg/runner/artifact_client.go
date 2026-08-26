package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
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

type ArtifactRecord struct {
	ID           string     `json:"id"`
	Project      string     `json:"project"`
	JobName      string     `json:"jobName"`
	JobUID       string     `json:"jobUID"`
	Category     string     `json:"category"`
	FileName     string     `json:"fileName"`
	RelativePath string     `json:"relativePath"`
	ContentType  string     `json:"contentType"`
	Size         int64      `json:"size"`
	SHA256       string     `json:"sha256"`
	State        string     `json:"state"`
	CompletedAt  *time.Time `json:"completedAt,omitempty"`
}

type UploadArtifactInput struct {
	JobUID       string `json:"jobUID"`
	Category     string `json:"category"`
	FileName     string `json:"fileName"`
	RelativePath string `json:"relativePath"`
	ContentType  string `json:"contentType"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
}

type ManifestFile struct {
	ArtifactID   string `json:"artifactID"`
	RelativePath string `json:"relativePath"`
	Category     string `json:"category"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
	Required     bool   `json:"required"`
}

type CompleteManifestInput struct {
	JobUID     string         `json:"jobUID"`
	Generation int64          `json:"generation"`
	Files      []ManifestFile `json:"files"`
}

type CompletedManifest struct {
	JobUID        string         `json:"jobUID"`
	Generation    int64          `json:"generation"`
	State         string         `json:"state"`
	ArtifactCount int            `json:"artifactCount"`
	Digest        string         `json:"digest"`
	Files         []ManifestFile `json:"files,omitempty"`
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

func (c *ArtifactClient) UploadArtifact(ctx context.Context, project, job, key, path string, input UploadArtifactInput) (ArtifactRecord, error) {
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return ArtifactRecord{}, fmt.Errorf("get runner token: %w", err)
	}
	result, unauthorized, err := c.uploadArtifactOnce(ctx, token, project, job, key, path, input)
	if !unauthorized {
		return result, err
	}
	token, err = c.tokens.RefreshAfterUnauthorized(ctx, token)
	if err != nil {
		return ArtifactRecord{}, fmt.Errorf("refresh runner token: %w", err)
	}
	result, _, err = c.uploadArtifactOnce(ctx, token, project, job, key, path, input)
	return result, err
}

func (c *ArtifactClient) uploadArtifactOnce(ctx context.Context, token, project, job, key, path string, input UploadArtifactInput) (ArtifactRecord, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return ArtifactRecord{}, false, err
	}
	defer file.Close()

	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	writeDone := make(chan error, 1)
	go func() {
		metadataHeader := make(textproto.MIMEHeader)
		metadataHeader.Set("Content-Disposition", `form-data; name="metadata"`)
		metadataHeader.Set("Content-Type", "application/json")
		part, writeErr := multipartWriter.CreatePart(metadataHeader)
		if writeErr == nil {
			writeErr = json.NewEncoder(part).Encode(input)
		}
		if writeErr == nil {
			fileHeader := make(textproto.MIMEHeader)
			fileHeader.Set("Content-Disposition", `form-data; name="file"; filename="artifact"`)
			fileHeader.Set("Content-Type", "application/octet-stream")
			part, writeErr = multipartWriter.CreatePart(fileHeader)
		}
		if writeErr == nil {
			_, writeErr = io.Copy(part, file)
		}
		if writeErr == nil {
			writeErr = multipartWriter.Close()
		}
		_ = writer.CloseWithError(writeErr)
		writeDone <- writeErr
	}()

	u := *c.baseURL
	u.Path = singleJoiningSlash(c.baseURL.Path, c.jobPath(project, job)+"/artifacts")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), reader)
	if err != nil {
		_ = reader.CloseWithError(err)
		return ArtifactRecord{}, false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	resp, requestErr := c.httpClient.Do(req)
	_ = reader.Close()
	writeErr := <-writeDone
	if requestErr != nil {
		return ArtifactRecord{}, false, requestErr
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return ArtifactRecord{}, true, artifactResponseError(resp)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ArtifactRecord{}, false, artifactResponseError(resp)
	}
	if writeErr != nil {
		return ArtifactRecord{}, false, writeErr
	}
	var out struct {
		Artifact ArtifactRecord `json:"artifact"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return ArtifactRecord{}, false, fmt.Errorf("decode artifact-manager response: %w", err)
	}
	return out.Artifact, false, nil
}

func (c *ArtifactClient) CompleteManifest(ctx context.Context, project, job, key string, input CompleteManifestInput) (CompletedManifest, error) {
	data, err := json.Marshal(input)
	if err != nil {
		return CompletedManifest{}, err
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("Idempotency-Key", key)
	var out CompletedManifest
	err = c.do(ctx, http.MethodPost, c.jobPath(project, job)+"/manifest/complete", data, headers, &out)
	return out, err
}

func (c *ArtifactClient) GetManifest(ctx context.Context, project, job, uid string, generation int64) (CompletedManifest, error) {
	q := url.Values{"jobUID": {uid}, "generation": {strconv.FormatInt(generation, 10)}}
	var out CompletedManifest
	err := c.do(ctx, http.MethodGet, c.jobPath(project, job)+"/manifest?"+q.Encode(), nil, nil, &out)
	if out.ArtifactCount == 0 && len(out.Files) > 0 {
		out.ArtifactCount = len(out.Files)
	}
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
