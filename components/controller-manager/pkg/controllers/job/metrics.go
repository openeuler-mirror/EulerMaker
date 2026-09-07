package job

import "controller-manager/pkg/metrics"

var (
	runnerLostObservations = metrics.NewCounter("job_controller_runner_lost_observations", "Number of new Runner-unavailable grace-period observations.")
	runnerLostFailures     = metrics.NewCounter("job_controller_runner_lost_failures_total", "Number of Jobs marked Failed after their Runner remained unavailable.")
	statusUpdateConflicts  = metrics.NewCounter("job_controller_status_update_conflicts_total", "Number of Job status update conflicts.")
	statusUpdateUnknown    = metrics.NewCounter("job_controller_status_update_unknown_total", "Number of Job status updates with an unknown result.")
	historyGCScheduled     = metrics.NewCounter("job_controller_history_gc_scheduled", "Number of delayed terminal Job history checks scheduled.")
	historyDeleted         = metrics.NewCounter("job_controller_history_deleted_total", "Number of expired terminal Jobs deleted.")
	historyMissingEndTime  = metrics.NewCounter("job_controller_history_missing_end_time_total", "Number of terminal Jobs observed without an end time.")
	historyDeleteUnknown   = metrics.NewCounter("job_controller_history_delete_unknown_total", "Number of Job deletes with an unknown result.")
)
