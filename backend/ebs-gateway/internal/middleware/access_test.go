package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	iamv1 "ebs-api/iam/v1"
	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/policy"
)

func TestAdmitIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		who  identity.Principal
		want int
	}{
		{"anonymous", identity.Principal{}, http.StatusForbidden},
		{"user", identity.Principal{Type: identity.UserType, Subject: "user", Scope: identity.UserScope}, http.StatusNoContent},
		{"runner", identity.Principal{Type: identity.RunnerType, Subject: "runner"}, http.StatusNoContent},
		{"service without grants", identity.Principal{Type: identity.ServiceType, Subject: "service"}, http.StatusNoContent},
		{"unknown identity", identity.Principal{Type: "unknown", Subject: "other"}, http.StatusForbidden},
		{"missing subject", identity.Principal{Type: identity.ServiceType}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := gin.New()
			router.Use(func(c *gin.Context) { c.Set(principalKey, tc.who) })
			router.POST("/check", AdmitIdentity(policy.AllowAuthenticated), func(c *gin.Context) {
				c.Status(http.StatusNoContent)
			})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/check", nil))
			if response.Code != tc.want {
				t.Fatalf("status=%d, want %d", response.Code, tc.want)
			}
		})
	}
}

func TestAuthorize(t *testing.T) {
	identities := []struct {
		name string
		who  identity.Principal
	}{
		{"anonymous", identity.Principal{}},
		{"user", identity.Principal{Type: identity.UserType, Subject: "user", Scope: identity.UserScope}},
		{"ops", identity.Principal{Type: identity.UserType, Subject: "ops", Scope: identity.OpsScope}},
		{"admin", identity.Principal{Type: identity.UserType, Subject: "admin", Scope: identity.AdminScope}},
		{"runner", identity.Principal{Type: identity.RunnerType, Subject: "runner"}},
		{"service", identity.Principal{Type: identity.ServiceType, Subject: "service", ServiceGrants: &identity.ServiceGrants{Projects: []string{"team"}, Permissions: []iamv1.MachineAccountPermission{iamv1.MachinePermissionBuildsCreate}}}},
		{"empty subject", identity.Principal{Type: identity.RunnerType}},
		{"invalid scope", identity.Principal{Type: identity.UserType, Subject: "user", Scope: "invalid"}},
		{"unknown type", identity.Principal{Type: "unknown", Subject: "user"}},
	}

	rules := map[RouteKey]RoutePolicy{
		{http.MethodGet, "/anonymous"}:                       {Access: RouteAccess{Authentication: AuthenticationNone, Authorization: policy.Rule{Identities: policy.AllowAnonymous}}},
		{http.MethodGet, "/users"}:                           {Access: RouteAccess{Authorization: policy.Rule{Identities: policy.AllowUsers}}},
		{http.MethodGet, "/privileged"}:                      {Access: RouteAccess{Authorization: policy.Rule{Identities: policy.AllowPrivileged}}},
		{http.MethodGet, "/admin"}:                           {Access: RouteAccess{Authorization: policy.Rule{Identities: policy.AllowAdmin}}},
		{http.MethodGet, "/runner/:name"}:                    {Access: RouteAccess{Authorization: policy.Rule{Identities: policy.AllowRunner}}},
		{http.MethodGet, "/projects/:project/service"}:       {Access: RouteAccess{Authorization: policy.Rule{Identities: policy.AllowService, ServicePermission: iamv1.MachinePermissionBuildsCreate}}},
		{http.MethodGet, "/projects/:project/authenticated"}: {Access: RouteAccess{Authorization: policy.Rule{Identities: policy.AllowAuthenticated, ServicePermission: iamv1.MachinePermissionBuildsCreate}}},
		{http.MethodGet, "/optional"}:                        {Access: RouteAccess{Authentication: AuthenticationOptional, Authorization: policy.Rule{Identities: policy.AllowAnonymous | policy.AllowUsers}}},
		{http.MethodGet, "/service-unconfigured"}:            {Access: RouteAccess{Authorization: policy.Rule{Identities: policy.AllowService}}},
		{http.MethodGet, "/service-invalid"}:                 {Access: RouteAccess{Authorization: policy.Rule{Identities: policy.AllowService, ServicePermission: "unknown"}}},
	}
	access := NewRoutePolicies(rules)
	for _, tc := range []struct {
		method, path, allowed string
	}{
		{http.MethodGet, "/anonymous", "anonymous"},
		{http.MethodGet, "/users", "user,ops,admin"},
		{http.MethodGet, "/privileged", "ops,admin"},
		{http.MethodGet, "/admin", "admin"},
		{http.MethodGet, "/runner/:name", "runner"},
		{http.MethodGet, "/projects/:project/service", "service"},
		{http.MethodGet, "/projects/:project/authenticated", "user,ops,admin,runner,service"},
		{http.MethodGet, "/optional", "anonymous,user,ops,admin"},
		{http.MethodGet, "/service-unconfigured", ""},
		{http.MethodGet, "/service-invalid", ""},
		{http.MethodPost, "/runner/:name", ""},
		{http.MethodGet, "/runner/name", ""},
		{http.MethodGet, "/new-resource", ""},
	} {
		for _, subject := range identities {
			t.Run(tc.method+tc.path+"/"+subject.name, func(t *testing.T) {
				router := gin.New()
				called := false
				decision := ""
				router.Use(func(c *gin.Context) {
					c.Set(principalKey, subject.who)
					c.Next()
					decision = c.GetString("serviceAuthorization")
				}, Authorize(access, nil))
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
				if subject.who.Type == identity.ServiceType {
					want := "denied"
					if allowed {
						want = "allowed"
					}
					if decision != want {
						t.Fatalf("service audit=%s, want %s", decision, want)
					}
				}
			})
		}
	}
}

func TestServiceAuthorizationAudit(t *testing.T) {
	for _, project := range []string{"team", "other"} {
		t.Run(project, func(t *testing.T) {
			who := identity.Principal{
				Type: identity.ServiceType, Subject: "service",
				ServiceGrants: &identity.ServiceGrants{
					Projects:    []string{"team"},
					Permissions: []iamv1.MachineAccountPermission{iamv1.MachinePermissionBuildsCreate},
				},
			}
			access := NewRoutePolicies(map[RouteKey]RoutePolicy{
				{http.MethodPost, "/projects/:project/builds"}: {
					Access: RouteAccess{Authorization: policy.Rule{
						Identities:        policy.AllowService,
						ServicePermission: iamv1.MachinePermissionBuildsCreate,
					}},
				},
			})
			router := gin.New()
			router.Use(func(c *gin.Context) {
				c.Set(principalKey, who)
				c.Next()
				want := "denied"
				if project == "team" {
					want = "allowed"
				}
				if c.GetString("serviceAuthorization") != want ||
					c.GetString("serviceProject") != project ||
					c.GetString("servicePermission") != string(iamv1.MachinePermissionBuildsCreate) {
					t.Fatalf("unexpected audit: %v", c.Keys)
				}
			}, Authorize(access, policy.New(nil, 1<<20)))
			router.POST("/projects/:project/builds", func(c *gin.Context) { c.Status(http.StatusNoContent) })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/projects/"+project+"/builds", nil))
			want := http.StatusForbidden
			if project == "team" {
				want = http.StatusNoContent
			}
			if response.Code != want {
				t.Fatalf("status=%d, want %d", response.Code, want)
			}
		})
	}
}
