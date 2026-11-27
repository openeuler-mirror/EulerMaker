package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestPrepareQueryPagination(t *testing.T) {
	rules := NewRoutePolicies(map[RouteKey]RoutePolicy{
		{http.MethodGet, "/items"}: {Options: RouteOptions{Watch: true, ListLimit: 100}},
		{http.MethodGet, "/small"}: {Options: RouteOptions{ListLimit: 20}},
	})
	for _, tc := range []struct {
		name, method, path, query, want string
	}{
		{"default", "GET", "/items", "", "limit=100"},
		{"valid", "GET", "/items", "limit=10", "limit=10"},
		{"upper bound", "GET", "/items", "limit=100", "limit=100"},
		{"too large", "GET", "/items", "limit=101", "limit=100"},
		{"zero", "GET", "/items", "limit=0", "limit=100"},
		{"negative", "GET", "/items", "limit=-1", "limit=100"},
		{"invalid", "GET", "/items", "limit=bad", "limit=100"},
		{"repeated", "GET", "/items", "limit=1&limit=2", "limit=100"},
		{"other parameters", "GET", "/items", "continue=next&limit=200", "continue=next&limit=100"},
		{"configured maximum", "GET", "/small", "limit=30", "limit=20"},
		{"watch", "GET", "/items", "watch=true&limit=500", "watch=true&limit=500"},
		{"numeric watch", "GET", "/items", "watch=1", "watch=1"},
		{"repeated watch", "GET", "/items", "watch=false&watch=true", "watch=false&watch=true"},
		{"ordinary list", "GET", "/items", "watch=false", "limit=100&watch=false"},
		{"unconfigured", "GET", "/other", "limit=500", "limit=500"},
		{"object", "GET", "/items/one", "", ""},
		{"write", "POST", "/items", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := gin.New()
			router.Use(PrepareQuery(rules))
			router.Handle(tc.method, tc.path, func(c *gin.Context) {
				if c.Request.URL.RawQuery != tc.want {
					t.Errorf("query=%q, want %q", c.Request.URL.RawQuery, tc.want)
				}
				c.Status(http.StatusNoContent)
			})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path+"?"+tc.query, nil))
			if response.Code != http.StatusNoContent {
				t.Fatalf("status=%d", response.Code)
			}
		})
	}
}

func TestPrepareQueryWatch(t *testing.T) {
	routes := NewRoutePolicies(map[RouteKey]RoutePolicy{
		{http.MethodGet, "/apis/ebs/v1/projects/:project/jobs"}: {Options: RouteOptions{Watch: true}},
		{http.MethodGet, "/apis/ebs/v1/runners/:runner/jobs"}:   {Options: RouteOptions{Watch: true}},
	})
	for _, tc := range []struct {
		name, method, pattern, path, query string
		status                             int
	}{
		{"Job list", "GET", "/apis/ebs/v1/projects/:project/jobs", "/apis/ebs/v1/projects/demo/jobs", "watch=true", 204},
		{"Runner assignments", "GET", "/apis/ebs/v1/runners/:runner/jobs", "/apis/ebs/v1/runners/runner-1/jobs", "watch=true", 204},
		{"create Build", "POST", "/apis/ebs/v1/projects/:project/builds", "/apis/ebs/v1/projects/demo/builds", "watch=true", 403},
		{"write assignments", "PUT", "/apis/ebs/v1/runners/:runner/jobs", "/apis/ebs/v1/runners/runner-1/jobs", "watch=true", 403},
		{"named Job", "GET", "/apis/ebs/v1/projects/:project/jobs/:name", "/apis/ebs/v1/projects/demo/jobs/job-1", "watch=true", 403},
		{"Job status", "GET", "/apis/ebs/v1/projects/:project/jobs/:name/status", "/apis/ebs/v1/projects/demo/jobs/job-1/status", "watch=true", 403},
		{"static lookalike", "GET", "/apis/ebs/v1/projects/demo/jobs", "/apis/ebs/v1/projects/demo/jobs", "watch=true", 403},
		{"new resource", "GET", "/apis/ebs/v1/new-resource", "/apis/ebs/v1/new-resource", "watch=true", 403},
		{"noncanonical Watch", "GET", "/openapi.json", "/openapi.json", "watch=1", 403},
		{"repeated Watch", "GET", "/openapi.json", "/openapi.json", "watch=false&watch=true", 403},
		{"empty Watch", "GET", "/openapi.json", "/openapi.json", "watch=", 403},
		{"disabled Watch", "GET", "/openapi.json", "/openapi.json", "watch=false", 204},
		{"ordinary request", "GET", "/openapi.json", "/openapi.json", "", 204},
		{"unknown path", "GET", "/openapi.json", "/missing", "watch=true", 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := gin.New()
			router.Use(PrepareQuery(routes))
			handled := false
			router.Handle(tc.method, tc.pattern, func(c *gin.Context) {
				handled = true
				c.Status(http.StatusNoContent)
			})
			noRoute := false
			router.NoRoute(func(c *gin.Context) {
				noRoute = true
				c.Status(http.StatusNotFound)
			})
			res := httptest.NewRecorder()
			router.ServeHTTP(res, httptest.NewRequest(tc.method, tc.path+"?"+tc.query, nil))
			if res.Code != tc.status || handled != (tc.status == 204) || noRoute != (tc.status == 404) {
				t.Fatalf("status=%d handled=%v noRoute=%v, want status %d", res.Code, handled, noRoute, tc.status)
			}
			if tc.status == 403 && res.Body.String() != `{"error":"watch is not supported for this route"}` {
				t.Fatalf("unexpected rejection: %s", res.Body)
			}
		})
	}
}
