package runner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTokenProviderExchangesCredential(t *testing.T) {
	credential := testMachineCredential()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientID, clientSecret, ok := r.BasicAuth()
		if !ok || clientID != credential.ClientID || clientSecret != credential.ClientSecret {
			t.Fatalf("unexpected basic credential")
		}
		if r.URL.Path != "/auth/runner-token" || r.Method != http.MethodPost {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var input map[string]string
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input["runner"] != "runner-a" {
			t.Fatalf("unexpected input %#v err=%v", input, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"runner-token","tokenType":"Bearer","expiresIn":3600}`))
	}))
	defer server.Close()

	provider, err := NewTokenProvider(server.URL, "runner-a", credential, server.Client())
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	token, err := provider.Token(context.Background())
	if err != nil || token != "runner-token" {
		t.Fatalf("token=%q err=%v", token, err)
	}
}

func TestTokenProviderCoalescesConcurrentExchange(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"accessToken":"runner-token","tokenType":"Bearer","expiresIn":3600}`))
	}))
	defer server.Close()
	provider, err := NewTokenProvider(server.URL, "runner-a", testMachineCredential(), server.Client())
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if token, err := provider.Token(context.Background()); err != nil || token != "runner-token" {
				t.Errorf("token=%q err=%v", token, err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("exchange calls=%d", calls.Load())
	}
}

func TestTokenProviderDoesNotInvalidateConcurrentReplacement(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		_, _ = w.Write([]byte(`{"accessToken":"token-` + string(rune('0'+n)) + `","tokenType":"Bearer","expiresIn":3600}`))
	}))
	defer server.Close()
	provider, err := NewTokenProvider(server.URL, "runner-a", testMachineCredential(), server.Client())
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	first, err := provider.Token(context.Background())
	if err != nil {
		t.Fatalf("first token: %v", err)
	}
	second, err := provider.RefreshAfterUnauthorized(context.Background(), first)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	again, err := provider.RefreshAfterUnauthorized(context.Background(), first)
	if err != nil || again != second {
		t.Fatalf("token=%q want=%q err=%v", again, second, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("exchange calls=%d", calls.Load())
	}
}

func TestTokenProviderRefreshesInRefreshWindow(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		_, _ = w.Write([]byte(`{"accessToken":"token-` + string(rune('0'+n)) + `","tokenType":"Bearer","expiresIn":3600}`))
	}))
	defer server.Close()
	provider, err := NewTokenProvider(server.URL, "runner-a", testMachineCredential(), server.Client())
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	now := time.Unix(1_800_000_000, 0)
	provider.now = func() time.Time { return now }
	first, err := provider.Token(context.Background())
	if err != nil {
		t.Fatalf("first token: %v", err)
	}
	provider.mu.Lock()
	now = provider.refreshAt.Add(time.Second)
	provider.mu.Unlock()
	second, err := provider.Token(context.Background())
	if err != nil {
		t.Fatalf("refreshed token: %v", err)
	}
	if first == second || calls.Load() != 2 {
		t.Fatalf("first=%q second=%q calls=%d", first, second, calls.Load())
	}
}

func testMachineCredential() MachineCredential {
	return MachineCredential{
		ClientID:     "runner-site-a",
		ClientSecret: base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("s", 32))),
	}
}
