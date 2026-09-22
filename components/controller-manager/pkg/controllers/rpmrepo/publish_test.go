package rpmrepo

import (
	"context"
	"testing"
	"time"

	ebsv1 "ebs-api/ebs/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// releasableRpmRepo builds an object that already promoted one batch, so only the release flow is left.
func releasableRpmRepo(name string) *ebsv1.RpmRepo {
	repo := newRpmRepo(name)
	repo.Status.Repository.RepositoryUID = "repo-1"
	repo.Status.Repository.ContentURL = "/repositories/v1/repo-1/"
	repo.Status.Repository.SourceJobUIDs = []string{"uid-job-a"}
	conditions, _ := MergeCondition(nil, ebsv1.RpmRepoConditionRepositoryReady, metav1.ConditionTrue, ebsv1.RpmRepoReasonRepositoryCreated, "", 1, metav1.Now())
	repo.Status.Conditions = conditions
	return repo
}

func TestReconcileReleaseSubmitsActivatesAndCollects(t *testing.T) {
	client := NewFakeClient()
	repo := releasableRpmRepo(testBuild)
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}

	artifacts := NewFakeArtifactManager()
	artifacts.SubmitReleaseFunc = func(_ context.Context, req CreateReleaseRequest) (ReleaseResponse, error) {
		return ReleaseResponse{BuildName: req.BuildName, State: ReleasePrepared, Attempt: 1, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	artifacts.ActivateReleaseFunc = func(_ context.Context, buildName string) (ReleaseResponse, error) {
		return ReleaseResponse{BuildName: buildName, State: ReleaseReady, Attempt: 1, ContentURL: "/repositories/" + testProject + "/" + testOS + "/" + testArch + "/", UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !result.Requeue {
		t.Fatalf("a successful release must ask for the next candidate, got %+v", result)
	}
	updated := client.RpmRepos[key(testProject, testBuild)]
	if updated.Status.Release == nil || updated.Status.Release.Phase != ebsv1.RpmRepoReleaseReady {
		t.Fatalf("release was not collected: %+v", updated.Status.Release)
	}
	if updated.Status.Release.Transition != nil {
		t.Fatalf("the release checkpoint must be cleared")
	}
	if updated.Status.Release.SourceRepositoryUID != "repo-1" {
		t.Fatalf("unexpected source repository UID %q", updated.Status.Release.SourceRepositoryUID)
	}
	if !conditionMatches(updated.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionTrue, ebsv1.RpmRepoReasonReleaseActivated) {
		t.Fatalf("PublishSucceed=True/ReleaseActivated missing: %+v", updated.Status.Conditions)
	}
	if len(artifacts.SubmitReleaseRequests) != 1 || len(artifacts.ActivateReleaseNames) != 1 {
		t.Fatalf("expected one submit and one activation, got %d/%d", len(artifacts.SubmitReleaseRequests), len(artifacts.ActivateReleaseNames))
	}
	if len(client.RpmRepoListOptions) != 1 {
		t.Fatalf("expected one RpmRepo list call, got %d", len(client.RpmRepoListOptions))
	}
	options := client.RpmRepoListOptions[0]
	if options.FieldSelector != nonTerminalRpmRepoFieldSelector {
		t.Fatalf("unexpected field selector %q", options.FieldSelector)
	}
	wantSelector := labels.Set{ebsv1.BuildTargetOSLabel: testOS, ebsv1.BuildTargetArchLabel: testArch}.String()
	if options.LabelSelector != wantSelector {
		t.Fatalf("unexpected label selector %q, want %q", options.LabelSelector, wantSelector)
	}
}

func TestReconcileReleaseFailureCollectsTerminal(t *testing.T) {
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
	artifacts.GetReleaseFunc = func(_ context.Context, buildName string) (ReleaseResponse, error) {
		return ReleaseResponse{BuildName: buildName, State: ReleaseFailed, Attempt: 1, Failure: &FailureInfo{Code: "ReleaseMaterializationFailed", Retryable: false}, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("a collected failure must not requeue, got %+v", result)
	}
	updated := client.RpmRepos[key(testProject, testBuild)]
	if updated.Status.Release == nil || updated.Status.Release.Phase != ebsv1.RpmRepoReleaseFailed {
		t.Fatalf("release terminal missing: %+v", updated.Status.Release)
	}
	if updated.Status.Release.Transition != nil {
		t.Fatalf("the failing release must clear its checkpoint")
	}
	if !conditionMatches(updated.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionFalse, ebsv1.RpmRepoReasonReleaseFailed) {
		t.Fatalf("PublishSucceed=False/ReleaseFailed missing: %+v", updated.Status.Conditions)
	}
	if updated.Status.Repository.RepositoryUID != "repo-1" {
		t.Fatalf("the repository fields must not change on release failure")
	}
}

func TestReconcileReleaseReplaysMissingRecord(t *testing.T) {
	client := NewFakeClient()
	repo := releasableRpmRepo(testBuild)
	repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{
		Phase:      ebsv1.RpmRepoReleasePending,
		Transition: &ebsv1.ReleaseTransition{SourceRepositoryUID: "repo-1", ExcludeSpecs: []string{"gcc"}},
	}
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)

	artifacts := NewFakeArtifactManager()
	artifacts.GetReleaseFunc = func(context.Context, string) (ReleaseResponse, error) {
		return ReleaseResponse{}, &artifactError{operation: "get-release", kind: artifactNotFound, code: "ReleaseNotFound"}
	}
	artifacts.SubmitReleaseFunc = func(_ context.Context, req CreateReleaseRequest) (ReleaseResponse, error) {
		return ReleaseResponse{BuildName: req.BuildName, State: ReleaseCreating, Attempt: 2, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Fatalf("RequeueAfter = %s, want the poll delay", result.RequeueAfter)
	}
	if len(artifacts.SubmitReleaseRequests) != 1 {
		t.Fatalf("the frozen request must be replayed once, got %d", len(artifacts.SubmitReleaseRequests))
	}
	replayed := artifacts.SubmitReleaseRequests[0]
	if replayed.SourceRepositoryUID != "repo-1" || len(replayed.ExcludeSpecs) != 1 || replayed.ExcludeSpecs[0] != "gcc" {
		t.Fatalf("the replay must reuse the frozen checkpoint: %+v", replayed)
	}
}

func TestReconcileReleaseCollectsAbortedInFlight(t *testing.T) {
	client := NewFakeClient()
	repo := releasableRpmRepo(testBuild)
	repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{
		Phase:      ebsv1.RpmRepoReleasePrepared,
		Transition: &ebsv1.ReleaseTransition{SourceRepositoryUID: "repo-1"},
	}
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
	build := newBuild(testBuild)
	build.Status.Phase = ebsv1.BuildAborted
	client.Builds[key(testProject, testBuild)] = build

	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())

	if _, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	updated := client.RpmRepos[key(testProject, testBuild)]
	if updated.Status.Release == nil || updated.Status.Release.Phase != ebsv1.RpmRepoReleaseFailed || updated.Status.Release.Transition != nil {
		t.Fatalf("an aborted in-flight release must be collected: %+v", updated.Status.Release)
	}
	if !conditionMatches(updated.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionFalse, ebsv1.RpmRepoReasonBuildAborted) {
		t.Fatalf("PublishSucceed=False/BuildAborted missing: %+v", updated.Status.Conditions)
	}
	if len(artifacts.GetReleaseBuildNames) != 0 || len(artifacts.ActivateReleaseNames) != 0 {
		t.Fatalf("the abort terminal must not call Artifact Manager")
	}
}

func TestReconcileReleaseSkipsCandidateWhenPolicySaysNo(t *testing.T) {
	client := NewFakeClient()
	repo := releasableRpmRepo(testBuild)
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
	build := newBuild(testBuild)
	build.Spec.BuildTarget.PublishFlag = false
	client.Builds[key(testProject, testBuild)] = build
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}

	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("a skipped candidate must not requeue, got %+v", result)
	}
	if len(client.StatusWrites) != 0 {
		t.Fatalf("a policy skip without a residual release must not write status, got %d writes", len(client.StatusWrites))
	}
	if len(artifacts.SubmitReleaseRequests) != 0 || len(artifacts.ActivateReleaseNames) != 0 {
		t.Fatalf("a skipped candidate must not reach Artifact Manager")
	}
	if repo := client.RpmRepos[key(testProject, testBuild)]; repo.Status.Release != nil {
		t.Fatalf("a skipped candidate must not persist a release state: %+v", repo.Status.Release)
	}
}

func TestReconcileReleaseClearsResidualReleaseBeforeSkipping(t *testing.T) {
	client := NewFakeClient()
	repo := releasableRpmRepo(testBuild)
	repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleasePending}
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
	build := newBuild(testBuild)
	build.Spec.BuildTarget.PublishFlag = false
	client.Builds[key(testProject, testBuild)] = build
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}

	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())

	if _, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(client.StatusWrites) != 1 {
		t.Fatalf("a residual release must be cleared exactly once, got %d writes", len(client.StatusWrites))
	}
	if client.StatusWrites[0].Status.Release != nil {
		t.Fatalf("the residual release must be cleared")
	}
}
