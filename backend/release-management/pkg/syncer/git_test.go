package syncer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, data)
	}
	return strings.TrimSpace(string(data))
}

func newGitTestSource(t *testing.T) (string, Source, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	gitTest(t, repo, "init", "-b", "main")
	gitTest(t, repo, "config", "user.email", "test@example.com")
	gitTest(t, repo, "config", "user.name", "Test")
	if err := os.MkdirAll(filepath.Join(repo, "baseos", "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(repo, "baseos", "packages.yaml")
	if err := os.WriteFile(file, manifest("gcc").Data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "baseos", "nested", "ignored.yaml"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-m", "first")
	first := gitTest(t, repo, "rev-parse", "HEAD")
	gitTest(t, repo, "tag", "-a", "v1", "-m", "tag")
	gitTest(t, repo, "update-server-info")
	server := httptest.NewServer(http.FileServer(http.Dir(filepath.Join(repo, ".git"))))
	t.Cleanup(server.Close)
	return repo, Source{Name: "source", Repository: server.URL, Ref: "refs/heads/main", AllowPrivateNetwork: true}, first
}

func TestGitReaderHTTPAndPinnedTree(t *testing.T) {
	repo, source, first := newGitTestSource(t)
	file := filepath.Join(repo, "baseos", "packages.yaml")
	reader := NewGitReader(t.TempDir())
	paths := []SourcePath{{Directory: "baseos"}}
	commit, files, err := reader.Read(context.Background(), source, paths)
	if err != nil || commit != first || len(files) != 1 {
		t.Fatalf("read: %s %+v %v", commit, files, err)
	}
	var concurrent sync.WaitGroup
	for range 4 {
		concurrent.Add(1)
		go func() {
			defer concurrent.Done()
			got, manifests, err := reader.Read(context.Background(), source, paths)
			if err != nil || got != first || len(manifests) != 1 {
				t.Errorf("concurrent source read: %s %v", got, err)
			}
		}()
	}
	concurrent.Wait()
	if err := os.WriteFile(file, manifest("zlib").Data, 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-m", "second")
	gitTest(t, repo, "update-server-info")
	if !strings.Contains(string(files[0].Data), "gcc") {
		t.Fatal("previous pinned read changed")
	}
	second, files, err := reader.Read(context.Background(), source, paths)
	if err != nil || first == second || !strings.Contains(string(files[0].Data), "zlib") {
		t.Fatal(second, err)
	}
	source.Ref = "refs/tags/v1"
	commit, _, err = reader.Read(context.Background(), source, paths)
	if err != nil || commit != first {
		t.Fatal("annotated tag", commit, err)
	}
	source.Ref = "refs/heads/missing"
	if _, _, err := reader.Read(context.Background(), source, paths); err == nil || !failureOf(err).Blocked {
		t.Fatal("missing ref", err)
	}
	source.AllowPrivateNetwork = false
	if _, err := resolveSource(context.Background(), source); err == nil || !failureOf(err).Blocked {
		t.Fatal("private source allowed", err)
	}
	if _, err := readManifests(context.Background(), repo, first, []SourcePath{{Directory: "missing"}}); err == nil {
		t.Fatal("missing directory accepted")
	}
	if err := os.Symlink("packages.yaml", filepath.Join(repo, "baseos", "link.yaml")); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-m", "symlink")
	if _, err := readManifests(context.Background(), repo, "HEAD", paths); err == nil {
		t.Fatal("symlink accepted")
	}
}
