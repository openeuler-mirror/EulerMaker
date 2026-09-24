package rpmrepo

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

func TestReleasePhaseIsNotRewrittenWhenItAlreadyMatches(t *testing.T) {
	client := NewFakeClient()
	repo := inFlightReleaseRepo(testBuild)
	repo.Status.Release.Phase = ebsv1.RpmRepoReleaseCreating
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.RpmRepoSet[testProject] = []ebsv1.RpmRepo{*repo.DeepCopy()}
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	artifacts := NewFakeArtifactManager()
	artifacts.GetReleaseFunc = func(_ context.Context, buildName string) (ReleaseResponse, error) {
		return ReleaseResponse{BuildName: buildName, State: ReleaseCreating, Attempt: 2, PollAfterSeconds: 9, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch))
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

func TestFinishRepositoryConvergesWhenTheObjectDissapears(t *testing.T) {
	client := NewFakeClient()
	repo := newRpmRepo(testBuild)
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoProcessing)
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}
	writes := 0
	client.UpdateStatusHook = func(request, current *ebsv1.RpmRepo) (*ebsv1.RpmRepo, error) {
		writes++
		updated := current.DeepCopy()
		updated.Status = request.DeepCopy().Status
		updated.ResourceVersion = fmt.Sprintf("%d", writes+1)
		// The promotion is the second write: the object disappears right after it lands, before the
		// post-promotion re-read.
		if writes == 2 {
			delete(client.RpmRepos, key(request.Namespace, request.Name))
		} else {
			client.RpmRepos[key(request.Namespace, request.Name)] = updated
		}
		return updated, nil
	}
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
	if result != (controller.ReconcileResult{}) {
		t.Fatalf("an object that vanished after promotion must converge, got %+v", result)
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

	if _, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch)); err == nil {
		t.Fatalf("a failed re-read must surface as an error")
	}
	if len(artifacts.GetReleaseBuildNames) != 0 {
		t.Fatalf("a failed re-read must not query Artifact Manager")
	}
}

func TestCommitStatusTreatsPreconditionFailureAsConflict(t *testing.T) {
	client, artifacts := statusWriteFailureClient(t, &clientpkg.WriteError{
		Operation: "update-status", Resource: source.RpmReposGVR.GroupResource(),
		Outcome: clientpkg.WriteRejected, StatusCode: 412, Err: errors.New("precondition failed"),
	})
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), buildKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.RequeueAfter != conflictRequeueDelay {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, conflictRequeueDelay)
	}
	if len(artifacts.SubmitRepositoryRequests) != 0 {
		t.Fatalf("a rejected checkpoint must not submit")
	}
}

func TestBuildWithoutBuildInfoWaits(t *testing.T) {
	client := NewFakeClient()
	repo := newRpmRepo(testBuild)
	repo.Status.Repository.SourceJobUIDs = []string{"uid-job-a"}
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), buildKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result != (controller.ReconcileResult{}) {
		t.Fatalf("an absent BuildInfo must wait, got %+v", result)
	}
	if len(client.StatusWrites) != 0 || c.Queue().Len() != 0 {
		t.Fatalf("an absent BuildInfo must not write status or enqueue the release key")
	}
}

func TestRepositoryErrorPathHonoursCancellation(t *testing.T) {
	client := NewFakeClient()
	client.RpmRepos[key(testProject, testBuild)] = inFlightRepo()
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	artifacts := NewFakeArtifactManager()
	artifacts.GetRepositoryFunc = func(context.Context, string) (RepositoryResponse, error) {
		return RepositoryResponse{}, errors.New("artifact manager unavailable")
	}
	c := newTestController(t, client, artifacts, testConfig())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.sync(ctx, buildKey(testProject, testBuild))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled round must return the context error, got %v", err)
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

	result, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch))
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

			result, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch))
			if err != nil {
				t.Fatalf("sync: %v", err)
			}
			if result != (controller.ReconcileResult{}) {
				t.Fatalf("a cleared checkpoint must converge this round, got %+v", result)
			}
			if len(artifacts.GetReleaseBuildNames) != 0 || len(artifacts.SubmitReleaseRequests) != 0 || len(artifacts.ActivateReleaseNames) != 0 {
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
	artifacts.GetReleaseFunc = func(context.Context, string) (ReleaseResponse, error) {
		return ReleaseResponse{}, &artifactError{operation: "get-release", kind: artifactNotFound, code: "ReleaseNotFound"}
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch))
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
	// The phase advance lands without its checkpoint: the unknown write must not be confirmed, and nothing may
	// follow it (no activation with a checkpoint that no longer exists).
	client.UpdateStatusHook = func(request, current *ebsv1.RpmRepo) (*ebsv1.RpmRepo, error) {
		updated := current.DeepCopy()
		updated.Status = request.DeepCopy().Status
		updated.Status.Release.Transition = nil
		updated.ResourceVersion = "2"
		client.RpmRepos[key(request.Namespace, request.Name)] = updated
		return nil, unknownWrite("update-status", source.RpmReposGVR, errors.New("connection reset while writing status"))
	}
	artifacts := NewFakeArtifactManager()
	artifacts.GetReleaseFunc = func(_ context.Context, buildName string) (ReleaseResponse, error) {
		return ReleaseResponse{BuildName: buildName, State: ReleasePrepared, Attempt: 1, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	artifacts.ActivateReleaseFunc = func(_ context.Context, buildName string) (ReleaseResponse, error) {
		return ReleaseResponse{BuildName: buildName, State: ReleaseReady, Attempt: 1, ContentURL: "/repositories/project/openEuler/aarch64/", UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.RequeueAfter != conflictRequeueDelay {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, conflictRequeueDelay)
	}
	if len(artifacts.ActivateReleaseNames) != 0 {
		t.Fatalf("an unconfirmed phase advance must not be followed by an activation")
	}
}
