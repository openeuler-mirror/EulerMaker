package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/policy"
)

func TestResourceMethodBoundary(t *testing.T) {
	tests := []struct {
		name      string
		route     policy.Route
		wantAllow bool
	}{
		{"project delete", policy.Route{Resource: "projects", Name: "p", Method: http.MethodDelete}, false},
		{"collection list", policy.Route{Resource: "projects", Method: http.MethodGet}, true},
		{"collection update", policy.Route{Resource: "projects", Method: http.MethodPut}, false},
		{"named create", policy.Route{Resource: "projects", Name: "p", Method: http.MethodPost}, false},
		{"status read", policy.Route{Resource: "jobs", Name: "j", Subresource: "status", Method: http.MethodGet}, true},
		{"status delete", policy.Route{Resource: "jobs", Name: "j", Subresource: "status", Method: http.MethodDelete}, false},
		{"abort post", policy.Route{Resource: "jobs", Name: "j", Subresource: "abort", Method: http.MethodPost}, true},
		{"abort get", policy.Route{Resource: "jobs", Name: "j", Subresource: "abort", Method: http.MethodGet}, false},
		{"runner jobs watch", policy.Route{Resource: "runners", Name: "r", Subresource: "jobs", Method: http.MethodGet}, true},
		{"runner jobs mutate", policy.Route{Resource: "runners", Name: "r", Subresource: "jobs", Method: http.MethodPatch}, false},
		{"snapshot list", policy.Route{Resource: "snapshots", Project: "p", Method: http.MethodGet}, true},
		{"snapshot create", policy.Route{Resource: "snapshots", Project: "p", Method: http.MethodPost}, false},
		{"buildinfo update", policy.Route{Resource: "buildinfos", Project: "p", Name: "b", Method: http.MethodPut}, false},
		{"rpmrepo status read", policy.Route{Resource: "rpmrepos", Project: "p", Name: "r", Subresource: "status", Method: http.MethodGet}, true},
		{"rpmrepo status update", policy.Route{Resource: "rpmrepos", Project: "p", Name: "r", Subresource: "status", Method: http.MethodPatch}, false},
		{"rpmrepo head", policy.Route{Resource: "rpmrepos", Project: "p", Name: "r", Method: http.MethodHead}, false},
		{"script head", policy.Route{Resource: "scripts", Name: "s", Method: http.MethodHead}, false},
		{"project abort", policy.Route{Resource: "projects", Name: "p", Subresource: "abort", Method: http.MethodPost}, false},
		{"runner abort", policy.Route{Resource: "runners", Name: "r", Subresource: "abort", Method: http.MethodPost}, false},
		{"config status", policy.Route{Resource: "configs", Name: "c", Subresource: "status", Method: http.MethodGet}, false},
		{"script status", policy.Route{Resource: "scripts", Name: "s", Subresource: "status", Method: http.MethodGet}, false},
		{"oldjob status", policy.Route{Resource: "oldjobs", Name: "j", Subresource: "status", Method: http.MethodGet}, false},
		{"unnamed abort", policy.Route{Resource: "jobs", Subresource: "abort", Method: http.MethodPost}, false},
		{"unknown resource", policy.Route{Resource: "unknown", Method: http.MethodGet}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(test.route.Method, "/test", nil)
			if got := validateResourceRoute(ctx, test.route); got != test.wantAllow {
				t.Fatalf("method allowed = %v, want %v", got, test.wantAllow)
			}
		})
	}
}

