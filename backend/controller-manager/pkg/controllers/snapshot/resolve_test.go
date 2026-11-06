package snapshot

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"controller-manager/pkg/clients/gitserver"
	ebsv1 "ebs-api/ebs/v1"
)

func TestResolvePinnedCommit(t *testing.T) {
	const commitID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, ref := range []string{"", "refs/heads/main"} {
		snapshot, build := baseObjects([]ebsv1.PackageRepo{{Name: "pkg", URL: "https://example.com/pkg.git", Ref: ref, CommitID: commitID}})
		git := &fakeGitClient{}
		api := &fakeClient{snapshot: snapshot, build: build}
		c := newTestController(t, api, git, Config{})
		if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
			t.Fatal(err)
		}
		status := api.snapshot.Status.PackageRepoStatuses["pkg"]
		if status.CommitID != commitID || status.CloneURL != "git-server:///git/example.com/repo.git" || git.commitID != commitID {
			t.Fatalf("status=%+v resolved commit=%q", status, git.commitID)
		}
	}
}

func TestResolvePackageRefAndReuseCommit(t *testing.T) {
	for _, ref := range []string{"refs/heads/main", "refs/tags/v1", "refs/merge-requests/501/head"} {
		t.Run(ref, func(t *testing.T) {
			snapshot, build := baseObjects([]ebsv1.PackageRepo{{Name: "pkg", URL: "https://example.com/pkg.git", Ref: ref}})
			build.Spec.BuildType = "single"
			snapshot.Spec.DefaultRef = "refs/heads/default"
			api := &fakeClient{snapshot: snapshot, build: build}
			git := &fakeGitClient{}
			c := newTestController(t, api, git, Config{})
			if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
				t.Fatal(err)
			}
			status := api.snapshot.Status.PackageRepoStatuses["pkg"]
			if api.snapshot.Status.Phase != ebsv1.SnapshotActive || status.CommitID != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" ||
				status.CloneURL != "git-server:///git/example.com/repo.git" || git.fullRef != ref {
				t.Fatalf("phase=%s status=%+v resolved ref=%q", api.snapshot.Status.Phase, status, git.fullRef)
			}
			if api.snapshot.Spec.PackageRepos[0].Ref != ref {
				t.Fatal("resolution must preserve the frozen package ref")
			}
			// A new controller also reuses a committed package result while the Snapshot is Processing.
			api.snapshot.Status.Phase = ebsv1.SnapshotProcessing
			git.fullRef = ""
			c = newTestController(t, api, git, Config{})
			if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
				t.Fatal(err)
			}
			if git.fullRef != "" || api.snapshot.Status.PackageRepoStatuses["pkg"] != status {
				t.Fatal("persisted package commit must be reused after restart")
			}
		})
	}
}

func TestResolveSnapshotDefaultRef(t *testing.T) {
	for _, tc := range []struct {
		name          string
		ref, fallback string
		skipped       bool
	}{
		{name: "branch fallback", fallback: "refs/heads/release"},
		{name: "tag fallback", fallback: "refs/tags/v1"},
		{name: "explicit wins", ref: "refs/heads/main", fallback: "refs/tags/v1"},
		{name: "both empty", skipped: true},
		{name: "explicit full ref", ref: "refs/merge-requests/501/head", fallback: "refs/tags/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, build := baseObjects([]ebsv1.PackageRepo{{Name: "pkg", URL: "https://example.com/pkg.git", Ref: tc.ref}})
			snapshot.Spec.DefaultRef = tc.fallback
			git := &fakeGitClient{}
			c := newTestController(t, &fakeClient{snapshot: snapshot, build: build}, git, Config{})
			results, err := c.resolveAll(context.Background(), snapshot, snapshot.Spec.PackageRepos)
			if err != nil || len(results) != 1 {
				t.Fatalf("results=%v err=%v", results, err)
			}
			result := results[0]
			if tc.skipped {
				if result.state != stateSkipped || result.status.Error.Code != ebsv1.PackageRepoValidationFailed {
					t.Fatalf("result=%+v", result)
				}
			} else {
				want := tc.ref
				if want == "" {
					want = tc.fallback
				}
				if result.state != stateResolved {
					t.Fatalf("result=%+v", result)
				}
				if git.fullRef != want {
					t.Fatalf("ref=%+v want=%+v", git.fullRef, want)
				}
			}
			if snapshot.Spec.PackageRepos[0].Ref != tc.ref {
				t.Fatal("spec ref mutated")
			}
		})
	}
}

func TestPinnedCommitWaitsForSyncAndReportsMissingCommit(t *testing.T) {
	const commitID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	snapshot, build := baseObjects([]ebsv1.PackageRepo{{Name: "pkg", URL: "https://example.com/pkg.git", Ref: "refs/heads/main", CommitID: commitID}})
	api := &fakeClient{snapshot: snapshot, build: build}
	g := &fakeGitClient{unsynced: true}
	c := newTestController(t, api, g, Config{})
	task := resolveTask{repo: snapshot.Spec.PackageRepos[0]}
	result := c.resolveOne(context.Background(), c.services.(*fakeServices).git, task, snapshot.CreationTimestamp.Time)
	if result.state != stateWaiting || g.publishes != 1 || g.resolves != 0 {
		t.Fatalf("waiting result=%+v publishes=%d resolves=%d", result, g.publishes, g.resolves)
	}
	g.unsynced = false
	g.resolveErr = &gitserver.Error{Kind: gitserver.ErrorNotFound, Err: errors.New("missing commit")}
	result = c.resolveOne(context.Background(), c.services.(*fakeServices).git, task, snapshot.CreationTimestamp.Time)
	if result.state != stateSkipped || result.status.Error.Code != ebsv1.PackageRepoResolveFailed || g.commitID != commitID || g.resolves != 1 {
		t.Fatalf("missing commit result=%+v ref=%s resolves=%d", result, g.commitID, g.resolves)
	}
}

