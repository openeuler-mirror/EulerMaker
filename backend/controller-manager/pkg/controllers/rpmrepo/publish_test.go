package rpmrepo

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clocktesting "k8s.io/utils/clock/testing"

	"controller-manager/pkg/controller"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

// releasableRpmRepo builds an object that already promoted one batch, so only the release flow is left.
func releasableRpmRepo(name string) *ebsv1.RpmRepo {
	repo := newRpmRepo(name)
	repo.Status.Repository.RepositoryUID = "repo-1"
	repo.Status.Repository.ContentURL = "/repositories/v1/repo-1/"
	repo.Status.Repository.SourceJobNames = []string{"job-a"}
	conditions, _ := MergeCondition(nil, ebsv1.RpmRepoConditionRepositoryReady, metav1.ConditionTrue, ebsv1.RpmRepoReasonRepositoryCreated, "", 1, metav1.Now())
	repo.Status.Conditions = conditions
	return repo
}

func TestReconcileReleaseSubmitsPollsAndCollects(t *testing.T) {
	client := NewFakeClient()
	repo := releasableRpmRepo(testBuild)
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}

	artifacts := NewFakeArtifactManager()
	artifacts.SubmitReleaseFunc = func(_ context.Context, req CreateReleaseRequest) (ReleaseResponse, error) {
		return ReleaseResponse{BuildName: req.BuildName, State: ReleaseCreating, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	artifacts.GetReleaseFunc = func(_ context.Context, _ string, buildName string) (ReleaseResponse, error) {
		return ReleaseResponse{BuildName: buildName, State: ReleaseReady, ContentURL: "/repositories/" + testProject + "/" + testArch + "/", UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Fatalf("creating release must wait for the poll delay: %+v", result)
	}
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*client.RpmRepos[key(testProject, testBuild)].DeepCopy()}
	result, err = c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("poll: %v", err)
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
	if got, want := updated.Status.Release.ContentURL, "artifact:///repositories/"+testProject+"/"+testArch+"/"; got != want {
		t.Fatalf("release contentURL = %q, want %q", got, want)
	}
	if !conditionMatches(updated.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionTrue, ebsv1.RpmRepoReasonReleaseActivated) {
		t.Fatalf("PublishSucceed=True/ReleaseActivated missing: %+v", updated.Status.Conditions)
	}
	if len(artifacts.SubmitReleaseRequests) != 1 || len(artifacts.GetReleaseBuildNames) != 1 {
		t.Fatalf("expected one submit and one poll, got %d/%d", len(artifacts.SubmitReleaseRequests), len(artifacts.GetReleaseBuildNames))
	}
	if len(client.RpmRepoListOptions) != 2 {
		t.Fatalf("expected one RpmRepo list call per round, got %d", len(client.RpmRepoListOptions))
	}
	options := client.RpmRepoListOptions[0]
	if options.FieldSelector != nonTerminalRpmRepoFieldSelector {
		t.Fatalf("unexpected field selector %q", options.FieldSelector)
	}
	wantSelector := labels.Set{ebsv1.BuildTargetArchLabel: testArch}.String()
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
	artifacts.GetReleaseFunc = func(_ context.Context, _ string, buildName string) (ReleaseResponse, error) {
		return ReleaseResponse{BuildName: buildName, State: ReleaseFailed, Failure: &FailureInfo{Code: "ReleaseMaterializationFailed", Retryable: false}, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
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
	if updated.Status.Release.Transition == nil || updated.Status.Release.Transition.SourceRepositoryUID != "repo-1" {
		t.Fatalf("the failing release must retain its checkpoint for deletion recovery")
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
	artifacts.GetReleaseFunc = func(context.Context, string, string) (ReleaseResponse, error) {
		return ReleaseResponse{}, &artifactError{operation: "get-release", kind: artifactNotFound, code: "ReleaseNotFound"}
	}
	artifacts.SubmitReleaseFunc = func(_ context.Context, req CreateReleaseRequest) (ReleaseResponse, error) {
		return ReleaseResponse{BuildName: req.BuildName, State: ReleaseCreating, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
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
		Phase:      ebsv1.RpmRepoReleaseCreating,
		Transition: &ebsv1.ReleaseTransition{SourceRepositoryUID: "repo-1"},
	}
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
	build := newBuild(testBuild)
	build.Status.Phase = ebsv1.BuildAborted
	client.Builds[key(testProject, testBuild)] = build

	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())

	if _, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	updated := client.RpmRepos[key(testProject, testBuild)]
	if updated.Status.Release == nil || updated.Status.Release.Phase != ebsv1.RpmRepoReleaseFailed || updated.Status.Release.Transition == nil {
		t.Fatalf("an aborted in-flight release must be collected: %+v", updated.Status.Release)
	}
	if !conditionMatches(updated.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionFalse, ebsv1.RpmRepoReasonBuildAborted) {
		t.Fatalf("PublishSucceed=False/BuildAborted missing: %+v", updated.Status.Conditions)
	}
	if len(artifacts.GetReleaseBuildNames) != 0 {
		t.Fatalf("the abort terminal must not call Artifact Manager")
	}
}

func TestReconcileReleaseSkipsCandidateWhenPolicySaysNo(t *testing.T) {
	client := NewFakeClient()
	repo := releasableRpmRepo(testBuild)
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
	build := newBuild(testBuild)
	build.Spec.Publish = new(bool)
	client.Builds[key(testProject, testBuild)] = build
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}

	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())

	result, err := syncReleaseForTest(c, context.Background(), testProject, testArch)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("a skipped candidate must not requeue, got %+v", result)
	}
	if len(client.StatusWrites) != 1 {
		t.Fatalf("a policy skip must persist one terminal status, got %d writes", len(client.StatusWrites))
	}
	if len(artifacts.SubmitReleaseRequests) != 0 {
		t.Fatalf("a skipped candidate must not reach Artifact Manager")
	}
	if repo := client.RpmRepos[key(testProject, testBuild)]; repo.Status.Release == nil || repo.Status.Release.Phase != ebsv1.RpmRepoReleaseSkipped {
		t.Fatalf("a skipped candidate must persist Skipped: %+v", repo.Status.Release)
	}
}

func TestReconcileReleaseReplacesResidualReleaseWithSkipped(t *testing.T) {
	client := NewFakeClient()
	repo := releasableRpmRepo(testBuild)
	repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleasePending}
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
	build := newBuild(testBuild)
	build.Spec.Publish = new(bool)
	client.Builds[key(testProject, testBuild)] = build
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}

	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())

	if _, err := syncReleaseForTest(c, context.Background(), testProject, testArch); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(client.StatusWrites) != 1 {
		t.Fatalf("a residual release must be replaced exactly once, got %d writes", len(client.StatusWrites))
	}
	if release := client.StatusWrites[0].Status.Release; release == nil || release.Phase != ebsv1.RpmRepoReleaseSkipped || release.Transition != nil {
		t.Fatalf("the residual release must become Skipped: %+v", release)
	}
}

