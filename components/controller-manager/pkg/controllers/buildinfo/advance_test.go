// advance_test.go covers the 19.1 Job-backfill, dispatch-gate, completion and
// stop-dispatch groups: 7.4.2 count floor, 7.4.4 multi-generation latest pick,
// 7.4.5 phase mapping, 7.4.7 install three branches plus runtime edge
// appends, the 7.4.6 gates, the 6.4 completion check, the 6.5/6.5.1
// convergence branches and the E-28/E-29/E-30 escalations.
package buildinfo

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"controller-manager/pkg/controllers/buildinfo/rpmver"
	"controller-manager/pkg/controllers/buildinfo/specparse"
	ebsv1 "ebs-api/ebs/v1"
)

// seedProcessingRound seeds the objects every Processing advance round needs:
// active project, processing build, build resource, build conf, the given
// BuildInfo, a one-repo snapshot and a held RpmRepo. depends feeds the 15.11
// Processing assembly cache (no git) and sources the metadata cache (URL
// match, no download). Returns the stored BuildInfo (server-assigned UID).
func seedProcessingRound(client *fakeClient, c *Controller, bi *ebsv1.BuildInfo, depends map[string]specparse.SpecDepend, sources *rpmver.RpmMetaSources) *ebsv1.BuildInfo {
	key := testNS + "/" + testBuild
	client.SeedProject(testProjectObj(ebsv1.ProjectActive))
	client.SeedBuild(testBuildObj("full"))
	client.SeedBuildResourceRules(testBuildResourceRules())
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

// seedJobAt seeds an identity-complete Job with an explicit creation time and
// returns the stored copy (server-assigned UID).
func seedJobAt(client *fakeClient, bi *ebsv1.BuildInfo, spec string, generation int64, phase ebsv1.JobPhase, at time.Time) *ebsv1.Job {
	job := testJobObj(bi, spec, generation, phase)
	job.CreationTimestamp = metav1.NewTime(at)
	return client.SeedJob(job)
}

// --- 7.4.2 count floor / 7.4.4 latest pick / 7.4.5 phase mapping ---

func TestAdvanceBackfillMultiGeneration(t *testing.T) {
	c, client, _, _ := newTestController(t)
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	bi.Status.Dcg = dcgStateAB()
	bi.Status.SpecStatus = map[string]ebsv1.SpecStatus{
		"a": {Build: ebsv1.SpecBuildStatus{Status: SpecBuildRunning}, DispatchCount: 1},
		"b": {},
	}
	seeded := seedProcessingRound(client, c, bi, map[string]specparse.SpecDepend{
		"a": dependEntry("a"), "b": dependEntry("b"),
	}, testSources())
	// The older generation Succeeded, the latest (7.4.4) Failed: the rebuild
	// maps to RebuildFailed and the count floor rises to 2 (7.4.2).
	seedJobAt(client, seeded, "a", 1, ebsv1.JobSucceeded, testStart)
	seedJobAt(client, seeded, "a", 2, ebsv1.JobFailed, testStart.Add(time.Minute))
	// Out-of-build-set Jobs are ignored by the scoped grouping (E-05).
	seedJobAt(client, seeded, "ghost", 1, ebsv1.JobSucceeded, testStart.Add(2*time.Minute))

	reconcileOnce(t, c)

	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoProcessing)
	a := persisted.Status.SpecStatus["a"]
	if a.Build.Status != SpecBuildFailed || a.DispatchCount != 2 {
		t.Fatalf("specStatus[a] = %+v, want Failed with floor count 2", a)
	}
	requireCondition(t, a.Build.Conditions, ConditionRebuildFailed, ReasonRebuildJobFailed)
	if want := jobNameFor(string(seeded.UID), "a", 2); a.Build.JobName != want {
		t.Fatalf("specStatus[a].JobName = %q, want generation-2 job %q", a.Build.JobName, want)
	}
	if _, ok := persisted.Status.SpecStatus["ghost"]; ok {
		t.Fatalf("out-of-scope job leaked into specStatus: %v", persisted.Status.SpecStatus)
	}
	// A Failed upstream exempts both 7.4.6 gates (7.4.2): b dispatches at once.
	b := persisted.Status.SpecStatus["b"]
	if b.DispatchCount != 1 || b.Build.JobName == "" {
		t.Fatalf("specStatus[b] = %+v, want dispatched (gen 1)", b)
	}
}

