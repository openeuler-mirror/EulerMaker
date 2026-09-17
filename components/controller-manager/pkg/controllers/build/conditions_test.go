package build

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testTime(hour int) metav1.Time {
	return metav1.NewTime(time.Date(2026, 9, 16, hour, 0, 0, 0, time.UTC))
}

func TestMergeConditionInsertsSortedWithTransitionTime(t *testing.T) {
	now := testTime(10)
	input := []metav1.Condition{{
		Type: ConditionPublishSucceed, Status: metav1.ConditionTrue,
		Reason: ReasonPublishSucceeded, Message: ReasonPublishSucceeded, LastTransitionTime: testTime(9),
	}}
	conditions, changed := MergeCondition(input, ConditionBuildSucceed, metav1.ConditionFalse, ReasonBuildFailed, ReasonBuildFailed, 4, now)
	if !changed {
		t.Fatal("insert must report a change")
	}
	if len(conditions) != 2 || conditions[0].Type != ConditionBuildSucceed || conditions[1].Type != ConditionPublishSucceed {
		t.Fatalf("conditions are not sorted by type: %+v", conditions)
	}
	if !conditions[0].LastTransitionTime.Time.Equal(now.Time) || conditions[0].ObservedGeneration != 4 {
		t.Fatalf("inserted condition = %+v", conditions[0])
	}
	if !input[0].LastTransitionTime.Time.Equal(testTime(9).Time) {
		t.Fatal("input conditions must not be mutated")
	}
}

func TestMergeConditionStatusChangeUpdatesTransitionTime(t *testing.T) {
	now := testTime(11)
	input := []metav1.Condition{{
		Type: ConditionBuildSucceed, Status: metav1.ConditionTrue,
		Reason: ReasonBuildSucceeded, Message: ReasonBuildSucceeded, LastTransitionTime: testTime(9),
	}}
	conditions, changed := MergeCondition(input, ConditionBuildSucceed, metav1.ConditionFalse, ReasonBuildFailed, ReasonBuildFailed, 1, now)
	if !changed || len(conditions) != 1 {
		t.Fatalf("changed=%v conditions=%+v", changed, conditions)
	}
	if conditions[0].Status != metav1.ConditionFalse || !conditions[0].LastTransitionTime.Time.Equal(now.Time) {
		t.Fatalf("condition = %+v", conditions[0])
	}
}

func TestMergeConditionFieldChangeKeepsTransitionTime(t *testing.T) {
	now := testTime(12)
	input := []metav1.Condition{{
		Type: ConditionBuildSucceed, Status: metav1.ConditionTrue,
		Reason: ReasonBuildSucceeded, Message: ReasonBuildSucceeded,
		ObservedGeneration: 1, LastTransitionTime: testTime(9),
	}}
	conditions, changed := MergeCondition(input, ConditionBuildSucceed, metav1.ConditionTrue, ReasonBuildSucceeded, ReasonBuildSucceeded, 7, now)
	if !changed {
		t.Fatal("observedGeneration change must report a change")
	}
	if conditions[0].ObservedGeneration != 7 {
		t.Fatalf("observedGeneration = %d", conditions[0].ObservedGeneration)
	}
	if !conditions[0].LastTransitionTime.Time.Equal(testTime(9).Time) {
		t.Fatalf("transition time must be preserved: %+v", conditions[0].LastTransitionTime)
	}
}

func TestMergeConditionNoChangeIgnoresNewClockValue(t *testing.T) {
	input := []metav1.Condition{{
		Type: ConditionPublishSucceed, Status: metav1.ConditionTrue,
		Reason: ReasonPublishSucceeded, Message: ReasonPublishSucceeded,
		ObservedGeneration: 2, LastTransitionTime: testTime(9),
	}}
	conditions, changed := MergeCondition(input, ConditionPublishSucceed, metav1.ConditionTrue, ReasonPublishSucceeded, ReasonPublishSucceeded, 2, testTime(18))
	if changed {
		t.Fatal("identical condition must not report a change")
	}
	if !conditions[0].LastTransitionTime.Time.Equal(testTime(9).Time) {
		t.Fatalf("transition time changed: %+v", conditions[0].LastTransitionTime)
	}
}
