package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	defaultDCWorkingDir = "/workspace"
	defaultDCResultDir  = "/results"
)

type DCExecutor struct {
	WorkDir         string
	ResultRoot      string
	RunnerName      string
	Docker          DockerRuntime
	StopGracePeriod time.Duration
}

type DCRuntimeSpec struct {
	Image           string            `json:"image,omitempty"`
	ImagePullPolicy string            `json:"imagePullPolicy,omitempty"`
	Privileged      bool              `json:"privileged,omitempty"`
	NetworkMode     string            `json:"networkMode,omitempty"`
	WorkingDir      string            `json:"workingDir,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	Mounts          []DCMount         `json:"mounts,omitempty"`
	Command         []string          `json:"command,omitempty"`
	Args            []string          `json:"args,omitempty"`
}

type DCMount struct {
	Name      string `json:"name,omitempty"`
	MountPath string `json:"mountPath,omitempty"`
}

type ContainerSpec struct {
	Name        string
	Image       string
	Privileged  bool
	NetworkMode string
	WorkingDir  string
	Env         map[string]string
	Mounts      map[string]string
	Labels      map[string]string
	Command     []string
	Args        []string
}

type DockerRuntime interface {
	ImageExists(ctx context.Context, image string) (bool, error)
	Pull(ctx context.Context, image string) error
	Remove(ctx context.Context, name string) error
	Create(ctx context.Context, spec ContainerSpec) (string, error)
	Start(ctx context.Context, id string) error
	Logs(ctx context.Context, id string, output io.Writer) error
	Wait(ctx context.Context, id string) (int, error)
	Stop(ctx context.Context, id string, gracePeriod time.Duration) error
	Kill(ctx context.Context, id string) error
}

func (e *DCExecutor) Execute(ctx context.Context, job JobResource) (string, error) {
	project := job.Metadata.Namespace
	if project == "" {
		project = "default"
	}
	jobName := job.Metadata.Name
	if jobName == "" {
		return "", fmt.Errorf("job name is required")
	}

	spec, err := parseDCRuntimeSpec(job.Spec.RuntimeSpec)
	if err != nil {
		return "", err
	}
	if spec.Image == "" {
		return "", fmt.Errorf("dc runtimeSpec.image is required")
	}

	workDir := filepath.Join(e.WorkDir, project, jobName)
	resultRoot := filepath.Join(e.ResultRoot, project, jobName)
	if err := os.RemoveAll(workDir); err != nil {
		return "", fmt.Errorf("clean work dir: %w", err)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return "", fmt.Errorf("create work dir: %w", err)
	}
	if err := os.RemoveAll(resultRoot); err != nil {
		return "", fmt.Errorf("clean result root: %w", err)
	}
	if err := os.MkdirAll(resultRoot, 0o755); err != nil {
		return "", fmt.Errorf("create result root: %w", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "payload.yaml"), []byte(job.Spec.Payload), 0o644); err != nil {
		return "", fmt.Errorf("write payload.yaml: %w", err)
	}

	docker := e.Docker
	if docker == nil {
		docker = DockerCLI{}
	}
	gracePeriod := e.StopGracePeriod
	if gracePeriod <= 0 {
		gracePeriod = 10 * time.Second
	}

	if err := ensureImage(ctx, docker, spec.Image, spec.ImagePullPolicy); err != nil {
		return resultRoot, err
	}

	containerName := containerName(project, jobName)
	_ = docker.Remove(context.Background(), containerName)

	containerSpec := ContainerSpec{
		Name:        containerName,
		Image:       spec.Image,
		Privileged:  spec.Privileged,
		NetworkMode: spec.NetworkMode,
		WorkingDir:  valueOrDefault(spec.WorkingDir, defaultDCWorkingDir),
		Env:         spec.Env,
		Mounts:      dcMounts(spec.Mounts, workDir, resultRoot),
		Labels: map[string]string{
			"ebs.io/project": project,
			"ebs.io/job":     jobName,
			"ebs.io/runner":  e.RunnerName,
		},
		Command: spec.Command,
		Args:    spec.Args,
	}

	id, err := docker.Create(ctx, containerSpec)
	if err != nil {
		return resultRoot, fmt.Errorf("create container: %w", err)
	}
	defer func() {
		_ = docker.Remove(context.Background(), id)
	}()

	logFile, err := os.Create(filepath.Join(resultRoot, "container.log"))
	if err != nil {
		return resultRoot, fmt.Errorf("create container log: %w", err)
	}
	defer logFile.Close()

	if err := docker.Start(ctx, id); err != nil {
		return resultRoot, fmt.Errorf("start container: %w", err)
	}

	logCtx, cancelLogs := context.WithCancel(context.Background())
	logsDone := make(chan error, 1)
	go func() {
		logsDone <- docker.Logs(logCtx, id, logFile)
	}()

	exitCode, waitErr := waitContainer(ctx, docker, id, gracePeriod)
	cancelLogs()
	select {
	case <-logsDone:
	case <-time.After(2 * time.Second):
	}
	if waitErr != nil {
		return resultRoot, waitErr
	}
	if exitCode != 0 {
		return resultRoot, fmt.Errorf("container exited with code %d", exitCode)
	}
	return resultRoot, nil
}

func ensureImage(ctx context.Context, docker DockerRuntime, image, policy string) error {
	switch strings.ToLower(policy) {
	case "always":
		if err := docker.Pull(ctx, image); err != nil {
			return fmt.Errorf("pull image %q: %w", image, err)
		}
		return nil
	case "never":
		return nil
	case "", "ifnotpresent":
		exists, err := docker.ImageExists(ctx, image)
		if err != nil {
			return fmt.Errorf("inspect image %q: %w", image, err)
		}
		if exists {
			return nil
		}
		if err := docker.Pull(ctx, image); err != nil {
			return fmt.Errorf("pull image %q: %w", image, err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported imagePullPolicy %q", policy)
	}
}

func parseDCRuntimeSpec(raw json.RawMessage) (DCRuntimeSpec, error) {
	var spec DCRuntimeSpec
	if len(raw) == 0 {
		return spec, nil
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return spec, fmt.Errorf("parse dc runtimeSpec: %w", err)
	}
	return spec, nil
}

func dcMounts(specMounts []DCMount, workDir, resultRoot string) map[string]string {
	workMount := defaultDCWorkingDir
	resultMount := defaultDCResultDir
	for _, mount := range specMounts {
		switch mount.Name {
		case "work":
			if mount.MountPath != "" {
				workMount = mount.MountPath
			}
		case "results":
			if mount.MountPath != "" {
				resultMount = mount.MountPath
			}
		}
	}
	return map[string]string{
		workDir:    workMount,
		resultRoot: resultMount,
	}
}

func waitContainer(ctx context.Context, docker DockerRuntime, id string, gracePeriod time.Duration) (int, error) {
	type waitResult struct {
		exitCode int
		err      error
	}
	waitDone := make(chan waitResult, 1)
	go func() {
		code, err := docker.Wait(context.Background(), id)
		waitDone <- waitResult{exitCode: code, err: err}
	}()

	select {
	case result := <-waitDone:
		return result.exitCode, result.err
	case <-ctx.Done():
		stopCtx, cancel := context.WithTimeout(context.Background(), gracePeriod+5*time.Second)
		defer cancel()
		_ = docker.Stop(stopCtx, id, gracePeriod)
		select {
		case <-waitDone:
			return -1, ctx.Err()
		case <-time.After(gracePeriod + 2*time.Second):
			_ = docker.Kill(context.Background(), id)
			<-waitDone
			return -1, ctx.Err()
		}
	}
}

type DockerCLI struct{}

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

func (DockerCLI) Create(ctx context.Context, spec ContainerSpec) (string, error) {
	args := []string{"create", "--name", spec.Name}
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

func containerName(project, job string) string {
	name := "ebs-" + sanitizeDockerName(project) + "-" + sanitizeDockerName(job)
	if len(name) <= 120 {
		return name
	}
	return name[:120]
}

var invalidDockerNameChar = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

func sanitizeDockerName(value string) string {
	value = invalidDockerNameChar.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-_.")
	if value == "" {
		return "default"
	}
	return value
}

func valueOrDefault(value, defaultValue string) string {
	if value == "" {
		return defaultValue
	}
	return value
}
