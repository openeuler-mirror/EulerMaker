package iam

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/upstream"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestClientUsesInternalIAMContract(t *testing.T) {
	paths := make([]string, 0, 3)
	upstreamClient, err := upstream.New("https://api.example", transportFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "" || request.Header.Get("X-EBS-User") != "" {
			t.Error("caller credentials reached internal IAM")
		}
		paths = append(paths, request.Method+" "+request.URL.Path)
		var body string
		switch request.URL.Path {
		case "/apis/iam.ebs/v1/users/alice":
			body = `{"metadata":{"name":"alice"},"spec":{"enabled":true,"scopes":["ebs:user"]}}`
		case "/internal/iam/v1/authenticate":
			body = `{"authenticated":true,"username":"alice"}`
		case "/internal/iam/v1/machineaccounts/worker/authenticate":
			body = `{"authenticated":true,"name":"worker","tokenTTLSeconds":3600}`
		default:
			t.Errorf("unexpected path %s", request.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	client := New(upstreamClient)
	user, status, err := client.GetUser(context.Background(), "alice")
	if err != nil || status != http.StatusOK || user.Name != "alice" || len(user.Scopes) != 1 || user.Scopes[0] != identity.UserScope {
		t.Fatalf("GetUser() = %+v, %d, %v", user, status, err)
	}
	valid, status, err := client.AuthenticateUser(context.Background(), "alice", "password")
	if err != nil || status != http.StatusOK || !valid {
		t.Fatalf("AuthenticateUser() = %v, %d, %v", valid, status, err)
	}
	ttl, valid, status, err := client.AuthenticateMachine(context.Background(), "worker", "secret")
	if err != nil || status != http.StatusOK || !valid || ttl != 3600 {
		t.Fatalf("AuthenticateMachine() = %d, %v, %d, %v", ttl, valid, status, err)
	}
	if got := strings.Join(paths, ", "); got != "GET /apis/iam.ebs/v1/users/alice, POST /internal/iam/v1/authenticate, POST /internal/iam/v1/machineaccounts/worker/authenticate" {
		t.Fatalf("unexpected IAM request sequence %q", got)
	}
}
