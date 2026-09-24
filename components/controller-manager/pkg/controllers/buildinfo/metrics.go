package buildinfo

import "controller-manager/pkg/metrics"

// Metrics of the BuildInfo controller (design 11.2): label-less counters
// registered via pkg/metrics. State-change counters are incremented only
// after the write succeeded (including Unknown writes confirmed by the 10.3
// confirmation read), so retried rounds never double-count.
var (
	// phaseTransitions counts phase writes confirmed persisted.
	phaseTransitions = metrics.NewCounter("build_info_controller_phase_transitions_total", "BuildInfo phase transitions confirmed persisted.")
	// conditionsUpserts counts condition upsert/removes confirmed persisted.
	conditionsUpserts = metrics.NewCounter("build_info_controller_conditions_total", "BuildInfo condition upserts and removals confirmed persisted.")
	// conflictRequeues counts 409 delayed requeues (10.2).
	conflictRequeues = metrics.NewCounter("build_info_controller_conflict_requeues_total", "BuildInfo status writes rejected with 409 Conflict that requeue with a 1s delay.")
	// unknownWrites counts writes entering the 10.3 confirmation read.
	unknownWrites = metrics.NewCounter("build_info_controller_unknown_writes_total", "BuildInfo status writes and Job creates whose outcome is Unknown and enter the confirmation read.")
	// jobCreates counts Job create requests sent.
	jobCreates = metrics.NewCounter("build_info_controller_job_create_total", "Job create requests sent.")
	// jobCreateFailures counts Job creates that failed finally (including
	// Unknown whose confirmation GET found no Job).
	jobCreateFailures = metrics.NewCounter("build_info_controller_job_create_failures_total", "Job create requests that failed finally.")
	// dispatches counts successful dispatch write-backs (bootstrap, first and
	// rebuild dispatches are indistinguishable here).
	dispatches = metrics.NewCounter("build_info_controller_dispatch_total", "Spec dispatches confirmed persisted.")
	// bootstrapBreaks counts bootstrap-break selections (initial and
	// runtime-appended) confirmed persisted.
	bootstrapBreaks = metrics.NewCounter("build_info_controller_bootstrap_break_total", "Bootstrap-break selections confirmed persisted.")
	// edgesAdded counts runtime install edges confirmed persisted.
	edgesAdded = metrics.NewCounter("build_info_controller_edge_added_total", "Runtime install edges added to the DCG, confirmed persisted.")
	// specDependsCacheHits counts per-BuildInfo specDepends cache hits on
	// Processing rounds (Pending re-assembly rounds never count).
	specDependsCacheHits = metrics.NewCounter("build_info_controller_specdepends_cache_hits_total", "Per-BuildInfo specDepends cache hits on Processing rounds.")
	// specFileCacheHits counts global spec file LRU hits (no git-server
	// download needed).
	specFileCacheHits = metrics.NewCounter("build_info_controller_specfile_cache_hits_total", "Global spec file LRU cache hits.")
	// specDependsFills counts completed step-0 assembly rounds.
	specDependsFills = metrics.NewCounter("build_info_controller_specdepends_fill_total", "SpecDepends assembly rounds completed.")
	// rpmMetaRefreshes counts RpmMetaSources layer re-downloads.
	rpmMetaRefreshes = metrics.NewCounter("build_info_controller_rpmmeta_source_refresh_total", "RpmMetaSources layers re-downloaded and parsed.")
	// gitServerFailures counts git-server requests that failed after the
	// in-process retry budget was exhausted.
	gitServerFailures = metrics.NewCounter("build_info_controller_gitserver_request_failures_total", "Git-server requests that failed after the retry budget was exhausted.")
	// rpmRepoEscalations counts E-29 escalations whose final Completed write
	// succeeded.
	rpmRepoEscalations = metrics.NewCounter("build_info_controller_rpmrepo_unavailable_escalations_total", "RpmRepoUnavailable escalations whose final Completed write succeeded.")
	// snapshotEscalations counts E-30 escalations whose final Completed write
	// succeeded.
	snapshotEscalations = metrics.NewCounter("build_info_controller_snapshot_unavailable_escalations_total", "SnapshotUnavailable escalations whose final Completed write succeeded.")
	// invalidResults counts structurally invalid ReconcileResults returned by
	// the business sync (7.5: programming error, not retried).
	invalidResults = metrics.NewCounter("build_info_controller_invalid_results_total", "Structurally invalid ReconcileResults returned by the sync function.")
)
