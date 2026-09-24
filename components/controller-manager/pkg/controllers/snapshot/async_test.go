package snapshot

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/clients/gitserver"
	ebsv1 "ebs-api/ebs/v1"
)

type asyncGit struct {
	mu                          sync.Mutex
	checks, publishes, resolves int
	synced                      bool
	publishErr                  error
}

func (g *asyncGit) CheckSynced(context.Context, string, time.Time) (gitserver.SyncCheckResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.checks++
	return gitserver.SyncCheckResult{Synced: g.synced, CloneURL: "git://mirror/pkg"}, nil
}
func (g *asyncGit) PublishSyncTask(context.Context, string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.publishes++
	return g.publishErr
}
func (g *asyncGit) ResolveCommit(context.Context, string, ebsv1.GitRef) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resolves++
	return strings.Repeat("a", 40), nil
}
func (g *asyncGit) ExecCommand(context.Context, string, string) (string, error) {
	panic("Snapshot Controller must not call ExecCommand")
}

func branchRepos(n int) []ebsv1.PackageRepo {
	repos := make([]ebsv1.PackageRepo, n)
	for i := range repos {
		repos[i] = ebsv1.PackageRepo{Name: fmt.Sprintf("pkg-%d", i), URL: fmt.Sprintf("https://example.com/pkg-%d.git", i), Ref: ebsv1.GitRef{Type: ebsv1.GitRefBranch, Value: "main"}}
	}
	return repos
}

func TestAsyncSyncAcrossReconciles(t *testing.T) {
	snapshot, build := baseObjects(branchRepos(5000))
	api := &fakeClient{snapshot: snapshot, build: build}
	g := &asyncGit{}
	c := newTestController(t, api, g, Config{})
	result, err := c.sync(context.Background(), "project-a/build-a")
	if err != nil || result.RequeueAfter != c.config.SyncRequeueDelay || api.snapshot.Status.Phase != ebsv1.SnapshotProcessing {
		t.Fatalf("result=%+v error=%v phase=%s", result, err, api.snapshot.Status.Phase)
	}
	if g.checks != 5000 || g.publishes != 5000 || g.resolves != 0 || len(api.snapshot.Status.PackageRepoStatuses) != 0 {
		t.Fatalf("requests=%+v statuses=%d", g, len(api.snapshot.Status.PackageRepoStatuses))
	}
	// Restart: no in-memory publication flag is needed to recover.
	g.synced = true
	c = newTestController(t, api, g, Config{})
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatal(err)
	}
	if g.checks != 10000 || g.publishes != 5000 || g.resolves != 5000 || api.snapshot.Status.Phase != ebsv1.SnapshotActive {
		t.Fatalf("requests=%+v phase=%s", g, api.snapshot.Status.Phase)
	}
}

func TestBudgetDefersAndRotatesWithoutConsumingRetries(t *testing.T) {
	snapshot, build := baseObjects(branchRepos(5))
	snapshot.Status.Phase = ebsv1.SnapshotProcessing
	snapshot.Status.PackageRepoStatuses = map[string]ebsv1.PackageRepoStatus{
		"pkg-4": {Error: &ebsv1.SpecCommitError{Code: ebsv1.SpecCommitSyncFailed, Message: "previous failure", Retryable: true}},
	}
	api := &fakeClient{snapshot: snapshot, build: build}
	calls := 0
	g := &checkingGit{check: func(ctx context.Context) (gitserver.SyncCheckResult, error) {
		calls++
		<-ctx.Done()
		return gitserver.SyncCheckResult{}, ctx.Err()
	}}
	c := newTestController(t, api, g, Config{})
	c.config.ResolveWorkers = 1
	c.config.ResolveBudget = 20 * time.Millisecond
	c.failures.set(snapshot.UID, "pkg-4", 2)
	original := snapshot.DeepCopy().Status
	for round := 0; round < 5; round++ {
		result, err := c.sync(context.Background(), "project-a/build-a")
		if err != nil || result.RequeueAfter != c.config.SyncRequeueDelay {
			t.Fatalf("round=%d result=%+v error=%v", round, result, err)
		}
		if calls != round+1 || c.failures.cursor(snapshot.UID) != (round+1)%5 {
			t.Fatalf("round=%d calls=%d cursor=%d", round, calls, c.failures.cursor(snapshot.UID))
		}
		if !reflect.DeepEqual(original, api.snapshot.Status) || c.failures.count(snapshot.UID, "pkg-4") != 2 {
			t.Fatalf("deferred tasks changed status or retries: %+v", api.snapshot.Status)
		}
	}
	if api.updates != 0 {
		t.Fatalf("deferred-only cycles wrote status %d times", api.updates)
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
	if got := c.requestFailure(context.Background(), task, "sync", context.DeadlineExceeded); got.state != stateFailed || got.status.Error.Code != ebsv1.SpecCommitSyncTimeout {
		t.Fatalf("request timeout: %+v", got)
	}
	parent, stop := context.WithCancel(context.Background())
	stop()
	if got := c.resolveOne(parent, task, time.Time{}); got.state != stateFatal || !errors.Is(got.fatal, context.Canceled) {
		t.Fatalf("manager canceled: %+v", got)
	}
}

func TestPublishFailureCountsOnceAndRecovers(t *testing.T) {
	snapshot, build := baseObjects(branchRepos(1))
	api := &fakeClient{snapshot: snapshot, build: build}
	g := &asyncGit{publishErr: &gitserver.Error{Kind: gitserver.ErrorTemporary, Err: errors.New("response lost")}}
	c := newTestController(t, api, g, Config{})
	if _, err := c.sync(context.Background(), "project-a/build-a"); err == nil || c.failures.count(snapshot.UID, "pkg-0") != 1 {
		t.Fatalf("err=%v count=%d", err, c.failures.count(snapshot.UID, "pkg-0"))
	}
	g.synced = true
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatal(err)
	}
	if g.publishes != 1 || api.snapshot.Status.Phase != ebsv1.SnapshotActive || api.snapshot.Status.PackageRepoStatuses["pkg-0"].Error != nil {
		t.Fatalf("requests=%+v status=%+v", g, api.snapshot.Status)
	}
}

