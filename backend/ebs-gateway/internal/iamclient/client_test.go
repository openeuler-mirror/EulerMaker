package iamclient

import (
	"context"
	"encoding/json"
	"errors"
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
		if request.Header.Get("Authorization") != "" {
			t.Error("caller credentials reached internal IAM")
		}
		paths = append(paths, request.Method+" "+request.URL.Path)
		var body string
		switch request.URL.Path {
		case "/apis/iam.ebs/v1/users/alice":
			body = `{"metadata":{"name":"alice"},"spec":{"enabled":true,"scopes":["ebs:user"]}}`
		case "/internal/iam/v1/users/authenticate":
			var input struct {
				Username string `json:"username"`
				Password string `json:"password"`
			}
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.Username != "alice" || input.Password != "password" {
				t.Fatalf("unexpected user authentication request: %+v, %v", input, err)
			}
			body = `{"authenticated":true,"username":"alice"}`
		case "/internal/iam/v1/machineaccounts/authenticate":
			var input struct {
				Name         string `json:"name"`
				ClientSecret string `json:"clientSecret"`
			}
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.Name != "worker" || input.ClientSecret != "secret" {
				t.Fatalf("unexpected machine authentication request: %+v, %v", input, err)
			}
			body = `{"authenticated":true,"name":"worker","type":"runner","tokenTTLSeconds":3600}`
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
	if err != nil || status != http.StatusOK || user.Name != "alice" || !user.Enabled || len(user.Scopes) != 1 || user.Scopes[0] != identity.UserScope {
		t.Fatalf("GetUser() = %+v, %d, %v", user, status, err)
	}
	valid, status, err := client.AuthenticateUser(context.Background(), "alice", "password")
	if err != nil || status != http.StatusOK || !valid {
		t.Fatalf("AuthenticateUser() = %v, %d, %v", valid, status, err)
	}
	spec, valid, status, err := client.AuthenticateMachine(context.Background(), "worker", "secret")
	if err != nil || status != http.StatusOK || !valid || spec.TokenTTLSeconds != 3600 || spec.Type != "runner" {
		t.Fatalf("AuthenticateMachine() = %+v, %v, %d, %v", spec, valid, status, err)
	}
	if got := strings.Join(paths, ", "); got != "GET /apis/iam.ebs/v1/users/alice, POST /internal/iam/v1/users/authenticate, POST /internal/iam/v1/machineaccounts/authenticate" {
		t.Fatalf("unexpected IAM request sequence %q", got)
	}
}

func TestGetUserEnabled(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spec    string
		enabled bool
	}{
		{"enabled", `{"enabled":true,"scopes":["ebs:user"]}`, true},
		{"disabled", `{"enabled":false,"scopes":["ebs:user"]}`, false},
		{"omitted", `{"scopes":["ebs:user"]}`, false},
		{"null", `{"enabled":null,"scopes":["ebs:user"]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := upstream.New("https://api.example", transportFunc(func(*http.Request) (*http.Response, error) {
				body := `{"metadata":{"name":"alice"},"spec":` + tc.spec + `}`
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			user, status, err := New(client).GetUser(context.Background(), "alice")
			if err != nil || status != http.StatusOK || user.Enabled != tc.enabled {
				t.Fatalf("GetUser()=%+v, %d, %v", user, status, err)
			}
		})
	}
}

func TestOAuthIAMContract(t *testing.T) {
	for _, registration := range []bool{false, true} {
		client, err := upstream.New("https://api.example", transportFunc(func(req *http.Request) (*http.Response, error) {
			var payload map[string]any
			json.NewDecoder(req.Body).Decode(&payload)
			external, ok := payload["externalIdentity"].(map[string]any)
			if !ok || external["issuer"] != "issuer" || external["subject"] != "subject" || payload["method"] != "oauth" {
				t.Fatalf("payload=%v", payload)
			}
			status, body := 200, `{"authenticated":true,"username":"alice","uid":"uid"}`
			if registration {
				if req.URL.Path != "/internal/iam/v1/users/register" || payload["username"] != "alice" {
					t.Fatalf("register=%v", req.URL)
				}
				status, body = 201, `{"username":"alice","uid":"uid"}`
			} else if req.URL.Path != "/internal/iam/v1/users/authenticate" {
				t.Fatal(req.URL.Path)
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		var result OAuthAccount
		if registration {
			result, err = New(client).RegisterOAuth(context.Background(), ExternalIdentity{"issuer", "subject"}, "alice", "Alice", "")
		} else {
			result, err = New(client).AuthenticateOAuth(context.Background(), ExternalIdentity{"issuer", "subject"})
		}
		if err != nil || result.UID != "uid" || result.Username != "alice" {
			t.Fatalf("%+v %v", result, err)
		}
	}
}

func TestOAuthIAMResponseBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body, code string
	}{
		{"new identity", 200, `{"authenticated":false,"registrationRequired":true}`, ""},
		{"pending", 409, `{"code":"RegistrationInProgress","error":"internal details"}`, "RegistrationInProgress"},
		{"unknown", 500, `{"code":"unexpected","error":"secret"}`, "OAuthUnavailable"},
		{"middleware", 403, `{"kind":"Status","reason":"Forbidden"}`, "OAuthUnavailable"},
		{"wrong status", 200, `{"code":"IAMUnavailable"}`, "OAuthUnavailable"},
		{"conflicting flags", 200, `{"authenticated":true,"registrationRequired":true,"username":"alice","uid":"uid"}`, "OAuthUnavailable"},
		{"missing UID", 200, `{"authenticated":true,"username":"alice"}`, "OAuthUnavailable"},
		{"invalid body", 200, `<html>error</html>`, "OAuthUnavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, _ := upstream.New("https://api.example", transportFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}))
			result, err := New(up).AuthenticateOAuth(context.Background(), ExternalIdentity{"issuer", "subject"})
			if tc.code == "" {
				if err != nil || !result.RegistrationRequired {
					t.Fatalf("%+v %v", result, err)
				}
				return
			}
			var failure *OAuthError
			if !errors.As(err, &failure) || failure.Code != tc.code || strings.Contains(err.Error(), "secret") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}
