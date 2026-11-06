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
