package snapshot

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/clients/gitserver"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/serviceconfig"
	ebsv1 "ebs-api/ebs/v1"
)

func TestServiceConfigFailurePreservesPackageBudget(t *testing.T) {
	snapshot, build := baseObjects([]ebsv1.PackageRepo{{Name: "pkg", URL: "https://example.com/pkg.git", Ref: "refs/heads/main"}})
	api := &fakeClient{snapshot: snapshot, build: build}
	c := newTestController(t, api, &fakeGitClient{}, Config{})
	services := c.services.(*fakeServices)
	services.err = &serviceconfig.Error{Err: context.DeadlineExceeded}
	for i := 0; i < 6; i++ {
		if _, err := c.sync(context.Background(), "project-a/build-a"); !serviceconfig.IsError(err) {
			t.Fatalf("expected configuration error: %v", err)
		}
		if c.failures.count(snapshot.UID, "pkg") != 0 || len(api.snapshot.Status.PackageRepoStatuses) != 0 {
			t.Fatal("configuration failure consumed package retry budget")
		}
	}
	services.err = nil
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatal(err)
	}
	if api.snapshot.Status.Phase != ebsv1.SnapshotActive {
		t.Fatalf("phase=%s", api.snapshot.Status.Phase)
	}
	loads := services.loads
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatal(err)
	}
	if services.loads != loads {
		t.Fatal("terminal Snapshot read service configuration")
	}
}

func TestPendingSnapshotResolvesRefAndBecomesActive(t *testing.T) {
	for _, tc := range []struct {
		name      string
		unknownAt int
	}{
		{name: "confirmed writes"},
		{name: "phase write unknown", unknownAt: 1},
		{name: "result write unknown", unknownAt: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := ebsv1.PackageRepo{Name: "pkg", URL: "https://example.com/pkg.git", Ref: "refs/heads/main"}
			snapshot, build := baseObjects([]ebsv1.PackageRepo{repo})
			api := &fakeClient{snapshot: snapshot, build: build, unknownAt: tc.unknownAt}
			c := newTestController(t, api, &fakeGitClient{}, Config{})
			result, err := c.sync(context.Background(), "project-a/build-a")
			if err != nil || result != (controller.ReconcileResult{}) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if api.updates != 2 || api.snapshot.Status.Phase != ebsv1.SnapshotActive ||
				api.snapshot.Status.PackageRepoStatuses["pkg"].CommitID != resolvedCommit {
				t.Fatalf("unexpected snapshot after reconcile: updates=%d status=%+v", api.updates, api.snapshot.Status)
			}
			assertTrackerCleared(t, c.failures, snapshot.UID)
		})
	}
}

func TestDeletingBuildStopsSnapshotResolution(t *testing.T) {
	for _, phase := range []ebsv1.SnapshotPhase{ebsv1.SnapshotPending, ebsv1.SnapshotProcessing} {
		t.Run(string(phase), func(t *testing.T) {
			repo := ebsv1.PackageRepo{Name: "pkg", URL: "https://example.com/pkg.git", Ref: "refs/heads/main"}
			snapshot, build := baseObjects([]ebsv1.PackageRepo{repo})
			snapshot.Status.Phase = phase
			now := metav1.Now()
			build.DeletionTimestamp = &now
			api := &fakeClient{snapshot: snapshot, build: build}
			c := newTestController(t, api, &fakeGitClient{}, Config{})
			c.failures.observe("project-a/build-a", snapshot.UID)
			c.failures.set(snapshot.UID, "pkg", 2)
			c.failures.setCursor(snapshot.UID, 1)
			for range 2 {
				if result, err := c.sync(context.Background(), "project-a/build-a"); err != nil || result != (controller.ReconcileResult{}) {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				assertTrackerCleared(t, c.failures, snapshot.UID)
			}
			wantUpdates := 0
			if phase == ebsv1.SnapshotPending {
				wantUpdates = 1
			}
			if api.updates != wantUpdates || api.snapshot.Status.Phase != ebsv1.SnapshotProcessing ||
				len(api.snapshot.Status.PackageRepoStatuses) != 0 || c.services.(*fakeServices).loads != 0 {
				t.Fatalf("updates=%d status=%+v service loads=%d", api.updates, api.snapshot.Status, c.services.(*fakeServices).loads)
			}
		})
	}
}

func TestOnlySingleBuildUsesPackages(t *testing.T) {
	repos := []ebsv1.PackageRepo{
		{Name: "one", Ref: "refs/heads/main"},
		{Name: "two", Ref: "refs/tags/v1"},
	}
	snapshot, build := baseObjects(repos)
	build.Spec.BuildType, build.Spec.Packages = "single", []string{"one"}
	api := &fakeClient{snapshot: snapshot, build: build}
	c := newTestController(t, api, &fakeGitClient{}, Config{})
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatal(err)
	}
	if len(api.snapshot.Status.PackageRepoStatuses) != 1 || api.snapshot.Status.PackageRepoStatuses["one"].CommitID == "" {
		t.Fatalf("unexpected statuses: %+v", api.snapshot.Status.PackageRepoStatuses)
	}

	snapshot, build = baseObjects(repos)
	build.Spec.BuildType, build.Spec.Packages = "incremental", []string{"one"}
	api = &fakeClient{snapshot: snapshot, build: build}
	c = newTestController(t, api, &fakeGitClient{}, Config{})
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatal(err)
	}
	if len(api.snapshot.Status.PackageRepoStatuses) != 2 {
		t.Fatalf("non-single build did not resolve all repositories: %+v", api.snapshot.Status.PackageRepoStatuses)
	}
}

