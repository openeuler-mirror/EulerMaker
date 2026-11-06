package rpmrepo

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

// unknownWriteClient models a status write whose outcome cannot be confirmed: the request may or may not have
// reached the server.
func unknownWriteClient(t *testing.T, land bool) *FakeClient {
	t.Helper()
	client := NewFakeClient()
	repo := newRpmRepo(testBuild)
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}
	client.UpdateStatusHook = func(request, current *ebsv1.RpmRepo) (*ebsv1.RpmRepo, error) {
		if land {
			updated := current.DeepCopy()
			updated.Status = request.DeepCopy().Status
			updated.ResourceVersion = "2"
			client.RpmRepos[key(request.Namespace, request.Name)] = updated
		}
		return nil, unknownWrite("update-status", source.RpmReposGVR, fmt.Errorf("connection reset while writing status"))
	}
	return client
}

func TestCommitStatusConfirmsUnknownWriteByReadingBackTheIntent(t *testing.T) {
	client := unknownWriteClient(t, true)
	artifacts := NewFakeArtifactManager()
	artifacts.SubmitRepositoryFunc = func(_ context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryCreating, Attempt: 1, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("a confirmed unknown write must continue the round: %v", err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Fatalf("RequeueAfter = %s, want the poll delay", result.RequeueAfter)
	}
	if len(artifacts.SubmitRepositoryRequests) != 1 {
		t.Fatalf("the confirmed checkpoint must be used to submit once, got %d", len(artifacts.SubmitRepositoryRequests))
	}
}

func TestCommitStatusAbandonsUnconfirmedUnknownWrite(t *testing.T) {
	client := unknownWriteClient(t, false)
	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.RequeueAfter != conflictRequeueDelay {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, conflictRequeueDelay)
	}
	if repo := client.RpmRepos[key(testProject, testBuild)]; repo.Status.Repository.Transition != nil {
		t.Fatalf("an unconfirmed write must not be assumed to have landed")
	}
	if len(artifacts.SubmitRepositoryRequests) != 0 {
		t.Fatalf("no external request may follow an unconfirmed checkpoint")
	}
}

func TestCommitStatusRejectsMissingObjectAsConvergence(t *testing.T) {
	client := NewFakeClient()
	client.RpmRepos[key(testProject, testBuild)] = newRpmRepo(testBuild)
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}
	client.UpdateStatusHook = func(request, _ *ebsv1.RpmRepo) (*ebsv1.RpmRepo, error) {
		delete(client.RpmRepos, key(request.Namespace, request.Name))
		return nil, &clientpkg.WriteError{Operation: "update-status", Outcome: clientpkg.WriteRejected, StatusCode: 404, Err: fmt.Errorf("not found")}
	}
	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result != (controller.ReconcileResult{}) {
		t.Fatalf("a deleted object must converge this round, got %+v", result)
	}
	if len(artifacts.SubmitRepositoryRequests) != 0 {
		t.Fatalf("a deleted object must not be submitted")
	}
}

func TestCommitStatusRejectsPartiallyMatchingUnknownWrite(t *testing.T) {
	client := NewFakeClient()
	client.RpmRepos[key(testProject, testBuild)] = newRpmRepo(testBuild)
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}
	client.UpdateStatusHook = func(request, current *ebsv1.RpmRepo) (*ebsv1.RpmRepo, error) {
		// Another writer promoted a different version: the checkpoint identity matches but the batch does not,
		// so the unknown write must not be confirmed.
		updated := current.DeepCopy()
		updated.Status = request.DeepCopy().Status
		updated.Status.Repository.Transition.RepositoryUID = "someone-else"
		updated.ResourceVersion = "2"
		client.RpmRepos[key(request.Namespace, request.Name)] = updated
		return nil, unknownWrite("update-status", source.RpmReposGVR, fmt.Errorf("connection reset while writing status"))
	}
	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.RequeueAfter != conflictRequeueDelay {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, conflictRequeueDelay)
	}
	if len(artifacts.SubmitRepositoryRequests) != 0 {
		t.Fatalf("an unconfirmed checkpoint must not be submitted")
	}
}

func statusWriteFailureClient(t *testing.T, failure error) (*FakeClient, *FakeArtifactManager) {
	t.Helper()
	client := NewFakeClient()
	client.RpmRepos[key(testProject, testBuild)] = newRpmRepo(testBuild)
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}
	client.UpdateStatusHook = func(*ebsv1.RpmRepo, *ebsv1.RpmRepo) (*ebsv1.RpmRepo, error) {
		return nil, failure
	}
	artifacts := NewFakeArtifactManager()
	return client, artifacts
}

