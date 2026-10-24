package buildinfo

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"controller-manager/pkg/controllers/buildinfo/rpmver"
	"controller-manager/pkg/controllers/buildinfo/specparse"
	ebsv1 "ebs-api/ebs/v1"
)

// seedProcessingRound seeds the objects every Processing advance round needs:
// active project, processing build, build resource, build conf, the given BuildInfo, a one-repo snapshot and a held
// RpmRepo. depends feeds the Processing assembly cache (no git) and sources the metadata cache (URL match, no
// download). Returns the stored BuildInfo (server-assigned UID).
func seedProcessingRound(
	client *fakeClient,
	c *Controller,
	bi *ebsv1.BuildInfo,
	depends map[string]specparse.SpecDepend,
	sources *rpmver.RpmMetaSources,
) *ebsv1.BuildInfo {
	key := testNS + "/" + testBuild
	client.SeedProject(testProjectObj(ebsv1.ProjectActive))
	client.SeedBuild(testBuildObj("full"))
	client.SetBuildResourceContent(testBuildResourceContent())
	client.SetBuildTargetContent(testBuildTargetContent())
	seeded := client.SeedBuildInfo(bi)
	client.SeedSnapshot(testSnapshotObj(repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	client.SeedRpmRepo(testRpmRepoObj(testRepoURL))
	c.specDependsCache.Set(key, depends)
	c.rpmMetaSources.Set(key, sources)
	return seeded
}

// dependEntry renders one cached assembly view entry for repo1.
func dependEntry(spec string) specparse.SpecDepend {
	return specparse.SpecDepend{RepoName: "repo1", SpecName: spec, SpecFileName: spec + ".spec", Version: "1.0"}
}

// seedJobAt seeds an identity-complete Job with an explicit creation time and returns the stored copy (server-assigned
// UID).
func seedJobAt(
	client *fakeClient,
	bi *ebsv1.BuildInfo,
	spec string,
	generation int64,
	phase ebsv1.JobPhase,
	at time.Time,
) *ebsv1.Job {
	job := testJobObj(bi, spec, generation, phase)
	job.CreationTimestamp = metav1.NewTime(at)
	return client.SeedJob(job)
}

// Job result backfill and phase mapping.

func TestAdvanceBackfillMultiGeneration(t *testing.T) {
	c, client, _, _ := newTestController(t)
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	client.SeedBuildGraph(bi, dcgStateAB())
	bi.Status.SpecResults = testSpecResults(map[string]ebsv1.SpecResult{
		"a": {Status: ebsv1.SpecRunning, DispatchCount: 1},
		"b": {},
	})
	seeded := seedProcessingRound(client, c, bi, map[string]specparse.SpecDepend{
		"a": dependEntry("a"), "b": dependEntry("b"),
	}, testSources())
	// The latest generation Failed, so the rebuild fails and the count rises to 2.
	seedJobAt(client, seeded, "a", 1, ebsv1.JobSucceeded, testStart)
	seedJobAt(client, seeded, "a", 2, ebsv1.JobFailed, testStart.Add(time.Minute))
	// Out-of-build-set Jobs are ignored by the scoped grouping.
	seedJobAt(client, seeded, "ghost", 1, ebsv1.JobSucceeded, testStart.Add(2*time.Minute))

	reconcileOnce(t, c)

	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoProcessing)
	a := persisted.Status.SpecResults.Entry("a")
	if a.Status != ebsv1.SpecBuildFailed || a.DispatchCount != 2 {
		t.Fatalf("specResults[a] = %+v, want Failed with floor count 2", a)
	}
	requireCondition(t, a.Conditions, ConditionRebuildFailed, ReasonRebuildJobFailed)
	if want := jobNameFor(string(seeded.UID), "a", 2); !strings.Contains(a.Conditions[0].Message, want) {
		t.Fatalf("specResults[a] condition = %+v, want generation-2 job %q", a.Conditions, want)
	}
	if _, ok := persisted.Status.SpecResults.Lookup("ghost"); ok {
		t.Fatalf("out-of-scope job leaked into specResults: %v", persisted.Status.SpecResults)
	}
	// A failed upstream allows b to dispatch without waiting for its output.
	b := persisted.Status.SpecResults.Entry("b")
	if b.DispatchCount != 1 {
		t.Fatalf("specResults[b] = %+v, want dispatched (gen 1)", b)
	}
}

func TestAdvanceBackfillUnknownPhaseSkipped(t *testing.T) {
	c, client, _, _ := newTestController(t)
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	client.SeedBuildGraph(bi, map[string]ebsv1.BuildGraphNode{"a": {Version: "1.0-1"}})
	bi.Status.SpecResults = testSpecResults(map[string]ebsv1.SpecResult{
		"a": {Status: ebsv1.SpecRunning, DispatchCount: 1},
	})
	seeded := seedProcessingRound(client, c, bi, map[string]specparse.SpecDepend{"a": dependEntry("a")}, testSources())
	seedJobAt(client, seeded, "a", 1, ebsv1.JobPhase("Phasing"), testStart)

	reconcileOnce(t, c)

	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoProcessing)
	a := persisted.Status.SpecResults.Entry("a")
	if a.Status != ebsv1.SpecRunning {
		t.Fatalf("specResults[a].Status = %q, want Running (unknown phase unmapped)", a.Status)
	}
	requireNoCondition(t, a.Conditions, ConditionBuildFailed)
}

