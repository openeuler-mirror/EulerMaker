package runner

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	minRunnerTokenTTL = 5 * time.Minute
	maxRunnerTokenTTL = 24 * time.Hour
)

type TokenSource interface {
	Token(context.Context) (string, error)
	RefreshAfterUnauthorized(context.Context, string) (string, error)
}

type TokenProvider struct {
	baseURL    *url.URL
	httpClient *http.Client
	credential MachineCredential
	runner     string
	now        func() time.Time

	mu         sync.Mutex
	token      string
	expiresAt  time.Time
	refreshAt  time.Time
	refreshing bool
	wait       chan struct{}
	lastErr    error
	retryDelay time.Duration
}

func NewTokenProvider(gateway, runner string, credential MachineCredential, httpClient *http.Client) (*TokenProvider, error) {
	baseURL, err := parseGatewayURL(gateway)
	if err != nil {
		return nil, err
	}
	if !validDNS1123Label(runner) {
		return nil, fmt.Errorf("runner name must be a DNS1123 label")
	}
	if httpClient == nil {
		return nil, fmt.Errorf("http client is required")
	}
	if err := validateMachineCredential(credential); err != nil {
		return nil, err
	}
	return &TokenProvider{baseURL: baseURL, httpClient: httpClient, credential: credential, runner: runner, now: time.Now}, nil
}

func (p *TokenProvider) Token(ctx context.Context) (string, error) {
	return p.tokenFor(ctx, "")
}

func (p *TokenProvider) RefreshAfterUnauthorized(ctx context.Context, rejected string) (string, error) {
	if rejected == "" {
		return "", fmt.Errorf("rejected token is empty")
	}
	return p.tokenFor(ctx, rejected)
}

func (p *TokenProvider) tokenFor(ctx context.Context, rejected string) (string, error) {
	for {
		now := p.now()
		p.mu.Lock()
		if rejected != "" && p.token != "" && p.token != rejected && now.Before(p.expiresAt) {
			token := p.token
			p.mu.Unlock()
			return token, nil
		}
		if rejected == "" && p.token != "" && now.Before(p.refreshAt) && now.Before(p.expiresAt) {
			token := p.token
			p.mu.Unlock()
			return token, nil
		}
		if rejected != "" && p.token == rejected {
			p.token = ""
			p.expiresAt = time.Time{}
			p.refreshAt = time.Time{}
		}
		if p.refreshing {
			wait := p.wait
			p.mu.Unlock()
			select {
			case <-wait:
			case <-ctx.Done():
				return "", ctx.Err()
			}
			p.mu.Lock()
			if p.token != "" && p.now().Before(p.expiresAt) && (rejected == "" || p.token != rejected) {
				token := p.token
				p.mu.Unlock()
				return token, nil
			}
			err := p.lastErr
			p.mu.Unlock()
			if err != nil {
				return "", err
			}
			continue
		}

		oldToken, oldExpiresAt := p.token, p.expiresAt
		p.refreshing = true
		p.wait = make(chan struct{})
		p.mu.Unlock()

		startedAt := p.now()
		token, ttl, err := p.exchange(ctx)

		p.mu.Lock()
		if err == nil {
			p.token = token
			p.expiresAt = startedAt.Add(ttl)
			p.refreshAt = calculateRefreshAt(startedAt, ttl)
			p.retryDelay = 0
		} else if rejected == "" && oldToken != "" && p.now().Before(oldExpiresAt) {
			p.token = oldToken
			p.expiresAt = oldExpiresAt
			p.refreshAt = p.now().Add(p.nextRetryDelay(err))
		} else if err != nil {
			p.nextRetryDelay(err)
		}
		p.lastErr = err
		p.refreshing = false
		close(p.wait)
		if p.token != "" && p.now().Before(p.expiresAt) && (rejected == "" || p.token != rejected) {
			result := p.token
			p.mu.Unlock()
			return result, nil
		}
		p.mu.Unlock()
		if err != nil {
			return "", err
		}
	}
}

func (p *TokenProvider) nextRetryDelay(err error) time.Duration {
	var statusErr StatusError
	if errors.As(err, &statusErr) {
		if statusErr.Code == http.StatusBadRequest || statusErr.Code == http.StatusUnauthorized {
			p.retryDelay = 30 * time.Second
			return p.retryDelay
		}
		if statusErr.Code == http.StatusTooManyRequests && statusErr.RetryAfter > 0 {
			p.retryDelay = statusErr.RetryAfter
			return p.retryDelay
		}
	}
	if p.retryDelay < time.Second {
		p.retryDelay = time.Second
	} else if p.retryDelay < 30*time.Second {
		p.retryDelay *= 2
		if p.retryDelay > 30*time.Second {
			p.retryDelay = 30 * time.Second
		}
	}
	return p.retryDelay
}

func (p *TokenProvider) exchange(ctx context.Context) (string, time.Duration, error) {
	payload, _ := json.Marshal(map[string]string{"runner": p.runner})
	u := *p.baseURL
	u.Path = singleJoiningSlash(p.baseURL.Path, "/auth/runner-token")
	u.RawQuery = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(p.credential.ClientID, p.credential.ClientSecret)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", 0, responseError(resp)
	}
	var result struct {
		AccessToken string `json:"accessToken"`
		TokenType   string `json:"tokenType"`
		ExpiresIn   int64  `json:"expiresIn"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxCredentialFileSize+1))
	if err := decoder.Decode(&result); err != nil {
		return "", 0, fmt.Errorf("decode runner token response: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return "", 0, fmt.Errorf("decode runner token response: %w", err)
	}
	ttl := time.Duration(result.ExpiresIn) * time.Second
	if result.AccessToken == "" || result.TokenType != "Bearer" || ttl < minRunnerTokenTTL || ttl > maxRunnerTokenTTL {
		return "", 0, fmt.Errorf("invalid runner token response")
	}
	return result.AccessToken, ttl, nil
}

func calculateRefreshAt(startedAt time.Time, ttl time.Duration) time.Time {
	lead := ttl / 5
	if lead > 10*time.Minute {
		lead = 10 * time.Minute
	}
	var random [8]byte
	if _, err := cryptorand.Read(random[:]); err == nil && lead > 0 {
		jitterRange := uint64(lead / 4)
		if jitterRange > 0 {
			lead += time.Duration(binary.LittleEndian.Uint64(random[:]) % jitterRange)
		}
	}
	return startedAt.Add(ttl - lead)
}

func parseGatewayURL(gateway string) (*url.URL, error) {
	baseURL, err := url.Parse(gateway)
	if err != nil {
		return nil, fmt.Errorf("parse gateway url: %w", err)
	}
	if baseURL.Scheme == "" || baseURL.Host == "" || (baseURL.Scheme != "http" && baseURL.Scheme != "https") {
		return nil, fmt.Errorf("gateway must be an http or https URL with scheme and host")
	}
	if baseURL.User != nil {
		return nil, fmt.Errorf("gateway URL must not contain user info")
	}
	return baseURL, nil
}