func TestAdvanceBackfillUnknownPhaseSkipped(t *testing.T) {
	c, client, _, _ := newTestController(t)
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	bi.Status.Dcg = map[string]ebsv1.DcgNodeState{"a": {Version: "1.0-1"}}
	bi.Status.SpecStatus = map[string]ebsv1.SpecStatus{
		"a": {Build: ebsv1.SpecBuildStatus{Status: SpecBuildRunning}, DispatchCount: 1},
	}
	seeded := seedProcessingRound(client, c, bi, map[string]specparse.SpecDepend{"a": dependEntry("a")}, testSources())
	seedJobAt(client, seeded, "a", 1, ebsv1.JobPhase("Phasing"), testStart)

	reconcileOnce(t, c)

	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoProcessing)
	a := persisted.Status.SpecStatus["a"]
	if a.Build.Status != SpecBuildRunning {
		t.Fatalf("specStatus[a].Build.Status = %q, want Running (unknown phase unmapped)", a.Build.Status)
	}
	if a.Build.JobName == "" {
		t.Fatal("specStatus[a].JobName empty, want the latest job name backfilled")
	}
	requireNoCondition(t, a.Build.Conditions, ConditionBuildFailed)
}

func TestAdvanceBackfillJobAborted(t *testing.T) {
	c, client, _, _ := newTestController(t)
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	bi.Status.Dcg = map[string]ebsv1.DcgNodeState{"a": {Version: "1.0-1"}}
	bi.Status.SpecStatus = map[string]ebsv1.SpecStatus{
		"a": {Build: ebsv1.SpecBuildStatus{Status: SpecBuildRunning}, DispatchCount: 1},
	}
	seeded := seedProcessingRound(client, c, bi, map[string]specparse.SpecDepend{"a": dependEntry("a")}, testSources())
	seedJobAt(client, seeded, "a", 1, ebsv1.JobAborted, testStart)

	reconcileOnce(t, c)

	persisted := getBuildInfo(t, client)
	a := persisted.Status.SpecStatus["a"]
	if a.Build.Status != SpecBuildFailed {
		t.Fatalf("specStatus[a].Build.Status = %q, want Failed (Aborted treated as Failed)", a.Build.Status)
	}
	requireCondition(t, a.Build.Conditions, ConditionBuildAborted, ReasonBuildAborted)
	requirePhase(t, persisted, ebsv1.BuildInfoCompleted)
	requireCondition(t, persisted.Status.Conditions, ConditionPartialFailure, ReasonPartialFailure)
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

// --- 7.4.7 install backfill three branches + runtime edge appends ---

func TestAdvanceInstallBackfillBranches(t *testing.T) {
	t.Run("empty message marks install succeeded", func(t *testing.T) {
		c, client, _, _ := newTestController(t)
		bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
		bi.Status.Dcg = map[string]ebsv1.DcgNodeState{"a": {Version: "1.0-1"}}
		bi.Status.SpecStatus = map[string]ebsv1.SpecStatus{
			"a": {Build: ebsv1.SpecBuildStatus{Status: SpecBuildRunning}, DispatchCount: 1},
		}
		seeded := seedProcessingRound(client, c, bi, map[string]specparse.SpecDepend{"a": dependEntry("a")}, testSources())
		seedJobAt(client, seeded, "a", 1, ebsv1.JobSucceeded, testStart)

		reconcileOnce(t, c)

		persisted := getBuildInfo(t, client)
		if got := persisted.Status.SpecStatus["a"].Install.Status; got != SpecBuildSucceeded {
			t.Fatalf("install status = %q, want Succeeded", got)
		}
		requirePhase(t, persisted, ebsv1.BuildInfoCompleted)
		requireCondition(t, persisted.Status.Conditions, ConditionAllSpecsSucceeded, ReasonAllSpecsSucceeded)
	})

	t.Run("non-JSON message leaves install untouched", func(t *testing.T) {
		c, client, _, _ := newTestController(t)
		bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
		bi.Status.Dcg = map[string]ebsv1.DcgNodeState{"a": {Version: "1.0-1"}}
		bi.Status.SpecStatus = map[string]ebsv1.SpecStatus{
			"a": {Build: ebsv1.SpecBuildStatus{Status: SpecBuildRunning}, DispatchCount: 1},
		}
		seeded := seedProcessingRound(client, c, bi, map[string]specparse.SpecDepend{"a": dependEntry("a")}, testSources())
		job := testJobObj(seeded, "a", 1, ebsv1.JobSucceeded)
		job.CreationTimestamp = metav1.NewTime(testStart)
		job.Status.Message = "build ok"
		client.SeedJob(job)

		reconcileOnce(t, c)

		persisted := getBuildInfo(t, client)
		a := persisted.Status.SpecStatus["a"]
		if a.Install.Status != "" {
			t.Fatalf("install status = %q, want untouched (unparseable message)", a.Install.Status)
		}
		requireNoCondition(t, a.Install.Conditions, ConditionInstall)
		requirePhase(t, persisted, ebsv1.BuildInfoCompleted)
	})

	t.Run("missing deps fail install and append runtime edges", func(t *testing.T) {
		c, client, _, _ := newTestController(t)
		bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
		bi.Status.Dcg = map[string]ebsv1.DcgNodeState{
			"a": {Version: "1.0-1"},
			"b": {Version: "1.0-1"},
		}
		bi.Status.SpecStatus = map[string]ebsv1.SpecStatus{
			"a": {},
			"b": {
				Build:         ebsv1.SpecBuildStatus{Status: SpecBuildSucceeded},
				DispatchCount: 1,
				Install: ebsv1.SpecInstallStatus{
					MissingDeps: map[string]ebsv1.MissingDep{
						"a": {NeededBy: "old", VersionRequests: ebsv1.VersionConst{GE: "1.0"}},
					},
				},
			},
		}
		seeded := seedProcessingRound(client, c, bi, map[string]specparse.SpecDepend{
			"a": dependEntry("a"), "b": dependEntry("b"),
		}, testSources(testRpm("a", "a", "1.0")))
		job := testJobObj(seeded, "b", 1, ebsv1.JobSucceeded)
		job.CreationTimestamp = metav1.NewTime(testStart)
		job.Status.Message = `{"missing_deps":{"a":{"needed_by":"new","version_requests":{"GE":"2.0"}}}}`
		client.SeedJob(job)

		reconcileOnce(t, c)

		persisted := getBuildInfo(t, client)
		requirePhase(t, persisted, ebsv1.BuildInfoProcessing)
		b := persisted.Status.SpecStatus["b"]
		if b.Install.Status != SpecBuildFailed {
			t.Fatalf("install status = %q, want Failed", b.Install.Status)
		}
		// Idempotent merge (7.4.7): the pre-existing entry is never overwritten.
		if got := b.Install.MissingDeps["a"]; got.NeededBy != "old" || got.VersionRequests.GE != "1.0" {
			t.Fatalf("missingDeps[a] = %+v, want the pre-existing entry kept", got)
		}
		requireCondition(t, b.Install.Conditions, ConditionInstall, ReasonInstallCheckFailed)
		// Runtime edge: provider a is in the build set and non-terminal (7.4.7);
		// the candidate graph is persisted before any dispatch (G-02).
		if _, ok := persisted.Status.Dcg["b"].InstallInDep["a"]; !ok {
			t.Fatalf("dcg[b].InstallInDep = %v, want runtime edge on a", persisted.Status.Dcg["b"].InstallInDep)
		}
		a := persisted.Status.SpecStatus["a"]
		if a.DispatchCount != 1 || a.Build.JobName == "" {
			t.Fatalf("specStatus[a] = %+v, want dispatched (gen 1)", a)
		}
	})
}

// --- 7.4.6 dispatch gates ---

func TestAdvanceGatePublishConfirmation(t *testing.T) {
	c, client, _, _ := newTestController(t)
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	bi.Status.Dcg = dcgStateAB()
	bi.Status.SpecStatus = map[string]ebsv1.SpecStatus{
		"a": {Build: ebsv1.SpecBuildStatus{Status: SpecBuildRunning}, DispatchCount: 1},
		"b": {},
	}
	seeded := seedProcessingRound(client, c, bi, map[string]specparse.SpecDepend{
		"a": dependEntry("a"), "b": dependEntry("b"),
	}, testSources())
	a1 := seedJobAt(client, seeded, "a", 1, ebsv1.JobSucceeded, testStart)

	// Round 1: gate 2 blocks b — the Succeeded upstream's Job UID is not in
	// the RpmRepo sourceJobUIDs set; no condition is written (7.4.6).
	reconcileOnce(t, c)
	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoProcessing)
	if got := persisted.Status.SpecStatus["a"].Build.Status; got != SpecBuildSucceeded {
		t.Fatalf("specStatus[a].Build.Status = %q, want Succeeded", got)
	}
	if got := persisted.Status.SpecStatus["b"].DispatchCount; got != 0 {
		t.Fatalf("specStatus[b].DispatchCount = %d, want 0 (publish gate)", got)
	}
	if len(persisted.Status.SpecStatus["b"].Build.Conditions) != 0 {
		t.Fatalf("specStatus[b] conditions = %v, want none", persisted.Status.SpecStatus["b"].Build.Conditions)
	}
	if got := len(listJobs(t, client)); got != 1 {
		t.Fatalf("jobs = %d, want 1", got)
	}

	// Round 2: the upstream Job is published — b dispatches.
	repo := testRpmRepoObj(testRepoURL)
	repo.Status.Repository.SourceJobUIDs = []string{string(a1.UID)}
	client.SeedRpmRepo(repo)
	reconcileOnce(t, c)

	persisted = getBuildInfo(t, client)
	b := persisted.Status.SpecStatus["b"]
	if b.DispatchCount != 1 || b.Build.JobName == "" {
		t.Fatalf("specStatus[b] = %+v, want dispatched after publish", b)
	}
}

