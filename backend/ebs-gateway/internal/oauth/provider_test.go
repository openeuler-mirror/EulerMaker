package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProviderTransportBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, target, body string
		status             int
	}{
		{"untrusted", "https://untrusted.example/token", `{}`, 200},
		{"redirect", "https://id.example.com/token", `{}`, 302},
		{"large response", "https://id.example.com/token", strings.Repeat("x", (1<<20)+1), 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			p := newProvider(testConfig("https://id.example.com"), time.Now, transportFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Location": {"https://untrusted.example/token"}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}))
			req, _ := http.NewRequest("POST", tc.target, nil)
			var out map[string]any
			if p.request(req, &out) == nil {
				t.Fatal("accepted unsafe provider response")
			}
			if calls > 1 || (tc.name == "untrusted" && calls != 0) {
				t.Fatal("redirect/retry leaked request", calls)
			}
		})
	}
}

func TestJWKSRefreshBoundedAndMerged(t *testing.T) {
	var calls atomic.Int32
	now := time.Now()
	transport := &keyTransport{now: func() time.Time { return now }, base: transportFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"keys":[]}`))}, nil
	})}
	var group sync.WaitGroup
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			req, _ := http.NewRequest("GET", "https://id.example.com/keys", nil)
			result, err := transport.RoundTrip(req)
			if err != nil {
				t.Error(err)
				return
			}
			result.Body.Close()
		}()
	}
	group.Wait()
	if calls.Load() != 1 {
		t.Fatal("unbounded JWKS refresh", calls.Load())
	}
	now = now.Add(31 * time.Second)
	req, _ := http.NewRequest("GET", "https://id.example.com/keys", nil)
	result, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	result.Body.Close()
	if calls.Load() != 2 {
		t.Fatal("key rotation was never refreshed")
	}
}

func TestDiscoveryFailureRetriesWithoutBlockingOtherServices(t *testing.T) {
	now := time.Now()
	calls := 0
	p := newProvider(testConfig("https://id.example.com"), func() time.Time { return now }, transportFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, fmt.Errorf("offline")
	}))
	for range 3 {
		if _, _, err := p.ensure(context.Background()); err == nil {
			t.Fatal("provider unexpectedly ready")
		}
	}
	if calls != 1 {
		t.Fatal("discovery retried without backoff")
	}
	now = now.Add(31 * time.Second)
	p.ensure(context.Background())
	if calls != 2 {
		t.Fatal("discovery did not recover its retry opportunity")
	}
}

func TestOAuthUserInfoSubAdapter(t *testing.T) {
	cfg := testConfig("https://id.example.com")
	cfg.Provider.Protocol, cfg.Provider.Adapter = "oauth2", "userinfo-sub"
	cfg.Provider.AuthorizationURL, cfg.Provider.TokenURL, cfg.Provider.UserInfoURL = "https://id.example.com/auth", "https://id.example.com/token", "https://id.example.com/user"
	cfg.Provider.TokenEndpointAuthMethod = "client_secret_post"
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	calls := 0
	p := newProvider(cfg, time.Now, transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		var body string
		if r.URL.Path == "/token" {
			r.ParseForm()
			if r.Form.Get("client_secret") != "secret" || r.Form.Get("code_verifier") != "verifier" || r.Header.Get("Authorization") != "" {
				t.Fatal("incorrect token authentication")
			}
			body = `{"access_token":"access","token_type":"Bearer"}`
		} else {
			if r.Header.Get("Authorization") != "Bearer access" {
				t.Fatal("missing userinfo authentication")
			}
			body = `{"sub":"immutable-id","name":"Example","email":"unverified@example.com","email_verified":false}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	external, profile, err := p.exchange(context.Background(), "code", browserContext{Verifier: "verifier"}, "")
	if err != nil || external.Subject != "immutable-id" || profile.Email != "" || calls != 2 {
		t.Fatalf("identity=%+v profile=%+v err=%v", external, profile, err)
	}
}

func TestOIDCRejectsInvalidSignature(t *testing.T) {
	f := newFixture(t)
	cookie, state := f.login(t)
	base := f.h.provider.client.Transport
	f.h.provider.client.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		response, err := base.RoundTrip(r)
		if err != nil || r.URL.Path != "/token" {
			return response, err
		}
		var token map[string]any
		json.NewDecoder(response.Body).Decode(&token)
		response.Body.Close()
		parts := strings.Split(token["id_token"].(string), ".")
		parts[2] = strings.Repeat("A", len(parts[2]))
		token["id_token"] = strings.Join(parts, ".")
		data, _ := json.Marshal(token)
		response.Body = io.NopCloser(strings.NewReader(string(data)))
		return response, nil
	})
	result := serve(f.h, "POST", "/auth/oauth/community/callback", `{"code":"code","state":"`+state+`"}`, cookie)
	if result.Code != 401 || f.accounts.authCalls != 0 {
		t.Fatal(result.Body.String())
	}
}
