package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestValidateWatchRoute(t *testing.T) {
	routes := NewRoutePolicies(map[RouteKey]RoutePolicy{
		{http.MethodGet, "/apis/ebs/v1/projects/:project/jobs"}: {Watch: true},
		{http.MethodGet, "/apis/ebs/v1/runners/:runner/jobs"}:   {Watch: true},
	})
	for _, tc := range []struct {
		name, method, pattern, path, query string
		status                             int
	}{
		{"Job list", "GET", "/apis/ebs/v1/projects/:project/jobs", "/apis/ebs/v1/projects/demo/jobs", "watch=true", 204},
		{"Runner assignments", "GET", "/apis/ebs/v1/runners/:runner/jobs", "/apis/ebs/v1/runners/runner-1/jobs", "watch=true", 204},
		{"create Job", "POST", "/apis/ebs/v1/projects/:project/jobs", "/apis/ebs/v1/projects/demo/jobs", "watch=true", 403},
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
			router.Use(ValidateWatchRoute(routes))
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
