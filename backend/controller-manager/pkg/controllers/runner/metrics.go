package runner

import "controller-manager/pkg/metrics"

var (
	heartbeatChecks       = metrics.NewCounter("runner_controller_heartbeat_checks_total", "Number of Runner heartbeat deadline checks.")
	heartbeatTimeouts     = metrics.NewCounter("runner_controller_heartbeat_timeouts_total", "Number of authoritatively confirmed Runner heartbeat timeouts.")
	offlineUpdates        = metrics.NewCounter("runner_controller_offline_updates_total", "Number of Runner Offline status updates confirmed successful.")
	statusUpdateConflicts = metrics.NewCounter("runner_controller_status_update_conflicts_total", "Number of Runner status update conflicts.")
	statusUpdateUnknown   = metrics.NewCounter("runner_controller_status_update_unknown_total", "Number of Runner status updates with an unknown result.")
	futureHeartbeats      = metrics.NewCounter("runner_controller_future_heartbeat_total", "Number of reconciliations observing a future Runner health timestamp.")
	invalidTimestamps     = metrics.NewCounter("runner_controller_invalid_timestamp_total", "Number of Runner objects with invalid health timestamps.")
)
