package gitops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"git-server/pkg/auth"
)

type Remote struct {
	URL            string
	Scheme         string
	Host           string
	RepositoryPath string
	User           string
}

type OperationError struct {
	Code      string
	Message   string
	Retryable bool
}

func (e *OperationError) Error() string { return e.Message }

type Client struct {
	auth    *auth.Manager
	timeout time.Duration
}

func New(authManager *auth.Manager, timeout time.Duration) *Client {
	return &Client{auth: authManager, timeout: timeout}
}

func (c *Client) Clone(ctx context.Context, remote Remote, destination, operationID string) error {
	prepared, err := c.auth.Prepare(remote.Scheme, remote.Host, remote.RepositoryPath, remote.User, operationID)
	if err != nil {
		return &OperationError{Code: "AuthNotConfigured", Message: err.Error(), Retryable: false}
	}
	defer prepared.Cleanup()
	_, stderr, err := c.run(ctx, prepared.Env, "git", "clone", "--mirror", "--", remote.URL, destination)
	return classify(err, prepared.Redact(stderr))
}

func (c *Client) Fetch(ctx context.Context, remote Remote, repositoryPath, operationID string) error {
	prepared, err := c.auth.Prepare(remote.Scheme, remote.Host, remote.RepositoryPath, remote.User, operationID)
	if err != nil {
		return &OperationError{Code: "AuthNotConfigured", Message: err.Error(), Retryable: false}
	}
	defer prepared.Cleanup()
	_, stderr, err := c.run(ctx, prepared.Env, "git", "-C", repositoryPath, "fetch", "--prune", "origin", "+refs/*:refs/*")
	return classify(err, prepared.Redact(stderr))
}

func (c *Client) ValidateBare(ctx context.Context, repositoryPath string) error {
	stdout, stderr, err := c.run(ctx, auth.BaseEnvironment(), "git", "-C", repositoryPath, "rev-parse", "--is-bare-repository")
	if err != nil || strings.TrimSpace(stdout) != "true" {
		if err == nil {
			err = errors.New("not a bare repository")
		}
		return &OperationError{Code: "InvalidRepository", Message: sanitizedMessage(stderr, err), Retryable: false}
	}
	return nil
}

func (c *Client) Origin(ctx context.Context, repositoryPath string) (string, error) {
	stdout, stderr, err := c.run(ctx, auth.BaseEnvironment(), "git", "-C", repositoryPath, "config", "--get", "remote.origin.url")
	if err != nil || strings.TrimSpace(stdout) == "" {
		return "", &OperationError{Code: "InvalidRepository", Message: sanitizedMessage(stderr, err), Retryable: false}
	}
	return strings.TrimSpace(stdout), nil
}

func (c *Client) run(parent context.Context, env []string, name string, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	defer cancel()
	cmd := exec.Command(name, args...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return "", "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return stdout.String(), stderr.String(), err
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return stdout.String(), stderr.String(), ctx.Err()
	}
}

func classify(err error, stderr string) error {
	if err == nil {
		return nil
	}
	message := sanitizedMessage(stderr, err)
	lower := strings.ToLower(message)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return &OperationError{Code: "OperationTimeout", Message: "Git operation timed out", Retryable: true}
	}
	if strings.Contains(lower, "authentication failed") || strings.Contains(lower, "permission denied") || strings.Contains(lower, "could not read username") || strings.Contains(lower, "host key verification failed") || strings.Contains(lower, "repository not found") {
		return &OperationError{Code: "AuthenticationFailed", Message: message, Retryable: false}
	}
	if strings.Contains(lower, "could not resolve host") || strings.Contains(lower, "connection timed out") || strings.Contains(lower, "connection reset") || strings.Contains(lower, "remote end hung up") || strings.Contains(lower, "the requested url returned error: 5") {
		return &OperationError{Code: "RemoteUnavailable", Message: message, Retryable: true}
	}
	return &OperationError{Code: "GitOperationFailed", Message: message, Retryable: false}
}

func sanitizedMessage(stderr string, err error) string {
	message := strings.TrimSpace(stderr)
	if message == "" && err != nil {
		message = err.Error()
	}
	if len(message) > 4096 {
		message = message[:4096]
	}
	if message == "" {
		message = "Git operation failed"
	}
	return message
}

func AsOperationError(err error) *OperationError {
	var operationErr *OperationError
	if errors.As(err, &operationErr) {
		return operationErr
	}
	return &OperationError{Code: "InternalError", Message: fmt.Sprintf("%v", err), Retryable: false}
}
