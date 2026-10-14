package build

import "controller-manager/pkg/metrics"

var (
	statusUpdateConflicts = metrics.NewCounter("build_controller_status_update_conflicts_total", "Build status decisions abandoned or rejected by optimistic concurrency.")
	statusUpdateUnknowns  = metrics.NewCounter("build_controller_status_update_unknown_total", "Build status writes whose outcome could not be confirmed by the client.")
	ensureConflicts       = metrics.NewCounter("build_controller_ensure_conflicts_total", "Sub-resource creates that lost the race and reused the existing object.")
	ensureTerminating     = metrics.NewCounter("build_controller_ensure_terminating_total", "Sub-resource ensures that found an object under deletion.")
)
