package gateway

import (
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/iam"
	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/limit"
	"ebs-gateway/internal/upstream"
)

func TestValidRunnerJobsQuery(t *testing.T) {
	for _, test := range []struct {
		query url.Values
		allow bool
	}{
		{url.Values{"watch": {"true"}, "resourceVersion": {"7"}, "timeoutSeconds": {"300"}}, true},
		{url.Values{"limit": {"100"}, "continue": {"next"}}, true},
		{url.Values{"includeFields": {"metadata.name,status.phase"}, "excludeFields": {"status.stage"}}, true},
		{url.Values{"fieldSelector": {"status.runner=other"}}, false},
		{url.Values{"watch": {"true", "false"}}, false},
		{url.Values{"includeFields": {"metadata.name", "status.phase"}}, false},
		{url.Values{"resourceVersion": nil}, false},
		{url.Values{"timeoutSeconds": {"301"}}, false},
	} {
		if got := validRunnerJobsQuery(test.query); got != test.allow {
			t.Errorf("validRunnerJobsQuery(%v) = %v, want %v", test.query, got, test.allow)
		}
	}
}

func TestRunnerResourceQueryAndWatchLifecycle(t *testing.T) {
	tokens, err := identity.NewTokens(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
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
			var api *Handler
			client, err := upstream.New("https://api.example", roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if tc.user && r.URL.Path == "/apis/iam.ebs/v1/users/admin" {
					body := `{"metadata":{"name":"admin"},"spec":{"enabled":true,"scopes":["ebs:admin"]}}`
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
				}
				calls++
				if !tc.user && tc.query == "watch=true" && !api.runnerWatches.Has("runner-1") {
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
			api, err = NewHandler(Dependencies{Upstream: client, IAM: iam.New(client), Tokens: tokens, Limits: limit.New(100, 100, time.Now), BodyLimit: 1 << 20})
			if err != nil {
				t.Fatal(err)
			}
			if tc.active {
				api.runnerWatches.Insert("runner-1")
			}
			who := identity.Principal{Type: identity.RunnerType, Subject: "runner-1", Runner: "runner-1"}
			if tc.user {
				who = identity.Principal{Type: identity.UserType, Subject: "admin", Scope: identity.AdminScope}
			}
			token, err := tokens.Issue(who.Subject, who.Runner, who.Type, who.Scope, time.Hour, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			router := gin.New()
			router.GET("/runners/:runner/jobs", api.resourceGuard("runners", "", "runner", "jobs"), api.runnerJobsListGuard, api.forwardResource)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/runners/runner-1/jobs?"+tc.query, nil)
			request.Header.Set("Authorization", "Bearer "+token)
			router.ServeHTTP(recorder, request)
			if recorder.Code != tc.wantStatus || calls != tc.wantCalls {
				t.Fatalf("status=%d calls=%d, want %d/%d", recorder.Code, calls, tc.wantStatus, tc.wantCalls)
			}
			if tc.active {
				if !api.runnerWatches.Has("runner-1") {
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