func TestTemporaryFailureAtLimitIsSkipped(t *testing.T) {
	repo := ebsv1.PackageRepo{Name: "pkg", URL: "https://example.com/pkg.git", Ref: "refs/heads/main"}
	snapshot, build := baseObjects([]ebsv1.PackageRepo{repo})
	api := &fakeClient{snapshot: snapshot, build: build}
	gitErr := &gitserver.Error{Operation: "sync", Kind: gitserver.ErrorTemporary, Err: errors.New("unavailable")}
	config := Config{PollPeriod: time.Second, ResolveWorkers: 1, ResolveBudget: time.Minute, SyncRequeueDelay: time.Second, FailureLimit: 1, MaxRetries: 2}
	c := newTestController(t, api, &fakeGitClient{check: func(context.Context) (gitserver.SyncCheckResult, error) {
		return gitserver.SyncCheckResult{}, gitErr
	}}, config)
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatal(err)
	}
	status := api.snapshot.Status.PackageRepoStatuses["pkg"]
	if api.snapshot.Status.Phase != ebsv1.SnapshotActive || status.Error == nil || status.Error.Code != ebsv1.PackageRepoRetryExhausted || status.Error.Retryable {
		t.Fatalf("unexpected exhausted status: phase=%s status=%+v", api.snapshot.Status.Phase, status)
	}
}

func TestFullRefSyncAndResolveAcrossReconciles(t *testing.T) {
	for _, kind := range []gitserver.ErrorKind{"", gitserver.ErrorNotFound, gitserver.ErrorTemporary} {
		t.Run(string(kind), func(t *testing.T) {
			snapshot, build := baseObjects(branchRepos(1))
			build.Spec.BuildType = "single"
			build.Spec.Packages = []string{"pkg-0"}
			snapshot.Spec.PackageRepos[0].Ref = "refs/merge-requests/501/head"
			snapshot.Spec.DefaultRef = "refs/heads/default"
			api := &fakeClient{snapshot: snapshot, build: build}
			g := &fakeGitClient{unsynced: true}
			c := newTestController(t, api, g, Config{})
			result, err := c.sync(context.Background(), "project-a/build-a")
			if err != nil || result.RequeueAfter != c.config.SyncRequeueDelay || g.publishes != 1 || g.resolves != 0 {
				t.Fatalf("waiting result=%+v err=%v publishes=%d resolves=%d", result, err, g.publishes, g.resolves)
			}
			g.unsynced = false
			if kind != "" {
				g.resolveErr = &gitserver.Error{Kind: kind, Err: errors.New("resolve failed")}
			}
			_, err = c.sync(context.Background(), "project-a/build-a")
			status := api.snapshot.Status.PackageRepoStatuses["pkg-0"]
			if g.fullRef != snapshot.Spec.PackageRepos[0].Ref || g.resolves != 1 || g.publishes != 1 {
				t.Fatalf("ref=%q resolves=%d publishes=%d", g.fullRef, g.resolves, g.publishes)
			}
			switch kind {
			case "":
				if err != nil || status.CommitID != resolvedCommit || api.snapshot.Status.Phase != ebsv1.SnapshotActive {
					t.Fatalf("resolved status=%+v err=%v", status, err)
				}
			case gitserver.ErrorNotFound:
				if err != nil || status.Error == nil || status.Error.Code != ebsv1.PackageRepoResolveFailed ||
					status.Error.Retryable || status.CommitID != "" || api.snapshot.Status.Phase != ebsv1.SnapshotActive {
					t.Fatalf("missing ref status=%+v err=%v", status, err)
				}
			case gitserver.ErrorTemporary:
				if !errors.Is(err, errPackagesRetryable) || status.Error == nil || !status.Error.Retryable ||
					c.failures.count(snapshot.UID, "pkg-0") != 1 || api.snapshot.Status.Phase != ebsv1.SnapshotProcessing {
					t.Fatalf("retry status=%+v err=%v", status, err)
				}
				g.resolveErr = nil
				if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
					t.Fatal(err)
				}
				if api.snapshot.Status.Phase != ebsv1.SnapshotActive || c.failures.count(snapshot.UID, "pkg-0") != 0 {
					t.Fatal("successful retry must complete the Snapshot and clear the failure budget")
				}
			}
		})
	}
}

