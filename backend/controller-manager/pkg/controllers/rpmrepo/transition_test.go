package rpmrepo

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
)

// scanFixture prepares one RpmRepo, Build and a Processing BuildInfo with a single succeeded Job.
func scanFixture(t *testing.T, job ebsv1.Job, artifacts *FakeArtifactManager, client *FakeClient) *Controller {
	t.Helper()
	client.RpmRepos[key(testProject, testBuild)] = newRpmRepo(testBuild)
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoProcessing)
	client.Jobs[testProject] = []ebsv1.Job{job}
	return newTestController(t, client, artifacts, testConfig())
}

func TestScanDecodesSpecNameForRepositoryInput(t *testing.T) {
	const raw = "dvd+rw-tools"
	client := NewFakeClient()
	job := newSucceededJob("job-a", raw, "uid-job-a", time.Unix(1, 0))
	controller := scanFixture(t, job, NewFakeArtifactManager(), client)
	reconciler := &reconciler{controller: controller, ctx: context.Background(), project: testProject, key: rpmRepoKey(testProject, testBuild)}
	scan, err := reconciler.scanCandidates(client.RpmRepos[key(testProject, testBuild)], client.Builds[key(testProject, testBuild)])
	if err != nil || len(scan) != 1 || scan[0].specName != raw {
		t.Fatalf("scanCandidates = %+v, %v; want original SPEC name", scan, err)
	}
	if inputs := repositoryInputs(scan); len(inputs) != 1 || inputs[0].SpecName != raw {
		t.Fatalf("repository inputs lost original SPEC name: %+v", inputs)
	}
}

func TestScanRejectsJobsThatAreNotCandidates(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ebsv1.Job)
	}{
		{
			name:   "not-succeeded",
			mutate: func(job *ebsv1.Job) { job.Status.Phase = ebsv1.JobRunning },
		},
		{
			name:   "missing-spec-name",
			mutate: func(job *ebsv1.Job) { delete(job.Labels, ebsv1.JobSpecNameLabel) },
		},
		{
			name:   "missing-target-arch",
			mutate: func(job *ebsv1.Job) { delete(job.Labels, ebsv1.BuildTargetArchLabel) },
		},
		{
			name:   "foreign-target-arch",
			mutate: func(job *ebsv1.Job) { job.Labels[ebsv1.BuildTargetArchLabel] = "x86_64" },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := NewFakeClient()
			job := newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))
			tc.mutate(&job)
			artifacts := NewFakeArtifactManager()
			c := scanFixture(t, job, artifacts, client)

			result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
			if err != nil {
				t.Fatalf("sync: %v", err)
			}
			if result != (controller.ReconcileResult{}) {
				t.Fatalf("a job that is not a candidate must not start a batch: %+v", result)
			}
			if len(artifacts.SubmitRepositoryRequests) != 0 {
				t.Fatalf("a rejected candidate must not be submitted")
			}
			if len(client.StatusWrites) != 0 {
				t.Fatalf("a rejected candidate must not write status")
			}
		})
	}
}

func TestScanSkipsConsumedJobs(t *testing.T) {
	client := NewFakeClient()
	repo := newRpmRepo(testBuild)
	repo.Status.Repository.SourceJobNames = []string{"job-a"}
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoProcessing)
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}
	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())

	if _, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(artifacts.SubmitRepositoryRequests) != 0 {
		t.Fatalf("a consumed Job must not be submitted again")
	}
	if len(client.StatusWrites) != 0 {
		t.Fatalf("a consumed Job must not produce a write")
	}
}

