package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ebs-runner/pkg/artifact"
	"ebs-runner/pkg/gateway"
	"ebs-runner/pkg/options"
)

type fakeArtifactRemote struct{}

func (f *fakeArtifactRemote) LogStatus(context.Context, string, string) (artifact.LogStatus, error) {
	size := int64(12)
	return artifact.LogStatus{State: "Completed", ArtifactID: "log-1", FinalSize: &size, FinalSHA256: "log-sha"}, nil
}

func (f *fakeArtifactRemote) UploadArtifact(_ context.Context, project, job, key string, _ string, input artifact.UploadArtifactInput) (artifact.ArtifactRecord, error) {
	return artifact.ArtifactRecord{
		ID:       "artifact-" + input.FileName,
		Category: input.Category, FileName: input.FileName, RelativePath: input.RelativePath,
		ContentType: input.ContentType, Size: input.Size, SHA256: input.SHA256, State: "Completed",
	}, nil
}

func (f *fakeArtifactRemote) CompleteManifest(_ context.Context, _, _ string, input artifact.CompleteManifestInput) (artifact.CompletedManifest, error) {
	return artifact.CompletedManifest{State: "Completed", ArtifactCount: len(input.Files), UploadDigest: input.UploadDigest()}, nil
}

func (f *fakeArtifactRemote) GetManifest(context.Context, string, string) (artifact.CompletedManifest, error) {
	return artifact.CompletedManifest{}, os.ErrNotExist
}

type fakeExecutor struct {
	called     bool
	resultRoot string
	err        error
}

func (f *fakeExecutor) Execute(context.Context, gateway.JobResource) (string, error) {
	f.called = true
	return f.resultRoot, f.err
}

type fakeRunnerAPI struct {
	mu          sync.Mutex
	jobStatuses []gateway.JobStatus
}

func (f *fakeRunnerAPI) GetRunner(context.Context, string) (*gateway.RunnerResource, error) {
	return nil, os.ErrNotExist
}

func (f *fakeRunnerAPI) CreateRunner(context.Context, gateway.RunnerResource) error { return nil }

func (f *fakeRunnerAPI) UpdateRunner(context.Context, gateway.RunnerResource) error { return nil }

func (f *fakeRunnerAPI) PatchRunnerStatus(context.Context, string, gateway.RunnerStatus) error {
	return nil
}

func (f *fakeRunnerAPI) GetJob(context.Context, string, string) (*gateway.JobResource, error) {
	return nil, os.ErrNotExist
}

func (f *fakeRunnerAPI) UpdateJobStatus(_ context.Context, job gateway.JobResource, status gateway.JobStatus) (*gateway.JobResource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobStatuses = append(f.jobStatuses, status)
	job.Status = status
	return &job, nil
}

func (f *fakeRunnerAPI) ListAssignedJobs(context.Context, string) (*gateway.JobList, error) {
	return &gateway.JobList{}, nil
}

func (f *fakeRunnerAPI) WatchAssignedJobs(context.Context, string, string) (<-chan gateway.WatchEvent, <-chan error) {
	events := make(chan gateway.WatchEvent)
	errs := make(chan error)
	close(events)
	close(errs)
	return events, errs
}

func TestRunJobCompletesManifestStatusAndCleansLocalState(t *testing.T) {
	root := t.TempDir()
	job := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "job", Namespace: "project", UID: "uid"}, Status: gateway.JobStatus{Phase: "Running", Runner: "runner-a"}}
	resultDir := filepath.Join(root, "results", "project", "job")
	if err := os.MkdirAll(resultDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resultDir, "result.txt"), []byte("success"), 0o644); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(root, "logs", "project", "job")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(root, "work", "project", "job")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	remote := &fakeArtifactRemote{}
	api := &fakeRunnerAPI{}
	agent := &Agent{
		cfg: options.Config{
			Name:     "runner-a",
			RootDir:  root,
			Artifact: artifact.Config{Upload: artifact.UploadConfig{Timeout: time.Second}},
		},
		client:    api,
		executor:  &fakeExecutor{resultRoot: resultDir},
		artifacts: &artifact.Processor{Remote: remote, RootDir: root, MaxFileSize: 1024, MaxJobSize: 2048, MaxFiles: 10, Concurrency: 1},
		cleanup:   &artifact.CleanupManager{RootDir: root, FailedRetention: 24 * time.Hour},
	}

	agent.runJob(context.Background(), jobKey(job), job)
	api.mu.Lock()
	statuses := append([]gateway.JobStatus(nil), api.jobStatuses...)
	api.mu.Unlock()
	if len(statuses) != 3 {
		t.Fatalf("job status updates = %d, want 3", len(statuses))
	}
	if statuses[1].Phase != "Running" || statuses[1].Stage != "PostRun" {
		t.Fatalf("post-run status = %#v", statuses[1])
	}
	final := statuses[2]
	if final.Phase != "Succeeded" {
		t.Fatalf("final status = %#v", final)
	}
	if _, err := os.Stat(resultDir); !os.IsNotExist(err) {
		t.Fatalf("successful result directory was not cleaned: %v", err)
	}
	if _, err := os.Stat(logDir); !os.IsNotExist(err) {
		t.Fatalf("successful log directory was not cleaned: %v", err)
	}
	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Fatalf("Job work directory was not cleaned after PostRun: %v", err)
	}
}

