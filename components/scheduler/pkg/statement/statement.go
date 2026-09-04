package statement

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"scheduler/pkg/cache"
	"scheduler/pkg/client"
	"scheduler/pkg/framework"
)

var (
	ErrBindOutcomeUnknown     = errors.New("bind outcome unknown")
	ErrConflictRetryable      = errors.New("bind conflict is retryable")
	ErrJobNoLongerSchedulable = errors.New("job is no longer schedulable")
	ErrStatementDiscarded     = errors.New("statement discarded")
)

type Request struct {
	JobKey             string
	JobUID             types.UID
	RunnerName         string
	RunnerUID          types.UID
	RunnerRevision     uint64
	Requests           framework.Resource
	JobResourceVersion string
}
type State string

const (
	StateNew       State = "New"
	StateAssumed   State = "Assumed"
	StateCommitted State = "Committed"
	StateUnknown   State = "Unknown"
	StateDiscarded State = "Discarded"
)

type Statement struct {
	cache            cache.Interface
	jobs             client.JobInterface
	request          Request
	assumeGeneration uint64
	state            State
	commitErr        error
}

func New(c cache.Interface, jobs client.JobInterface, request Request) (*Statement, error) {
	if c == nil || jobs == nil {
		return nil, fmt.Errorf("cache and job client are required")
	}
	if request.JobKey == "" || request.JobUID == "" || request.RunnerName == "" || request.RunnerUID == "" || request.JobResourceVersion == "" || !strings.Contains(request.JobKey, "/") {
		return nil, fmt.Errorf("invalid statement request")
	}
	return &Statement{cache: c, jobs: jobs, request: request, state: StateNew}, nil
}
func (s *Statement) State() State { return s.state }
func (s *Statement) Commit(ctx context.Context) error {
	if s.state != StateNew {
		return s.commitErr
	}
	generation, err := s.cache.Assume(cache.AssumeRequest{JobKey: s.request.JobKey, JobUID: s.request.JobUID, RunnerName: s.request.RunnerName, RunnerUID: s.request.RunnerUID, RunnerRevision: s.request.RunnerRevision, Requests: s.request.Requests, JobResourceVersion: s.request.JobResourceVersion})
	if err != nil {
		return s.discard(err)
	}
	s.assumeGeneration = generation
	s.state = StateAssumed
	parts := strings.SplitN(s.request.JobKey, "/", 2)
	current, err := s.jobs.Get(ctx, parts[0], parts[1], metav1.GetOptions{})
	if err != nil {
		return s.discard(err)
	}
	if current == nil || current.UID != s.request.JobUID || current.Status.Phase != "Pending" || current.Status.Runner != "" || current.ResourceVersion != s.request.JobResourceVersion {
		return s.discard(ErrJobNoLongerSchedulable)
	}
	updated := current.DeepCopy()
	updated.Status.Phase = "Running"
	updated.Status.Runner = s.request.RunnerName
	response, err := s.jobs.UpdateStatus(ctx, parts[0], parts[1], updated, metav1.UpdateOptions{})
	if err != nil {
		var writeErr *client.WriteError
		if errors.As(err, &writeErr) && writeErr.Outcome == client.WriteUnknown {
			s.state = StateUnknown
			s.commitErr = fmt.Errorf("%w: %v", ErrBindOutcomeUnknown, err)
			return s.commitErr
		}
		s.discard(err)
		if apierrors.IsConflict(err) {
			latest, getErr := s.jobs.Get(ctx, parts[0], parts[1], metav1.GetOptions{})
			if getErr != nil {
				s.commitErr = getErr
				return getErr
			}
			if latest != nil && latest.UID == s.request.JobUID && latest.Status.Phase == "Pending" && latest.Status.Runner == "" {
				s.commitErr = ErrConflictRetryable
			} else {
				s.commitErr = ErrJobNoLongerSchedulable
			}
			return s.commitErr
		}
		return s.commitErr
	}
	if response == nil || response.UID != s.request.JobUID || response.Status.Phase != "Running" || response.Status.Runner != s.request.RunnerName || response.ResourceVersion == "" {
		s.state = StateUnknown
		s.commitErr = fmt.Errorf("%w: unexpected-success-response", ErrBindOutcomeUnknown)
		return s.commitErr
	}
	s.state = StateCommitted
	s.commitErr = nil
	return nil
}
func (s *Statement) discard(err error) error {
	if s.state == StateAssumed {
		s.cache.Forget(s.request.JobKey, s.request.JobUID, s.assumeGeneration)
	}
	s.state = StateDiscarded
	s.commitErr = err
	return err
}
func (s *Statement) Discard() {
	if s.state == StateNew {
		s.discard(ErrStatementDiscarded)
	} else if s.state == StateAssumed {
		s.discard(ErrStatementDiscarded)
	}
}