func TestAdvanceBackfillJobAborted(t *testing.T) {
	c, client, _, _ := newTestController(t)
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	client.SeedBuildGraph(bi, map[string]ebsv1.BuildGraphNode{"a": {Version: "1.0-1"}})
	bi.Status.SpecResults = testSpecResults(map[string]ebsv1.SpecResult{
		"a": {Status: ebsv1.SpecRunning, DispatchCount: 1},
	})
	seeded := seedProcessingRound(client, c, bi, map[string]specparse.SpecDepend{"a": dependEntry("a")}, testSources())
	seedJobAt(client, seeded, "a", 1, ebsv1.JobAborted, testStart)

	reconcileOnce(t, c)

	persisted := getBuildInfo(t, client)
	a := persisted.Status.SpecResults.Entry("a")
	if a.Status != ebsv1.SpecBuildFailed {
		t.Fatalf("specResults[a].Status = %q, want Failed (Aborted treated as Failed)", a.Status)
	}
	requireCondition(t, a.Conditions, ConditionBuildAborted, ReasonBuildAborted)
	requirePhase(t, persisted, ebsv1.BuildInfoCompleted)
}

func TestLatestJobOrdering(t *testing.T) {
	at := metav1.NewTime(testStart)
	group := []ebsv1.Job{
		{ObjectMeta: metav1.ObjectMeta{Name: "job-b", CreationTimestamp: at}},
		{ObjectMeta: metav1.ObjectMeta{Name: "job-a", CreationTimestamp: at}},
		{ObjectMeta: metav1.ObjectMeta{Name: "job-z"}}, // zero timestamp sorts earliest
	}
	if got := latestJob(group); got.Name != "job-b" {
		t.Fatalf("latestJob = %q, want job-b (greatest creationTimestamp, then name)", got.Name)
	}
}

// Install result backfill and runtime edges.

