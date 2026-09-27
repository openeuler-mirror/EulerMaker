package route

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConfigVisibilityUsesOneUpstreamResponse(t *testing.T) {
	for _, tc := range []struct {
		visibility string
		status     int
	}{
		{"Public", http.StatusOK},
		{"OpsOnly", http.StatusForbidden},
	} {
		t.Run(tc.visibility, func(t *testing.T) {
			calls := 0
			api := newTestAPI(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path != "/apis/ebs/v1/configs/example" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				body := `{"metadata":{"name":"example"},"spec":{"visibility":"` + tc.visibility + `","content":"hello"}}`
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			recorder := httptest.NewRecorder()
			api.Router().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/apis/ebs/v1/configs/example", nil))
			if calls != 1 || recorder.Code != tc.status {
				t.Fatalf("calls=%d status=%d body=%q", calls, recorder.Code, recorder.Body.String())
			}
			if tc.status == http.StatusForbidden && strings.Contains(recorder.Body.String(), "hello") {
				t.Fatal("OpsOnly content leaked")
			}
		})
	}
}
