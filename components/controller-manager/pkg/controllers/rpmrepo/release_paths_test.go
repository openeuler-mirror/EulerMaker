package rpmrepo

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
)

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

			_, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch))
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
					Inputs:        []ebsv1.RepositoryInput{{JobName: "job-b", JobUID: "uid-job-b", SpecName: "gcc"}},
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
			result, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch))
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

		if _, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch)); err != nil {
			t.Fatalf("sync: %v", err)
		}
		stored := client.RpmRepos[key(testProject, testBuild)]
		if stored.Status.Release == nil || stored.Status.Release.Phase != ebsv1.RpmRepoReleaseFailed || stored.Status.Release.Transition != nil {
			t.Fatalf("an aborted in-flight release must be collected: %+v", stored.Status.Release)
		}
		if !conditionMatches(stored.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionFalse, ebsv1.RpmRepoReasonBuildAborted) {
			t.Fatalf("PublishSucceed=False/BuildAborted missing: %+v", stored.Status.Conditions)
		}
		if len(artifacts.GetReleaseBuildNames) != 0 || len(artifacts.ActivateReleaseNames) != 0 {
			t.Fatalf("the abort terminal must not call Artifact Manager")
		}
	})

	t.Run("label-mismatch", func(t *testing.T) {
		client := NewFakeClient()
		repo := inFlightReleaseRepo(testBuild)
		repo.Labels = map[string]string{ebsv1.BuildTargetOSLabel: testOS, ebsv1.BuildTargetArchLabel: "x86_64"}
		client.RpmRepos[key(testProject, testBuild)] = repo
		client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
		client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
		artifacts := NewFakeArtifactManager()
		c := newTestController(t, client, artifacts, testConfig())

		if _, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch)); err != nil {
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
			artifacts.GetReleaseFunc = func(context.Context, string) (ReleaseResponse, error) {
				return ReleaseResponse{}, tc.err
			}
			c := newTestController(t, client, artifacts, testConfig())

			result, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch))
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
		return ReleaseResponse{BuildName: buildName, State: ReleaseFailed, Attempt: 1, Failure: &FailureInfo{Retryable: true}, UpdatedAt: time.Now()}
	}
	artifacts.GetReleaseFunc = func(_ context.Context, buildName string) (ReleaseResponse, error) {
		return response(buildName), nil
	}
	artifacts.SubmitReleaseFunc = func(_ context.Context, req CreateReleaseRequest) (ReleaseResponse, error) {
		return response(req.BuildName), nil
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
		t.Fatalf("the second replay must be deferred to a later round, got %+v", result)
	}
	if len(client.StatusWrites) != 0 {
		t.Fatalf("a retryable failure must not write status")
	}
}
