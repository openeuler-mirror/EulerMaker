package syncer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type machineCredential struct {
	ClientID     string `json:"clientID"`
	ClientSecret string `json:"clientSecret"`
}

type cachedToken struct {
	value        string
	credential   [32]byte
	refreshAfter time.Time
}

type tokenFlight struct {
	done       chan struct{}
	credential [32]byte
	err        error
}

type tokenState struct {
	mu     sync.Mutex
	token  cachedToken
	flight *tokenFlight
}

func readMachineCredential(filename string) (machineCredential, [32]byte, error) {
	var credential machineCredential
	var fingerprint [32]byte
	f, err := os.Open(filename)
	if err != nil {
		return credential, fingerprint, blocked("machine credential file cannot be read")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return credential, fingerprint, blocked("machine credential file exceeds size limit or cannot be read")
	}
	// Require one JSON object with unique keys, using the existing strict YAML tree inspection.
	value, err := decodeYAML(data)
	if err != nil || !json.Valid(data) {
		return credential, fingerprint, blocked("invalid machine credential JSON")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return credential, fingerprint, blocked("invalid machine credential JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&credential) != nil || credential.ClientID == "" || len(credential.ClientID) > 63 ||
		!projectName.MatchString(credential.ClientID) {
		return credential, fingerprint, blocked("invalid machine credential fields")
	}
	secret, err := base64.RawURLEncoding.Strict().DecodeString(credential.ClientSecret)
	if err != nil || len(secret) < 32 || len(credential.ClientSecret) > 256 {
		return credential, fingerprint, blocked("invalid machine credential secret")
	}
	return credential, sha256.Sum256(encoded), nil
}

func (c *GatewayClient) timeNow() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *GatewayClient) invalidateToken(value string) {
	state := &c.token
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.token.value == value {
		state.token = cachedToken{}
	}
}

func (c *GatewayClient) serviceToken(ctx context.Context, rejected string) (string, error) {
	if rejected != "" {
		c.invalidateToken(rejected)
	}
	credential, fingerprint, err := readMachineCredential(c.credentialFile)
	if err != nil {
		return "", err
	}
	state := &c.token
	for {
		if ctx.Err() != nil {
			return "", retry("token exchange canceled")
		}
		state.mu.Lock()
		if rejected != "" && state.token.value == rejected {
			state.token = cachedToken{}
		}
		if state.token.credential == fingerprint && state.token.value != "" && c.timeNow().Before(state.token.refreshAfter) {
			value := state.token.value
			state.mu.Unlock()
			return value, nil
		}
		if flight := state.flight; flight != nil {
			state.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", retry("token exchange canceled")
			case <-flight.done:
				if flight.credential == fingerprint && flight.err != nil {
					return "", flight.err
				}
				continue
			}
		}
		flight := &tokenFlight{done: make(chan struct{}), credential: fingerprint}
		state.flight = flight
		state.mu.Unlock()

		token, err := c.exchangeToken(ctx, credential)
		token.credential = fingerprint
		state.mu.Lock()
		if err == nil {
			state.token = token
		}
		flight.err = err
		state.flight = nil
		close(flight.done)
		state.mu.Unlock()
		return token.value, err
	}
}

func (c *GatewayClient) exchangeToken(ctx context.Context, credential machineCredential) (cachedToken, error) {
	started := c.timeNow()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/auth/machine-token", strings.NewReader("{}"))
	if err != nil {
		return cachedToken{}, blocked("invalid token exchange request")
	}
	request.SetBasicAuth(credential.ClientID, credential.ClientSecret)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return cachedToken{}, retry("machine token exchange failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return cachedToken{}, &Failure{
			Message:    fmt.Sprintf("machine token exchange returned HTTP %d", response.StatusCode),
			Status:     response.StatusCode,
			Blocked:    response.StatusCode != 429 && response.StatusCode < 500,
			RetryAfter: retryAfter(response.Header.Get("Retry-After"), c.timeNow()),
		}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	var result struct {
		AccessToken string `json:"accessToken"`
		TokenType   string `json:"tokenType"`
		ExpiresIn   int64  `json:"expiresIn"`
	}
	if err != nil || len(data) > 64<<10 || json.Unmarshal(data, &result) != nil ||
		result.AccessToken == "" || strings.IndexFunc(result.AccessToken, func(r rune) bool { return r <= 0x20 || r >= 0x7f }) >= 0 ||
		result.TokenType != "Bearer" || result.ExpiresIn < 300 || result.ExpiresIn > 86400 {
		return cachedToken{}, retry("invalid machine token response")
	}
	ttl := time.Duration(result.ExpiresIn) * time.Second
	refreshAfter := started.Add(ttl - min(5*time.Minute, ttl/5))
	if !c.timeNow().Before(refreshAfter) {
		return cachedToken{}, retry("machine token response is already due for refresh")
	}
	return cachedToken{value: result.AccessToken, refreshAfter: refreshAfter}, nil
}
