package artifact

import (
	"bytes"
	"context"
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
			authorizer := &GatewayAuthorizer{url: "https://gateway/auth/check", ttl: time.Second, cache: make(map[[32]byte]cachedIdentity)}
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
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(payload))}, nil
			})}
			_, err := authorizer.Authenticate(context.Background(), "token")
			if (err != nil) != test.wantError {
				t.Fatalf("Authenticate error = %v, wantError %v", err, test.wantError)
			}
		})
	}
}
