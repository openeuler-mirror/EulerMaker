package es

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestAPIKeyAuthenticatesPingAndAliasCheck(t *testing.T) {
	var methods []string
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		methods = append(methods, req.Method)
		if got := req.Header.Get("Authorization"); got != "ApiKey encoded-key" {
			t.Errorf("Authorization = %q, want API key", got)
		}
		return response(http.StatusOK, `{}`), nil
	})}
	client := NewClientForTesting("http://elasticsearch", httpClient)
	client.apiKey = "encoded-key"
	if err := client.ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := client.ensureResourceIndices([]string{"project"}); err != nil {
		t.Fatalf("ensure index: %v", err)
	}
	if len(methods) != 2 || methods[0] != http.MethodGet || methods[1] != http.MethodHead {
		t.Fatalf("request methods = %v, want GET and HEAD", methods)
	}
}

func TestHTTPErrorStatus(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return response(http.StatusNotFound, "missing"), nil
	})}
	client := NewClientForTesting("http://elasticsearch", httpClient)
	_, err := client.Get(context.Background(), "project", "missing")
	if !IsStatus(err, http.StatusNotFound) {
		t.Fatalf("expected 404 HTTPError, got %v", err)
	}
}

func TestIsDocumentNotFound(t *testing.T) {
	for _, test := range []struct {
		body   string
		absent bool
	}{
		{`{"_index":"ebs-oauth-identity-claims-v1","_id":"id","found":false}`, true},
		{`{"error":{"type":"index_not_found_exception"},"status":404}`, false},
		{`{"found":false,"error":{"type":"index_not_found_exception"}}`, false},
		{`{"found":true}`, false},
		{`404 not found`, false},
	} {
		err := &HTTPError{StatusCode: 404, Body: test.body}
		if got := IsDocumentNotFound(err); got != test.absent {
			t.Errorf("IsDocumentNotFound(%s)=%t, want %t", test.body, got, test.absent)
		}
	}
}
