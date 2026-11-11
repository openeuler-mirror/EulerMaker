package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ebs-runner/pkg/artifact"
	"ebs-runner/pkg/gateway"
	"ebs-runner/pkg/options"
)

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