func TestAsyncSyncAcrossReconciles(t *testing.T) {
	snapshot, build := baseObjects(branchRepos(5000))
	api := &fakeClient{snapshot: snapshot, build: build}
	g := &fakeGitClient{unsynced: true}
	c := newTestController(t, api, g, Config{})
	result, err := c.sync(context.Background(), "project-a/build-a")
	if err != nil || result.RequeueAfter != c.config.SyncRequeueDelay || api.snapshot.Status.Phase != ebsv1.SnapshotProcessing {
		t.Fatalf("result=%+v error=%v phase=%s", result, err, api.snapshot.Status.Phase)
	}
	if g.checks != 5000 || g.publishes != 5000 || g.resolves != 0 || len(api.snapshot.Status.PackageRepoStatuses) != 0 {
		t.Fatalf("requests=%+v statuses=%d", g, len(api.snapshot.Status.PackageRepoStatuses))
	}
	// Restart: no in-memory publication flag is needed to recover.
	g.unsynced = false
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
		"pkg-4": {Error: &ebsv1.PackageRepoError{Code: ebsv1.PackageRepoSyncFailed, Message: "previous failure", Retryable: true}},
	}
	api := &fakeClient{snapshot: snapshot, build: build}
	calls := 0
	g := &fakeGitClient{check: func(ctx context.Context) (gitserver.SyncCheckResult, error) {
		calls++
		<-ctx.Done()
		return gitserver.SyncCheckResult{}, ctx.Err()
	}}
	c := newTestController(t, api, g, Config{})
	c.config.ResolveWorkers = 1
	c.config.ResolveBudget = 20 * time.Millisecond
	c.failures.observe("project-a/build-a", snapshot.UID)
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

func TestPublishFailureCountsOnceAndRecovers(t *testing.T) {
	snapshot, build := baseObjects(branchRepos(1))
	api := &fakeClient{snapshot: snapshot, build: build}
	g := &fakeGitClient{unsynced: true, publishErr: &gitserver.Error{Kind: gitserver.ErrorTemporary, Err: errors.New("response lost")}}
	c := newTestController(t, api, g, Config{})
	if _, err := c.sync(context.Background(), "project-a/build-a"); err == nil || c.failures.count(snapshot.UID, "pkg-0") != 1 {
		t.Fatalf("err=%v count=%d", err, c.failures.count(snapshot.UID, "pkg-0"))
	}
	g.unsynced = false
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatal(err)
	}
	if g.publishes != 1 || api.snapshot.Status.Phase != ebsv1.SnapshotActive || api.snapshot.Status.PackageRepoStatuses["pkg-0"].Error != nil {
		t.Fatalf("requests=%+v status=%+v", g, api.snapshot.Status)
	}
}

