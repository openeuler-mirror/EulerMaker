package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"ebs-runner/pkg/artifact"
	containerruntime "ebs-runner/pkg/container"
	"ebs-runner/pkg/gateway"
)

const (
	defaultCTWorkingDir = "/workspace"
	defaultCTResultDir  = "/results"
)

type CTExecutor struct {
	WorkDir         string
	ResultRoot      string
	RunnerName      string
	Runtime         containerruntime.Runtime
	Scripts         *ScriptCache
	LogFactory      artifact.JobLogSinkFactory
	ResolvePayload  func(gateway.JobResource) (string, error)
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

func (e *CTExecutor) Execute(ctx context.Context, job gateway.JobResource) (string, error) {
	if e.LogFactory != nil && (job.Metadata.Namespace == "" || job.Metadata.Name == "") {
		return "", fmt.Errorf("job namespace and name are required for log upload")
	}
	project := job.Metadata.Namespace
	if project == "" {
		project = "default"
	}
	jobName := job.Metadata.Name
	if !artifact.ValidLocalPathSegment(jobName) {
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
	payload := job.Spec.Payload
	if e.ResolvePayload != nil {
		payload, err = e.ResolvePayload(job)
		if err != nil {
			return "", fmt.Errorf("resolve Job repositories: %w", err)
		}
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
	if err := os.WriteFile(filepath.Join(workDir, "payload.json"), []byte(payload), 0o644); err != nil {
		return "", fmt.Errorf("write payload.json: %w", err)
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
		container = containerruntime.DockerCLI{}
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

	containerSpec := containerruntime.Spec{
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
	var logSink artifact.JobLogSink
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
	logOutput = newTimestampLogWriter(logOutput, time.Now)

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

func ensureImage(ctx context.Context, runtime containerruntime.Runtime, image, policy string) error {
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

func waitContainer(ctx context.Context, runtime containerruntime.Runtime, id string, gracePeriod time.Duration) (int, error) {
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
func stopContainer(runtime containerruntime.Runtime, id string, gracePeriod time.Duration) error {
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
