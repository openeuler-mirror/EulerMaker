package rpmrepo

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clocktesting "k8s.io/utils/clock/testing"

	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
)

// stubPolicy returns a scripted decision so the release flow can be driven without the default policy.
type stubPolicy struct {
	decision PublishDecision
	err      error
	calls    int
}

func (p *stubPolicy) Decide(context.Context, PublishPolicyInput) (PublishDecision, error) {
	p.calls++
	return p.decision, p.err
}

func releaseCandidateFixture(t *testing.T, mutate func(*ebsv1.RpmRepo), policy PublishPolicy) (*FakeClient, *FakeArtifactManager, *Controller) {
	t.Helper()
	client := NewFakeClient()
	repo := releasableRpmRepo(testBuild)
	if mutate != nil {
		mutate(repo)
	}
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}
	artifacts := NewFakeArtifactManager()
	artifacts.SubmitReleaseFunc = func(_ context.Context, req CreateReleaseRequest) (ReleaseResponse, error) {
		return ReleaseResponse{BuildName: req.BuildName, State: ReleaseCreating, Attempt: 1, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	c, err := New(&stubSource{}, client, artifacts, policy, clocktesting.NewFakeClock(now), testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client, artifacts, c
}

func TestReleaseSkipsCandidateWithoutBuild(t *testing.T) {
	client, artifacts, c := releaseCandidateFixture(t, nil, DefaultPublishPolicy{})
	delete(client.Builds, key(testProject, testBuild))

	result, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result != (controller.ReconcileResult{}) {
		t.Fatalf("a candidate without a Build must be skipped, got %+v", result)
	}
	if len(client.StatusWrites) != 0 || len(artifacts.SubmitReleaseRequests) != 0 {
		t.Fatalf("a missing Build must not produce a write or a release request")
	}
}

func TestReleaseSkipsCandidateWithMismatchedLabels(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ebsv1.RpmRepo)
	}{
		{
			name:   "foreign-target",
			mutate: func(repo *ebsv1.RpmRepo) { repo.Labels[ebsv1.BuildTargetArchLabel] = "x86_64" },
		},
		{
			name:   "no-labels",
			mutate: func(repo *ebsv1.RpmRepo) { repo.Labels = nil },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, artifacts, c := releaseCandidateFixture(t, tc.mutate, DefaultPublishPolicy{})
			result, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch))
			if err != nil {
				t.Fatalf("sync: %v", err)
			}
			if result != (controller.ReconcileResult{}) {
				t.Fatalf("a mismatched candidate must be skipped, got %+v", result)
			}
			if len(client.StatusWrites) != 0 || len(artifacts.SubmitReleaseRequests) != 0 {
				t.Fatalf("a mismatched candidate must not reach Artifact Manager or write status")
			}
		})
	}
}

func TestReleasePolicyErrorIsRetryable(t *testing.T) {
	policy := &stubPolicy{err: errors.New("policy unavailable")}
	client, artifacts, c := releaseCandidateFixture(t, nil, policy)

	if _, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch)); err == nil {
		t.Fatalf("a policy error must surface as an error")
	}
	if policy.calls != 1 {
		t.Fatalf("the policy must be consulted once, got %d", policy.calls)
	}
	if len(client.StatusWrites) != 0 || len(artifacts.SubmitReleaseRequests) != 0 {
		t.Fatalf("a policy error must not write status or submit a release")
	}
}

func TestReleaseCheckpointNormalizesExcludeSpecs(t *testing.T) {
	policy := &stubPolicy{decision: PublishDecision{Publish: true, ExcludeSpecs: []string{"kernel", "gcc", "gcc", ""}}}
	client, artifacts, c := releaseCandidateFixture(t, nil, policy)

	if _, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(artifacts.SubmitReleaseRequests) != 1 {
		t.Fatalf("expected one release submission, got %d", len(artifacts.SubmitReleaseRequests))
	}
	submitted := artifacts.SubmitReleaseRequests[0]
	if len(submitted.ExcludeSpecs) != 2 || submitted.ExcludeSpecs[0] != "gcc" || submitted.ExcludeSpecs[1] != "kernel" {
		t.Fatalf("the checkpoint must carry normalized excludeSpecs, got %v", submitted.ExcludeSpecs)
	}
	if submitted.SourceRepositoryUID != "repo-1" {
		t.Fatalf("the checkpoint must freeze the current repository UID, got %q", submitted.SourceRepositoryUID)
	}
	stored := client.RpmRepos[key(testProject, testBuild)]
	if stored.Status.Release == nil || stored.Status.Release.Transition == nil {
		t.Fatalf("the checkpoint must be persisted: %+v", stored.Status.Release)
	}
	if len(stored.Status.Release.Transition.ExcludeSpecs) != 2 {
		t.Fatalf("the persisted checkpoint must carry normalized excludeSpecs")
	}
}

func TestReleaseReplaysRetryableFailureWithFrozenCheckpoint(t *testing.T) {
	client := NewFakeClient()
	repo := releasableRpmRepo(testBuild)
	repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{
		Phase:      ebsv1.RpmRepoReleaseCreating,
		Transition: &ebsv1.ReleaseTransition{SourceRepositoryUID: "repo-1", ExcludeSpecs: []string{"gcc"}},
	}
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	artifacts := NewFakeArtifactManager()
	artifacts.GetReleaseFunc = func(_ context.Context, buildName string) (ReleaseResponse, error) {
		return ReleaseResponse{BuildName: buildName, State: ReleaseFailed, Attempt: 1, Failure: &FailureInfo{Retryable: true}, UpdatedAt: time.Now()}, nil
	}
	artifacts.SubmitReleaseFunc = func(_ context.Context, req CreateReleaseRequest) (ReleaseResponse, error) {
		return ReleaseResponse{BuildName: req.BuildName, State: ReleasePrepared, Attempt: 2, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	if _, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(artifacts.SubmitReleaseRequests) != 1 {
		t.Fatalf("expected one replay, got %d", len(artifacts.SubmitReleaseRequests))
	}
	replayed := artifacts.SubmitReleaseRequests[0]
	if replayed.SourceRepositoryUID != "repo-1" || len(replayed.ExcludeSpecs) != 1 || replayed.ExcludeSpecs[0] != "gcc" {
		t.Fatalf("the replay must reuse the frozen checkpoint: %+v", replayed)
	}
}

func TestReleaseIdentityConflictCollectsFailure(t *testing.T) {
	client := NewFakeClient()
	repo := releasableRpmRepo(testBuild)
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}
	artifacts := NewFakeArtifactManager()
	artifacts.SubmitReleaseFunc = func(context.Context, CreateReleaseRequest) (ReleaseResponse, error) {
		return ReleaseResponse{}, &artifactError{operation: "submit-release", kind: artifactPermanent, code: "ReleaseIdentityConflict", statusCode: 409}
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result != (controller.ReconcileResult{}) {
		t.Fatalf("a collected failure must not requeue, got %+v", result)
	}
	stored := client.RpmRepos[key(testProject, testBuild)]
	if stored.Status.Release == nil || stored.Status.Release.Phase != ebsv1.RpmRepoReleaseFailed || stored.Status.Release.Transition != nil {
		t.Fatalf("a non-retryable release failure must be collected: %+v", stored.Status.Release)
	}
	if !conditionMatches(stored.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionFalse, ebsv1.RpmRepoReasonReleaseFailed) {
		t.Fatalf("PublishSucceed=False/ReleaseFailed missing: %+v", stored.Status.Conditions)
	}
	if stored.Status.Repository.RepositoryUID != "repo-1" {
		t.Fatalf("the repository fields must not change on a release failure")
	}
}
