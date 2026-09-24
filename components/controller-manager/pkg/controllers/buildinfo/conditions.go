// conditions.go centralizes the condition catalog (design 9.1) and the
// upsert/remove helpers. BuildInfo-level, spec-level and install-level
// conditions are written with status=True via meta.SetStatusCondition and
// removed via meta.RemoveStatusCondition; lastTransitionTime is managed by
// the meta helpers, never by business code (9.2).
package buildinfo

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	ebsv1 "ebs-api/ebs/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Spec build/install status values persisted in SpecStatus (design 6.2/6.3).
const (
	// SpecBuildRunning: a Job has been dispatched and has not terminated.
	SpecBuildRunning = "Running"
	// SpecBuildSucceeded: terminal, the latest-generation Job succeeded.
	SpecBuildSucceeded = "Succeeded"
	// SpecBuildFailed: terminal, the latest-generation Job failed, or a
	// pre-dispatch verdict failed the spec (E-17/E-18/E-19/E-27).
	SpecBuildFailed = "Failed"
	// SpecBuildAborted: legacy residual value (pre-v1 Job phase passthrough).
	// v1 never writes it; reading it stops the round and waits for
	// parentAbortGuard (6.4).
	SpecBuildAborted = "Aborted"
)

// BuildInfo-level condition types (design 9.1).
const (
	// ConditionSpecDependsFillFailed carries spec parsing degradation and
	// single-build empty-set closeouts.
	ConditionSpecDependsFillFailed = "SpecDependsFillFailed"
	// ConditionSpecCommitMissing records package-repo entries skipped for a
	// non-retryable resolution failure (E-24 degraded path).
	ConditionSpecCommitMissing = "SpecCommitMissing"
	// ConditionDcgBuildFailed records a DCG build/break failure; removed as
	// soon as the DCG is obtained again (9.1).
	ConditionDcgBuildFailed = "DcgBuildFailed"
	// ConditionPartialFailure marks Completed with at least one Failed spec.
	ConditionPartialFailure = "PartialFailure"
	// ConditionAllSpecsSucceeded marks Completed with every spec Succeeded.
	ConditionAllSpecsSucceeded = "AllSpecsSucceeded"
	// ConditionReleaseFailed is the persisted stop-dispatch marker (E-28).
	ConditionReleaseFailed = "ReleaseFailed"
	// ConditionRpmRepoUnavailable is the persisted stop-dispatch marker (E-29).
	ConditionRpmRepoUnavailable = "RpmRepoUnavailable"
	// ConditionSnapshotUnavailable is the persisted stop-dispatch marker (E-30).
	ConditionSnapshotUnavailable = "SnapshotUnavailable"
)

// BuildInfo-level condition reasons (design 9.1/E-23/E-24/E-28/E-29/E-30).
const (
	ReasonSpecParseFailed          = "SpecParseFailed"
	ReasonSpecifiedBuildSetEmpty   = "SpecifiedBuildSetEmpty"
	ReasonSpecCommitMissing        = "SpecCommitMissing"
	ReasonDcgBuildFailed           = "DcgBuildFailed"
	ReasonPartialFailure           = "PartialFailure"
	ReasonAllSpecsSucceeded        = "AllSpecsSucceeded"
	ReasonRpmRepoReleaseFailed     = "RpmRepoReleaseFailed"
	ReasonRpmRepoNotFound          = "RpmRepoNotFound"
	ReasonRpmRepoQueryFailed       = "RpmRepoQueryFailed"
	ReasonRpmRepoXMLDownloadFailed = "RpmRepoXmlDownloadFailed"
	ReasonRpmRepoXMLParseFailed    = "RpmRepoXmlParseFailed"
	ReasonBootstrapRepoXMLUnavail  = "BootstrapRepoXmlUnavailable"
	ReasonSnapshotNotFound         = "SnapshotNotFound"
	ReasonSnapshotQueryFailed      = "SnapshotQueryFailed"
)

// Spec-level condition types and reasons (design 9.1).
const (
	ConditionBuildFailed                  = "BuildFailed"
	ReasonJobFailed                       = "JobFailed"
	ConditionRebuildFailed                = "RebuildFailed"
	ReasonRebuildJobFailed                = "RebuildJobFailed"
	ReasonRpmDependsMissing               = "RpmDependsMissing"
	ConditionArchUnsupported              = "ArchUnsupported"
	ReasonArchUnsupported                 = "ArchUnsupported"
	ConditionDefaultBuildResourceConfigNotFound = "DefaultBuildResourceConfigNotFound"
	ReasonDefaultBuildResourceConfigNotFound    = "DefaultBuildResourceConfigNotFound"
	ConditionBuildAborted                 = "BuildAborted"
	ReasonBuildAborted                    = "BuildAborted"
	ConditionInstall                      = "Install"
	ReasonInstallCheckFailed              = "InstallCheckFailed"
)

// conditionMessageMax bounds a persisted condition message (apiserver limit).
const conditionMessageMax = 1024

// stopConditionTypes are the persisted stop-dispatch markers (design 6.5):
// once written they never recover, and any of them routes the round to the
// 6.5 convergence path.
var stopConditionTypes = []string{
	ConditionReleaseFailed,
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
// truncated and suffixed with `...(+N deps total)` (design 9.1).
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

// installCondition writes the install-level condition (design 9.1).
func installCondition(spec *ebsv1.SpecStatus, jobName string) {
	upsertCondition(&spec.Install.Conditions, ConditionInstall, ReasonInstallCheckFailed, "job "+jobName+" install check failed")
}
