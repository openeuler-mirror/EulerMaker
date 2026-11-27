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

	"github.com/gin-gonic/gin"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/upstream"
)

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
