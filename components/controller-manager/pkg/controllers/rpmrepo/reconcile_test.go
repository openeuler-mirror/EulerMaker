package rpmrepo

import (
	"context"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"

	"controller-manager/pkg/controller"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

const (
	testProject = "project"
	testBuild   = "build-a"
	testOS      = "openEuler"
	testArch    = "aarch64"
)

type stubSource struct {
	handler source.ResourceEventHandler
}

func (s *stubSource) Name() string { return "rpmrepos" }

func (s *stubSource) AddEventHandler(handler source.ResourceEventHandler) error {
	s.handler = handler
	return nil
}

func (s *stubSource) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (s *stubSource) HasSynced() bool { return true }

func (s *stubSource) Ready() bool { return true }

func testConfig() Config {
	return Config{
		ArtifactManagerAddr:    "http://artifact-manager",
		ArtifactManagerTimeout: 5 * time.Second,
		MaxJobsPerBatch:        20,
		MaterializeRetryLimit:  3,
		PollPeriod:             30 * time.Second,
		MaxRetries:             3,
		Backoff:                Backoff{Initial: 30 * time.Second, Max: 15 * time.Minute, Jitter: 0},
	}
}

func newTestController(t *testing.T, client Client, artifacts ArtifactManagerClient, config Config) *Controller {
	t.Helper()
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	c, err := New(&stubSource{}, client, artifacts, DefaultPublishPolicy{}, clocktesting.NewFakeClock(now), config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func newRpmRepo(name string) *ebsv1.RpmRepo {
	return &ebsv1.RpmRepo{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       testProject,
			UID:             types.UID("uid-" + name),
			ResourceVersion: "1",
			Generation:      1,
			Labels: map[string]string{
				ebsv1.BuildTargetOSLabel:   testOS,
				ebsv1.BuildTargetArchLabel: testArch,
			},
		},
		Status: ebsv1.RpmRepoStatus{Repository: &ebsv1.RpmRepoRepositoryStatus{}},
	}
}

func newBuild(name string) *ebsv1.Build {
	return &ebsv1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testProject, UID: types.UID("uid-" + name), ResourceVersion: "1"},
		Spec: ebsv1.BuildSpec{
			BuildType:   "incremental",
			BuildTarget: ebsv1.BuildTarget{Os: testOS, Arch: testArch, BuildFlag: true, PublishFlag: true},
		},
		Status: ebsv1.BuildStatus{Phase: ebsv1.BuildProcessing},
	}
}

func newBuildInfo(name string, phase ebsv1.BuildInfoPhase) *ebsv1.BuildInfo {
	return &ebsv1.BuildInfo{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testProject, UID: types.UID("uid-" + name), ResourceVersion: "1"},
		Status:     ebsv1.BuildInfoStatus{Phase: phase},
	}
}

func newSucceededJob(name, spec, uid string, created time.Time) ebsv1.Job {
	return ebsv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         testProject,
			UID:               types.UID(uid),
			ResourceVersion:   "1",
			CreationTimestamp: metav1.NewTime(created),
			Labels: map[string]string{
				ebsv1.JobBuildNameLabel:    testBuild,
				ebsv1.JobSpecNameLabel:     spec,
				ebsv1.BuildTargetOSLabel:   testOS,
				ebsv1.BuildTargetArchLabel: testArch,
			},
		},
		Status: ebsv1.JobStatus{Phase: ebsv1.JobSucceeded},
	}
}

