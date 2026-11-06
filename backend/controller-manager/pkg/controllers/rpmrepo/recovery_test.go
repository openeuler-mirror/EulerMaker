package rpmrepo

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ebsv1 "ebs-api/ebs/v1"
)

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
		return ReleaseResponse{BuildName: buildName, State: ReleaseCreating, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	if _, err := syncReleaseForTest(c, context.Background(), testProject, testArch); err != nil {
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
		return ReleaseResponse{BuildName: req.BuildName, State: ReleaseCreating, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	if _, err := syncReleaseForTest(c, context.Background(), testProject, testArch); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(artifacts.SubmitReleaseRequests) != 0 || c.Queue().Len() == 0 {
		t.Fatalf("the other candidate must be queued, not executed under this key: %+v", artifacts.SubmitReleaseRequests)
	}
	r := &reconciler{controller: c, ctx: context.Background(), key: key(testProject, "build-b"), project: testProject, now: metav1.Now()}
	if _, err := r.reconcileRelease(testArch); err != nil {
		t.Fatal(err)
	}
	if len(artifacts.SubmitReleaseRequests) != 1 || artifacts.SubmitReleaseRequests[0].BuildName != "build-b" {
		t.Fatalf("ready candidate was not submitted on its own key: %+v", artifacts.SubmitReleaseRequests)
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
