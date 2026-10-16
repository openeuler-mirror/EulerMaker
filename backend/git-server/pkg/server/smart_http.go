package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"git-server/pkg/repository"
)

const maxGitRequestBytes = 10 << 20

type cloneRepositoryManager interface {
	Ready() bool
	WithCloneRepository(context.Context, string, func(string) error) error
}

type SmartHTTP struct {
	manager cloneRepositoryManager
	git     string
	timeout time.Duration
	slots   chan struct{}
}

func NewSmartHTTP(manager cloneRepositoryManager, timeout time.Duration, workers int) (*SmartHTTP, error) {
	if timeout <= 0 || workers <= 0 {
		return nil, errors.New("Smart HTTP timeout and concurrency must be positive")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, err
	}
	path, err := exec.Command(git, "--exec-path").Output()
	if err != nil {
		return nil, err
	}
	backend := filepath.Join(strings.TrimSpace(string(path)), "git-http-backend")
	if info, err := os.Stat(backend); err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return nil, fmt.Errorf("git-http-backend is unavailable at %s", backend)
	}
	return &SmartHTTP{manager: manager, git: git, timeout: timeout, slots: make(chan struct{}, workers)}, nil
}

func (h *SmartHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.manager.Ready() {
		writeError(w, 503, "NotReady", "service is not ready")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/git/")
	var key, service string
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(path, ".git/info/refs") && r.URL.Query().Get("service") == "git-upload-pack":
		key, service = strings.TrimSuffix(path, "/info/refs"), "/info/refs"
	case r.Method == http.MethodPost && strings.HasSuffix(path, ".git/git-upload-pack") && r.Header.Get("Content-Type") == "application/x-git-upload-pack-request" && r.URL.RawQuery == "":
		key, service = strings.TrimSuffix(path, "/git-upload-pack"), "/git-upload-pack"
	default:
		writeError(w, 403, "ReadOnlyRepository", "only Smart HTTP clone and fetch are supported")
		return
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part == "." || part == ".." || part == ".tmp" || strings.ContainsAny(part, "\\\x00") {
			writeError(w, 404, "RepositoryNotFound", "repository is not available")
			return
		}
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		w.Header().Set("Retry-After", "5")
		writeError(w, 503, "CloneBusy", "clone concurrency limit reached")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(deadline)
	_ = controller.SetWriteDeadline(deadline)
	defer controller.SetReadDeadline(time.Time{})
	defer controller.SetWriteDeadline(time.Time{})
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxGitRequestBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			writeError(w, 413, "RequestTooLarge", "Git request body exceeds 10 MiB")
		case ctx.Err() != nil:
			writeError(w, 504, "CloneTimeout", "Git request was canceled or timed out")
		default:
			writeError(w, 400, "InvalidRequest", "could not read Git request body")
		}
		return
	}
	err = h.manager.WithCloneRepository(ctx, key, func(path string) error {
		return h.serveBackend(ctx, w, r, path, service, body)
	})
	if err != nil {
		switch {
		case repository.IsNotFound(err):
			writeError(w, 404, "RepositoryNotFound", "repository is not available")
		case ctx.Err() != nil:
			writeError(w, 504, "CloneTimeout", "Git request was canceled or timed out")
		default:
			log.Printf("component=git-server operation=clone key=%q error=%q", key, err)
			writeError(w, 502, "CloneFailed", "Git backend failed")
		}
	}
}

func (h *SmartHTTP) serveBackend(ctx context.Context, w http.ResponseWriter, r *http.Request, path, service string, body []byte) error {
	cmd := exec.CommandContext(ctx, h.git, "-c", "http.uploadpack=true", "-c", "http.receivepack=false", "-c", "http.getanyfile=false", "http-backend")
	cmd.Dir = path
	// Only the selected repository and read-only services are exposed; remote credentials are not inherited.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_PROJECT_ROOT=" + path, "GIT_HTTP_EXPORT_ALL=1", "PATH_INFO=" + service,
		"REQUEST_METHOD=" + r.Method, "CONTENT_TYPE=" + r.Header.Get("Content-Type"),
		"CONTENT_LENGTH=" + strconv.Itoa(len(body)), "HTTP_CONTENT_ENCODING=" + r.Header.Get("Content-Encoding"),
		"GIT_PROTOCOL=" + r.Header.Get("Git-Protocol"),
	}
	if service == "/info/refs" {
		cmd.Env = append(cmd.Env, "QUERY_STRING=service=git-upload-pack")
	}
	cmd.Stdin = bytes.NewReader(body)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer stdout.Close()
	reader := bufio.NewReader(stdout)
	headers, err := readGitHeaders(reader)
	if err != nil {
		_ = cmd.Cancel()
		_ = cmd.Wait()
		return err
	}
	status := http.StatusOK
	if value := headers.Get("Status"); value != "" {
		status, err = strconv.Atoi(strings.SplitN(value, " ", 2)[0])
		if err != nil || status < 200 || status > 599 {
			_ = cmd.Cancel()
			_ = cmd.Wait()
			return errors.New("invalid Git backend status")
		}
	}
	for _, name := range []string{"Content-Type", "Cache-Control", "Expires", "Pragma"} {
		if value := headers.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(status)
	_, copyErr := io.Copy(w, reader)
	if copyErr != nil {
		_ = cmd.Cancel()
	}
	waitErr := cmd.Wait()
	if copyErr != nil || waitErr != nil {
		// A partial pack must terminate the response, not append JSON to a successful Git stream.
		panic(http.ErrAbortHandler)
	}
	return nil
}

func readGitHeaders(reader *bufio.Reader) (textproto.MIMEHeader, error) {
	var header []byte
	for {
		line, err := reader.ReadSlice('\n')
		header = append(header, line...)
		if len(header) > 32<<10 {
			return nil, errors.New("Git backend headers exceed limit")
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if bytes.Equal(line, []byte("\r\n")) || bytes.Equal(line, []byte("\n")) {
			return textproto.NewReader(bufio.NewReader(bytes.NewReader(header))).ReadMIMEHeader()
		}
	}
}
