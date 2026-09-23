package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJobAbortAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name     string
		claims   *jwtClaims
		method   string
		want     int
		relation string
	}{
		{"owner", ptrClaims(userClaims("bob")), "POST", 200, ""},
		{"member", ptrClaims(userClaims("alice")), "POST", 200, ""},
		{"other", ptrClaims(userClaims("carol")), "POST", 403, ""},
		{"ops unrelated", ptrClaims(opsClaims()), "POST", 403, ""},
		{"admin unrelated", ptrClaims(adminClaims()), "POST", 403, ""},
		{"ops owner", ptrClaims(opsClaims()), "POST", 200, "owner"},
		{"ops member", ptrClaims(opsClaims()), "POST", 200, "member"},
		{"admin owner", ptrClaims(adminClaims()), "POST", 200, "owner"},
		{"admin member", ptrClaims(adminClaims()), "POST", 200, "member"},
		{"system", ptrClaims(systemClaims()), "POST", 403, ""},
		{"runner", ptrClaims(runnerClaims("r")), "POST", 403, ""},
		{"anonymous", nil, "POST", 401, ""},
		{"method", ptrClaims(adminClaims()), "GET", 405, "owner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/apis/iam.ebs/v1/users/carol" {
					io.WriteString(w, `{"metadata":{"name":"carol"},"spec":{"enabled":true,"scopes":["ebs:user"]}}`)
					return
				}
				if r.URL.Path == apiPrefix+"/projects/p" {
					labels := map[string]string{"ebs.io/owner-user": "bob", "ebs.io/member-user.alice": "true"}
					if tc.relation == "owner" {
						labels["ebs.io/owner-user"] = tc.claims.Subject
					}
					if tc.relation == "member" {
						labels["ebs.io/member-user."+tc.claims.Subject] = "true"
					}
					json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "p", "labels": labels}})
					return
				}
				calls++
				if tc.claims == nil || r.Header.Get("X-EBS-User") != tc.claims.Subject || r.Header.Get("X-EBS-Scopes") != strings.Join(tc.claims.Scopes, ",") || r.Header.Get("X-EBS-Admin") != "" {
					t.Fatal("abort handler did not receive sanitized identity headers")
				}
				body, _ := io.ReadAll(r.Body)
				if string(body) != `{"uid":"u"}` {
					t.Fatalf("body changed %s", body)
				}
				io.WriteString(w, `{"status":{"phase":"Aborted"}}`)
			}), 100, 200)
			path := apiPrefix + "/projects/p/jobs/j/abort"
			r := httptest.NewRequest(tc.method, path, strings.NewReader(`{"uid":"u"}`))
			if tc.claims != nil {
				r = authenticatedRequest(t, tc.method, path, strings.NewReader(`{"uid":"u"}`), *tc.claims)
			}
			w := httptest.NewRecorder()
			r.Header.Set("X-EBS-User", "spoofed")
			r.Header.Set("X-EBS-Admin", "true")
			gw.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("got %d %s want %d", w.Code, w.Body, tc.want)
			}
			if tc.want != 200 && calls != 0 {
				t.Fatal("denied operation reached upstream")
			}
		})
	}
}

func TestJobAbortPreservesUpstreamErrors(t *testing.T) {
	for _, code := range []int{404, 409, 422, 500} {
		gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == apiPrefix+"/projects/p" {
				io.WriteString(w, `{"metadata":{"name":"p","labels":{"ebs.io/owner-user":"admin"}}}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			io.WriteString(w, `{"kind":"Status","message":"upstream"}`)
		}), 100, 200)
		r := authenticatedRequest(t, "POST", apiPrefix+"/projects/p/jobs/j/abort", strings.NewReader(`{"uid":"u"}`), adminClaims())
		w := httptest.NewRecorder()
		gw.ServeHTTP(w, r)
		if w.Code != code || w.Body.String() != `{"kind":"Status","message":"upstream"}` {
			t.Fatalf("response changed: %d %s", w.Code, w.Body)
		}
	}
}

func TestJobStatusCannotBypassAbortAuthorization(t *testing.T) {
	for _, claims := range []jwtClaims{userClaims("bob"), userClaims("alice"), opsClaims(), adminClaims()} {
		gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("status write reached upstream") }), 100, 200)
		for _, method := range []string{"PUT", "PATCH"} {
			r := authenticatedRequest(t, method, apiPrefix+"/projects/p/jobs/j/status", strings.NewReader(`{"status":{"phase":"Aborted"}}`), claims)
			w := httptest.NewRecorder()
			gw.ServeHTTP(w, r)
			if w.Code != 403 {
				t.Fatalf("got %d", w.Code)
			}
		}
	}
}

func TestJobAbortProjectLookupFailure(t *testing.T) {
	for _, claims := range []jwtClaims{userClaims("bob"), opsClaims(), adminClaims()} {
		for _, code := range []int{404, 500} {
			gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != apiPrefix+"/projects/p" {
					t.Fatal("abort reached upstream after failed project lookup")
				}
				w.WriteHeader(code)
			}), 100, 200)
			r := authenticatedRequest(t, "POST", apiPrefix+"/projects/p/jobs/j/abort", strings.NewReader(`{"uid":"u"}`), claims)
			w := httptest.NewRecorder()
			gw.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("scope %v, lookup %d: got %d", claims.Scopes, code, w.Code)
			}
		}
	}
}