func TestScanStopsSelectingWhenBatchIsFull(t *testing.T) {
	tests := []struct {
		name         string
		specs        []string
		maxJobs      int
		reverseJobs  bool
		wantSelected []string
	}{
		{
			name:  "job count limit",
			specs: []string{"gcc", "glibc", "bash"}, maxJobs: 2,
			reverseJobs:  true,
			wantSelected: []string{"job-a", "job-b"},
		},
		{
			name:  "duplicate spec does not fill batch",
			specs: []string{"gcc", "gcc", "glibc", "bash"}, maxJobs: 2,
			wantSelected: []string{"job-a", "job-c"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewFakeClient()
			for i, spec := range tt.specs {
				name := "job-" + string(rune('a'+i))
				client.Jobs[testProject] = append(client.Jobs[testProject], newSucceededJob(name, spec, "uid-"+name, time.Unix(int64(i+1), 0)))
			}
			if tt.reverseJobs {
				jobs := client.Jobs[testProject]
				for i, j := 0, len(jobs)-1; i < j; i, j = i+1, j-1 {
					jobs[i], jobs[j] = jobs[j], jobs[i]
				}
			}
			artifacts := NewFakeArtifactManager()
			config := testConfig()
			config.MaxJobsPerBatch = tt.maxJobs
			c := newTestController(t, client, artifacts, config)
			r := &reconciler{controller: c, ctx: context.Background(), project: testProject}
			scan, err := r.scanCandidates(newRpmRepo(testBuild), newBuild(testBuild))
			if err != nil {
				t.Fatalf("scanCandidates: %v", err)
			}
			selection := selectBatch(scan, config.MaxJobsPerBatch)
			var gotSelected []string
			for _, item := range selection {
				gotSelected = append(gotSelected, item.name)
			}
			if !reflect.DeepEqual(gotSelected, tt.wantSelected) {
				t.Fatalf("selected jobs = %v, want %v", gotSelected, tt.wantSelected)
			}
		})
	}
}

func TestCandidateScanUsesTheBuildNameSelector(t *testing.T) {
	client := NewFakeClient()
	job := newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))
	artifacts := NewFakeArtifactManager()
	artifacts.SubmitRepositoryFunc = func(_ context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryCreating, Attempt: 1, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	c := scanFixture(t, job, artifacts, client)
	if _, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(client.JobListOptions) != 1 {
		t.Fatalf("expected one Job list call, got %d", len(client.JobListOptions))
	}
	options := client.JobListOptions[0]
	if options.LabelSelector != "ebs.io/build-name="+testBuild {
		t.Fatalf("unexpected Job label selector %q", options.LabelSelector)
	}
	if options.FieldSelector != "" {
		t.Fatalf("the Job list must not carry a field selector, got %q", options.FieldSelector)
	}
}

func TestRepositoryAdvancesWhileBuildInfoIsStillProcessing(t *testing.T) {
	client := NewFakeClient()
	job := newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))
	artifacts := NewFakeArtifactManager()
	artifacts.SubmitRepositoryFunc = func(_ context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryReady, Attempt: 1, ContentURL: "/repositories/project/builds/build/next/", UpdatedAt: time.Now()}, nil
	}
	c := scanFixture(t, job, artifacts, client)

	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result != (controller.ReconcileResult{}) {
		t.Fatalf("a promoted batch with an unfinished BuildInfo must just wait, got %+v", result)
	}
	updated := client.RpmRepos[key(testProject, testBuild)]
	if updated.Status.Repository.Transition != nil || len(updated.Status.Repository.SourceJobNames) != 1 {
		t.Fatalf("the batch must be promoted while BuildInfo is Processing: %+v", updated.Status.Repository)
	}
	if updated.Status.Release != nil {
		t.Fatalf("an unfinished BuildInfo must not start a release: %+v", updated.Status.Release)
	}
	if c.Queue().Len() != 0 {
		t.Fatalf("an unfinished BuildInfo must not enqueue the release key")
	}
}

