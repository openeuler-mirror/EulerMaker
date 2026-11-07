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

func TestEnsureIndicesDoesNotCreateWhenAliasExists(t *testing.T) {
	putRequests := 0
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodHead && strings.HasPrefix(req.URL.Path, "/_alias/") {
			return response(http.StatusOK, `{}`), nil
		}
		if req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/_alias/") {
			alias := strings.TrimPrefix(req.URL.Path, "/_alias/")
			return response(http.StatusOK, `{"index":{"aliases":{"`+alias+`":{"is_write_index":true}}}}`), nil
		}
		if req.Method == http.MethodPut {
			putRequests++
		}
		return response(http.StatusInternalServerError, `{}`), nil
	})}
	client := NewClientForTesting("http://elasticsearch", httpClient)
	if err := client.ensureIndices(); err != nil {
		t.Fatalf("ensure indices: %v", err)
	}
	if putRequests != 0 {
		t.Fatalf("created %d indices despite existing aliases", putRequests)
	}
}

func TestEnsureIAMIndices(t *testing.T) {
	created := map[string]bool{}
	mappings := map[string]string{}
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodHead {
			return response(http.StatusNotFound, ``), nil
		}
		if req.Method == http.MethodPut {
			index := strings.TrimPrefix(req.URL.Path, "/")
			created[index] = true
			body, _ := io.ReadAll(req.Body)
			mappings[index] = string(body)
			return response(http.StatusOK, `{"acknowledged":true}`), nil
		}
		if req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/_mapping") {
			index := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/"), "/_mapping") + "-v1"
			return response(http.StatusOK, `{"`+index+`":`+mappings[index]+`}`), nil
		}
		t.Fatalf("unexpected method %s", req.Method)
		return nil, nil
	})}
	client := NewClientForTesting("http://elasticsearch", httpClient)
	if err := client.EnsureIAMIndices(); err != nil {
		t.Fatalf("ensure IAM indices: %v", err)
	}
	for _, index := range []string{"ebs-users-v1", "ebs-machineaccounts-v1", "ebs-oauth-identity-claims-v1"} {
		if !created[index] {
			t.Errorf("index %s was not created", index)
		}
	}
	if created["ebs-projects-v1"] {
		t.Fatalf("core index unexpectedly created: %#v", created)
	}
	for index, mapping := range mappings {
		if index == "ebs-oauth-identity-claims-v1" {
			continue
		}
		if !strings.Contains(mapping, `"credential"`) {
			t.Errorf("IAM index %s does not contain credential mapping: %s", index, mapping)
		}
		if strings.Contains(mapping, `"phase"`) || strings.Contains(mapping, `"stage"`) {
			t.Errorf("IAM index %s unexpectedly maps status fields: %s", index, mapping)
		}
	}
}

func TestCoordinationAliasValidation(t *testing.T) {
	for _, body := range []string{`{}`, `{"a":{"aliases":{}},"b":{"aliases":{}}}`, `{"a":{"aliases":{"ebs-build-target-claims":{"is_write_index":false}}}}`} {
		c := NewClientForTesting("http://es", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response(200, body), nil })})
		if err := c.validateCoordinationAlias("ebs-build-target-claims"); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

func TestCoordinationIndexConcurrentInitialization(t *testing.T) {
	c := NewClientForTesting("http://es", &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.Method {
		case http.MethodHead:
			return response(404, ""), nil
		case http.MethodPut:
			return response(400, `{"error":{"type":"resource_already_exists_exception"}}`), nil
		default:
			return response(200, `{"ebs-build-target-claims-v1":{"aliases":{"ebs-build-target-claims":{"is_write_index":true}}}}`), nil
		}
	})})
	if err := c.ensureResourceIndices([]string{"buildclaim"}); err != nil {
		t.Fatal(err)
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
