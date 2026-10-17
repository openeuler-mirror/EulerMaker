package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	containerruntime "ebs-runner/pkg/container"
	"ebs-runner/pkg/gateway"
	"ebs-runner/pkg/options"
)

type recoveryRuntime struct {
	containerruntime.ManagedRuntime
	filters map[string]string
	stopped []string
	removed []string
}

func (r *recoveryRuntime) List(_ context.Context, labels map[string]string) ([]string, error) {
	r.filters = labels
	return []string{"managed"}, nil
}

func (*recoveryRuntime) Labels(context.Context, string) (map[string]string, error) {
	return map[string]string{"ebs.io/runner": "runner-a", "ebs.io/project": "project-a", "ebs.io/job": "job-a", "ebs.io/job-uid": "uid-a"}, nil
}

func (r *recoveryRuntime) Stop(_ context.Context, id string, _ time.Duration) error {
	r.stopped = append(r.stopped, id)
	return nil
}

func (r *recoveryRuntime) Remove(_ context.Context, id string) error {
	r.removed = append(r.removed, id)
	return nil
}

type recoveryAPI struct {
	RunnerAPI
	project string
	name    string
}

func (a *recoveryAPI) GetJob(_ context.Context, project, name string) (*gateway.JobResource, error) {
	a.project, a.name = project, name
	return &gateway.JobResource{Status: gateway.JobStatus{Phase: "Running"}}, nil
}

func TestRecoveryUsesSelectedRuntime(t *testing.T) {
	runtime := &recoveryRuntime{}
	client := &recoveryAPI{}
	agent := &Agent{
		cfg: options.Config{
			Type: "ct",
			Name: "runner-a",
		},
		containers: runtime,
		client:     client,
	}
	if err := agent.recoverContainers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(runtime.filters, map[string]string{"ebs.io/runner": "runner-a", "ebs.io/job-uid": ""}) {
		t.Fatalf("recovery filters = %v", runtime.filters)
	}
	if client.project != "project-a" || client.name != "job-a" {
		t.Fatalf("confirmed Job = %s/%s", client.project, client.name)
	}
	if !reflect.DeepEqual(runtime.stopped, []string{"managed"}) || !reflect.DeepEqual(runtime.removed, []string{"managed"}) {
		t.Fatalf("stopped = %v, removed = %v", runtime.stopped, runtime.removed)
	}
}

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
	a := &Agent{cfg: options.Config{RootDir: root}, client: api, executions: map[string]*jobExecution{
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