func TestReleaseRecheckSkipsCandidateWithoutBuildInfo(t *testing.T) {
	client, artifacts, c := releaseCandidateFixture(t, nil, DefaultPublishPolicy{})
	delete(client.BuildInfos, key(testProject, testBuild))

	result, err := syncReleaseForTest(c, context.Background(), testProject, testArch)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result != (controller.ReconcileResult{}) {
		t.Fatalf("a candidate without BuildInfo must be skipped, got %+v", result)
	}
	if len(client.StatusWrites) != 0 || len(artifacts.SubmitReleaseRequests) != 0 {
		t.Fatalf("a candidate without BuildInfo must not write status or submit")
	}
}

func TestReleaseRecheckReturnsToTheRepositoryWhenInputsRemain(t *testing.T) {
	client, artifacts, c := releaseCandidateFixture(t, nil, DefaultPublishPolicy{})
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-b", "kernel", "uid-job-b", time.Unix(2, 0))}

	result, err := syncReleaseForTest(c, context.Background(), testProject, testArch)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result != (controller.ReconcileResult{}) {
		t.Fatalf("a candidate with unconsumed inputs must be skipped, got %+v", result)
	}
	if len(artifacts.SubmitReleaseRequests) != 0 {
		t.Fatalf("a candidate with unconsumed inputs must not start a release")
	}
	if c.Queue().Len() != 1 {
		t.Fatalf("the repository key must be re-enqueued for the remaining inputs")
	}
	item, _ := c.Queue().Get()
	if item != rpmRepoKey(testProject, testBuild) {
		t.Fatalf("unexpected queued key %v", item)
	}
}

