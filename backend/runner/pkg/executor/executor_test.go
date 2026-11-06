package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"ebs-runner/pkg/gateway"
)

func TestRuntimeManagerDispatchesCT(t *testing.T) {
	executor := &fakeExecutor{resultRoot: "/results/project/job"}
	manager := &Manager{
		RunnerType: "ct",
		Executors:  map[string]Executor{"ct": executor},
	}

	resultRoot, err := manager.Execute(context.Background(), gateway.JobResource{})
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
	manager := &Manager{
		RunnerType: "ct",
		Executors:  map[string]Executor{"ct": &fakeExecutor{}},
	}

	_, err := manager.Execute(context.Background(), gateway.JobResource{Spec: gateway.JobSpec{Runtime: "vm"}})
	if err == nil {
		t.Fatalf("expected runtime mismatch error")
	}
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

func TestShellExecutorRunsPayload(t *testing.T) {
	dir := t.TempDir()
	executor := &ShellExecutor{
		WorkDir:    filepath.Join(dir, "work"),
		ResultRoot: filepath.Join(dir, "results"),
	}
	job := gateway.JobResource{
		Metadata: gateway.ObjectMeta{Name: "job-a", Namespace: "project-a"},
		Spec:     gateway.JobSpec{Payload: "MESSAGE=ok\nprintf '%s' \"$MESSAGE\" > output.txt"},
	}

	resultRoot, err := executor.Execute(context.Background(), job)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if resultRoot != filepath.Join(dir, "results", "project-a", "job-a") {
		t.Fatalf("result root = %s", resultRoot)
	}
	data, err := os.ReadFile(filepath.Join(dir, "work", "project-a", "job-a", "output.txt"))
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(data) != "ok" {
		t.Fatalf("output = %q", string(data))
	}
}

func TestShellExecutorReturnsCommandError(t *testing.T) {
	dir := t.TempDir()
	executor := &ShellExecutor{
		WorkDir:    filepath.Join(dir, "work"),
		ResultRoot: filepath.Join(dir, "results"),
	}
	job := gateway.JobResource{
		Metadata: gateway.ObjectMeta{Name: "job-a", Namespace: "project-a"},
		Spec:     gateway.JobSpec{Payload: "echo boom >&2; exit 7"},
	}

	if _, err := executor.Execute(context.Background(), job); err == nil {
		t.Fatalf("expected command error")
	}
}
