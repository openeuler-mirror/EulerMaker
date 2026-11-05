package rpmrepo

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	clientpkg "controller-manager/pkg/clients/apiserver"
	ebsv1 "ebs-api/ebs/v1"
)

func TestDirectRpmRepoDeletionDrainsBeforeFinalizing(t *testing.T) {
	client, artifacts := NewFakeClient(), NewFakeArtifactManager()
	repo := newRpmRepo(testBuild)
	now := metav1.Now()
	repo.DeletionTimestamp = &now
	repo.Finalizers = []string{ebsv1.RpmRepoCleanupFinalizer, "other/controller"}
	repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseFailed}
	repo.Status.Repository = &ebsv1.RpmRepoRepositoryStatus{Transition: &ebsv1.RepositoryTransition{RepositoryUID: "pending"}}
	client.RpmRepos[key(testProject, testBuild)] = repo
	drained := false
	artifacts.StopBuildRepositoriesFunc = func(context.Context, string, string, string) (bool, error) {
		return drained, nil
	}
	c := newTestController(t, client, artifacts, testConfig())
	run := func() {
		t.Helper()
		if _, err := c.sync(context.Background(), key(testProject, testBuild)); err != nil {
			t.Fatal(err)
		}
	}
	run()
	if len(client.Updates) != 0 || client.RpmRepos[key(testProject, testBuild)].Status.Repository.Transition == nil {
		t.Fatal("pending work must retain the checkpoint and finalizer")
	}
	drained = true
	run()
	ack := client.RpmRepos[key(testProject, testBuild)].DeepCopy()
	if !meta.IsStatusConditionTrue(ack.Status.Conditions, ebsv1.RpmRepoConditionCleanupReady) || len(client.Updates) != 0 {
		t.Fatal("acknowledgment must precede finalizer removal")
	}
	// A restarted controller finishes deletion from the persisted acknowledgment.
	c = newTestController(t, client, artifacts, testConfig())
	artifacts.DeleteBuildRepositoriesFunc = func(context.Context, string, string, string) (bool, error) {
		return false, nil
	}
	run()
	if len(client.Updates) != 0 {
		t.Fatal("removed finalizer before repository cleanup completed")
	}
	artifacts.DeleteBuildRepositoriesFunc = func(context.Context, string, string, string) (bool, error) {
		return true, nil
	}
	run()
	current := client.RpmRepos[key(testProject, testBuild)]
	if !slices.Equal(current.Finalizers, []string{"other/controller"}) || !reflect.DeepEqual(current.Status, ack.Status) {
		t.Fatalf("cleanup changed business status or another finalizer: %+v", current)
	}
	if len(artifacts.SubmitRepositoryRequests) != 0 || len(artifacts.SubmitReleaseRequests) != 0 {
		t.Fatal("deletion submitted new work")
	}
	if !reflect.DeepEqual(artifacts.DeleteBuildRequests, [][3]string{
		{testProject, testArch, testBuild}, {testProject, testArch, testBuild},
	}) {
		t.Fatalf("unexpected cleanup scope: %v", artifacts.DeleteBuildRequests)
	}
}

func TestRepositoryCleanupFailureRetainsFinalizer(t *testing.T) {
	client, artifacts := NewFakeClient(), NewFakeArtifactManager()
	repo := newRpmRepo(testBuild)
	now := metav1.Now()
	repo.DeletionTimestamp, repo.Finalizers = &now, []string{ebsv1.RpmRepoCleanupFinalizer}
	repo.Status.Conditions = []metav1.Condition{{Type: ebsv1.RpmRepoConditionCleanupReady, Status: metav1.ConditionTrue}}
	client.RpmRepos[key(testProject, testBuild)] = repo
	artifacts.DeleteBuildRepositoriesFunc = func(context.Context, string, string, string) (bool, error) {
		return false, fmt.Errorf("cleanup response lost")
	}
	c := newTestController(t, client, artifacts, testConfig())
	if _, err := c.sync(context.Background(), key(testProject, testBuild)); err == nil || len(client.Updates) != 0 {
		t.Fatalf("unconfirmed cleanup removed finalizer: %v", err)
	}
	artifacts.DeleteBuildRepositoriesFunc = func(context.Context, string, string, string) (bool, error) { return true, nil }
	if _, err := c.sync(context.Background(), key(testProject, testBuild)); err != nil || len(client.Updates) != 1 {
		t.Fatalf("cleanup retry: %v, updates=%d", err, len(client.Updates))
	}
}