func TestReleaseRecheckSkipsCandidateWithoutOwnVersion(t *testing.T) {
	client, artifacts, c := releaseCandidateFixture(t, func(repo *ebsv1.RpmRepo) {
		repo.Status.Repository.SourceJobNames = nil
	}, DefaultPublishPolicy{})
	// The candidate predicate already filters this object; keep the test honest by asserting nothing happens.
	result, err := syncReleaseForTest(c, context.Background(), testProject, testArch)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result != (controller.ReconcileResult{}) || len(artifacts.SubmitReleaseRequests) != 0 {
		t.Fatalf("an object without its own version must not release, got %+v", result)
	}
	if len(client.StatusWrites) != 0 {
		t.Fatalf("an object without its own version must not write status")
	}
}

// Release candidate reads preserve the same error classification as repository reconciliation.
func TestReleaseRecheckDependencyErrorsKeepTheirClass(t *testing.T) {
	cases := []struct {
		name          string
		buildErr      error
		buildInfoErr  error
		wantPermanent bool
		wantCanceled  bool
	}{
		{
			name:     "build-network",
			buildErr: errors.New("apiserver unavailable"),
		},
		{
			name:          "build-forbidden",
			buildErr:      apierrors.NewForbidden(schema.GroupResource{Group: "ebs", Resource: "builds"}, testBuild, errors.New("denied")),
			wantPermanent: true,
		},
		{
			name:         "buildinfo-network",
			buildInfoErr: errors.New("apiserver unavailable"),
		},
		{
			name:          "buildinfo-invalid",
			buildInfoErr:  apierrors.NewInvalid(schema.GroupKind{Group: "ebs", Kind: "BuildInfo"}, testBuild, nil),
			wantPermanent: true,
		},
		{
			name:         "buildinfo-canceled",
			buildInfoErr: context.Canceled,
			wantCanceled: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := &stubPolicy{decision: PublishDecision{Publish: true}}
			client, artifacts, c := releaseCandidateFixture(t, nil, policy)
			client.GetBuildErr = tc.buildErr
			client.GetBuildInfoErr = tc.buildInfoErr

			_, err := syncReleaseForTest(c, context.Background(), testProject, testArch)
			if err == nil {
				t.Fatalf("a dependency read failure must surface as an error")
			}
			if got := controller.IsPermanent(err); got != tc.wantPermanent {
				t.Fatalf("permanent = %t, want %t (%v)", got, tc.wantPermanent, err)
			}
			if tc.wantCanceled && !errors.Is(err, context.Canceled) {
				t.Fatalf("a canceled read must keep the context error, got %v", err)
			}
			if len(client.StatusWrites) != 0 {
				t.Fatalf("a failed read must not write status, got %d writes", len(client.StatusWrites))
			}
			if policy.calls != 0 {
				t.Fatalf("the publish policy must not be consulted, got %d calls", policy.calls)
			}
			if len(artifacts.SubmitReleaseRequests) != 0 || len(artifacts.GetReleaseBuildNames) != 0 {
				t.Fatalf("a failed read must not reach Artifact Manager")
			}
		})
	}
}

func inFlightReleaseRepo(name string) *ebsv1.RpmRepo {
	repo := releasableRpmRepo(name)
	repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{
		Phase:      ebsv1.RpmRepoReleaseCreating,
		Transition: &ebsv1.ReleaseTransition{SourceRepositoryUID: "repo-1"},
	}
	return repo
}

