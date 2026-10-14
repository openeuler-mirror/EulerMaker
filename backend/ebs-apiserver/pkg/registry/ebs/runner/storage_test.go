package runner

import (
	"context"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ebsv1 "ebs-api/ebs/v1"
)

func TestPrepareForCreateDefaultsOffline(t *testing.T) {
	runner := &ebsv1.Runner{Status: ebsv1.RunnerStatus{Phase: ebsv1.RunnerOnline}}

	(&strategy{}).PrepareForCreate(context.Background(), runner)

	if runner.Status.Phase != ebsv1.RunnerOffline {
		t.Fatalf("status.phase = %q, want Offline", runner.Status.Phase)
	}
}

func TestOrdinaryUpdatePreservesRunnerStatus(t *testing.T) {
	old := &ebsv1.Runner{Status: ebsv1.RunnerStatus{
		Phase: ebsv1.RunnerEvicted, Heartbeat: metav1.NewTime(time.Unix(100, 0)),
	}}
	for _, test := range []struct {
		name   string
		status ebsv1.RunnerStatus
	}{
		{name: "omitted status"},
		{name: "explicit status", status: ebsv1.RunnerStatus{Phase: ebsv1.RunnerOnline}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := &ebsv1.Runner{Status: test.status}
			candidate.Spec.Unschedulable = true
			(&strategy{}).PrepareForUpdate(context.Background(), candidate, old)
			if !reflect.DeepEqual(candidate.Status, old.Status) || !candidate.Spec.Unschedulable {
				t.Fatalf("ordinary update did not preserve status and apply spec: %+v", candidate)
			}
		})
	}
}

func TestStatusUpdateKeepsRequestedRunnerPhase(t *testing.T) {
	for _, phase := range []ebsv1.RunnerPhase{ebsv1.RunnerOnline, ebsv1.RunnerEvicted, ebsv1.RunnerOffline} {
		t.Run(string(phase), func(t *testing.T) {
			old := &ebsv1.Runner{}
			old.Spec.Unschedulable = true
			wanted := ebsv1.RunnerStatus{Phase: phase, Heartbeat: metav1.NewTime(time.Unix(200, 0))}
			candidate := &ebsv1.Runner{Status: wanted}
			s := &statusStrategy{}
			s.PrepareForUpdate(context.Background(), candidate, old)
			if !reflect.DeepEqual(candidate.Status, wanted) || !reflect.DeepEqual(candidate.Spec, old.Spec) {
				t.Fatalf("status update did not apply status and preserve spec: %+v", candidate)
			}
			if errs := s.ValidateUpdate(context.Background(), candidate, old); len(errs) != 0 {
				t.Fatalf("status update failed: %v", errs)
			}
		})
	}
}
