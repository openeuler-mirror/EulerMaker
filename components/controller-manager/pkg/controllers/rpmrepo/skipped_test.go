package rpmrepo

import (
	"context"
	"testing"
	"time"

	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
)

// skippedPhaseFieldSelector is the literal the design document requires: the polling source and the release
// candidate list both exclude Ready, Failed and Skipped.
const skippedPhaseFieldSelector = "status.release.phase!=Ready,status.release.phase!=Failed,status.release.phase!=Skipped"

func TestFieldSelectorExcludesSkipped(t *testing.T) {
	if nonTerminalRpmRepoFieldSelector != skippedPhaseFieldSelector {
		t.Fatalf("field selector = %q, want %q", nonTerminalRpmRepoFieldSelector, skippedPhaseFieldSelector)
	}
}

func TestReleaseTerminalIncludesSkipped(t *testing.T) {
	cases := []struct {
		name string
		repo *ebsv1.RpmRepo
		want bool
	}{
		{name: "no-release", repo: newRpmRepo(testBuild), want: false},
		{
			name: "ready",
			repo: func() *ebsv1.RpmRepo {
				repo := newRpmRepo(testBuild)
				repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseReady}
				return repo
			}(),
			want: true,
		},
		{
			name: "failed",
			repo: func() *ebsv1.RpmRepo {
				repo := newRpmRepo(testBuild)
				repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseFailed}
				return repo
			}(),
			want: true,
		},
		{
			name: "skipped",
			repo: func() *ebsv1.RpmRepo {
				repo := newRpmRepo(testBuild)
				repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseSkipped}
				return repo
			}(),
			want: true,
		},
		{
			name: "pending",
			repo: func() *ebsv1.RpmRepo {
				repo := newRpmRepo(testBuild)
				repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleasePending}
				return repo
			}(),
			want: false,
		},
		{
			name: "creating",
			repo: func() *ebsv1.RpmRepo {
				repo := newRpmRepo(testBuild)
				repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseCreating}
				return repo
			}(),
			want: false,
		},
		{
			name: "prepared",
			repo: func() *ebsv1.RpmRepo {
				repo := newRpmRepo(testBuild)
				repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleasePrepared}
				return repo
			}(),
			want: false,
		},
		{name: "nil-object", repo: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := releaseTerminal(tc.repo); got != tc.want {
				t.Fatalf("releaseTerminal = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestSkippedObjectsAreNotEnqueued(t *testing.T) {
	c := handlerController(t)
	skipped := newRpmRepo(testBuild)
	skipped.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseSkipped}
	c.onAdd(skipped)
	c.onUpdate(skipped, skipped)
	if c.Queue().Len() != 0 {
		t.Fatalf("a skipped RpmRepo must not be enqueued")
	}
}

// skippedWithBaseline builds the shape the Build Controller creates for a single build: release.phase=Skipped
// plus the inherited repository pointers (or no repository at all when there is no baseline).
func skippedWithBaseline(withBaseline bool) *ebsv1.RpmRepo {
	repo := newRpmRepo(testBuild)
	repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseSkipped}
	if withBaseline {
		repo.Status.Repository = &ebsv1.RpmRepoRepositoryStatus{
			RepositoryUID: "base-1",
			ContentURL:    "/repositories/v1/base-1/",
		}
	} else {
		repo.Status.Repository = nil
	}
	return repo
}

