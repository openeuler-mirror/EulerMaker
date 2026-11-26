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
	}{
		{"user", identity.UserType, identity.UserScope, "/apis/iam.ebs/v1/users/account"},
		{"runner", identity.RunnerType, "", ""},
		{"service", identity.ServiceType, "", "/apis/iam.ebs/v1/machineaccounts/account"},
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
			called := false
			router.GET("/", auth.Authenticate, func(c *gin.Context) {
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
			request := httptest.NewRequest(http.MethodGet, "/", nil)
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
			called := false
			router.GET("/", auth.Authenticate, func(c *gin.Context) {
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
			if auth.VerifyAccess(c) || !c.IsAborted() || response.Code != tc.wantStatus || Principal(c).Subject != tc.auditSubject {
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

func TestRequireAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, scope := range []identity.Scope{"", identity.UserScope, identity.OpsScope, identity.AdminScope} {
		t.Run(string(scope), func(t *testing.T) {
			router := gin.New()
			called := false
			router.GET("/", func(c *gin.Context) {
				c.Set(principalKey, identity.Principal{Type: identity.UserType, Scope: scope})
			}, RequireAdmin, func(c *gin.Context) {
				called = true
				c.Status(http.StatusNoContent)
			})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
			want := http.StatusForbidden
			if scope == identity.AdminScope {
				want = http.StatusNoContent
			}
			if response.Code != want || called != (scope == identity.AdminScope) {
				t.Fatalf("status=%d handler called=%v", response.Code, called)
			}
		})
	}
}
