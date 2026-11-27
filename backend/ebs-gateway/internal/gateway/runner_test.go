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

	"ebs-gateway/internal/iamclient"
	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/limit"
	"ebs-gateway/internal/middleware"
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

func TestRunnerRoutesLimitOnce(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "", http.StatusForbidden},
		{http.MethodGet, "/runner-1", http.StatusOK},
		{http.MethodGet, "/runner-1/status", http.StatusForbidden},
		{http.MethodGet, "/runner-1/jobs", http.StatusOK},
		{http.MethodGet, "/runner-1/jobs?watch=true", http.StatusOK},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			calls := 0
			api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			}))
			api.limits = limit.New(1, 1, api.now)
			token, err := api.tokens.Issue("runner-1", identity.RunnerType, "", time.Hour, api.now())
			if err != nil {
				t.Fatal(err)
			}
			router := api.Router()
			for i, status := range []int{http.StatusUnauthorized, tc.status, http.StatusTooManyRequests} {
				req := httptest.NewRequest(tc.method, "/apis/ebs/v1/runners"+tc.path, nil)
				if i > 0 {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				res := httptest.NewRecorder()
				router.ServeHTTP(res, req)
				if res.Code != status {
					t.Fatalf("request %d: status=%d, want %d: %s", i, res.Code, status, res.Body)
				}
				if status == http.StatusTooManyRequests && res.Header().Get("Retry-After") != "1" {
					t.Fatal("missing rate limit retry delay")
				}
			}
			wantCalls := 0
			if tc.status == http.StatusOK {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("upstream calls=%d, want %d", calls, wantCalls)
			}
		})
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
		active          bool
		upstreamFailure bool
		wantStatus      int
		wantCalls       int
	}{
		{"runner list", "", false, false, 200, 1},
		{"list rejects field selector", "fieldSelector=status.runner=other", false, false, 403, 0},
		{"runner watch", "watch=true", false, false, 200, 1},
		{"upstream failure releases watch", "watch=true", false, true, 502, 1},
		{"second watch denied", "watch=true", true, false, 429, 0},
		{"query checked before quota", "watch=true&fieldSelector=status.runner=other", true, false, 403, 0},
		{"list does not acquire watch", "watch=false", true, false, 200, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var api *Handler
			client, err := upstream.New("https://api.example", roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/apis/ebs/v1/runners/runner-1/jobs" {
					t.Fatalf("unexpected upstream path: %s", r.URL.Path)
				}
				calls++
				if tc.query == "watch=true" && !api.runnerWatches.Has("runner-1") {
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
			api, err = NewHandler(Dependencies{Upstream: client, Auth: middleware.NewAuth(tokens, iamclient.New(client), time.Now), Limits: limit.New(100, 100, time.Now), BodyLimit: 1 << 20})
			if err != nil {
				t.Fatal(err)
			}
			if tc.active {
				api.runnerWatches.Insert("runner-1")
			}
			who := identity.Principal{Type: identity.RunnerType, Subject: "runner-1"}
			token, err := tokens.Issue(who.Subject, who.Type, who.Scope, time.Hour, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			router := gin.New()
			router.Use(middleware.ValidateWatchRoute(middleware.NewRoutePolicies(routeRules())), middleware.Authenticate(api.auth, middleware.NewRoutePolicies(routeRules())),
				middleware.LimitIdentity(api.limits))
			registerRunners(router, api)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/apis/ebs/v1/runners/runner-1/jobs?"+tc.query, nil)
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

func TestRunnerRoutesRequireAuthorization(t *testing.T) {
	api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/apis/iam.ebs/v1/users/user":
			body = `{"metadata":{"name":"user"},"spec":{"enabled":true,"scopes":["ebs:user"]}}`
		case "/apis/iam.ebs/v1/machineaccounts/service":
			body = `{"metadata":{"name":"service"},"spec":{"type":"service"}}`
		default:
			t.Fatalf("unauthorized request reached upstream: %s", r.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	router := api.Router()
	for _, who := range []identity.Principal{
		{},
		{Subject: "user", Type: identity.UserType, Scope: identity.UserScope},
		{Subject: "service", Type: identity.ServiceType},
		{Subject: "runner-1", Type: identity.RunnerType},
	} {
		var token string
		status := http.StatusUnauthorized
		if who.Type != "" {
			var err error
			token, err = api.tokens.Issue(who.Subject, who.Type, who.Scope, time.Hour, api.now())
			if err != nil {
				t.Fatal(err)
			}
			status = http.StatusForbidden
		}
		for _, tc := range []struct{ method, path string }{
			{"GET", ""}, {"POST", ""},
			{"GET", "/other"}, {"PUT", "/other"}, {"PATCH", "/other"}, {"DELETE", "/other"},
			{"GET", "/other/status"}, {"PUT", "/other/status"}, {"PATCH", "/other/status"},
			{"GET", "/other/jobs"}, {"GET", "/other/jobs?watch=true"},
		} {
			t.Run(who.Subject+"/"+tc.method+tc.path, func(t *testing.T) {
				req := httptest.NewRequest(tc.method, "/apis/ebs/v1/runners"+tc.path, strings.NewReader(`{"metadata":{"name":"other"}}`))
				req.Header.Set("Content-Type", "application/json")
				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				res := httptest.NewRecorder()
				router.ServeHTTP(res, req)
				if res.Code != status {
					t.Fatalf("status=%d, want %d: %s", res.Code, status, res.Body)
				}
			})
		}
	}
}

func TestPrivilegedRunnerAccess(t *testing.T) {
	for _, scope := range []identity.Scope{identity.OpsScope, identity.AdminScope} {
		t.Run(string(scope), func(t *testing.T) {
			listed := 0
			statusReads := 0
			api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodGet && r.URL.Path == "/apis/ebs/v1/runners" {
					listed++
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"items":[]}`))}, nil
				}
				if r.Method == http.MethodGet && r.URL.Path == "/apis/ebs/v1/runners/runner-1/status" {
					statusReads++
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"metadata":{"name":"runner-1"},"status":{"phase":"Online"}}`))}, nil
				}
				if r.Method != http.MethodGet || r.URL.Path != "/apis/iam.ebs/v1/users/runner-1" {
					t.Fatalf("denied request reached upstream: %s %s", r.Method, r.URL)
				}
				body := `{"metadata":{"name":"runner-1"},"spec":{"enabled":true,"scopes":["` + string(scope) + `"]}}`
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			token, err := api.tokens.Issue("runner-1", identity.UserType, scope, time.Hour, api.now())
			if err != nil {
				t.Fatal(err)
			}
			router := api.Router()
			for _, tc := range []struct {
				method, suffix string
				status         int
				body           string
			}{
				{http.MethodGet, "", http.StatusOK, `{"items":[]}`},
				{http.MethodGet, "/runner-1/status", http.StatusOK, `{"metadata":{"name":"runner-1"},"status":{"phase":"Online"}}`},
				{http.MethodPost, "", http.StatusForbidden, `{"error":"identity is not allowed for this route"}`},
				{http.MethodGet, "/runner-1/jobs", http.StatusForbidden, `{"error":"identity is not allowed for this route"}`},
				{http.MethodGet, "/runner-1/jobs?watch=true", http.StatusForbidden, `{"error":"identity is not allowed for this route"}`},
			} {
				req := httptest.NewRequest(tc.method, "/apis/ebs/v1/runners"+tc.suffix, strings.NewReader(`{"metadata":{"name":"runner-1"}}`))
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Content-Type", "application/json")
				res := httptest.NewRecorder()
				router.ServeHTTP(res, req)
				if res.Code != tc.status || res.Body.String() != tc.body {
					t.Fatalf("%s: status=%d body=%s", tc.method, res.Code, res.Body)
				}
			}
			if listed != 1 {
				t.Fatalf("list requests=%d, want 1", listed)
			}
			if statusReads != 1 {
				t.Fatalf("status requests=%d, want 1", statusReads)
			}
		})
	}
}

func TestRunnerWriteChecksBeforeForwarding(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body string
		status                   int
	}{
		{"register", "POST", "", `{"metadata":{"name":"runner-1"}}`, http.StatusOK},
		{"registration identity mismatch", "POST", "", `{"metadata":{"name":"other"}}`, http.StatusForbidden},
		{"invalid registration", "POST", "", `{`, http.StatusBadRequest},
		{"update", "PUT", "/runner-1", `{"metadata":{"name":"runner-1","resourceVersion":"1"},"spec":{"arch":"aarch64"}}`, http.StatusOK},
		{"patch", "PATCH", "/runner-1", `{"spec":{"arch":"aarch64"}}`, http.StatusOK},
		{"protected field", "PATCH", "/runner-1", `{"spec":{"taints":[]}}`, http.StatusForbidden},
		{"invalid patch", "PATCH", "/runner-1", `{`, http.StatusBadRequest},
		{"missing Runner", "PUT", "/missing", `{}`, http.StatusNotFound},
		{"delete", "DELETE", "/runner-1", `{}`, http.StatusOK},
		{"delete exceeds Gateway body limit", "DELETE", "/runner-1", strings.Repeat(" ", 257), http.StatusOK},
		{"delete rejected by apiserver", "DELETE", "/runner-1", `{}`, http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := `{"metadata":{"name":"runner-1","resourceVersion":"1"},"spec":{}}`
				status := http.StatusOK
				if r.URL.Path == "/apis/iam.ebs/v1/users/admin" {
					body = `{"metadata":{"name":"admin"},"spec":{"enabled":true,"scopes":["ebs:admin"]}}`
				} else if r.Method != http.MethodGet {
					writes++
					if tc.method == http.MethodDelete {
						raw, err := io.ReadAll(r.Body)
						if err != nil || r.Method != http.MethodDelete || string(raw) != tc.body ||
							r.Header.Get("Content-Type") != "application/json" {
							t.Fatalf("unexpected delete: method=%s body=%s err=%v", r.Method, raw, err)
						}
						status = tc.status
					}
					if tc.method == http.MethodPut || tc.method == http.MethodPatch {
						raw, err := io.ReadAll(r.Body)
						if err != nil || r.Method != http.MethodPut ||
							!strings.Contains(string(raw), `"resourceVersion":"1"`) ||
							!strings.Contains(string(raw), `"arch":"aarch64"`) {
							t.Fatalf("unexpected prepared update: method=%s body=%s err=%v", r.Method, raw, err)
						}
					}
				} else if strings.HasSuffix(r.URL.Path, "/missing") {
					status = http.StatusNotFound
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			api.bodyLimit = 256
			who := identity.Principal{Subject: "runner-1", Type: identity.RunnerType}
			if tc.method == http.MethodDelete || tc.path == "/missing" {
				who = identity.Principal{Subject: "admin", Type: identity.UserType, Scope: identity.AdminScope}
			}
			token, err := api.tokens.Issue(who.Subject, who.Type, who.Scope, time.Hour, api.now())
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(tc.method, "/apis/ebs/v1/runners"+tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			if tc.method == http.MethodPatch {
				req.Header.Set("Content-Type", "application/merge-patch+json")
			}
			res := httptest.NewRecorder()
			api.Router().ServeHTTP(res, req)
			wantWrites := 0
			if tc.status == http.StatusOK || tc.method == http.MethodDelete {
				wantWrites = 1
			}
			if res.Code != tc.status || writes != wantWrites {
				t.Fatalf("status=%d writes=%d, want %d/%d: %s", res.Code, writes, tc.status, wantWrites, res.Body)
			}
		})
	}
}