func TestAdvanceInstallBackfillBranches(t *testing.T) {
	t.Run("structured install result marks succeeded", func(t *testing.T) {
		c, client, _, _ := newTestController(t)
		bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
		client.SeedBuildGraph(bi, map[string]ebsv1.BuildGraphNode{"a": {Version: "1.0-1"}})
		bi.Status.SpecResults = testSpecResults(map[string]ebsv1.SpecResult{
			"a": {Status: ebsv1.SpecRunning, DispatchCount: 1},
		})
		seeded := seedProcessingRound(
			client,
			c,
			bi,
			map[string]specparse.SpecDepend{"a": dependEntry("a")},
			testSources(),
		)
		seedJobAt(client, seeded, "a", 1, ebsv1.JobSucceeded, testStart)

		reconcileOnce(t, c)

		persisted := getBuildInfo(t, client)
		if got := persisted.Status.SpecResults.Entry("a").Status; got != ebsv1.SpecSucceeded {
			t.Fatalf("install status = %q, want Succeeded", got)
		}
		requirePhase(t, persisted, ebsv1.BuildInfoCompleted)
	})

	t.Run("missing install result is not success", func(t *testing.T) {
		c, client, _, _ := newTestController(t)
		bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
		client.SeedBuildGraph(bi, map[string]ebsv1.BuildGraphNode{"a": {Version: "1.0-1"}})
		bi.Status.SpecResults = testSpecResults(map[string]ebsv1.SpecResult{
			"a": {Status: ebsv1.SpecRunning, DispatchCount: 1},
		})
		seeded := seedProcessingRound(
			client,
			c,
			bi,
			map[string]specparse.SpecDepend{"a": dependEntry("a")},
			testSources(),
		)
		job := testJobObj(seeded, "a", 1, ebsv1.JobSucceeded)
		job.CreationTimestamp = metav1.NewTime(testStart)
		job.Status.Message = "build ok"
		job.Status.Install = nil
		client.SeedJob(job)

		reconcileOnce(t, c)

		persisted := getBuildInfo(t, client)
		a := persisted.Status.SpecResults.Entry("a")
		if a.Status != ebsv1.SpecInstallFailed {
			t.Fatalf("install status = %q, want Failed", a.Status)
		}
		requireCondition(t, a.Conditions, ConditionInstall, ReasonInstallResultMissing)
		requirePhase(t, persisted, ebsv1.BuildInfoCompleted)
	})

	t.Run("missing deps fail install and append runtime edges", func(t *testing.T) {
		c, client, _, _ := newTestController(t)
		bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
		client.SeedBuildGraph(bi, map[string]ebsv1.BuildGraphNode{
			"a": {Version: "1.0-1"},
			"b": {Version: "1.0-1"},
		})
		bi.Status.SpecResults = testSpecResults(map[string]ebsv1.SpecResult{
			"a": {},
			"b": {
				Status:        ebsv1.SpecSucceeded,
				DispatchCount: 1,
				MissingDeps: map[string]ebsv1.MissingDep{
					"a": {NeededBy: "old", VersionRequests: ebsv1.VersionConstraint{GE: "1.0"}},
				},
			},
		})
		seeded := seedProcessingRound(client, c, bi, map[string]specparse.SpecDepend{
			"a": dependEntry("a"), "b": dependEntry("b"),
		}, testSources(testRpm("a", "a", "1.0")))
		job := testJobObj(seeded, "b", 1, ebsv1.JobSucceeded)
		job.CreationTimestamp = metav1.NewTime(testStart)
		job.Status.Install = &ebsv1.JobInstallResult{
			Status: ebsv1.JobResultFailed,
			MissingDeps: map[string]ebsv1.MissingDep{
				"a": {NeededBy: "new", VersionRequests: ebsv1.VersionConstraint{GE: "1.0"}},
			},
		}
		client.SeedJob(job)

		reconcileOnce(t, c)

		persisted := getBuildInfo(t, client)
		requirePhase(t, persisted, ebsv1.BuildInfoProcessing)
		b := persisted.Status.SpecResults.Entry("b")
		if b.Status != ebsv1.SpecInstallFailed {
			t.Fatalf("install status = %q, want Failed", b.Status)
		}
		if got := b.MissingDeps["a"]; got.NeededBy != "new" || got.VersionRequests.GE != "1.0" {
			t.Fatalf("missingDeps[a] = %+v, want the latest Job result", got)
		}
		requireCondition(t, b.Conditions, ConditionInstall, ReasonInstallCheckFailed)
		// Provider a is non-terminal; persist the new edge before dispatch.
		if _, ok := testGraph(t, client, persisted).Status.Nodes["b"].InstallInDep["a"]; !ok {
			t.Fatalf("dcg[b].InstallInDep = %v, want runtime edge on a", testGraph(t, client, persisted).Status.Nodes["b"].InstallInDep)
		}
		a := persisted.Status.SpecResults.Entry("a")
		if a.DispatchCount != 1 {
			t.Fatalf("specResults[a] = %+v, want dispatched (gen 1)", a)
		}
	})
}

