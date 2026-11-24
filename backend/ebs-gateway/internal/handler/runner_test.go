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
