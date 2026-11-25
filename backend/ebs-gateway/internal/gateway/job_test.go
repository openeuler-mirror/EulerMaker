package gateway

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

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