// Dispatch gates.

func TestAdvanceGatePublishConfirmation(t *testing.T) {
	c, client, _, _ := newTestController(t)
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	client.SeedBuildGraph(bi, dcgStateAB())
	bi.Status.SpecResults = testSpecResults(map[string]ebsv1.SpecResult{
		"a": {Status: ebsv1.SpecRunning, DispatchCount: 1},
		"b": {},
	})
	seeded := seedProcessingRound(client, c, bi, map[string]specparse.SpecDepend{
		"a": dependEntry("a"), "b": dependEntry("b"),
	}, testSources())
	a1 := seedJobAt(client, seeded, "a", 1, ebsv1.JobSucceeded, testStart)

	// b waits until the successful upstream Job appears in sourceJobNames.
	reconcileOnce(t, c)
	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoProcessing)
	if got := persisted.Status.SpecResults.Entry("a").Status; got != ebsv1.SpecSucceeded {
		t.Fatalf("specResults[a].Status = %q, want Succeeded", got)
	}
	if got := persisted.Status.SpecResults.Entry("b").DispatchCount; got != 0 {
		t.Fatalf("specResults[b].DispatchCount = %d, want 0 (publish gate)", got)
	}
	if len(persisted.Status.SpecResults.Entry("b").Conditions) != 0 {
		t.Fatalf("specResults[b] conditions = %v, want none", persisted.Status.SpecResults.Entry("b").Conditions)
	}
	if got := len(listJobs(t, client)); got != 1 {
		t.Fatalf("jobs = %d, want 1", got)
	}

	// Round 2: the upstream Job is published — b dispatches.
	repo := testRpmRepoObj(testRepoURL)
	repo.Status.Repository.SourceJobNames = []string{a1.Name}
	client.SeedRpmRepo(repo)
	reconcileOnce(t, c)

	persisted = getBuildInfo(t, client)
	b := persisted.Status.SpecResults.Entry("b")
	if b.DispatchCount != 1 {
		t.Fatalf("specResults[b] = %+v, want dispatched after publish", b)
	}
}