func TestReconcileReleaseListFailureKeepsItsClass(t *testing.T) {
	for _, tc := range []struct {
		name          string
		err           error
		wantPermanent bool
	}{
		{name: "transient", err: errors.New("list unavailable")},
		{name: "forbidden", err: apierrors.NewForbidden(schema.GroupResource{Group: "ebs", Resource: "rpmrepos"}, testBuild, errors.New("denied")), wantPermanent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := NewFakeClient()
			client.ListRpmReposErr = tc.err
			artifacts := NewFakeArtifactManager()
			c := newTestController(t, client, artifacts, testConfig())

			_, err := syncReleaseForTest(c, context.Background(), testProject, testArch)
			if err == nil {
				t.Fatalf("a list failure must surface as an error")
			}
			if got := controller.IsPermanent(err); got != tc.wantPermanent {
				t.Fatalf("permanent = %t, want %t (%v)", got, tc.wantPermanent, err)
			}
			if len(client.StatusWrites) != 0 || len(artifacts.SubmitReleaseRequests) != 0 {
				t.Fatalf("a failed list must not produce writes or release requests")
			}
		})
	}
}

func TestReconcileReleaseCandidatePredicateSkipsObjects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ebsv1.RpmRepo)
	}{
		{
			name: "repository-batch-in-flight",
			mutate: func(repo *ebsv1.RpmRepo) {
				repo.Status.Repository.Transition = &ebsv1.RepositoryTransition{
					Inputs:        []ebsv1.RepositoryInput{{JobName: "job-b", SpecName: "gcc"}},
					RepositoryUID: "next-1",
				}
			},
		},
		{
			name:   "no-own-version",
			mutate: func(repo *ebsv1.RpmRepo) { repo.Status.Repository.SourceJobNames = nil },
		},
		{
			name: "deleting",
			mutate: func(repo *ebsv1.RpmRepo) {
				now := metav1.Now()
				repo.DeletionTimestamp = &now
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, artifacts, c := releaseCandidateFixture(t, tc.mutate, DefaultPublishPolicy{})
			result, err := syncReleaseForTest(c, context.Background(), testProject, testArch)
			if err != nil {
				t.Fatalf("sync: %v", err)
			}
			if result != (controller.ReconcileResult{}) {
				t.Fatalf("a skipped candidate must not requeue, got %+v", result)
			}
			if len(client.StatusWrites) != 0 || len(artifacts.SubmitReleaseRequests) != 0 || len(artifacts.GetReleaseBuildNames) != 0 {
				t.Fatalf("a skipped candidate must not touch status or Artifact Manager")
			}
		})
	}
}

func TestResumeReleaseStopsOnAbortOrMismatch(t *testing.T) {
	t.Run("aborted", func(t *testing.T) {
		client := NewFakeClient()
		repo := inFlightReleaseRepo(testBuild)
		client.RpmRepos[key(testProject, testBuild)] = repo
		client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
		build := newBuild(testBuild)
		build.Status.Phase = ebsv1.BuildAborted
		client.Builds[key(testProject, testBuild)] = build
		artifacts := NewFakeArtifactManager()
		c := newTestController(t, client, artifacts, testConfig())

		if _, err := syncReleaseForTest(c, context.Background(), testProject, testArch); err != nil {
			t.Fatalf("sync: %v", err)
		}
		stored := client.RpmRepos[key(testProject, testBuild)]
		if stored.Status.Release == nil || stored.Status.Release.Phase != ebsv1.RpmRepoReleaseFailed || stored.Status.Release.Transition == nil {
			t.Fatalf("an aborted in-flight release must be collected: %+v", stored.Status.Release)
		}
		if !conditionMatches(stored.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionFalse, ebsv1.RpmRepoReasonBuildAborted) {
			t.Fatalf("PublishSucceed=False/BuildAborted missing: %+v", stored.Status.Conditions)
		}
		if len(artifacts.GetReleaseBuildNames) != 0 {
			t.Fatalf("the abort terminal must not call Artifact Manager")
		}
	})

	t.Run("label-mismatch", func(t *testing.T) {
		client := NewFakeClient()
		repo := inFlightReleaseRepo(testBuild)
		repo.Labels = map[string]string{ebsv1.BuildTargetArchLabel: "x86_64"}
		client.RpmRepos[key(testProject, testBuild)] = repo
		client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
		client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
		artifacts := NewFakeArtifactManager()
		c := newTestController(t, client, artifacts, testConfig())

		if _, err := syncReleaseForTest(c, context.Background(), testProject, testArch); err != nil {
			t.Fatalf("sync: %v", err)
		}
		if len(client.StatusWrites) != 0 {
			t.Fatalf("a mismatched in-flight release must keep its checkpoint untouched")
		}
		if len(artifacts.GetReleaseBuildNames) != 0 {
			t.Fatalf("a mismatched in-flight release must not query Artifact Manager")
		}
	})
}

