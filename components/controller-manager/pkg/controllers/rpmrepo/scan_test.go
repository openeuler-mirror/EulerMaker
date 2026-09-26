package rpmrepo

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

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
			name:   "missing-target-os",
			mutate: func(job *ebsv1.Job) { delete(job.Labels, ebsv1.BuildTargetOSLabel) },
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

			result, err := c.sync(context.Background(), buildKey(testProject, testBuild))
			if err != nil {
				t.Fatalf("sync: %v", err)
			}
			if result != (controller.ReconcileResult{}) {
				t.Fatalf("a job that is not a candidate must not start a batch: %+v", result)
			}
			if len(artifacts.ManifestRequests) != 0 {
				t.Fatalf("a rejected candidate must not be queried for a manifest: %+v", artifacts.ManifestRequests)
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

	if _, err := c.sync(context.Background(), buildKey(testProject, testBuild)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(artifacts.ManifestRequests) != 0 || len(artifacts.SubmitRepositoryRequests) != 0 {
		t.Fatalf("a consumed Job must not be read or submitted again")
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
			if len(artifacts.ManifestRequests) != 0 {
				t.Fatalf("selection must not query manifests: %v", artifacts.ManifestRequests)
			}
			selection := selectBatch(scan.candidates, config.MaxJobsPerBatch)
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

func TestScanDoesNotReadManifestBeforeSubmission(t *testing.T) {
	cases := []struct {
		name        string
		manifest    JobUploadManifest
		manifestErr error
	}{
		{
			name:        "missing",
			manifestErr: &artifactError{operation: "get-manifest", kind: artifactNotFound, code: "NotFound"},
		},
		{
			name:     "not-ready",
			manifest: JobUploadManifest{State: ManifestOpen},
		},
		{
			name:     "completing",
			manifest: JobUploadManifest{State: ManifestCompleting},
		},
		{
			name:     "failed",
			manifest: JobUploadManifest{State: ManifestFailed},
		},
		{
			name:     "completed",
			manifest: completedManifest(100),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := NewFakeClient()
			job := newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))
			artifacts := NewFakeArtifactManager()
			artifacts.GetJobManifestFunc = func(context.Context, string, string, string) (JobUploadManifest, error) {
				return tc.manifest, tc.manifestErr
			}
			artifacts.SubmitRepositoryFunc = func(_ context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
				return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryCreating, Attempt: 1, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
			}
			c := scanFixture(t, job, artifacts, client)

			if _, err := c.sync(context.Background(), buildKey(testProject, testBuild)); err != nil {
				t.Fatalf("sync: %v", err)
			}
			if len(artifacts.ManifestRequests) != 0 {
				t.Fatalf("manifest must not be read before submission: %v", artifacts.ManifestRequests)
			}
			if len(artifacts.SubmitRepositoryRequests) != 1 || len(client.StatusWrites) == 0 {
				t.Fatalf("succeeded Job must be submitted regardless of manifest: submits=%d writes=%d", len(artifacts.SubmitRepositoryRequests), len(client.StatusWrites))
			}
		})
	}
}

func TestCandidateScanUsesTheBuildNameSelector(t *testing.T) {
	client := NewFakeClient()
	job := newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))
	artifacts := NewFakeArtifactManager()
	artifacts.GetJobManifestFunc = func(context.Context, string, string, string) (JobUploadManifest, error) {
		return completedManifest(100), nil
	}
	artifacts.SubmitRepositoryFunc = func(_ context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryCreating, Attempt: 1, PollAfterSeconds: 5, UpdatedAt: time.Now()}, nil
	}
	c := scanFixture(t, job, artifacts, client)
	if _, err := c.sync(context.Background(), buildKey(testProject, testBuild)); err != nil {
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
	artifacts.GetJobManifestFunc = func(context.Context, string, string, string) (JobUploadManifest, error) {
		return completedManifest(100), nil
	}
	artifacts.SubmitRepositoryFunc = func(_ context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
		return RepositoryResponse{RepositoryUID: req.RepositoryUID, State: RepositoryReady, Attempt: 1, ContentURL: "/repositories/v1/next/", UpdatedAt: time.Now()}, nil
	}
	c := scanFixture(t, job, artifacts, client)

	result, err := c.sync(context.Background(), buildKey(testProject, testBuild))
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

			result, err := c.sync(context.Background(), buildKey(testProject, testBuild))
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

func TestDependencyReadFailuresKeepTheirClass(t *testing.T) {
	cases := []struct {
		name          string
		buildErr      error
		buildInfoErr  error
		wantPermanent bool
		wantErr       bool
	}{
		{
			name:     "build-5xx",
			buildErr: errors.New("internal error"),
			wantErr:  true,
		},
		{
			name:          "build-forbidden",
			buildErr:      apierrors.NewForbidden(schema.GroupResource{Group: "ebs", Resource: "builds"}, testBuild, errors.New("denied")),
			wantPermanent: true,
			wantErr:       true,
		},
		{
			name:          "buildinfo-forbidden",
			buildInfoErr:  apierrors.NewForbidden(schema.GroupResource{Group: "ebs", Resource: "buildinfos"}, testBuild, errors.New("denied")),
			wantPermanent: true,
			wantErr:       true,
		},
		{
			name:         "buildinfo-5xx",
			buildInfoErr: errors.New("boom"),
			wantErr:      true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := NewFakeClient()
			client.RpmRepos[key(testProject, testBuild)] = newRpmRepo(testBuild)
			client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
			client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
			client.GetBuildErr = tc.buildErr
			client.GetBuildInfoErr = tc.buildInfoErr
			artifacts := NewFakeArtifactManager()
			c := newTestController(t, client, artifacts, testConfig())

			_, err := c.sync(context.Background(), buildKey(testProject, testBuild))
			if tc.wantErr && err == nil {
				t.Fatalf("expected an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if got := controller.IsPermanent(err); got != tc.wantPermanent {
				t.Fatalf("permanent = %t, want %t (err %v)", got, tc.wantPermanent, err)
			}
		})
	}
}
