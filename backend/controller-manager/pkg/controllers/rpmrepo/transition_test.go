package rpmrepo

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

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
		return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryReady, Attempt: 1, ContentURL: "/repositories/v1/next/", UpdatedAt: time.Now()}, nil
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
			artifacts.GetRepositoryFunc = func(_ context.Context, repositoryUID string) (RepositoryResponse, error) {
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
		return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryReady, Attempt: 1, ContentURL: "/repositories/v1/next/", UpdatedAt: time.Now()}, nil
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
	artifacts.GetRepositoryFunc = func(context.Context, string) (RepositoryResponse, error) {
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
	artifacts.GetRepositoryFunc = func(_ context.Context, repositoryUID string) (RepositoryResponse, error) {
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
	artifacts.GetRepositoryFunc = func(context.Context, string) (RepositoryResponse, error) {
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
