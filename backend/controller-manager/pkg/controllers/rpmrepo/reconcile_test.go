package rpmrepo

import (
	"context"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
)

func TestSkippableInputFailure(t *testing.T) {
	for _, code := range []string{
		"ManifestNotReady", "ManifestInvalid", "ManifestContainsNoPackages", "MaterializationInputExpired",
		"PackageMetadataInvalid", "PackageArchitectureMismatch", "PackageConflict",
	} {
		if !skippableInputFailure(code) {
			t.Fatalf("input error %q must be skippable", code)
		}
	}
	for _, code := range []string{"", "BaseRepositoryNotReady", "RepositoryFilesystemMismatch", "RepositoryCommandFailed"} {
		if skippableInputFailure(code) {
			t.Fatalf("batch error %q must not be skipped", code)
		}
	}
}

func TestReconcileSkipsOffendingInputAndRebatchesAfterRestart(t *testing.T) {
	client := NewFakeClient()
	repo := inFlightRepo()
	repo.Status.Repository.Transition.Inputs = append(repo.Status.Repository.Transition.Inputs,
		ebsv1.RepositoryInput{JobName: "job-b", SpecName: "glibc"})
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoProcessing)
	client.Jobs[testProject] = []ebsv1.Job{
		newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0)),
		newSucceededJob("job-b", "glibc", "uid-job-b", time.Unix(2, 0)),
	}
	artifacts := NewFakeArtifactManager()
	artifacts.GetRepositoryFunc = func(_ context.Context, uid string) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: uid, State: RepositoryFailed, Attempt: 1, UpdatedAt: time.Now(),
			Failure: &FailureInfo{Code: "ManifestInvalid", JobName: "job-a"}}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())
	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
	if err != nil || !result.Requeue {
		t.Fatalf("skip result = %+v, %v", result, err)
	}
	updated := client.RpmRepos[key(testProject, testBuild)]
	if updated.Status.Repository.Transition != nil || !reflect.DeepEqual(updated.Status.Repository.SkippedJobNames, []string{"job-a"}) {
		t.Fatalf("failed input was not durably skipped: %+v", updated.Status.Repository)
	}
	if updated.Status.Release != nil || len(updated.Status.Repository.SourceJobNames) != 0 {
		t.Fatalf("skipping must not create a release or mark input as materialized: %+v", updated.Status)
	}
	artifacts.SubmitRepositoryFunc = func(_ context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryCreating, Attempt: 1, UpdatedAt: time.Now()}, nil
	}
	c = newTestController(t, client, artifacts, testConfig())
	if _, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild)); err != nil {
		t.Fatalf("rebatch after restart: %v", err)
	}
	if len(artifacts.SubmitRepositoryRequests) != 1 || !reflect.DeepEqual(artifacts.SubmitRepositoryRequests[0].Manifests,
		[]ManifestReference{{JobName: "job-b"}}) {
		t.Fatalf("rebatch must contain only the healthy Job: %+v", artifacts.SubmitRepositoryRequests)
	}
}

func TestReconcileDoesNotSkipUnidentifiedManifestFailure(t *testing.T) {
	client := NewFakeClient()
	client.RpmRepos[key(testProject, testBuild)] = inFlightRepo()
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoProcessing)
	artifacts := NewFakeArtifactManager()
	artifacts.GetRepositoryFunc = func(_ context.Context, uid string) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: uid, State: RepositoryFailed, Attempt: 1, UpdatedAt: time.Now(),
			Failure: &FailureInfo{Code: "ManifestInvalid", JobName: "foreign-job"}}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())
	if _, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	updated := client.RpmRepos[key(testProject, testBuild)]
	if len(updated.Status.Repository.SkippedJobNames) != 0 || updated.Status.Release == nil || updated.Status.Release.Phase != ebsv1.RpmRepoReleaseFailed {
		t.Fatalf("unidentified failure must use the existing terminal path: %+v", updated.Status)
	}
}

func TestReconcileBuildSubmitsBatchAndWaitsForMaterialization(t *testing.T) {
	client := NewFakeClient()
	repo := newRpmRepo(testBuild)
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}

	artifacts := NewFakeArtifactManager()
	artifacts.SubmitRepositoryFunc = func(_ context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryCreating, Attempt: 1, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Fatalf("RequeueAfter = %s, want 5s", result.RequeueAfter)
	}
	if len(client.StatusWrites) != 1 {
		t.Fatalf("expected exactly the checkpoint write, got %d", len(client.StatusWrites))
	}
	transition := client.StatusWrites[0].Status.Repository.Transition
	if transition == nil || len(transition.Inputs) != 1 || transition.Inputs[0].JobName != "job-a" {
		t.Fatalf("checkpoint does not freeze the batch: %+v", transition)
	}
	if len(artifacts.SubmitRepositoryRequests) != 1 {
		t.Fatalf("expected one SubmitRepository call, got %d", len(artifacts.SubmitRepositoryRequests))
	}
	request := artifacts.SubmitRepositoryRequests[0]
	if request.RepositoryUID != transition.RepositoryUID || request.RepositoryName != testBuild || request.TargetArch != testArch {
		t.Fatalf("unexpected materialization request %+v", request)
	}
	if client.Jobs[testProject][0].Status.Phase != ebsv1.JobSucceeded {
		t.Fatalf("the controller must not modify Job status")
	}
}

