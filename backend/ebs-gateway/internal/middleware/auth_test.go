package middleware

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

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
		name        string
		typ         identity.Type
		scope       identity.Scope
		resolveUser bool
		status      int
		lookupPath  string
	}{
		{"user", identity.UserType, identity.UserScope, true, http.StatusOK, "/apis/iam.ebs/v1/users/account"},
		{"runner", identity.RunnerType, "", true, http.StatusOK, ""},
		{"service token check", identity.ServiceType, "", false, http.StatusOK, "/apis/iam.ebs/v1/machineaccounts/account"},
		{"service skips user resolution", identity.ServiceType, "", true, http.StatusOK, "/apis/iam.ebs/v1/machineaccounts/account"},
	} {
		t.Run(test.name, func(t *testing.T) {
			paths := make(chan string, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths <- r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				if test.typ == identity.ServiceType {
					fmt.Fprint(w, `{"metadata":{"name":"account"},"spec":{"type":"service"}}`)
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
			runner := ""
			if test.typ == identity.RunnerType {
				runner = "account"
			}
			token, err := tokens.Issue("account", runner, test.typ, test.scope, time.Hour, now)
			if err != nil {
				t.Fatal(err)
			}
			router := gin.New()
			router.Use(auth.Authenticate)
			if test.resolveUser {
				router.Use(auth.ResolveUser)
			}
			called := false
			router.GET("/", func(c *gin.Context) {
				called = true
				who := Principal(c)
				if who.Subject != "account" || who.Type != test.typ || who.Scope != test.scope || who.Runner != runner {
					t.Errorf("principal = %+v", who)
				}
				c.Status(http.StatusOK)
			})
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header.Set("Authorization", "Bearer "+token)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != test.status || called != (test.status == http.StatusOK) {
				t.Fatalf("status=%d handler called=%v, want status=%d", recorder.Code, called, test.status)
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

func TestPrincipalWithoutAuthentication(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if got := Principal(c); got != (identity.Principal{}) {
		t.Fatalf("anonymous principal = %+v", got)
	}
}
