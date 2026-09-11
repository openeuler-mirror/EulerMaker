package runner

import (
	"context"
	"testing"

	ebsv1 "ebs-api/ebs/v1"
)

func TestPrepareForCreateDefaultsOffline(t *testing.T) {
	runner := &ebsv1.Runner{Status: ebsv1.RunnerStatus{Phase: ebsv1.RunnerOnline}}

	(&strategy{}).PrepareForCreate(context.Background(), runner)

	if runner.Status.Phase != ebsv1.RunnerOffline {
		t.Fatalf("status.phase = %q, want Offline", runner.Status.Phase)
	}
}
