package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ebs-gateway/internal/identity"
)

func TestJobStatusPatchForwarding(t *testing.T) {
	const path = "/apis/ebs/v1/projects/team/jobs/job-1/status"
	const old = `{"apiVersion":"ebs/v1","kind":"Job","metadata":{"name":"job-1","namespace":"team","resourceVersion":"7"},"spec":{},"status":{"runner":"runner-1","phase":"Running"}}`
	reads, writes := 0, 0
	api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != path {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			reads++
			if r.URL.RawQuery != "" {
				t.Fatalf("status update lookup must read the full object: %s", r.URL)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(old))}, nil
		case http.MethodPut:
			writes++
			var object struct {
				Kind     string
				Metadata struct{ Name, Namespace, ResourceVersion string }
				Status   struct{ Runner, Phase string }
			}
			if err := json.NewDecoder(r.Body).Decode(&object); err != nil {
				t.Fatal(err)
			}
			if object.Kind != "Job" || object.Metadata.Name != "job-1" || object.Metadata.Namespace != "team" || object.Metadata.ResourceVersion != "7" || object.Status.Runner != "runner-1" || object.Status.Phase != "Succeeded" || r.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("invalid merged object: %+v", object)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		default:
			t.Fatalf("unexpected method: %s", r.Method)
			return nil, nil
		}
	}))
	token, err := api.tokens.Issue("runner-1", identity.RunnerType, "", time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(`{"metadata":{"resourceVersion":"7"},"status":{"phase":"Succeeded"}}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/merge-patch+json")
	response := httptest.NewRecorder()
	router := api.Router()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || reads != 1 || writes != 1 {
		t.Fatalf("status=%d reads=%d writes=%d body=%s", response.Code, reads, writes, response.Body)
	}
}

func TestRunnerJobReadProjectsAssignment(t *testing.T) {
	const path = "/apis/ebs/v1/projects/team/jobs/job-1"
	const query = "includeFields=status.phase&excludeFields=spec"
	const body = `{"status":{"phase":"Running"}}`
	reads := 0
	api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		reads++
		if r.Method != http.MethodGet || r.URL.Path != path {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
		}
		response := body
		if reads == 1 {
			if r.URL.Query().Get("includeFields") != "metadata.name,metadata.namespace,status.runner" || len(r.URL.Query()) != 1 {
				t.Fatalf("unexpected assignment projection: %s", r.URL)
			}
			response = `{"metadata":{"name":"job-1","namespace":"team"},"status":{"runner":"runner-1"}}`
		} else if r.URL.RawQuery != query {
			t.Fatalf("caller projection changed: %s", r.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response))}, nil
	}))
	token, err := api.tokens.Issue("runner-1", identity.RunnerType, "", time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, path+"?"+query, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	api.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK || reads != 2 || response.Body.String() != body {
		t.Fatalf("status=%d reads=%d body=%s", response.Code, reads, response.Body)
	}
}

func TestJobAbortForwarding(t *testing.T) {
	for _, tc := range []struct {
		name, subject, body string
		upstreamStatus      int
		want                int
	}{
		{"owner", "alice", `{"uid":"id","reason":"stop"}`, 200, 200},
		{"member", "carol", `{"uid":"id"}`, 200, 200},
		{"upstream body rejection", "alice", strings.Repeat("x", 17<<10), 400, 400},
		{"nonmember", "bob", `{"uid":"id"}`, 200, 403},
		{"admin nonmember", "admin", `{"uid":"id"}`, 200, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := identity.UserScope
			if tc.subject == "admin" {
				scope = identity.AdminScope
			}
			forwarded := false
			api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				status, body := http.StatusOK, "{}"
				switch r.URL.Path {
				case "/apis/iam.ebs/v1/users/" + tc.subject:
					body = `{"metadata":{"name":"` + tc.subject + `"},"spec":{"enabled":true,"scopes":["` + string(scope) + `"]}}`
				case "/apis/ebs/v1/projects/team":
					body = `{"metadata":{"name":"team","labels":{"ebs.io/owner-user":"alice","ebs.io/member-user.carol":"true"}}}`
				case "/apis/ebs/v1/projects/team/jobs/job-1/abort":
					forwarded = true
					raw, err := io.ReadAll(r.Body)
					if err != nil || string(raw) != tc.body || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
						t.Fatalf("request changed: method=%s body=%s err=%v", r.Method, raw, err)
					}
					status = tc.upstreamStatus
				default:
					t.Fatalf("unexpected upstream request: %s %s", r.Method, r.URL)
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			api.bodyLimit = 8
			token, err := api.tokens.Issue(tc.subject, identity.UserType, scope, time.Hour, api.now())
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/apis/ebs/v1/projects/team/jobs/job-1/abort", strings.NewReader(tc.body))
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			api.Router().ServeHTTP(response, request)
			if response.Code != tc.want || forwarded != (tc.want != http.StatusForbidden) {
				t.Fatalf("status=%d forwarded=%v body=%s", response.Code, forwarded, response.Body)
			}
		})
	}
}

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
			calls := 0
			api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodGet || r.URL.Path != "/apis/ebs/v1/projects/team/jobs/job-1" {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
				}
				if tc.status == 0 {
					return nil, errors.New("private connection details")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}))
			token, err := api.tokens.Issue("runner-1", identity.RunnerType, "", time.Hour, api.now())
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/apis/ebs/v1/projects/team/jobs/job-1", nil)
			request.Header.Set("Authorization", "Bearer "+token)
			router := api.Router()
			router.ServeHTTP(recorder, request)
			wantCalls := 1
			if tc.want == http.StatusOK {
				wantCalls = 2
			}
			if recorder.Code != tc.want || calls != wantCalls {
				t.Fatalf("status=%d calls=%d, want %d/%d", recorder.Code, calls, tc.want, wantCalls)
			}
			if strings.Contains(recorder.Body.String(), "private") {
				t.Fatal("upstream details exposed")
			}
		})
	}
}
