package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDCExecutorCreatesContainerWithPayloadFile(t *testing.T) {
	dir := t.TempDir()
	docker := &fakeDockerRuntime{exitCode: 0}
	runtimeSpec := mustJSON(t, DCRuntimeSpec{
		Image:       "openeuler:22.03",
		NetworkMode: "bridge",
		WorkingDir:  "/workspace",
		Env: map[string]string{
			"BUILD_ENV": "production",
		},
		Mounts: []DCMount{
			{Name: "work", MountPath: "/workspace"},
			{Name: "results", MountPath: "/results"},
		},
	})
	executor := &DCExecutor{
		WorkDir:    filepath.Join(dir, "work"),
		ResultRoot: filepath.Join(dir, "results"),
		RunnerName: "runner-a",
		Docker:     docker,
	}
	job := JobResource{
		Metadata: ObjectMeta{Name: "job-a", Namespace: "project-a"},
		Spec: JobSpec{
			RuntimeSpec: runtimeSpec,
			Payload:     "build:\n  target: rpm\n",
		},
	}

	resultRoot, err := executor.Execute(context.Background(), job)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if resultRoot != filepath.Join(dir, "results", "project-a", "job-a") {
		t.Fatalf("result root = %s", resultRoot)
	}
	payload, err := os.ReadFile(filepath.Join(dir, "work", "project-a", "job-a", "payload.yaml"))
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if string(payload) != "build:\n  target: rpm\n" {
		t.Fatalf("payload = %q", string(payload))
	}
	if docker.created.Image != "openeuler:22.03" {
		t.Fatalf("image = %q", docker.created.Image)
	}
	if docker.created.Labels["ebs.io/project"] != "project-a" || docker.created.Labels["ebs.io/job"] != "job-a" || docker.created.Labels["ebs.io/runner"] != "runner-a" {
		t.Fatalf("labels = %#v", docker.created.Labels)
	}
	if docker.created.Mounts[filepath.Join(dir, "work", "project-a", "job-a")] != "/workspace" {
		t.Fatalf("work mount = %#v", docker.created.Mounts)
	}
	if docker.created.Mounts[filepath.Join(dir, "results", "project-a", "job-a")] != "/results" {
		t.Fatalf("result mount = %#v", docker.created.Mounts)
	}
	if !docker.started || !docker.removed {
		t.Fatalf("expected container start and cleanup, started=%v removed=%v", docker.started, docker.removed)
	}
	logData, err := os.ReadFile(filepath.Join(resultRoot, "container.log"))
	if err != nil {
		t.Fatalf("read container log: %v", err)
	}
	if string(logData) != "container log\n" {
		t.Fatalf("container log = %q", string(logData))
	}
}

func TestDCExecutorReturnsContainerExitCode(t *testing.T) {
	dir := t.TempDir()
	docker := &fakeDockerRuntime{exitCode: 7}
	executor := &DCExecutor{
		WorkDir:    filepath.Join(dir, "work"),
		ResultRoot: filepath.Join(dir, "results"),
		Docker:     docker,
	}
	job := JobResource{
		Metadata: ObjectMeta{Name: "job-a", Namespace: "project-a"},
		Spec:     JobSpec{RuntimeSpec: mustJSON(t, DCRuntimeSpec{Image: "openeuler:22.03"})},
	}

	_, err := executor.Execute(context.Background(), job)
	if err == nil || err.Error() != "container exited with code 7" {
		t.Fatalf("expected exit code error, got %v", err)
	}
}

func TestDCExecutorStopsContainerOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	docker := &fakeDockerRuntime{waitBlock: make(chan struct{})}
	executor := &DCExecutor{
		WorkDir:         filepath.Join(dir, "work"),
		ResultRoot:      filepath.Join(dir, "results"),
		Docker:          docker,
		StopGracePeriod: time.Millisecond,
	}
	job := JobResource{
		Metadata: ObjectMeta{Name: "job-a", Namespace: "project-a"},
		Spec:     JobSpec{RuntimeSpec: mustJSON(t, DCRuntimeSpec{Image: "openeuler:22.03"})},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := executor.Execute(ctx, job)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got %v", err)
	}
	if !docker.stopped {
		t.Fatalf("expected container stop")
	}
}

func TestRuntimeManagerDispatchesDC(t *testing.T) {
	executor := &fakeExecutor{resultRoot: "/results/project/job"}
	manager := &RuntimeManager{
		RunnerType: "dc",
		Executors:  map[string]Executor{"dc": executor},
	}

	resultRoot, err := manager.Execute(context.Background(), JobResource{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if resultRoot != "/results/project/job" {
		t.Fatalf("result root = %s", resultRoot)
	}
	if !executor.called {
		t.Fatalf("expected executor call")
	}
}

func TestRuntimeManagerRejectsMismatchedRuntime(t *testing.T) {
	manager := &RuntimeManager{
		RunnerType: "dc",
		Executors:  map[string]Executor{"dc": &fakeExecutor{}},
	}

	_, err := manager.Execute(context.Background(), JobResource{Spec: JobSpec{Runtime: "vm"}})
	if err == nil {
		t.Fatalf("expected runtime mismatch error")
	}
}

type fakeDockerRuntime struct {
	created   ContainerSpec
	started   bool
	stopped   bool
	removed   bool
	pulled    bool
	exitCode  int
	waitBlock chan struct{}
}

func (f *fakeDockerRuntime) ImageExists(context.Context, string) (bool, error) {
	return !f.pulled, nil
}

func (f *fakeDockerRuntime) Pull(context.Context, string) error {
	f.pulled = true
	return nil
}

func (f *fakeDockerRuntime) Remove(context.Context, string) error {
	f.removed = true
	return nil
}

func (f *fakeDockerRuntime) Create(_ context.Context, spec ContainerSpec) (string, error) {
	f.created = spec
	return "container-a", nil
}

func (f *fakeDockerRuntime) Start(context.Context, string) error {
	f.started = true
	return nil
}

func (f *fakeDockerRuntime) Logs(ctx context.Context, _ string, output io.Writer) error {
	_, _ = io.WriteString(output, "container log\n")
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeDockerRuntime) Wait(context.Context, string) (int, error) {
	if f.waitBlock != nil {
		<-f.waitBlock
	}
	return f.exitCode, nil
}

func (f *fakeDockerRuntime) Stop(context.Context, string, time.Duration) error {
	f.stopped = true
	if f.waitBlock != nil {
		close(f.waitBlock)
		f.waitBlock = nil
	}
	return nil
}

func (f *fakeDockerRuntime) Kill(context.Context, string) error {
	if f.waitBlock != nil {
		close(f.waitBlock)
		f.waitBlock = nil
	}
	return nil
}

type fakeExecutor struct {
	called     bool
	resultRoot string
	err        error
}

func (f *fakeExecutor) Execute(context.Context, JobResource) (string, error) {
	f.called = true
	return f.resultRoot, f.err
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	return data
}
