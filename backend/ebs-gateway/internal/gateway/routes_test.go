package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/middleware"
)

func TestAdmitIdentity(t *testing.T) {
	identities := []struct {
		name string
		who  identity.Principal
	}{
		{"anonymous", identity.Principal{}},
		{"user", identity.Principal{Type: identity.UserType, Subject: "user", Scope: identity.UserScope}},
		{"ops", identity.Principal{Type: identity.UserType, Subject: "ops", Scope: identity.OpsScope}},
		{"admin", identity.Principal{Type: identity.UserType, Subject: "admin", Scope: identity.AdminScope}},
		{"runner", identity.Principal{Type: identity.RunnerType, Subject: "runner"}},
		{"service", identity.Principal{Type: identity.ServiceType, Subject: "service"}},
		{"empty subject", identity.Principal{Type: identity.RunnerType}},
		{"invalid scope", identity.Principal{Type: identity.UserType, Subject: "user", Scope: "invalid"}},
		{"unknown type", identity.Principal{Type: "unknown", Subject: "user"}},
	}
	for _, tc := range []struct {
		method, path, allowed string
	}{
		{http.MethodGet, "/apis/ebs/v1/configs", "ops,admin"},
		{http.MethodPost, "/apis/ebs/v1/configs", "ops,admin"},
		{http.MethodGet, "/apis/ebs/v1/scripts", "user,ops,admin,runner"},
		{http.MethodPut, "/apis/ebs/v1/scripts/:name", "ops,admin"},
		{http.MethodGet, "/apis/iam.ebs/v1/users", "admin"},
		{http.MethodPost, "/auth/machineaccounts", "admin"},
		{http.MethodPost, "/auth/check", "user,ops,admin,runner,service"},
		{http.MethodPut, "/auth/users/:name/password", "user,ops,admin"},
		{http.MethodGet, "/apis/ebs/v1/build-targets", "user,ops,admin"},
		{http.MethodGet, "/apis/ebs/v1/service-endpoints", "runner"},
		{http.MethodGet, "/apis/ebs/v1/runners", "ops,admin"},
		{http.MethodPost, "/apis/ebs/v1/runners", "runner"},
		{http.MethodGet, "/apis/ebs/v1/runners/:runner", "ops,admin,runner"},
		{http.MethodGet, "/apis/ebs/v1/runners/:runner/jobs", "runner"},
		{http.MethodGet, "/apis/ebs/v1/runners/:runner/status", "ops,admin"},
		{http.MethodPatch, "/apis/ebs/v1/runners/:runner/status", "ops,admin,runner"},
		{http.MethodDelete, "/apis/ebs/v1/runners/:runner", "ops,admin"},
		{http.MethodGet, "/apis/ebs/v1/projects/:project", "anonymous,user,ops,admin"},
		{http.MethodPost, "/apis/ebs/v1/projects", "user,ops,admin"},
		{http.MethodPut, "/apis/ebs/v1/projects/:project", "user,ops,admin,service"},
		{http.MethodGet, "/apis/ebs/v1/scripts/:name", "user,ops,admin,runner"},
		{http.MethodGet, "/apis/ebs/v1/projects/:project/status", ""},
		{http.MethodPut, "/apis/ebs/v1/projects/:project/status", ""},
		{http.MethodPatch, "/apis/ebs/v1/projects/:project/status", ""},
		{http.MethodPost, "/apis/ebs/v1/projects/:project/builds", "user,ops,admin,service"},
		{http.MethodPost, "/apis/ebs/v1/projects/:project/builds/:name/abort", "user,ops,admin,service"},
		{http.MethodDelete, "/apis/ebs/v1/projects/:project/builds/:name", "user,ops,admin"},
		{http.MethodGet, "/apis/ebs/v1/projects/:project/jobs", "anonymous,user,ops,admin"},
		{http.MethodGet, "/apis/ebs/v1/projects/:project/jobs/:name", "anonymous,user,ops,admin,runner"},
		{http.MethodGet, "/apis/ebs/v1/projects/:project/jobs/:name/status", "anonymous,user,ops,admin"},
		{http.MethodPut, "/apis/ebs/v1/projects/:project/jobs/:name/status", ""},
		{http.MethodPatch, "/apis/ebs/v1/projects/:project/jobs/:name/status", "runner"},
		{http.MethodPost, "/apis/ebs/v1/projects/:project/jobs", ""},
		{http.MethodDelete, "/apis/ebs/v1/projects/:project/jobs/:name", ""},
		{http.MethodPut, "/apis/ebs/v1/projects/:project/jobs/:name", ""},
		{http.MethodPatch, "/apis/ebs/v1/projects/:project/jobs/:name", ""},
		{http.MethodGet, "/apis/ebs/v1/new-resource", ""},
		{http.MethodPost, "/apis/ebs/v1/build-targets", ""},
	} {
		for _, subject := range identities {
			t.Run(tc.method+tc.path+"/"+subject.name, func(t *testing.T) {
				router := gin.New()
				called := false
				decision := ""
				router.Use(func(c *gin.Context) {
					c.Set("principal", subject.who)
					c.Next()
					decision = c.GetString("serviceAuthorization")
				}, middleware.AdmitIdentity(middleware.NewRoutePolicies(routeRules())))
				router.Handle(tc.method, tc.path, func(c *gin.Context) {
					called = true
					c.Status(http.StatusNoContent)
				})
				path := strings.NewReplacer(":project", "team", ":runner", "runner", ":name", "name").Replace(tc.path)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, httptest.NewRequest(tc.method, path, nil))
				allowed := strings.Contains(","+tc.allowed+",", ","+subject.name+",")
				want := http.StatusForbidden
				if allowed {
					want = http.StatusNoContent
				}
				if response.Code != want || called != allowed {
					t.Fatalf("status=%d called=%v, want status=%d called=%v", response.Code, called, want, allowed)
				}
				if subject.who.Type == identity.ServiceType && !allowed && decision != "denied" {
					t.Fatal("missing service denial audit")
				}
			})
		}
	}
}

func TestRoutePolicyConsistency(t *testing.T) {
	for route, rule := range routeRules() {
		if rule.Identities == 0 {
			t.Errorf("route has no allowed identities: %+v", route)
		}
		if rule.Watch && (route.Method != http.MethodGet || rule.Authentication == middleware.AuthenticationNone) {
			t.Errorf("Watch must use GET and cannot bypass authentication: %+v", route)
		}
		if (rule.Authentication != middleware.AuthenticationRequired) != (rule.Identities&middleware.AllowAnonymous != 0) {
			t.Errorf("anonymous admission and authentication disagree: %+v", route)
		}
	}
}

func hasRoutePolicy(method, path string) bool {
	_, ok := routeRules()[middleware.RouteKey{Method: method, Path: path}]
	return ok
}