func TestCleanupDrainsPreparedReleaseWithoutActivation(t *testing.T) {
	client, artifacts := NewFakeClient(), NewFakeArtifactManager()
	build := newBuild(testBuild)
	now := metav1.Now()
	build.DeletionTimestamp = &now
	client.Builds[key(testProject, testBuild)] = build
	repo := newRpmRepo(testBuild)
	repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{
		Phase: ebsv1.RpmRepoReleasePrepared, Transition: &ebsv1.ReleaseTransition{SourceRepositoryUID: "source"},
	}
	client.RpmRepos[key(testProject, testBuild)] = repo
	c := newTestController(t, client, artifacts, testConfig())
	if _, err := c.sync(context.Background(), key(testProject, testBuild)); err != nil {
		t.Fatal(err)
	}
	current := client.RpmRepos[key(testProject, testBuild)]
	if current.Status.Release.Transition != nil || !meta.IsStatusConditionTrue(current.Status.Conditions, ebsv1.RpmRepoConditionCleanupReady) {
		t.Fatalf("prepared release not drained: %+v", current.Status)
	}
	if len(artifacts.ActivateReleaseNames) != 0 || len(artifacts.DeleteBuildRequests) != 0 {
		t.Fatal("parent deletion must only drain work, not activate or physically remove repositories")
	}
}

func TestRpmRepoFinalizerWriteRecovery(t *testing.T) {
	for _, outcome := range []string{"success", "conflict", "unknown-applied", "unknown-unapplied"} {
		t.Run(outcome, func(t *testing.T) {
			client, artifacts := NewFakeClient(), NewFakeArtifactManager()
			repo := newRpmRepo(testBuild)
			now := metav1.Now()
			repo.DeletionTimestamp, repo.Finalizers = &now, []string{ebsv1.RpmRepoCleanupFinalizer}
			repo.Status.Conditions = []metav1.Condition{{Type: ebsv1.RpmRepoConditionCleanupReady, Status: metav1.ConditionTrue}}
			client.RpmRepos[key(testProject, testBuild)] = repo
			if outcome != "success" {
				client.UpdateHook = func(request, current *ebsv1.RpmRepo) (*ebsv1.RpmRepo, error) {
					if outcome == "unknown-applied" {
						delete(client.RpmRepos, key(testProject, testBuild))
					}
					write := &clientpkg.WriteError{Outcome: clientpkg.WriteUnknown, Err: fmt.Errorf("response lost")}
					if outcome == "conflict" {
						write.Outcome, write.StatusCode = clientpkg.WriteRejected, 409
					}
					return nil, write
				}
			}
			c := newTestController(t, client, artifacts, testConfig())
			_, err := c.sync(context.Background(), key(testProject, testBuild))
			pending := outcome == "conflict" || outcome == "unknown-unapplied"
			if (err != nil) != pending || len(client.Updates) != 1 {
				t.Fatalf("pending=%t writes=%d error=%v", pending, len(client.Updates), err)
			}
			if pending {
				current := client.RpmRepos[key(testProject, testBuild)]
				if !slices.Contains(current.Finalizers, ebsv1.RpmRepoCleanupFinalizer) {
					t.Fatal("unconfirmed removal lost finalizer")
				}
				current.ResourceVersion = "99"
				client.UpdateHook = nil
				if _, err := c.sync(context.Background(), key(testProject, testBuild)); err != nil {
					t.Fatal(err)
				}
				if client.Updates[1].ResourceVersion != "99" {
					t.Fatal("replayed stale resource version")
				}
			}
			if client.RpmRepos[key(testProject, testBuild)] != nil {
				t.Fatal("RpmRepo was not removed")
			}
		})
	}
}

