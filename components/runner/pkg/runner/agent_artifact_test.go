package runner

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeRunnerAPI struct {
	mu          sync.Mutex
	jobStatuses []JobStatus
}

func (f *fakeRunnerAPI) GetRunner(context.Context, string) (*RunnerResource, error) {
	return nil, os.ErrNotExist
}
func (f *fakeRunnerAPI) CreateRunner(context.Context, RunnerResource) error { return nil }
func (f *fakeRunnerAPI) UpdateRunner(context.Context, RunnerResource) error { return nil }
func (f *fakeRunnerAPI) PatchRunnerStatus(context.Context, string, RunnerStatus) error {
	return nil
}
func (f *fakeRunnerAPI) PatchJobStatus(_ context.Context, _, _ string, status JobStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobStatuses = append(f.jobStatuses, status)
	return nil
}
func (f *fakeRunnerAPI) ListAssignedJobs(context.Context, string) (*JobList, error) {
	return &JobList{}, nil
}
func (f *fakeRunnerAPI) WatchAssignedJobs(context.Context, string, string) (<-chan WatchEvent, <-chan error) {
	events := make(chan WatchEvent)
	errs := make(chan error)
	close(events)
	close(errs)
	return events, errs
}

func TestRunJobCompletesManifestStatusAndCleansLocalState(t *testing.T) {
	root := t.TempDir()
	job := JobResource{Metadata: ObjectMeta{Name: "job", Namespace: "project", UID: "uid"}, Status: JobStatus{Phase: "Running", Runner: "runner-a"}}
	resultDir := filepath.Join(root, "results", "project", "uid")
	if err := os.MkdirAll(resultDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resultDir, "result.txt"), []byte("success"), 0o644); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(root, "logs", "project", "uid")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	remote := &fakeArtifactRemote{}
	api := &fakeRunnerAPI{}
	agent := &Agent{
		cfg:        Config{Name: "runner-a", RootDir: root, ArtifactUploadTimeout: time.Second},
		client:     api,
		executor:   &fakeExecutor{resultRoot: resultDir},
		artifacts:  &ArtifactProcessor{Remote: remote, RootDir: root, MaxFileSize: 1024, MaxJobSize: 2048, MaxFiles: 10, Concurrency: 1},
		cleanup:    &ArtifactCleanupManager{RootDir: root, FailedRetention: 24 * time.Hour},
		activeJobs: map[string]struct{}{},
	}

	agent.runJob(context.Background(), jobKey(job), job)
	api.mu.Lock()
	statuses := append([]JobStatus(nil), api.jobStatuses...)
	api.mu.Unlock()
	if len(statuses) != 3 {
		t.Fatalf("job status updates = %d, want 3", len(statuses))
	}
	if statuses[1].Phase != "Running" || statuses[1].Stage != "PostRun" || statuses[1].ArtifactState != "Uploading" {
		t.Fatalf("post-run status = %#v", statuses[1])
	}
	final := statuses[2]
	if final.Phase != "Completed" || final.ArtifactState != "Completed" || final.ResultRoot != "artifact://uid" || final.ArtifactGeneration != 1 || final.ArtifactCount != 2 {
		t.Fatalf("final status = %#v", final)
	}
	if _, err := os.Stat(resultDir); !os.IsNotExist(err) {
		t.Fatalf("successful result directory was not cleaned: %v", err)
	}
	if _, err := os.Stat(logDir); !os.IsNotExist(err) {
		t.Fatalf("successful log directory was not cleaned: %v", err)
	}
}

func TestResumePostRunDoesNotExecuteJobAgain(t *testing.T) {
	root := t.TempDir()
	resultDir := filepath.Join(root, "results", "project", "uid")
	if err := os.MkdirAll(resultDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resultDir, "result.txt"), []byte("success"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := JobResource{
		Metadata: ObjectMeta{Name: "job", Namespace: "project", UID: "uid"},
		Status:   JobStatus{Phase: "Running", Stage: "PostRun", Runner: "runner-a", ArtifactState: "Uploading", ResultRoot: resultDir},
	}
	executor := &fakeExecutor{resultRoot: resultDir}
	api := &fakeRunnerAPI{}
	agent := &Agent{
		cfg: Config{Name: "runner-a", RootDir: root, ArtifactUploadTimeout: time.Second}, client: api, executor: executor,
		artifacts: &ArtifactProcessor{Remote: &fakeArtifactRemote{}, RootDir: root, MaxFileSize: 1024, MaxJobSize: 2048, MaxFiles: 10, Concurrency: 1},
		cleanup:   &ArtifactCleanupManager{RootDir: root, FailedRetention: 24 * time.Hour}, activeJobs: map[string]struct{}{},
	}

	agent.resumePostRun(context.Background(), jobKey(job), job)
	if executor.called {
		t.Fatal("PostRun recovery executed the build again")
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.jobStatuses) != 1 || api.jobStatuses[0].Phase != "Completed" || api.jobStatuses[0].ArtifactState != "Completed" {
		t.Fatalf("recovered statuses = %#v", api.jobStatuses)
	}
}