func TestGetRepositoryResponseDispatch(t *testing.T) {
	cases := []struct {
		name       string
		state      RepositoryState
		poll       int
		failure    *FailureInfo
		err        error
		wantDelay  time.Duration
		wantFailed bool
	}{
		{
			name:      "creating",
			state:     RepositoryCreating,
			poll:      7,
			wantDelay: 7 * time.Second,
		},
		{
			name:      "creating-without-poll",
			state:     RepositoryCreating,
			wantDelay: 5 * time.Second,
		},
		{
			name:       "non-retryable-failure",
			state:      RepositoryFailed,
			failure:    &FailureInfo{Retryable: false},
			wantFailed: true,
		},
		{
			name:       "deleting",
			state:      RepositoryDeleting,
			wantFailed: true,
		},
		{
			name:       "expired-input",
			err:        &artifactError{operation: "get-repository", kind: artifactPermanent, code: "MaterializationInputExpired"},
			wantFailed: true,
		},
		{
			name:      "queue-full",
			err:       &artifactError{operation: "get-repository", kind: artifactRetryable, code: "RepositoryQueueFull", retryAfter: 3 * time.Second},
			wantDelay: 3 * time.Second,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := NewFakeClient()
			client.RpmRepos[key(testProject, testBuild)] = inFlightRepo()
			client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
			artifacts := NewFakeArtifactManager()
			artifacts.GetRepositoryFunc = func(_ context.Context, _, _ string, repositoryUID string) (RepositoryResponse, error) {
				if tc.err != nil {
					return RepositoryResponse{}, tc.err
				}
				return RepositoryResponse{RepositoryUID: repositoryUID, State: tc.state, Attempt: 1, PollAfterSeconds: tc.poll, Failure: tc.failure, UpdatedAt: time.Now()}, nil
			}
			c := newTestController(t, client, artifacts, testConfig())

			result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
			if err != nil {
				t.Fatalf("sync: %v", err)
			}
			if result.RequeueAfter != tc.wantDelay {
				t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, tc.wantDelay)
			}
			updated := client.RpmRepos[key(testProject, testBuild)]
			failed := updated.Status.Release != nil && updated.Status.Release.Phase == ebsv1.RpmRepoReleaseFailed
			if failed != tc.wantFailed {
				t.Fatalf("release failed terminal = %t, want %t", failed, tc.wantFailed)
			}
			if len(artifacts.SubmitRepositoryRequests) != 0 {
				t.Fatalf("no response in this table may trigger a replay")
			}
		})
	}
}

func TestFinishRepositoryConvergesWhenTheObjectDisappears(t *testing.T) {
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
	artifacts.SubmitRepositoryFunc = func(_ context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryReady, Attempt: 1, ContentURL: "/repositories/project/builds/build/next/", UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result != (controller.ReconcileResult{}) {
		t.Fatalf("an object that vanished after promotion must converge, got %+v", result)
	}
}

func TestRepositoryErrorPathHonoursCancellation(t *testing.T) {
	client := NewFakeClient()
	client.RpmRepos[key(testProject, testBuild)] = inFlightRepo()
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	artifacts := NewFakeArtifactManager()
	artifacts.GetRepositoryFunc = func(context.Context, string, string, string) (RepositoryResponse, error) {
		return RepositoryResponse{}, errors.New("artifact manager unavailable")
	}
	c := newTestController(t, client, artifacts, testConfig())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.sync(ctx, rpmRepoKey(testProject, testBuild))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled round must return the context error, got %v", err)
	}
}