func TestAdvanceGateRebuildConsistencyCycle(t *testing.T) {
	c, client, _, _ := newTestController(t)
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	// Cycle a<->b with a as the break point: both require 2 dispatches.
	bi.Status.Dcg = map[string]ebsv1.DcgNodeState{
		"a": {Version: "1.0-1", OutDep: []string{"b"}, InDep: map[string]ebsv1.VersionConst{"b": {}}, BootstrapBreak: true},
		"b": {Version: "1.0-1", OutDep: []string{"a"}, InDep: map[string]ebsv1.VersionConst{"a": {}}},
	}
	bi.Status.SpecStatus = map[string]ebsv1.SpecStatus{
		"a": {Build: ebsv1.SpecBuildStatus{Status: SpecBuildSucceeded}, DispatchCount: 1},
		"b": {Build: ebsv1.SpecBuildStatus{Status: SpecBuildSucceeded}, DispatchCount: 1},
	}
	seeded := seedProcessingRound(client, c, bi, map[string]specparse.SpecDepend{
		"a": dependEntry("a"), "b": dependEntry("b"),
	}, testSources())
	a1 := seedJobAt(client, seeded, "a", 1, ebsv1.JobSucceeded, testStart)
	b1 := seedJobAt(client, seeded, "b", 1, ebsv1.JobSucceeded, testStart.Add(time.Minute))
	publish := func(uids ...string) {
		repo := testRpmRepoObj(testRepoURL)
		repo.Status.Repository.SourceJobUIDs = uids
		client.SeedRpmRepo(repo)
	}
	publish(string(a1.UID))

	// Round 1: a's second dispatch is publish-gated on b; b's second dispatch
	// is rebuild-consistency-gated on a (7.4.6 ①: upstream below its
	// effective required).
	reconcileOnce(t, c)
	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoProcessing)
	if got := persisted.Status.SpecStatus["a"].DispatchCount; got != 1 {
		t.Fatalf("round1 specStatus[a].DispatchCount = %d, want 1 (gate 2 blocks the rebuild)", got)
	}
	if got := persisted.Status.SpecStatus["b"].DispatchCount; got != 1 {
		t.Fatalf("round1 specStatus[b].DispatchCount = %d, want 1 (gate 1 blocks the rebuild)", got)
	}
	if got := len(listJobs(t, client)); got != 2 {
		t.Fatalf("round1 jobs = %d, want 2", got)
	}

	// Round 2: b published — a rebuilds (the break point skips gate 1). b then
	// passes gate 1 (a Succeeded at count 2) and gate 2 (the round-start List
	// still holds a's published generation-1 Job; the generation-2 Job was
	// created after the List, 7.4.6 gate 2) — the cycle completes at the
	// required counts in the same round (6.4).
	publish(string(a1.UID), string(b1.UID))
	reconcileOnce(t, c)
	persisted = getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoCompleted)
	requireCondition(t, persisted.Status.Conditions, ConditionAllSpecsSucceeded, ReasonAllSpecsSucceeded)
	if got := persisted.Status.SpecStatus["a"].DispatchCount; got != 2 {
		t.Fatalf("round2 specStatus[a].DispatchCount = %d, want 2", got)
	}
	if got := persisted.Status.SpecStatus["b"].DispatchCount; got != 2 {
		t.Fatalf("round2 specStatus[b].DispatchCount = %d, want 2", got)
	}
	if got := len(listJobs(t, client)); got != 4 {
		t.Fatalf("round2 jobs = %d, want 4 (two generations each)", got)
	}
}