func TestAdvanceGateRebuildConsistencyCycle(t *testing.T) {
	c, client, _, _ := newTestController(t)
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	// Cycle a<->b with a as the break point: both require 2 dispatches.
	client.SeedBuildGraph(bi, map[string]ebsv1.BuildGraphNode{
		"a": {
			Version:        "1.0-1",
			OutDep:         []string{"b"},
			InDep:          map[string]ebsv1.VersionConstraint{"b": {}},
			BootstrapBreak: true,
		},
		"b": {Version: "1.0-1", OutDep: []string{"a"}, InDep: map[string]ebsv1.VersionConstraint{"a": {}}},
	})
	bi.Status.SpecResults = testSpecResults(map[string]ebsv1.SpecResult{
		"a": {Status: ebsv1.SpecSucceeded, DispatchCount: 1},
		"b": {Status: ebsv1.SpecSucceeded, DispatchCount: 1},
	})
	seeded := seedProcessingRound(client, c, bi, map[string]specparse.SpecDepend{
		"a": dependEntry("a"), "b": dependEntry("b"),
	}, testSources())
	a1 := seedJobAt(client, seeded, "a", 1, ebsv1.JobSucceeded, testStart)
	b1 := seedJobAt(client, seeded, "b", 1, ebsv1.JobSucceeded, testStart.Add(time.Minute))
	publish := func(uids ...string) {
		repo := testRpmRepoObj(testRepoURL)
		repo.Status.Repository.SourceJobNames = uids
		client.SeedRpmRepo(repo)
	}
	publish(a1.Name)

	// Round 1: a's second dispatch is publish-gated on b; b's second dispatch is rebuild-consistency-gated on a (upstream
	// below its effective required).
	reconcileOnce(t, c)
	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoProcessing)
	if got := persisted.Status.SpecResults.Entry("a").DispatchCount; got != 1 {
		t.Fatalf("round1 specResults[a].DispatchCount = %d, want 1 (gate 2 blocks the rebuild)", got)
	}
	if got := persisted.Status.SpecResults.Entry("b").DispatchCount; got != 1 {
		t.Fatalf("round1 specResults[b].DispatchCount = %d, want 1 (gate 1 blocks the rebuild)", got)
	}
	if got := len(listJobs(t, client)); got != 2 {
		t.Fatalf("round1 jobs = %d, want 2", got)
	}

	// Round 2: b published — a rebuilds (the break point skips gate 1). b then passes gate 1 (a Succeeded at count 2) and
	// gate 2 (the round-start List still holds a's published generation-1 Job; the generation-2 Job was created after the
	// List) — the cycle completes at the required counts.
	publish(a1.Name, b1.Name)
	reconcileOnce(t, c)
	persisted = getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoCompleted)
	if got := persisted.Status.SpecResults.Entry("a").DispatchCount; got != 2 {
		t.Fatalf("round2 specResults[a].DispatchCount = %d, want 2", got)
	}
	if got := persisted.Status.SpecResults.Entry("b").DispatchCount; got != 2 {
		t.Fatalf("round2 specResults[b].DispatchCount = %d, want 2", got)
	}
	if got := len(listJobs(t, client)); got != 4 {
		t.Fatalf("round2 jobs = %d, want 4 (two generations each)", got)
	}
}

// Completion checks.

func TestAdvanceCompletionRecordsFailedPackage(t *testing.T) {
	c, client, _, _ := newTestController(t)
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	client.SeedBuildGraph(bi, map[string]ebsv1.BuildGraphNode{
		"a": {Version: "1.0-1"},
		"b": {Version: "1.0-1"},
	})
	bi.Status.SpecResults = testSpecResults(map[string]ebsv1.SpecResult{
		"a": {Status: ebsv1.SpecRunning, DispatchCount: 1},
		"b": {Status: ebsv1.SpecRunning, DispatchCount: 1},
	})
	seeded := seedProcessingRound(client, c, bi, map[string]specparse.SpecDepend{
		"a": dependEntry("a"), "b": dependEntry("b"),
	}, testSources())
	seedJobAt(client, seeded, "a", 1, ebsv1.JobSucceeded, testStart)
	seedJobAt(client, seeded, "b", 1, ebsv1.JobFailed, testStart)

	reconcileOnce(t, c)

	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoCompleted)
	requireCondition(t, persisted.Status.SpecResults.Entry("b").Conditions, ConditionBuildFailed, ReasonJobFailed)
	if got := persisted.Status.SpecResults.FailedPackages(); len(got) != 1 || got[0] != "repo1" {
		t.Fatalf("failedPackages = %v, want [repo1]", got)
	}
}

func TestAdvanceCompletionRecordsInstallFailureRepository(t *testing.T) {
	c, client, _, _ := newTestController(t)
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	client.SeedBuildGraph(bi, map[string]ebsv1.BuildGraphNode{"a": {Version: "1.0-1"}})
	bi.Status.SpecResults = testSpecResults(map[string]ebsv1.SpecResult{
		"a": {
			Status: ebsv1.SpecInstallFailed,

			DispatchCount: 1,
		},
	})
	seedProcessingRound(client, c, bi, map[string]specparse.SpecDepend{"a": dependEntry("a")}, testSources())

	reconcileOnce(t, c)

	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoCompleted)
	if got := persisted.Status.SpecResults.FailedPackages(); len(got) != 1 || got[0] != "repo1" {
		t.Fatalf("failedPackages = %v, want [repo1]", got)
	}
}

