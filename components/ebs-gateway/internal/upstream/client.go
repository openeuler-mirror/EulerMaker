package upstream

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

// Client owns all traffic from the gateway to the API server. Its internal
// requests never reuse caller-supplied authentication or identity headers.
type Client struct {
	endpoint *url.URL
	http     *http.Client
	proxy    *httputil.ReverseProxy
}

type publicRequestKey struct{}

func New(address string, transport http.RoundTripper) (*Client, error) {
	endpoint, err := url.Parse(address)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, fmt.Errorf("invalid API server address %q", address)
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	proxy := httputil.NewSingleHostReverseProxy(endpoint)
	proxy.Transport = transport
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}
	proxy.ModifyResponse = func(response *http.Response) error {
		if response.Request == nil || response.Request.Context().Value(publicRequestKey{}) != true {
			return nil
		}
		for name := range response.Header {
			switch http.CanonicalHeaderKey(name) {
			case "Content-Type", "Cache-Control", "X-Request-Id":
			default:
				response.Header.Del(name)
			}
		}
		return nil
	}
	return &Client{endpoint: endpoint, http: &http.Client{Transport: transport}, proxy: proxy}, nil
}

func (c *Client) Do(ctx context.Context, method, path string, body io.Reader, headers http.Header) (*http.Response, error) {
	reference, err := url.Parse(path)
	if err != nil || reference.IsAbs() || !strings.HasPrefix(reference.Path, "/") {
		return nil, fmt.Errorf("invalid upstream path %q", path)
	}
	target := *c.endpoint
	target.Path = strings.TrimRight(c.endpoint.Path, "/") + reference.Path
	target.RawQuery = reference.RawQuery
	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	for name, values := range headers {
		if strings.EqualFold(name, "Authorization") || strings.HasPrefix(strings.ToLower(name), "x-ebs-") {
			continue
		}
		request.Header[name] = append([]string(nil), values...)
	}
	return c.http.Do(request)
}

func (c *Client) Forward(ctx *gin.Context, subject, kind, scope string, public bool) {
	request := ctx.Request
	if public {
		request = request.WithContext(context.WithValue(request.Context(), publicRequestKey{}, true))
	}
	for name := range request.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-ebs-") {
			request.Header.Del(name)
		}
	}
	request.Header.Del("Authorization")
	request.Header.Del("Proxy-Authorization")
	request.Header.Del("Forwarded")
	request.Header.Del("X-Forwarded-For")
	request.Header.Del("X-Forwarded-Host")
	request.Header.Del("X-Forwarded-Proto")
	request.Host = c.endpoint.Host
	if !public {
		request.Header.Set("X-EBS-User", subject)
		request.Header.Set("X-EBS-Type", kind)
		if scope != "" {
			request.Header.Set("X-EBS-Scopes", scope)
		}
	} else {
		// The public response header allowlist intentionally excludes
		// Content-Encoding. Let the transport negotiate/decode gzip itself.
		request.Header.Del("Accept-Encoding")
	}
	var writer http.ResponseWriter = responseWriter{ctx.Writer}
	if request.Method == http.MethodHead {
		writer = headWriter{writer}
	}
	c.proxy.ServeHTTP(writer, request)
}

// ReverseProxy probes optional net/http writer interfaces. Wrapping Gin's
// writer keeps those probes aligned with the actual underlying connection.
type responseWriter struct{ http.ResponseWriter }

func (w responseWriter) Flush()                      { _ = http.NewResponseController(w.ResponseWriter).Flush() }
func (w responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type headWriter struct{ http.ResponseWriter }

func (w headWriter) Write(data []byte) (int, error) { return len(data), nil }
