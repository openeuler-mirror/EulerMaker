package runner

import (
	"context"
	"errors"
	"fmt"
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

type postRunLogRemote struct {
	fakeArtifactRemote
	mu            sync.Mutex
	status        artifact.LogStatus
	failOperation string
	failures      int
	permanent     bool
	manifestFiles int
	deadline      time.Time
}

func (r *postRunLogRemote) fail(operation string) error {
	if r.failOperation != operation || r.failures == 0 {
		return nil
	}
	r.failures--
	if r.permanent {
		return artifact.APIError{StatusCode: 422, Code: "DigestMismatch"}
	}
	return artifact.APIError{StatusCode: 503, Code: "Unavailable", Retryable: true}
}

func (r *postRunLogRemote) LogStatus(context.Context, string, string) (artifact.LogStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status, r.fail("status")
}

func (r *postRunLogRemote) AppendLog(_ context.Context, _, _ string, seq int64, _ string, data []byte) (artifact.AppendLogResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.fail("append"); err != nil {
		return artifact.AppendLogResult{}, err
	}
	if seq != r.status.NextSequence {
		return artifact.AppendLogResult{}, fmt.Errorf("unexpected sequence %d", seq)
	}
	r.status.NextSequence++
	r.status.CommittedBytes += int64(len(data))
	return artifact.AppendLogResult{NextSequence: r.status.NextSequence, CommittedBytes: r.status.CommittedBytes}, nil
}

func (r *postRunLogRemote) CompleteLog(ctx context.Context, _, _, _ string, in artifact.CompleteLogInput) (artifact.CompletedLog, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deadline, _ = ctx.Deadline()
	if err := r.fail("complete"); err != nil {
		return artifact.CompletedLog{}, err
	}
	r.status.State, r.status.ArtifactID = "Completed", "log-1"
	r.status.FinalSize, r.status.FinalSHA256 = &in.Size, in.SHA256
	return artifact.CompletedLog{State: "Completed", ArtifactID: "log-1", Size: in.Size, SHA256: in.SHA256}, nil
}

func (r *postRunLogRemote) CompleteManifest(ctx context.Context, project, job string, in artifact.CompleteManifestInput) (artifact.CompletedManifest, error) {
	deadline, ok := ctx.Deadline()
	if !ok || !deadline.Equal(r.deadline) {
		return artifact.CompletedManifest{}, artifact.APIError{StatusCode: 422, Code: "PostRunDeadlineChanged"}
	}
	r.manifestFiles = len(in.Files)
	return r.fakeArtifactRemote.CompleteManifest(ctx, project, job, in)
}

func TestPostRunLogCompletion(t *testing.T) {
	for _, test := range []struct {
		name, operation, executionError, wantPhase string
		failures                                   int
		permanent, cancel, fresh                   bool
	}{
		{name: "status outage", operation: "status", failures: 1, wantPhase: "Succeeded"},
		{name: "append outage", operation: "append", failures: 1, wantPhase: "Succeeded"},
		{name: "seal outage", operation: "complete", failures: 1, wantPhase: "Succeeded"},
		{name: "normal execution", operation: "complete", failures: 1, fresh: true, wantPhase: "Succeeded"},
		{name: "execution failed", operation: "complete", failures: 1, executionError: "exit failure", wantPhase: "Failed"},
		{name: "upload deadline", operation: "complete", failures: 100, wantPhase: "Failed"},
		{name: "invalid log", operation: "complete", failures: 1, permanent: true, wantPhase: "Failed"},
		{name: "shutdown", operation: "complete", failures: 100, cancel: true, wantPhase: "Running"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			job := completionJob()
			spool := filepath.Join(root, "logs", "p", "j", "combined.log")
			for _, path := range []string{filepath.Dir(spool), filepath.Join(root, "results", "p", "j")} {
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(spool, []byte("build output\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "results", "p", "j", "package.rpm"), []byte("rpm"), 0600); err != nil {
				t.Fatal(err)
			}
			var executionErr error
			if test.executionError != "" {
				executionErr = errors.New(test.executionError)
			}
			if err := recordExecution(root, job, executionErr, true); err != nil {
				t.Fatal(err)
			}
			record, err := loadCompletion(root, job)
			if err != nil {
				t.Fatal(err)
			}
			remote := &postRunLogRemote{
				status: artifact.LogStatus{State: "Open"}, failOperation: test.operation,
				failures: test.failures, permanent: test.permanent,
			}
			api := &lifecycleAPI{job: job}
			timeout := 3 * time.Second
			if test.failures == 100 {
				timeout = 50 * time.Millisecond
			}
			a := &Agent{
				cfg: options.Config{RootDir: root, Artifact: artifact.Config{
					Log: artifact.LogConfig{DrainTimeout: time.Millisecond}, Upload: artifact.UploadConfig{Timeout: timeout},
				}},
				client:     api,
				logFactory: &artifact.LogFactory{Remote: remote, RootDir: root, RetryMaxBackoff: time.Millisecond},
				artifacts:  &artifact.Processor{Remote: remote, RootDir: root},
				cleanup:    &artifact.CleanupManager{RootDir: root, FailedRetention: 24 * time.Hour},
			}
			ctx := context.Background()
			if test.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 10*time.Millisecond)
				defer cancel()
			}
			if test.fresh {
				a.executor = &fakeExecutor{resultRoot: filepath.Join(root, "results", "p", "j")}
				a.runJob(ctx, "u", job)
			} else {
				a.resumeCompletion(ctx, "u", job, record)
			}
			if api.job.Status.Phase != test.wantPhase || api.job.Status.Stage != "PostRun" {
				t.Fatalf("status=%+v", api.job.Status)
			}
			if test.wantPhase == "Succeeded" || test.executionError != "" {
				wantFiles := 2
				if test.executionError != "" {
					wantFiles = 1
				}
				if remote.status.State != "Completed" || remote.manifestFiles != wantFiles || api.job.Status.Message != test.executionError {
					t.Fatalf("log=%+v files=%d status=%+v", remote.status, remote.manifestFiles, api.job.Status)
				}
			} else {
				if remote.manifestFiles != 0 {
					t.Fatal("manifest submitted before log completion")
				}
				if _, err := os.Stat(spool); err != nil {
					t.Fatalf("log spool not retained: %v", err)
				}
				if test.cancel {
					pending, err := loadCompletion(root, job)
					if err != nil || pending == nil || !pending.LogsPending {
						t.Fatalf("lost pending completion: %+v, %v", pending, err)
					}
					remote.failures = 0
					a.cfg.Artifact.Upload.Timeout = 3 * time.Second
					a.resumeCompletion(context.Background(), "u", api.job, pending)
					if api.job.Status.Phase != "Succeeded" || remote.manifestFiles != 2 {
						t.Fatalf("resume after shutdown: status=%+v files=%d", api.job.Status, remote.manifestFiles)
					}
				} else if !strings.Contains(api.job.Status.Message, "complete artifact log") {
					t.Fatalf("missing upload failure: %+v", api.job.Status)
				}
			}
		})
	}
}

