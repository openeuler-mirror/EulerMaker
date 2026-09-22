package runner

import (
	"context"
	"testing"
	"time"
)

type waitingArtifactRemote struct {
	fakeArtifactRemote
	started chan struct{}
}

func (r *waitingArtifactRemote) LogStatus(ctx context.Context, _, _, _ string) (LogStatus, error) {
	close(r.started)
	<-ctx.Done()
	return LogStatus{}, ctx.Err()
}

func TestAbortCancelsPostRunWithoutFinalStatusWrite(t *testing.T) {
	job := JobResource{Metadata: ObjectMeta{Name: "j", Namespace: "p", UID: "u"}, Status: JobStatus{Phase: "Running", Runner: "r", Stage: "PostRun"}}
	api := &lifecycleAPI{job: job}
	remote := &waitingArtifactRemote{started: make(chan struct{})}
	a := &Agent{cfg: Config{Name: "r", ArtifactUploadTimeout: time.Minute}, client: api, artifacts: &ArtifactProcessor{Remote: remote}}
	ctx, key, _ := a.registerExecution(context.Background(), job)
	done := make(chan struct{})
	go func() { a.resumePostRun(ctx, key, job); close(done) }()
	select {
	case <-remote.started:
	case <-time.After(time.Second):
		t.Fatal("upload did not start")
	}
	job.Status.Phase = "Aborted"
	api.lock.Lock()
	api.job = job
	api.lock.Unlock()
	a.handleEvent(context.Background(), WatchEvent{Type: "MODIFIED", Object: job})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("upload not cancelled")
	}
	api.lock.Lock()
	defer api.lock.Unlock()
	if api.job.Status.Phase != "Aborted" {
		t.Fatal("terminal result overwritten")
	}
}