// --- 6.4 completion check ---

func TestAdvanceCompletionPartialFailure(t *testing.T) {
	c, client, _, _ := newTestController(t)
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	bi.Status.Dcg = map[string]ebsv1.DcgNodeState{
		"a": {Version: "1.0-1"},
		"b": {Version: "1.0-1"},
	}
	bi.Status.SpecStatus = map[string]ebsv1.SpecStatus{
		"a": {Build: ebsv1.SpecBuildStatus{Status: SpecBuildRunning}, DispatchCount: 1},
		"b": {Build: ebsv1.SpecBuildStatus{Status: SpecBuildRunning}, DispatchCount: 1},
	}
	seeded := seedProcessingRound(client, c, bi, map[string]specparse.SpecDepend{
		"a": dependEntry("a"), "b": dependEntry("b"),
	}, testSources())
	seedJobAt(client, seeded, "a", 1, ebsv1.JobSucceeded, testStart)
	seedJobAt(client, seeded, "b", 1, ebsv1.JobFailed, testStart)

	reconcileOnce(t, c)

	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoCompleted)
	cond := requireCondition(t, persisted.Status.Conditions, ConditionPartialFailure, ReasonPartialFailure)
	if !strings.Contains(cond.Message, "b") {
		t.Fatalf("PartialFailure message = %q, want failed spec b listed", cond.Message)
	}
	requireNoCondition(t, persisted.Status.Conditions, ConditionAllSpecsSucceeded)
	requireCondition(t, persisted.Status.SpecStatus["b"].Build.Conditions, ConditionBuildFailed, ReasonJobFailed)
	if got := persisted.Status.FailedPackages; len(got) != 1 || got[0] != "repo1" {
		t.Fatalf("failedPackages = %v, want [repo1]", got)
	}
}

