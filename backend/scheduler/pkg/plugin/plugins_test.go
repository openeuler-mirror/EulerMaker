package plugin

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ebsv1 "ebs-api/ebs/v1"
	"scheduler/pkg/framework"
)

func TestTaintMatching(t *testing.T) {
	taint := ebsv1.RunnerTaint{Key: "dedicated", Value: "build", Effect: ebsv1.TaintEffectNoSchedule}
	for _, tc := range []struct {
		name       string
		toleration ebsv1.Toleration
	}{
		{"default operator and effect", ebsv1.Toleration{Key: "dedicated", Value: "build"}},
		{"equal", ebsv1.Toleration{Key: "dedicated", Value: "build", Operator: ebsv1.TolerationEqual, Effect: ebsv1.TaintEffectNoSchedule}},
		{"exists", ebsv1.Toleration{Key: "dedicated", Operator: ebsv1.TolerationExists}},
		{"all keys", ebsv1.Toleration{Operator: ebsv1.TolerationExists}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := []ebsv1.Toleration{tc.toleration}
			if err := validateTolerations(items); err != nil {
				t.Fatal(err)
			}
			if !tolerated(taint, items) {
				t.Fatal("matching toleration was ignored")
			}
		})
	}
	for _, effect := range []ebsv1.TaintEffect{ebsv1.TaintEffectNoSchedule, ebsv1.TaintEffectNoExecute, ebsv1.TaintEffectPreferNoSchedule} {
		taint.Effect = effect
		runner := &framework.RunnerSnapshot{Runner: &ebsv1.Runner{Spec: ebsv1.RunnerSpec{Taints: []ebsv1.RunnerTaint{taint}}}}
		session := &framework.Session{Job: &ebsv1.Job{}}
		want := framework.Unschedulable
		if effect == ebsv1.TaintEffectPreferNoSchedule {
			want = framework.Success
		}
		if got := Taint().Filter(context.Background(), session, runner); got.Code != want {
			t.Fatalf("effect %q: %+v, want %v", effect, got, want)
		}
		session.Job.Spec.Tolerations = []ebsv1.Toleration{{Operator: ebsv1.TolerationExists, Effect: effect}}
		if got := Taint().Filter(context.Background(), session, runner); got.Code != framework.Success {
			t.Fatalf("tolerated effect %q: %+v", effect, got)
		}
	}
}

func TestExecutionTypeMatching(t *testing.T) {
	values := []ebsv1.ExecutionType{"", ebsv1.ExecutionTypeContainer, ebsv1.ExecutionTypeVM, ebsv1.ExecutionTypeHardware}
	for _, jobType := range values {
		for _, runnerType := range values {
			session := &framework.Session{Job: &ebsv1.Job{Spec: ebsv1.JobSpec{Runtime: jobType}}}
			runner := &framework.RunnerSnapshot{Runner: &ebsv1.Runner{Spec: ebsv1.RunnerSpec{Type: runnerType}}}
			effectiveJob, effectiveRunner := jobType, runnerType
			if effectiveJob == "" {
				effectiveJob = ebsv1.ExecutionTypeContainer
			}
			if effectiveRunner == "" {
				effectiveRunner = ebsv1.ExecutionTypeContainer
			}
			want := framework.Unschedulable
			if effectiveJob == effectiveRunner {
				want = framework.Success
			}
			if got := Runtime().Filter(context.Background(), session, runner); got.Code != want {
				t.Fatalf("job=%q runner=%q: %+v, want %v", jobType, runnerType, got, want)
			}
		}
	}
}

func TestFiltersAndLeastAllocated(t *testing.T) {
	requests, _ := framework.ParseRequests(ebsv1.ResourceRequirements{
		Requests: map[string]string{"cpu": "1", "memory": "1Gi"},
	})
	alloc, _ := framework.ParseAllocatable(map[string]string{"cpu": "4", "memory": "4Gi"})
	runner := &framework.RunnerSnapshot{
		Runner: &ebsv1.Runner{
			ObjectMeta: metav1.ObjectMeta{Name: "r", Labels: map[string]string{"arch": "x86_64"}},
			Spec:       ebsv1.RunnerSpec{Type: "ct"},
			Status:     ebsv1.RunnerStatus{Phase: ebsv1.RunnerOnline},
		},
		Allocatable: alloc,
		Available:   alloc,
	}
	session := &framework.Session{
		Job: &ebsv1.Job{
			Spec: ebsv1.JobSpec{Runtime: "ct", NodeSelector: map[string]string{"arch": "x86_64"}},
		},
		Requests: requests,
	}
	for _, f := range DefaultFilters() {
		if status := f.Filter(context.Background(), session, runner); status.Code != framework.Success {
			t.Fatalf("%s: %+v", f.Name(), status)
		}
	}
	score, status := LeastAllocated().Score(context.Background(), session, runner)
	if status.Code != framework.Success || score != 75 {
		t.Fatalf("score=%d status=%+v", score, status)
	}
}

func TestPhaseFilterOnlyAcceptsOnline(t *testing.T) {
	filter := Phase()
	for _, phase := range []ebsv1.RunnerPhase{
		ebsv1.RunnerOnline, ebsv1.RunnerOffline, ebsv1.RunnerEvicted, "Idle", "Running", "",
	} {
		t.Run(string(phase), func(t *testing.T) {
			runner := &framework.RunnerSnapshot{Runner: &ebsv1.Runner{Status: ebsv1.RunnerStatus{Phase: phase}}}
			status := filter.Filter(context.Background(), &framework.Session{}, runner)
			if phase == ebsv1.RunnerOnline && status.Code != framework.Success {
				t.Fatalf("Online Runner rejected: %+v", status)
			}
			if phase != ebsv1.RunnerOnline && status.Code != framework.Unschedulable {
				t.Fatalf("phase %q returned %v, want Unschedulable", phase, status.Code)
			}
		})
	}
}
