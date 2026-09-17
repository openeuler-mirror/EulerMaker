package build

import (
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// ConditionBuildSucceed records whether every spec of the build completed successfully.
	ConditionBuildSucceed = "BuildSucceed"
	// ConditionPublishSucceed records whether the formal release finished successfully.
	ConditionPublishSucceed = "PublishSucceed"
)

const (
	ReasonBuildSucceeded       = "BuildSucceeded"
	ReasonBuildFailed          = "BuildFailed"
	ReasonPublishSucceeded     = "PublishSucceeded"
	ReasonPublishFailed        = "PublishFailed"
	ReasonChildResourceMissing = "ChildResourceMissing"
)

// MergeCondition is a pure helper that merges one condition of condType into conditions.
// It never calls the clock: now is supplied by the caller and is only used when the condition is inserted
// or when its status changes. reason and message are fixed phrases for the recorded result.
func MergeCondition(
	conditions []metav1.Condition,
	condType string,
	status metav1.ConditionStatus,
	reason, message string,
	observedGeneration int64,
	now metav1.Time,
) ([]metav1.Condition, bool) {
	result := make([]metav1.Condition, 0, len(conditions)+1)
	changed := false
	found := false
	for _, item := range conditions {
		if item.Type != condType {
			result = append(result, item)
			continue
		}
		found = true
		updated := item
		if updated.Status != status {
			updated.Status = status
			updated.LastTransitionTime = now
			changed = true
		}
		if updated.Reason != reason || updated.Message != message || updated.ObservedGeneration != observedGeneration {
			updated.Reason = reason
			updated.Message = message
			updated.ObservedGeneration = observedGeneration
			changed = true
		}
		result = append(result, updated)
	}
	if !found {
		result = append(result, metav1.Condition{
			Type:               condType,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: observedGeneration,
			LastTransitionTime: now,
		})
		changed = true
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Type < result[j].Type })
	return result, changed
}
