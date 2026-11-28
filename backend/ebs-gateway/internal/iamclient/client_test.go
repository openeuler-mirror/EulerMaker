package iamclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	iamv1 "ebs-api/iam/v1"

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
			if request.URL.Query().Get("includeFields") != "metadata.name,metadata.uid,spec.enabled,spec.scopes" {
				t.Fatalf("unexpected user projection: %s", request.URL)
			}
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
	if err != nil || status != http.StatusOK || user.Name != "alice" || user.Spec.Enabled == nil || !*user.Spec.Enabled || len(user.Spec.Scopes) != 1 || identity.Scope(user.Spec.Scopes[0]) != identity.UserScope {
		t.Fatalf("GetUser() = %+v, %d, %v", user, status, err)
	}
	valid, status, err := client.AuthenticateUser(context.Background(), "alice", "password")
	if err != nil || status != http.StatusOK || !valid {
		t.Fatalf("AuthenticateUser() = %v, %d, %v", valid, status, err)
	}
	result, valid, status, err := client.AuthenticateMachine(context.Background(), "worker", "secret")
	if err != nil || status != http.StatusOK || !valid || result.Name != "worker" || result.TokenTTLSeconds != 3600 || result.Type != "runner" {
		t.Fatalf("AuthenticateMachine() = %+v, %v, %d, %v", result, valid, status, err)
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
			if err != nil || status != http.StatusOK || (user.Spec.Enabled != nil && *user.Spec.Enabled) != tc.enabled {
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
		var result OAuthResult
		if registration {
			result, err = New(client).RegisterUserOAuth(context.Background(), ExternalIdentity{"issuer", "subject"}, "alice", "Alice", "")
		} else {
			result, err = New(client).AuthenticateUserOAuth(context.Background(), ExternalIdentity{"issuer", "subject"})
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
			result, err := New(up).AuthenticateUserOAuth(context.Background(), ExternalIdentity{"issuer", "subject"})
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

func TestAuthenticateMachineResult(t *testing.T) {
	for _, typ := range []iamv1.MachineAccountType{iamv1.MachineAccountRunner, iamv1.MachineAccountService} {
		t.Run(string(typ), func(t *testing.T) {
			client, err := upstream.New("https://api.example", transportFunc(func(request *http.Request) (*http.Response, error) {
				if request.Method != http.MethodPost || request.URL.Path != "/internal/iam/v1/machineaccounts/authenticate" {
					t.Fatalf("unexpected authentication request: %s %s", request.Method, request.URL)
				}
				body := `{"authenticated":true,"name":"worker","type":"` + string(typ) + `","tokenTTLSeconds":7200,"projects":["demo"],"permissions":["builds:create"]}`
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			result, authenticated, status, err := New(client).AuthenticateMachine(context.Background(), "worker", "secret")
			want := MachineAuthResult{Name: "worker", Type: typ, TokenTTLSeconds: 7200}
			if err != nil || status != http.StatusOK || !authenticated || result != want {
				t.Fatalf("AuthenticateMachine() = %+v, %v, %d, %v; want %+v", result, authenticated, status, err, want)
			}
		})
	}
}

func TestGetMachineProjectsAuthorizationFields(t *testing.T) {
	client, err := upstream.New("https://api.example", transportFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Path != "/apis/iam.ebs/v1/machineaccounts/service" ||
			request.URL.Query().Get("includeFields") != "metadata.name,spec.type,spec.projects,spec.permissions" {
			t.Fatalf("unexpected machine lookup: %s %s", request.Method, request.URL)
		}
		body := `{"metadata":{"name":"service"},"spec":{"type":"service","projects":["*"],"permissions":["builds:create"]}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	account, status, err := New(client).GetMachine(context.Background(), "service")
	if err != nil || status != http.StatusOK {
		t.Fatalf("GetMachine status=%d err=%v", status, err)
	}
	if account.Name != "service" || account.Spec.Type != "service" ||
		len(account.Spec.Projects) != 1 || account.Spec.Projects[0] != iamv1.MachineAccountAllProjects ||
		len(account.Spec.Permissions) != 1 || account.Spec.Permissions[0] != "builds:create" {
		t.Fatalf("incomplete machine authorization: %+v", account)
	}
}
