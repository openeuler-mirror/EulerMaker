package diagnostics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
)

// Cause reports system failures without copying URLs, filenames, command output or arbitrary error text.
func Cause(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "context canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "context deadline exceeded"
	}
	var path *os.PathError
	if errors.As(err, &path) {
		return fmt.Sprintf("filesystem %s: %s", Code(path.Op), Cause(path.Err))
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno.Error()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return fmt.Sprintf("command exited with code %d", exit.ExitCode())
	}
	if errors.Is(err, exec.ErrNotFound) {
		return "executable not found"
	}
	for _, known := range []error{io.EOF, io.ErrUnexpectedEOF, os.ErrNotExist, os.ErrPermission, os.ErrDeadlineExceeded} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	// Unknown errors can contain upstream responses or uploaded content.
	return fmt.Sprintf("error type %T", err)
}

// Code accepts protocol error identifiers, not free-form messages.
func Code(value string) string {
	if value == "" || len(value) > 80 {
		return "UnknownError"
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return "UnknownError"
		}
	}
	return value
}
