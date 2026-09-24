// exec.go carries the BuildInfo controller's read-only command surface
// (design 4.2): ExecCommand executes whitelisted git commands on the synced
// local mirror via POST /command; mirror location is resolved server-side
// from the repository key, and the client never holds a store path, never
// checks readiness, and never publishes sync tasks (those belong to the
// Snapshot controller, via the methods in client.go).

package gitserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// GitServerClient is the read-only command surface consumed by the BuildInfo
// controller (design 4.2).
type GitServerClient interface {
	// ExecCommand runs a whitelisted read-only git command (first token
	// must be git-ls-tree or git-show, e.g. "git-ls-tree --name-only <commit>"
	// or "git-show <commit>:<path>") on the mirror identified by cloneURL and
	// returns stdout.
	ExecCommand(ctx context.Context, cloneURL, command string) (string, error)
}

var _ GitServerClient = (*Client)(nil)

// maxExecResponseBody bounds one /command response read; spec file contents
// and tree listings can legitimately be large.
const maxExecResponseBody = 16 << 20

// ExecCommand runs a whitelisted read-only git command on the synced mirror
// identified by cloneURL and returns stdout. Failures are classified for the
// E-23 routing consumed by the BuildInfo controller: ErrorTransient-class
// failures (ErrorTemporary here) keep the BuildInfo Pending for a next round,
// everything else is deterministic — retrying cannot change the outcome.
func (c *Client) ExecCommand(ctx context.Context, cloneURL, command string) (result string, resultErr error) {
	defer recordRequest(execRequests, &resultErr)
	tokens, err := validateCommand(cloneURL, command)
	if err != nil {
		return "", gitError("exec", ErrorValidation, err)
	}
	var last error
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		stdout, err := c.doExec(ctx, cloneURL, tokens)
		if err == nil {
			return stdout, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		last = err
		var execErr *Error
		if !errors.As(err, &execErr) || execErr.Kind != ErrorTemporary || attempt >= c.retries {
			return "", last
		}
	}
}

func (c *Client) doExec(ctx context.Context, cloneURL string, tokens []string) (string, error) {
	payload, err := json.Marshal(commandRequest{Repo: cloneURL, Command: tokens})
	if err != nil {
		return "", gitError("exec", ErrorValidation, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.address+"/command", bytes.NewReader(payload))
	if err != nil {
		return "", gitError("exec", ErrorValidation, err)
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		return "", gitError("exec", ErrorTemporary, err)
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxExecResponseBody))
	_ = response.Body.Close()
	if readErr != nil {
		return "", gitError("exec", ErrorTemporary, readErr)
	}
	switch {
	case response.StatusCode == http.StatusOK:
		var success commandResponse
		if err := json.Unmarshal(body, &success); err != nil {
			return "", gitError("exec", ErrorTemporary, fmt.Errorf("decode response: %w", err))
		}
		// A truncated success response carries incomplete content; the same
		// command would truncate again, so it is deterministic.
		if success.ExitCode != 0 || success.StdoutTruncated {
			return "", gitError("exec", ErrorPermanent, fmt.Errorf("invalid command success response (exit_code=%d, truncated=%t)", success.ExitCode, success.StdoutTruncated))
		}
		return success.Stdout, nil
	case response.StatusCode == http.StatusUnprocessableEntity || response.StatusCode == http.StatusRequestEntityTooLarge || response.StatusCode == http.StatusBadRequest:
		// 422: the git command itself failed (invalid commitId, missing
		// path). 413: output limit exceeded for this input. 400: invalid
		// request, i.e. a client contract bug. All deterministic.
		var failure commandResponse
		_ = json.Unmarshal(body, &failure)
		return "", gitError("exec", ErrorPermanent, fmt.Errorf("HTTP %d: %s", response.StatusCode, firstNonEmpty(failure.Stderr, http.StatusText(response.StatusCode))))
	default:
		// 404 (mirror not available yet), 408/429, 504 (command timeout)
		// and other 5xx are transient: retry within the fixed budget.
		var failure errorResponse
		_ = json.Unmarshal(body, &failure)
		message := failure.Code
		if failure.Message != "" {
			message += ": " + failure.Message
		}
		if message == "" {
			message = http.StatusText(response.StatusCode)
		}
		return "", gitError("exec", ErrorTemporary, fmt.Errorf("HTTP %d: %s", response.StatusCode, message))
	}
}

// validateCommand splits the command string and enforces the client-side
// whitelist: only read-only git-ls-tree / git-show with at least one operand
// are ever sent.
func validateCommand(cloneURL, command string) ([]string, error) {
	if cloneURL == "" || strings.ContainsAny(cloneURL, "\x00\r\n") {
		return nil, fmt.Errorf("clone URL is invalid")
	}
	tokens := strings.Fields(command)
	if len(tokens) < 2 {
		return nil, fmt.Errorf("command requires a verb and at least one operand")
	}
	if tokens[0] != "git-ls-tree" && tokens[0] != "git-show" {
		return nil, fmt.Errorf("command %q is not allowed", tokens[0])
	}
	for _, token := range tokens {
		if len(token) > 8192 || strings.IndexByte(token, 0) >= 0 {
			return nil, fmt.Errorf("invalid command argument")
		}
	}
	return tokens, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
