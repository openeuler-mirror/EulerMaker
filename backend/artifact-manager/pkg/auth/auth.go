package auth

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type Identity struct {
	Name      string
	ExpiresAt time.Time
}

type Authorizer interface {
	Authenticate(context.Context, string) (Identity, error)
}

type Config struct {
	URL                string
	CA                 string
	InsecureSkipVerify bool
	AuthCacheTTL       time.Duration
}

type GatewayAuthorizer struct {
	url       string
	client    *http.Client
	ttl       time.Duration
	mu        sync.Mutex
	cache     map[[32]byte]cachedIdentity
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

type cachedIdentity struct {
	identity Identity
	until    time.Time
}

func NewGatewayAuthorizer(c Config) (*GatewayAuthorizer, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tc := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: c.InsecureSkipVerify}
	if c.CA != "" {
		b, e := os.ReadFile(c.CA)
		if e != nil {
			return nil, e
		}
		p := x509.NewCertPool()
		if !p.AppendCertsFromPEM(b) {
			return nil, fmt.Errorf("invalid gateway CA")
		}
		tc.RootCAs = p
	}
	tr.TLSClientConfig = tc
	a := &GatewayAuthorizer{
		url:    strings.TrimRight(c.URL, "/") + "/auth/check",
		client: &http.Client{Transport: tr, Timeout: 10 * time.Second},
		ttl:    c.AuthCacheTTL,
		cache:  make(map[[32]byte]cachedIdentity),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go a.cleanupLoop()
	return a, nil
}

func (a *GatewayAuthorizer) cleanupLoop() {
	defer close(a.done)
	interval := time.Minute
	if a.ttl > 0 && a.ttl < interval {
		interval = a.ttl
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-a.stop:
			return
		case now := <-ticker.C:
			a.removeExpired(now)
		}
	}
}

func (a *GatewayAuthorizer) removeExpired(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, entry := range a.cache {
		if !now.Before(entry.until) || !now.Before(entry.identity.ExpiresAt) {
			delete(a.cache, key)
		}
	}
}

// Close stops cache cleanup and releases cached identities and idle connections.
func (a *GatewayAuthorizer) Close() {
	a.closeOnce.Do(func() {
		close(a.stop)
		<-a.done
		a.mu.Lock()
		clear(a.cache)
		a.mu.Unlock()
		a.client.CloseIdleConnections()
	})
}

func (a *GatewayAuthorizer) Authenticate(ctx context.Context, token string) (Identity, error) {
	key := sha256.Sum256([]byte(token))
	now := time.Now()
	a.mu.Lock()
	if entry, ok := a.cache[key]; ok && now.Before(entry.until) && now.Before(entry.identity.ExpiresAt) {
		a.mu.Unlock()
		return entry.identity, nil
	}
	delete(a.cache, key)
	a.mu.Unlock()
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, a.url, nil)
	if e != nil {
		return Identity{}, e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, e := a.client.Do(req)
	if e != nil {
		return Identity{}, e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		io.Copy(io.Discard, res.Body)
		return Identity{}, fmt.Errorf("token rejected")
	}
	var out struct {
		Authenticated bool `json:"authenticated"`
		Identity      struct {
			Type   string   `json:"type"`
			Name   string   `json:"name"`
			Scopes []string `json:"scopes"`
		} `json:"identity"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if json.NewDecoder(res.Body).Decode(&out) != nil || !out.Authenticated || out.Identity.Type != "runner" || out.Identity.Name == "" || out.Identity.Scopes == nil || len(out.Identity.Scopes) != 0 {
		return Identity{}, fmt.Errorf("invalid auth response")
	}
	identity := Identity{Name: out.Identity.Name, ExpiresAt: out.ExpiresAt}
	until := now.Add(a.ttl)
	if out.ExpiresAt.Before(until) {
		until = out.ExpiresAt
	}
	if until.After(time.Now()) {
		a.mu.Lock()
		select {
		case <-a.stop:
		default:
			a.cache[key] = cachedIdentity{identity: identity, until: until}
		}
		a.mu.Unlock()
	}
	return identity, nil
}