func TestDeletingBuildWaitsForSubmissionFence(t *testing.T) {
	client, artifacts := NewFakeClient(), NewFakeArtifactManager()
	build := newBuild(testBuild)
	now := metav1.Now()
	build.DeletionTimestamp = &now
	client.Builds[key(testProject, testBuild)] = build
	repo := newRpmRepo(testBuild)
	repo.Status.Repository = &ebsv1.RpmRepoRepositoryStatus{Transition: &ebsv1.RepositoryTransition{
		RepositoryUID: "inflight", Inputs: []ebsv1.RepositoryInput{{JobName: "job", SpecName: "gcc"}},
	}}
	// Failed releases may still carry an abandoned materialization checkpoint.
	repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseFailed}
	client.RpmRepos[key(testProject, testBuild)] = repo
	drained := false
	artifacts.StopBuildRepositoriesFunc = func(context.Context, string, string, string) (bool, error) {
		return drained, nil
	}
	c := newTestController(t, client, artifacts, testConfig())
	if _, err := c.sync(context.Background(), key(testProject, testBuild)); err != nil {
		t.Fatal(err)
	}
	if client.RpmRepos[key(testProject, testBuild)].Status.Repository.Transition == nil {
		t.Fatal("running work lost its checkpoint")
	}
	drained = true
	if _, err := c.sync(context.Background(), key(testProject, testBuild)); err != nil {
		t.Fatal(err)
	}
	if client.RpmRepos[key(testProject, testBuild)].Status.Repository.Transition != nil {
		t.Fatal("settled work retained its checkpoint")
	}
	if len(artifacts.SubmitRepositoryRequests) != 0 || len(artifacts.SubmitReleaseRequests) != 0 {
		t.Fatal("cleanup submitted new work")
	}
}

func TestDeletingBuildRetainsCheckpointUntilStopConfirmed(t *testing.T) {
	client, artifacts := NewFakeClient(), NewFakeArtifactManager()
	build := newBuild(testBuild)
	now := metav1.Now()
	build.DeletionTimestamp = &now
	client.Builds[key(testProject, testBuild)] = build
	repo := inFlightReleaseRepo(testBuild)
	repo.Status.Release.Phase = ebsv1.RpmRepoReleaseFailed
	client.RpmRepos[key(testProject, testBuild)] = repo
	drained := false
	stopErr := fmt.Errorf("request outcome unknown")
	artifacts.StopBuildRepositoriesFunc = func(context.Context, string, string, string) (bool, error) {
		return drained, stopErr
	}
	c := newTestController(t, client, artifacts, testConfig())
	for _, failure := range []error{stopErr, &artifactError{kind: artifactNotFound}} {
		stopErr = failure
		if _, err := c.sync(context.Background(), key(testProject, testBuild)); err == nil {
			t.Fatal("unknown release must block cleanup")
		}
	}
	stopErr = nil
	if result, err := c.sync(context.Background(), key(testProject, testBuild)); err != nil || result.RequeueAfter != 5*time.Second {
		t.Fatalf("pending stop: %+v, %v", result, err)
	}
	if client.RpmRepos[key(testProject, testBuild)].Status.Release.Transition == nil {
		t.Fatal("unresolved release checkpoint disappeared")
	}
	// A restarted controller can discard even a never-submitted request after the fence is confirmed.
	c = newTestController(t, client, artifacts, testConfig())
	drained = true
	if _, err := c.sync(context.Background(), key(testProject, testBuild)); err != nil {
		t.Fatal(err)
	}
	if client.RpmRepos[key(testProject, testBuild)].Status.Release.Transition != nil {
		t.Fatal("stopped release checkpoint not drained")
	}
	if len(artifacts.SubmitReleaseRequests) != 0 || len(artifacts.SubmitRepositoryRequests) != 0 {
		t.Fatal("deletion must not submit work")
	}
}

