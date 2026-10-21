package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestAuthenticationCacheLifetime(t *testing.T) {
	for _, tokenTTL := range []time.Duration{time.Minute, 2 * time.Hour} {
		t.Run(tokenTTL.String(), func(t *testing.T) {
			a, err := NewGatewayAuthorizer(Config{URL: "https://gateway", AuthCacheTTL: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(a.Close)
			expires := time.Now().Add(tokenTTL).UTC()
			calls := 0
			a.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				payload := fmt.Sprintf(`{"authenticated":true,"identity":{"type":"runner","name":"runner","scopes":[]},"expiresAt":%q}`, expires.Format(time.RFC3339Nano))
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewBufferString(payload))}, nil
			})
			before := time.Now()
			for i := 0; i < 2; i++ {
				identity, err := a.Authenticate(context.Background(), "token")
				if err != nil || identity.Name != "runner" {
					t.Fatalf("authenticate: %+v, %v", identity, err)
				}
			}
			if calls != 1 {
				t.Fatalf("gateway calls = %d, want 1", calls)
			}
			a.mu.Lock()
			entry := a.cache[sha256.Sum256([]byte("token"))]
			a.mu.Unlock()
			if tokenTTL < a.ttl {
				if !entry.until.Equal(expires) {
					t.Fatalf("cache expiry = %v, token expires at %v", entry.until, expires)
				}
			} else if entry.until.Before(before.Add(a.ttl)) || entry.until.After(time.Now().Add(a.ttl)) {
				t.Fatalf("cache expiry does not respect TTL: %v", entry.until)
			}
			a.removeExpired(entry.until)
			a.mu.Lock()
			remaining := len(a.cache)
			a.mu.Unlock()
			if remaining != 0 {
				t.Fatalf("expired entries remaining: %d", remaining)
			}
		})
	}
}

func TestAuthenticationCacheAutomaticallyRemovesExpiredEntries(t *testing.T) {
	a, err := NewGatewayAuthorizer(Config{URL: "https://gateway", AuthCacheTTL: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	now := time.Now()
	expiredKey := sha256.Sum256([]byte("expired"))
	activeKey := sha256.Sum256([]byte("active"))
	a.mu.Lock()
	a.cache[expiredKey] = cachedIdentity{identity: Identity{ExpiresAt: now.Add(time.Hour)}, until: now.Add(-time.Second)}
	a.cache[activeKey] = cachedIdentity{identity: Identity{ExpiresAt: now.Add(time.Hour)}, until: now.Add(time.Hour)}
	a.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for {
		a.mu.Lock()
		_, expiredExists := a.cache[expiredKey]
		_, activeExists := a.cache[activeKey]
		a.mu.Unlock()
		if !activeExists {
			t.Fatal("cleanup removed a valid entry")
		}
		if !expiredExists {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background cleanup did not remove expired entry")
		}
		time.Sleep(5 * time.Millisecond)
	}
	a.Close()
	a.Close()
	select {
	case <-a.done:
	default:
		t.Fatal("cleanup goroutine is still running")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.cache) != 0 {
		t.Fatal("close retained cached identities")
	}
}

func TestGatewayAuthorizerRequiresRunnerIdentity(t *testing.T) {
	for _, test := range []struct {
		name, identityType string
		scopes             string
		wantError          bool
	}{
		{name: "runner", identityType: "runner", scopes: `[]`},
		{name: "runner with missing scopes", identityType: "runner", scopes: `null`, wantError: true},
		{name: "user", identityType: "user", scopes: `["ebs:user"]`, wantError: true},
		{name: "runner with user scope", identityType: "runner", scopes: `["ebs:user"]`, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := fmt.Sprintf(`{"authenticated":true,"identity":{"type":%q,"name":"identity-1","scopes":%s},"expiresAt":%q}`, test.identityType, test.scopes, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			authorizer := &GatewayAuthorizer{
				url:   "https://gateway/auth/check",
				ttl:   time.Second,
				cache: make(map[[32]byte]cachedIdentity),
			}
			authorizer.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/auth/check" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				if r.Body != nil {
					body, _ := io.ReadAll(r.Body)
					if len(body) != 0 {
						t.Errorf("unexpected request body %q", body)
					}
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(bytes.NewBufferString(payload)),
				}, nil
			})}
			_, err := authorizer.Authenticate(context.Background(), "token")
			if (err != nil) != test.wantError {
				t.Fatalf("Authenticate error = %v, wantError %v", err, test.wantError)
			}
		})
	}
}