func TestReconcileBuildConvergesOnSkipped(t *testing.T) {
	cases := []struct {
		name         string
		withBaseline bool
		buildPhase   ebsv1.BuildPhase
	}{
		{name: "with-baseline", withBaseline: true, buildPhase: ebsv1.BuildProcessing},
		{name: "without-baseline", withBaseline: false, buildPhase: ebsv1.BuildProcessing},
		{name: "aborted-build", withBaseline: true, buildPhase: ebsv1.BuildAborted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := NewFakeClient()
			repo := skippedWithBaseline(tc.withBaseline)
			client.RpmRepos[key(testProject, testBuild)] = repo
			build := newBuild(testBuild)
			build.Status.Phase = tc.buildPhase
			client.Builds[key(testProject, testBuild)] = build
			client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
			client.Jobs[testProject] = []ebsv1.Job{newSucceededJob("job-a", "gcc", "uid-job-a", time.Unix(1, 0))}
			artifacts := NewFakeArtifactManager()
			c := newTestController(t, client, artifacts, testConfig())

			readyBefore := repositoryReady.Value()
			repositoryFailedBefore := repositoryFailed.Value()
			releaseFailedBefore := releaseFailed.Value()
			buildMissingBefore := buildMissing.Value()

			for round := 0; round < 2; round++ {
				result, err := c.sync(context.Background(), buildKey(testProject, testBuild))
				if err != nil {
					t.Fatalf("round %d: %v", round, err)
				}
				if result != (controller.ReconcileResult{}) {
					t.Fatalf("round %d: a skipped object must converge, got %+v", round, result)
				}
			}
			if len(client.StatusWrites) != 0 {
				last := client.StatusWrites[len(client.StatusWrites)-1]
				t.Fatalf("a skipped object must not be written, got %d writes (last phase %+v)",
					len(client.StatusWrites), last.Status.Release)
			}
			if client.GetBuildCalls != 0 || client.GetBuildInfoCalls != 0 || len(client.JobListOptions) != 0 {
				t.Fatalf("a skipped object must not read Build/BuildInfo/Job: build=%d buildinfo=%d jobs=%d",
					client.GetBuildCalls, client.GetBuildInfoCalls, len(client.JobListOptions))
			}
			if client.GetRpmRepoCalls < 2 {
				t.Fatalf("the round must start by reading the RpmRepo, got %d reads", client.GetRpmRepoCalls)
			}
			if len(artifacts.SubmitRepositoryRequests) != 0 || len(artifacts.GetRepositoryUIDs) != 0 ||
				len(artifacts.SubmitReleaseRequests) != 0 || len(artifacts.ActivateReleaseNames) != 0 ||
				len(artifacts.ManifestRequests) != 0 {
				t.Fatalf("a skipped object must not reach Artifact Manager")
			}
			if c.Queue().Len() != 0 {
				t.Fatalf("a skipped object must not enqueue the release key")
			}
			if repositoryReady.Value() != readyBefore || repositoryFailed.Value() != repositoryFailedBefore ||
				releaseFailed.Value() != releaseFailedBefore || buildMissing.Value() != buildMissingBefore {
				t.Fatalf("a skipped object must not move metrics")
			}
			stored := client.RpmRepos[key(testProject, testBuild)]
			if stored.Status.Release == nil || stored.Status.Release.Phase != ebsv1.RpmRepoReleaseSkipped {
				t.Fatalf("the skipped terminal must survive: %+v", stored.Status.Release)
			}
			if len(stored.Status.Conditions) != 0 {
				t.Fatalf("a skipped object must not gain conditions: %+v", stored.Status.Conditions)
			}
			if tc.withBaseline {
				if stored.Status.Repository.RepositoryUID != "base-1" || stored.Status.Repository.ContentURL != "/repositories/v1/base-1/" {
					t.Fatalf("the inherited baseline must stay untouched: %+v", stored.Status.Repository)
				}
			} else if stored.Status.Repository != nil {
				t.Fatalf("an object without baseline must keep repository nil: %+v", stored.Status.Repository)
			}
		})
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

	result, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch))
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
	if len(artifacts.SubmitReleaseRequests) != 0 || len(artifacts.GetReleaseBuildNames) != 0 || len(artifacts.ActivateReleaseNames) != 0 {
		t.Fatalf("a skipped object must not reach Artifact Manager")
	}
	if stored := client.RpmRepos[key(testProject, testBuild)]; stored.Status.Release == nil || stored.Status.Release.Phase != ebsv1.RpmRepoReleaseSkipped {
		t.Fatalf("the skipped terminal must survive: %+v", stored.Status.Release)
	}
}

func TestInitializerSelectorExcludesSkipped(t *testing.T) {
	factory := &stubPollingFactory{}
	config := initializedConfig()
	if _, _, err := Initializer(config)(context.Background(), initContext(factory)); err != nil {
		t.Fatalf("Initializer: %v", err)
	}
	if factory.options.FieldSelector != skippedPhaseFieldSelector {
		t.Fatalf("polling field selector = %q, want %q", factory.options.FieldSelector, skippedPhaseFieldSelector)
	}
}

func TestReleaseCandidateQueryExcludesSkipped(t *testing.T) {
	client, _, c := releaseCandidateFixture(t, nil, DefaultPublishPolicy{})
	if _, err := c.sync(context.Background(), releaseKey(testProject, testOS, testArch)); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(client.RpmRepoListOptions) == 0 {
		t.Fatalf("expected a release candidate list call")
	}
	options := client.RpmRepoListOptions[0]
	if options.FieldSelector != skippedPhaseFieldSelector {
		t.Fatalf("release candidate field selector = %q, want %q", options.FieldSelector, skippedPhaseFieldSelector)
	}
	if options.LabelSelector == "" {
		t.Fatalf("the release candidate query must keep its target label selector")
	}
}