func TestAdvanceCompletionRecordsInstallFailureRepository(t *testing.T) {
	c, client, _, _ := newTestController(t)
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	bi.Status.Dcg = map[string]ebsv1.DcgNodeState{"a": {Version: "1.0-1"}}
	bi.Status.SpecStatus = map[string]ebsv1.SpecStatus{
		"a": {
			Build:         ebsv1.SpecBuildStatus{Status: SpecBuildSucceeded},
			Install:       ebsv1.SpecInstallStatus{Status: SpecBuildFailed},
			DispatchCount: 1,
		},
	}
	seedProcessingRound(client, c, bi, map[string]specparse.SpecDepend{"a": dependEntry("a")}, testSources())

	reconcileOnce(t, c)

	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoCompleted)
	if got := persisted.Status.FailedPackages; len(got) != 1 || got[0] != "repo1" {
		t.Fatalf("failedPackages = %v, want [repo1]", got)
	}
}

func TestAdvanceCompletionBlockedByPendingCreates(t *testing.T) {
	c, client, _, _ := newTestController(t)
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	bi.UID = "bi-pending-block"
	bi.Status.Dcg = map[string]ebsv1.DcgNodeState{"a": {Version: "1.0-1"}}
	bi.Status.SpecStatus = map[string]ebsv1.SpecStatus{
		"a": {Build: ebsv1.SpecBuildStatus{Status: SpecBuildRunning}, DispatchCount: 1},
	}
	bi.Status.PendingJobCreates = map[string]ebsv1.PendingJobCreate{
		"a": {JobName: jobNameFor("bi-pending-block", "a", 2), DispatchGeneration: 2},
	}
	seeded := seedProcessingRound(client, c, bi, map[string]specparse.SpecDepend{"a": dependEntry("a")}, testSources())
	seedJobAt(client, seeded, "a", 1, ebsv1.JobSucceeded, testStart)

	reconcileOnce(t, c)

	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoProcessing)
	requireNoCondition(t, persisted.Status.Conditions, ConditionAllSpecsSucceeded)
	if len(persisted.Status.PendingJobCreates) != 1 {
		t.Fatalf("pendingJobCreates = %v, want the unresolved entry kept (6.5.1)", persisted.Status.PendingJobCreates)
	}
}