// Stop-dispatch convergence.

func TestConvergeBackfillsCompletedJob(t *testing.T) {
	c, client, _, _ := newTestController(t)
	client.SeedProject(testProjectObj(ebsv1.ProjectActive))
	client.SeedBuild(testBuildObj("full"))
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	bi.UID = "bi-converge-confirm"
	upsertCondition(&bi.Status.Conditions, ConditionRpmRepoUnavailable, ReasonRpmRepoNotFound, "rpmrepo gone")
	bi.Status.SpecResults = testSpecResults(map[string]ebsv1.SpecResult{
		"a": {Status: ebsv1.SpecRunning, DispatchCount: 1},
	})
	seeded := client.SeedBuildInfo(bi)
	seedJobAt(client, seeded, "a", 1, ebsv1.JobSucceeded, testStart)

	reconcileOnce(t, c)

	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoCompleted)
	if got := persisted.Status.SpecResults.Entry("a").Status; got != ebsv1.SpecSucceeded {
		t.Fatalf("specResults[a].Status = %q, want Succeeded", got)
	}
	requireCondition(t, persisted.Status.Conditions, ConditionRpmRepoUnavailable, ReasonRpmRepoNotFound)
}

func TestConvergeUnknownPhaseJobWaits(t *testing.T) {
	c, client, _, _ := newTestController(t)
	client.SeedProject(testProjectObj(ebsv1.ProjectActive))
	client.SeedBuild(testBuildObj("full"))
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	upsertCondition(&bi.Status.Conditions, ConditionReleaseUnavailable, ReasonRpmRepoReleaseFailed, "release failed")
	bi.Status.SpecResults = testSpecResults(map[string]ebsv1.SpecResult{
		"a": {Status: ebsv1.SpecRunning, DispatchCount: 1},
	})
	seeded := client.SeedBuildInfo(bi)
	seedJobAt(client, seeded, "a", 1, ebsv1.JobPhase("Phasing"), testStart)

	reconcileOnce(t, c)

	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoProcessing)
	requireCondition(t, persisted.Status.Conditions, ConditionReleaseUnavailable, ReasonRpmRepoReleaseFailed)
	if got := persisted.Status.SpecResults.Entry("a").Status; got != ebsv1.SpecRunning {
		t.Fatalf("specResults[a].Status = %q, want Running (unknown phase unmapped)", got)
	}
}

// Escalation after repeated child-resource failures.

func TestReleaseUnavailableStopsDispatch(t *testing.T) {
	c, client, _, _ := newTestController(t)
	client.SeedProject(testProjectObj(ebsv1.ProjectActive))
	client.SeedBuild(testBuildObj("full"))
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	bi.Status.SpecResults = testSpecResults(map[string]ebsv1.SpecResult{
		"a": {Status: ebsv1.SpecRunning, DispatchCount: 1},
	})
	seeded := client.SeedBuildInfo(bi)
	repo := testRpmRepoObj(testRepoURL)
	repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseFailed}
	client.SeedRpmRepo(repo)
	seedJobAt(client, seeded, "a", 1, ebsv1.JobSucceeded, testStart)

	// The stop marker write and the convergence land in the same round.
	reconcileOnce(t, c)

	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoCompleted)
	requireCondition(t, persisted.Status.Conditions, ConditionReleaseUnavailable, ReasonRpmRepoReleaseFailed)
}

