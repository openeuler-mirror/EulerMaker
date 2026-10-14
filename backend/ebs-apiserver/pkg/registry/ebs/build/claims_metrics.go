package build

import (
	"sync"

	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

var (
	claimMetricsOnce       sync.Once
	claimConflicts         = metrics.NewCounter(&metrics.CounterOpts{Name: "ebs_build_claim_conflicts_total", Help: "Build target admission conflicts."})
	claimRecoveryErrors    = metrics.NewCounter(&metrics.CounterOpts{Name: "ebs_build_claim_recovery_errors_total", Help: "Build claim recovery and cleanup errors."})
	claimUnconfirmed       = metrics.NewGauge(&metrics.GaugeOpts{Name: "ebs_build_claim_unconfirmed", Help: "Unconfirmed Creating claims observed by the last complete scan."})
	claimOldestUnconfirmed = metrics.NewGauge(&metrics.GaugeOpts{Name: "ebs_build_claim_oldest_unconfirmed_seconds", Help: "Age of the oldest unconfirmed Creating claim observed by the last complete scan."})
)

func registerClaimMetrics() {
	claimMetricsOnce.Do(func() {
		legacyregistry.MustRegister(claimConflicts, claimRecoveryErrors, claimUnconfirmed, claimOldestUnconfirmed)
	})
}