func TestReconcileBuildPromotesBatchWithoutSeparateReleaseKey(t *testing.T) {
	client := NewFakeClient()
	repo := newRpmRepo(testBuild)
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}

	artifacts := NewFakeArtifactManager()
	artifacts.SubmitRepositoryFunc = func(_ context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryReady, Attempt: 1, ContentURL: "/repositories/v1/next/", UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("unexpected result %+v", result)
	}
	updated := client.RpmRepos[key(testProject, testBuild)]
	if updated.Status.Repository.RepositoryUID == "" || updated.Status.Repository.ContentURL != "artifact:///repositories/v1/next/" {
		t.Fatalf("batch was not promoted: %+v", updated.Status.Repository)
	}
	if updated.Status.Repository.Transition != nil {
		t.Fatalf("promotion must clear the checkpoint")
	}
	if len(updated.Status.Repository.SourceJobNames) != 1 || updated.Status.Repository.SourceJobNames[0] != "job-a" {
		t.Fatalf("unexpected sourceJobNames %+v", updated.Status.Repository.SourceJobNames)
	}
	if !conditionMatches(updated.Status.Conditions, ebsv1.RpmRepoConditionRepositoryReady, metav1.ConditionTrue, ebsv1.RpmRepoReasonRepositoryCreated) {
		t.Fatalf("RepositoryReady=True/RepositoryCreated is missing: %+v", updated.Status.Conditions)
	}
	if c.Queue().Len() != 0 {
		t.Fatalf("release must not enqueue a separate key, queue length %d", c.Queue().Len())
	}
}

func TestReconcileBuildWithoutPublishingMaterializesBeforeSkipping(t *testing.T) {
	client := NewFakeClient()
	client.RpmRepos[key(testProject, testBuild)] = newRpmRepo(testBuild)
	build := newBuild(testBuild)
	build.Spec.Publish = new(bool)
	client.Builds[key(testProject, testBuild)] = build
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}

	artifacts := NewFakeArtifactManager()
	artifacts.SubmitRepositoryFunc = func(_ context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryReady, Attempt: 1, ContentURL: "/repositories/v1/next/", UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())
	if _, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	updated := client.RpmRepos[key(testProject, testBuild)]
	if len(artifacts.SubmitRepositoryRequests) != 1 || updated.Status.Repository == nil || updated.Status.Repository.ContentURL != "artifact:///repositories/v1/next/" {
		t.Fatalf("process repository was not materialized: %+v", updated.Status.Repository)
	}
	if updated.Status.Release == nil || updated.Status.Release.Phase != ebsv1.RpmRepoReleaseSkipped || updated.Status.Release.UpdatedAt == nil || updated.Status.Release.ContentURL != "" {
		t.Fatalf("release was not skipped: %+v", updated.Status.Release)
	}
	if len(artifacts.SubmitReleaseRequests) != 0 || c.Queue().Len() != 0 {
		t.Fatal("a nonpublishing build must not submit or enqueue a release")
	}
	if _, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild)); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if len(artifacts.SubmitRepositoryRequests) != 1 {
		t.Fatal("a skipped release must not rematerialize the repository")
	}
}

func TestReconcileBuildWithoutPublishingAndWithoutArtifactsSkipsRelease(t *testing.T) {
	client := NewFakeClient()
	client.RpmRepos[key(testProject, testBuild)] = newRpmRepo(testBuild)
	build := newBuild(testBuild)
	build.Spec.Publish = new(bool)
	client.Builds[key(testProject, testBuild)] = build
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())
	if _, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	updated := client.RpmRepos[key(testProject, testBuild)]
	if updated.Status.Release == nil || updated.Status.Release.Phase != ebsv1.RpmRepoReleaseSkipped {
		t.Fatalf("release phase = %+v, want Skipped", updated.Status.Release)
	}
	if len(updated.Status.Conditions) != 0 || len(artifacts.SubmitReleaseRequests) != 0 {
		t.Fatal("skipping without artifacts must not register a publication failure")
	}
}