func completedManifest(size int64) JobUploadManifest {
	return JobUploadManifest{
		State: ManifestCompleted,
		Files: []ManifestFile{{RelativePath: "packages/gcc.rpm", Size: size}},
	}
}

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
		ebsv1.RepositoryInput{JobName: "job-b", JobUID: "uid-job-b", SpecName: "glibc"})
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
			Failure: &FailureInfo{Code: "ManifestInvalid", JobUID: "uid-job-a"}}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())
	result, err := c.sync(context.Background(), buildKey(testProject, testBuild))
	if err != nil || !result.Requeue {
		t.Fatalf("skip result = %+v, %v", result, err)
	}
	updated := client.RpmRepos[key(testProject, testBuild)]
	if updated.Status.Repository.Transition != nil || !reflect.DeepEqual(updated.Status.Repository.SkippedJobUIDs, []string{"uid-job-a"}) {
		t.Fatalf("failed input was not durably skipped: %+v", updated.Status.Repository)
	}
	if updated.Status.Release != nil || len(updated.Status.Repository.SourceJobUIDs) != 0 {
		t.Fatalf("skipping must not create a release or mark input as materialized: %+v", updated.Status)
	}
	artifacts.SubmitRepositoryFunc = func(_ context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryCreating, Attempt: 1, UpdatedAt: time.Now()}, nil
	}
	c = newTestController(t, client, artifacts, testConfig())
	if _, err := c.sync(context.Background(), buildKey(testProject, testBuild)); err != nil {
		t.Fatalf("rebatch after restart: %v", err)
	}
	if len(artifacts.SubmitRepositoryRequests) != 1 || !reflect.DeepEqual(artifacts.SubmitRepositoryRequests[0].Manifests,
		[]ManifestReference{{JobName: "job-b", JobUID: "uid-job-b"}}) {
		t.Fatalf("rebatch must contain only the healthy Job: %+v", artifacts.SubmitRepositoryRequests)
	}
	if len(artifacts.ManifestRequests) != 0 {
		t.Fatalf("rebatch must not pre-read manifests: %+v", artifacts.ManifestRequests)
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
			Failure: &FailureInfo{Code: "ManifestInvalid", JobUID: "foreign-uid"}}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())
	if _, err := c.sync(context.Background(), buildKey(testProject, testBuild)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	updated := client.RpmRepos[key(testProject, testBuild)]
	if len(updated.Status.Repository.SkippedJobUIDs) != 0 || updated.Status.Release == nil || updated.Status.Release.Phase != ebsv1.RpmRepoReleaseFailed {
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
	artifacts.GetJobManifestFunc = func(context.Context, string, string, string) (JobUploadManifest, error) {
		return completedManifest(100), nil
	}
	artifacts.SubmitRepositoryFunc = func(_ context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryCreating, Attempt: 1, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), buildKey(testProject, testBuild))
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
	if transition == nil || len(transition.Inputs) != 1 || transition.Inputs[0].JobUID != "uid-job-a" {
		t.Fatalf("checkpoint does not freeze the batch: %+v", transition)
	}
	if len(artifacts.SubmitRepositoryRequests) != 1 {
		t.Fatalf("expected one SubmitRepository call, got %d", len(artifacts.SubmitRepositoryRequests))
	}
	request := artifacts.SubmitRepositoryRequests[0]
	if request.RepositoryUID != transition.RepositoryUID || request.RepositoryName != testBuild || request.TargetOS != testOS || request.TargetArch != testArch {
		t.Fatalf("unexpected materialization request %+v", request)
	}
	if client.Jobs[testProject][0].Status.Phase != ebsv1.JobSucceeded {
		t.Fatalf("the controller must not modify Job status")
	}
}

func TestReconcileBuildPromotesBatchAndEnqueuesRelease(t *testing.T) {
	client := NewFakeClient()
	repo := newRpmRepo(testBuild)
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}

	artifacts := NewFakeArtifactManager()
	artifacts.GetJobManifestFunc = func(context.Context, string, string, string) (JobUploadManifest, error) {
		return completedManifest(100), nil
	}
	artifacts.SubmitRepositoryFunc = func(_ context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryReady, Attempt: 1, ContentURL: "/repositories/v1/next/", UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), buildKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("unexpected result %+v", result)
	}
	updated := client.RpmRepos[key(testProject, testBuild)]
	if updated.Status.Repository.RepositoryUID == "" || updated.Status.Repository.ContentURL != "/repositories/v1/next/" {
		t.Fatalf("batch was not promoted: %+v", updated.Status.Repository)
	}
	if updated.Status.Repository.Transition != nil {
		t.Fatalf("promotion must clear the checkpoint")
	}
	if len(updated.Status.Repository.SourceJobUIDs) != 1 || updated.Status.Repository.SourceJobUIDs[0] != "uid-job-a" {
		t.Fatalf("unexpected sourceJobUIDs %+v", updated.Status.Repository.SourceJobUIDs)
	}
	if !conditionMatches(updated.Status.Conditions, ebsv1.RpmRepoConditionRepositoryReady, metav1.ConditionTrue, ebsv1.RpmRepoReasonRepositoryCreated) {
		t.Fatalf("RepositoryReady=True/RepositoryCreated is missing: %+v", updated.Status.Conditions)
	}
	if c.Queue().Len() != 1 {
		t.Fatalf("expected the release key to be enqueued, queue length %d", c.Queue().Len())
	}
	item, _ := c.Queue().Get()
	if item != releaseKey(testProject, testOS, testArch) {
		t.Fatalf("unexpected queued key %v", item)
	}
}