// inFlightRepo builds an object whose repository batch is already checkpointed.
func inFlightRepo() *ebsv1.RpmRepo {
	repo := newRpmRepo(testBuild)
	repo.Status.Repository.Transition = &ebsv1.RepositoryTransition{
		Inputs:        []ebsv1.RepositoryInput{{JobName: "job-a", SpecName: "gcc"}},
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
	artifacts.GetRepositoryFunc = func(_ context.Context, _, _ string, repositoryUID string) (RepositoryResponse, error) {
		// A retryable failure without updatedAt is a response contract error: the controller must not replay.
		return RepositoryResponse{RepositoryUID: repositoryUID, State: RepositoryFailed, Attempt: 1, Failure: &FailureInfo{Retryable: true}}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
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
	artifacts.GetRepositoryFunc = func(context.Context, string, string, string) (RepositoryResponse, error) {
		return RepositoryResponse{}, notFound
	}
	artifacts.SubmitRepositoryFunc = func(context.Context, CreateRepositoryRequest) (RepositoryResponse, error) {
		return RepositoryResponse{}, &artifactError{operation: "submit-repository", kind: artifactNotFound, code: "RepositoryNotFound"}
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(artifacts.SubmitRepositoryRequests) != 1 {
		t.Fatalf("expected exactly one same-round replay, got %d", len(artifacts.SubmitRepositoryRequests))
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("a deferred replay must ask for a later round, got %+v", result)
	}
	if _, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild)); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if len(artifacts.SubmitRepositoryRequests) != 2 {
		t.Fatalf("each round may replay once, got %d submits after two rounds", len(artifacts.SubmitRepositoryRequests))
	}
}

func TestReconcileBuildDefersReadyWithoutContentURL(t *testing.T) {
	client := NewFakeClient()
	client.RpmRepos[key(testProject, testBuild)] = newRpmRepo(testBuild)
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoProcessing)
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}

	artifacts := NewFakeArtifactManager()
	artifacts.SubmitRepositoryFunc = func(_ context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryReady, Attempt: 1, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
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

func TestMaterializeRetriesCountsOnlyRealReplays(t *testing.T) {
	before := materializeRetries.Value()

	// Scheduled retry without a same-round replay must not count.
	client := NewFakeClient()
	client.RpmRepos[key(testProject, testBuild)] = inFlightRepo()
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	artifacts := NewFakeArtifactManager()
	artifacts.GetRepositoryFunc = func(context.Context, string, string, string) (RepositoryResponse, error) {
		return RepositoryResponse{}, &artifactError{operation: "get-repository", kind: artifactRetryable, code: "RepositoryQueueFull", retryAfter: 5 * time.Second}
	}
	c := newTestController(t, client, artifacts, testConfig())
	if _, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild)); err != nil {
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
	artifacts.GetRepositoryFunc = func(_ context.Context, _, _ string, repositoryUID string) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: repositoryUID, State: RepositoryFailed, Attempt: 1, Failure: &FailureInfo{Retryable: true}, UpdatedAt: time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)}, nil
	}
	artifacts.SubmitRepositoryFunc = func(_ context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryCreating, Attempt: 2, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	c = newTestController(t, client, artifacts, testConfig())
	if _, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if delta := materializeRetries.Value() - before; delta != 1 {
		t.Fatalf("one replay must count exactly once, delta %d", delta)
	}
}

func TestReconcileBuildRejectsForeignRepositoryResponse(t *testing.T) {
	client := NewFakeClient()
	client.RpmRepos[key(testProject, testBuild)] = inFlightRepo()
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)

	artifacts := NewFakeArtifactManager()
	artifacts.GetRepositoryFunc = func(context.Context, string, string, string) (RepositoryResponse, error) {
		// The response belongs to another repository: it must never be used to promote this object.
		return RepositoryResponse{RepositoryUID: "other-repository", State: RepositoryReady, Attempt: 1, ContentURL: "/repositories/project/builds/build/other/", UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
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

func TestReconcileBuildReRegistersAbandonedBatchTerminal(t *testing.T) {
	client := NewFakeClient()
	repo := inFlightRepo()
	conditions, _ := MergeCondition(nil, ebsv1.RpmRepoConditionRepositoryReady, metav1.ConditionFalse, ebsv1.RpmRepoReasonRepositoryCreationFailed, "", 1, metav1.Now())
	conditions, _ = MergeCondition(conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionFalse, ebsv1.RpmRepoReasonRepositoryCreationFailed, "", 1, metav1.Now())
	repo.Status.Conditions = conditions
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)

	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())
	failedBefore := repositoryFailed.Value()

	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result != (controller.ReconcileResult{}) {
		t.Fatalf("unexpected result %+v", result)
	}
	updated := client.RpmRepos[key(testProject, testBuild)]
	if updated.Status.Release == nil || updated.Status.Release.Phase != ebsv1.RpmRepoReleaseFailed {
		t.Fatalf("the abandoned batch must re-register its release terminal: %+v", updated.Status.Release)
	}
	if !conditionMatches(updated.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionFalse, ebsv1.RpmRepoReasonRepositoryCreationFailed) {
		t.Fatalf("PublishSucceed=False/RepositoryCreationFailed missing: %+v", updated.Status.Conditions)
	}
	if updated.Status.Repository.UpdatedAt != nil {
		t.Fatalf("re-registering the terminal must not write repository.*")
	}
	if updated.Status.Repository.Transition == nil || updated.Status.Repository.Transition.RepositoryUID != "next-1" {
		t.Fatalf("the abandoned batch must stay frozen: %+v", updated.Status.Repository.Transition)
	}
	if delta := repositoryFailed.Value() - failedBefore; delta != 0 {
		t.Fatalf("re-registering must not count another repository failure, delta %d", delta)
	}
	if len(artifacts.SubmitRepositoryRequests) != 0 || len(artifacts.GetRepositoryUIDs) != 0 {
		t.Fatalf("re-registering the terminal must not call Artifact Manager")
	}
}

