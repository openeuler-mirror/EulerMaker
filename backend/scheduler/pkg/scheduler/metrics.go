package scheduler

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"scheduler/pkg/framework"
)

type metrics struct {
	mu              sync.Mutex
	attempts        map[string]uint64
	bind            map[string]uint64
	confirm         map[string]uint64
	invalid         map[string]uint64
	cycles          uint64
	duration        time.Duration
	unknownReleased uint64
}

func newMetrics() *metrics {
	return &metrics{attempts: map[string]uint64{}, bind: map[string]uint64{}, confirm: map[string]uint64{}, invalid: map[string]uint64{}}
}
func (m *metrics) recordCycle(result *framework.CycleResult, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cycles++
	m.duration += d
	m.attempts[string(result.Code)]++
	if result.Code == framework.Scheduled || result.Code == framework.BindUnknown {
		m.bind[string(result.Code)]++
	}
	if result.Code == framework.UnschedulableError {
		m.invalid[result.Reason]++
	}
}
func (m *metrics) recordConfirm(result string, released bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.confirm[result]++
	if released {
		m.unknownReleased++
	}
}
func keys(values map[string]uint64) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}
func (m *metrics) write(w io.Writer, assumed int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, key := range keys(m.attempts) {
		fmt.Fprintf(w, "scheduler_scheduling_attempts_total{result=%q} %d\n", key, m.attempts[key])
	}
	fmt.Fprintf(w, "scheduler_scheduling_duration_seconds_count %d\nscheduler_scheduling_duration_seconds_sum %.9f\n", m.cycles, m.duration.Seconds())
	for _, key := range keys(m.bind) {
		fmt.Fprintf(w, "scheduler_bind_total{result=%q} %d\n", key, m.bind[key])
	}
	fmt.Fprintf(w, "scheduler_bind_unknown_release_total %d\nscheduler_assumed_jobs %d\n", m.unknownReleased, assumed)
	for _, key := range keys(m.confirm) {
		fmt.Fprintf(w, "scheduler_assume_confirmation_total{result=%q} %d\n", key, m.confirm[key])
	}
	for _, key := range keys(m.invalid) {
		fmt.Fprintf(w, "scheduler_invalid_jobs_total{reason=%q} %d\n", key, m.invalid[key])
	}
	fmt.Fprint(w, "scheduler_plugin_duration_seconds_count 0\nscheduler_filter_rejections_total{plugin=\"none\",reason=\"none\"} 0\nscheduler_resource_overcommit_total{resource=\"cpu\"} 0\nscheduler_resource_overcommit_total{resource=\"memory\"} 0\n")
}