func TestReconcileBuildWithoutPublishingMaterializesBeforeSkipping(t *testing.T) {
	client := NewFakeClient()
	client.RpmRepos[key(testProject, testBuild)] = newRpmRepo(testBuild)
	build := newBuild(testBuild)
	build.Spec.BuildTarget.PublishFlag = false
	client.Builds[key(testProject, testBuild)] = build
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}

	artifacts := NewFakeArtifactManager()
	artifacts.GetJobManifestFunc = func(context.Context, string, string, string) (JobUploadManifest, error) {
		return completedManifest(100), nil
	}
	artifacts.SubmitRepositoryFunc = func(_ context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryReady, Attempt: 1, ContentURL: "/repositories/v1/next/", UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())
	if _, err := c.sync(context.Background(), buildKey(testProject, testBuild)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	updated := client.RpmRepos[key(testProject, testBuild)]
	if len(artifacts.SubmitRepositoryRequests) != 1 || updated.Status.Repository == nil || updated.Status.Repository.ContentURL != "/repositories/v1/next/" {
		t.Fatalf("process repository was not materialized: %+v", updated.Status.Repository)
	}
	if updated.Status.Release == nil || updated.Status.Release.Phase != ebsv1.RpmRepoReleaseSkipped || updated.Status.Release.UpdatedAt == nil || updated.Status.Release.ContentURL != "" {
		t.Fatalf("release was not skipped: %+v", updated.Status.Release)
	}
	if len(artifacts.SubmitReleaseRequests) != 0 || c.Queue().Len() != 0 {
		t.Fatal("a nonpublishing build must not submit or enqueue a release")
	}
	if _, err := c.sync(context.Background(), buildKey(testProject, testBuild)); err != nil {
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
	build.Spec.BuildTarget.PublishFlag = false
	client.Builds[key(testProject, testBuild)] = build
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())
	if _, err := c.sync(context.Background(), buildKey(testProject, testBuild)); err != nil {
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
		Inputs:            []ebsv1.RepositoryInput{{JobName: "job-a", JobUID: "uid-job-a", SpecName: "gcc"}},
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

	result, err := c.sync(context.Background(), buildKey(testProject, testBuild))
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
		Inputs:        []ebsv1.RepositoryInput{{JobName: "job-a", JobUID: "uid-job-a", SpecName: "gcc"}},
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

	result, err := c.sync(context.Background(), buildKey(testProject, testBuild))
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
	if request.RepositoryUID != "next-1" || len(request.Manifests) != 1 || request.Manifests[0].JobUID != "uid-job-a" {
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

	result, err := c.sync(context.Background(), buildKey(testProject, testBuild))
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
	repo.Status.Repository.SourceJobUIDs = []string{"uid-job-a"}
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoProcessing)

	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), buildKey(testProject, testBuild))
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

func TestReconcileBuildCollectsAbortedTerminal(t *testing.T) {
	client := NewFakeClient()
	repo := newRpmRepo(testBuild)
	repo.Status.Repository.Transition = &ebsv1.RepositoryTransition{
		Inputs:        []ebsv1.RepositoryInput{{JobName: "job-a", JobUID: "uid-job-a", SpecName: "gcc"}},
		RepositoryUID: "next-1",
	}
	client.RpmRepos[key(testProject, testBuild)] = repo
	build := newBuild(testBuild)
	build.Status.Phase = ebsv1.BuildAborted
	client.Builds[key(testProject, testBuild)] = build

	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), buildKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result != (controller.ReconcileResult{}) {
		t.Fatalf("unexpected result %+v", result)
	}
	updated := client.RpmRepos[key(testProject, testBuild)]
	if updated.Status.Release == nil || updated.Status.Release.Phase != ebsv1.RpmRepoReleaseFailed {
		t.Fatalf("aborted builds must register a release terminal: %+v", updated.Status.Release)
	}
	if !conditionMatches(updated.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionFalse, ebsv1.RpmRepoReasonBuildAborted) {
		t.Fatalf("PublishSucceed=False/BuildAborted missing: %+v", updated.Status.Conditions)
	}
	if updated.Status.Repository.Transition == nil {
		t.Fatalf("the abandoned batch must stay in place")
	}
	if len(artifacts.SubmitRepositoryRequests) != 0 || len(artifacts.GetRepositoryUIDs) != 0 {
		t.Fatalf("the abort terminal must not call Artifact Manager")
	}
}

func TestReconcileBuildCountsOrphanRpmRepo(t *testing.T) {
	client := NewFakeClient()
	client.RpmRepos[key(testProject, testBuild)] = newRpmRepo(testBuild)

	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), buildKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result != (controller.ReconcileResult{}) {
		t.Fatalf("unexpected result %+v", result)
	}
	if len(client.StatusWrites) != 0 {
		t.Fatalf("an orphan RpmRepo must not be written, got %d writes", len(client.StatusWrites))
	}
}

func TestReconcileBuildRetriesWhenRpmRepoIsMissing(t *testing.T) {
	client := NewFakeClient()
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)

	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())

	if _, err := c.sync(context.Background(), buildKey(testProject, testBuild)); err == nil {
		t.Fatalf("a readable Build without its RpmRepo must return a retryable error")
	}
}

func TestReconcileRejectsMalformedKey(t *testing.T) {
	client := NewFakeClient()
	c := newTestController(t, client, NewFakeArtifactManager(), testConfig())
	if _, err := c.sync(context.Background(), "release/project"); err == nil || !controller.IsPermanent(err) {
		t.Fatalf("a malformed key must be permanent, got %v", err)
	}
}
