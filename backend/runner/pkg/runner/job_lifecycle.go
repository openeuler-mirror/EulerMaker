package runner

import (
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"time"
)

type jobExecution struct {
	job    JobResource
	cancel context.CancelFunc
	ctx    context.Context
}

func terminalJob(phase string) bool {
	return phase == "Succeeded" || phase == "Failed" || phase == "Aborted"
}

func (a *Agent) registerExecution(parent context.Context, job JobResource) (context.Context, string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := job.Metadata.UID
	if key == "" {
		return nil, "", false
	}
	if _, exists := a.executions[key]; exists {
		return nil, key, false
	}
	// Scheduler accounting can release an aborted Job before its process exits.
	// Hold new local starts while any cancelled execution is still stopping.
	for _, execution := range a.executions {
		if execution.ctx.Err() != nil {
			return nil, key, false
		}
	}
	if a.executions == nil {
		a.executions = make(map[string]*jobExecution)
	}
	ctx, cancel := context.WithCancel(parent)
	a.executions[key] = &jobExecution{job: job, cancel: cancel, ctx: ctx}
	return ctx, key, true
}

func (a *Agent) cancelExecution(uid string) {
	a.mu.Lock()
	execution := a.executions[uid]
	a.mu.Unlock()
	if execution != nil {
		execution.cancel()
	}
}

// A relist also checks active jobs missing from the assigned list. A missing
// object or a new incarnation must stop the old execution, not start a new one.
func (a *Agent) reconcileExecutions(ctx context.Context) {
	a.mu.Lock()
	jobs := make([]JobResource, 0, len(a.executions))
	for _, execution := range a.executions {
		jobs = append(jobs, execution.job)
	}
	a.mu.Unlock()
	for _, job := range jobs {
		checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		current, err := a.client.GetJob(checkCtx, job.Metadata.Namespace, job.Metadata.Name)
		cancel()
		var statusErr StatusError
		if errors.As(err, &statusErr) && statusErr.Code == 404 || err == nil && (current.Metadata.UID != job.Metadata.UID || current.Status.Runner != a.cfg.Name || terminalJob(current.Status.Phase)) {
			a.cancelExecution(job.Metadata.UID)
		} else if err != nil {
			log.Printf("confirm active Job %s: %v", jobKey(job), err)
		}
	}
}

// Never replay a status derived from an old version. After an ambiguous write,
// confirm the entire intent; otherwise let the next list/watch reconcile it.
func (a *Agent) writeJobStatus(ctx context.Context, job *JobResource, status JobStatus) error {
	if status.StartTime != nil {
		t := status.StartTime.UTC().Truncate(time.Second)
		status.StartTime = &t
	}
	if status.EndTime != nil {
		t := status.EndTime.UTC().Truncate(time.Second)
		status.EndTime = &t
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	updated, err := a.client.UpdateJobStatus(writeCtx, *job, status)
	if err == nil && updated != nil && updated.Metadata.UID == job.Metadata.UID && reflect.DeepEqual(updated.Status, status) {
		*job = *updated
		return nil
	}
	confirmCtx, confirmCancel := context.WithTimeout(ctx, 10*time.Second)
	defer confirmCancel()
	current, getErr := a.client.GetJob(confirmCtx, job.Metadata.Namespace, job.Metadata.Name)
	if getErr == nil && current.Metadata.UID == job.Metadata.UID && reflect.DeepEqual(current.Status, status) {
		*job = *current
		return nil
	}
	if getErr == nil && (current.Metadata.UID != job.Metadata.UID || terminalJob(current.Status.Phase)) {
		a.cancelExecution(job.Metadata.UID)
	}
	return fmt.Errorf("Job status intent unconfirmed: write=%v, confirmation=%v", err, getErr)
}
