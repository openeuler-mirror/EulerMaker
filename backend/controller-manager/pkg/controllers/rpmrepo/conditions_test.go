package rpmrepo

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ebsv1 "ebs-api/ebs/v1"
)

func TestMergeConditionSemantics(t *testing.T) {
	first := metav1.NewTime(time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC))
	later := metav1.NewTime(first.Add(time.Hour))

	conditions, changed := MergeCondition(nil, ebsv1.RpmRepoConditionRepositoryReady, metav1.ConditionTrue, ebsv1.RpmRepoReasonRepositoryCreated, "", 3, first)
	if !changed || len(conditions) != 1 {
		t.Fatalf("first insert must report a change: %+v", conditions)
	}
	if !conditions[0].LastTransitionTime.Equal(&first) || conditions[0].ObservedGeneration != 3 {
		t.Fatalf("first insert must use the supplied now and generation: %+v", conditions[0])
	}

	same, changed := MergeCondition(conditions, ebsv1.RpmRepoConditionRepositoryReady, metav1.ConditionTrue, ebsv1.RpmRepoReasonRepositoryCreated, "", 3, later)
	if changed {
		t.Fatalf("an identical condition must not report a change")
	}
	if !same[0].LastTransitionTime.Equal(&first) {
		t.Fatalf("an unchanged condition must keep its transition time: %+v", same[0])
	}

	reasonOnly, changed := MergeCondition(same, ebsv1.RpmRepoConditionRepositoryReady, metav1.ConditionTrue, "OtherReason", "", 3, later)
	if !changed {
		t.Fatalf("a reason change must report a change")
	}
	if !reasonOnly[0].LastTransitionTime.Equal(&first) {
		t.Fatalf("a reason-only change must keep the transition time: %+v", reasonOnly[0])
	}

	statusChanged, changed := MergeCondition(reasonOnly, ebsv1.RpmRepoConditionRepositoryReady, metav1.ConditionFalse, ebsv1.RpmRepoReasonRepositoryCreationFailed, "", 4, later)
	if !changed || !statusChanged[0].LastTransitionTime.Equal(&later) || statusChanged[0].ObservedGeneration != 4 {
		t.Fatalf("a status change must move the transition time and generation: %+v", statusChanged[0])
	}

	sorted, _ := MergeCondition(statusChanged, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionTrue, ebsv1.RpmRepoReasonReleaseActivated, "", 4, later)
	if len(sorted) != 2 || sorted[0].Type != ebsv1.RpmRepoConditionPublishSucceed || sorted[1].Type != ebsv1.RpmRepoConditionRepositoryReady {
		t.Fatalf("conditions must be sorted by type: %+v", sorted)
	}
}

func TestBackoffDelayGrowsExponentiallyAndCaps(t *testing.T) {
	r := &reconciler{controller: &Controller{config: testConfig()}}
	r.controller.config.Backoff = Backoff{Initial: time.Second, Max: 4 * time.Second, Jitter: 0}
	for _, tc := range []struct {
		attempt int
		want    time.Duration
	}{
		{0, time.Second},
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 4 * time.Second},
		{9, 4 * time.Second},
	} {
		if got := r.backoffDelay(tc.attempt); got != tc.want {
			t.Fatalf("backoffDelay(%d) = %s, want %s", tc.attempt, got, tc.want)
		}
	}
}

func TestWindowRemainingUsesTheResponseAnchor(t *testing.T) {
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	if got := windowRemaining(time.Time{}, time.Second, now); got != 0 {
		t.Fatalf("a missing anchor must report no remaining window, got %s", got)
	}
	if got := windowRemaining(now.Add(-500*time.Millisecond), time.Second, now); got != 500*time.Millisecond {
		t.Fatalf("remaining window = %s, want 500ms", got)
	}
	if got := windowRemaining(now.Add(-2*time.Second), time.Second, now); got != 0 {
		t.Fatalf("an elapsed window must report zero, got %s", got)
	}
}