func TestResumePostRunDoesNotExecuteJobAgain(t *testing.T) {
	root := t.TempDir()
	resultDir := filepath.Join(root, "results", "project", "job")
	if err := os.MkdirAll(resultDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resultDir, "result.txt"), []byte("success"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := gateway.JobResource{
		Metadata: gateway.ObjectMeta{Name: "job", Namespace: "project", UID: "uid"},
		Status:   gateway.JobStatus{Phase: "Running", Stage: "PostRun", Runner: "runner-a"},
	}
	workDir := filepath.Join(root, "work", "project", "job")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	executor := &fakeExecutor{resultRoot: resultDir}
	api := &fakeRunnerAPI{}
	agent := &Agent{
		cfg: options.Config{
			Name:     "runner-a",
			RootDir:  root,
			Artifact: artifact.Config{Upload: artifact.UploadConfig{Timeout: time.Second}},
		},
		client:    api,
		executor:  executor,
		artifacts: &artifact.Processor{Remote: &fakeArtifactRemote{}, RootDir: root, MaxFileSize: 1024, MaxJobSize: 2048, MaxFiles: 10, Concurrency: 1},
		cleanup:   &artifact.CleanupManager{RootDir: root, FailedRetention: 24 * time.Hour},
	}

	agent.resumePostRun(context.Background(), jobKey(job), job)
	if executor.called {
		t.Fatal("PostRun recovery executed the build again")
	}
	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Fatalf("recovered PostRun work directory was not cleaned: %v", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.jobStatuses) != 1 || api.jobStatuses[0].Phase != "Succeeded" {
		t.Fatalf("recovered statuses = %#v", api.jobStatuses)
	}
}

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

type cancellingExecutor struct {
	started     chan struct{}
	stopped     chan struct{}
	allowReturn chan struct{}
}

func (e *cancellingExecutor) Execute(ctx context.Context, _ gateway.JobResource) (string, error) {
	close(e.started)
	<-ctx.Done()
	close(e.stopped)
	if e.allowReturn != nil {
		<-e.allowReturn
	}
	return "", ctx.Err()
}

func TestAbortWatchCancelsActiveExecution(t *testing.T) {
	job := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "j", Namespace: "p", UID: "u", ResourceVersion: "1"}, Status: gateway.JobStatus{Phase: "Running", Runner: "r"}}
	api := &lifecycleAPI{job: job}
	executor := &cancellingExecutor{started: make(chan struct{}), stopped: make(chan struct{}), allowReturn: make(chan struct{})}
	root := t.TempDir()
	workPath := filepath.Join(root, "work", "p", "j")
	if err := os.MkdirAll(workPath, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &Agent{cfg: options.Config{Name: "r", RootDir: root}, client: api, executor: executor}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.handleEvent(ctx, gateway.WatchEvent{Type: "ADDED", Object: job})
	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("execution did not start")
	}
	job.Status.Phase = "Aborted"
	api.lock.Lock()
	api.job = job
	api.lock.Unlock()
	a.handleEvent(ctx, gateway.WatchEvent{Type: "MODIFIED", Object: job})
	select {
	case <-executor.stopped:
	case <-time.After(time.Second):
		t.Fatal("execution did not stop")
	}
	if _, err := os.Stat(workPath); err != nil {
		t.Fatalf("work directory removed before executor returned: %v", err)
	}
	close(executor.allowReturn)
	deadline := time.After(time.Second)
	for {
		if _, err := os.Stat(workPath); os.IsNotExist(err) {
			break
		}
		select {
		case <-deadline:
			t.Fatal("aborted Job work directory was not cleaned")
		case <-time.After(time.Millisecond):
		}
	}
	current, _ := api.GetJob(ctx, "p", "j")
	if current.Status.Phase != "Aborted" {
		t.Fatal("abort overwritten")
	}
}

func TestTerminalEventDuringStartupAndStaleRunning(t *testing.T) {
	job := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "j", Namespace: "p", UID: "u"}, Status: gateway.JobStatus{Phase: "Running", Runner: "r"}}
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

type waitingArtifactRemote struct {
	fakeArtifactRemote
	started chan struct{}
}

func (r *waitingArtifactRemote) LogStatus(ctx context.Context, _, _ string) (artifact.LogStatus, error) {
	close(r.started)
	<-ctx.Done()
	return artifact.LogStatus{}, ctx.Err()
}

func TestAbortCancelsPostRunWithoutFinalStatusWrite(t *testing.T) {
	job := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "j", Namespace: "p", UID: "u"}, Status: gateway.JobStatus{Phase: "Running", Runner: "r", Stage: "PostRun"}}
	api := &lifecycleAPI{job: job}
	remote := &waitingArtifactRemote{started: make(chan struct{})}
	a := &Agent{
		cfg: options.Config{
			Name:     "r",
			Artifact: artifact.Config{Upload: artifact.UploadConfig{Timeout: time.Minute}},
		},
		client:    api,
		artifacts: &artifact.Processor{Remote: remote},
	}
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
	a.handleEvent(context.Background(), gateway.WatchEvent{Type: "MODIFIED", Object: job})
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
