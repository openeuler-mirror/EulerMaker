package runner

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type lifecycleAPI struct {
	fakeRunnerAPI
	lock          sync.Mutex
	job           JobResource
	writeErr      error
	confirmIntent bool
}

func (f *lifecycleAPI) GetJob(context.Context, string, string) (*JobResource, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	job := f.job
	return &job, nil
}
func (f *lifecycleAPI) UpdateJobStatus(_ context.Context, job JobResource, status JobStatus) (*JobResource, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	if f.writeErr != nil {
		if f.confirmIntent {
			f.job.Status = status
			f.job.Metadata.ResourceVersion = "2"
		}
		return nil, f.writeErr
	}
	if terminalJob(f.job.Status.Phase) {
		return nil, StatusError{Code: 409}
	}
	job.Status = status
	job.Metadata.ResourceVersion = "2"
	f.job = job
	return &job, nil
}

type cancellingExecutor struct {
	started chan struct{}
	stopped chan struct{}
}

func (e *cancellingExecutor) Execute(ctx context.Context, _ JobResource) (string, error) {
	close(e.started)
	<-ctx.Done()
	close(e.stopped)
	return "", ctx.Err()
}

func TestAbortWatchCancelsActiveExecution(t *testing.T) {
	job := JobResource{Metadata: ObjectMeta{Name: "j", Namespace: "p", UID: "u", ResourceVersion: "1"}, Status: JobStatus{Phase: "Running", Runner: "r"}}
	api := &lifecycleAPI{job: job}
	executor := &cancellingExecutor{started: make(chan struct{}), stopped: make(chan struct{})}
	a := &Agent{cfg: Config{Name: "r"}, client: api, executor: executor}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.handleEvent(ctx, WatchEvent{Type: "ADDED", Object: job})
	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("execution did not start")
	}
	job.Status.Phase = "Aborted"
	api.lock.Lock()
	api.job = job
	api.lock.Unlock()
	a.handleEvent(ctx, WatchEvent{Type: "MODIFIED", Object: job})
	select {
	case <-executor.stopped:
	case <-time.After(time.Second):
		t.Fatal("execution did not stop")
	}
	current, _ := api.GetJob(ctx, "p", "j")
	if current.Status.Phase != "Aborted" {
		t.Fatal("abort overwritten")
	}
}

func TestTerminalEventDuringStartupAndStaleRunning(t *testing.T) {
	job := JobResource{Metadata: ObjectMeta{Name: "j", Namespace: "p", UID: "u"}, Status: JobStatus{Phase: "Running", Runner: "r"}}
	a := &Agent{}
	ctx, key, ok := a.registerExecution(context.Background(), job)
	if !ok {
		t.Fatal("not registered")
	}
	a.cancelExecution("u")
	if ctx.Err() == nil {
		t.Fatal("startup context not cancelled")
	}
	if _, _, ok := a.registerExecution(context.Background(), job); ok {
		t.Fatal("stale event restarted active Job")
	}
	a.finishJob(key)
}

func TestStatusWriteUnknownConfirmsIntent(t *testing.T) {
	for _, confirmed := range []bool{true, false} {
		job := JobResource{Metadata: ObjectMeta{UID: "u", ResourceVersion: "1"}, Status: JobStatus{Phase: "Running"}}
		api := &lifecycleAPI{job: job, writeErr: errors.New("lost response"), confirmIntent: confirmed}
		if !confirmed {
			api.job.Status.Phase = "Aborted"
		}
		a := &Agent{client: api}
		err := a.writeJobStatus(context.Background(), &job, JobStatus{Phase: "Succeeded"})
		if (err == nil) != confirmed {
			t.Fatalf("confirmed=%v err=%v", confirmed, err)
		}
		if confirmed && job.Metadata.ResourceVersion != "2" {
			t.Fatal("did not use confirmed object")
		}
		if !confirmed && api.job.Status.Phase != "Aborted" {
			t.Fatal("replayed old decision")
		}
	}
}
