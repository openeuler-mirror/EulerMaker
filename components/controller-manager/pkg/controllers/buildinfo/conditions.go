// BuildInfo, spec, and install conditions share these helpers. Kubernetes
// metadata helpers manage lastTransitionTime.
package buildinfo

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ebsv1 "ebs-api/ebs/v1"
)

// Spec build/install status values persisted in SpecStatus.
const (
	// SpecBuildRunning: a Job has been dispatched and has not terminated.
	SpecBuildRunning = "Running"
	// SpecBuildSucceeded: terminal, the latest-generation Job succeeded.
	SpecBuildSucceeded = "Succeeded"
	// SpecBuildFailed: terminal, the latest-generation Job failed, or a
	// pre-dispatch verdict failed the spec.
	SpecBuildFailed = "Failed"
	// SpecBuildArchUnsupported: terminal, the spec cannot build on the target architecture.
	SpecBuildArchUnsupported = "ArchUnsupported"
)

// BuildInfo-level condition types.
const (
	// ConditionSpecDependsFillFailed carries spec parsing degradation and
	// single-build empty-set closeouts.
	ConditionSpecDependsFillFailed = "SpecDependsFillFailed"
	// ConditionSpecCommitMissing records package-repo entries skipped for a
	// non-retryable resolution failure.
	ConditionSpecCommitMissing = "SpecCommitMissing"
	// ConditionDcgBuildFailed records a DCG build/break failure; removed as
	// soon as the DCG is obtained again.
	ConditionDcgBuildFailed = "DcgBuildFailed"
	// ConditionReleaseUnavailable is a persisted stop-dispatch marker.
	ConditionReleaseUnavailable = "ReleaseUnavailable"
	// ConditionRpmRepoRetrying records a recoverable repository error.
	ConditionRpmRepoRetrying = "RpmRepoRetrying"
	// ConditionRpmRepoUnavailable is a persisted stop-dispatch marker.
	ConditionRpmRepoUnavailable = "RpmRepoUnavailable"
	// ConditionSnapshotUnavailable is a persisted stop-dispatch marker.
	ConditionSnapshotUnavailable = "SnapshotUnavailable"
)

// BuildInfo-level condition reasons.
const (
	ReasonSpecParseFailed             = "SpecParseFailed"
	ReasonSpecifiedBuildSetEmpty      = "SpecifiedBuildSetEmpty"
	ReasonSpecCommitMissing           = "SpecCommitMissing"
	ReasonDcgBuildFailed              = "DcgBuildFailed"
	ReasonRpmRepoReleaseFailed        = "RpmRepoReleaseFailed"
	ReasonRpmRepoNotFound             = "RpmRepoNotFound"
	ReasonRpmRepoQueryFailed          = "RpmRepoQueryFailed"
	ReasonRpmRepoQueryRejected        = "RpmRepoQueryRejected"
	ReasonRpmRepoConfigInvalid        = "RpmRepoConfigInvalid"
	ReasonRpmRepoXMLDownloadFailed    = "RpmRepoXmlDownloadFailed"
	ReasonRpmRepoXMLParseFailed       = "RpmRepoXmlParseFailed"
	ReasonBootstrapRepoXMLUnavail     = "BootstrapRepoXmlUnavailable"
	ReasonBootstrapRepoXMLParseFailed = "BootstrapRepoXmlParseFailed"
	ReasonBootstrapRepoConfigInvalid  = "BootstrapRepoConfigInvalid"
	ReasonSnapshotNotFound            = "SnapshotNotFound"
	ReasonSnapshotQueryFailed         = "SnapshotQueryFailed"
)

// Spec-level condition types and reasons.
const (
	ConditionBuildFailed                        = "BuildFailed"
	ReasonJobFailed                             = "JobFailed"
	ConditionRebuildFailed                      = "RebuildFailed"
	ReasonRebuildJobFailed                      = "RebuildJobFailed"
	ReasonRpmDependsMissing                     = "RpmDependsMissing"
	ConditionDefaultBuildResourceConfigNotFound = "DefaultBuildResourceConfigNotFound"
	ReasonDefaultBuildResourceConfigNotFound    = "DefaultBuildResourceConfigNotFound"
	ConditionBuildAborted                       = "BuildAborted"
	ReasonBuildAborted                          = "BuildAborted"
	ConditionJobAbortFailed                     = "JobAbortFailed"
	ReasonJobAbortFailed                        = "JobAbortFailed"
	ConditionJobCreateRejected                  = "JobCreateRejected"
	ReasonJobCreateRejected                     = "JobCreateRejected"
	ConditionInstall                            = "Install"
	ReasonInstallCheckFailed                    = "InstallCheckFailed"
	ReasonInstallResultMissing                  = "InstallResultMissing"
	ReasonInstallResultInvalid                  = "InstallResultInvalid"
)

