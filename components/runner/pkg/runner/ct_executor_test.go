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

func TestCTExecutorCreatesContainerWithPayloadFile(t *testing.T) {
	dir := t.TempDir()
	container := &fakeContainerRuntime{exitCode: 0}
	runtimeSpec := mustJSON(t, ContainerRuntimeSpec{
		Image:       "openeuler:22.03",
		NetworkMode: "bridge",
		WorkingDir:  "/workspace",
		Env: map[string]string{
			"BUILD_ENV": "production",
		},
		Mounts: []ContainerMount{
			{Name: "work", MountPath: "/workspace"},
			{Name: "results", MountPath: "/results"},
		},
	})
	executor := &CTExecutor{
		WorkDir:    filepath.Join(dir, "work"),
		ResultRoot: filepath.Join(dir, "results"),
		RunnerName: "runner-a",
		Runtime:    container,
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
	if container.created.Image != "openeuler:22.03" {
		t.Fatalf("image = %q", container.created.Image)
	}
	if container.created.Labels["ebs.io/project"] != "project-a" || container.created.Labels["ebs.io/job"] != "job-a" || container.created.Labels["ebs.io/runner"] != "runner-a" {
		t.Fatalf("labels = %#v", container.created.Labels)
	}
	if container.created.Mounts[filepath.Join(dir, "work", "project-a", "job-a")] != "/workspace" {
		t.Fatalf("work mount = %#v", container.created.Mounts)
	}
	if container.created.Mounts[filepath.Join(dir, "results", "project-a", "job-a")] != "/results" {
		t.Fatalf("result mount = %#v", container.created.Mounts)
	}
	if !container.started || !container.removed {
		t.Fatalf("expected container start and cleanup, started=%v removed=%v", container.started, container.removed)
	}
	logData, err := os.ReadFile(filepath.Join(resultRoot, "container.log"))
	if err != nil {
		t.Fatalf("read container log: %v", err)
	}
	if string(logData) != "container log\n" {
		t.Fatalf("container log = %q", string(logData))
	}
}

func TestCTExecutorReturnsContainerExitCode(t *testing.T) {
	dir := t.TempDir()
	container := &fakeContainerRuntime{exitCode: 7}
	executor := &CTExecutor{
		WorkDir:    filepath.Join(dir, "work"),
		ResultRoot: filepath.Join(dir, "results"),
		Runtime:    container,
	}
	job := JobResource{
		Metadata: ObjectMeta{Name: "job-a", Namespace: "project-a"},
		Spec:     JobSpec{RuntimeSpec: mustJSON(t, ContainerRuntimeSpec{Image: "openeuler:22.03"})},
	}

	_, err := executor.Execute(context.Background(), job)
	if err == nil || err.Error() != "container exited with code 7" {
		t.Fatalf("expected exit code error, got %v", err)
	}
}

func TestCTExecutorStopsContainerOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	container := &fakeContainerRuntime{waitBlock: make(chan struct{})}
	executor := &CTExecutor{
		WorkDir:         filepath.Join(dir, "work"),
		ResultRoot:      filepath.Join(dir, "results"),
		Runtime:         container,
		StopGracePeriod: time.Millisecond,
	}
	job := JobResource{
		Metadata: ObjectMeta{Name: "job-a", Namespace: "project-a"},
		Spec:     JobSpec{RuntimeSpec: mustJSON(t, ContainerRuntimeSpec{Image: "openeuler:22.03"})},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := executor.Execute(ctx, job)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got %v", err)
	}
	if !container.stopped {
		t.Fatalf("expected container stop")
	}
}

func TestRuntimeManagerDispatchesCT(t *testing.T) {
	executor := &fakeExecutor{resultRoot: "/results/project/job"}
	manager := &RuntimeManager{
		RunnerType: "ct",
		Executors:  map[string]Executor{"ct": executor},
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
		RunnerType: "ct",
		Executors:  map[string]Executor{"ct": &fakeExecutor{}},
	}

	_, err := manager.Execute(context.Background(), JobResource{Spec: JobSpec{Runtime: "vm"}})
	if err == nil {
		t.Fatalf("expected runtime mismatch error")
	}
}

type fakeContainerRuntime struct {
	created   ContainerSpec
	started   bool
	stopped   bool
	removed   bool
	pulled    bool
	exitCode  int
	waitBlock chan struct{}
}

func (f *fakeContainerRuntime) ImageExists(context.Context, string) (bool, error) {
	return !f.pulled, nil
}

func (f *fakeContainerRuntime) Pull(context.Context, string) error {
	f.pulled = true
	return nil
}

func (f *fakeContainerRuntime) Remove(context.Context, string) error {
	f.removed = true
	return nil
}

func (f *fakeContainerRuntime) Create(_ context.Context, spec ContainerSpec) (string, error) {
	f.created = spec
	return "container-a", nil
}

func (f *fakeContainerRuntime) Start(context.Context, string) error {
	f.started = true
	return nil
}

func (f *fakeContainerRuntime) Logs(ctx context.Context, _ string, output io.Writer) error {
	_, _ = io.WriteString(output, "container log\n")
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeContainerRuntime) Wait(context.Context, string) (int, error) {
	if f.waitBlock != nil {
		<-f.waitBlock
	}
	return f.exitCode, nil
}

func (f *fakeContainerRuntime) Stop(context.Context, string, time.Duration) error {
	f.stopped = true
	if f.waitBlock != nil {
		close(f.waitBlock)
		f.waitBlock = nil
	}
	return nil
}

func (f *fakeContainerRuntime) Kill(context.Context, string) error {
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