func TestReconcileBuildNoPublishableArtifactsWritesReleaseSideOnly(t *testing.T) {
	client := NewFakeClient()
	repo := newRpmRepo(testBuild)
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)

	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())

	if _, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	updated := client.RpmRepos[key(testProject, testBuild)]
	if updated.Status.Repository.UpdatedAt != nil {
		t.Fatalf("the no-artifacts terminal must not write repository.*")
	}
	if conditionMatches(updated.Status.Conditions, ebsv1.RpmRepoConditionRepositoryReady, metav1.ConditionFalse, ebsv1.RpmRepoReasonNoPublishableArtifacts) {
		t.Fatalf("the no-artifacts terminal must not claim a repository result")
	}
	if !conditionMatches(updated.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionFalse, ebsv1.RpmRepoReasonNoPublishableArtifacts) {
		t.Fatalf("PublishSucceed=False/NoPublishableArtifacts missing: %+v", updated.Status.Conditions)
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
		ebsv1.RepositoryInput{JobName: "job-b", SpecName: "glibc"})
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoProcessing)
	client.Jobs[testProject] = []ebsv1.Job{
		newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0)),
		newSucceededJob("job-b", "glibc", "uid-job-b", time.Unix(2, 0)),
	}
	artifacts := NewFakeArtifactManager()
	artifacts.GetRepositoryFunc = func(_ context.Context, _, _ string, uid string) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: uid, State: RepositoryFailed, Attempt: 1, UpdatedAt: time.Now(),
			Failure: &FailureInfo{Code: "ManifestInvalid", JobName: "job-a"}}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())
	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
	if err != nil || !result.Requeue {
		t.Fatalf("skip result = %+v, %v", result, err)
	}
	updated := client.RpmRepos[key(testProject, testBuild)]
	if updated.Status.Repository.Transition != nil || !reflect.DeepEqual(updated.Status.Repository.SkippedJobNames, []string{"job-a"}) {
		t.Fatalf("failed input was not durably skipped: %+v", updated.Status.Repository)
	}
	if updated.Status.Release != nil || len(updated.Status.Repository.SourceJobNames) != 0 {
		t.Fatalf("skipping must not create a release or mark input as materialized: %+v", updated.Status)
	}
	artifacts.SubmitRepositoryFunc = func(_ context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryCreating, Attempt: 1, UpdatedAt: time.Now()}, nil
	}
	c = newTestController(t, client, artifacts, testConfig())
	if _, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild)); err != nil {
		t.Fatalf("rebatch after restart: %v", err)
	}
	if len(artifacts.SubmitRepositoryRequests) != 1 || !reflect.DeepEqual(artifacts.SubmitRepositoryRequests[0].Manifests,
		[]ManifestReference{{JobName: "job-b"}}) {
		t.Fatalf("rebatch must contain only the healthy Job: %+v", artifacts.SubmitRepositoryRequests)
	}
}

