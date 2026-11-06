package rpmrepo

import (
	"context"
	"testing"
	"time"

	ebsv1 "ebs-api/ebs/v1"
)

func TestReconcileReleaseBoundsSameRoundReplays(t *testing.T) {
	client := NewFakeClient()
	repo := releasableRpmRepo(testBuild)
	repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{
		Phase:      ebsv1.RpmRepoReleasePending,
		Transition: &ebsv1.ReleaseTransition{SourceRepositoryUID: "repo-1"},
	}
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)

	artifacts := NewFakeArtifactManager()
	artifacts.GetReleaseFunc = func(context.Context, string) (ReleaseResponse, error) {
		return ReleaseResponse{}, &artifactError{operation: "get-release", kind: artifactNotFound, code: "ReleaseNotFound"}
	}
	artifacts.SubmitReleaseFunc = func(context.Context, CreateReleaseRequest) (ReleaseResponse, error) {
		return ReleaseResponse{}, &artifactError{operation: "submit-release", kind: artifactNotFound, code: "ReleaseNotFound"}
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := syncReleaseForTest(c, context.Background(), testProject, testArch)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(artifacts.SubmitReleaseRequests) != 1 {
		t.Fatalf("expected exactly one same-round replay, got %d", len(artifacts.SubmitReleaseRequests))
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("a deferred replay must ask for a later round, got %+v", result)
	}
	if _, err := syncReleaseForTest(c, context.Background(), testProject, testArch); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if len(artifacts.SubmitReleaseRequests) != 2 {
		t.Fatalf("each round may replay once, got %d submits after two rounds", len(artifacts.SubmitReleaseRequests))
	}
}

func TestReconcileReleaseBoundsActivationRetries(t *testing.T) {
	client := NewFakeClient()
	repo := releasableRpmRepo(testBuild)
	repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{
		Phase:      ebsv1.RpmRepoReleasePrepared,
		Transition: &ebsv1.ReleaseTransition{SourceRepositoryUID: "repo-1"},
	}
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)

	artifacts := NewFakeArtifactManager()
	artifacts.GetReleaseFunc = func(_ context.Context, buildName string) (ReleaseResponse, error) {
		return ReleaseResponse{BuildName: buildName, State: ReleasePrepared, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	artifacts.ActivateReleaseFunc = func(_ context.Context, buildName string) (ReleaseResponse, error) {
		// Activation never converges: the round must still stop after one activation.
		return ReleaseResponse{BuildName: buildName, State: ReleasePrepared, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := syncReleaseForTest(c, context.Background(), testProject, testArch)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(artifacts.ActivateReleaseNames) != 1 {
		t.Fatalf("expected exactly one activation per round, got %d", len(artifacts.ActivateReleaseNames))
	}
	if result.RequeueAfter != 5*time.Second {
		t.Fatalf("RequeueAfter = %s, want the poll delay", result.RequeueAfter)
	}
}

func TestReconcileReleaseDefersReadyWithoutContentURL(t *testing.T) {
	client := NewFakeClient()
	repo := releasableRpmRepo(testBuild)
	repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{
		Phase:      ebsv1.RpmRepoReleaseCreating,
		Transition: &ebsv1.ReleaseTransition{SourceRepositoryUID: "repo-1"},
	}
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)

	artifacts := NewFakeArtifactManager()
	artifacts.GetReleaseFunc = func(_ context.Context, buildName string) (ReleaseResponse, error) {
		return ReleaseResponse{BuildName: buildName, State: ReleaseReady, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	_, err := syncReleaseForTest(c, context.Background(), testProject, testArch)
	if err == nil {
		t.Fatalf("a ready release without contentURL must not be collected")
	}
	if len(client.StatusWrites) != 0 {
		t.Fatalf("the incomplete response must not write status, got %d writes", len(client.StatusWrites))
	}
}

func TestReconcileReleaseRejectsForeignReleaseResponse(t *testing.T) {
	client := NewFakeClient()
	repo := releasableRpmRepo(testBuild)
	repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{
		Phase:      ebsv1.RpmRepoReleaseCreating,
		Transition: &ebsv1.ReleaseTransition{SourceRepositoryUID: "repo-1"},
	}
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)

	artifacts := NewFakeArtifactManager()
	artifacts.GetReleaseFunc = func(context.Context, string) (ReleaseResponse, error) {
		return ReleaseResponse{BuildName: "other-build", State: ReleaseReady, ContentURL: "/repositories/other/", UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	if _, err := syncReleaseForTest(c, context.Background(), testProject, testArch); err == nil {
		t.Fatalf("a foreign release response must not be collected")
	}
	if len(client.StatusWrites) != 0 {
		t.Fatalf("a foreign response must not write status, got %d writes", len(client.StatusWrites))
	}
	if repo := client.RpmRepos[key(testProject, testBuild)]; repo.Status.Release.Transition == nil {
		t.Fatalf("the checkpoint must survive a contract violation")
	}
}

func TestReconcileReleaseRejectsResponseWithoutUpdatedAt(t *testing.T) {
	client := NewFakeClient()
	repo := releasableRpmRepo(testBuild)
	repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{
		Phase:      ebsv1.RpmRepoReleaseCreating,
		Transition: &ebsv1.ReleaseTransition{SourceRepositoryUID: "repo-1"},
	}
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)

	artifacts := NewFakeArtifactManager()
	artifacts.GetReleaseFunc = func(_ context.Context, buildName string) (ReleaseResponse, error) {
		return ReleaseResponse{BuildName: buildName, State: ReleaseCreating, PollAfterSeconds: 5}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	if _, err := syncReleaseForTest(c, context.Background(), testProject, testArch); err == nil {
		t.Fatalf("a release response without updatedAt must be rejected")
	}
	if len(client.StatusWrites) != 0 {
		t.Fatalf("an incomplete response must not write status, got %d writes", len(client.StatusWrites))
	}
}