func TestResourceAllowHeader(t *testing.T) {
	for _, tc := range []struct {
		name  string
		route policy.Route
		want  string
	}{
		{"project collection", policy.Route{Resource: "projects"}, "GET, HEAD, POST"},
		{"project object", policy.Route{Resource: "projects", Name: "p"}, "GET, HEAD, PUT, PATCH"},
		{"script collection", policy.Route{Resource: "scripts"}, "GET, POST"},
		{"script object", policy.Route{Resource: "scripts", Name: "s"}, "GET, PUT, PATCH, DELETE"},
		{"job object", policy.Route{Resource: "jobs", Name: "j"}, "GET, HEAD, PUT, PATCH, DELETE"},
		{"job status", policy.Route{Resource: "jobs", Name: "j", Subresource: "status"}, "GET, HEAD, PUT, PATCH"},
		{"abort", policy.Route{Resource: "jobs", Name: "j", Subresource: "abort"}, "POST"},
		{"runner jobs", policy.Route{Resource: "runners", Name: "r", Subresource: "jobs"}, "GET"},
		{"read only", policy.Route{Resource: "buildinfos", Name: "b", Subresource: "status"}, "GET"},
		{"build object", policy.Route{Resource: "builds", Name: "b"}, "GET, HEAD, PUT, PATCH, DELETE"},
		{"config object", policy.Route{Resource: "configs", Name: "c"}, "GET, HEAD, PUT, PATCH, DELETE"},
		{"runner status", policy.Route{Resource: "runners", Name: "r", Subresource: "status"}, "GET, HEAD, PUT, PATCH"},
		{"oldjob object", policy.Route{Resource: "oldjobs", Name: "j"}, "GET"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodOptions, "/test", nil)
			tc.route.Method = http.MethodOptions
			if validateResourceRoute(ctx, tc.route) || !ctx.IsAborted() || recorder.Code != http.StatusMethodNotAllowed {
				t.Fatal("unsupported method was not rejected")
			}
			if got := recorder.Header().Get("Allow"); got != tc.want {
				t.Fatalf("Allow=%q, want %q", got, tc.want)
			}
			for _, method := range strings.Split(tc.want, ", ") {
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(method, "/test", nil)
				tc.route.Method = method
				if !validateResourceRoute(ctx, tc.route) {
					t.Fatalf("advertised method %s rejected", method)
				}
			}
		})
	}
}

func TestResourceWatchBoundary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		route policy.Route
		allow bool
	}{
		{"project jobs", policy.Route{Resource: "jobs", Project: "p", Method: http.MethodGet}, true},
		{"runner jobs", policy.Route{Resource: "runners", Name: "r", Subresource: "jobs", Method: http.MethodGet}, true},
		{"global jobs", policy.Route{Resource: "jobs", Method: http.MethodGet}, false},
		{"named job", policy.Route{Resource: "jobs", Project: "p", Name: "j", Method: http.MethodGet}, false},
		{"job status", policy.Route{Resource: "jobs", Project: "p", Name: "j", Subresource: "status", Method: http.MethodGet}, false},
		{"job creation", policy.Route{Resource: "jobs", Project: "p", Method: http.MethodPost}, false},
		{"job HEAD", policy.Route{Resource: "jobs", Project: "p", Method: http.MethodHead}, false},
		{"runners", policy.Route{Resource: "runners", Method: http.MethodGet}, false},
		{"builds", policy.Route{Resource: "builds", Project: "p", Method: http.MethodGet}, false},
		{"projects", policy.Route{Resource: "projects", Method: http.MethodGet}, false},
		{"configs", policy.Route{Resource: "configs", Method: http.MethodGet}, false},
		{"scripts", policy.Route{Resource: "scripts", Method: http.MethodGet}, false},
		{"snapshots", policy.Route{Resource: "snapshots", Project: "p", Method: http.MethodGet}, false},
		{"oldjobs", policy.Route{Resource: "oldjobs", Project: "p", Method: http.MethodGet}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, query := range []string{"watch=true", "watch=false&watch=true"} {
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(tc.route.Method, "/test?"+query, nil)
				if got := validateResourceRoute(ctx, tc.route); got != tc.allow {
					t.Fatalf("watch allowed=%v, want %v", got, tc.allow)
				}
				if !tc.allow && (recorder.Code != http.StatusForbidden || !ctx.IsAborted()) {
					t.Fatal("unsupported watch must abort with 403")
				}
			}
		})
	}
}