func TestRequestFailureClassification(t *testing.T) {
	c := newTestController(t, &fakeClient{}, &fakeGitClient{}, Config{})
	task := resolveTask{repo: branchRepos(1)[0]}
	budget, cancel := context.WithCancelCause(context.Background())
	cancel(errResolveBudget)
	if got := c.requestFailure(budget, task, "sync", budget.Err()); got.state != stateDeferred {
		t.Fatalf("budget: %+v", got)
	}
	if got := c.requestFailure(budget, task, "sync", errors.New("HTTP 503")); got.state != stateFailed {
		t.Fatalf("completed failure: %+v", got)
	}
	if got := c.requestFailure(context.Background(), task, "sync", context.DeadlineExceeded); got.state != stateFailed || got.status.Error.Code != ebsv1.PackageRepoSyncTimeout {
		t.Fatalf("request timeout: %+v", got)
	}
	parent, stop := context.WithCancel(context.Background())
	stop()
	if got := c.resolveOne(parent, c.services.(*fakeServices).git, task, time.Time{}); got.state != stateFatal || !errors.Is(got.fatal, context.Canceled) {
		t.Fatalf("manager canceled: %+v", got)
	}
}

func TestBudgetWaitsForWorkersAndPreservesStableOrder(t *testing.T) {
	snapshot, build := baseObjects(branchRepos(10))
	var mu sync.Mutex
	active, peak, calls := 0, 0, 0
	g := &fakeGitClient{check: func(ctx context.Context) (gitserver.SyncCheckResult, error) {
		mu.Lock()
		active++
		calls++
		if active > peak {
			peak = active
		}
		mu.Unlock()
		<-ctx.Done()
		mu.Lock()
		active--
		mu.Unlock()
		return gitserver.SyncCheckResult{}, ctx.Err()
	}}
	c := newTestController(t, &fakeClient{snapshot: snapshot, build: build}, g, Config{})
	c.config.ResolveBudget = 20 * time.Millisecond
	c.failures.observe("project-a/build-a", snapshot.UID)
	c.failures.setCursor(snapshot.UID, 5)
	results, err := c.resolveAll(context.Background(), snapshot, snapshot.Spec.PackageRepos)
	if err != nil || len(results) != 10 || active != 0 || peak != 2 || calls != 2 || c.failures.cursor(snapshot.UID) != 7 {
		t.Fatalf("err=%v results=%d active=%d peak=%d calls=%d cursor=%d", err, len(results), active, peak, calls, c.failures.cursor(snapshot.UID))
	}
	for i, result := range results {
		if result.index != i || result.state != stateDeferred {
			t.Fatalf("result[%d]=%+v", i, result)
		}
	}
}

func TestSyncCheckOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want resolveState
		code ebsv1.PackageRepoErrorCode
	}{
		{name: "waiting", want: stateWaiting},
		{name: "temporary error", err: errors.New("unavailable"), want: stateFailed, code: ebsv1.PackageRepoSyncFailed},
		{name: "request timeout", err: context.DeadlineExceeded, want: stateFailed, code: ebsv1.PackageRepoSyncTimeout},
		{name: "transport timeout", err: &net.DNSError{Err: "timeout", IsTimeout: true}, want: stateFailed, code: ebsv1.PackageRepoSyncTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &fakeGitClient{check: func(context.Context) (gitserver.SyncCheckResult, error) {
				return gitserver.SyncCheckResult{}, tc.err
			}}
			c := newTestController(t, &fakeClient{}, g, Config{})
			task := resolveTask{repo: ebsv1.PackageRepo{Name: "pkg", URL: "https://example.com/pkg.git", Ref: "refs/heads/main"}}
			got := c.resolveOne(context.Background(), g, task, time.Time{})
			if got.state != tc.want || g.checks != 1 {
				t.Fatalf("result=%+v calls=%d", got, g.checks)
			}
			if tc.code != "" && (got.status.Error == nil || got.status.Error.Code != tc.code) {
				t.Fatalf("status=%+v", got.status)
			}
		})
	}
}

func TestCommitConflictPreservesCloneURLAndCanonicalIdentity(t *testing.T) {
	ref := "refs/heads/main"
	first := ebsv1.PackageRepo{Name: "first", URL: "https://EXAMPLE.com:443/team/pkg", Ref: ref}
	second := ebsv1.PackageRepo{Name: "second", URL: "git@example.com:team/pkg.git", Ref: ref}
	snapshot, build := baseObjects([]ebsv1.PackageRepo{first, second})
	snapshot.Status.PackageRepoStatuses = map[string]ebsv1.PackageRepoStatus{"first": {CommitID: strings.Repeat("a", 40)}}
	c := newTestController(t, &fakeClient{snapshot: snapshot, build: build}, &fakeGitClient{}, Config{})
	c.mergeResults(snapshot, []resolveResult{{repo: second, state: stateResolved, status: ebsv1.PackageRepoStatus{CommitID: strings.Repeat("b", 40), CloneURL: "git-server:///git/example.com/pkg.git"}}})
	got := snapshot.Status.PackageRepoStatuses["second"]
	if got.Error == nil || got.Error.Code != ebsv1.PackageRepoCommitConflict || got.CommitID != "" || got.CloneURL != "git-server:///git/example.com/pkg.git" {
		t.Fatalf("status=%+v", got)
	}
}
