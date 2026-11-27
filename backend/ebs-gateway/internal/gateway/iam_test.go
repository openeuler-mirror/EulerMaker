package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/upstream"
)

func TestUserUpdateChain(t *testing.T) {
	const path = "/apis/iam.ebs/v1/users/alice"
	const old = `{"apiVersion":"iam.ebs/v1","kind":"User","metadata":{"name":"alice","resourceVersion":"7"},"spec":{"enabled":true,"scopes":["ebs:user"],"displayName":"original"}}`
	for _, method := range []string{http.MethodPut, http.MethodPatch} {
		for _, tc := range []struct {
			name, scopes string
			status       int
		}{
			{"ordinary update", "ebs:user", http.StatusOK},
			{"admin promotion", "ebs:admin", http.StatusForbidden},
		} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				reads, writes := 0, 0
				api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
					body := old
					switch {
					case r.Method == http.MethodGet && r.URL.Path == "/apis/iam.ebs/v1/users/operator":
						body = `{"metadata":{"name":"operator"},"spec":{"enabled":true,"scopes":["ebs:admin"]}}`
					case r.Method == http.MethodGet && r.URL.Path == path:
						reads++
					case r.Method == http.MethodPut && r.URL.Path == path:
						writes++
						var object struct {
							Metadata struct{ ResourceVersion string }
							Spec     struct{ DisplayName string }
						}
						if err := json.NewDecoder(r.Body).Decode(&object); err != nil {
							t.Fatal(err)
						}
						if object.Metadata.ResourceVersion != "7" || object.Spec.DisplayName != "updated" {
							t.Fatalf("invalid update: %+v", object)
						}
					default:
						t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
				}))
				token, err := api.tokens.Issue("operator", identity.UserType, identity.AdminScope, time.Hour, api.now())
				if err != nil {
					t.Fatal(err)
				}
				body := strings.Replace(strings.Replace(old, "original", "updated", 1), "ebs:user", tc.scopes, 1)
				contentType := "application/json"
				if method == http.MethodPatch {
					body = `{"spec":{"displayName":"updated","scopes":["` + tc.scopes + `"]}}`
					contentType = "application/merge-patch+json"
				}
				request := httptest.NewRequest(method, path, strings.NewReader(body))
				request.Header.Set("Authorization", "Bearer "+token)
				request.Header.Set("Content-Type", contentType)
				response := httptest.NewRecorder()
				api.Router().ServeHTTP(response, request)
				wantWrites := 0
				if tc.status == http.StatusOK {
					wantWrites = 1
				}
				if response.Code != tc.status || reads != 1 || writes != wantWrites {
					t.Fatalf("status=%d reads=%d writes=%d body=%s", response.Code, reads, writes, response.Body)
				}
			})
		}
	}
}

func TestUserDeleteProtection(t *testing.T) {
	const ordinary = `{"spec":{"scopes":["ebs:user"]}}`
	for _, tc := range []struct {
		name, object string
		status       int
	}{
		{"ordinary", ordinary, http.StatusOK},
		{"admin", `{"spec":{"scopes":["ebs:admin"]}}`, http.StatusForbidden},
		{"oversized", ordinary + strings.Repeat(" ", 64), http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			client, err := upstream.New("http://iam", roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls = append(calls, r.Method)
				if r.Method == http.MethodGet && r.URL.Query().Get("includeFields") != "spec.scopes" {
					t.Fatalf("unexpected delete protection lookup: %s", r.URL)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.object))}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			api := &Handler{upstream: client, bodyLimit: 64}
			router := gin.New()
			router.DELETE("/users/:name", api.protectAdminUser, api.upstream.Forward)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/users/example", nil))
			wantCalls := "GET"
			if tc.status == http.StatusOK {
				wantCalls = "GET,DELETE"
			}
			if response.Code != tc.status || strings.Join(calls, ",") != wantCalls {
				t.Fatalf("status=%d upstream=%v; want status=%d upstream=%s", response.Code, calls, tc.status, wantCalls)
			}
		})
	}
}
