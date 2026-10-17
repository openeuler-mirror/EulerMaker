package container

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

type DockerCLI struct{}

var _ ManagedRuntime = DockerCLI{}

func (DockerCLI) Close() error { return nil }

func (DockerCLI) ImageExists(ctx context.Context, image string) (bool, error) {
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", image)
	if err := cmd.Run(); err != nil {
		return false, nil
	}
	return true, nil
}

func (DockerCLI) Pull(ctx context.Context, image string) error {
	return runDocker(ctx, "pull", image)
}

func (DockerCLI) Remove(ctx context.Context, name string) error {
	return runDocker(ctx, "rm", "-f", name)
}

func (DockerCLI) Create(ctx context.Context, spec Spec) (string, error) {
	// Some ARM hosts run Docker with a seccomp profile that denies clone3 with
	// EPERM. glibc cannot fall back to clone in that case, so git inside the
	// Job container fails to create its async fetch thread. The Runner
	// container's seccomp setting does not carry over to containers it creates.
	// TODO: Remove this override once the host Docker/seccomp stack returns
	// ENOSYS for unsupported clone3 calls or permits them.
	args := []string{"create", "--name", spec.Name, "--security-opt", "seccomp=unconfined"}
	for key, value := range spec.Labels {
		args = append(args, "--label", key+"="+value)
	}
	for hostPath, containerPath := range spec.Mounts {
		args = append(args, "-v", hostPath+":"+containerPath)
	}
	if spec.WorkingDir != "" {
		args = append(args, "--workdir", spec.WorkingDir)
	}
	if spec.NetworkMode != "" {
		args = append(args, "--network", spec.NetworkMode)
	}
	if spec.Privileged {
		args = append(args, "--privileged")
	}
	for key, value := range spec.Env {
		args = append(args, "-e", key+"="+value)
	}
	if spec.Entrypoint != "" {
		args = append(args, "--entrypoint", spec.Entrypoint)
	}
	args = append(args, spec.Image)
	args = append(args, spec.Command...)
	args = append(args, spec.Args...)

	output, err := outputDocker(ctx, args...)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(output)
	if id == "" {
		return "", fmt.Errorf("docker create returned empty container id")
	}
	return id, nil
}

func (DockerCLI) Start(ctx context.Context, id string) error {
	return runDocker(ctx, "start", id)
}

func (DockerCLI) Logs(ctx context.Context, id string, output io.Writer) error {
	cmd := exec.CommandContext(ctx, "docker", "logs", "-f", id)
	cmd.Stdout = output
	cmd.Stderr = output
	return cmd.Run()
}

func (DockerCLI) Wait(ctx context.Context, id string) (int, error) {
	output, err := outputDocker(ctx, "wait", id)
	if err != nil {
		return -1, err
	}
	codeText := strings.TrimSpace(output)
	var code int
	if _, err := fmt.Sscanf(codeText, "%d", &code); err != nil {
		return -1, fmt.Errorf("parse docker wait exit code %q: %w", codeText, err)
	}
	return code, nil
}

func (DockerCLI) Stop(ctx context.Context, id string, gracePeriod time.Duration) error {
	seconds := int(gracePeriod.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	return runDocker(ctx, "stop", "-t", fmt.Sprintf("%d", seconds), id)
}

func (DockerCLI) Kill(ctx context.Context, id string) error {
	return runDocker(ctx, "kill", id)
}

// List returns container IDs matching every label, including stopped containers.
func (DockerCLI) List(ctx context.Context, labels map[string]string) ([]string, error) {
	args := []string{"ps", "-aq"}
	for key, value := range labels {
		filter := "label=" + key
		if value != "" {
			filter += "=" + value
		}
		args = append(args, "--filter", filter)
	}
	output, err := outputDocker(ctx, args...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(output), nil
}

func (DockerCLI) Labels(ctx context.Context, id string) (map[string]string, error) {
	output, err := outputDocker(ctx, "inspect", "--format", "{{json .Config.Labels}}", id)
	if err != nil {
		return nil, err
	}
	var labels map[string]string
	if err := json.Unmarshal([]byte(output), &labels); err != nil {
		return nil, err
	}
	return labels, nil
}

func runDocker(ctx context.Context, args ...string) error {
	_, err := outputDocker(ctx, args...)
	return err
}

func outputDocker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(output.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("docker %s failed: %s", strings.Join(args, " "), msg)
	}
	return output.String(), nil
}