func TestHandleReleaseErrorPaths(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantDelay  time.Duration
		wantErr    bool
		wantFailed bool
	}{
		{
			name:       "deleting",
			err:        &artifactError{operation: "get-release", kind: artifactDeleting, code: "ReleaseDeleting", statusCode: 409},
			wantFailed: true,
		},
		{
			name:      "retry-with-header",
			err:       &artifactError{operation: "get-release", kind: artifactRetryable, code: "ReleaseStorageUnavailable", statusCode: 503, retryAfter: 4 * time.Second},
			wantDelay: 4 * time.Second,
		},
		{
			name:    "retry-without-header",
			err:     &artifactError{operation: "get-release", kind: artifactRetryable, code: "ReleaseInternalError", statusCode: 500},
			wantErr: true,
		},
		{
			name:       "invalid-request",
			err:        &artifactError{operation: "get-release", kind: artifactPermanent, code: "InvalidRequest", statusCode: 422},
			wantFailed: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := NewFakeClient()
			repo := inFlightReleaseRepo(testBuild)
			client.RpmRepos[key(testProject, testBuild)] = repo
			client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
			client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
			artifacts := NewFakeArtifactManager()
			artifacts.GetReleaseFunc = func(context.Context, string, string) (ReleaseResponse, error) {
				return ReleaseResponse{}, tc.err
			}
			c := newTestController(t, client, artifacts, testConfig())

			result, err := syncReleaseForTest(c, context.Background(), testProject, testArch)
			if tc.wantErr && err == nil {
				t.Fatalf("expected an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if result.RequeueAfter != tc.wantDelay {
				t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, tc.wantDelay)
			}
			stored := client.RpmRepos[key(testProject, testBuild)]
			failed := stored.Status.Release != nil && stored.Status.Release.Phase == ebsv1.RpmRepoReleaseFailed
			if failed != tc.wantFailed {
				t.Fatalf("release failed = %t, want %t", failed, tc.wantFailed)
			}
			if len(artifacts.SubmitReleaseRequests) != 0 {
				t.Fatalf("a query error must not trigger a replay")
			}
		})
	}
}

