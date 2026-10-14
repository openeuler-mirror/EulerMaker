package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCounterHandler(t *testing.T) {
	counter := NewCounter("controller_manager_test_total", "Test counter.")
	counter.Inc()
	recorder := httptest.NewRecorder()
	Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), "controller_manager_test_total 1") {
		t.Fatalf("unexpected metrics response: code=%d body=%q", recorder.Code, recorder.Body.String())
	}
}
