package syncer

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func assertTokenExchange(t *testing.T, r *http.Request) {
	t.Helper()
	name, secret, ok := r.BasicAuth()
	raw, err := base64.RawURLEncoding.DecodeString(secret)
	body, readErr := io.ReadAll(r.Body)
	if r.Method != http.MethodPost || r.URL.Path != "/auth/machine-token" ||
		r.Header.Get("Content-Type") != "application/json" || !ok || name != "service" ||
		err != nil || len(raw) != 32 || readErr != nil || string(body) != "{}" {
		t.Error("invalid machine token request")
	}
}

func TestServiceTokenCacheRefreshAndRotation(t *testing.T) {
	c := testConfig(t)
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertTokenExchange(t, r)
		fmt.Fprintf(w, `{"accessToken":"token-%d","tokenType":"Bearer","expiresIn":3600}`, calls.Add(1))
	}))
	defer server.Close()
	now := time.Now()
	client := &GatewayClient{credentialFile: c.MachineCredentialFile, base: server.URL, http: server.Client(), now: func() time.Time { return now }}
	file := c.MachineCredentialFile
	check := func(want string, rejected string) {
		t.Helper()
		got, err := client.serviceToken(context.Background(), rejected)
		if err != nil || got != want {
			t.Fatalf("token=%q error=%v want=%q", got, err, want)
		}
	}
	check("token-1", "")
	check("token-1", "")
	now = now.Add(55 * time.Minute)
	check("token-2", "")
	// A late rejection of the previous token must not discard a refreshed token.
	check("token-2", "token-1")
	writeCredential(t, file, "service", "r")
	check("token-3", "")
	if calls.Load() != 3 {
		t.Fatalf("exchanges=%d", calls.Load())
	}
}

func TestServiceTokenConcurrentExchange(t *testing.T) {
	c := testConfig(t)
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertTokenExchange(t, r)
		calls.Add(1)
		fmt.Fprint(w, `{"accessToken":"shared-token","tokenType":"Bearer","expiresIn":3600}`)
	}))
	defer server.Close()
	client := &GatewayClient{credentialFile: c.MachineCredentialFile, base: server.URL, http: server.Client()}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			got, err := client.serviceToken(context.Background(), "")
			if err != nil || got != "shared-token" {
				t.Errorf("concurrent token exchange failed: %v", err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("exchanges=%d", calls.Load())
	}
}

func TestServiceTokenExchangeFailures(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		body    string
		blocked bool
	}{
		{"credentials rejected", 401, "private-secret", true},
		{"forbidden", 403, "private-secret", true},
		{"limited", 429, "private-secret", false},
		{"unavailable", 503, "private-secret", false},
		{"invalid JSON", 200, "private-secret", false},
		{"missing token", 200, `{"tokenType":"Bearer","expiresIn":3600}`, false},
		{"wrong type", 200, `{"accessToken":"private-secret","tokenType":"Basic","expiresIn":3600}`, false},
		{"invalid ttl", 200, `{"accessToken":"private-secret","tokenType":"Bearer","expiresIn":0}`, false},
		{"fractional ttl", 200, `{"accessToken":"private-secret","tokenType":"Bearer","expiresIn":300.5}`, false},
		{"header injection", 200, `{"accessToken":"private-secret\r\n","tokenType":"Bearer","expiresIn":3600}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testConfig(t)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			client := &GatewayClient{credentialFile: c.MachineCredentialFile, base: server.URL, http: server.Client()}
			value, err := client.serviceToken(context.Background(), "")
			if err == nil || value != "" || failureOf(err).Blocked != test.blocked || strings.Contains(err.Error(), "private-secret") {
				t.Fatalf("unexpected token failure: %v", err)
			}
			if test.status == 429 && failureOf(err).RetryAfter != 7*time.Second {
				t.Fatal("missing exchange retry delay")
			}
		})
	}
}

func TestMachineCredentialValidation(t *testing.T) {
	c := testConfig(t)
	file := c.MachineCredentialFile
	secret := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("s", 32)))
	for _, content := range []string{
		"owner-token", `null`, `{}`, `{"clientID":"service","clientSecret":"short"}`,
		`{"clientID":"service","clientSecret":"` + secret + `","extra":true}`,
		`{"clientID":"service","clientID":"other","clientSecret":"` + secret + `"}`,
		`{"clientID":"service","clientSecret":"` + secret + `"} {}`,
	} {
		if err := os.WriteFile(file, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readMachineCredential(file); err == nil || !failureOf(err).Blocked {
			t.Fatal("invalid machine credential accepted")
		}
	}
}

func TestTokenExchangeWaitCancellation(t *testing.T) {
	c := testConfig(t)
	client := &GatewayClient{credentialFile: c.MachineCredentialFile}
	state := &client.token
	state.flight = &tokenFlight{done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.serviceToken(ctx, "")
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled waiter succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter stuck behind token exchange")
	}
}
