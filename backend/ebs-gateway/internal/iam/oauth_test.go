package iam

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"ebs-gateway/internal/upstream"
)

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