// --- 6.5/6.5.1 stop-dispatch convergence ---

func TestConvergePendingCreateConfirmed(t *testing.T) {
	c, client, _, _ := newTestController(t)
	client.SeedProject(testProjectObj(ebsv1.ProjectActive))
	client.SeedBuild(testBuildObj("full"))
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	bi.UID = "bi-converge-confirm"
	upsertCondition(&bi.Status.Conditions, ConditionRpmRepoUnavailable, ReasonRpmRepoNotFound, "rpmrepo gone")
	bi.Status.SpecStatus = map[string]ebsv1.SpecStatus{
		"a": {Build: ebsv1.SpecBuildStatus{Status: SpecBuildRunning}, DispatchCount: 1},
	}
	bi.Status.PendingJobCreates = map[string]ebsv1.PendingJobCreate{
		"a": {JobName: jobNameFor("bi-converge-confirm", "a", 1), DispatchGeneration: 1},
	}
	seeded := client.SeedBuildInfo(bi)
	seedJobAt(client, seeded, "a", 1, ebsv1.JobSucceeded, testStart)

	reconcileOnce(t, c)

	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoCompleted)
	if len(persisted.Status.PendingJobCreates) != 0 {
		t.Fatalf("pendingJobCreates = %v, want confirmed and removed (6.5.1 #2)", persisted.Status.PendingJobCreates)
	}
	if got := persisted.Status.SpecStatus["a"].Build.Status; got != SpecBuildSucceeded {
		t.Fatalf("specStatus[a].Build.Status = %q, want Succeeded", got)
	}
	requireCondition(t, persisted.Status.Conditions, ConditionRpmRepoUnavailable, ReasonRpmRepoNotFound)
	requireNoCondition(t, persisted.Status.Conditions, ConditionAllSpecsSucceeded)
	requireNoCondition(t, persisted.Status.Conditions, ConditionPartialFailure)
}

func TestConvergePendingCreate404Kept(t *testing.T) {
	c, client, _, _ := newTestController(t)
	client.SeedProject(testProjectObj(ebsv1.ProjectActive))
	client.SeedBuild(testBuildObj("full"))
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	bi.UID = "bi-converge-404"
	upsertCondition(&bi.Status.Conditions, ConditionSnapshotUnavailable, ReasonSnapshotNotFound, "snapshot gone")
	bi.Status.SpecStatus = map[string]ebsv1.SpecStatus{
		"a": {Build: ebsv1.SpecBuildStatus{Status: SpecBuildRunning}, DispatchCount: 1},
	}
	bi.Status.PendingJobCreates = map[string]ebsv1.PendingJobCreate{
		"a": {JobName: jobNameFor("bi-converge-404", "a", 1), DispatchGeneration: 1},
	}
	client.SeedBuildInfo(bi)
	// No Job stored: a 404 never proves an in-flight create (6.5.1 #5).

	reconcileOnce(t, c)

	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoProcessing)
	if len(persisted.Status.PendingJobCreates) != 1 {
		t.Fatalf("pendingJobCreates = %v, want the 404 entry kept", persisted.Status.PendingJobCreates)
	}
	requireCondition(t, persisted.Status.Conditions, ConditionSnapshotUnavailable, ReasonSnapshotNotFound)
}

func TestConvergeUnknownPhaseJobWaits(t *testing.T) {
	c, client, _, _ := newTestController(t)
	client.SeedProject(testProjectObj(ebsv1.ProjectActive))
	client.SeedBuild(testBuildObj("full"))
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	upsertCondition(&bi.Status.Conditions, ConditionReleaseFailed, ReasonRpmRepoReleaseFailed, "release failed")
	bi.Status.SpecStatus = map[string]ebsv1.SpecStatus{
		"a": {Build: ebsv1.SpecBuildStatus{Status: SpecBuildRunning}, DispatchCount: 1},
	}
	seeded := client.SeedBuildInfo(bi)
	seedJobAt(client, seeded, "a", 1, ebsv1.JobPhase("Phasing"), testStart)

	reconcileOnce(t, c)

	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoProcessing)
	requireCondition(t, persisted.Status.Conditions, ConditionReleaseFailed, ReasonRpmRepoReleaseFailed)
	if got := persisted.Status.SpecStatus["a"].Build.Status; got != SpecBuildRunning {
		t.Fatalf("specStatus[a].Build.Status = %q, want Running (unknown phase unmapped)", got)
	}
}

