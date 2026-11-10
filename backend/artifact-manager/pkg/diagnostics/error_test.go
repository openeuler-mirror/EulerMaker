package diagnostics

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

func TestCause(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"disk full", &os.PathError{Op: "write", Path: "secret-body", Err: syscall.ENOSPC}, "filesystem write: no space left on device"},
		{"permission", fmt.Errorf("secret-body: %w", syscall.EACCES), "permission denied"},
		{"timeout", &url.Error{Op: "Get", URL: "https://secret-token@example", Err: context.DeadlineExceeded}, "context deadline exceeded"},
		{"missing executable", &exec.Error{Name: "secret-body", Err: exec.ErrNotFound}, "executable not found"},
		{"unknown", errors.New("secret-token secret-body"), "error type *errors.errorString"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := Cause(test.err); got != test.want {
				t.Fatalf("cause = %q, want %q", got, test.want)
			}
		})
	}
	cmd := exec.Command("sh", "-c", "echo secret-body >&2; exit 7")
	if got := Cause(cmd.Run()); got != "command exited with code 7" {
		t.Fatalf("command cause = %q", got)
	}
}

func TestCode(t *testing.T) {
	if got := Code("SigningBackendUnavailable"); got != "SigningBackendUnavailable" {
		t.Fatal(got)
	}
	for _, value := range []string{"", "Bearer secret-token", "bad\nmessage", strings.Repeat("x", 81)} {
		if got := Code(value); got != "UnknownError" {
			t.Fatalf("unsafe code = %q", got)
		}
	}
}