func TestDeletingBuildInfoDrainsFailedRelease(t *testing.T) {
	client, artifacts := NewFakeClient(), NewFakeArtifactManager()
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	now := metav1.Now()
	client.BuildInfos[key(testProject, testBuild)] = &ebsv1.BuildInfo{ObjectMeta: metav1.ObjectMeta{Name: testBuild, Namespace: testProject, DeletionTimestamp: &now}}
	repo := inFlightReleaseRepo(testBuild)
	repo.Status.Release.Phase = ebsv1.RpmRepoReleaseFailed
	client.RpmRepos[key(testProject, testBuild)] = repo
	c := newTestController(t, client, artifacts, testConfig())
	if _, err := c.sync(context.Background(), key(testProject, testBuild)); err != nil {
		t.Fatal(err)
	}
	if current := client.RpmRepos[key(testProject, testBuild)]; current.Status.Release.Transition != nil || !meta.IsStatusConditionTrue(current.Status.Conditions, ebsv1.RpmRepoConditionCleanupReady) {
		t.Fatal("BuildInfo deletion did not drain checkpoint")
	}
	if len(artifacts.SubmitReleaseRequests) != 0 || len(artifacts.SubmitRepositoryRequests) != 0 {
		t.Fatal("cleanup created external work")
	}
}

func TestCleanupAcknowledgmentStopsFutureProduction(t *testing.T) {
	client, artifacts := NewFakeClient(), NewFakeArtifactManager()
	build := newBuild(testBuild)
	now := metav1.Now()
	build.DeletionTimestamp = &now
	client.Builds[key(testProject, testBuild)] = build
	repo := newRpmRepo(testBuild)
	repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseSkipped}
	repo.Generation = 3
	repo.Status.Conditions = []metav1.Condition{{
		Type: ebsv1.RpmRepoConditionRepositoryReady, Status: metav1.ConditionTrue,
		Reason: ebsv1.RpmRepoReasonRepositoryCreated, LastTransitionTime: now,
	}}
	client.RpmRepos[key(testProject, testBuild)] = repo
	c := newTestController(t, client, artifacts, testConfig())
	if _, err := c.sync(context.Background(), key(testProject, testBuild)); err != nil {
		t.Fatal(err)
	}
	if !meta.IsStatusConditionTrue(client.RpmRepos[key(testProject, testBuild)].Status.Conditions, ebsv1.RpmRepoConditionCleanupReady) {
		t.Fatal("a terminal repository with no checkpoints still needs explicit acknowledgment")
	}
	conditions := client.RpmRepos[key(testProject, testBuild)].Status.Conditions
	ack := meta.FindStatusCondition(conditions, ebsv1.RpmRepoConditionCleanupReady)
	if ack.Reason != ebsv1.RpmRepoReasonOperationsDrained || ack.ObservedGeneration != 3 || ack.LastTransitionTime.IsZero() {
		t.Fatalf("incomplete acknowledgment: %+v", ack)
	}
	if !meta.IsStatusConditionTrue(conditions, ebsv1.RpmRepoConditionRepositoryReady) {
		t.Fatal("cleanup replaced the business condition")
	}
	// The producer remains stopped even when the parent or BuildInfo disappears after acknowledgment.
	delete(client.Builds, key(testProject, testBuild))
	writes := len(client.StatusWrites)
	if _, err := c.sync(context.Background(), key(testProject, testBuild)); err != nil {
		t.Fatal(err)
	}
	if len(client.StatusWrites) != writes || len(artifacts.SubmitRepositoryRequests) != 0 || len(artifacts.SubmitReleaseRequests) != 0 {
		t.Fatal("acknowledged repository resumed production")
	}
	if !reflect.DeepEqual(artifacts.StopBuildRequests, [][3]string{{testProject, testArch, testBuild}}) {
		t.Fatalf("unexpected stop scope or repeated stop: %v", artifacts.StopBuildRequests)
	}
}