func TestReconcileDoesNotSkipUnidentifiedManifestFailure(t *testing.T) {
	client := NewFakeClient()
	client.RpmRepos[key(testProject, testBuild)] = inFlightRepo()
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoProcessing)
	artifacts := NewFakeArtifactManager()
	artifacts.GetRepositoryFunc = func(_ context.Context, _, _ string, uid string) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: uid, State: RepositoryFailed, Attempt: 1, UpdatedAt: time.Now(),
			Failure: &FailureInfo{Code: "ManifestInvalid", JobName: "foreign-job"}}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())
	if _, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	updated := client.RpmRepos[key(testProject, testBuild)]
	if len(updated.Status.Repository.SkippedJobNames) != 0 || updated.Status.Release == nil || updated.Status.Release.Phase != ebsv1.RpmRepoReleaseFailed {
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
	artifacts.SubmitRepositoryFunc = func(_ context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryCreating, Attempt: 1, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
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
	if transition == nil || len(transition.Inputs) != 1 || transition.Inputs[0].JobName != "job-a" {
		t.Fatalf("checkpoint does not freeze the batch: %+v", transition)
	}
	if len(artifacts.SubmitRepositoryRequests) != 1 {
		t.Fatalf("expected one SubmitRepository call, got %d", len(artifacts.SubmitRepositoryRequests))
	}
	request := artifacts.SubmitRepositoryRequests[0]
	if request.RepositoryUID != transition.RepositoryUID || request.RepositoryName != testBuild || request.TargetArch != testArch {
		t.Fatalf("unexpected materialization request %+v", request)
	}
	if client.Jobs[testProject][0].Status.Phase != ebsv1.JobSucceeded {
		t.Fatalf("the controller must not modify Job status")
	}
}

func TestReconcileBuildPromotesBatchWithoutSeparateReleaseKey(t *testing.T) {
	client := NewFakeClient()
	repo := newRpmRepo(testBuild)
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}

	artifacts := NewFakeArtifactManager()
	artifacts.SubmitRepositoryFunc = func(_ context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryReady, Attempt: 1, ContentURL: "/repositories/project/builds/build/next/", UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("unexpected result %+v", result)
	}
	updated := client.RpmRepos[key(testProject, testBuild)]
	if updated.Status.Repository.RepositoryUID == "" || updated.Status.Repository.ContentURL != "artifact:///repositories/project/builds/build/next/" {
		t.Fatalf("batch was not promoted: %+v", updated.Status.Repository)
	}
	if updated.Status.Repository.Transition != nil {
		t.Fatalf("promotion must clear the checkpoint")
	}
	if len(updated.Status.Repository.SourceJobNames) != 1 || updated.Status.Repository.SourceJobNames[0] != "job-a" {
		t.Fatalf("unexpected sourceJobNames %+v", updated.Status.Repository.SourceJobNames)
	}
	if !conditionMatches(updated.Status.Conditions, ebsv1.RpmRepoConditionRepositoryReady, metav1.ConditionTrue, ebsv1.RpmRepoReasonRepositoryCreated) {
		t.Fatalf("RepositoryReady=True/RepositoryCreated is missing: %+v", updated.Status.Conditions)
	}
	if c.Queue().Len() != 0 {
		t.Fatalf("release must not enqueue a separate key, queue length %d", c.Queue().Len())
	}
}

func TestReconcileBuildWithoutPublishingMaterializesBeforeSkipping(t *testing.T) {
	for _, phase := range []ebsv1.BuildPhase{ebsv1.BuildProcessing, ebsv1.BuildSuccess, ebsv1.BuildFailed} {
		t.Run(string(phase), func(t *testing.T) {
			client := NewFakeClient()
			client.RpmRepos[key(testProject, testBuild)] = newRpmRepo(testBuild)
			build := newBuild(testBuild)
			build.Spec.Publish = new(bool)
			build.Status.Phase = phase
			build.Status.Stage = ebsv1.BuildStageBuild
			client.Builds[key(testProject, testBuild)] = build
			client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
			client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}

			artifacts := NewFakeArtifactManager()
			artifacts.SubmitRepositoryFunc = func(_ context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
				return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryReady, Attempt: 1, ContentURL: "/repositories/project/builds/build/next/", UpdatedAt: time.Now()}, nil
			}
			c := newTestController(t, client, artifacts, testConfig())
			if _, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild)); err != nil {
				t.Fatalf("sync: %v", err)
			}
			updated := client.RpmRepos[key(testProject, testBuild)]
			if len(artifacts.SubmitRepositoryRequests) != 1 || updated.Status.Repository == nil || updated.Status.Repository.ContentURL != "artifact:///repositories/project/builds/build/next/" {
				t.Fatalf("process repository was not materialized: %+v", updated.Status.Repository)
			}
			if updated.Status.Release == nil || updated.Status.Release.Phase != ebsv1.RpmRepoReleaseSkipped || updated.Status.Release.UpdatedAt == nil || updated.Status.Release.ContentURL != "" {
				t.Fatalf("release was not skipped: %+v", updated.Status.Release)
			}
			if len(artifacts.SubmitReleaseRequests) != 0 || c.Queue().Len() != 0 {
				t.Fatal("a nonpublishing build must not submit or enqueue a release")
			}
			if _, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild)); err != nil {
				t.Fatalf("second sync: %v", err)
			}
			if len(artifacts.SubmitRepositoryRequests) != 1 {
				t.Fatal("a skipped release must not rematerialize the repository")
			}
		})
	}
}

