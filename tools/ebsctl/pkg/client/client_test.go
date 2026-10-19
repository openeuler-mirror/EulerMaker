package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetAuthenticationFallback(t *testing.T) {
	for _, test := range []struct {
		name       string
		token      string
		publicRead bool
		firstCode  int
		secondCode int
		wantCalls  int
		wantCode   int
	}{
		{"public success", "expired-token", true, 401, 200, 2, 0},
		{"anonymous unauthorized", "expired-token", true, 401, 401, 2, 401},
		{"anonymous forbidden", "expired-token", true, 401, 403, 2, 403},
		{"anonymous unavailable", "expired-token", true, 401, 503, 2, 503},
		{"protected read", "expired-token", false, 401, 200, 1, 401},
		{"permission denied", "valid-token", true, 403, 200, 1, 403},
		{"already anonymous", "", true, 401, 200, 1, 401},
		{"authenticated success", "valid-token", true, 200, 200, 1, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				wantAuth := ""
				code := test.secondCode
				if call == 1 {
					code = test.firstCode
					if test.token != "" {
						wantAuth = "Bearer " + test.token
					}
				}
				if r.Header.Get("Authorization") != wantAuth || r.Method != http.MethodGet ||
					r.URL.RequestURI() != "/apis/ebs/v1/projects?limit=5" {
					t.Errorf("unexpected request: %s %s auth=%q", r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"))
				}
				w.Header().Set("X-Request-ID", "response-id")
				w.WriteHeader(code)
				_, _ = io.WriteString(w, `{}`)
			}))
			defer server.Close()
			diagnostic := &bytes.Buffer{}
			api, err := New(Options{Gateway: server.URL, Token: test.token, Diagnostic: diagnostic})
			if err != nil {
				t.Fatal(err)
			}
			body, header, err := api.Get(context.Background(), "/apis/ebs/v1/projects?limit=5", "project", "", test.publicRead)
			if test.wantCode == 0 {
				if err != nil || string(body) != `{}` || header.Get("X-Request-ID") != "response-id" {
					t.Fatalf("body=%q header=%v err=%v", body, header, err)
				}
			} else {
				var apiError *APIError
				if !errors.As(err, &apiError) || apiError.StatusCode != test.wantCode {
					t.Fatalf("expected HTTP %d, got %v", test.wantCode, err)
				}
			}
			if calls.Load() != int32(test.wantCalls) || api.token != test.token {
				t.Fatalf("calls=%d; client credentials preserved=%v", calls.Load(), api.token == test.token)
			}
			wantWarnings := 0
			if test.wantCalls == 2 {
				wantWarnings = 1
			}
			if strings.Count(diagnostic.String(), "retrying this public read anonymously") != wantWarnings {
				t.Fatalf("unexpected diagnostic: %q", diagnostic.String())
			}
		})
	}
}

func TestDoInjectsTokenAndRequestID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token-a" || r.Header.Get("X-Request-ID") == "" {
			t.Fatalf("unexpected headers: %#v", r.Header)
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	api, err := New(Options{Gateway: server.URL, Token: "token-a", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := api.Do(context.Background(), http.MethodGet, "/test", "", nil, "project", "")
	if err != nil || string(body) != `{"ok":true}` {
		t.Fatalf("unexpected response %q: %v", body, err)
	}
}

func TestGETRetriesServiceUnavailable(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) < 3 {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	api, _ := New(Options{Gateway: server.URL, Timeout: 3 * time.Second})
	if _, _, err := api.Do(context.Background(), http.MethodGet, "/test", "", nil, "project", ""); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 3 {
		t.Fatalf("expected 3 attempts, got %d", hits.Load())
	}
}

func TestAPIErrorDoesNotExposeAuthorization(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()
	api, _ := New(Options{Gateway: server.URL, Token: "sensitive-token", Timeout: time.Second})
	_, _, err := api.Do(context.Background(), http.MethodGet, "/test", "", nil, "project", "")
	if err == nil || strings.Contains(err.Error(), "sensitive-token") || ExitClass(err) != 3 {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/auth/check" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer token-a" {
			t.Fatalf("unexpected Authorization header: %q", r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{"authenticated":true,"identity":{"type":"user","name":"alice","scopes":["ebs:user"]}}`)
	}))
	defer server.Close()
	api, err := New(Options{Gateway: server.URL, Token: "token-a", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := api.CheckIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if identity.Type != "user" || identity.Name != "alice" || len(identity.Scopes) != 1 || identity.Scopes[0] != "ebs:user" {
		t.Fatalf("unexpected identity: %#v", identity)
	}
}