func TestMissingRpmRepoKeepsRetrying(t *testing.T) {
	c, client, _, _ := newTestController(t)
	key := testNS + "/" + testBuild
	client.SeedProject(testProjectObj(ebsv1.ProjectActive))
	client.SeedBuild(testBuildObj("full"))
	client.SetBuildResourceContent(testBuildResourceContent())
	client.SetBuildTargetContent(testBuildTargetContent())
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	bi.UID = "bi-e29"
	client.SeedBuildGraph(bi, map[string]ebsv1.BuildGraphNode{"a": {Version: "1.0-1"}})
	bi.Status.SpecResults = testSpecResults(map[string]ebsv1.SpecResult{
		"a": {Status: ebsv1.SpecRunning, DispatchCount: 1},
	})
	seeded := client.SeedBuildInfo(bi)
	client.SeedSnapshot(testSnapshotObj(repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	c.specDependsCache.Set(key, map[string]specparse.SpecDepend{"a": dependEntry("a")})
	seedJobAt(client, seeded, "a", 1, ebsv1.JobRunning, testStart)
	// The Build controller may create the RpmRepo later. Repeated 404s must leave the build resumable, even beyond the
	// parse-failure threshold.

	firstWriteCount := 0
	for round := 1; round <= 5; round++ {
		reconcileOnce(t, c)
		persisted := getBuildInfo(t, client)
		requirePhase(t, persisted, ebsv1.BuildInfoProcessing)
		requireCondition(t, persisted.Status.Conditions, ConditionRpmRepoRetrying, ReasonRpmRepoNotFound)
		requireNoCondition(t, persisted.Status.Conditions, ConditionRpmRepoUnavailable)
		if round == 1 {
			firstWriteCount = client.statusWrites
		} else if client.statusWrites != firstWriteCount {
			t.Fatalf("status writes after repeated 404 = %d, want %d", client.statusWrites, firstWriteCount)
		}
	}
	if got := c.counters.Count(counterRpmRepo, key); got != 0 {
		t.Fatalf("parse failure counter = %d, want 0 for missing repo", got)
	}

	// Once the repo arrives, reconciliation resumes and backfills the latest Job without a permanent stop marker.
	client.SeedRpmRepo(testRpmRepoObj(""))
	seedJobAt(client, seeded, "a", 1, ebsv1.JobSucceeded, testStart)
	reconcileOnce(t, c)
	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoCompleted)
	requireNoCondition(t, persisted.Status.Conditions, ConditionRpmRepoRetrying)
	requireNoCondition(t, persisted.Status.Conditions, ConditionRpmRepoUnavailable)
}

func TestStopConvergenceRecordsFailedAndUnfinishedPackages(t *testing.T) {
	c, client, _, _ := newTestController(t)
	client.SeedProject(testProjectObj(ebsv1.ProjectActive))
	client.SeedBuild(testBuildObj("full"))
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	bi.Status.SpecResults = testSpecResults(map[string]ebsv1.SpecResult{
		"repo-failed/failed.spec":             {Status: ebsv1.SpecBuildFailed},
		"repo-unfinished/unfinished.spec":     {Status: ebsv1.SpecPrepared},
		"repo-succeeded/succeeded.spec":       {Status: ebsv1.SpecSucceeded},
		"repo-install/install.spec":           {Status: ebsv1.SpecInstallFailed},
		"repo-undispatched/undispatched.spec": {Status: ebsv1.SpecPrepared},
		"parse-failed/":                       {Status: ebsv1.SpecPrepareFailed},
	})
	upsertCondition(
		&bi.Status.Conditions,
		ConditionRpmRepoUnavailable,
		ReasonRpmRepoXMLDownloadFailed,
		"repository unavailable",
	)
	client.SeedBuildInfo(bi)

	reconcileOnce(t, c)
	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoCompleted)
	want := []string{"parse-failed", "repo-failed", "repo-install", "repo-undispatched", "repo-unfinished"}
	if !slices.Equal(persisted.Status.SpecResults.FailedPackages(), want) {
		t.Fatalf("failedPackages = %v, want %v", persisted.Status.SpecResults.FailedPackages(), want)
	}
}

