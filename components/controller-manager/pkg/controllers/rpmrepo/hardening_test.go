package rpmrepo

import (
	"context"
	"testing"
	"time"

	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// inFlightRepo builds an object whose repository batch is already checkpointed.
func inFlightRepo() *ebsv1.RpmRepo {
	repo := newRpmRepo(testBuild)
	repo.Status.Repository.Transition = &ebsv1.RepositoryTransition{
		Inputs:        []ebsv1.RepositoryInput{{JobName: "job-a", JobUID: "uid-job-a", SpecName: "gcc"}},
		RepositoryUID: "next-1",
	}
	return repo
}

func TestReconcileBuildWaitsWhenBackoffAnchorIsMissing(t *testing.T) {
	client := NewFakeClient()
	repo := inFlightRepo()
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)

	artifacts := NewFakeArtifactManager()
	artifacts.GetRepositoryFunc = func(_ context.Context, repositoryUID string) (RepositoryResponse, error) {
		// A retryable failure without updatedAt is a response contract error: the controller must not replay.
		return RepositoryResponse{RepositoryUID: repositoryUID, State: RepositoryFailed, Attempt: 1, Failure: &FailureInfo{Retryable: true}}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), buildKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("a missing anchor must wait for the next window, got %+v", result)
	}
	if len(artifacts.SubmitRepositoryRequests) != 0 {
		t.Fatalf("a response without updatedAt must not be replayed, got %d submits", len(artifacts.SubmitRepositoryRequests))
	}
	if len(client.StatusWrites) != 0 {
		t.Fatalf("the wait path must not write status, got %d writes", len(client.StatusWrites))
	}
}

func TestReconcileBuildBoundsSameRoundReplays(t *testing.T) {
	client := NewFakeClient()
	client.RpmRepos[key(testProject, testBuild)] = inFlightRepo()
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)

	notFound := &artifactError{operation: "get-repository", kind: artifactNotFound, code: "RepositoryNotFound"}
	artifacts := NewFakeArtifactManager()
	artifacts.GetRepositoryFunc = func(context.Context, string) (RepositoryResponse, error) {
		return RepositoryResponse{}, notFound
	}
	artifacts.SubmitRepositoryFunc = func(context.Context, CreateRepositoryRequest) (RepositoryResponse, error) {
		return RepositoryResponse{}, &artifactError{operation: "submit-repository", kind: artifactNotFound, code: "RepositoryNotFound"}
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), buildKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(artifacts.SubmitRepositoryRequests) != 1 {
		t.Fatalf("expected exactly one same-round replay, got %d", len(artifacts.SubmitRepositoryRequests))
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("a deferred replay must ask for a later round, got %+v", result)
	}
	if _, err := c.sync(context.Background(), buildKey(testProject, testBuild)); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if len(artifacts.SubmitRepositoryRequests) != 2 {
		t.Fatalf("each round may replay once, got %d submits after two rounds", len(artifacts.SubmitRepositoryRequests))
	}
}

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

	result, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(artifacts.SubmitReleaseRequests) != 1 {
		t.Fatalf("expected exactly one same-round replay, got %d", len(artifacts.SubmitReleaseRequests))
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("a deferred replay must ask for a later round, got %+v", result)
	}
	if _, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch)); err != nil {
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
		return ReleaseResponse{BuildName: buildName, State: ReleasePrepared, Attempt: 1, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	artifacts.ActivateReleaseFunc = func(_ context.Context, buildName string) (ReleaseResponse, error) {
		// Activation never converges: the round must still stop after one activation.
		return ReleaseResponse{BuildName: buildName, State: ReleasePrepared, Attempt: 1, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch))
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

func TestReconcileBuildRejectsPermanentManifestReadError(t *testing.T) {
	client := NewFakeClient()
	client.RpmRepos[key(testProject, testBuild)] = newRpmRepo(testBuild)
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoProcessing)
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}

	artifacts := NewFakeArtifactManager()
	artifacts.GetJobManifestFunc = func(context.Context, string, string, string) (JobUploadManifest, error) {
		return JobUploadManifest{}, &artifactError{operation: "get-manifest", kind: artifactPermanent, code: "InvalidRequest"}
	}
	c := newTestController(t, client, artifacts, testConfig())

	_, err := c.sync(context.Background(), buildKey(testProject, testBuild))
	if err == nil || !controller.IsPermanent(err) {
		t.Fatalf("a permanent manifest read failure must not be swallowed, got %v", err)
	}
	if len(client.StatusWrites) != 0 {
		t.Fatalf("the failure must not write status, got %d writes", len(client.StatusWrites))
	}
	if len(artifacts.SubmitRepositoryRequests) != 0 {
		t.Fatalf("the failure must not submit a batch")
	}
}

