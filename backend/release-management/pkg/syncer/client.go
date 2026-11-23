package syncer

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	ebsv1 "ebs-api/ebs/v1"
)

// Failure messages contain only locally generated diagnostics, never upstream bodies or transport URLs.
type Failure struct {
	Message    string
	Blocked    bool
	Status     int
	RetryAfter time.Duration
}

func (e *Failure) Error() string { return e.Message }

func blocked(message string) error { return &Failure{Message: message, Blocked: true} }

func retry(message string) error { return &Failure{Message: message} }

type Project struct {
	Object   map[string]json.RawMessage
	Metadata struct {
		Name              string  `json:"name"`
		UID               string  `json:"uid"`
		ResourceVersion   string  `json:"resourceVersion"`
		DeletionTimestamp *string `json:"deletionTimestamp"`
	}
	Spec struct {
		DefaultRef   string              `json:"defaultRef"`
		PackageRepos []ebsv1.PackageRepo `json:"packageRepos"`
	}
}

type ProjectClient interface {
	Get(context.Context, Binding) (*Project, error)
	Patch(context.Context, Binding, []byte) (*Project, error)
}

type GatewayClient struct {
	base           string
	maxBytes       int64
	http           *http.Client
	credentialFile string
	token          tokenState
	now            func() time.Time
}

func NewGatewayClient(c Config) (*GatewayClient, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if c.GatewayCAFile != "" {
		pem, err := os.ReadFile(c.GatewayCAFile)
		if err != nil {
			return nil, fmt.Errorf("read Gateway CA failed")
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("invalid Gateway CA")
		}
		transport.TLSClientConfig.RootCAs = roots
	}
	return &GatewayClient{
		credentialFile: c.MachineCredentialFile,
		base:           strings.TrimRight(c.GatewayURL, "/"), maxBytes: c.MaxProjectBytes,
		http: &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

func (c *GatewayClient) Get(ctx context.Context, b Binding) (*Project, error) {
	return c.request(ctx, b, http.MethodGet, nil)
}

func (c *GatewayClient) Patch(ctx context.Context, b Binding, body []byte) (*Project, error) {
	return c.request(ctx, b, http.MethodPatch, body)
}

func (c *GatewayClient) request(ctx context.Context, b Binding, method string, body []byte) (*Project, error) {
	if int64(len(body)) > c.maxBytes {
		return nil, blocked("PATCH body exceeds maxProjectBytes")
	}
	response, err := c.send(ctx, b, method, body)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	return c.projectResponse(response)
}

func (c *GatewayClient) send(ctx context.Context, b Binding, method string, body []byte) (*http.Response, error) {
	value := ""
	var err error
	if method == http.MethodPatch {
		value, err = c.serviceToken(ctx, "")
		if err != nil {
			return nil, err
		}
	}
	for attempt := 0; ; attempt++ {
		request, err := http.NewRequestWithContext(ctx, method, c.base+"/apis/ebs/v1/projects/"+b.Project, bytes.NewReader(body))
		if err != nil {
			return nil, blocked("invalid Gateway request")
		}
		if method == http.MethodPatch {
			request.Header.Set("Authorization", "Bearer "+value)
		}
		request.Header.Set("Accept", "application/json")
		if method == http.MethodPatch {
			request.Header.Set("Content-Type", "application/merge-patch+json")
		}
		response, err := c.http.Do(request)
		if err != nil {
			return nil, retry("Gateway request failed")
		}
		if response.StatusCode != http.StatusUnauthorized || method != http.MethodPatch {
			return response, nil
		}
		if attempt == 1 {
			c.invalidateToken(value)
			return response, nil
		}
		response.Body.Close()
		value, err = c.serviceToken(ctx, value)
		if err != nil {
			return nil, err
		}
	}
}

func (c *GatewayClient) projectResponse(response *http.Response) (*Project, error) {
	limit := c.maxBytes
	if response.StatusCode != http.StatusOK {
		limit = 64 << 10
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, retry("Gateway response read failed")
	}
	if response.StatusCode != http.StatusOK {
		failure := &Failure{Status: response.StatusCode, Message: fmt.Sprintf("Gateway returned HTTP %d", response.StatusCode)}
		failure.Blocked = response.StatusCode != 409 && response.StatusCode != 429 && response.StatusCode < 500
		var payload struct {
			Error   string `json:"error"`
			Details struct {
				Causes []struct {
					Field string `json:"field"`
				} `json:"causes"`
			} `json:"details"`
		}
		_ = json.Unmarshal(data, &payload)
		if response.StatusCode == http.StatusUnprocessableEntity {
			var fields []string
			for _, cause := range payload.Details.Causes {
				if len(fields) == 8 {
					break
				}
				if len(cause.Field) <= 128 && diagnosticField.MatchString(cause.Field) {
					fields = append(fields, cause.Field)
				}
			}
			if len(fields) > 0 {
				failure.Message += "; invalid fields: " + strings.Join(fields, ", ")
			}
		}
		if response.StatusCode == 413 || response.StatusCode == 502 && payload.Error == "current object exceeds body limit" {
			failure.Blocked = true
			failure.Message = "Project exceeds Gateway request size limit"
		}
		if response.StatusCode == 429 || response.StatusCode == 503 {
			failure.RetryAfter = retryAfter(response.Header.Get("Retry-After"), time.Now())
		}
		return nil, failure
	}
	if int64(len(data)) > c.maxBytes {
		return nil, blocked("complete Project exceeds maxProjectBytes")
	}
	return decodeProject(data)
}

var diagnosticField = regexp.MustCompile(`^(spec|metadata)\.[A-Za-z0-9_.\[\]-]+$`)

func decodeProject(data []byte) (*Project, error) {
	p := &Project{}
	if json.Unmarshal(data, &p.Object) != nil || p.Object == nil || json.Unmarshal(p.Object["metadata"], &p.Metadata) != nil || json.Unmarshal(p.Object["spec"], &p.Spec) != nil ||
		p.Metadata.Name == "" || p.Metadata.UID == "" || p.Metadata.ResourceVersion == "" {
		return nil, retry("invalid Project response")
	}
	return p, nil
}

func retryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(value, 10, 32); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return date.Sub(now)
	}
	return 0
}

func preflight(p *Project, list []ebsv1.PackageRepo, maxBytes int64) ([]byte, error) {
	patch := map[string]any{
		"metadata": map[string]string{"uid": p.Metadata.UID, "resourceVersion": p.Metadata.ResourceVersion},
		"spec":     map[string]any{"packageRepos": list},
	}
	body, _ := json.Marshal(patch)
	var spec map[string]json.RawMessage
	if json.Unmarshal(p.Object["spec"], &spec) != nil || spec == nil {
		return nil, retry("invalid Project spec")
	}
	spec["packageRepos"], _ = json.Marshal(list)
	merged := make(map[string]json.RawMessage, len(p.Object))
	for key, value := range p.Object {
		merged[key] = value
	}
	merged["spec"], _ = json.Marshal(spec)
	complete, _ := json.Marshal(merged)
	old, _ := json.Marshal(p.Object)
	if int64(len(body)) > maxBytes || int64(len(complete)) > maxBytes || int64(len(old)) > maxBytes {
		return nil, blocked("complete Project or PATCH exceeds maxProjectBytes")
	}
	return body, nil
}

func failureOf(err error) *Failure {
	var failure *Failure
	if errors.As(err, &failure) {
		return failure
	}
	return &Failure{Message: "local operation failed"}
}
