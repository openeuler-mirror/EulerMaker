package runner

import (
	"context"
	"errors"
	"sync"
	"testing"

	"ebs-runner/pkg/gateway"
	"ebs-runner/pkg/options"
)

type lifecycleAPI struct {
	fakeRunnerAPI
	lock          sync.Mutex
	job           gateway.JobResource
	writeErr      error
	confirmIntent bool
}

func (f *lifecycleAPI) GetJob(context.Context, string, string) (*gateway.JobResource, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	job := f.job
	return &job, nil
}

func (f *lifecycleAPI) UpdateJobStatus(_ context.Context, job gateway.JobResource, status gateway.JobStatus) (*gateway.JobResource, error) {
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
		return nil, gateway.StatusError{Code: 409}
	}
	job.Status = status
	job.Metadata.ResourceVersion = "2"
	f.job = job
	return &job, nil
}

func TestTerminalEventDuringStartupAndStaleRunning(t *testing.T) {
	job := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "j", Namespace: "p", UID: "u"}, Status: gateway.JobStatus{Phase: "Running", Runner: "r"}}
	a := &Agent{cfg: options.Config{RootDir: t.TempDir()}, client: &lifecycleAPI{job: job}}
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
		job := gateway.JobResource{Metadata: gateway.ObjectMeta{UID: "u", ResourceVersion: "1"}, Status: gateway.JobStatus{Phase: "Running"}}
		api := &lifecycleAPI{job: job, writeErr: errors.New("lost response"), confirmIntent: confirmed}
		if !confirmed {
			api.job.Status.Phase = "Aborted"
		}
		a := &Agent{client: api}
		err := a.writeJobStatus(context.Background(), &job, gateway.JobStatus{Phase: "Succeeded"})
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
