package build

import (
	"context"
	"slices"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	clientpkg "controller-manager/pkg/clients/apiserver"
	ebsv1 "ebs-api/ebs/v1"
)

func cleanupFixture(t *testing.T) (*fakeAPI, *Controller, *ebsv1.Build) {
	t.Helper()
	f := newFakeAPI()
	b := newBuild("project-a", "build-a", ebsv1.BuildTypeFull, nil)
	now := metav1.Now()
	b.DeletionTimestamp, b.Finalizers = &now, []string{ebsv1.BuildCleanupFinalizer, "another/controller"}
	b.Status.Phase = ebsv1.BuildSuccess
	f.builds[key(b.Namespace, b.Name)] = b.DeepCopy()
	return f, newTestController(t, f, newTestClock()), b
}

func TestBuildCleanupDelegatesRepositoryFinalization(t *testing.T) {
	f, c, b := cleanupFixture(t)
	k := key(b.Namespace, b.Name)
	f.rpmRepos[k] = &ebsv1.RpmRepo{ObjectMeta: metav1.ObjectMeta{
		Name: b.Name, Namespace: b.Namespace, UID: "repo", ResourceVersion: "1",
		Finalizers: []string{ebsv1.RpmRepoCleanupFinalizer},
	}}
	if result, err := c.sync(context.Background(), k); err != nil || result.RequeueAfter == 0 || !slices.Equal(f.cleanupWrites, []string{"rpmrepos"}) {
		t.Fatalf("repository deletion was not delegated: %v %v %v", result, err, f.cleanupWrites)
	}
	if f.rpmRepos[k].DeletionTimestamp == nil {
		t.Fatal("DELETE did not mark RpmRepo")
	}
	if _, err := c.sync(context.Background(), k); err != nil || len(f.cleanupWrites) != 1 {
		t.Fatalf("must wait for RpmRepo Controller: %v %v", err, f.cleanupWrites)
	}
	delete(f.rpmRepos, k)
	if _, err := c.sync(context.Background(), k); err != nil || slices.Contains(f.builds[k].Finalizers, ebsv1.BuildCleanupFinalizer) {
		t.Fatalf("Build did not finalize after RpmRepo disappeared: %v", err)
	}
}

