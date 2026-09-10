package command

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"git-server/pkg/auth"
)

type Request struct {
	Repo    string   `json:"repo"`
	Command []string `json:"command"`
}

type Response struct {
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	ExitCode        int    `json:"exit_code"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
}

type ResultKind string

const (
	Success      ResultKind = "Success"
	Failed       ResultKind = "CommandFailed"
	TimedOut     ResultKind = "CommandTimeout"
	OutputLimit  ResultKind = "OutputLimitExceeded"
	StartFailure ResultKind = "InternalError"
)

type Executor struct {
	timeout   time.Duration
	maxOutput int64
}

func New(timeout time.Duration, maxOutput int64) *Executor {
	return &Executor{timeout: timeout, maxOutput: maxOutput}
}

func Validate(request Request) error {
	if request.Repo == "" || len(request.Command) == 0 {
		return fmt.Errorf("repo and command are required")
	}
	if len(request.Command) > 128 {
		return fmt.Errorf("too many command arguments")
	}
	allowed := map[string]bool{"git-rev-parse": true, "git-show": true, "git-log": true, "git-ls-tree": true}
	if !allowed[request.Command[0]] {
		return fmt.Errorf("command is not allowed")
	}
	for _, arg := range request.Command {
		if len(arg) > 8192 || strings.IndexByte(arg, 0) >= 0 {
			return fmt.Errorf("invalid command argument")
		}
	}
	return nil
}

func (e *Executor) Run(parent context.Context, repositoryPath string, request Request) (Response, ResultKind, error) {
	if err := Validate(request); err != nil {
		return Response{}, StartFailure, err
	}
	ctx, cancel := context.WithTimeout(parent, e.timeout)
	defer cancel()
	args := []string{"-C", repositoryPath, strings.TrimPrefix(request.Command[0], "git-")}
	args = append(args, request.Command[1:]...)
	cmd := exec.Command("git", args...)
	cmd.Env = auth.BaseEnvironment()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	capture := newCapture(e.maxOutput, func() { cancel() })
	cmd.Stdout = &streamWriter{capture: capture, stdout: true}
	cmd.Stderr = &streamWriter{capture: capture}
	if err := cmd.Start(); err != nil {
		return Response{}, StartFailure, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var runErr error
	select {
	case runErr = <-done:
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		runErr = <-done
	}
	response := capture.response()
	if capture.exceeded() {
		response.ExitCode = -1
		return response, OutputLimit, nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		response.ExitCode = -1
		return response, TimedOut, nil
	}
	if runErr == nil {
		response.ExitCode = 0
		return response, Success, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		response.ExitCode = exitErr.ExitCode()
		return response, Failed, nil
	}
	return Response{}, StartFailure, runErr
}

type capture struct {
	mu              sync.Mutex
	stdout, stderr  bytes.Buffer
	limit, consumed int64
	stdoutTruncated bool
	stderrTruncated bool
	onLimit         func()
}

func newCapture(limit int64, onLimit func()) *capture {
	return &capture{limit: limit, onLimit: onLimit}
}

type streamWriter struct {
	capture *capture
	stdout  bool
}

func (w *streamWriter) Write(value []byte) (int, error) {
	w.capture.mu.Lock()
	remaining := w.capture.limit - w.capture.consumed
	toWrite := int64(len(value))
	if toWrite > remaining {
		toWrite = remaining
	}
	if toWrite > 0 {
		if w.stdout {
			_, _ = w.capture.stdout.Write(value[:toWrite])
		} else {
			_, _ = w.capture.stderr.Write(value[:toWrite])
		}
		w.capture.consumed += toWrite
	}
	exceeded := int64(len(value)) > toWrite
	if exceeded {
		if w.stdout {
			w.capture.stdoutTruncated = true
		} else {
			w.capture.stderrTruncated = true
		}
	}
	onLimit := w.capture.onLimit
	w.capture.mu.Unlock()
	if exceeded {
		onLimit()
	}
	return len(value), nil
}

func (c *capture) exceeded() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stdoutTruncated || c.stderrTruncated
}

func (c *capture) response() Response {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Response{
		Stdout:          validUTF8(c.stdout.Bytes()),
		Stderr:          validUTF8(c.stderr.Bytes()),
		StdoutTruncated: c.stdoutTruncated,
		StderrTruncated: c.stderrTruncated,
	}
}

func validUTF8(value []byte) string {
	if utf8.Valid(value) {
		return string(value)
	}
	return strings.ToValidUTF8(string(value), "\uFFFD")
}
