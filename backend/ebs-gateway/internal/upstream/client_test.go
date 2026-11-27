package upstream

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestForwardPreservesResponseHeadersAndCompression(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Accept-Encoding") != "gzip" {
				t.Errorf("Accept-Encoding changed: %q", r.Header.Get("Accept-Encoding"))
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Retry-After", "5")
			w.Header().Set("Warning", `299 ebs-apiserver "example warning"`)
			w.Header().Set("Audit-Id", "audit-1")
			w.Header().Set("Connection", "X-Hop")
			w.Header().Set("X-Hop", "hop-only")
			w.WriteHeader(status)
			compressed := gzip.NewWriter(w)
			_, _ = io.WriteString(compressed, `{"message":"response"}`)
			_ = compressed.Close()
		}))
		client, err := New(server.URL, nil)
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		router := gin.New()
		router.GET("/test", client.Forward)
		request := httptest.NewRequest(http.MethodGet, "/test", nil)
		request.Header.Set("Accept-Encoding", "gzip")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		server.Close()
		if response.Code != status || response.Header().Get("Content-Encoding") != "gzip" ||
			response.Header().Get("Retry-After") != "5" ||
			response.Header().Get("Warning") != `299 ebs-apiserver "example warning"` ||
			response.Header().Get("Audit-Id") != "audit-1" || response.Header().Get("X-Hop") != "" {
			t.Fatalf("unexpected response: status=%d headers=%v", response.Code, response.Header())
		}
		decoded, err := gzip.NewReader(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(decoded)
		_ = decoded.Close()
		if err != nil || string(body) != `{"message":"response"}` {
			t.Fatalf("response body=%s err=%v", body, err)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestInternalRequestTimeout(t *testing.T) {
	for _, phase := range []string{"headers", "body"} {
		t.Run(phase, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if phase == "body" {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			client, err := New(server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			if client.http.Timeout != 30*time.Second {
				t.Fatal("missing default timeout")
			}
			client.http.Timeout = 50 * time.Millisecond
			response, err := client.Do(context.Background(), http.MethodGet, "/test", nil, nil)
			if phase == "body" {
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				_, err = io.ReadAll(response.Body)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("want timeout, got %v", err)
			}
		})
	}
}

func TestInternalRequestHonorsCallerCancellation(t *testing.T) {
	client, err := New("https://api.example", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Do(ctx, http.MethodGet, "/test", nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("want cancellation, got %v", err)
	}
}

func TestWatchDoesNotUseInternalRequestTimeout(t *testing.T) {
	client, err := New("https://api.example", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if _, ok := r.Context().Deadline(); ok {
			t.Error("proxy acquired an internal request deadline")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("event\n"))}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	client.http.Timeout = time.Nanosecond
	router := gin.New()
	router.GET("/watch", client.Forward)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/watch?watch=true", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "event\n" {
		t.Fatalf("watch interrupted: status=%d body=%s", recorder.Code, recorder.Body)
	}
}

func TestInternalRequestDoesNotForwardCallerCredentials(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/internal/iam/v1/users/authenticate" || r.URL.Query().Get("a") != "b" {
			t.Errorf("unexpected upstream URL %s", r.URL.String())
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("caller credentials reached upstream")
		}
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	client, err := New("https://api.example", transport)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(context.Background(), http.MethodPost, "/internal/iam/v1/users/authenticate?a=b", nil, http.Header{
		"Authorization": []string{"Bearer forged"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("unexpected status %d", response.StatusCode)
	}
}

func TestForwardDropsBearer(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("caller bearer token reached upstream: %v", r.Header)
		}
		if r.Header.Get("X-Client-Marker") != "ordinary value" {
			t.Errorf("ordinary request header was changed: %v", r.Header)
		}
		if r.Host != "api.example" || r.Header.Get("X-Forwarded-Host") != "" {
			t.Errorf("untrusted proxy host reached upstream: host=%q headers=%v", r.Host, r.Header)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	client, err := New("https://api.example", transport)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.GET("/test", client.Forward)
	request := httptest.NewRequest(http.MethodGet, "/test", nil)
	request.Header.Set("X-Client-Marker", "ordinary value")
	request.Header.Set("Authorization", "Bearer forged")
	request.Header.Set("X-Forwarded-Host", "evil.example")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "ok" {
		t.Fatalf("unexpected proxy result: status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestForwardResponseHeaderTimeout(t *testing.T) {
	for _, tc := range []struct {
		name               string
		watch, established bool
	}{
		{"waiting for headers", false, false},
		{"watch waiting for headers", true, false},
		{"established watch", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const headerTimeout = 100 * time.Millisecond
			canceled := make(chan struct{})
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(canceled)
				if tc.established {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					timer := time.NewTimer(3 * headerTimeout)
					defer timer.Stop()
					select {
					case <-timer.C:
						_, _ = io.WriteString(w, "event\n")
						w.(http.Flusher).Flush()
					case <-r.Context().Done():
						return
					}
				}
				<-r.Context().Done()
			}))
			defer backend.Close()
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.ResponseHeaderTimeout = headerTimeout
			defer transport.CloseIdleConnections()
			client, err := New(backend.URL, transport)
			if err != nil {
				t.Fatal(err)
			}
			// The internal client's total timeout must not apply to the proxy.
			client.http.Timeout = time.Nanosecond
			router := gin.New()
			router.GET("/jobs", client.Forward)
			proxy := httptest.NewServer(router)
			defer proxy.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			path := proxy.URL + "/jobs"
			if tc.watch {
				path += "?watch=true"
			}
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := proxy.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if tc.established {
				if response.StatusCode != http.StatusOK {
					t.Fatalf("watch status=%d", response.StatusCode)
				}
				event := make([]byte, len("event\n"))
				if _, err := io.ReadFull(response.Body, event); err != nil || string(event) != "event\n" {
					t.Fatalf("established watch interrupted: event=%q err=%v", event, err)
				}
				cancel()
				if _, err := io.ReadAll(response.Body); !errors.Is(err, context.Canceled) {
					t.Fatalf("watch cancellation error=%v", err)
				}
			} else {
				body, err := io.ReadAll(response.Body)
				if err != nil || response.StatusCode != http.StatusBadGateway || string(body) != "upstream unavailable\n" {
					t.Fatalf("status=%d body=%q err=%v", response.StatusCode, body, err)
				}
			}
			select {
			case <-canceled:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream request was not canceled")
			}
		})
	}
}
