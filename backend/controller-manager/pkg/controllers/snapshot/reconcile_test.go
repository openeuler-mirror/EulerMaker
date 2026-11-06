package snapshot

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/clients/gitserver"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/serviceconfig"
	ebsv1 "ebs-api/ebs/v1"
)

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
	git := &checkingGit{}
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
				api.snapshot.Status.PackageRepoStatuses["pkg"].CommitID != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
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
