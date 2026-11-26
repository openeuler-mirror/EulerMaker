package middleware

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	iamv1 "ebs-api/iam/v1"
	"ebs-gateway/internal/iam"
	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/upstream"
)

func TestAuthMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Now()
	tokens, err := identity.NewTokens(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("s", 32))))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		typ        identity.Type
		scope      identity.Scope
		lookupPath string
		route      string
	}{
		{"user", identity.UserType, identity.UserScope, "/apis/iam.ebs/v1/users/account", "/"},
		{"runner", identity.RunnerType, "", "", "/"},
		{"service", identity.ServiceType, "", "/apis/iam.ebs/v1/machineaccounts/account", "/"},
		{"optional user", identity.UserType, identity.UserScope, "/apis/iam.ebs/v1/users/account", "/apis/ebs/v1/projects"},
		{"optional runner", identity.RunnerType, "", "", "/apis/ebs/v1/projects"},
		{"optional service", identity.ServiceType, "", "/apis/iam.ebs/v1/machineaccounts/account", "/apis/ebs/v1/projects"},
	} {
		t.Run(test.name, func(t *testing.T) {
			paths := make(chan string, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths <- r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				if test.typ == identity.ServiceType {
					fmt.Fprint(w, `{"metadata":{"name":"account"},"spec":{"type":"service","projects":["team"],"permissions":["projects:update"]}}`)
				} else {
					fmt.Fprint(w, `{"metadata":{"name":"account"},"spec":{"enabled":true,"scopes":["ebs:user"]}}`)
				}
			}))
			defer server.Close()
			client, err := upstream.New(server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			auth := NewAuth(tokens, iam.New(client), func() time.Time { return now })
			token, err := tokens.Issue("account", test.typ, test.scope, time.Hour, now)
			if err != nil {
				t.Fatal(err)
			}
			router := gin.New()
			router.Use(auth.AuthenticateRequest)
			called := false
			router.GET(test.route, func(c *gin.Context) {
				called = true
				who := Principal(c)
				if who.Subject != "account" || who.Type != test.typ || who.Scope != test.scope {
					t.Errorf("principal = %+v", who)
				}
				var grants *identity.ServiceGrants
				if test.typ == identity.ServiceType {
					grants = &identity.ServiceGrants{Projects: []string{"team"}, Permissions: []iamv1.MachineAccountPermission{iamv1.MachinePermissionProjectsUpdate}}
				}
				if !reflect.DeepEqual(who.ServiceGrants, grants) {
					t.Errorf("service grants = %+v, want %+v", who.ServiceGrants, grants)
				}
				c.Status(http.StatusOK)
			})
			request := httptest.NewRequest(http.MethodGet, test.route, nil)
			request.Header.Set("Authorization", "Bearer "+token)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK || !called {
				t.Fatalf("status=%d handler called=%v", recorder.Code, called)
			}
			var lookups []string
			for len(paths) > 0 {
				lookups = append(lookups, <-paths)
			}
			if strings.Join(lookups, ",") != test.lookupPath {
				t.Fatalf("account lookups=%v, want %q", lookups, test.lookupPath)
			}
		})
	}
}

func TestAuthenticateRequestRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Now()
	tokens, err := identity.NewTokens(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	token, err := tokens.Issue("runner", identity.RunnerType, "", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	auth := NewAuth(tokens, nil, func() time.Time { return now })
	for _, tc := range []struct {
		name, method, route, target, authorization string
		status                                     int
		identified                                 bool
	}{
		{"openapi", "GET", "/openapi.json", "/openapi.json", "", 204, false},
		{"providers", "GET", "/auth/providers", "/auth/providers", "", 204, false},
		{"oauth login", "GET", "/auth/oauth/:provider/login", "/auth/oauth/community/login", "", 204, false},
		{"oauth callback", "POST", "/auth/oauth/:provider/callback", "/auth/oauth/community/callback", "", 204, false},
		{"oauth register", "POST", "/auth/oauth/register", "/auth/oauth/register", "", 204, false},
		{"register", "POST", "/auth/register", "/auth/register", "", 204, false},
		{"login", "POST", "/auth/login", "/auth/login", "Bearer invalid", 204, false},
		{"machine token", "POST", "/auth/machine-token", "/auth/machine-token", "", 204, false},
		{"public list", "GET", "/apis/ebs/v1/projects", "/apis/ebs/v1/projects", "", 204, false},
		{"public invalid token", "GET", "/apis/ebs/v1/projects", "/apis/ebs/v1/projects", "Bearer invalid", 401, false},
		{"public valid token", "GET", "/apis/ebs/v1/projects", "/apis/ebs/v1/projects", "Bearer " + token, 204, true},
		{"public write", "POST", "/apis/ebs/v1/projects", "/apis/ebs/v1/projects", "", 401, false},
		{"job watch", "GET", "/apis/ebs/v1/projects/:project/jobs", "/apis/ebs/v1/projects/demo/jobs?watch=true", "", 401, false},
		{"empty watch", "GET", "/apis/ebs/v1/projects/:project/jobs", "/apis/ebs/v1/projects/demo/jobs?watch=", "", 401, false},
		{"repeated watch", "GET", "/apis/ebs/v1/projects/:project/jobs", "/apis/ebs/v1/projects/demo/jobs?watch=false&watch=true", "", 401, false},
		{"non watch", "GET", "/apis/ebs/v1/projects/:project/jobs", "/apis/ebs/v1/projects/demo/jobs?watch=false", "", 204, false},
		{"authenticated watch", "GET", "/apis/ebs/v1/projects/:project/jobs", "/apis/ebs/v1/projects/demo/jobs?watch=true", "Bearer " + token, 204, true},
		{"runner watch", "GET", "/apis/ebs/v1/runners/:runner/jobs", "/apis/ebs/v1/runners/runner/jobs?watch=true", "", 401, false},
		{"script", "GET", "/apis/ebs/v1/scripts", "/apis/ebs/v1/scripts", "", 401, false},
		{"config", "GET", "/apis/ebs/v1/configs", "/apis/ebs/v1/configs", "", 401, false},
		{"targets", "GET", "/apis/ebs/v1/build-targets", "/apis/ebs/v1/build-targets", "", 401, false},
		{"endpoints", "GET", "/apis/ebs/v1/service-endpoints", "/apis/ebs/v1/service-endpoints", "", 401, false},
		{"check", "POST", "/auth/check", "/auth/check", "", 401, false},
		{"unlisted auth route", "GET", "/auth/new", "/auth/new", "", 401, false},
		{"unlisted resource", "GET", "/apis/ebs/v1/projects/:project/new", "/apis/ebs/v1/projects/demo/new", "", 401, false},
		{"different method", "POST", "/openapi.json", "/openapi.json", "", 401, false},
		{"different template", "GET", "/apis/ebs/v1/projects/demo", "/apis/ebs/v1/projects/demo", "", 401, false},
		{"unknown", "GET", "", "/unknown", "", 401, false},
		{"unknown authenticated", "GET", "", "/unknown", "Bearer " + token, 404, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := gin.New()
			router.Use(auth.AuthenticateRequest)
			identified := false
			endpoint := func(c *gin.Context) {
				identified = Principal(c).Type != ""
				c.Status(http.StatusNoContent)
			}
			if tc.route != "" {
				router.Handle(tc.method, tc.route, endpoint)
			}
			router.NoRoute(func(c *gin.Context) {
				identified = Principal(c).Type != ""
				c.Status(http.StatusNotFound)
			})
			request := httptest.NewRequest(tc.method, tc.target, nil)
			request.Header.Set("Authorization", tc.authorization)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != tc.status || identified != tc.identified {
				t.Fatalf("status=%d identified=%v, want %d/%v", response.Code, identified, tc.status, tc.identified)
			}
		})
	}
}

func TestAuthenticateStopsInvalidToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tokens, err := identity.NewTokens(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("s", 32))))
	if err != nil {
		t.Fatal(err)
	}
	auth := NewAuth(tokens, nil, time.Now)
	for _, header := range []string{"", "Basic secret", "Bearer invalid"} {
		t.Run(header, func(t *testing.T) {
			router := gin.New()
			router.Use(auth.AuthenticateRequest)
			called := false
			router.GET("/", func(c *gin.Context) {
				called = true
			})
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header.Set("Authorization", header)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusUnauthorized || called {
				t.Fatalf("status=%d handler called=%v", recorder.Code, called)
			}
		})
	}
}

func TestRejectedAccountAuditIdentity(t *testing.T) {
	now := time.Now()
	tokens, err := identity.NewTokens(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name         string
		typ          identity.Type
		scope        identity.Scope
		iamStatus    int
		wantStatus   int
		auditSubject string
	}{
		{"missing user", identity.UserType, identity.UserScope, 404, 403, "account"},
		{"missing service", identity.ServiceType, "", 404, 401, ""},
		{"user lookup unavailable", identity.UserType, identity.UserScope, 503, 503, "account"},
		{"service lookup unavailable", identity.ServiceType, "", 503, 503, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.iamStatus)
			}))
			defer server.Close()
			client, err := upstream.New(server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			token, err := tokens.Issue("account", tc.typ, tc.scope, time.Hour, now)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
			c.Request.Header.Set("Authorization", "Bearer "+token)
			auth := NewAuth(tokens, iam.New(client), func() time.Time { return now })
			if auth.verifyAccess(c) || !c.IsAborted() || response.Code != tc.wantStatus || Principal(c).Subject != tc.auditSubject {
				t.Fatalf("aborted=%v status=%d audit subject=%q", c.IsAborted(), response.Code, Principal(c).Subject)
			}
		})
	}
}

func TestPrincipalWithoutAuthentication(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if got := Principal(c); got != (identity.Principal{}) {
		t.Fatalf("anonymous principal = %+v", got)
	}
}
