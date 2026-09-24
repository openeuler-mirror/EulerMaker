package build

import (
	"context"
	"errors"
	"reflect"
	"testing"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func unresolvedIncremental(base string) *ebsv1.Build {
	build := newBuild("project-a", "build-a", "incremental", nil)
	build.Status.BaseBuildRef = &ebsv1.BaseBuildRef{Name: base}
	return build
}

func snapshotWithCommits(name string, commits map[string]string) *ebsv1.Snapshot {
	statuses := make(map[string]ebsv1.PackageRepoStatus, len(commits))
	repos := make([]ebsv1.PackageRepo, 0, len(commits))
	for repo, commit := range commits {
		statuses[repo] = ebsv1.PackageRepoStatus{CommitID: commit}
		repos = append(repos, ebsv1.PackageRepo{Name: repo})
	}
	return &ebsv1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "project-a"},
		Spec:       ebsv1.SnapshotSpec{PackageRepos: repos},
		Status: ebsv1.SnapshotStatus{
			Phase: ebsv1.SnapshotActive, PackageRepoStatuses: statuses,
		},
	}
}

func TestIncrementalPackagesUsePublishedBaselineAndFailedPackages(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = unresolvedIncremental("build-prev")
	api.storeSnapshot(snapshotWithCommits("build-a", map[string]string{"gcc": "a", "glibc": "b", "zlib": "c"}))
	api.storeSnapshot(snapshotWithCommits("build-prev", map[string]string{"gcc": "a", "glibc": "old", "removed": "r"}))
	api.storeBuildInfo(&ebsv1.BuildInfo{
		ObjectMeta: metav1.ObjectMeta{Name: "build-prev", Namespace: "project-a"},
		Status:     ebsv1.BuildInfoStatus{Phase: ebsv1.BuildInfoCompleted, FailedPackages: []string{"gcc", "removed", "gcc"}},
	})
	api.storeRpmRepo(&ebsv1.RpmRepo{ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a"}})
	c := newTestController(t, api, newTestClock())
	result, err := c.sync(context.Background(), "project-a/build-a")
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	stored := api.build("project-a", "build-a")
	if !reflect.DeepEqual(stored.Spec.Packages, []string{"gcc", "glibc", "zlib"}) || stored.Status.Phase != ebsv1.BuildPrepared {
		t.Fatalf("Build after resolution: spec=%+v status=%+v", stored.Spec, stored.Status)
	}
	if api.CallCount("UpdateBuild ") != 1 || api.CallCount("UpdateBuildStatus ") != 1 || api.CallCount("GetRpmRepo") == 0 {
		t.Fatalf("unexpected first-round calls: %v", api.Calls())
	}
	if api.CallCount("GetBuildInfo project-a/build-prev") != 1 {
		t.Fatalf("historical BuildInfo read count is not one: %v", api.Calls())
	}
}

func TestIncrementalEmptySeedIsResolved(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = unresolvedIncremental("build-prev")
	api.storeSnapshot(snapshotWithCommits("build-a", map[string]string{"gcc": "a"}))
	api.storeSnapshot(snapshotWithCommits("build-prev", map[string]string{"gcc": "a"}))
	api.storeBuildInfo(&ebsv1.BuildInfo{ObjectMeta: metav1.ObjectMeta{Name: "build-prev", Namespace: "project-a"}, Status: ebsv1.BuildInfoStatus{Phase: ebsv1.BuildInfoCompleted}})
	c := newTestController(t, api, newTestClock())
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	stored := api.build("project-a", "build-a")
	if len(stored.Spec.Packages) != 0 || api.CallCount("UpdateBuild ") != 0 || api.CallCount("CreateRpmRepo ") != 1 {
		t.Fatalf("empty seed was not resolved: spec=%+v status=%+v calls=%v", stored.Spec, stored.Status, api.Calls())
	}
}

func TestIncrementalMissingHistoricalChildIsNotAnEmptyBaseline(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = unresolvedIncremental("build-prev")
	api.storeSnapshot(snapshotWithCommits("build-a", map[string]string{"gcc": "a"}))
	c := newTestController(t, api, newTestClock())
	if _, err := c.sync(context.Background(), "project-a/build-a"); !controller.IsPermanent(err) {
		t.Fatalf("missing historical Snapshot must be permanent, got %v", err)
	}
	if api.CallCount("UpdateBuild ") != 0 || api.CallCount("UpdateBuildStatus ") != 0 {
		t.Fatalf("missing history must not be treated as empty: %v", api.Calls())
	}
}

func TestIncrementalActiveSnapshotMissingPackageStatusFails(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = unresolvedIncremental("")
	snapshot := snapshotWithCommits("build-a", map[string]string{"gcc": "a"})
	delete(snapshot.Status.PackageRepoStatuses, "gcc")
	api.storeSnapshot(snapshot)
	c := newTestController(t, api, newTestClock())
	if _, err := c.sync(context.Background(), "project-a/build-a"); !controller.IsPermanent(err) {
		t.Fatalf("missing package status must be permanent, got %v", err)
	}
	if api.CallCount("UpdateBuild ") != 0 || api.CallCount("UpdateBuildStatus ") != 0 {
		t.Fatalf("incomplete Snapshot must not resolve packages: %v", api.Calls())
	}
}

func TestIncrementalUpdateUnknownConfirmsOriginalPackages(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(map[bool]string{false: "not applied", true: "applied"}[applied], func(t *testing.T) {
			api := newFakeAPI()
			api.builds[key("project-a", "build-a")] = unresolvedIncremental("")
			api.storeSnapshot(snapshotWithCommits("build-a", map[string]string{"gcc": "a"}))
			api.hooks.updateBuild = func(request *ebsv1.Build) (*ebsv1.Build, error) {
				if applied {
					if _, err := api.commitBuild(request); err != nil {
						t.Fatalf("commit: %v", err)
					}
				}
				return nil, &clientpkg.WriteError{Operation: "update", Outcome: clientpkg.WriteUnknown, Err: errors.New("response lost")}
			}
			c := newTestController(t, api, newTestClock())
			result, err := c.sync(context.Background(), "project-a/build-a")
			if err != nil {
				t.Fatalf("sync: %v", err)
			}
			if applied {
				if !reflect.DeepEqual(api.build("project-a", "build-a").Spec.Packages, []string{"gcc"}) || result != (controller.ReconcileResult{}) {
					t.Fatalf("applied write was not confirmed: result=%+v", result)
				}
			} else if result.RequeueAfter != conflictRequeueDelay || len(api.build("project-a", "build-a").Spec.Packages) != 0 {
				t.Fatalf("unapplied write was incorrectly confirmed: result=%+v", result)
			}
			if api.CallCount("UpdateBuild ") != 1 {
				t.Fatalf("unknown PUT was replayed: %v", api.Calls())
			}
		})
	}
}

func TestIncrementalSpecWriteDoesNotOverrideAbortedBuild(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = unresolvedIncremental("")
	api.storeSnapshot(snapshotWithCommits("build-a", map[string]string{"gcc": "a"}))
	api.hooks.updateBuild = func(*ebsv1.Build) (*ebsv1.Build, error) {
		aborted := api.build("project-a", "build-a")
		aborted.Status.Phase = ebsv1.BuildAborted
		api.storeBuild(aborted)
		return nil, &clientpkg.WriteError{Operation: "update", Outcome: clientpkg.WriteUnknown, Err: errors.New("response lost")}
	}
	c := newTestController(t, api, newTestClock())
	result, err := c.sync(context.Background(), "project-a/build-a")
	if err != nil || result != (controller.ReconcileResult{}) || api.CallCount("UpdateBuildStatus ") != 0 {
		t.Fatalf("aborted Build was advanced: result=%+v err=%v calls=%v", result, err, api.Calls())
	}
}

func TestIncrementalSpecWritePreflightConflictRequeues(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = unresolvedIncremental("")
	api.storeSnapshot(snapshotWithCommits("build-a", map[string]string{"gcc": "a"}))
	reads := 0
	api.hooks.getBuild = func(project, name string) (*ebsv1.Build, error) {
		reads++
		if reads == 2 {
			concurrent := api.build(project, name)
			concurrent.Annotations = map[string]string{"other-writer": "updated"}
			api.storeBuild(concurrent)
		}
		return api.build(project, name), nil
	}
	c := newTestController(t, api, newTestClock())
	result, err := c.sync(context.Background(), "project-a/build-a")
	if err != nil || result.RequeueAfter != conflictRequeueDelay || api.CallCount("UpdateBuild ") != 0 || api.CallCount("UpdateBuildStatus ") != 0 {
		t.Fatalf("conflict was not deferred: result=%+v err=%v calls=%v", result, err, api.Calls())
	}
}

func TestIncrementalSpecWriteIsIdempotentAcrossReconcile(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = unresolvedIncremental("")
	api.storeSnapshot(snapshotWithCommits("build-a", map[string]string{"gcc": "a"}))
	api.hooks.updateStatus = func(*ebsv1.Build) (*ebsv1.Build, error) {
		return nil, &clientpkg.WriteError{Operation: "update-status", Outcome: clientpkg.WriteRejected, StatusCode: 409, Err: errors.New("conflict")}
	}
	c := newTestController(t, api, newTestClock())
	if result, err := c.sync(context.Background(), "project-a/build-a"); err != nil || result.RequeueAfter == 0 {
		t.Fatalf("first sync: result=%+v err=%v", result, err)
	}
	api.hooks.updateStatus = nil
	if _, err := newTestController(t, api, newTestClock()).sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatalf("sync after restart: %v", err)
	}
	if api.CallCount("UpdateBuild ") != 1 || api.build("project-a", "build-a").Status.Phase != ebsv1.BuildPrepared {
		t.Fatalf("package write was replayed or phase did not advance: %v", api.Calls())
	}
}