func TestBuildCleanupDeletesDirectChildren(t *testing.T) {
	f, c, b := cleanupFixture(t)
	k := key(b.Namespace, b.Name)
	f.snapshots[k] = &ebsv1.Snapshot{ObjectMeta: metav1.ObjectMeta{
		Name: b.Name, Namespace: b.Namespace, UID: "snapshot", ResourceVersion: "1",
	}}
	f.rpmRepos[k] = &ebsv1.RpmRepo{ObjectMeta: metav1.ObjectMeta{
		Name: b.Name, Namespace: b.Namespace, UID: "repo", ResourceVersion: "1",
	}, Status: ebsv1.RpmRepoStatus{Conditions: []metav1.Condition{{
		Type: ebsv1.RpmRepoConditionCleanupReady, Status: metav1.ConditionTrue,
		Reason: ebsv1.RpmRepoReasonOperationsDrained, LastTransitionTime: metav1.Now(),
	}}}}
	// Missing BuildInfo completes that dependency without invoking descendant APIs.
	f.cleanupFail = "rpmrepos"
	if _, err := c.sync(context.Background(), k); err == nil {
		t.Fatal("expected deletion error")
	}
	if !slices.Contains(f.builds[k].Finalizers, ebsv1.BuildCleanupFinalizer) {
		t.Fatal("finalized before direct children disappeared")
	}
	f.cleanupFail = ""
	for range 2 {
		if _, err := c.sync(context.Background(), k); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(f.cleanupWrites, []string{"snapshots", "rpmrepos", "rpmrepos"}) {
		t.Fatalf("direct child deletes = %v", f.cleanupWrites)
	}
	result := f.builds[k]
	if !slices.Equal(result.Finalizers, []string{"another/controller"}) || result.Status.Phase != ebsv1.BuildSuccess {
		t.Fatalf("cleanup changed business state or other finalizers: %+v", result)
	}
}

func TestBuildCleanupDeletesSnapshotWhileWaitingForBuildInfo(t *testing.T) {
	f, c, b := cleanupFixture(t)
	k := key(b.Namespace, b.Name)
	info := &ebsv1.BuildInfo{ObjectMeta: metav1.ObjectMeta{
		Name: b.Name, Namespace: b.Namespace, UID: "info", ResourceVersion: "1",
		Finalizers: []string{ebsv1.BuildInfoCleanupFinalizer},
	}}
	f.buildInfos[k] = info
	f.snapshots[k] = &ebsv1.Snapshot{ObjectMeta: metav1.ObjectMeta{
		Name: b.Name, Namespace: b.Namespace, UID: "snapshot", ResourceVersion: "1",
	}}
	f.rpmRepos[k] = &ebsv1.RpmRepo{ObjectMeta: metav1.ObjectMeta{
		Name: b.Name, Namespace: b.Namespace, UID: "repo", ResourceVersion: "1",
	}}
	run := func() {
		t.Helper()
		if result, err := c.sync(context.Background(), k); err != nil || result.RequeueAfter == 0 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	}
	run() // Mark BuildInfo and delete Snapshot in the same reconcile.
	if f.snapshots[k] != nil || info.DeletionTimestamp == nil {
		t.Fatal("Snapshot deletion waited for BuildInfo cleanup")
	}
	run() // Wait for its controller.
	info.Finalizers = []string{"other/controller"}
	run() // Other finalizer owners also need to finish.
	if !slices.Equal(f.cleanupWrites, []string{"buildinfos", "snapshots"}) || f.rpmRepos[k] == nil ||
		!slices.Contains(f.builds[k].Finalizers, ebsv1.BuildCleanupFinalizer) {
		t.Fatalf("repository cleanup must wait for BuildInfo: writes=%v", f.cleanupWrites)
	}
	delete(f.buildInfos, k)
	for range 2 {
		if _, err := c.sync(context.Background(), k); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(f.cleanupWrites, []string{"buildinfos", "snapshots", "rpmrepos"}) ||
		!slices.Equal(f.builds[k].Finalizers, []string{"another/controller"}) {
		t.Fatal("Build was not finalized after ordered direct child removal")
	}
}

func TestBuildCleanupRetriesSnapshotWhileBuildInfoIsDeleting(t *testing.T) {
	f, c, b := cleanupFixture(t)
	k := key(b.Namespace, b.Name)
	f.buildInfos[k] = &ebsv1.BuildInfo{ObjectMeta: metav1.ObjectMeta{
		Name: b.Name, Namespace: b.Namespace, UID: "info", ResourceVersion: "1",
		DeletionTimestamp: b.DeletionTimestamp, Finalizers: []string{ebsv1.BuildInfoCleanupFinalizer},
	}}
	f.snapshots[k] = &ebsv1.Snapshot{ObjectMeta: metav1.ObjectMeta{
		Name: b.Name, Namespace: b.Namespace, UID: "snapshot", ResourceVersion: "1",
	}}
	f.hooks.getRpmRepo = func(_, _ string) (*ebsv1.RpmRepo, error) {
		t.Fatal("RpmRepo must not be queried before BuildInfo disappears")
		return nil, nil
	}
	f.cleanupFail = "snapshots"
	if _, err := c.sync(context.Background(), k); err == nil {
		t.Fatal("expected Snapshot deletion error")
	}
	f.cleanupFail = ""
	if result, err := c.sync(context.Background(), k); err != nil || result.RequeueAfter == 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if f.snapshots[k] != nil || !slices.Equal(f.cleanupWrites, []string{"snapshots", "snapshots"}) {
		t.Fatalf("Snapshot deletion did not recover independently: writes=%v", f.cleanupWrites)
	}
	if !slices.Contains(f.builds[k].Finalizers, ebsv1.BuildCleanupFinalizer) {
		t.Fatal("Build finalized while BuildInfo was still deleting")
	}
}

func TestBuildCleanupRetriesChildRead(t *testing.T) {
	for _, resource := range []string{"buildinfos", "snapshots", "rpmrepos"} {
		t.Run(resource, func(t *testing.T) {
			f, c, b := cleanupFixture(t)
			k := key(b.Namespace, b.Name)
			readErr := apierrors.NewServiceUnavailable("read failed")
			switch resource {
			case "buildinfos":
				f.hooks.getBuildInfo = func(_, _ string) (*ebsv1.BuildInfo, error) { return nil, readErr }
			case "snapshots":
				f.hooks.getSnapshot = func(_, _ string) (*ebsv1.Snapshot, error) { return nil, readErr }
			case "rpmrepos":
				f.hooks.getRpmRepo = func(_, _ string) (*ebsv1.RpmRepo, error) { return nil, readErr }
			}
			if _, err := c.sync(context.Background(), k); err == nil {
				t.Fatal("expected child read error")
			}
			if !slices.Contains(f.builds[k].Finalizers, ebsv1.BuildCleanupFinalizer) {
				t.Fatal("child read error was treated as absence")
			}
			f.hooks.getBuildInfo, f.hooks.getSnapshot, f.hooks.getRpmRepo = nil, nil, nil
			if _, err := c.sync(context.Background(), k); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(f.builds[k].Finalizers, []string{"another/controller"}) {
				t.Fatal("cleanup did not recover")
			}
		})
	}
}

func TestBuildCleanupWaitsForDeletingSnapshot(t *testing.T) {
	f, c, b := cleanupFixture(t)
	k := key(b.Namespace, b.Name)
	f.snapshots[k] = &ebsv1.Snapshot{ObjectMeta: metav1.ObjectMeta{
		Name: b.Name, Namespace: b.Namespace, UID: "snapshot", ResourceVersion: "1",
		DeletionTimestamp: b.DeletionTimestamp,
	}}
	if result, err := c.sync(context.Background(), k); err != nil || result.RequeueAfter == 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(f.cleanupWrites) != 0 || !slices.Contains(f.builds[k].Finalizers, ebsv1.BuildCleanupFinalizer) {
		t.Fatal("must wait for Snapshot disappearance without repeating DELETE")
	}
	delete(f.snapshots, k)
	if _, err := c.sync(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.builds[k].Finalizers, []string{"another/controller"}) {
		t.Fatal("Build did not finalize after Snapshot disappeared")
	}
}

func TestBuildCleanupHandlesChildDisappearingDuringDelete(t *testing.T) {
	for _, resource := range []string{"buildinfos", "snapshots", "rpmrepos"} {
		t.Run(resource, func(t *testing.T) {
			f, c, b := cleanupFixture(t)
			k := key(b.Namespace, b.Name)
			metadata := metav1.ObjectMeta{
				Name: b.Name, Namespace: b.Namespace, UID: "child", ResourceVersion: "1",
			}
			switch resource {
			case "buildinfos":
				metadata.Finalizers = []string{ebsv1.BuildInfoCleanupFinalizer}
				f.buildInfos[k] = &ebsv1.BuildInfo{ObjectMeta: metadata}
			case "snapshots":
				f.snapshots[k] = &ebsv1.Snapshot{ObjectMeta: metadata}
			case "rpmrepos":
				f.rpmRepos[k] = &ebsv1.RpmRepo{ObjectMeta: metadata}
			}
			f.hooks.deleteResource = func(gvr schema.GroupVersionResource, project, name string, _ clientpkg.DeletePreconditions) error {
				target := gvr.Resource
				if project != b.Namespace || target != resource || name != b.Name {
					t.Fatalf("unexpected delete: %s/%s/%s", project, target, name)
				}
				delete(f.buildInfos, k)
				delete(f.snapshots, k)
				delete(f.rpmRepos, k)
				return apierrors.NewNotFound(ebsv1.Resource(target), name)
			}
			if result, err := c.sync(context.Background(), k); err != nil || result.RequeueAfter != 0 {
				t.Fatalf("cleanup result=%+v err=%v", result, err)
			}
			if !slices.Equal(f.builds[k].Finalizers, []string{"another/controller"}) {
				t.Fatal("Build did not finalize after DELETE confirmed absence")
			}
		})
	}
}
