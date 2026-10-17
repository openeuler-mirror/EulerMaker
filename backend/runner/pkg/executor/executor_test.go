package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"ebs-runner/pkg/gateway"
)

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