func TestReconcileBuildDefersReadyWithoutContentURL(t *testing.T) {
	client := NewFakeClient()
	client.RpmRepos[key(testProject, testBuild)] = newRpmRepo(testBuild)
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoProcessing)
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}

	artifacts := NewFakeArtifactManager()
	artifacts.GetJobManifestFunc = func(context.Context, string, string, string) (JobUploadManifest, error) {
		return completedManifest(100), nil
	}
	artifacts.SubmitRepositoryFunc = func(_ context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryReady, Attempt: 1, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), buildKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("a ready response without contentURL must wait, got %+v", result)
	}
	if len(client.StatusWrites) != 1 {
		t.Fatalf("only the checkpoint may be written, got %d writes", len(client.StatusWrites))
	}
	if client.StatusWrites[0].Status.Repository.Transition == nil {
		t.Fatalf("the checkpoint must stay in place until a complete ready response arrives")
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
		return ReleaseResponse{BuildName: buildName, State: ReleaseReady, Attempt: 1, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	_, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch))
	if err == nil {
		t.Fatalf("a ready release without contentURL must not be collected")
	}
	if len(client.StatusWrites) != 0 {
		t.Fatalf("the incomplete response must not write status, got %d writes", len(client.StatusWrites))
	}
}

func TestMaterializeRetriesCountsOnlyRealReplays(t *testing.T) {
	before := materializeRetries.Value()

	// Scheduled retry without a same-round replay must not count.
	client := NewFakeClient()
	client.RpmRepos[key(testProject, testBuild)] = inFlightRepo()
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	artifacts := NewFakeArtifactManager()
	artifacts.GetRepositoryFunc = func(context.Context, string) (RepositoryResponse, error) {
		return RepositoryResponse{}, &artifactError{operation: "get-repository", kind: artifactRetryable, code: "RepositoryQueueFull", retryAfter: 5 * time.Second}
	}
	c := newTestController(t, client, artifacts, testConfig())
	if _, err := c.sync(context.Background(), buildKey(testProject, testBuild)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if delta := materializeRetries.Value() - before; delta != 0 {
		t.Fatalf("a deferred retry must not count as a replay, delta %d", delta)
	}

	// A real same-round replay counts once.
	client = NewFakeClient()
	client.RpmRepos[key(testProject, testBuild)] = inFlightRepo()
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	artifacts = NewFakeArtifactManager()
	artifacts.GetRepositoryFunc = func(_ context.Context, repositoryUID string) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: repositoryUID, State: RepositoryFailed, Attempt: 1, Failure: &FailureInfo{Retryable: true}, UpdatedAt: time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)}, nil
	}
	artifacts.SubmitRepositoryFunc = func(_ context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryCreating, Attempt: 2, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	c = newTestController(t, client, artifacts, testConfig())
	if _, err := c.sync(context.Background(), buildKey(testProject, testBuild)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if delta := materializeRetries.Value() - before; delta != 1 {
		t.Fatalf("one replay must count exactly once, delta %d", delta)
	}
}

func TestListAllStopsAtPageBudget(t *testing.T) {
	shared := &endlessCursorClient{}
	client := newAPIClient(shared)
	if _, err := client.ListJobs(context.Background(), testProject, metav1.ListOptions{}); err == nil {
		t.Fatalf("an endless cursor stream must be rejected")
	}
	if shared.pages != maxListPages {
		t.Fatalf("expected the read to stop after %d pages, got %d", maxListPages, shared.pages)
	}
}

func TestReconcileBuildRejectsForeignRepositoryResponse(t *testing.T) {
	client := NewFakeClient()
	client.RpmRepos[key(testProject, testBuild)] = inFlightRepo()
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)

	artifacts := NewFakeArtifactManager()
	artifacts.GetRepositoryFunc = func(context.Context, string) (RepositoryResponse, error) {
		// The response belongs to another repository: it must never be used to promote this object.
		return RepositoryResponse{RepositoryUID: "other-repository", State: RepositoryReady, Attempt: 1, ContentURL: "/repositories/v1/other/", UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), buildKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("an identity mismatch must be deferred, got %+v", result)
	}
	if len(client.StatusWrites) != 0 {
		t.Fatalf("a foreign response must not write status, got %d writes", len(client.StatusWrites))
	}
	if len(artifacts.SubmitRepositoryRequests) != 0 {
		t.Fatalf("a foreign response must not trigger a replay")
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
		return ReleaseResponse{BuildName: "other-build", State: ReleaseReady, Attempt: 1, ContentURL: "/repositories/other/", UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	if _, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch)); err == nil {
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
		return ReleaseResponse{BuildName: buildName, State: ReleaseCreating, Attempt: 1, PollAfterSeconds: 5}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	if _, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch)); err == nil {
		t.Fatalf("a release response without updatedAt must be rejected")
	}
	if len(client.StatusWrites) != 0 {
		t.Fatalf("an incomplete response must not write status, got %d writes", len(client.StatusWrites))
	}
}

func TestReconcileBuildRejectsUnknownManifestState(t *testing.T) {
	client := NewFakeClient()
	client.RpmRepos[key(testProject, testBuild)] = newRpmRepo(testBuild)
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoProcessing)
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}

	artifacts := NewFakeArtifactManager()
	artifacts.GetJobManifestFunc = func(context.Context, string, string, string) (JobUploadManifest, error) {
		return JobUploadManifest{State: ManifestState("Unknown")}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	_, err := c.sync(context.Background(), buildKey(testProject, testBuild))
	if err == nil || !controller.IsPermanent(err) {
		t.Fatalf("an unknown manifest state must be a permanent contract error, got %v", err)
	}
	if len(artifacts.SubmitRepositoryRequests) != 0 {
		t.Fatalf("an unknown manifest state must not submit a batch")
	}
}