func TestCursorAdvancesDespiteStatusConflict(t *testing.T) {
	snapshot, build := baseObjects(branchRepos(5))
	snapshot.Status.Phase = ebsv1.SnapshotProcessing
	api := &fakeClient{snapshot: snapshot, build: build}
	calls := 0
	g := &fakeGitClient{check: func(ctx context.Context) (gitserver.SyncCheckResult, error) {
		calls++
		if calls == 1 {
			return gitserver.SyncCheckResult{}, errors.New("unavailable")
		}
		<-ctx.Done()
		return gitserver.SyncCheckResult{}, ctx.Err()
	}}
	c := newTestController(t, api, g, Config{})
	api.updateErr = &clientpkg.WriteError{Outcome: clientpkg.WriteRejected, StatusCode: 409, Err: errors.New("conflict")}
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

func TestSnapshotExitClearsTracker(t *testing.T) {
	for _, state := range []string{"not-found", "deleting", "active"} {
		t.Run(state, func(t *testing.T) {
			snapshot, build := baseObjects(branchRepos(1))
			api := &fakeClient{snapshot: snapshot, build: build}
			c := newTestController(t, api, &fakeGitClient{}, Config{})
			c.failures.observe("project-a/build-a", snapshot.UID)
			c.failures.set(snapshot.UID, "pkg-0", 2)
			c.failures.setCursor(snapshot.UID, 1)
			switch state {
			case "not-found":
				api.snapshot = nil
			case "deleting":
				now := metav1.Now()
				snapshot.DeletionTimestamp = &now
			case "active":
				snapshot.Status.Phase = ebsv1.SnapshotActive
			}
			if result, err := c.sync(context.Background(), "project-a/build-a"); err != nil || result != (controller.ReconcileResult{}) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			assertTrackerCleared(t, c.failures, snapshot.UID)
			if api.updates != 0 || c.services.(*fakeServices).loads != 0 {
				t.Fatal("exited Snapshot continued resolution")
			}
		})
	}
}

func TestDeletionDuringResolutionDoesNotRestoreTracker(t *testing.T) {
	snapshot, build := baseObjects(branchRepos(1))
	snapshot.Status.Phase = ebsv1.SnapshotProcessing
	api := &fakeClient{snapshot: snapshot, build: build}
	git := &fakeGitClient{}
	c := newTestController(t, api, git, Config{})
	git.check = func(context.Context) (gitserver.SyncCheckResult, error) {
		c.onDelete(snapshot)
		return gitserver.SyncCheckResult{}, errors.New("request failed after deletion")
	}
	if _, err := c.sync(context.Background(), "project-a/build-a"); !errors.Is(err, errPackagesRetryable) {
		t.Fatalf("expected retryable resolution failure, got %v", err)
	}
	assertTrackerCleared(t, c.failures, snapshot.UID)
}

func TestMissingSnapshotDuringPhaseWriteClearsTracker(t *testing.T) {
	snapshot, build := baseObjects(branchRepos(1))
	api := &fakeClient{
		snapshot: snapshot, build: build,
		updateErr: &clientpkg.WriteError{Outcome: clientpkg.WriteRejected, StatusCode: 404, Err: errors.New("deleted")},
	}
	c := newTestController(t, api, &fakeGitClient{}, Config{})
	c.failures.observe("project-a/build-a", snapshot.UID)
	c.failures.set(snapshot.UID, "pkg-0", 2)
	if result, err := c.sync(context.Background(), "project-a/build-a"); err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	assertTrackerCleared(t, c.failures, snapshot.UID)
	if c.services.(*fakeServices).loads != 0 {
		t.Fatal("deleted Snapshot started resolution")
	}
}

func TestWriteConfirmationClearsDeletedSnapshotTracker(t *testing.T) {
	for _, state := range []string{"rejected-not-found", "unknown-not-found", "unknown-replaced"} {
		t.Run(state, func(t *testing.T) {
			snapshot, build := baseObjects(branchRepos(1))
			api := &fakeClient{build: build}
			c := newTestController(t, api, &fakeGitClient{}, Config{})
			c.failures.observe("project-a/build-a", snapshot.UID)
			c.failures.set(snapshot.UID, "pkg-0", 2)
			c.failures.setCursor(snapshot.UID, 1)
			writeErr := &clientpkg.WriteError{Outcome: clientpkg.WriteUnknown, Err: errors.New("lost response")}
			if state == "rejected-not-found" {
				writeErr.Outcome, writeErr.StatusCode = clientpkg.WriteRejected, 404
			}
			if state == "unknown-replaced" {
				api.snapshot = snapshot.DeepCopy()
				api.snapshot.UID = "replacement-uid"
				c.failures.observe("project-a/build-a", api.snapshot.UID)
				c.failures.set(api.snapshot.UID, "pkg-0", 1)
			}
			confirmed := false
			result, err := c.handleWriteError(context.Background(), writeErr, snapshot, func() (controller.ReconcileResult, error) {
				confirmed = true
				return controller.ReconcileResult{}, nil
			})
			if err != nil || result != (controller.ReconcileResult{}) || confirmed {
				t.Fatalf("result=%+v err=%v confirmed=%t", result, err, confirmed)
			}
			assertTrackerCleared(t, c.failures, snapshot.UID)
			if state == "unknown-replaced" && c.failures.count(api.snapshot.UID, "pkg-0") != 1 {
				t.Fatal("confirmation cleared replacement state")
			}
		})
	}
}
