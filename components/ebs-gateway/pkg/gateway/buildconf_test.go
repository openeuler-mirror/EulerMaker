package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildConfAuthorization(t *testing.T) {
	admin := userClaims("admin")
	admin.Scopes = []string{"ebs:admin"}
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }), 100, 200)
	for _, identity := range []struct {
		name   string
		claims *jwtClaims
	}{
		{"anonymous", nil}, {"user", ptrClaims(userClaims("alice"))}, {"ops", ptrClaims(opsClaims())}, {"admin", ptrClaims(admin)}, {"system", ptrClaims(systemClaims())},
	} {
		for _, route := range []struct {
			method, path string
			read, write  bool
		}{
			{"GET", "/buildconfs", true, false}, {"HEAD", "/buildconfs/default", true, false},
			{"POST", "/buildconfs", false, true}, {"PUT", "/buildconfs/default", false, true}, {"PATCH", "/buildconfs/default", false, true},
			{"DELETE", "/buildconfs/default", false, false}, {"GET", "/buildconfs/default/status", false, false}, {"GET", "/buildconfs?watch=true", false, false},
			{"PUT", "/buildconfs/other", false, false}, {"PUT", "/projects/project-a/buildconfs/default", false, false},
		} {
			t.Run(identity.name+"/"+route.method+route.path, func(t *testing.T) {
				req := httptest.NewRequest(route.method, apiPrefix+route.path, strings.NewReader("{}"))
				if identity.claims != nil {
					req = authenticatedRequest(t, route.method, apiPrefix+route.path, strings.NewReader("{}"), *identity.claims)
				}
				rec := httptest.NewRecorder()
				gw.ServeHTTP(rec, req)
				allowed := route.read || route.write && identity.name != "anonymous" && identity.name != "user"
				if (rec.Code == 200) != allowed {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
			})
		}
	}
}

func ptrClaims(c jwtClaims) *jwtClaims { return &c }
