package buildinfo

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"controller-manager/pkg/clients/gitserver"
	"controller-manager/pkg/controllers/buildinfo/specparse"
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
	git := &blockingRepoGitServer{
		entered:   make(chan struct{}, maxConcurrentRepoParses+1),
		release:   make(chan struct{}),
		specCount: 1,
	}
	c.services.(*fakeServices).git = git
	snapshot := &ebsv1.Snapshot{Status: ebsv1.SnapshotStatus{PackageRepoStatuses: map[string]ebsv1.PackageRepoStatus{}}}
	for i := 0; i <= maxConcurrentRepoParses; i++ {
		name := fmt.Sprintf("repo%02d", i)
		snapshot.Spec.PackageRepos = append(
			snapshot.Spec.PackageRepos,
			ebsv1.PackageRepo{Name: name, URL: "https://example.test/" + name},
		)
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
	c.services.(*fakeServices).git = git
	done := make(chan bool, 1)
	go func() {
		_, ok := c.fetchRepoSpecs(context.Background(), git, &reconcileRound{}, "repo", "https://example.test/repo",
			ebsv1.PackageRepoStatus{CommitID: "commit"}, "x86_64", nil, &specAssembly{})
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

func TestUnparsableSpecConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		clearA  bool
	}{
		{name: "missing key"},
		{name: "empty list", payload: "unparsable_spec: []\n"},
		{name: "non-list", payload: "unparsable_spec: a\n"},
		{name: "mixed list", payload: "unparsable_spec: [42, a]\n", clearA: true},
		{name: "unknown spec", payload: "unparsable_spec: [other]\n"},
		{name: "case sensitive", payload: "unparsable_spec: [A]\n"},
		{name: "repository name is not spec name", payload: "unparsable_spec: [repo1]\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _, _, _ := newTestController(t)
			bi := testBuildInfoObj(ebsv1.BuildInfoPending)
			bi.Spec.BuildPayload = tt.payload
			round := &reconcileRound{key: testNS + "/" + testBuild, current: bi}
			a := dependEntry("a")
			a.BuildRequires = map[string]ebsv1.VersionConstraint{"dependency": {}}
			b := dependEntry("b")
			b.BuildRequires = map[string]ebsv1.VersionConstraint{"dependency": {}}
			depends := map[string]specparse.SpecDepend{"a": a, "b": b}

			c.ignoreBuildRequires(round, depends)

			if cleared := len(depends["a"].BuildRequires) == 0; cleared != tt.clearA {
				t.Fatalf("a BuildRequires cleared=%t, want %t", cleared, tt.clearA)
			}
			if len(depends["b"].BuildRequires) != 1 {
				t.Fatalf("b BuildRequires = %v, want unchanged", depends["b"].BuildRequires)
			}
		})
	}
}

func TestExpandBuildSetUsesOnlyOriginalSeeds(t *testing.T) {
	full := map[string]specparse.SpecDepend{
		"a": {SpecName: "a", Provides: []string{"virtual-a"}},
		"b": {SpecName: "b", BuildRequires: map[string]ebsv1.VersionConstraint{"virtual-a": {}}},
		"c": {SpecName: "c", BuildRequires: map[string]ebsv1.VersionConstraint{"b": {}}},
	}

	got := expandBuildSet(map[string]specparse.SpecDepend{"a": full["a"]}, full, nil)
	if len(got) != 2 || got["a"].SpecName != "a" || got["b"].SpecName != "b" {
		t.Fatalf("build set = %v, want only a and b", got)
	}

	got = expandBuildSet(map[string]specparse.SpecDepend{"a": full["a"], "b": full["b"]}, full, nil)
	if len(got) != 3 || got["c"].SpecName != "c" {
		t.Fatalf("build set with two seeds = %v, want a, b and c", got)
	}
}
