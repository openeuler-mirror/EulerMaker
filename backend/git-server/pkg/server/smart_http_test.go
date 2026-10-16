package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git-server/pkg/gitops"
	"git-server/pkg/repository"
	"git-server/pkg/storage"
)

func runGit(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestSmartHTTPCloneAndFetch(t *testing.T) {
	for _, name := range []string{"repo", "repo+name"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			seed := filepath.Join(root, "seed")
			runGit(t, "init", "--initial-branch=main", seed)
			if err := os.WriteFile(filepath.Join(seed, "source.txt"), []byte("source content"), 0600); err != nil {
				t.Fatal(err)
			}
			runGit(t, "-C", seed, "add", "source.txt")
			runGit(t, "-C", seed, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
			parsed, err := repository.ParseURL("https://example.com/team/" + name + ".git")
			if err != nil {
				t.Fatal(err)
			}
			store, err := storage.Open(filepath.Join(root, "data"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			bare := filepath.Join(store.DataDir(), parsed.Key)
			runGit(t, "clone", "--mirror", seed, bare)
			runGit(t, "-C", bare, "config", "remote.origin.url", parsed.Original)
			// Even a repository configured to accept pushes must be exported read-only.
			runGit(t, "-C", bare, "config", "http.receivepack", "true")
			ctx, cancel := context.WithCancel(context.Background())
			manager, err := repository.NewManager(ctx, store, gitops.New(nil, time.Second), repository.ManagerConfig{
				Workers: 1, RetryBase: time.Second, RetryMax: time.Second, CleanupPeriod: time.Hour,
			})
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { manager.Run(ctx); close(done) }()
			defer func() { cancel(); <-done }()
			handler, err := NewSmartHTTP(manager, 10*time.Second, 2)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(New(manager, nil, 1024, handler))
			defer server.Close()
			cloneURL := server.URL + (&url.URL{Path: "/git/" + parsed.Key}).EscapedPath()
			for _, protocol := range []string{"0", "2"} {
				clone := filepath.Join(root, "clone-"+protocol)
				runGit(t, "-c", "protocol.version="+protocol, "clone", cloneURL, clone)
				got, err := os.ReadFile(filepath.Join(clone, "source.txt"))
				if err != nil || string(got) != "source content" {
					t.Fatalf("cloned source = %q, %v", got, err)
				}
				runGit(t, "-C", seed, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", protocol)
				runGit(t, "-C", bare, "fetch", seed, "+refs/heads/main:refs/heads/main")
				runGit(t, "-C", clone, "-c", "protocol.version="+protocol, "fetch", "origin")
				if got := runGit(t, "-C", clone, "rev-parse", "origin/main"); got != runGit(t, "-C", seed, "rev-parse", "HEAD") {
					t.Fatal("fetch did not receive the latest commit")
				}
				cmd := exec.Command("git", "-C", clone, "push", "origin", "HEAD:refs/heads/forbidden")
				if out, err := cmd.CombinedOutput(); err == nil {
					t.Fatalf("read-only server accepted push: %s", out)
				}
			}
			for _, path := range []string{"/config", "/HEAD", "/objects/info/packs", "/info/refs?service=git-receive-pack"} {
				res, err := http.Get(cloneURL + path)
				if err != nil {
					t.Fatal(err)
				}
				res.Body.Close()
				if res.StatusCode != http.StatusForbidden {
					t.Fatalf("%s returned %d", path, res.StatusCode)
				}
			}
			res, err := http.Get(server.URL + "/git/.tmp/clone.git/info/refs?service=git-upload-pack")
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != http.StatusNotFound {
				t.Fatalf("temporary repository returned %d", res.StatusCode)
			}
		})
	}
}

type cloneManagerStub struct{ path string }

func (cloneManagerStub) Ready() bool { return true }
func (m cloneManagerStub) WithCloneRepository(ctx context.Context, key string, fn func(string) error) error {
	return fn(m.path)
}

func TestSmartHTTPTimeoutAndConcurrency(t *testing.T) {
	root := t.TempDir()
	backend := filepath.Join(root, "backend")
	if err := os.WriteFile(backend, []byte("#!/bin/sh\nsleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	h := &SmartHTTP{manager: cloneManagerStub{root}, git: backend, timeout: 50 * time.Millisecond, slots: make(chan struct{}, 1)}
	request := httptest.NewRequest("GET", "/git/example.com/repo.git/info/refs?service=git-upload-pack", nil)
	response := httptest.NewRecorder()
	started := time.Now()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusGatewayTimeout || time.Since(started) > 3*time.Second {
		t.Fatalf("timeout response: %d after %s", response.Code, time.Since(started))
	}
	h.slots <- struct{}{}
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "5" {
		t.Fatalf("busy response: %d, %v", response.Code, response.Header())
	}
}
