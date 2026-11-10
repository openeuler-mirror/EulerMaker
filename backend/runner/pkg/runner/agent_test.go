package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ebs-runner/pkg/artifact"
	"ebs-runner/pkg/gateway"
	"ebs-runner/pkg/options"
)

func TestCancelledExecutionRetainsResumableData(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		root := t.TempDir()
		job := gateway.JobResource{
			Metadata: gateway.ObjectMeta{Namespace: "project", Name: "job", UID: "uid"},
			Status:   gateway.JobStatus{Phase: "Running", Stage: "PostRun"},
		}
		api := &workCleanupAPI{jobs: map[string]gateway.JobResource{"project/job": job}}
		if unavailable {
			api.err = errors.New("gateway unavailable")
		}
		a := &Agent{cfg: options.Config{RootDir: root}, client: api}
		_, key, _ := a.registerExecution(context.Background(), job)
		for _, category := range []string{"work", "results", "logs", "uploads"} {
			if err := os.MkdirAll(filepath.Join(root, category, "project", "job"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		a.cancelExecution(key)
		a.finishJob(key)
		for _, category := range []string{"work", "results", "logs", "uploads"} {
			if _, err := os.Stat(filepath.Join(root, category, "project", "job")); err != nil {
				t.Fatalf("resumable %s removed (unavailable=%t): %v", category, unavailable, err)
			}
		}
	}
}

func TestAgentVersionStatus(t *testing.T) {
	a := &Agent{cfg: options.Config{RootDir: t.TempDir()}}
	status := a.currentStatus()
	if status.Info.AgentVersion != agentVersion {
		t.Fatalf("agentVersion = %q, want %q", status.Info.AgentVersion, agentVersion)
	}
	t.Logf("reported agentVersion: %s", status.Info.AgentVersion)
}

func TestMemoryQuantityFromMeminfo(t *testing.T) {
	meminfo := "MemTotal:       65857640 kB\nMemFree:         1024 kB\n"
	got := memoryQuantityFromMeminfo(meminfo)
	if got != "64314Mi" {
		t.Fatalf("expected 64314Mi, got %s", got)
	}
}

func TestBytesToGiQuantity(t *testing.T) {
	got := bytesToGiQuantity(10 * 1024 * 1024 * 1024)
	if got != "10Gi" {
		t.Fatalf("expected 10Gi, got %s", got)
	}
}

const testRunnerInstanceID = "01234567-89ab-4def-8123-456789abcdef"

type registrationAPI struct {
	getResults []*gateway.RunnerResource
	getErrors  []error
	getCalls   int
	createErr  error
	created    *gateway.RunnerResource
	updated    *gateway.RunnerResource
}

func (f *registrationAPI) GetRunner(context.Context, string) (*gateway.RunnerResource, error) {
	index := f.getCalls
	f.getCalls++
	var result *gateway.RunnerResource
	var err error
	if index < len(f.getResults) {
		result = f.getResults[index]
	}
	if index < len(f.getErrors) {
		err = f.getErrors[index]
	}
	return result, err
}

func (f *registrationAPI) CreateRunner(_ context.Context, runner gateway.RunnerResource) error {
	f.created = &runner
	return f.createErr
}

func (f *registrationAPI) UpdateRunner(_ context.Context, runner gateway.RunnerResource) error {
	f.updated = &runner
	return nil
}

func (*registrationAPI) PatchRunnerStatus(context.Context, string, gateway.RunnerStatus) error {
	return nil
}

func (*registrationAPI) GetJob(context.Context, string, string) (*gateway.JobResource, error) {
	return nil, os.ErrNotExist
}

func (*registrationAPI) UpdateJobStatus(_ context.Context, job gateway.JobResource, status gateway.JobStatus) (*gateway.JobResource, error) {
	job.Status = status
	return &job, nil
}

func (*registrationAPI) ListAssignedJobs(context.Context, string) (*gateway.JobList, error) {
	return nil, nil
}

func (*registrationAPI) WatchAssignedJobs(context.Context, string, string) (<-chan gateway.WatchEvent, <-chan error) {
	return nil, nil
}

func newRegistrationAgent(client RunnerAPI) *Agent {
	return &Agent{
		cfg:    options.Config{Name: "runner-a", Type: "ct", Arch: "x86_64"},
		client: client, instanceID: testRunnerInstanceID,
	}
}

func TestRegisterCreatesRunnerWithInstanceID(t *testing.T) {
	client := &registrationAPI{getErrors: []error{gateway.StatusError{Code: 404}}}
	if err := newRegistrationAgent(client).register(context.Background()); err != nil {
		t.Fatalf("register: %v", err)
	}
	if client.created == nil || client.created.Spec.InstanceID != testRunnerInstanceID {
		t.Fatalf("created runner = %#v", client.created)
	}
}

func TestRegisterUpdatesMatchingInstance(t *testing.T) {
	existing := &gateway.RunnerResource{
		Metadata: gateway.ObjectMeta{Name: "runner-a", Labels: map[string]string{"custom": "value"}},
		Spec:     gateway.RunnerSpec{InstanceID: testRunnerInstanceID, Type: "vm", Arch: "aarch64", Unschedulable: true},
	}
	client := &registrationAPI{getResults: []*gateway.RunnerResource{existing}}
	if err := newRegistrationAgent(client).register(context.Background()); err != nil {
		t.Fatalf("register: %v", err)
	}
	if client.updated == nil || client.updated.Spec.InstanceID != testRunnerInstanceID || !client.updated.Spec.Unschedulable {
		t.Fatalf("updated runner = %#v", client.updated)
	}
	if client.updated.Metadata.Labels["custom"] != "value" {
		t.Fatalf("custom labels were not preserved: %#v", client.updated.Metadata.Labels)
	}
}

func TestRegisterRejectsDifferentInstance(t *testing.T) {
	existing := &gateway.RunnerResource{Spec: gateway.RunnerSpec{InstanceID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}}
	client := &registrationAPI{getResults: []*gateway.RunnerResource{existing}}
	err := newRegistrationAgent(client).register(context.Background())
	if err == nil || !strings.Contains(err.Error(), "different instance") {
		t.Fatalf("expected instance conflict, got %v", err)
	}
	if client.updated != nil {
		t.Fatal("different instance was updated")
	}
}

func TestRegisterResolvesCreateConflictByInstanceID(t *testing.T) {
	existing := &gateway.RunnerResource{Spec: gateway.RunnerSpec{InstanceID: testRunnerInstanceID}}
	client := &registrationAPI{
		getResults: []*gateway.RunnerResource{nil, existing},
		getErrors:  []error{gateway.StatusError{Code: 404}, nil},
		createErr:  gateway.StatusError{Code: 409},
	}
	if err := newRegistrationAgent(client).register(context.Background()); err != nil {
		t.Fatalf("register after matching conflict: %v", err)
	}
	if client.updated == nil {
		t.Fatal("matching runner was not reconciled after create conflict")
	}
}

func TestReadJobResult(t *testing.T) {
	for _, tc := range []struct {
		name, content, want string
		wantErr             bool
	}{
		{name: "absent"},
		{name: "success", content: `{"status":"Succeeded"}`, want: "Succeeded"},
		{name: "build failure", content: `{"status":"BuildFailed"}`, want: "BuildFailed"},
		{name: "install failure", content: `{"status":"InstallFailed","missingDeps":{"foo":{"neededBy":"bar"}}}`, want: "InstallFailed"},
		{name: "dependency scope", content: `{"status":"BuildFailed","missingDeps":{"foo":{}}}`, wantErr: true},
		{name: "incomplete result", content: `{}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "job-result.json")
			if tc.content != "" {
				if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := readJobResult(path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("read result = %+v, %v", result, err)
			}
			if tc.want != "" && (result == nil || result.Status != tc.want) {
				t.Fatalf("result = %+v, want %s", result, tc.want)
			}
			if tc.name == "absent" && result != nil {
				t.Fatalf("absent result inferred as %+v", result)
			}
		})
	}
}

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
	for _, verdict := range []string{"Succeeded", "InstallFailed", "BuildFailed"} {
		t.Run(verdict, func(t *testing.T) {
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
			resultJSON := `{"status":"` + verdict + `"}`
			if verdict == "InstallFailed" {
				resultJSON = `{"status":"InstallFailed","missingDeps":{"libfoo":{"neededBy":"example"}}}`
			}
			if err := os.WriteFile(filepath.Join(workDir, "job-result.json"), []byte(resultJSON), 0o644); err != nil {
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
			wantPhase := "Succeeded"
			if verdict == "BuildFailed" {
				wantPhase = "Failed"
			}
			if final.Result == nil || final.Result.Status != verdict {
				t.Fatalf("final script result = %+v, want %s", final.Result, verdict)
			}
			if verdict == "InstallFailed" && final.Result.MissingDeps["libfoo"].NeededBy != "example" {
				t.Fatalf("install missingDeps not retained: %+v", final.Result)
			}
			if final.Phase != wantPhase {
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
		})
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
	for _, category := range []string{"work", "results", "logs", "uploads"} {
		if err := os.MkdirAll(filepath.Join(root, category, "p", "j"), 0o755); err != nil {
			t.Fatal(err)
		}
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
	a.sweepJobData(context.Background())
	for _, category := range []string{"work", "results", "logs", "uploads"} {
		if _, err := os.Stat(filepath.Join(root, category, "p", "j")); err != nil {
			t.Fatalf("%s directory removed before executor returned: %v", category, err)
		}
	}
	close(executor.allowReturn)
	deadline := time.After(time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "uploads", "p", "j")); os.IsNotExist(err) {
			break
		}
		select {
		case <-deadline:
			t.Fatal("aborted Job work directory was not cleaned")
		case <-time.After(time.Millisecond):
		}
	}
	if _, err := os.Stat(workPath); !os.IsNotExist(err) {
		t.Fatalf("work directory retained: %v", err)
	}
	current, _ := api.GetJob(ctx, "p", "j")
	if current.Status.Phase != "Aborted" {
		t.Fatal("abort overwritten")
	}
}

type waitingArtifactRemote struct {
	fakeArtifactRemote
	started, cancelled, allowReturn chan struct{}
}

func (r *waitingArtifactRemote) UploadArtifact(ctx context.Context, _, _, _, _ string, _ artifact.UploadArtifactInput) (artifact.ArtifactRecord, error) {
	close(r.started)
	<-ctx.Done()
	close(r.cancelled)
	<-r.allowReturn
	return artifact.ArtifactRecord{}, ctx.Err()
}

func TestAbortCancelsPostRunWithoutFinalStatusWrite(t *testing.T) {
	job := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "j", Namespace: "p", UID: "u"}, Status: gateway.JobStatus{Phase: "Running", Runner: "r", Stage: "PostRun"}}
	api := &lifecycleAPI{job: job}
	remote := &waitingArtifactRemote{started: make(chan struct{}), cancelled: make(chan struct{}), allowReturn: make(chan struct{})}
	root := t.TempDir()
	for _, category := range []string{"results", "logs", "uploads"} {
		if err := os.MkdirAll(filepath.Join(root, category, "p", "j"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "results", "p", "j", "package.rpm"), []byte("rpm"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Agent{
		cfg: options.Config{
			Name:     "r",
			RootDir:  root,
			Artifact: artifact.Config{Upload: artifact.UploadConfig{Timeout: time.Minute}},
		},
		client:    api,
		artifacts: &artifact.Processor{Remote: remote, RootDir: root},
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
	case <-remote.cancelled:
	case <-time.After(time.Second):
		t.Fatal("upload not cancelled")
	}
	a.sweepJobData(context.Background())
	for _, category := range []string{"results", "logs", "uploads"} {
		if _, err := os.Stat(filepath.Join(root, category, "p", "j")); err != nil {
			t.Fatalf("%s removed before upload returned: %v", category, err)
		}
	}
	close(remote.allowReturn)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("upload not cancelled")
	}
	for _, category := range []string{"results", "logs", "uploads"} {
		if _, err := os.Stat(filepath.Join(root, category, "p", "j")); !os.IsNotExist(err) {
			t.Fatalf("%s retained after upload returned: %v", category, err)
		}
	}
	api.lock.Lock()
	defer api.lock.Unlock()
	if api.job.Status.Phase != "Aborted" {
		t.Fatal("terminal result overwritten")
	}
}

func TestUploadFailurePreservesScriptResult(t *testing.T) {
	root := t.TempDir()
	resultDir := filepath.Join(root, "results", "project", "job")
	if err := os.MkdirAll(resultDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resultDir, "large.rpm"), []byte("too large"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := gateway.JobResource{
		Metadata: gateway.ObjectMeta{Name: "job", Namespace: "project", UID: "uid"},
		Status: gateway.JobStatus{Phase: "Running", Stage: "PostRun", Runner: "runner-a",
			Result: &gateway.JobResult{Status: "Succeeded"}},
	}
	api := &fakeRunnerAPI{}
	agent := &Agent{
		cfg:       options.Config{RootDir: root, Artifact: artifact.Config{Upload: artifact.UploadConfig{Timeout: time.Second}}},
		client:    api,
		artifacts: &artifact.Processor{Remote: &fakeArtifactRemote{}, RootDir: root, MaxFileSize: 1, MaxJobSize: 2048, MaxFiles: 10, Concurrency: 1},
		cleanup:   &artifact.CleanupManager{RootDir: root, FailedRetention: 24 * time.Hour},
	}
	agent.finalizeArtifacts(context.Background(), job, job.Status, resultDir, nil)
	if len(api.jobStatuses) != 1 {
		t.Fatalf("status writes = %d", len(api.jobStatuses))
	}
	final := api.jobStatuses[0]
	if final.Phase != "Failed" || final.Result == nil || final.Result.Status != "Succeeded" {
		t.Fatalf("upload failure changed script result: %+v", final)
	}
}