func failedSpecBuildStatus(status string) bool {
	return status == SpecBuildFailed || status == SpecBuildArchUnsupported
}

func terminalSpecBuildStatus(status string) bool {
	return status == SpecBuildSucceeded || failedSpecBuildStatus(status)
}

// conditionMessageMax bounds a persisted condition message (apiserver limit).
const conditionMessageMax = 1024

// stopConditionTypes persist across rounds and route directly to convergence.
var stopConditionTypes = []string{
	ConditionReleaseUnavailable,
	ConditionRpmRepoUnavailable,
	ConditionSnapshotUnavailable,
}

// upsertCondition sets a status=True condition, preserving the original
// lastTransitionTime when nothing changed (meta.SetStatusCondition).
func upsertCondition(conditions *[]metav1.Condition, condType, reason, message string) {
	apiMeta.SetStatusCondition(conditions, metav1.Condition{
		Type:    condType,
		Status:  metav1.ConditionTrue,
		Reason:  reason,
		Message: sanitizeMessage(message),
	})
}

// removeCondition drops a condition type (meta.RemoveStatusCondition).
func removeCondition(conditions *[]metav1.Condition, condType string) {
	apiMeta.RemoveStatusCondition(conditions, condType)
}

// findCondition returns the condition with the given type, or nil.
func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	return apiMeta.FindStatusCondition(conditions, condType)
}

// stopCondition returns the first persisted stop-dispatch marker, or nil.
func stopCondition(conditions []metav1.Condition) *metav1.Condition {
	for _, condType := range stopConditionTypes {
		if cond := findCondition(conditions, condType); cond != nil && cond.Status == metav1.ConditionTrue {
			return cond
		}
	}
	return nil
}

// missingDepsMessage renders the RpmDependsMissing message: dependency names
// sorted, de-duplicated and comma-joined; over 1024 characters the list is
// truncated and suffixed with `...(+N deps total)`.
func missingDepsMessage(names []string) string {
	if len(names) == 0 {
		return ""
	}
	sorted := make([]string, 0, len(names))
	seen := map[string]struct{}{}
	for _, name := range names {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	message := strings.Join(sorted, ",")
	if len(message) <= conditionMessageMax {
		return message
	}
	suffix := fmt.Sprintf("...(+%d deps total)", len(sorted))
	keep := conditionMessageMax - len(suffix)
	if keep < 0 {
		keep = 0
	}
	// Walk back to the last comma so no partial name survives.
	cut := strings.LastIndex(message[:keep], ",")
	if cut > 0 {
		keep = cut
	} else {
		// No comma in the kept prefix (a single oversized dep name): back
		// off to a rune boundary so the byte cut never splits a multi-byte
		// UTF-8 sequence (the result is exactly conditionMessageMax bytes,
		// so sanitizeMessage's own guard would not repair it).
		for keep > 0 && !utf8.RuneStart(message[keep]) {
			keep--
		}
	}
	return message[:keep] + suffix
}

// sanitizeMessage bounds any condition message to the apiserver limit.
func sanitizeMessage(message string) string {
	if len(message) <= conditionMessageMax {
		return message
	}
	// Back off to a rune boundary so the byte cut never splits a multi-byte
	// UTF-8 sequence (a split would corrupt the tail with U+FFFD on marshal).
	cut := conditionMessageMax
	for cut > 0 && !utf8.RuneStart(message[cut]) {
		cut--
	}
	return message[:cut]
}

// specCondition writes a spec-level condition into the given SpecStatus.
func specCondition(spec *ebsv1.SpecStatus, condType, reason, message string) {
	upsertCondition(&spec.Build.Conditions, condType, reason, message)
}

// installCondition writes the install-level condition.
func installCondition(spec *ebsv1.SpecStatus, jobName string) {
	upsertCondition(&spec.Install.Conditions, ConditionInstall, ReasonInstallCheckFailed, "job "+jobName+" install check failed")
}
