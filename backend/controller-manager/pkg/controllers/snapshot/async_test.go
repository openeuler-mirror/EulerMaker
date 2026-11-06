package snapshot

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"controller-manager/pkg/clients/gitserver"
	ebsv1 "ebs-api/ebs/v1"
)

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