func TestCursorLifecycle(t *testing.T) {
	tracker := newFailureTracker()
	tracker.observe("p/s", "old")
	tracker.setCursor("old", 42)
	tracker.observe("p/s", "new")
	tracker.setCursor("old", 43) // Late result must not resurrect old UID state.
	if tracker.cursor("old") != 0 {
		t.Fatal("old cursor retained")
	}
	tracker.setCursor("new", 2)
	tracker.clearObject("p/s")
	if tracker.cursor("new") != 0 {
		t.Fatal("deleted cursor retained")
	}
	tracker.observe("p/s", "new")
	tracker.setCursor("new", 3)
	tracker.clearUID("new")
	if tracker.cursor("new") != 0 {
		t.Fatal("terminal cursor retained")
	}
}

type conflictSnapshotClient struct{ *fakeClient }

func (c *conflictSnapshotClient) UpdateSnapshotStatus(context.Context, *ebsv1.Snapshot) (*ebsv1.Snapshot, error) {
	return nil, &clientpkg.WriteError{Outcome: clientpkg.WriteRejected, StatusCode: 409, Err: errors.New("conflict")}
}

func TestCursorAdvancesDespiteStatusConflict(t *testing.T) {
	snapshot, build := baseObjects(branchRepos(5))
	snapshot.Status.Phase = ebsv1.SnapshotProcessing
	api := &fakeClient{snapshot: snapshot, build: build}
	calls := 0
	g := &checkingGit{check: func(ctx context.Context) (gitserver.SyncCheckResult, error) {
		calls++
		if calls == 1 {
			return gitserver.SyncCheckResult{}, errors.New("unavailable")
		}
		<-ctx.Done()
		return gitserver.SyncCheckResult{}, ctx.Err()
	}}
	c := newTestController(t, api, g, Config{})
	c.client = &conflictSnapshotClient{fakeClient: api}
	c.config.ResolveWorkers = 1
	c.config.ResolveBudget = 20 * time.Millisecond
	result, err := c.sync(context.Background(), "project-a/build-a")
	if err != nil || !result.Requeue || c.failures.cursor(snapshot.UID) != 2 {
		t.Fatalf("result=%+v err=%v cursor=%d", result, err, c.failures.cursor(snapshot.UID))
	}
	if c.failures.count(snapshot.UID, "pkg-0") != 0 || len(api.snapshot.Status.PackageRepoStatuses) != 0 {
		t.Fatal("conflict committed pending failure state")
	}
}

func TestBudgetWaitsForWorkersAndPreservesStableOrder(t *testing.T) {
	snapshot, build := baseObjects(branchRepos(10))
	var mu sync.Mutex
	active, peak, calls := 0, 0, 0
	g := &checkingGit{check: func(ctx context.Context) (gitserver.SyncCheckResult, error) {
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
