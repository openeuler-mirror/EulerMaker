package rpmrepo

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

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
	artifacts.GetJobManifestFunc = func(context.Context, string, string, string) (JobUploadManifest, error) {
		return completedManifest(100), nil
	}
	artifacts.SubmitRepositoryFunc = func(_ context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryCreating, Attempt: 1, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), buildKey(testProject, testBuild))
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
	artifacts.GetJobManifestFunc = func(context.Context, string, string, string) (JobUploadManifest, error) {
		return completedManifest(100), nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), buildKey(testProject, testBuild))
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

func TestCommitStatusDelaysOnConflict(t *testing.T) {
	client := NewFakeClient()
	repo := newRpmRepo(testBuild)
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}
	client.UpdateStatusHook = func(*ebsv1.RpmRepo, *ebsv1.RpmRepo) (*ebsv1.RpmRepo, error) {
		return nil, &clientpkg.WriteError{Operation: "update-status", Outcome: clientpkg.WriteRejected, StatusCode: 409, Err: fmt.Errorf("conflict")}
	}
	artifacts := NewFakeArtifactManager()
	artifacts.GetJobManifestFunc = func(context.Context, string, string, string) (JobUploadManifest, error) {
		return completedManifest(100), nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), buildKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.RequeueAfter != conflictRequeueDelay {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, conflictRequeueDelay)
	}
	if len(artifacts.SubmitRepositoryRequests) != 0 {
		t.Fatalf("a conflicting checkpoint must not be followed by an external request")
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
	artifacts.GetJobManifestFunc = func(context.Context, string, string, string) (JobUploadManifest, error) {
		return completedManifest(100), nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), buildKey(testProject, testBuild))
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
	artifacts.GetJobManifestFunc = func(context.Context, string, string, string) (JobUploadManifest, error) {
		return completedManifest(100), nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), buildKey(testProject, testBuild))
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
	artifacts.GetJobManifestFunc = func(context.Context, string, string, string) (JobUploadManifest, error) {
		return completedManifest(100), nil
	}
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

			_, err := c.sync(context.Background(), buildKey(testProject, testBuild))
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
	if _, err := c.sync(context.Background(), buildKey(testProject, testBuild)); err == nil || !controller.IsPermanent(err) {
		t.Fatalf("a local validation failure must be permanent, got %v", err)
	}

	transient := &clientpkg.WriteError{Operation: "update-status", Resource: source.RpmReposGVR.GroupResource(),
		Outcome: clientpkg.WriteNotSent, Err: &net.DNSError{Err: "temporary failure", IsTimeout: true}}
	client, artifacts = statusWriteFailureClient(t, transient)
	c = newTestController(t, client, artifacts, testConfig())
	err := func() error {
		_, err := c.sync(context.Background(), buildKey(testProject, testBuild))
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

	_, err := c.sync(ctx, buildKey(testProject, testBuild))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled round must return the context error, got %v", err)
	}
	if len(client.StatusWrites) != 0 {
		t.Fatalf("a canceled round must not write status")
	}
}
