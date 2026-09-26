package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	defaultCTWorkingDir = "/workspace"
	defaultCTResultDir  = "/results"
)

type CTExecutor struct {
	WorkDir         string
	ResultRoot      string
	RunnerName      string
	Runtime         ContainerRuntime
	Scripts         *ScriptCache
	LogFactory      JobLogSinkFactory
	LogDrainTimeout time.Duration
	StopGracePeriod time.Duration
}

type ContainerRuntimeSpec struct {
	Image           string            `json:"image,omitempty"`
	ImagePullPolicy string            `json:"imagePullPolicy,omitempty"`
	Privileged      bool              `json:"privileged,omitempty"`
	NetworkMode     string            `json:"networkMode,omitempty"`
	WorkingDir      string            `json:"workingDir,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	Mounts          []ContainerMount  `json:"mounts,omitempty"`
	Command         []string          `json:"command,omitempty"`
	Args            []string          `json:"args,omitempty"`
}

type ContainerMount struct {
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
	Entrypoint  string
	Command     []string
	Args        []string
}

type ContainerRuntime interface {
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

func (e *CTExecutor) Execute(ctx context.Context, job JobResource) (string, error) {
	if e.LogFactory != nil && (job.Metadata.Namespace == "" || job.Metadata.Name == "") {
		return "", fmt.Errorf("job namespace and name are required for log upload")
	}
	project := job.Metadata.Namespace
	if project == "" {
		project = "default"
	}
	jobName := job.Metadata.Name
	if !validLocalPathSegment(jobName) {
		return "", fmt.Errorf("invalid job name for local path")
	}

	spec, err := parseContainerRuntimeSpec(job.Spec.RuntimeSpec)
	if err != nil {
		return "", err
	}
	if spec.Image == "" {
		return "", fmt.Errorf("ct runtimeSpec.image is required")
	}
	if len(job.Spec.ScriptRefs) > 0 && (len(spec.Command) > 0 || len(spec.Args) > 0) {
		return "", fmt.Errorf("ct runtimeSpec.command/args cannot be set with scriptRef")
	}
	seenScripts := make(map[string]struct{}, len(job.Spec.ScriptRefs))
	for _, ref := range job.Spec.ScriptRefs {
		if ref.Name == "" || ref.Name == "." || ref.Name == ".." || filepath.Base(ref.Name) != ref.Name || strings.ContainsAny(ref.Name, "/\\") {
			return "", fmt.Errorf("invalid Job script name %q", ref.Name)
		}
		if _, exists := seenScripts[ref.Name]; exists {
			return "", fmt.Errorf("duplicate Job script name %q", ref.Name)
		}
		seenScripts[ref.Name] = struct{}{}
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
	var scriptPath string
	if len(job.Spec.ScriptRefs) > 0 {
		scriptsDir := filepath.Join(workDir, "scripts")
		if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
			return resultRoot, fmt.Errorf("create Job scripts dir: %w", err)
		}
		for _, ref := range job.Spec.ScriptRefs {
			content, err := e.Scripts.Resolve(ctx, ref)
			if err != nil {
				return resultRoot, fmt.Errorf("resolve Job script %q: %w", ref.Name, err)
			}
			if err := writeJobScript(filepath.Join(scriptsDir, ref.Name), content); err != nil {
				return resultRoot, err
			}
		}
		scriptPath = filepath.Join(defaultCTWorkingDir, "scripts", job.Spec.ScriptRefs[0].Name)
	}

	container := e.Runtime
	if container == nil {
		container = DockerCLI{}
	}
	gracePeriod := e.StopGracePeriod
	if gracePeriod <= 0 {
		gracePeriod = 10 * time.Second
	}

	if err := ensureImage(ctx, container, spec.Image, spec.ImagePullPolicy); err != nil {
		return resultRoot, err
	}

	containerID := job.Metadata.UID
	if containerID == "" {
		containerID = jobName
	}
	containerName := containerName(project, containerID)
	if err := ctx.Err(); err != nil {
		return resultRoot, err
	}

	containerSpec := ContainerSpec{
		Name:        containerName,
		Image:       spec.Image,
		Privileged:  spec.Privileged,
		NetworkMode: spec.NetworkMode,
		WorkingDir:  valueOrDefault(spec.WorkingDir, defaultCTWorkingDir),
		Env:         spec.Env,
		Mounts:      containerMounts(spec.Mounts, workDir, resultRoot),
		Labels: map[string]string{
			"ebs.io/job-uid": job.Metadata.UID,
			"ebs.io/project": project,
			"ebs.io/job":     jobName,
			"ebs.io/runner":  e.RunnerName,
		},
		Command: spec.Command,
		Args:    spec.Args,
	}
	if scriptPath != "" {
		if containerSpec.Mounts[workDir] != defaultCTWorkingDir {
			return resultRoot, fmt.Errorf("script Job requires work mount at %s", defaultCTWorkingDir)
		}
		for hostPath, target := range containerSpec.Mounts {
			if hostPath != workDir && (target == "/" || target == defaultCTWorkingDir || strings.HasPrefix(target, defaultCTWorkingDir+"/")) {
				return resultRoot, fmt.Errorf("container mount %q shadows Job script", target)
			}
		}
		containerSpec.Entrypoint = scriptPath
		containerSpec.Command = nil
		containerSpec.Args = nil
	}

	id, err := container.Create(ctx, containerSpec)
	if err != nil {
		return resultRoot, fmt.Errorf("create container: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := container.Remove(cleanupCtx, id); err != nil {
			log.Printf("remove Job container %s: %v", id, err)
		}
	}()

	var logOutput io.Writer
	var logSink JobLogSink
	if e.LogFactory != nil {
		logSink, err = e.LogFactory.Open(job)
		if err != nil {
			return resultRoot, fmt.Errorf("open artifact log: %w", err)
		}
		logOutput = logSink
	} else {
		logFile, createErr := os.Create(filepath.Join(resultRoot, "container.log"))
		if createErr != nil {
			return resultRoot, fmt.Errorf("create container log: %w", createErr)
		}
		defer logFile.Close()
		logOutput = logFile
	}

	if err := ctx.Err(); err != nil {
		return resultRoot, err
	}
	if err := container.Start(ctx, id); err != nil {
		// Start may have reached the daemon despite a lost response.
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		_, _ = waitContainer(cancelled, container, id, gracePeriod)
		return resultRoot, fmt.Errorf("start container: %w", err)
	}

	logCtx, cancelLogs := context.WithCancel(context.Background())
	logsDone := make(chan error, 1)
	go func() {
		logsDone <- container.Logs(logCtx, id, logOutput)
	}()

	exitCode, waitErr := waitContainer(ctx, container, id, gracePeriod)
	drainTimeout := e.LogDrainTimeout
	if drainTimeout <= 0 {
		drainTimeout = 30 * time.Second
	}
	var logErr error
	select {
	case logErr = <-logsDone:
	case <-time.After(drainTimeout):
		cancelLogs()
		logErr = fmt.Errorf("drain container logs: timeout after %s", drainTimeout)
	}
	cancelLogs()
	if logSink != nil {
		completeCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
		_, completeErr := logSink.Complete(completeCtx)
		cancel()
		if completeErr != nil {
			logErr = errors.Join(logErr, fmt.Errorf("complete artifact log: %w", completeErr))
		}
	}
	if waitErr != nil {
		return resultRoot, errors.Join(waitErr, logErr)
	}
	if exitCode != 0 {
		return resultRoot, errors.Join(fmt.Errorf("container exited with code %d", exitCode), logErr)
	}
	return resultRoot, logErr
}

func writeJobScript(path, content string) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".build-script-")
	if err != nil {
		return fmt.Errorf("create build script: %w", err)
	}
	defer os.Remove(file.Name())
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		return fmt.Errorf("write build script: %w", err)
	}
	if err := file.Chmod(0o555); err != nil {
		_ = file.Close()
		return fmt.Errorf("chmod build script: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close build script: %w", err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("install build script: %w", err)
	}
	return nil
}

func ensureImage(ctx context.Context, runtime ContainerRuntime, image, policy string) error {
	switch strings.ToLower(policy) {
	case "always":
		if err := runtime.Pull(ctx, image); err != nil {
			return fmt.Errorf("pull image %q: %w", image, err)
		}
		return nil
	case "never":
		return nil
	case "", "ifnotpresent":
		exists, err := runtime.ImageExists(ctx, image)
		if err != nil {
			return fmt.Errorf("inspect image %q: %w", image, err)
		}
		if exists {
			return nil
		}
		if err := runtime.Pull(ctx, image); err != nil {
			return fmt.Errorf("pull image %q: %w", image, err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported imagePullPolicy %q", policy)
	}
}

func parseContainerRuntimeSpec(raw json.RawMessage) (ContainerRuntimeSpec, error) {
	var spec ContainerRuntimeSpec
	if len(raw) == 0 {
		return spec, nil
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return spec, fmt.Errorf("parse ct runtimeSpec: %w", err)
	}
	return spec, nil
}

func containerMounts(specMounts []ContainerMount, workDir, resultRoot string) map[string]string {
	workMount := defaultCTWorkingDir
	resultMount := defaultCTResultDir
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

func waitContainer(ctx context.Context, runtime ContainerRuntime, id string, gracePeriod time.Duration) (int, error) {
	type waitResult struct {
		exitCode int
		err      error
	}
	waitDone := make(chan waitResult, 1)
	waitCtx, cancelWait := context.WithCancel(context.Background())
	defer cancelWait()
	go func() {
		code, err := runtime.Wait(waitCtx, id)
		waitDone <- waitResult{exitCode: code, err: err}
	}()

	select {
	case result := <-waitDone:
		if result.err == nil {
			return result.exitCode, nil
		}
		// A failed wait does not establish process exit. Stop before releasing
		// local accounting, just as for cancellation.
		return -1, errors.Join(result.err, stopContainer(runtime, id, gracePeriod))
	case <-ctx.Done():
		return -1, errors.Join(ctx.Err(), stopContainer(runtime, id, gracePeriod))
	}
}

// Keep the execution registered until the daemon confirms a stop. Each call
// is bounded; persistent daemon failure retries without releasing capacity.
func stopContainer(runtime ContainerRuntime, id string, gracePeriod time.Duration) error {
	for {
		stopCtx, cancel := context.WithTimeout(context.Background(), gracePeriod+5*time.Second)
		err := runtime.Stop(stopCtx, id, gracePeriod)
		cancel()
		if err == nil {
			return nil
		}
		killCtx, cancelKill := context.WithTimeout(context.Background(), 10*time.Second)
		killErr := runtime.Kill(killCtx, id)
		cancelKill()
		if killErr == nil {
			return nil
		}
		log.Printf("Job container %s stop not confirmed; retaining execution: stop=%v kill=%v", id, err, killErr)
		time.Sleep(5 * time.Second)
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
