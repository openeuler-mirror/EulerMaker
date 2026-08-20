package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const maxResponseBytes = 32 << 20

type Options struct {
	Gateway            string
	Token              string
	CAFile             string
	ServerName         string
	InsecureSkipVerify bool
	Timeout            time.Duration
	Verbose            bool
	Diagnostic         io.Writer
}

type Client struct {
	base       *url.URL
	token      string
	http       *http.Client
	watchHTTP  *http.Client
	verbose    bool
	diagnostic io.Writer
}

type Identity struct {
	Type   string
	Name   string
	Scopes []string
}

type APIError struct {
	StatusCode int
	Code       string
	Message    string
	RequestID  string
	Resource   string
	Name       string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	parts := []string{}
	if e.Resource != "" {
		parts = append(parts, e.Resource)
	}
	if e.Name != "" {
		parts = append(parts, e.Name)
	}
	prefix := strings.Join(parts, "/")
	if prefix != "" {
		prefix += ": "
	}
	message := e.Message
	if message == "" {
		message = http.StatusText(e.StatusCode)
	}
	detail := fmt.Sprintf("HTTP %d", e.StatusCode)
	if e.Code != "" {
		detail += " code=" + e.Code
	}
	if e.RequestID != "" {
		detail += " requestID=" + e.RequestID
	}
	return prefix + message + " (" + detail + ")"
}

func New(options Options) (*Client, error) {
	base, err := url.Parse(options.Gateway)
	if err != nil {
		return nil, fmt.Errorf("parse gateway: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: options.ServerName}
	if options.InsecureSkipVerify {
		tlsConfig.InsecureSkipVerify = true
	}
	if options.CAFile != "" {
		data, err := os.ReadFile(options.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("CA file contains no certificates")
		}
		tlsConfig.RootCAs = pool
	}
	transport.TLSClientConfig = tlsConfig
	if options.Timeout == 0 {
		options.Timeout = 30 * time.Second
	}
	if options.Diagnostic == nil {
		options.Diagnostic = io.Discard
	}
	return &Client{
		base:       base,
		token:      options.Token,
		http:       &http.Client{Transport: transport, Timeout: options.Timeout},
		watchHTTP:  &http.Client{Transport: transport},
		verbose:    options.Verbose,
		diagnostic: options.Diagnostic,
	}, nil
}

func (c *Client) Do(ctx context.Context, method, path, contentType string, body []byte, resource, name string) ([]byte, http.Header, error) {
	attempts := 1
	if method == http.MethodGet || method == http.MethodHead {
		attempts = 3
	}
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		data, header, retry, err := c.doOnce(ctx, c.http, method, path, contentType, body, resource, name, maxResponseBytes)
		if err == nil {
			return data, header, nil
		}
		last = err
		if !retry || attempt+1 == attempts {
			break
		}
		delay := retryDelay(err, attempt)
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, nil, last
}

func (c *Client) OpenWatch(ctx context.Context, path, resource string) (*http.Response, error) {
	request, err := c.newRequest(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return nil, err
	}
	c.logRequest(request)
	response, err := c.watchHTTP.Do(request)
	if err != nil {
		return nil, networkError(err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return nil, decodeAPIError(response, data, resource, "")
	}
	return response, nil
}

func (c *Client) CheckIdentity(ctx context.Context) (Identity, error) {
	body, _, err := c.Do(ctx, http.MethodPost, "/auth/check", "application/json", nil, "identity", "")
	if err != nil {
		return Identity{}, err
	}
	var response struct {
		Authenticated bool     `json:"authenticated"`
		Identity      Identity `json:"identity"`
	}
	if err := json.Unmarshal(body, &response); err != nil || !response.Authenticated || response.Identity.Name == "" {
		return Identity{}, fmt.Errorf("Gateway returned an invalid identity response")
	}
	return response.Identity, nil
}

func (c *Client) doOnce(ctx context.Context, httpClient *http.Client, method, path, contentType string, body []byte, resource, name string, limit int64) ([]byte, http.Header, bool, error) {
	request, err := c.newRequest(ctx, method, path, contentType, body)
	if err != nil {
		return nil, nil, false, err
	}
	c.logRequest(request)
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, nil, isRetryableNetwork(err), networkError(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, nil, false, networkError(err)
	}
	if int64(len(data)) > limit {
		return nil, nil, false, fmt.Errorf("response body exceeds %d bytes", limit)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		apiError := decodeAPIError(response, data, resource, name)
		return nil, nil, response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusServiceUnavailable, apiError
	}
	return data, response.Header.Clone(), false, nil
}

func (c *Client) newRequest(ctx context.Context, method, path, contentType string, body []byte) (*http.Request, error) {
	reference, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	target := c.base.ResolveReference(reference)
	request, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Request-ID", uuid.NewString())
	request.Header.Set("User-Agent", "ebsctl/dev")
	return request, nil
}

func (c *Client) logRequest(request *http.Request) {
	if c.verbose {
		fmt.Fprintf(c.diagnostic, "> %s %s requestID=%s\n", request.Method, request.URL.Redacted(), request.Header.Get("X-Request-ID"))
	}
}

func decodeAPIError(response *http.Response, data []byte, resource, name string) error {
	value := struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"requestID"`
	}{}
	_ = json.Unmarshal(data, &value)
	if value.Message == "" {
		value.Message = strings.TrimSpace(string(data))
	}
	if value.RequestID == "" {
		value.RequestID = response.Header.Get("X-Request-ID")
	}
	return &APIError{StatusCode: response.StatusCode, Code: value.Code, Message: value.Message, RequestID: value.RequestID, Resource: resource, Name: name, RetryAfter: RetryAfter(response.Header)}
}

func retryDelay(err error, attempt int) time.Duration {
	var apiError *APIError
	if errors.As(err, &apiError) && apiError.RetryAfter > 0 {
		return apiError.RetryAfter
	}
	base := time.Duration(1<<attempt) * 200 * time.Millisecond
	return base + time.Duration(rand.Int63n(int64(base/2+1)))
}

func networkError(err error) error {
	return fmt.Errorf("network request failed: %w", err)
}

func isRetryableNetwork(err error) bool {
	var netError net.Error
	return errors.As(err, &netError)
}

func ExitClass(err error) int {
	var apiError *APIError
	if errors.As(err, &apiError) {
		switch apiError.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return 3
		case http.StatusConflict:
			return 5
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return 4
		default:
			return 1
		}
	}
	var netError net.Error
	if errors.As(err, &netError) || errors.Is(err, context.DeadlineExceeded) {
		return 4
	}
	return 1
}

func AddQuery(path string, values url.Values) string {
	if len(values) == 0 {
		return path
	}
	return path + "?" + values.Encode()
}

func ResourceVersion(object map[string]any) string {
	metadata, _ := object["metadata"].(map[string]any)
	return fmt.Sprint(metadata["resourceVersion"])
}

func RetryAfter(header http.Header) time.Duration {
	value := header.Get("Retry-After")
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if retryAt, err := http.ParseTime(value); err == nil {
		if delay := time.Until(retryAt); delay > 0 {
			return delay
		}
	}
	return 0
}
