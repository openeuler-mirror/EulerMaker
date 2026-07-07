package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Executor interface {
	Execute(ctx context.Context, job JobResource) (string, error)
}

type RuntimeManager struct {
	RunnerType string
	Executors  map[string]Executor
}

func (m *RuntimeManager) Execute(ctx context.Context, job JobResource) (string, error) {
	runtimeName := job.Spec.Runtime
	if runtimeName == "" {
		runtimeName = "dc"
	}
	if m.RunnerType != "" && runtimeName != m.RunnerType {
		return "", fmt.Errorf("runner type %q cannot execute job runtime %q", m.RunnerType, runtimeName)
	}
	executor, ok := m.Executors[runtimeName]
	if !ok || executor == nil {
		return "", fmt.Errorf("runtime %q is not supported", runtimeName)
	}
	return executor.Execute(ctx, job)
}

type ShellExecutor struct {
	WorkDir    string
	ResultRoot string
}

func (e *ShellExecutor) Execute(ctx context.Context, job JobResource) (string, error) {
	project := job.Metadata.Namespace
	if project == "" {
		project = "default"
	}
	jobName := job.Metadata.Name
	if jobName == "" {
		return "", fmt.Errorf("job name is required")
	}

	workDir := filepath.Join(e.WorkDir, project, jobName)
	resultRoot := filepath.Join(e.ResultRoot, project, jobName)
	if err := os.RemoveAll(workDir); err != nil {
		return "", fmt.Errorf("clean work dir: %w", err)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return "", fmt.Errorf("create work dir: %w", err)
	}
	if err := os.MkdirAll(resultRoot, 0o755); err != nil {
		return "", fmt.Errorf("create result root: %w", err)
	}

	payload := strings.TrimSpace(job.Spec.Payload)
	if payload == "" {
		payload = "echo no payload specified"
	}
	if err := runCommand(ctx, workDir, payload); err != nil {
		return resultRoot, err
	}
	return resultRoot, nil
}

func runCommand(ctx context.Context, dir, command string) error {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = os.Environ()

	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(output.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("command %q failed: %s", command, msg)
	}
	return nil
}