func TestReleaseReplayIsDeferredAfterOneAttempt(t *testing.T) {
	client := NewFakeClient()
	repo := inFlightReleaseRepo(testBuild)
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	artifacts := NewFakeArtifactManager()
	response := func(buildName string) ReleaseResponse {
		return ReleaseResponse{BuildName: buildName, State: ReleaseFailed, Failure: &FailureInfo{Retryable: true}, UpdatedAt: time.Now()}
	}
	artifacts.GetReleaseFunc = func(_ context.Context, _ string, buildName string) (ReleaseResponse, error) {
		return response(buildName), nil
	}
	artifacts.SubmitReleaseFunc = func(_ context.Context, req CreateReleaseRequest) (ReleaseResponse, error) {
		return response(req.BuildName), nil
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
		t.Fatalf("the second replay must be deferred to a later round, got %+v", result)
	}
	if len(client.StatusWrites) != 0 {
		t.Fatalf("a retryable failure must not write status")
	}
}

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
		return ReleaseResponse{BuildName: req.BuildName, State: ReleaseCreating, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
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

	result, err := syncReleaseForTest(c, context.Background(), testProject, testArch)
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
			result, err := syncReleaseForTest(c, context.Background(), testProject, testArch)
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

	if _, err := syncReleaseForTest(c, context.Background(), testProject, testArch); err == nil {
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

	if _, err := syncReleaseForTest(c, context.Background(), testProject, testArch); err != nil {
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
	artifacts.GetReleaseFunc = func(_ context.Context, _ string, buildName string) (ReleaseResponse, error) {
		return ReleaseResponse{BuildName: buildName, State: ReleaseFailed, Failure: &FailureInfo{Retryable: true}, UpdatedAt: time.Now()}, nil
	}
	artifacts.SubmitReleaseFunc = func(_ context.Context, req CreateReleaseRequest) (ReleaseResponse, error) {
		return ReleaseResponse{BuildName: req.BuildName, State: ReleaseCreating, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	if _, err := syncReleaseForTest(c, context.Background(), testProject, testArch); err != nil {
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

	result, err := syncReleaseForTest(c, context.Background(), testProject, testArch)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result != (controller.ReconcileResult{}) {
		t.Fatalf("a collected failure must not requeue, got %+v", result)
	}
	stored := client.RpmRepos[key(testProject, testBuild)]
	if stored.Status.Release == nil || stored.Status.Release.Phase != ebsv1.RpmRepoReleaseFailed || stored.Status.Release.Transition == nil {
		t.Fatalf("a non-retryable release failure must be collected: %+v", stored.Status.Release)
	}
	if !conditionMatches(stored.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionFalse, ebsv1.RpmRepoReasonReleaseFailed) {
		t.Fatalf("PublishSucceed=False/ReleaseFailed missing: %+v", stored.Status.Conditions)
	}
	if stored.Status.Repository.RepositoryUID != "repo-1" {
		t.Fatalf("the repository fields must not change on a release failure")
	}
}

func TestReleasePhaseIsNotRewrittenWhenItAlreadyMatches(t *testing.T) {
	client := NewFakeClient()
	repo := inFlightReleaseRepo(testBuild)
	repo.Status.Release.Phase = ebsv1.RpmRepoReleaseCreating
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	artifacts := NewFakeArtifactManager()
	artifacts.GetReleaseFunc = func(_ context.Context, _ string, buildName string) (ReleaseResponse, error) {
		return ReleaseResponse{BuildName: buildName, State: ReleaseCreating, PollAfterSeconds: 9, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := syncReleaseForTest(c, context.Background(), testProject, testArch)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.RequeueAfter != 9*time.Second {
		t.Fatalf("RequeueAfter = %s, want the poll delay", result.RequeueAfter)
	}
	if len(client.StatusWrites) != 0 {
		t.Fatalf("a matching phase must not be rewritten, got %d writes", len(client.StatusWrites))
	}
}

func TestResumeReleaseSurfacesReadFailures(t *testing.T) {
	client := NewFakeClient()
	repo := inFlightReleaseRepo(testBuild)
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.GetRpmRepoErr = errors.New("apiserver unavailable")
	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())

	if _, err := syncReleaseForTest(c, context.Background(), testProject, testArch); err == nil {
		t.Fatalf("a failed re-read must surface as an error")
	}
	if len(artifacts.GetReleaseBuildNames) != 0 {
		t.Fatalf("a failed re-read must not query Artifact Manager")
	}
}

func TestReleaseCandidateWithDeletedObjectIsSkipped(t *testing.T) {
	client := NewFakeClient()
	repo := releasableRpmRepo(testBuild)
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	delete(client.RpmRepos, key(testProject, testBuild))
	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())

	result, err := syncReleaseForTest(c, context.Background(), testProject, testArch)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result != (controller.ReconcileResult{}) {
		t.Fatalf("a vanished candidate must be skipped, got %+v", result)
	}
	if len(artifacts.SubmitReleaseRequests) != 0 {
		t.Fatalf("a vanished candidate must not be submitted")
	}
}

func TestResumeReleaseToleratesAClearedCheckpoint(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ebsv1.RpmRepo)
	}{
		{
			name:   "release-cleared",
			mutate: func(repo *ebsv1.RpmRepo) { repo.Status.Release = nil },
		},
		{
			name:   "transition-cleared",
			mutate: func(repo *ebsv1.RpmRepo) { repo.Status.Release.Transition = nil },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := NewFakeClient()
			// The list snapshot still says "in flight", but the reloaded object lost its checkpoint.
			snapshot := inFlightReleaseRepo(testBuild)
			fresh := inFlightReleaseRepo(testBuild)
			tc.mutate(fresh)
			client.RpmRepos[key(testProject, testBuild)] = fresh
			client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*snapshot.DeepCopy()}
			client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
			artifacts := NewFakeArtifactManager()
			c := newTestController(t, client, artifacts, testConfig())

			result, err := syncReleaseForTest(c, context.Background(), testProject, testArch)
			if err != nil {
				t.Fatalf("sync: %v", err)
			}
			if result != (controller.ReconcileResult{}) {
				t.Fatalf("a cleared checkpoint must converge this round, got %+v", result)
			}
			if len(artifacts.GetReleaseBuildNames) != 0 || len(artifacts.SubmitReleaseRequests) != 0 {
				t.Fatalf("a cleared checkpoint must not reach Artifact Manager")
			}
			if len(client.StatusWrites) != 0 {
				t.Fatalf("a cleared checkpoint must not write status")
			}
		})
	}
}

func TestReleaseReplayStopsWhenTheBuildIsGone(t *testing.T) {
	client := NewFakeClient()
	repo := inFlightReleaseRepo(testBuild)
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	// The first read (while loading the target) still sees the Build; the replay inside handleReleaseError does
	// not, which is the window this test is about.
	reads := 0
	client.GetBuildFunc = func(_ context.Context, project, name string) (*ebsv1.Build, error) {
		reads++
		if reads > 1 {
			return nil, apierrors.NewNotFound(schema.GroupResource{Group: "ebs", Resource: "builds"}, name)
		}
		return client.Builds[key(project, name)].DeepCopy(), nil
	}
	artifacts := NewFakeArtifactManager()
	// A missing Artifact Manager record forces the replay path, which then needs the owning Build.
	artifacts.GetReleaseFunc = func(context.Context, string, string) (ReleaseResponse, error) {
		return ReleaseResponse{}, &artifactError{operation: "get-release", kind: artifactNotFound, code: "ReleaseNotFound"}
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := syncReleaseForTest(c, context.Background(), testProject, testArch)
	if err != nil {
		t.Fatalf("a missing Build must stop the round instead of failing it: %v", err)
	}
	if result != (controller.ReconcileResult{}) {
		t.Fatalf("a missing Build must wait for the next round, got %+v", result)
	}
	if len(artifacts.SubmitReleaseRequests) != 0 {
		t.Fatalf("a missing Build must not submit a release")
	}
	stored := client.RpmRepos[key(testProject, testBuild)]
	if stored.Status.Release == nil || stored.Status.Release.Transition == nil {
		t.Fatalf("the in-flight checkpoint must survive: %+v", stored.Status.Release)
	}
}

func TestReleasePhaseAdvanceIsNotConfirmedWithoutTheCheckpoint(t *testing.T) {
	client := NewFakeClient()
	repo := inFlightReleaseRepo(testBuild)
	repo.Status.Release.Phase = ebsv1.RpmRepoReleasePending
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	// The phase advance lands without its checkpoint: the unknown write must not be confirmed.
	client.UpdateStatusHook = func(request, current *ebsv1.RpmRepo) (*ebsv1.RpmRepo, error) {
		updated := current.DeepCopy()
		updated.Status = request.DeepCopy().Status
		updated.Status.Release.Transition = nil
		updated.ResourceVersion = "2"
		client.RpmRepos[key(request.Namespace, request.Name)] = updated
		return nil, unknownWrite("update-status", source.RpmReposGVR, errors.New("connection reset while writing status"))
	}
	artifacts := NewFakeArtifactManager()
	artifacts.GetReleaseFunc = func(_ context.Context, _ string, buildName string) (ReleaseResponse, error) {
		return ReleaseResponse{BuildName: buildName, State: ReleaseCreating, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := syncReleaseForTest(c, context.Background(), testProject, testArch)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.RequeueAfter != conflictRequeueDelay {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, conflictRequeueDelay)
	}
	if len(artifacts.SubmitReleaseRequests) != 0 {
		t.Fatalf("an unconfirmed phase advance must not replay the release")
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
	artifacts.GetReleaseFunc = func(context.Context, string, string) (ReleaseResponse, error) {
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

func TestReconcileReleasePollsCreatingOncePerRound(t *testing.T) {
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
	artifacts.GetReleaseFunc = func(_ context.Context, _ string, buildName string) (ReleaseResponse, error) {
		return ReleaseResponse{BuildName: buildName, State: ReleaseCreating, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := syncReleaseForTest(c, context.Background(), testProject, testArch)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(artifacts.GetReleaseBuildNames) != 1 || len(artifacts.SubmitReleaseRequests) != 0 {
		t.Fatalf("creating release must only be polled once: %+v", artifacts)
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
	artifacts.GetReleaseFunc = func(_ context.Context, _ string, buildName string) (ReleaseResponse, error) {
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
	artifacts.GetReleaseFunc = func(context.Context, string, string) (ReleaseResponse, error) {
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
	artifacts.GetReleaseFunc = func(_ context.Context, _ string, buildName string) (ReleaseResponse, error) {
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
	artifacts.GetReleaseFunc = func(_ context.Context, _ string, buildName string) (ReleaseResponse, error) {
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

func TestReconcileReleaseSkipsSkippedObjectsFromAStaleSnapshot(t *testing.T) {
	client := NewFakeClient()
	// A synthesised shape: the list snapshot still carries a skipped object that looks releasable.
	repo := releasableRpmRepo(testBuild)
	repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseSkipped}
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())

	result, err := syncReleaseForTest(c, context.Background(), testProject, testArch)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result != (controller.ReconcileResult{}) {
		t.Fatalf("a skipped object must be skipped, got %+v", result)
	}
	if len(client.StatusWrites) != 0 {
		t.Fatalf("a skipped object must not be written, got %d writes", len(client.StatusWrites))
	}
	if client.GetBuildCalls != 0 {
		t.Fatalf("the terminal check must run before reading the Build, got %d reads", client.GetBuildCalls)
	}
	if len(artifacts.SubmitReleaseRequests) != 0 || len(artifacts.GetReleaseBuildNames) != 0 {
		t.Fatalf("a skipped object must not reach Artifact Manager")
	}
	if stored := client.RpmRepos[key(testProject, testBuild)]; stored.Status.Release == nil || stored.Status.Release.Phase != ebsv1.RpmRepoReleaseSkipped {
		t.Fatalf("the skipped terminal must survive: %+v", stored.Status.Release)
	}
}

func TestReleaseCandidateQueryExcludesSkipped(t *testing.T) {
	client, _, c := releaseCandidateFixture(t, nil, DefaultPublishPolicy{})
	if _, err := syncReleaseForTest(c, context.Background(), testProject, testArch); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(client.RpmRepoListOptions) == 0 {
		t.Fatalf("expected a release candidate list call")
	}
	options := client.RpmRepoListOptions[0]
	if options.FieldSelector != expectedNonTerminalSelector {
		t.Fatalf("release candidate field selector = %q, want %q", options.FieldSelector, expectedNonTerminalSelector)
	}
	if options.LabelSelector == "" {
		t.Fatalf("the release candidate query must keep its target label selector")
	}
}
