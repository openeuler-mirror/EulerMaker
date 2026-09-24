package gitserver

import "controller-manager/pkg/metrics"

var (
	syncRequests    = metrics.NewCounter("git_server_client_sync_requests_total", "Sync calls including local validation failures.")
	statusRequests  = metrics.NewCounter("git_server_client_status_requests_total", "Status calls including cache hits.")
	resolveRequests = metrics.NewCounter("git_server_client_resolve_requests_total", "Commit resolution calls.")
	execRequests    = metrics.NewCounter("git_server_client_exec_requests_total", "Read-only command executions including local validation failures.")
	requestFailures = metrics.NewCounter("git_server_client_request_failures_total", "Failed client calls after internal retries.")
	cacheHits       = metrics.NewCounter("git_server_client_cache_hits_total", "Repository status cache hits.")
)

func recordRequest(counter *metrics.Counter, err *error) {
	counter.Inc()
	if *err != nil {
		requestFailures.Inc()
	}
}