// --- E-28/E-29/E-30 escalations ---

func TestE28ReleaseFailedStopsDispatch(t *testing.T) {
	c, client, _, _ := newTestController(t)
	client.SeedProject(testProjectObj(ebsv1.ProjectActive))
	client.SeedBuild(testBuildObj("full"))
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	bi.Status.SpecStatus = map[string]ebsv1.SpecStatus{
		"a": {Build: ebsv1.SpecBuildStatus{Status: SpecBuildRunning}, DispatchCount: 1},
	}
	seeded := client.SeedBuildInfo(bi)
	repo := testRpmRepoObj(testRepoURL)
	repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseFailed}
	client.SeedRpmRepo(repo)
	seedJobAt(client, seeded, "a", 1, ebsv1.JobSucceeded, testStart)

	// The stop marker write and the convergence land in the same round.
	reconcileOnce(t, c)

	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoCompleted)
	requireCondition(t, persisted.Status.Conditions, ConditionReleaseFailed, ReasonRpmRepoReleaseFailed)
	requireNoCondition(t, persisted.Status.Conditions, ConditionAllSpecsSucceeded)
	requireNoCondition(t, persisted.Status.Conditions, ConditionPartialFailure)
}

func TestE29RpmRepoUnavailableEscalates(t *testing.T) {
	c, client, _, _ := newTestController(t)
	key := testNS + "/" + testBuild
	client.SeedProject(testProjectObj(ebsv1.ProjectActive))
	client.SeedBuild(testBuildObj("full"))
	client.SeedBuildResourceRules(testBuildResourceRules())
	client.SetBuildTargetContent(testBuildTargetContent())
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	bi.UID = "bi-e29"
	bi.Status.Dcg = map[string]ebsv1.DcgNodeState{"a": {Version: "1.0-1"}}
	bi.Status.SpecStatus = map[string]ebsv1.SpecStatus{
		"a": {Build: ebsv1.SpecBuildStatus{Status: SpecBuildRunning}, DispatchCount: 1},
	}
	bi.Status.PendingJobCreates = map[string]ebsv1.PendingJobCreate{
		"a": {JobName: jobNameFor("bi-e29", "a", 2), DispatchGeneration: 2},
	}
	seeded := client.SeedBuildInfo(bi)
	client.SeedSnapshot(testSnapshotObj(repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	c.specDependsCache.Set(key, map[string]specparse.SpecDepend{"a": dependEntry("a")})
	seedJobAt(client, seeded, "a", 1, ebsv1.JobSucceeded, testStart)
	// No RpmRepo seeded: 404 rounds count towards E-29 (E-16 continues below
	// the threshold).

	for round := 1; round <= 2; round++ {
		reconcileOnce(t, c)
		persisted := getBuildInfo(t, client)
		requirePhase(t, persisted, ebsv1.BuildInfoProcessing)
		requireNoCondition(t, persisted.Status.Conditions, ConditionRpmRepoUnavailable)
	}

	// Round 3: the threshold escalates to the stop marker; the convergence
	// path keeps the unresolved pending create on the 404 (6.5.1 #5).
	reconcileOnce(t, c)
	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoProcessing)
	requireCondition(t, persisted.Status.Conditions, ConditionRpmRepoUnavailable, ReasonRpmRepoNotFound)
	if len(persisted.Status.PendingJobCreates) != 1 {
		t.Fatalf("pendingJobCreates = %v, want the 404 entry kept", persisted.Status.PendingJobCreates)
	}
	// The readiness counter is cleared after the escalation (5.4).
	if got := c.counters.Count(counterRpmRepo, key); got != 0 {
		t.Fatalf("rpmrepo failure counter = %d, want cleared after escalation", got)
	}

	// The created Job appears: the list confirms the pending entry and the
	// convergence completes with the marker preserved (6.5).
	seedJobAt(client, seeded, "a", 2, ebsv1.JobSucceeded, testStart.Add(time.Minute))
	reconcileOnce(t, c)
	persisted = getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoCompleted)
	requireCondition(t, persisted.Status.Conditions, ConditionRpmRepoUnavailable, ReasonRpmRepoNotFound)
	requireNoCondition(t, persisted.Status.Conditions, ConditionAllSpecsSucceeded)
	if len(persisted.Status.PendingJobCreates) != 0 {
		t.Fatalf("pendingJobCreates = %v, want confirmed and removed", persisted.Status.PendingJobCreates)
	}
}

