package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const apiPrefix = "/apis/ebs/v1"

type Client struct {
	baseURL    *url.URL
	httpClient *http.Client
	token      string
}

func NewClient(gateway, token string, httpClient *http.Client) (*Client, error) {
	baseURL, err := url.Parse(gateway)
	if err != nil {
		return nil, fmt.Errorf("parse gateway url: %w", err)
	}
	if baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, fmt.Errorf("gateway must include scheme and host")
	}
	return &Client{baseURL: baseURL, httpClient: httpClient, token: token}, nil
}

func (c *Client) GetRunner(ctx context.Context, name string) (*RunnerResource, error) {
	var runner RunnerResource
	if err := c.doJSON(ctx, http.MethodGet, apiPrefix+"/runners/"+url.PathEscape(name), nil, &runner); err != nil {
		return nil, err
	}
	return &runner, nil
}

func (c *Client) CreateRunner(ctx context.Context, runner RunnerResource) error {
	return c.doJSON(ctx, http.MethodPost, apiPrefix+"/runners", runner, nil)
}

func (c *Client) UpdateRunner(ctx context.Context, runner RunnerResource) error {
	path := apiPrefix + "/runners/" + url.PathEscape(runner.Metadata.Name)
	return c.doJSON(ctx, http.MethodPut, path, runner, nil)
}

func (c *Client) PatchRunnerStatus(ctx context.Context, name string, status RunnerStatus) error {
	body := map[string]any{"status": status}
	path := apiPrefix + "/runners/" + url.PathEscape(name) + "/status"
	return c.doMergePatch(ctx, path, body, nil)
}

func (c *Client) PatchJobStatus(ctx context.Context, project, name string, status JobStatus) error {
	body := map[string]any{"status": status}
	path := apiPrefix + "/projects/" + url.PathEscape(project) + "/jobs/" + url.PathEscape(name) + "/status"
	return c.doMergePatch(ctx, path, body, nil)
}

func (c *Client) WatchJobs(ctx context.Context, resourceVersion string) (<-chan WatchEvent, <-chan error) {
	events := make(chan WatchEvent)
	errs := make(chan error, 1)

	go func() {
		defer close(events)
		defer close(errs)

		values := url.Values{"watch": []string{"true"}}
		if resourceVersion != "" {
			values.Set("resourceVersion", resourceVersion)
		}
		req, err := c.newRequest(ctx, http.MethodGet, apiPrefix+"/jobs?"+values.Encode(), nil)
		if err != nil {
			errs <- err
			return
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			errs <- err
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			errs <- responseError(resp)
			return
		}

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			var event WatchEvent
			if err := json.Unmarshal(line, &event); err != nil {
				errs <- fmt.Errorf("decode watch event: %w", err)
				return
			}
			select {
			case events <- event:
			case <-ctx.Done():
				errs <- ctx.Err()
				return
			}
		}
		if err := scanner.Err(); err != nil {
			errs <- err
		}
	}()

	return events, errs
}

func (c *Client) doMergePatch(ctx context.Context, path string, body any, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPatch, path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/merge-patch+json")
	return c.do(req, out)
}

func (c *Client) doJSON(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := c.newRequest(ctx, method, path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return responseError(resp)
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	u := *c.baseURL
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		parsed, err := url.Parse(path)
		if err != nil {
			return nil, err
		}
		u = *parsed
	} else {
		parsed, err := url.Parse(path)
		if err != nil {
			return nil, err
		}
		u.Path = singleJoiningSlash(c.baseURL.Path, parsed.Path)
		u.RawQuery = parsed.RawQuery
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func responseError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(data))
	if msg == "" {
		msg = resp.Status
	}
	return StatusError{Code: resp.StatusCode, Status: resp.Status, Message: msg}
}

type StatusError struct {
	Code    int
	Status  string
	Message string
}

func (e StatusError) Error() string {
	return fmt.Sprintf("gateway returned %s: %s", e.Status, e.Message)
}

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	default:
		return a + b
	}
}
