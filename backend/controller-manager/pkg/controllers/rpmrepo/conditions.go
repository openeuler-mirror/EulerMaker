package rpmrepo

import (
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MergeCondition merges one condition of condType into conditions. It never reads a clock: now comes from the
// single clock.Now() of the round and is only used when the condition is inserted or its status changes.
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

// conditionMatches reports whether the named condition already carries the wanted status and reason.
func conditionMatches(conditions []metav1.Condition, condType string, status metav1.ConditionStatus, reason string) bool {
	for _, item := range conditions {
		if item.Type != condType {
			continue
		}
		return item.Status == status && item.Reason == reason
	}
	return false
}
