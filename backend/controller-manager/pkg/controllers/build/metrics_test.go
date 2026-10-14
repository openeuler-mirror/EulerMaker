package build

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"controller-manager/pkg/metrics"
)

// The counters the design's observability section requires. Assertions read them from the exposition handler,
// which also proves they are registered and rendered.
const (
	metricStatusUpdateConflicts = "build_controller_status_update_conflicts_total"
	metricStatusUpdateUnknown   = "build_controller_status_update_unknown_total"
	metricEnsureConflicts       = "build_controller_ensure_conflicts_total"
	metricEnsureTerminating     = "build_controller_ensure_terminating_total"
)

func TestBuildControllerMetricsAreRegistered(t *testing.T) {
	for _, name := range []string{
		metricStatusUpdateConflicts,
		metricStatusUpdateUnknown,
		metricEnsureConflicts,
		metricEnsureTerminating,
	} {
		counterValue(t, name)
	}
}

// counterValue scrapes the process counter registry and fails when the metric is not registered.
func counterValue(t *testing.T, name string) uint64 {
	t.Helper()
	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if !strings.HasPrefix(line, name+" ") {
			continue
		}
		value, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, name+" ")), 10, 64)
		if err != nil {
			t.Fatalf("parse metric line %q: %v", line, err)
		}
		return value
	}
	t.Fatalf("metric %s is not registered", name)
	return 0
}

// assertCounterDelta asserts that the counter moved by want since before. The counters are process wide, so
// every expectation has to be expressed as a delta.
func assertCounterDelta(t *testing.T, name string, before, want uint64) {
	t.Helper()
	if after := counterValue(t, name); after != before+want {
		t.Fatalf("metric %s = %d, want %d (before=%d)", name, after, before+want, before)
	}
}
