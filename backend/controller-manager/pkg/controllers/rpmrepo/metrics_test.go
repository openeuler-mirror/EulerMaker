package rpmrepo

import (
	"testing"
)

func TestMetricsAreRegistered(t *testing.T) {
	for name, counter := range map[string]interface{ Value() uint64 }{
		"repository_ready":    repositoryReady,
		"repository_failed":   repositoryFailed,
		"materialize_retries": materializeRetries,
		"build_missing":       buildMissing,
		"release_ready":       releaseReady,
		"release_failed":      releaseFailed,
		"status_conflicts":    statusUpdateConflicts,
		"status_unknowns":     statusUpdateUnknowns,
	} {
		if counter == nil {
			t.Fatalf("metric %s is not registered", name)
		}
		_ = counter.Value()
	}
}
