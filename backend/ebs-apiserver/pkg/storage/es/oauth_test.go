package es

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestIdentityDocumentNotFound(t *testing.T) {
	for _, test := range []struct {
		body   string
		absent bool
	}{
		{`{"_index":"ebs-oauth-identity-claims-v1","_id":"id","found":false}`, true},
		{`{"error":{"type":"index_not_found_exception"},"status":404}`, false},
		{`{"found":false,"error":{"type":"index_not_found_exception"}}`, false},
		{`{"found":true}`, false},
		{`404 not found`, false},
	} {
		err := &HTTPError{StatusCode: 404, Body: test.body}
		if got := IsDocumentNotFound(err); got != test.absent {
			t.Errorf("IsDocumentNotFound(%s)=%t, want %t", test.body, got, test.absent)
		}
	}
}

func TestValidateOAuthMappings(t *testing.T) {
	for _, test := range []struct {
		name, userMapping, claimMapping string
		wantError                       bool
	}{
		{"current", iamIndexMapping, oauthIdentityMapping, false},
		{"missing identity", strings.Replace(iamIndexMapping, `"externalIdentity"`, `"unrelated"`, 1), oauthIdentityMapping, true},
		{"indexed identity", strings.Replace(iamIndexMapping, `"externalIdentity":{"type":"object","enabled":false}`, `"externalIdentity":{"type":"keyword"}`, 1), oauthIdentityMapping, true},
		{"invalid claim", iamIndexMapping, strings.ReplaceAll(oauthIdentityMapping, `"keyword"`, `"text"`), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := NewClientForTesting("http://elasticsearch", &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodGet || !strings.HasSuffix(req.URL.Path, "/_mapping") {
					t.Fatalf("mapping validation attempted mutation: %s %s", req.Method, req.URL)
				}
				mapping := test.claimMapping
				if req.URL.Path == "/ebs-users/_mapping" {
					mapping = test.userMapping
				}
				return response(200, `{"index":`+mapping+`}`), nil
			})})
			if err := client.validateOAuthMappings(); (err != nil) != test.wantError {
				t.Fatalf("validateOAuthMappings = %v", err)
			}
		})
	}
}

func TestOAuthClaimCreateAndRelease(t *testing.T) {
	claim := OAuthIdentityClaim{Issuer: "issuer", Subject: "subject", UserName: "alice", UserUID: "uid", OperationID: "operation"}
	client := NewClientForTesting("http://elasticsearch", &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/ebs-oauth-identity-claims/_doc/key" {
			t.Fatalf("unexpected claim path: %s", req.URL)
		}
		switch req.Method {
		case http.MethodPut:
			var got OAuthIdentityClaim
			if err := json.NewDecoder(req.Body).Decode(&got); err != nil || got != claim || req.URL.Query().Get("op_type") != "create" {
				t.Fatalf("claim create = %+v, %v, %s", got, err, req.URL)
			}
		case http.MethodDelete:
			if req.URL.Query().Get("if_seq_no") != "2" || req.URL.Query().Get("if_primary_term") != "3" {
				t.Fatalf("release missing CAS: %s", req.URL)
			}
		default:
			t.Fatalf("unexpected method: %s", req.Method)
		}
		return response(200, `{"_seq_no":2,"_primary_term":3}`), nil
	})})
	version, err := client.CreateOAuthIdentity(context.Background(), "key", claim)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteOAuthIdentity(context.Background(), "key", version); err != nil {
		t.Fatal(err)
	}
}
