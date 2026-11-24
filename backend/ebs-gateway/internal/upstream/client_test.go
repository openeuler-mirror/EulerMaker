package upstream

import (
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
	router.GET("/watch", func(c *gin.Context) { client.Forward(c, false) })
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
	router.GET("/test", func(c *gin.Context) { client.Forward(c, false) })
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