func TestE30SnapshotUnavailableEscalates(t *testing.T) {
	c, client, _, _ := newTestController(t)
	key := testNS + "/" + testBuild
	client.SeedProject(testProjectObj(ebsv1.ProjectActive))
	client.SeedBuild(testBuildObj("full"))
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	bi.Status.Dcg = map[string]ebsv1.DcgNodeState{"a": {Version: "1.0-1"}}
	bi.Status.SpecStatus = map[string]ebsv1.SpecStatus{
		"a": {Build: ebsv1.SpecBuildStatus{Status: SpecBuildRunning}, DispatchCount: 1},
	}
	seeded := client.SeedBuildInfo(bi)
	client.SeedRpmRepo(testRpmRepoObj(testRepoURL))
	seedJobAt(client, seeded, "a", 1, ebsv1.JobSucceeded, testStart)
	// No Snapshot seeded: 404 counts towards E-30 and fails the round below
	// the threshold.

	for round := 1; round <= 2; round++ {
		if _, err := c.reconcile(context.Background(), key); err == nil {
			t.Fatalf("round %d reconcile() error = nil, want the snapshot 404 round failure", round)
		}
		persisted := getBuildInfo(t, client)
		requirePhase(t, persisted, ebsv1.BuildInfoProcessing)
		requireNoCondition(t, persisted.Status.Conditions, ConditionSnapshotUnavailable)
	}

	// Round 3: the threshold escalates; the marker write and the convergence
	// land in the same round.
	reconcileOnce(t, c)
	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoCompleted)
	requireCondition(t, persisted.Status.Conditions, ConditionSnapshotUnavailable, ReasonSnapshotNotFound)
	requireNoCondition(t, persisted.Status.Conditions, ConditionAllSpecsSucceeded)
	if got := c.counters.Count(counterSnapshot, key); got != 0 {
		t.Fatalf("snapshot failure counter = %d, want cleared after escalation", got)
	}
}

// --- E-01 / residual-Aborted defenses ---

func TestAdvanceEmptySpecStatusWaits(t *testing.T) {
	c, client, _, _ := newTestController(t)
	client.SeedProject(testProjectObj(ebsv1.ProjectActive))
	client.SeedBuild(testBuildObj("full"))
	client.SeedRpmRepo(testRpmRepoObj(testRepoURL))
	client.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoProcessing))

	reconcileOnce(t, c)

	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoProcessing)
	if len(persisted.Status.Conditions) != 0 {
		t.Fatalf("conditions = %v, want none (E-01 waits silently)", persisted.Status.Conditions)
	}
	if got := len(listJobs(t, client)); got != 0 {
		t.Fatalf("jobs = %d, want 0", got)
	}
}

func TestAdvanceResidualAbortedSkips(t *testing.T) {
	c, client, _, _ := newTestController(t)
	client.SeedProject(testProjectObj(ebsv1.ProjectActive))
	client.SeedBuild(testBuildObj("full"))
	client.SeedRpmRepo(testRpmRepoObj(testRepoURL))
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	bi.Status.SpecStatus = map[string]ebsv1.SpecStatus{
		"a": {Build: ebsv1.SpecBuildStatus{Status: SpecBuildAborted}, DispatchCount: 1},
	}
	client.SeedBuildInfo(bi)

	reconcileOnce(t, c)

	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoProcessing)
	if got := persisted.Status.SpecStatus["a"].Build.Status; got != SpecBuildAborted {
		t.Fatalf("specStatus[a].Build.Status = %q, want the residual Aborted kept (6.4)", got)
	}
	if len(persisted.Status.Conditions) != 0 {
		t.Fatalf("conditions = %v, want none", persisted.Status.Conditions)
	}
}
