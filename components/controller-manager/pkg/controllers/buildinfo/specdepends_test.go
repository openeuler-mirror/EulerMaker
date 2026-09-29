package buildinfo

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"controller-manager/pkg/clients/gitserver"
	ebsv1 "ebs-api/ebs/v1"
)

type blockingRepoGitServer struct {
	gitserver.GitServerClient
	entered   chan struct{}
	release   chan struct{}
	specCount int
	active    atomic.Int32
	peak      atomic.Int32
}

func (g *blockingRepoGitServer) ExecCommand(ctx context.Context, originURL, command string) (string, error) {
	if strings.HasPrefix(command, "git-ls-tree ") {
		var names []string
		for i := 0; i < g.specCount; i++ {
			names = append(names, fmt.Sprintf("%02d.spec", i))
		}
		return strings.Join(names, "\n"), nil
	}
	if !strings.HasPrefix(command, "git-show ") {
		return "", fmt.Errorf("unexpected command %q", command)
	}
	active := g.active.Add(1)
	for {
		peak := g.peak.Load()
		if active <= peak || g.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	g.entered <- struct{}{}
	defer g.active.Add(-1)
	select {
	case <-g.release:
		return fmt.Sprintf("Name: %s\nVersion: 1\n", strings.TrimPrefix(originURL, "https://example.test/")), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestAssembleSpecDependsBoundsParallelRepositories(t *testing.T) {
	c, _, _, _ := newTestController(t)
	git := &blockingRepoGitServer{entered: make(chan struct{}, maxConcurrentRepoParses+1), release: make(chan struct{}), specCount: 1}
	c.gitServer = git
	snapshot := &ebsv1.Snapshot{Status: ebsv1.SnapshotStatus{PackageRepoStatuses: map[string]ebsv1.PackageRepoStatus{}}}
	for i := 0; i <= maxConcurrentRepoParses; i++ {
		name := fmt.Sprintf("repo%02d", i)
		snapshot.Spec.PackageRepos = append(snapshot.Spec.PackageRepos, ebsv1.PackageRepo{Name: name, URL: "https://example.test/" + name})
		snapshot.Status.PackageRepoStatuses[name] = ebsv1.PackageRepoStatus{CommitID: fmt.Sprintf("commit%02d", i)}
	}
	round := &reconcileRound{key: "project/build", current: &ebsv1.BuildInfo{}, build: &ebsv1.Build{}}
	done := make(chan *specAssembly, 1)
	go func() { done <- c.assembleSpecDepends(context.Background(), round, snapshot) }()
	for i := 0; i < maxConcurrentRepoParses; i++ {
		select {
		case <-git.entered:
		case <-time.After(3 * time.Second):
			close(git.release)
			t.Fatal("repositories did not run concurrently")
		}
	}
	close(git.release)
	select {
	case result := <-done:
		if result.incomplete || len(result.depends) != maxConcurrentRepoParses+1 {
			t.Fatalf("assembly incomplete=%t, specs=%d", result.incomplete, len(result.depends))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("repository workers did not finish")
	}
	if peak := git.peak.Load(); peak != maxConcurrentRepoParses {
		t.Fatalf("peak concurrency = %d, want %d", peak, maxConcurrentRepoParses)
	}
}

func TestFetchRepoSpecsParsesFilesSequentially(t *testing.T) {
	c, _, _, _ := newTestController(t)
	git := &blockingRepoGitServer{entered: make(chan struct{}, 5), release: make(chan struct{}), specCount: 5}
	c.gitServer = git
	done := make(chan bool, 1)
	go func() {
		_, ok := c.fetchRepoSpecs(context.Background(), &reconcileRound{}, "repo", "https://example.test/repo",
			ebsv1.PackageRepoStatus{CommitID: "commit"}, "x86_64", nil, &specAssembly{}, new([]string))
		done <- ok
	}()
	select {
	case <-git.entered:
	case <-time.After(3 * time.Second):
		close(git.release)
		t.Fatal("first spec download did not start")
	}
	select {
	case <-git.entered:
		close(git.release)
		t.Fatal("second spec started before the first completed")
	case <-time.After(50 * time.Millisecond):
	}
	close(git.release)
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("repo unexpectedly incomplete")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("spec parsing did not finish")
	}
	if peak := git.peak.Load(); peak != 1 {
		t.Fatalf("peak per-repo concurrency = %d, want 1", peak)
	}
}
