package artifact

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRouterResponses(t *testing.T) {
	h, _ := testHandler(t)
	tests := []struct {
		method string
		path   string
		status int
		allow  string
	}{
		{http.MethodGet, "/healthz", http.StatusOK, ""},
		{http.MethodGet, "/readyz", http.StatusOK, ""},
		{http.MethodGet, "/healthz/", http.StatusNotFound, ""},
		{http.MethodGet, "/internal/v1/repositories", http.StatusMethodNotAllowed, http.MethodPost},
		{http.MethodPut, "/artifacts/v1/projects/project/jobs/build/artifacts", http.StatusMethodNotAllowed, "GET, POST"},
		{http.MethodGet, "/artifacts/v1/projects/bad%20name/jobs/build/artifacts", http.StatusBadRequest, ""},
		{http.MethodGet, "/missing", http.StatusNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			h.ServeHTTP(response, httptest.NewRequest(tt.method, tt.path, nil))
			if response.Code != tt.status || response.Header().Get("Allow") != tt.allow {
				t.Fatalf("status = %d, Allow = %q; want %d, %q", response.Code, response.Header().Get("Allow"), tt.status, tt.allow)
			}
			if response.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("missing nosniff header")
			}
		})
	}
}