func TestReconcileBuildWithoutPublishingAndWithoutArtifactsSkipsRelease(t *testing.T) {
	client := NewFakeClient()
	client.RpmRepos[key(testProject, testBuild)] = newRpmRepo(testBuild)
	build := newBuild(testBuild)
	build.Spec.Publish = new(bool)
	client.Builds[key(testProject, testBuild)] = build
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())
	if _, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild)); err != nil {
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
		Inputs:            []ebsv1.RepositoryInput{{JobName: "job-a", SpecName: "gcc"}},
		BaseRepositoryUID: "base-1",
		RepositoryUID:     "next-1",
	}
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoProcessing)

	artifacts := NewFakeArtifactManager()
	artifacts.GetRepositoryFunc = func(_ context.Context, _, _ string, repositoryUID string) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: repositoryUID, State: RepositoryFailed, Attempt: 4, Failure: &FailureInfo{Code: "MaterializationFailed", Retryable: true}, UpdatedAt: time.Now()}, nil
	}
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
		Inputs:        []ebsv1.RepositoryInput{{JobName: "job-a", SpecName: "gcc"}},
		RepositoryUID: "next-1",
	}
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoProcessing)

	artifacts := NewFakeArtifactManager()
	updatedAt := time.Date(2026, 9, 21, 9, 59, 0, 0, time.UTC)
	artifacts.GetRepositoryFunc = func(_ context.Context, _, _ string, repositoryUID string) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: repositoryUID, State: RepositoryFailed, Attempt: 1, Failure: &FailureInfo{Retryable: true}, UpdatedAt: updatedAt}, nil
	}
	artifacts.SubmitRepositoryFunc = func(_ context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryCreating, Attempt: 2, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
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
	if request.RepositoryUID != "next-1" || len(request.Manifests) != 1 || request.Manifests[0].JobName != "job-a" {
		t.Fatalf("the replay must reuse the frozen checkpoint: %+v", request)
	}
}

func TestReconcileBuildRegistersNoPublishableArtifacts(t *testing.T) {
	client := NewFakeClient()
	repo := newRpmRepo(testBuild)
	repo.Status.Repository.RepositoryUID = "base-1"
	repo.Status.Repository.ContentURL = "/repositories/project/builds/build/base-1/"
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)

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
		t.Fatalf("release terminal missing: %+v", updated.Status.Release)
	}
	if !conditionMatches(updated.Status.Conditions, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionFalse, ebsv1.RpmRepoReasonNoPublishableArtifacts) {
		t.Fatalf("PublishSucceed=False/NoPublishableArtifacts missing: %+v", updated.Status.Conditions)
	}
	if updated.Status.Repository.RepositoryUID != "base-1" || updated.Status.Repository.ContentURL != "/repositories/project/builds/build/base-1/" {
		t.Fatalf("the inherited baseline must stay readable: %+v", updated.Status.Repository)
	}
}

func TestReconcileBuildWaitsWhenBuildInfoIsNotCompleted(t *testing.T) {
	client := NewFakeClient()
	repo := newRpmRepo(testBuild)
	repo.Status.Repository.SourceJobNames = []string{"job-a"}
	client.RpmRepos[key(testProject, testBuild)] = repo
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoProcessing)

	artifacts := NewFakeArtifactManager()
	c := newTestController(t, client, artifacts, testConfig())

	result, err := c.sync(context.Background(), rpmRepoKey(testProject, testBuild))
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
