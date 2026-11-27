package upstream

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Client owns all traffic from the gateway to the API server. Its internal
// requests never reuse caller-supplied bearer credentials.
type Client struct {
	endpoint *url.URL
	http     *http.Client
	proxy    *httputil.ReverseProxy
}

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
	return &Client{
		endpoint: endpoint,
		// Internal lookups and mutations are bounded through response body reads.
		// The streaming reverse proxy does not use this client's timeout.
		http:  &http.Client{Transport: transport, Timeout: 30 * time.Second},
		proxy: proxy,
	}, nil
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
		if strings.EqualFold(name, "Authorization") {
			continue
		}
		request.Header[name] = append([]string(nil), values...)
	}
	return c.http.Do(request)
}

func (c *Client) Forward(ctx *gin.Context) {
	request := ctx.Request
	request.Header.Del("Authorization")
	request.Header.Del("Proxy-Authorization")
	request.Header.Del("Forwarded")
	request.Header.Del("X-Forwarded-For")
	request.Header.Del("X-Forwarded-Host")
	request.Header.Del("X-Forwarded-Proto")
	request.Host = c.endpoint.Host
	c.proxy.ServeHTTP(responseWriter{ctx.Writer}, request)
}

// ReverseProxy probes optional net/http writer interfaces. Wrapping Gin's
// writer keeps those probes aligned with the actual underlying connection.
type responseWriter struct {
	http.ResponseWriter
}

func (w responseWriter) Flush()                      { _ = http.NewResponseController(w.ResponseWriter).Flush() }
func (w responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
