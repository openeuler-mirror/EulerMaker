package handler

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/policy"
	"ebs-gateway/internal/upstream"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRunnerJobLookupErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{"assigned", 200, `{"status":{"runner":"runner-1"}}`, 200},
		{"unassigned", 200, `{"status":{"runner":"runner-2"}}`, 403},
		{"deleted", 404, "", 404},
		{"unavailable", 503, "private server details", 503},
		{"throttled", 429, "", 503},
		{"connection error", 0, "", 503},
		{"invalid JSON", 200, "{", 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := upstream.New("https://api.example", roundTripFunc(func(*http.Request) (*http.Response, error) {
				if tc.status == 0 {
					return nil, errors.New("private connection details")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			a := &Handler{upstream: client, bodyLimit: 1 << 20}
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/apis/ebs/v1/projects/team/jobs/job-1", nil)
			allowed := a.runnerOwnsJob(ctx, "runner-1")
			if allowed != (tc.want == 200) || recorder.Code != tc.want {
				t.Fatalf("allowed=%v status=%d, want %d", allowed, recorder.Code, tc.want)
			}
			if strings.Contains(recorder.Body.String(), "private") {
				t.Fatal("upstream details exposed")
			}
		})
	}
}

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

func TestRunnerJobsQueryRejectsCallerFieldSelector(t *testing.T) {
	for _, test := range []struct {
		query url.Values
		allow bool
	}{
		{url.Values{"watch": {"true"}, "resourceVersion": {"7"}, "timeoutSeconds": {"300"}}, true},
		{url.Values{"limit": {"100"}, "continue": {"next"}}, true},
		{url.Values{"includeFields": {"metadata.name,status.phase"}, "excludeFields": {"status.stage"}}, true},
		{url.Values{"fieldSelector": {"status.runner=other"}}, false},
		{url.Values{"watch": {"true", "false"}}, false},
		{url.Values{"timeoutSeconds": {"301"}}, false},
	} {
		if got := validRunnerJobsQuery(test.query); got != test.allow {
			t.Errorf("validRunnerJobsQuery(%v) = %v, want %v", test.query, got, test.allow)
		}
	}
}

func TestRunnerResourceQueryAndWatchLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name            string
		query           string
		user            bool
		active          bool
		upstreamFailure bool
		wantStatus      int
		wantCalls       int
	}{
		{"runner watch", "watch=true", false, false, false, 200, 1},
		{"upstream failure releases watch", "watch=true", false, false, true, 502, 1},
		{"second watch denied", "watch=true", false, true, false, 429, 0},
		{"query checked before quota", "watch=true&fieldSelector=status.runner=other", false, true, false, 403, 0},
		{"list does not acquire watch", "watch=false", false, true, false, 200, 1},
		{"user is not subject to Runner restrictions", "watch=true&fieldSelector=status.runner=other", true, true, false, 200, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			api := &Handler{runnerWatches: make(map[string]int)}
			client, err := upstream.New("https://api.example", roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				if !tc.user && tc.query == "watch=true" && api.runnerWatches["runner-1"] != 1 {
					t.Error("watch forwarded without holding its quota")
				}
				if tc.upstreamFailure {
					return nil, errors.New("connection failed")
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			api.upstream = client
			if tc.active {
				api.runnerWatches["runner-1"] = 1
			}
			who := identity.Principal{Type: identity.RunnerType, Subject: "runner-1", Runner: "runner-1"}
			if tc.user {
				who = identity.Principal{Type: identity.UserType, Subject: "admin", Scope: identity.AdminScope}
			}
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/apis/ebs/v1/runners/runner-1/jobs?"+tc.query, nil)
			api.forwardRunnerResource(ctx, who, policy.Route{Resource: "runners", Name: "runner-1", Subresource: "jobs", Method: http.MethodGet})
			if recorder.Code != tc.wantStatus || calls != tc.wantCalls {
				t.Fatalf("status=%d calls=%d, want %d/%d", recorder.Code, calls, tc.wantStatus, tc.wantCalls)
			}
			if tc.active {
				if api.runnerWatches["runner-1"] != 1 {
					t.Fatal("existing watch quota changed")
				}
			} else if len(api.runnerWatches) != 0 {
				t.Fatal("watch quota not released")
			}
			if tc.wantStatus == http.StatusTooManyRequests && recorder.Header().Get("Retry-After") != "5" {
				t.Fatal("missing watch retry delay")
			}
		})
	}
}
