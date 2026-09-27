package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestInternalRequestDoesNotForwardCallerCredentials(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/internal/iam/v1/authenticate" || r.URL.Query().Get("a") != "b" {
			t.Errorf("unexpected upstream URL %s", r.URL.String())
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-EBS-User") != "" {
			t.Error("caller credentials reached upstream")
		}
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	client, err := New("https://api.example", transport)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(context.Background(), http.MethodPost, "/internal/iam/v1/authenticate?a=b", nil, http.Header{
		"Authorization": []string{"Bearer forged"},
		"X-EBS-User":    []string{"forged"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("unexpected status %d", response.StatusCode)
	}
}

func TestForwardReplacesIdentity(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-EBS-User") != "alice" || r.Header.Get("X-EBS-Scopes") != "ebs:user" || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected forwarded identity: %v", r.Header)
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
	router.GET("/test", func(c *gin.Context) { client.Forward(c, "alice", "ebs:user", false) })
	request := httptest.NewRequest(http.MethodGet, "/test", nil)
	request.Header.Set("X-EBS-User", "mallory")
	request.Header.Set("Authorization", "Bearer forged")
	request.Header.Set("X-Forwarded-Host", "evil.example")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "ok" {
		t.Fatalf("unexpected proxy result: status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}
