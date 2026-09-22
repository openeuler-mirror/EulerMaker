package rpmrepo

import (
	"context"
	"errors"
	"testing"
	"time"

	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestReleaseRecheckSkipsCandidateWithoutBuildInfo(t *testing.T) {
	client, artifacts, c := releaseCandidateFixture(t, nil, DefaultPublishPolicy{})
	delete(client.BuildInfos, key(testProject, testBuild))

	result, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch))
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
	artifacts.GetJobManifestFunc = func(context.Context, string, string, string) (JobUploadManifest, error) {
		return completedManifest(100), nil
	}

	result, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch))
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
	if item != buildKey(testProject, testBuild) {
		t.Fatalf("unexpected queued key %v", item)
	}
}

func TestReleaseRecheckWaitsForNotReadyInputs(t *testing.T) {
	client, artifacts, c := releaseCandidateFixture(t, nil, DefaultPublishPolicy{})
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-b", "kernel", "uid-job-b", time.Unix(2, 0))}
	artifacts.GetJobManifestFunc = func(context.Context, string, string, string) (JobUploadManifest, error) {
		return JobUploadManifest{State: ManifestOpen}, nil
	}

	result, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result != (controller.ReconcileResult{}) {
		t.Fatalf("a candidate with not-ready inputs must wait, got %+v", result)
	}
	if len(artifacts.SubmitReleaseRequests) != 0 || c.Queue().Len() != 0 {
		t.Fatalf("a candidate with not-ready inputs must neither submit nor re-enqueue")
	}
}

func TestReleaseRecheckSkipsCandidateWithoutOwnVersion(t *testing.T) {
	client, artifacts, c := releaseCandidateFixture(t, func(repo *ebsv1.RpmRepo) {
		repo.Status.Repository.SourceJobUIDs = nil
	}, DefaultPublishPolicy{})
	// The candidate predicate already filters this object; keep the test honest by asserting nothing happens.
	result, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch))
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

// TestReleaseRecheckDependencyErrorsKeepTheirClass covers the design requirement that the release candidate and
// the first-release recheck classify Build / BuildInfo read failures exactly like the repository flow does.
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

			_, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch))
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
			if len(artifacts.SubmitReleaseRequests) != 0 || len(artifacts.GetReleaseBuildNames) != 0 || len(artifacts.ActivateReleaseNames) != 0 {
				t.Fatalf("a failed read must not reach Artifact Manager")
			}
		})
	}
}
