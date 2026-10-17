package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"ebs-runner/pkg/gateway"
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
	return &job, nil
}

func TestSweepJobWork(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"aborted", "deleted", "running", "active"} {
		path := filepath.Join(root, "work", "project", name)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	api := &workCleanupAPI{jobs: map[string]gateway.JobResource{
		"project/aborted": {Status: gateway.JobStatus{Phase: "Aborted"}},
		"project/running": {Status: gateway.JobStatus{Phase: "Running"}},
		"project/active":  {Status: gateway.JobStatus{Phase: "Aborted"}},
	}}
	a := &Agent{cfg: Config{RootDir: root}, client: api, executions: map[string]*jobExecution{
		"uid": {job: gateway.JobResource{Metadata: gateway.ObjectMeta{Namespace: "project", Name: "active"}}},
	}}
	a.sweepJobWork(context.Background())
	for name, wantPresent := range map[string]bool{
		"aborted": false, "deleted": false, "running": true, "active": true,
	} {
		_, err := os.Stat(filepath.Join(root, "work", "project", name))
		if (err == nil) != wantPresent {
			t.Errorf("work directory %s: err=%v, want present=%t", name, err, wantPresent)
		}
	}
	api.err = errors.New("gateway unavailable")
	a.sweepJobWork(context.Background())
	if _, err := os.Stat(filepath.Join(root, "work", "project", "running")); err != nil {
		t.Fatalf("work directory removed when Job status could not be checked: %v", err)
	}
}