func completionJob() gateway.JobResource {
	return gateway.JobResource{Metadata: gateway.ObjectMeta{Namespace: "p", Name: "j", UID: "u", ResourceVersion: "1"}, Status: gateway.JobStatus{Runner: "r", Phase: "Running", Stage: "Running"}}
}

func TestRecoverExecutionWhileServerStillRunning(t *testing.T) {
	root := t.TempDir()
	job := completionJob()
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	job.Status.StartTime = &start
	if err := os.MkdirAll(filepath.Join(root, "results", "p", "j"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := recordExecution(root, job, errors.New("autoconf check failed"), false); err != nil {
		t.Fatal(err)
	}
	record, err := loadCompletion(root, job)
	if err != nil {
		t.Fatal(err)
	}
	api := &lifecycleAPI{job: job}
	exec := &fakeExecutor{}
	a := &Agent{
		cfg:    options.Config{RootDir: root, Artifact: artifact.Config{Upload: artifact.UploadConfig{Timeout: time.Second}}},
		client: api, executor: exec,
		artifacts: &artifact.Processor{Remote: &fakeArtifactRemote{}, RootDir: root, MaxFileSize: 1024, MaxJobSize: 2048, MaxFiles: 10, Concurrency: 1},
		cleanup:   &artifact.CleanupManager{RootDir: root},
	}
	a.resumeCompletion(context.Background(), "u", job, record)
	if exec.called || api.job.Status.Phase != "Failed" || api.job.Status.Message != "autoconf check failed" || !api.job.Status.StartTime.Equal(start) {
		t.Fatalf("executed=%v status=%+v", exec.called, api.job.Status)
	}
}

func TestCompletionSurvivesFailedWriteAndRestart(t *testing.T) {
	root := t.TempDir()
	job := completionJob()
	end := time.Now().UTC().Truncate(time.Second)
	status := job.Status
	status.Phase, status.Stage, status.Message, status.EndTime = "Failed", "PostRun", "container exited with code 1", &end
	record := completionRecord{Version: 1, UID: "u", Runner: "r", Status: status, FinishedAt: end}
	if err := saveCompletion(root, job, record); err != nil {
		t.Fatal(err)
	}
	api := &lifecycleAPI{job: job, writeErr: gateway.StatusError{Code: 403}}
	a := &Agent{cfg: options.Config{RootDir: root}, client: api}
	if err := a.writeCompletionStatus(context.Background(), &job, status); err == nil {
		t.Fatal("expected write failure")
	}
	restored, err := loadCompletion(root, job)
	if err != nil || restored == nil {
		t.Fatalf("lost completion: %v", err)
	}
	api.writeErr = nil
	exec := &fakeExecutor{}
	restarted := &Agent{cfg: options.Config{RootDir: root}, client: api, executor: exec, cleanup: &artifact.CleanupManager{RootDir: root}}
	restarted.resumeCompletion(context.Background(), "u", job, restored)
	if exec.called {
		t.Fatal("executed completed job again")
	}
	if api.job.Status.Phase != "Failed" || api.job.Status.Message != status.Message || !api.job.Status.EndTime.Equal(end) {
		t.Fatalf("lost final intent: %+v", api.job.Status)
	}
	if got, err := loadCompletion(root, job); err != nil || got != nil {
		t.Fatalf("completion not cleaned: %v %v", got, err)
	}
}

type retryCompletionAPI struct {
	lifecycleAPI
	writes int
}

func (f *retryCompletionAPI) UpdateJobStatus(ctx context.Context, job gateway.JobResource, status gateway.JobStatus) (*gateway.JobResource, error) {
	f.writes++
	if f.writes == 1 {
		f.job.Metadata.ResourceVersion = "2"
		return nil, gateway.StatusError{Code: 502}
	}
	if job.Metadata.ResourceVersion != "2" {
		return nil, errors.New("stale resourceVersion")
	}
	return f.lifecycleAPI.UpdateJobStatus(ctx, job, status)
}

func TestCompletionRetriesWithFreshVersion(t *testing.T) {
	job := completionJob()
	api := &retryCompletionAPI{lifecycleAPI: lifecycleAPI{job: job}}
	a := &Agent{client: api}
	status := job.Status
	status.Stage = "PostRun"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.writeCompletionStatus(ctx, &job, status); err != nil {
		t.Fatal(err)
	}
	if api.writes != 2 || api.job.Status.Stage != "PostRun" {
		t.Fatalf("writes=%d status=%+v", api.writes, api.job.Status)
	}
}

func TestCompletionDoesNotOverwriteChangedJob(t *testing.T) {
	for _, change := range []string{"abort", "uid", "runner", "deletion"} {
		t.Run(change, func(t *testing.T) {
			job := completionJob()
			current := job
			switch change {
			case "abort":
				current.Status.Phase = "Aborted"
			case "uid":
				current.Metadata.UID = "replacement"
			case "runner":
				current.Status.Runner = "other"
			case "deletion":
				now := time.Now()
				current.Metadata.DeletionTimestamp = &now
			}
			api := &lifecycleAPI{job: current, writeErr: gateway.StatusError{Code: 409}}
			a := &Agent{client: api}
			status := job.Status
			status.Stage = "PostRun"
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := a.writeCompletionStatus(ctx, &job, status); err == nil {
				t.Fatal("accepted stale completion")
			}
			if api.job.Status.Stage == "PostRun" {
				t.Fatal("overwrote changed job")
			}
		})
	}
}

func TestCompletionLostResponseConfirmed(t *testing.T) {
	job := completionJob()
	api := &lifecycleAPI{job: job, writeErr: errors.New("lost response"), confirmIntent: true}
	a := &Agent{client: api}
	status := job.Status
	status.Stage, status.Phase = "PostRun", "Failed"
	if err := a.writeCompletionStatus(context.Background(), &job, status); err != nil {
		t.Fatal(err)
	}
}

func TestCompletionRejectsCorruptRecord(t *testing.T) {
	root := t.TempDir()
	job := completionJob()
	if err := recordExecution(root, job, errors.New("exit failure"), true); err != nil {
		t.Fatal(err)
	}
	record, err := loadCompletion(root, job)
	if err != nil || !record.LogsPending || record.Status.Message != "exit failure" {
		t.Fatalf("record=%+v err=%v", record, err)
	}
	path, _ := completionPath(root, job)
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCompletion(root, job); err == nil {
		t.Fatal("accepted corrupt record")
	}
	job.Metadata.UID = "new-uid"
	if record, err := loadCompletion(root, job); err != nil || record != nil {
		t.Fatalf("reused old UID record: %v %v", record, err)
	}
}
