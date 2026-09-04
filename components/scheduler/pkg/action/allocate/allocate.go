package allocate

import (
	"context"
	"errors"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"scheduler/pkg/cache"
	"scheduler/pkg/client"
	"scheduler/pkg/framework"
	"scheduler/pkg/statement"
)

type Action struct {
	cache cache.Interface
	jobs  client.JobInterface
}

func New(c cache.Interface, jobs client.JobInterface) *Action { return &Action{cache: c, jobs: jobs} }
func (a *Action) Name() string                                { return "Allocate" }
func (a *Action) Execute(ctx context.Context, s *framework.Session) *framework.CycleResult {
	base := &framework.CycleResult{JobKey: s.Job.Namespace + "/" + s.Job.Name, JobUID: s.Job.UID, QueueAction: framework.QueueAddBackoff}
	if _, err := framework.ParseRequests(s.Job.Spec.Resources); err != nil {
		base.Code = framework.UnschedulableError
		base.Reason = "invalid-job-resources"
		base.Err = err
		base.QueueAction = framework.QueueDone
		return base
	}
	names := make([]string, 0, len(s.Runners))
	for name := range s.Runners {
		names = append(names, name)
	}
	sort.Strings(names)
	best := ""
	bestScore := int64(-1)
	hadError := false
	for _, name := range names {
		runner := s.Runners[name]
		accepted := true
		for _, p := range s.FilterPlugins {
			status := p.Filter(ctx, s, runner)
			if status == nil || status.Code == framework.Error {
				hadError = true
				accepted = false
				break
			}
			if status.Code != framework.Success {
				accepted = false
				break
			}
		}
		if !accepted {
			continue
		}
		weighted, total := int64(0), int64(0)
		for _, p := range s.ScorePlugins {
			score, status := p.Score(ctx, s, runner)
			if status == nil || status.Code != framework.Success {
				hadError = true
				accepted = false
				break
			}
			weighted += score * p.Weight()
			total += p.Weight()
		}
		if !accepted {
			continue
		}
		score := int64(0)
		if total > 0 {
			score = weighted / total
		}
		if score > bestScore {
			best, bestScore = name, score
		}
	}
	if best == "" {
		base.Code = framework.ResultUnschedulable
		base.Reason = "no-fit-runner"
		if hadError {
			base.Reason = "runner-or-plugin-error"
		}
		return base
	}
	runner := s.Runners[best]
	st, err := statement.New(a.cache, a.jobs, statement.Request{JobKey: base.JobKey, JobUID: s.Job.UID, RunnerName: best, RunnerUID: runner.Runner.UID, RunnerRevision: runner.Revision, Requests: s.Requests, JobResourceVersion: s.Job.ResourceVersion})
	if err != nil {
		base.Code = framework.InternalError
		base.Err = err
		return base
	}
	defer st.Discard()
	err = st.Commit(ctx)
	base.RunnerName = best
	base.Err = err
	switch {
	case err == nil:
		base.Code = framework.Scheduled
		base.Reason = "bound"
		base.QueueAction = framework.QueueDone
	case errors.Is(err, statement.ErrBindOutcomeUnknown):
		base.Code = framework.BindUnknown
		base.Reason = "bind-outcome-unknown"
		base.QueueAction = framework.QueueDone
	case errors.Is(err, statement.ErrJobNoLongerSchedulable):
		base.Code = framework.Conflict
		base.Reason = "job-no-longer-schedulable"
		base.QueueAction = framework.QueueDone
	case errors.Is(err, statement.ErrConflictRetryable) || errors.Is(err, cache.ErrStaleSnapshot):
		base.Code = framework.Conflict
		base.Reason = "conflict"
	case apierrors.IsUnauthorized(err) || apierrors.IsForbidden(err) || errors.Is(err, context.Canceled):
		base.Code = framework.InternalError
		base.Reason = "terminal-client-error"
		base.QueueAction = framework.QueueDone
	default:
		base.Code = framework.InternalError
		base.Reason = "bind-error"
	}
	return base
}
