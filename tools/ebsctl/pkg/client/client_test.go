package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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