func TestCommitStatusClassifiesRejectedWrites(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		wantPermanent bool
	}{
		{name: "field-invalid", status: 422, wantPermanent: true},
		{name: "forbidden", status: 403, wantPermanent: true},
		{name: "rate-limited", status: 429},
		{name: "unavailable", status: 503},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failure := &clientpkg.WriteError{Operation: "update-status", Resource: source.RpmReposGVR.GroupResource(),
				Outcome: clientpkg.WriteRejected, StatusCode: tc.status, Err: fmt.Errorf("rejected with %d", tc.status)}
			client, artifacts := statusWriteFailureClient(t, failure)
			c := newTestController(t, client, artifacts, testConfig())

			_, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
			if err == nil {
				t.Fatalf("status write failure %d must surface as an error", tc.status)
			}
			if got := controller.IsPermanent(err); got != tc.wantPermanent {
				t.Fatalf("permanent = %t, want %t for status %d", got, tc.wantPermanent, tc.status)
			}
			if len(artifacts.SubmitRepositoryRequests) != 0 {
				t.Fatalf("an unconfirmed checkpoint must not be submitted")
			}
		})
	}
}

func TestCommitStatusClassifiesNotSentWrites(t *testing.T) {
	local := &clientpkg.WriteError{Operation: "update-status", Resource: source.RpmReposGVR.GroupResource(),
		Outcome: clientpkg.WriteNotSent, Err: fmt.Errorf("metadata does not match target")}
	client, artifacts := statusWriteFailureClient(t, local)
	c := newTestController(t, client, artifacts, testConfig())
	if _, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild)); err == nil || !controller.IsPermanent(err) {
		t.Fatalf("a local validation failure must be permanent, got %v", err)
	}

	transient := &clientpkg.WriteError{Operation: "update-status", Resource: source.RpmReposGVR.GroupResource(),
		Outcome: clientpkg.WriteNotSent, Err: &net.DNSError{Err: "temporary failure", IsTimeout: true}}
	client, artifacts = statusWriteFailureClient(t, transient)
	c = newTestController(t, client, artifacts, testConfig())
	err := func() error {
		_, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
		return err
	}()
	if err == nil {
		t.Fatalf("a transient transport failure must surface as an error")
	}
	if controller.IsPermanent(err) {
		t.Fatalf("a transient transport failure must stay retryable, got %v", err)
	}
}

func TestCommitStatusStopsOnCanceledContext(t *testing.T) {
	client, artifacts := statusWriteFailureClient(t, fmt.Errorf("unused"))
	client.UpdateStatusHook = nil
	c := newTestController(t, client, artifacts, testConfig())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.sync(ctx, rpmRepoKey(testProject, testBuild))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled round must return the context error, got %v", err)
	}
	if len(client.StatusWrites) != 0 {
		t.Fatalf("a canceled round must not write status")
	}
}

func TestCommitStatusDelaysOnConflict(t *testing.T) {
	for _, status := range []int{409, 412} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			client, artifacts := statusWriteFailureClient(t, &clientpkg.WriteError{
				Operation:  "update-status",
				Outcome:    clientpkg.WriteRejected,
				StatusCode: status,
				Err:        fmt.Errorf("write conflict"),
			})
			c := newTestController(t, client, artifacts, testConfig())
			result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
			if err != nil {
				t.Fatalf("sync: %v", err)
			}
			if result.RequeueAfter != conflictRequeueDelay {
				t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, conflictRequeueDelay)
			}
			if len(artifacts.SubmitRepositoryRequests) != 0 {
				t.Fatal("a conflicting checkpoint must not be followed by an external request")
			}
		})
	}
}

func TestBuildWithoutBuildInfoWaits(t *testing.T) {
	client := NewFakeClient()
	repo := newRpmRepo(testBuild)
	repo.Status.Repository.SourceJobNames = []string{"job-a"}
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
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

// skippedWithBaseline creates a terminal repository with or without an available repository version.

func TestBackoffDelayGrowsExponentiallyAndCaps(t *testing.T) {
	r := &reconciler{controller: &Controller{config: testConfig()}}
	r.controller.config.Backoff = Backoff{Initial: time.Second, Max: 4 * time.Second, Jitter: 0}
	for _, tc := range []struct {
		attempt int
		want    time.Duration
	}{
		{0, time.Second},
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 4 * time.Second},
		{9, 4 * time.Second},
	} {
		if got := r.backoffDelay(tc.attempt); got != tc.want {
			t.Fatalf("backoffDelay(%d) = %s, want %s", tc.attempt, got, tc.want)
		}
	}
}

func TestWindowRemainingUsesTheResponseAnchor(t *testing.T) {
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	if got := windowRemaining(time.Time{}, time.Second, now); got != 0 {
		t.Fatalf("a missing anchor must report no remaining window, got %s", got)
	}
	if got := windowRemaining(now.Add(-500*time.Millisecond), time.Second, now); got != 500*time.Millisecond {
		t.Fatalf("remaining window = %s, want 500ms", got)
	}
	if got := windowRemaining(now.Add(-2*time.Second), time.Second, now); got != 0 {
		t.Fatalf("an elapsed window must report zero, got %s", got)
	}
}

