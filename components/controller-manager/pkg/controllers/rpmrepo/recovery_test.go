package rpmrepo

import (
	"context"
	"testing"
	"time"

	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestReconcileBuildReRegistersAbandonedBatchTerminal(t *testing.T) {
	client := NewFakeClient()
	repo := inFlightRepo()
	conditions, _ := MergeCondition(nil, ebsv1.RpmRepoConditionRepositoryReady, metav1.ConditionFalse, ebsv1.RpmRepoReasonRepositoryCreationFailed, "", 1, metav1.Now())
	conditions, _ = MergeCondition(conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionFalse, ebsv1.RpmRepoReasonRepositoryCreationFailed, "", 1, metav1.Now())
	repo.Status.Conditions = conditions
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)

	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())
	failedBefore := repositoryFailed.Value()

	result, err := c.sync(context.Background(), buildKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result != (controller.ReconcileResult{}) {
		t.Fatalf("unexpected result %+v", result)
	}
	updated := client.RpmRepos[key(testProject, testBuild)]
	if updated.Status.Release == nil || updated.Status.Release.Phase != ebsv1.RpmRepoReleaseFailed {
		t.Fatalf("the abandoned batch must re-register its release terminal: %+v", updated.Status.Release)
	}
	if !conditionMatches(updated.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionFalse, ebsv1.RpmRepoReasonRepositoryCreationFailed) {
		t.Fatalf("PublishSucceed=False/RepositoryCreationFailed missing: %+v", updated.Status.Conditions)
	}
	if updated.Status.Repository.UpdatedAt != nil {
		t.Fatalf("re-registering the terminal must not write repository.*")
	}
	if updated.Status.Repository.Transition == nil || updated.Status.Repository.Transition.RepositoryUID != "next-1" {
		t.Fatalf("the abandoned batch must stay frozen: %+v", updated.Status.Repository.Transition)
	}
	if delta := repositoryFailed.Value() - failedBefore; delta != 0 {
		t.Fatalf("re-registering must not count another repository failure, delta %d", delta)
	}
	if len(artifacts.SubmitRepositoryRequests) != 0 || len(artifacts.GetRepositoryUIDs) != 0 {
		t.Fatalf("re-registering the terminal must not call Artifact Manager")
	}
}

func TestReconcileReleaseKeepsOnlyOneInFlightRelease(t *testing.T) {
	client := NewFakeClient()
	earlier := releasableRpmRepo(testBuild)
	earlier.CreationTimestamp = metav1.NewTime(time.Unix(1, 0))
	earlier.Status.Release = &ebsv1.RpmRepoReleaseStatus{
		Phase:      ebsv1.RpmRepoReleasePending,
		Transition: &ebsv1.ReleaseTransition{SourceRepositoryUID: "repo-1"},
	}
	later := releasableRpmRepo("build-b")
	later.CreationTimestamp = metav1.NewTime(time.Unix(2, 0))
	later.Status.Release = &ebsv1.RpmRepoReleaseStatus{
		Phase:      ebsv1.RpmRepoReleasePending,
		Transition: &ebsv1.ReleaseTransition{SourceRepositoryUID: "repo-b"},
	}
	client.RpmRepos[key(testProject, testBuild)] = earlier
	client.RpmRepos[key(testProject, "build-b")] = later
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*later.DeepCopy(), *earlier.DeepCopy()}
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.Builds[key(testProject, "build-b")] = newBuild("build-b")

	artifacts := NewFakeArtifactManager()
	artifacts.GetReleaseFunc = func(_ context.Context, buildName string) (ReleaseResponse, error) {
		return ReleaseResponse{BuildName: buildName, State: ReleaseCreating, Attempt: 1, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	if _, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(artifacts.GetReleaseBuildNames) != 1 || artifacts.GetReleaseBuildNames[0] != testBuild {
		t.Fatalf("only the earliest in-flight release may be driven, got %v", artifacts.GetReleaseBuildNames)
	}
	storedLater := client.RpmRepos[key(testProject, "build-b")]
	if storedLater.Status.Release == nil || storedLater.Status.Release.Phase != ebsv1.RpmRepoReleasePending {
		t.Fatalf("the waiting in-flight release must keep its checkpoint: %+v", storedLater.Status.Release)
	}
}

func TestReconcileReleaseSkipsCandidateThatIsNotReady(t *testing.T) {
	client := NewFakeClient()
	first := releasableRpmRepo(testBuild)
	first.CreationTimestamp = metav1.NewTime(time.Unix(1, 0))
	second := releasableRpmRepo("build-b")
	second.CreationTimestamp = metav1.NewTime(time.Unix(2, 0))
	client.RpmRepos[key(testProject, testBuild)] = first
	client.RpmRepos[key(testProject, "build-b")] = second
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*first.DeepCopy(), *second.DeepCopy()}
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.Builds[key(testProject, "build-b")] = newBuild("build-b")
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoProcessing)
	client.BuildInfos[key(testProject, "build-b")] = newBuildInfo("build-b", ebsv1.BuildInfoCompleted)
	client.Jobs[testProject] = []ebsv1.Job{
		newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0)),
	}

	artifacts := NewFakeArtifactManager()
	artifacts.SubmitReleaseFunc = func(_ context.Context, req CreateReleaseRequest) (ReleaseResponse, error) {
		return ReleaseResponse{BuildName: req.BuildName, State: ReleaseCreating, Attempt: 1, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	if _, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(artifacts.SubmitReleaseRequests) != 1 || artifacts.SubmitReleaseRequests[0].BuildName != "build-b" {
		t.Fatalf("a candidate that is not ready must be skipped for a ready one, got %+v", artifacts.SubmitReleaseRequests)
	}
	if storedFirst := client.RpmRepos[key(testProject, testBuild)]; storedFirst.Status.Release != nil {
		t.Fatalf("the skipped candidate must not persist release state: %+v", storedFirst.Status.Release)
	}
	storedSecond := client.RpmRepos[key(testProject, "build-b")]
	if storedSecond.Status.Release == nil || storedSecond.Status.Release.Transition == nil {
		t.Fatalf("the ready candidate must get its checkpoint: %+v", storedSecond.Status.Release)
	}
	if storedSecond.Status.Release.Phase != ebsv1.RpmRepoReleasePending && storedSecond.Status.Release.Phase != ebsv1.RpmRepoReleaseCreating {
		t.Fatalf("unexpected release phase after starting the candidate: %q", storedSecond.Status.Release.Phase)
	}
}

func TestReconcileBuildNoPublishableArtifactsWritesReleaseSideOnly(t *testing.T) {
	client := NewFakeClient()
	repo := newRpmRepo(testBuild)
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)

	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())

	if _, err := c.sync(context.Background(), buildKey(testProject, testBuild)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	updated := client.RpmRepos[key(testProject, testBuild)]
	if updated.Status.Repository.UpdatedAt != nil {
		t.Fatalf("the no-artifacts terminal must not write repository.*")
	}
	if conditionMatches(updated.Status.Conditions, ebsv1.RpmRepoConditionRepositoryReady, metav1.ConditionFalse, ebsv1.RpmRepoReasonNoPublishableArtifacts) {
		t.Fatalf("the no-artifacts terminal must not claim a repository result")
	}
	if !conditionMatches(updated.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionFalse, ebsv1.RpmRepoReasonNoPublishableArtifacts) {
		t.Fatalf("PublishSucceed=False/NoPublishableArtifacts missing: %+v", updated.Status.Conditions)
	}
}
