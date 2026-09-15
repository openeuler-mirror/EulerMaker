package snapshot

import (
	"log"
	"reflect"
	"sort"

	"controller-manager/pkg/metrics"
	ebsv1 "ebs-api/ebs/v1"
)

var (
	phaseTransitions    = metrics.NewCounter("snapshot_controller_phase_transitions_total", "Confirmed Snapshot phase transitions.")
	conditionsChanged   = metrics.NewCounter("snapshot_controller_conditions_total", "Confirmed Snapshot condition changes.")
	conflictRequeues    = metrics.NewCounter("snapshot_controller_conflict_requeues_total", "Snapshot write conflict requeues.")
	unknownWrites       = metrics.NewCounter("snapshot_controller_unknown_writes_total", "Snapshot writes requiring confirmation.")
	resolveDuration     = metrics.NewCounter("snapshot_controller_resolve_duration_nanoseconds_total", "Total package resolution batch duration in nanoseconds.")
	resolveBatches      = metrics.NewCounter("snapshot_controller_resolve_batches_total", "Package resolution batches.")
	resolvedPackages    = metrics.NewCounter("snapshot_controller_resolved_total", "Resolved package results before status persistence.")
	waitingPackages     = metrics.NewCounter("snapshot_controller_waiting_total", "Normally waiting package results.")
	failedPackages      = metrics.NewCounter("snapshot_controller_failed_total", "Retryable package results before status persistence.")
	skippedPackages     = metrics.NewCounter("snapshot_controller_skipped_total", "Confirmed non-retryable package errors.")
	exhaustedPackages   = metrics.NewCounter("snapshot_controller_retry_exhausted_total", "Confirmed exhausted package retry budgets.")
	unexpectedGitErrors = metrics.NewCounter("snapshot_controller_unexpected_git_server_errors_total", "Unclassified git-server errors.")
	authFailures        = metrics.NewCounter("snapshot_controller_auth_failures_total", "API authentication or authorization failures.")
)

// Only log confirmed changes, not identical outcomes on every periodic resync.
// Error codes are safe to log; URLs and remote error text may contain credentials.
func (c *Controller) recordStatus(before, after *ebsv1.Snapshot) {
	if before.Status.Phase != after.Status.Phase {
		phaseTransitions.Inc()
		log.Printf("controller=snapshot key=%q snapshot_uid=%q from=%q to=%q resourceVersion=%q retries=%d reason=PhaseChanged", after.Namespace+"/"+after.Name, after.UID, before.Status.Phase, after.Status.Phase, before.ResourceVersion, c.Queue().NumRequeues(after.Namespace+"/"+after.Name))
	}
	if !reflect.DeepEqual(normalizeConditions(before.Status.Conditions), normalizeConditions(after.Status.Conditions)) {
		conditionsChanged.Inc()
	}
	names := make([]string, 0, len(after.Status.PackageRepoStatuses))
	for name := range after.Status.PackageRepoStatuses {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		status := after.Status.PackageRepoStatuses[name]
		if reflect.DeepEqual(before.Status.PackageRepoStatuses[name], status) {
			continue
		}
		reason := "PackageResolved"
		if status.Error != nil {
			reason = string(status.Error.Code)
			if !status.Error.Retryable {
				skippedPackages.Inc()
			}
			if status.Error.Code == ebsv1.SpecCommitRetryExhausted {
				exhaustedPackages.Inc()
			}
		}
		log.Printf("controller=snapshot key=%q snapshot_uid=%q package_name=%q phase=%q resourceVersion=%q reason=%q", after.Namespace+"/"+after.Name, after.UID, name, after.Status.Phase, before.ResourceVersion, reason)
	}
}