func TestSelectArtifactManagerUsesProjectInstance(t *testing.T) {
	client := NewFakeClient()
	client.Projects[testProject] = &ebsv1.Project{ObjectMeta: metav1.ObjectMeta{
		Name: testProject, UID: "project-uid", Annotations: map[string]string{ebsv1.ProjectArtifactsAnnotation: "art-01"},
	}}
	client.ArtifactEndpoints["art-01"] = "http://artifact-manager.art-01:8081"
	selected := ""
	c := newTestController(t, client, NewFakeArtifactManager(), testConfig())
	c.services = client
	c.artifactFactory = func(address string) (ArtifactManagerClient, error) {
		selected = address
		return NewFakeArtifactManager(), nil
	}
	r := &reconciler{controller: c, ctx: context.Background(), project: testProject}
	if err := r.selectArtifactManager(newRpmRepo(testBuild)); err != nil {
		t.Fatal(err)
	}
	if r.instance != "art-01" || selected != "http://artifact-manager.art-01:8081" || r.artifactClient() == nil {
		t.Fatalf("selected instance=%q address=%q client=%v", r.instance, selected, r.artifactClient())
	}
	client.ArtifactEndpoints["art-02"] = "http://artifact-manager.art-02:8081"
	client.Projects[testProject].Annotations[ebsv1.ProjectArtifactsAnnotation] = "art-02"
	repo := newRpmRepo(testBuild)
	repo.Status.Repository = &ebsv1.RpmRepoRepositoryStatus{ContentURL: "artifact://art-01/repositories/v1/previous/"}
	r = &reconciler{controller: c, ctx: context.Background(), project: testProject}
	if err := r.selectArtifactManager(repo); err != nil || r.instance != "art-01" {
		t.Fatalf("existing repository must remain on its instance: %q, %v", r.instance, err)
	}
}

func TestFailedBuildInfoStopsRpmRepo(t *testing.T) {
	for _, scenario := range []string{"repository", "release", "terminal"} {
		t.Run(scenario, func(t *testing.T) {
			client := NewFakeClient()
			repo := newRpmRepo(testBuild)
			if scenario == "release" {
				repo = inFlightReleaseRepo(testBuild)
			}
			if scenario == "terminal" {
				repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseSkipped}
			}
			client.RpmRepos[key(testProject, testBuild)] = repo
			client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
			client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoFailed)
			artifacts := NewFakeArtifactManager()
			controller := newTestController(t, client, artifacts, testConfig())
			if _, err := controller.sync(context.Background(), rpmRepoKey(testProject, testBuild)); err != nil {
				t.Fatal(err)
			}
			stored := client.RpmRepos[key(testProject, testBuild)]
			if scenario == "terminal" {
				if stored.Status.Release.Phase != ebsv1.RpmRepoReleaseSkipped || len(client.StatusWrites) != 0 {
					t.Fatalf("terminal RpmRepo changed: %+v", stored.Status.Release)
				}
				return
			}
			if stored.Status.Release == nil || stored.Status.Release.Phase != ebsv1.RpmRepoReleaseFailed ||
				(scenario == "release") != (stored.Status.Release.Transition != nil) {
				t.Fatalf("release = %+v", stored.Status.Release)
			}
			if !conditionMatches(stored.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionFalse, ebsv1.RpmRepoReasonBuildInfoFailed) {
				t.Fatalf("conditions = %+v", stored.Status.Conditions)
			}
			if len(artifacts.GetReleaseBuildNames) != 0 || len(artifacts.SubmitReleaseRequests) != 0 {
				t.Fatal("failed BuildInfo called Artifact Manager")
			}
		})
	}
}

func TestReconcileBuildCollectsAbortedTerminal(t *testing.T) {
	client := NewFakeClient()
	repo := newRpmRepo(testBuild)
	repo.Status.Repository.Transition = &ebsv1.RepositoryTransition{
		Inputs:        []ebsv1.RepositoryInput{{JobName: "job-a", SpecName: "gcc"}},
		RepositoryUID: "next-1",
	}
	client.RpmRepos[key(testProject, testBuild)] = repo
	build := newBuild(testBuild)
	build.Status.Phase = ebsv1.BuildAborted
	client.Builds[key(testProject, testBuild)] = build

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

func TestReconcileBuildAcknowledgesOrphanRpmRepoCleanup(t *testing.T) {
	client := NewFakeClient()
	client.RpmRepos[key(testProject, testBuild)] = newRpmRepo(testBuild)

	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result != (controller.ReconcileResult{}) {
		t.Fatalf("unexpected result %+v", result)
	}
	if len(client.StatusWrites) != 1 || !meta.IsStatusConditionTrue(client.RpmRepos[key(testProject, testBuild)].Status.Conditions, ebsv1.RpmRepoConditionCleanupReady) {
		t.Fatal("missing parent must not block standalone BuildInfo cleanup")
	}
}

func TestReconcileBuildRetriesWhenRpmRepoIsMissing(t *testing.T) {
	client := NewFakeClient()
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)

	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())

	if _, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild)); err == nil {
		t.Fatalf("a readable Build without its RpmRepo must return a retryable error")
	}
}

func TestReconcileRejectsMalformedKey(t *testing.T) {
	client := NewFakeClient()
	c := newTestController(t, client, NewFakeArtifactManager(), testConfig())
	if _, err := c.sync(context.Background(), "release/project/openEuler/aarch64"); err == nil || !controller.IsPermanent(err) {
		t.Fatalf("a malformed key must be permanent, got %v", err)
	}
}
