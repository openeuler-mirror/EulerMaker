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

type workCleanupAPI struct {
	fakeRunnerAPI
	jobs map[string]gateway.JobResource
	err  error
}

func (f *workCleanupAPI) GetJob(_ context.Context, project, name string) (*gateway.JobResource, error) {
	if f.err != nil {
		return nil, f.err
	}
	job, ok := f.jobs[project+"/"+name]
	if !ok {
		return nil, gateway.StatusError{Code: 404}
	}
	job.Metadata.Namespace, job.Metadata.Name = project, name
	return &job, nil
}

func TestSweepJobData(t *testing.T) {
	root := t.TempDir()
	categories := []string{"work", "results", "logs", "uploads"}
	for _, category := range categories {
		for _, name := range []string{"aborted", "deleted", "running", "active", "failed", "success", "failed-recent", "failed-expired"} {
			path := filepath.Join(root, category, "project", name)
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	// No work directory remains for this Job; other categories must still be discovered.
	if err := os.MkdirAll(filepath.Join(root, "uploads", "project", "upload-only"), 0o755); err != nil {
		t.Fatal(err)
	}
	recent, expired := time.Now().Add(-time.Hour), time.Now().Add(-25*time.Hour)
	api := &workCleanupAPI{jobs: map[string]gateway.JobResource{
		"project/aborted":        {Status: gateway.JobStatus{Phase: "Aborted"}},
		"project/running":        {Status: gateway.JobStatus{Phase: "Running"}},
		"project/active":         {Status: gateway.JobStatus{Phase: "Aborted"}},
		"project/failed":         {Status: gateway.JobStatus{Phase: "Failed"}},
		"project/success":        {Status: gateway.JobStatus{Phase: "Succeeded"}},
		"project/failed-recent":  {Status: gateway.JobStatus{Phase: "Failed", EndTime: &recent}},
		"project/failed-expired": {Status: gateway.JobStatus{Phase: "Failed", EndTime: &expired}},
	}}
	a := &Agent{cfg: options.Config{RootDir: root}, client: api, executions: map[string]*jobExecution{
		"uid": {job: gateway.JobResource{Metadata: gateway.ObjectMeta{Namespace: "project", Name: "active"}}},
	}}
	cleaner := artifact.CleanupManager{RootDir: root, FailedRetention: 24 * time.Hour}
	a.cleanup = &cleaner
	for _, name := range []string{"aborted", "failed"} {
		job := gateway.JobResource{Metadata: gateway.ObjectMeta{Namespace: "project", Name: name, UID: name}}
		if err := cleaner.MarkFailure(job); err != nil {
			t.Fatal(err)
		}
	}
	a.sweepJobData(context.Background())
	for name, present := range map[string]bool{"aborted": false, "failed": true} {
		_, err := os.Stat(filepath.Join(root, "cleanup", "project", name+".json"))
		if (err == nil) != present {
			t.Fatalf("cleanup marker %s: %v", name, err)
		}
	}
	for _, category := range categories {
		for name, wantPresent := range map[string]bool{
			"aborted": false, "deleted": false, "running": true, "active": true,
			"failed": category != "work", "upload-only": false,
			"success": false, "failed-recent": category != "work", "failed-expired": false,
		} {
			_, err := os.Stat(filepath.Join(root, category, "project", name))
			if (err == nil) != wantPresent {
				t.Errorf("directory %s/%s: err=%v, want present=%t", category, name, err, wantPresent)
			}
		}
	}
	api.err = errors.New("gateway unavailable")
	a.sweepJobData(context.Background())
	for _, category := range categories {
		if _, err := os.Stat(filepath.Join(root, category, "project", "running")); err != nil {
			t.Fatalf("%s directory removed when Job status could not be checked: %v", category, err)
		}
	}
}

type blockingCleanupAPI struct {
	fakeRunnerAPI
	started, release chan struct{}
	err              error
}

func (f *blockingCleanupAPI) GetJob(_ context.Context, project, name string) (*gateway.JobResource, error) {
	close(f.started)
	<-f.release
	return &gateway.JobResource{
		Metadata: gateway.ObjectMeta{Namespace: project, Name: name},
		Status:   gateway.JobStatus{Phase: "Aborted"},
	}, f.err
}

func TestJobCleanupReservation(t *testing.T) {
	for _, outcome := range []string{"success", "query-error", "delete-error"} {
		api := &blockingCleanupAPI{started: make(chan struct{}), release: make(chan struct{})}
		if outcome == "query-error" {
			api.err = errors.New("gateway unavailable")
		}
		a := &Agent{cfg: options.Config{RootDir: t.TempDir()}, client: api}
		if outcome == "delete-error" {
			if err := os.WriteFile(filepath.Join(a.cfg.RootDir, "results"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		done := make(chan struct{})
		go func() { a.cleanupInactiveJob(context.Background(), "project", "job"); close(done) }()
		select {
		case <-api.started:
		case <-time.After(time.Second):
			close(api.release)
			t.Fatal("cleanup did not start")
		}
		checks := make(chan bool, 1)
		go func() {
			job := gateway.JobResource{Metadata: gateway.ObjectMeta{Namespace: "project", Name: "job", UID: "new-uid"}}
			_, _, sameStarted := a.registerExecution(context.Background(), job)
			duplicate := a.beginJobCleanup(localJobKey{"project", "job"})
			job.Metadata.Name, job.Metadata.UID = "other", "other-uid"
			ctx, _, otherStarted := a.registerExecution(context.Background(), job)
			a.cancelExecution("other-uid")
			checks <- !sameStarted && !duplicate && otherStarted && ctx.Err() != nil
		}()
		select {
		case ok := <-checks:
			if !ok {
				close(api.release)
				t.Fatal("cleanup did not isolate only the reserved Job")
			}
		case <-time.After(time.Second):
			close(api.release)
			t.Fatal("cleanup held the Agent lock")
		}
		close(api.release)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("cleanup did not finish")
		}
		if !a.beginJobCleanup(localJobKey{"project", "job"}) {
			t.Fatalf("cleanup reservation retained after completion (%s)", outcome)
		}
		a.endJobCleanup(localJobKey{"project", "job"})
	}
}
