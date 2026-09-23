package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestScriptAuthorization(t *testing.T) {
	admin := userClaims("admin")
	admin.Scopes = []string{"ebs:admin"}
	for _, identity := range []struct {
		name   string
		claims *jwtClaims
	}{
		{"anonymous", nil}, {"user", ptrClaims(userClaims("alice"))},
		{"ops", ptrClaims(opsClaims())}, {"admin", ptrClaims(admin)},
		{"system", ptrClaims(systemClaims())}, {"runner", ptrClaims(runnerClaims("runner-a"))},
	} {
		for _, route := range []struct {
			method, path, permission string
		}{
			{"GET", "/scripts", "list"}, {"HEAD", "/scripts", "list"},
			{"GET", "/scripts/rpmbuild", "get"}, {"HEAD", "/scripts/rpmbuild", "get"},
			{"GET", "/scripts?limit=10&labelSelector=type%3Drpm", "list"},
			{"GET", "/scripts/rpmbuild?watch=false", "get"},
			{"POST", "/scripts", "write"}, {"PUT", "/scripts/rpmbuild", "write"}, {"PATCH", "/scripts/rpmbuild", "write"},
			{"DELETE", "/scripts/rpmbuild", "deny"}, {"DELETE", "/scripts", "deny"},
			{"POST", "/scripts/rpmbuild", "deny"}, {"PUT", "/scripts", "deny"},
			{"GET", "/scripts/rpmbuild/status", "deny"}, {"PUT", "/scripts/rpmbuild/status", "deny"},
			{"GET", "/scripts?watch=true", "deny"}, {"GET", "/scripts?watch=false&watch=true", "deny"},
			{"GET", "/scripts/", "deny"},
			{"GET", "/projects/project-a/scripts", "deny"}, {"PUT", "/projects/project-a/scripts/rpmbuild", "deny"},
		} {
			t.Run(identity.name+"/"+route.method+route.path, func(t *testing.T) {
				forwarded := false
				gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					forwarded = true
					w.WriteHeader(http.StatusOK)
				}), 100, 200)
				req := httptest.NewRequest(route.method, apiPrefix+route.path, strings.NewReader("{}"))
				if identity.claims != nil {
					req = authenticatedRequest(t, route.method, apiPrefix+route.path, strings.NewReader("{}"), *identity.claims)
				}
				rec := httptest.NewRecorder()
				gw.ServeHTTP(rec, req)
				privileged := identity.name == "ops" || identity.name == "admin" || identity.name == "system"
				allowed := identity.name != "anonymous" && (route.permission == "get" || route.permission == "list" && identity.name != "runner" || route.permission == "write" && privileged)
				want := http.StatusForbidden
				if identity.name == "anonymous" {
					want = http.StatusUnauthorized
				}
				if allowed {
					want = http.StatusOK
				}
				if rec.Code != want || forwarded != allowed {
					t.Fatalf("status=%d want=%d forwarded=%v body=%s", rec.Code, want, forwarded, rec.Body.String())
				}
			})
		}
	}
}
