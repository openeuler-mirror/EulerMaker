package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ebs-gateway/internal/identity"
)

func TestProjectUpdateReusesAuthorizedObject(t *testing.T) {
	const path = "/apis/ebs/v1/projects/team"
	const old = `{"apiVersion":"ebs/v1","kind":"Project","metadata":{"name":"team","resourceVersion":"7","labels":{"ebs.io/owner-user":"alice"}},"spec":{"description":"original"}}`
	for _, method := range []string{http.MethodPut, http.MethodPatch} {
		for _, tc := range []struct {
			name, subject string
			typ           identity.Type
			scope         identity.Scope
			status        int
		}{
			{"owner", "alice", identity.UserType, identity.UserScope, http.StatusOK},
			{"ops owner", "alice", identity.UserType, identity.OpsScope, http.StatusOK},
			{"nonowner", "bob", identity.UserType, identity.UserScope, http.StatusForbidden},
			{"service", "sync", identity.ServiceType, "", http.StatusOK},
		} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				reads, writes := 0, 0
				api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
					body := old
					switch {
					case r.Method == http.MethodGet && r.URL.Path == "/apis/iam.ebs/v1/users/"+tc.subject:
						body = `{"metadata":{"name":"` + tc.subject + `"},"spec":{"enabled":true,"scopes":["` + string(tc.scope) + `"]}}`
					case r.Method == http.MethodGet && r.URL.Path == "/apis/iam.ebs/v1/machineaccounts/sync":
						body = `{"metadata":{"name":"sync"},"spec":{"type":"service","projects":["team"],"permissions":["projects:update"]}}`
					case r.Method == http.MethodGet && r.URL.Path == path:
						reads++
						if r.URL.RawQuery != "" {
							t.Fatalf("update lookup must read the full object: %s", r.URL)
						}
					case r.Method == http.MethodPut && r.URL.Path == path:
						writes++
						var object struct {
							Metadata struct{ ResourceVersion string }
							Spec     struct{ Description string }
						}
						if err := json.NewDecoder(r.Body).Decode(&object); err != nil {
							t.Fatal(err)
						}
						if object.Metadata.ResourceVersion != "7" || object.Spec.Description != "updated" {
							t.Fatalf("invalid update: %+v", object)
						}
					default:
						t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
				}))
				token, err := api.tokens.Issue(tc.subject, tc.typ, tc.scope, time.Hour, api.now())
				if err != nil {
					t.Fatal(err)
				}
				body, contentType := strings.Replace(old, "original", "updated", 1), "application/json"
				if method == http.MethodPatch {
					body, contentType = `{"spec":{"description":"updated"}}`, "application/merge-patch+json"
				}
				request := httptest.NewRequest(method, path, strings.NewReader(body))
				request.Header.Set("Authorization", "Bearer "+token)
				request.Header.Set("Content-Type", contentType)
				response := httptest.NewRecorder()
				api.Router().ServeHTTP(response, request)
				if response.Code != tc.status || reads != 1 || (writes == 1) != (tc.status == http.StatusOK) {
					t.Fatalf("status=%d reads=%d writes=%d body=%s", response.Code, reads, writes, response.Body)
				}
			})
		}
	}
}
