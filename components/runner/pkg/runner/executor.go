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

	commands := job.Spec.Commands
	if len(commands) == 0 {
		commands = defaultCommands(job)
	}
	for _, command := range commands {
		if strings.TrimSpace(command) == "" {
			continue
		}
		if err := runCommand(ctx, workDir, job.Spec.Env, command); err != nil {
			return resultRoot, err
		}
	}
	return resultRoot, nil
}

func defaultCommands(job JobResource) []string {
	if job.Spec.Package != "" {
		return []string{"echo building package " + shellQuote(job.Spec.Package)}
	}
	return []string{"echo no commands specified"}
}

func runCommand(ctx context.Context, dir string, env map[string]string, command string) error {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}

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

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