func TestStopConvergenceFallsBackToSnapshotScope(t *testing.T) {
	c, client, _, _ := newTestController(t)
	project := testProjectObj(ebsv1.ProjectActive)
	project.Spec.PackageRepos = []ebsv1.PackageRepo{{Name: "new-project-repo"}}
	client.SeedProject(project)
	client.SeedBuild(testBuildObj("full"))
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	client.SeedBuildGraph(bi, map[string]ebsv1.BuildGraphNode{"failed": {Version: "1.0"}})
	upsertCondition(
		&bi.Status.Conditions,
		ConditionRpmRepoUnavailable,
		ReasonRpmRepoXMLDownloadFailed,
		"repository unavailable",
	)
	client.SeedBuildInfo(bi)

	if _, err := c.reconcile(context.Background(), testNS+"/"+testBuild); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reconcile with missing Snapshot = %v, want NotFound", err)
	}
	requirePhase(t, getBuildInfo(t, client), ebsv1.BuildInfoProcessing)
	client.SeedSnapshot(testSnapshotObj(
		repoEntry{name: "repo-a", declare: true},
		repoEntry{name: "repo-b", declare: true},
	))

	reconcileOnce(t, c)
	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoCompleted)
	if want := []string{"repo-a", "repo-b"}; !slices.Equal(persisted.Status.SpecResults.FailedPackages(), want) {
		t.Fatalf("failedPackages = %v, want conservative %v", persisted.Status.SpecResults.FailedPackages(), want)
	}
	wantFields := []string{"metadata,spec.packageRepos.name", "metadata,spec.packageRepos.name"}
	if !slices.Equal(client.snapshotReadFields, wantFields) {
		t.Fatalf("Snapshot read fields = %v, want %v", client.snapshotReadFields, wantFields)
	}
}

func TestSnapshotUnavailableEscalates(t *testing.T) {
	c, client, _, _ := newTestController(t)
	key := testNS + "/" + testBuild
	client.SeedProject(testProjectObj(ebsv1.ProjectActive))
	client.SeedBuild(testBuildObj("full"))
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	client.SeedBuildGraph(bi, map[string]ebsv1.BuildGraphNode{"a": {Version: "1.0-1"}})
	bi.Status.SpecResults = testSpecResults(map[string]ebsv1.SpecResult{
		"a": {Status: ebsv1.SpecRunning, DispatchCount: 1},
	})
	seeded := client.SeedBuildInfo(bi)
	client.SeedRpmRepo(testRpmRepoObj(testRepoURL))
	seedJobAt(client, seeded, "a", 1, ebsv1.JobSucceeded, testStart)
	// No Snapshot seeded: 404 counts towards the failure threshold and fails the round below the threshold.

	for round := 1; round <= 2; round++ {
		if _, err := c.reconcile(context.Background(), key); err == nil {
			t.Fatalf("round %d reconcile() error = nil, want the snapshot 404 round failure", round)
		}
		persisted := getBuildInfo(t, client)
		requirePhase(t, persisted, ebsv1.BuildInfoProcessing)
		requireNoCondition(t, persisted.Status.Conditions, ConditionSnapshotUnavailable)
	}

	// Round 3: the threshold escalates; the marker write and the convergence land in the same round.
	reconcileOnce(t, c)
	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoCompleted)
	requireCondition(t, persisted.Status.Conditions, ConditionSnapshotUnavailable, ReasonSnapshotNotFound)
	if got := c.counters.Count(counterSnapshot, key); got != 0 {
		t.Fatalf("snapshot failure counter = %d, want cleared after escalation", got)
	}
}

// Inconsistent status handling.

func TestAdvanceEmptySpecResultsWaits(t *testing.T) {
	c, client, _, _ := newTestController(t)
	client.SeedProject(testProjectObj(ebsv1.ProjectActive))
	client.SeedBuild(testBuildObj("full"))
	client.SeedRpmRepo(testRpmRepoObj(testRepoURL))
	client.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoProcessing))

	reconcileOnce(t, c)

	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoProcessing)
	if len(persisted.Status.Conditions) != 0 {
		t.Fatalf("conditions = %v, want none while spec status is empty", persisted.Status.Conditions)
	}
	if got := len(listJobs(t, client)); got != 0 {
		t.Fatalf("jobs = %d, want 0", got)
	}
}
