package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunnerEvictionAuthorization(t *testing.T) {
	const path = apiPrefix + "/runners/runner-a/status"
	for _, phase := range []string{"Evicted", "Offline"} {
		body := `{"metadata":{"resourceVersion":"7"},"status":{"phase":"` + phase + `"}}`
		for _, tc := range []struct {
			name           string
			claims         *jwtClaims
			upstreamStatus int
			want           int
		}{
			{"admin", ptrClaims(adminClaims()), 200, 200},
			{"admin conflict", ptrClaims(adminClaims()), 409, 409},
			{"admin not found", ptrClaims(adminClaims()), 404, 404},
			{"system", ptrClaims(systemClaims()), 200, 200},
			{"ops", ptrClaims(opsClaims()), 200, 200},
			{"ops conflict", ptrClaims(opsClaims()), 409, 409},
			{"user", ptrClaims(userClaims("alice")), 200, 403},
			{"anonymous", nil, 200, 401},
		} {
			t.Run(phase+"/"+tc.name, func(t *testing.T) {
				calls := 0
				gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					got, err := io.ReadAll(r.Body)
					if err != nil || r.Method != http.MethodPatch || r.URL.Path != path || string(got) != body {
						t.Fatalf("unexpected upstream request: %s %s %s err=%v", r.Method, r.URL.Path, got, err)
					}
					if r.Header.Get("Content-Type") != "application/merge-patch+json" {
						t.Fatal("patch content type not preserved")
					}
					w.WriteHeader(tc.upstreamStatus)
					_, _ = io.WriteString(w, `{"message":"upstream response"}`)
				}), 100, 200)
				req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
				if tc.claims != nil {
					req = authenticatedRequest(t, http.MethodPatch, path, strings.NewReader(body), *tc.claims)
				}
				req.Header.Set("Content-Type", "application/merge-patch+json")
				rec := httptest.NewRecorder()
				gw.ServeHTTP(rec, req)
				if rec.Code != tc.want {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
				if tc.want == 401 || tc.want == 403 {
					if calls != 0 {
						t.Fatal("unauthorized request reached apiserver")
					}
				} else if calls != 1 || rec.Body.String() != `{"message":"upstream response"}` {
					t.Fatalf("upstream response not preserved: calls=%d body=%s", calls, rec.Body.String())
				}
			})
		}
	}
}
