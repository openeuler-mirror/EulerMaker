package artifact

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestArtifactRouteRejectsMalformedPaths(t *testing.T) {
	for _, path := range []string{
		"/artifacts/v1/projects/project/jobs/job/logs/status/extra",
		"/artifacts/v1//projects/project/jobs/job/logs/status",
	} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			response := httptest.NewRecorder()
			(&Server{}).ServeHTTP(response, request)
			if response.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
			}
		})
	}
}