func TestReconcileBuildCollectsFailureAfterRetryBudget(t *testing.T) {
	client := NewFakeClient()
	repo := newRpmRepo(testBuild)
	repo.Status.Repository.Transition = &ebsv1.RepositoryTransition{
		Inputs:            []ebsv1.RepositoryInput{{JobName: "job-a", SpecName: "gcc"}},
		BaseRepositoryUID: "base-1",
		RepositoryUID:     "next-1",
	}
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoProcessing)

	artifacts := NewFakeArtifactManager()
	artifacts.GetRepositoryFunc = func(_ context.Context, repositoryUID string) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: repositoryUID, State: RepositoryFailed, Attempt: 4, Failure: &FailureInfo{Code: "MaterializationFailed", Retryable: true}, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result != (controller.ReconcileResult{}) {
		t.Fatalf("unexpected result %+v", result)
	}
	updated := client.RpmRepos[key(testProject, testBuild)]
	if updated.Status.Release == nil || updated.Status.Release.Phase != ebsv1.RpmRepoReleaseFailed {
		t.Fatalf("release terminal missing: %+v", updated.Status.Release)
	}
	if updated.Status.Repository.Transition == nil || updated.Status.Repository.Transition.RepositoryUID != "next-1" {
		t.Fatalf("the abandoned batch must stay in place: %+v", updated.Status.Repository.Transition)
	}
	if !conditionMatches(updated.Status.Conditions, ebsv1.RpmRepoConditionRepositoryReady, metav1.ConditionFalse, ebsv1.RpmRepoReasonRepositoryCreationFailed) {
		t.Fatalf("RepositoryReady=False/RepositoryCreationFailed missing: %+v", updated.Status.Conditions)
	}
	if !conditionMatches(updated.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionFalse, ebsv1.RpmRepoReasonRepositoryCreationFailed) {
		t.Fatalf("PublishSucceed=False/RepositoryCreationFailed missing: %+v", updated.Status.Conditions)
	}
	if len(artifacts.SubmitRepositoryRequests) != 0 {
		t.Fatalf("an exhausted budget must not resubmit, got %d submits", len(artifacts.SubmitRepositoryRequests))
	}
}

func TestReconcileBuildReplaysRetryableFailureWithinBudget(t *testing.T) {
	client := NewFakeClient()
	repo := newRpmRepo(testBuild)
	repo.Status.Repository.Transition = &ebsv1.RepositoryTransition{
		Inputs:        []ebsv1.RepositoryInput{{JobName: "job-a", SpecName: "gcc"}},
		RepositoryUID: "next-1",
	}
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoProcessing)

	artifacts := NewFakeArtifactManager()
	updatedAt := time.Date(2026, 9, 21, 9, 59, 0, 0, time.UTC)
	artifacts.GetRepositoryFunc = func(_ context.Context, repositoryUID string) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: repositoryUID, State: RepositoryFailed, Attempt: 1, Failure: &FailureInfo{Retryable: true}, UpdatedAt: updatedAt}, nil
	}
	artifacts.SubmitRepositoryFunc = func(_ context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryCreating, Attempt: 2, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(artifacts.SubmitRepositoryRequests) != 1 {
		t.Fatalf("expected the same request to be replayed once, got %d", len(artifacts.SubmitRepositoryRequests))
	}
	if result.RequeueAfter != 5*time.Second {
		t.Fatalf("RequeueAfter = %s, want the poll delay of the replay", result.RequeueAfter)
	}
	if len(client.StatusWrites) != 0 {
		t.Fatalf("the retry path must not write status, got %d writes", len(client.StatusWrites))
	}
	request := artifacts.SubmitRepositoryRequests[0]
	if request.RepositoryUID != "next-1" || len(request.Manifests) != 1 || request.Manifests[0].JobName != "job-a" {
		t.Fatalf("the replay must reuse the frozen checkpoint: %+v", request)
	}
}

func TestReconcileBuildRegistersNoPublishableArtifacts(t *testing.T) {
	client := NewFakeClient()
	repo := newRpmRepo(testBuild)
	repo.Status.Repository.RepositoryUID = "base-1"
	repo.Status.Repository.ContentURL = "/repositories/v1/base-1/"
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)

	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result != (controller.ReconcileResult{}) {
		t.Fatalf("unexpected result %+v", result)
	}
	updated := client.RpmRepos[key(testProject, testBuild)]
	if updated.Status.Release == nil || updated.Status.Release.Phase != ebsv1.RpmRepoReleaseFailed {
		t.Fatalf("release terminal missing: %+v", updated.Status.Release)
	}
	if !conditionMatches(updated.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionFalse, ebsv1.RpmRepoReasonNoPublishableArtifacts) {
		t.Fatalf("PublishSucceed=False/NoPublishableArtifacts missing: %+v", updated.Status.Conditions)
	}
	if updated.Status.Repository.RepositoryUID != "base-1" || updated.Status.Repository.ContentURL != "/repositories/v1/base-1/" {
		t.Fatalf("the inherited baseline must stay readable: %+v", updated.Status.Repository)
	}
}

func TestReconcileBuildWaitsWhenBuildInfoIsNotCompleted(t *testing.T) {
	client := NewFakeClient()
	repo := newRpmRepo(testBuild)
	repo.Status.Repository.SourceJobNames = []string{"job-a"}
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoProcessing)

	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result != (controller.ReconcileResult{}) {
		t.Fatalf("unexpected result %+v", result)
	}
	if len(client.StatusWrites) != 0 {
		t.Fatalf("waiting for BuildInfo must not write status, got %d writes", len(client.StatusWrites))
	}
	if c.Queue().Len() != 0 {
		t.Fatalf("waiting for BuildInfo must not enqueue the release key")
	}
}
