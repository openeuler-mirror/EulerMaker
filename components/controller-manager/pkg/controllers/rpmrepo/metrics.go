package rpmrepo

import "controller-manager/pkg/metrics"

// Counters are registered once per process; pkg/metrics carries no labels, so object level dimensions
// (key, repositoryUID, Build name) stay in the structured logs.
var (
	repositoryReady       = metrics.NewCounter("rpmrepo_controller_repository_ready_total", "Confirmed repository batches promoted to the current version.")
	repositoryFailed      = metrics.NewCounter("rpmrepo_controller_repository_failed_total", "Confirmed repository batch failures collected as terminal decisions.")
	materializeRetries    = metrics.NewCounter("rpmrepo_controller_materialize_retries_total", "Repository materialization requests replayed after a retryable failure.")
	buildMissing          = metrics.NewCounter("rpmrepo_controller_build_missing_total", "RpmRepo polling keys whose owning Build is missing.")
	releaseReady          = metrics.NewCounter("rpmrepo_controller_release_ready_total", "Confirmed formal releases activated.")
	releaseFailed         = metrics.NewCounter("rpmrepo_controller_release_failed_total", "Confirmed formal release failures.")
	statusUpdateConflicts = metrics.NewCounter("rpmrepo_controller_status_update_conflicts_total", "RpmRepo status decisions abandoned or rejected by optimistic concurrency.")
	statusUpdateUnknowns  = metrics.NewCounter("rpmrepo_controller_status_update_unknown_total", "RpmRepo status writes whose outcome could not be confirmed.")
)
