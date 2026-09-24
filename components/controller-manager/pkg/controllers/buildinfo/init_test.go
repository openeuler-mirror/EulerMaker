// init_test.go covers the 19.1 build-set (7.2.2/E-23/E-24) and single (7.2.3)
// test groups: per-type build-set determination, parent Build package seeds,
// fixed-point downstream expansion, shared incremental/specified degradation,
// single-only empty-set closeouts, the init deterministic
// checks (E-16/E-19/E-26/E-27) and the single直通 path (assembly, direct
// dispatch, Repo injection, empty-set closeout).
package buildinfo

import (
	"context"
	"errors"
	"strings"
	"testing"

	"controller-manager/pkg/clients/gitserver"
	ebsv1 "ebs-api/ebs/v1"
)

const (
	gitURL1 = "http://git.local/repo1"
	gitURL2 = "http://git.local/repo2"
	gitURL3 = "http://git.local/repo3"
	gitURL4 = "http://git.local/repo4"
)

// jobSpecNames collects the spec-name label set of every stored Job.
func jobSpecNames(t *testing.T, client *fakeClient) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	for _, job := range listJobs(t, client) {
		names[job.Labels[ebsv1.JobSpecNameLabel]] = true
	}
	return names
}

func requireSpecNames(t *testing.T, bi *ebsv1.BuildInfo, want ...string) {
	t.Helper()
	if len(bi.Status.SpecStatus) != len(want) {
		t.Fatalf("specStatus size = %d, want %d (%v)", len(bi.Status.SpecStatus), len(want), bi.Status.SpecStatus)
	}
	for _, name := range want {
		if _, ok := bi.Status.SpecStatus[name]; !ok {
			t.Fatalf("specStatus[%s] missing, have %v", name, bi.Status.SpecStatus)
		}
	}
}

// --- 7.2.2 build-set determination ---

func TestInitFullHappyPath(t *testing.T) {
	c, client, git, _ := newTestController(t)
	seedHealthyBasics(client, "full")
	client.SeedSnapshot(testSnapshotObj(
		repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true},
		repoEntry{name: "repo2", cloneURL: gitURL2, commitID: "c2", declare: true}))
	client.SeedRpmRepo(testRpmRepoObj(""))
	git.repo(gitURL1, "c1", map[string]string{"a.spec": specText("a")})
	git.repo(gitURL2, "c2", map[string]string{"b.spec": specText("b")})

	reconcileOnce(t, c)

	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoProcessing)
	requireSpecNames(t, bi, "a", "b")
	for _, name := range []string{"a", "b"} {
		ss := bi.Status.SpecStatus[name]
		// A freshly created Job carries no phase yet, so Build.Status stays
		// empty until the first backfill maps Pending/Running.
		if ss.DispatchCount != 1 || ss.Build.JobName == "" {
			t.Fatalf("specStatus[%s] = %+v, want dispatched (gen 1, job named)", name, ss)
		}
	}
	if got := jobSpecNames(t, client); len(got) != 2 || !got["a"] || !got["b"] {
		t.Fatalf("jobs = %v, want {a,b}", got)
	}
	if len(bi.Status.Dcg) != 2 {
		t.Fatalf("status.dcg size = %d, want 2 (G-02 persist)", len(bi.Status.Dcg))
	}
	if got := c.dcgDict.Len(); got != 1 {
		t.Fatalf("dcgDict len = %d, want 1", got)
	}
	if len(bi.Status.Conditions) != 0 {
		t.Fatalf("conditions = %v, want none", bi.Status.Conditions)
	}
}

func TestInitGitCommandsUseOriginURL(t *testing.T) {
	const originURL = "https://gitee.com/src-openeuler/repo1.git"
	const cloneURL = "git://git-server:9418/gitee.com/src-openeuler/repo1.git"
	c, client, git, _ := newTestController(t)
	seedHealthyBasics(client, "full")
	client.SeedSnapshot(testSnapshotObj(repoEntry{
		name: "repo1", originURL: originURL, cloneURL: cloneURL, commitID: "c1", declare: true,
	}))
	client.SeedRpmRepo(testRpmRepoObj(""))
	git.repo(originURL, "c1", map[string]string{"a.spec": specText("a")})

	reconcileOnce(t, c)

	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoProcessing)
	requireSpecNames(t, bi, "a")
	jobs := listJobs(t, client)
	if len(jobs) != 1 || !strings.Contains(jobs[0].Spec.Payload, cloneURL) {
		t.Fatalf("jobs = %+v, want one Job with clone URL in payload", jobs)
	}
}

func TestInitEmptySnapshotCompletesEmpty(t *testing.T) {
	c, client, _, _ := newTestController(t)
	seedHealthyBasics(client, "full")
	client.SeedSnapshot(testSnapshotObj())
	client.SeedRpmRepo(testRpmRepoObj(""))

	reconcileOnce(t, c)

	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoCompleted)
	requireSpecNames(t, bi)
	if len(bi.Status.Conditions) != 0 {
		t.Fatalf("conditions = %v, want none for an empty full build", bi.Status.Conditions)
	}
	if got := len(listJobs(t, client)); got != 0 {
		t.Fatalf("jobs = %d, want 0", got)
	}
}

func TestInitSpecifiedExpansionFixedPoint(t *testing.T) {
	c, client, git, _ := newTestController(t)
	key := testNS + "/" + testBuild
	seedHealthyBasics(client, "specified", "repo1")
	client.SeedSnapshot(testSnapshotObj(
		repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true},
		repoEntry{name: "repo2", cloneURL: gitURL2, commitID: "c2", declare: true},
		repoEntry{name: "repo3", cloneURL: gitURL3, commitID: "c3", declare: true},
		repoEntry{name: "repo4", cloneURL: gitURL4, commitID: "c4", declare: true}))
	client.SeedRpmRepo(testRpmRepoObj(testRepoURL))
	git.repo(gitURL1, "c1", map[string]string{"a.spec": specText("a")})
	git.repo(gitURL2, "c2", map[string]string{"b.spec": specText("b", "a")})
	git.repo(gitURL3, "c3", map[string]string{"c.spec": specText("c")})
	git.repo(gitURL4, "c4", map[string]string{"d.spec": specText("d")})
	// Pre-populated repo layer (URL match -> no download): b joins the build
	// set via the buildRequires reverse lookup, c via install requires, and d
	// one iteration later via c (fixed point).
	c.rpmMetaSources.Set(key, testSources(
		testRpm("a", "a", "1.0"),
		testRpm("b", "b", "1.0"),
		testRpm("c", "c", "1.0", "a"),
		testRpm("d", "d", "1.0", "c")))

	reconcileOnce(t, c)

	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoProcessing)
	requireSpecNames(t, bi, "a", "b", "c", "d")
	// Edges: b -build-> a, c -install-> a, d -install-> c; only a dispatches.
	if got := jobSpecNames(t, client); len(got) != 1 || !got["a"] {
		t.Fatalf("jobs = %v, want only {a} (zero indegree)", got)
	}
	dcg := bi.Status.Dcg
	if _, ok := dcg["b"].InDep["a"]; !ok {
		t.Fatalf("dcg[b].InDep = %v, want edge on a", dcg["b"].InDep)
	}
	if _, ok := dcg["c"].InstallInDep["a"]; !ok {
		t.Fatalf("dcg[c].InstallInDep = %v, want edge on a", dcg["c"].InstallInDep)
	}
	if _, ok := dcg["d"].InstallInDep["c"]; !ok {
		t.Fatalf("dcg[d].InstallInDep = %v, want edge on c", dcg["d"].InstallInDep)
	}
	if len(dcg["a"].OutDep) != 2 {
		t.Fatalf("dcg[a].OutDep = %v, want [b c]", dcg["a"].OutDep)
	}
}

func TestInitIncrementalUsesParentSeeds(t *testing.T) {
	c, client, git, _ := newTestController(t)
	seedHealthyBasics(client, "incremental", "repo1", "repo2", "repo3")
	// The parent Build has already selected the seed repositories.
	client.SeedSnapshot(testSnapshotObj(
		repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1-new", declare: true},
		repoEntry{name: "repo2", cloneURL: gitURL2, commitID: "c2", declare: true},
		repoEntry{name: "repo3", cloneURL: gitURL3, commitID: "c3", declare: true}))
	client.SeedRpmRepo(testRpmRepoObj(""))
	git.repo(gitURL1, "c1-new", map[string]string{"a.spec": specText("a")})
	git.repo(gitURL2, "c2", map[string]string{"b.spec": specText("b")})
	git.repo(gitURL3, "c3", map[string]string{"c.spec": specText("c")})

	reconcileOnce(t, c)

	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoProcessing)
	requireSpecNames(t, bi, "a", "b", "c")
	if got := jobSpecNames(t, client); len(got) != 3 {
		t.Fatalf("jobs = %v, want {a,b,c} all dispatched", got)
	}
}

func TestInitIncrementalNoBaseCompletesEmpty(t *testing.T) {
	c, client, _, _ := newTestController(t)
	seedHealthyBasics(client, "incremental")
	client.SeedSnapshot(testSnapshotObj())
	client.SeedRpmRepo(testRpmRepoObj(""))

	reconcileOnce(t, c)

	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoCompleted)
	requireSpecNames(t, bi)
	if len(bi.Status.Conditions) != 0 {
		t.Fatalf("conditions = %v, want none for empty incremental seed set", bi.Status.Conditions)
	}
}

func TestInitSpecifiedEmptySetCompletes(t *testing.T) {
	c, client, git, _ := newTestController(t)
	seedHealthyBasics(client, "specified", "repo1")
	client.SeedSnapshot(testSnapshotObj(
		repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	client.SeedRpmRepo(testRpmRepoObj(""))
	// Repository ready but no root-level *.spec: natural empty produce.
	git.repo(gitURL1, "c1", map[string]string{})

	reconcileOnce(t, c)

	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoCompleted)
	if len(bi.Status.Conditions) != 0 {
		t.Fatalf("conditions = %v, want none for empty build set", bi.Status.Conditions)
	}
	requireSpecNames(t, bi)
	if got := len(listJobs(t, client)); got != 0 {
		t.Fatalf("jobs = %d, want 0", got)
	}
}

func TestInitIncrementalAndSpecifiedRepoMissingDegrades(t *testing.T) {
	for _, buildType := range []string{"incremental", "specified"} {
		t.Run(buildType, func(t *testing.T) {
			c, client, git, _ := newTestController(t)
			// A missing seed is recorded but does not block the remaining build set.
			seedHealthyBasics(client, buildType, "repo1", "ghost")
			client.SeedSnapshot(testSnapshotObj(
				repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
			client.SeedRpmRepo(testRpmRepoObj(""))
			git.repo(gitURL1, "c1", map[string]string{"a.spec": specText("a")})

			reconcileOnce(t, c)

			bi := getBuildInfo(t, client)
			requirePhase(t, bi, ebsv1.BuildInfoProcessing)
			cond := requireCondition(t, bi.Status.Conditions, ConditionSpecCommitMissing, ReasonSpecCommitMissing)
			if !strings.Contains(cond.Message, "ghost") {
				t.Fatalf("condition message = %q, want the missing repo named", cond.Message)
			}
			requireSpecNames(t, bi, "a")
			if got := bi.Status.FailedPackages; len(got) != 1 || got[0] != "ghost" {
				t.Fatalf("failedPackages = %v, want [ghost]", got)
			}
			if got := len(listJobs(t, client)); got != 1 {
				t.Fatalf("jobs = %d, want 1", got)
			}
		})
	}
}

func TestInitE24NonRetryableDegrades(t *testing.T) {
	c, client, git, _ := newTestController(t)
	seedHealthyBasics(client, "full")
	snapshot := testSnapshotObj(
		repoEntry{name: "repo2", cloneURL: gitURL2, commitID: "c2", declare: true})
	snapshot.Spec.PackageRepos = append(snapshot.Spec.PackageRepos, ebsv1.PackageRepo{Name: "repo1", URL: gitURL1})
	snapshot.Status.PackageRepoStatuses["repo1"] = ebsv1.PackageRepoStatus{
		CloneURL: gitURL1, Error: &ebsv1.SpecCommitError{Message: "gone", Retryable: false}}
	client.SeedSnapshot(snapshot)
	client.SeedRpmRepo(testRpmRepoObj(""))
	git.repo(gitURL2, "c2", map[string]string{"b.spec": specText("b")})

	reconcileOnce(t, c)

	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoProcessing)
	cond := requireCondition(t, bi.Status.Conditions, ConditionSpecCommitMissing, ReasonSpecCommitMissing)
	if !strings.Contains(cond.Message, "repo1 (gone)") {
		t.Fatalf("condition message = %q, want repo1 (gone)", cond.Message)
	}
	requireSpecNames(t, bi, "b")
	if got := jobSpecNames(t, client); len(got) != 1 || !got["b"] {
		t.Fatalf("jobs = %v, want {b}", got)
	}
}

func TestInitE24RetryableStaysPending(t *testing.T) {
	c, client, _, _ := newTestController(t)
	seedHealthyBasics(client, "full")
	snapshot := testSnapshotObj()
	snapshot.Spec.PackageRepos = append(snapshot.Spec.PackageRepos, ebsv1.PackageRepo{Name: "repo1", URL: gitURL1})
	snapshot.Status.PackageRepoStatuses["repo1"] = ebsv1.PackageRepoStatus{
		CloneURL: gitURL1, Error: &ebsv1.SpecCommitError{Message: "resolving", Retryable: true}}
	client.SeedSnapshot(snapshot)
	client.SeedRpmRepo(testRpmRepoObj(""))

	reconcileOnce(t, c)

	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoPending)
	if len(bi.Status.Conditions) != 0 {
		t.Fatalf("conditions = %v, want none (defensive transient)", bi.Status.Conditions)
	}
	if got := len(listJobs(t, client)); got != 0 {
		t.Fatalf("jobs = %d, want 0", got)
	}
}

func TestInitE23ParseFailureSkipsSpec(t *testing.T) {
	c, client, git, _ := newTestController(t)
	seedHealthyBasics(client, "full")
	client.SeedSnapshot(testSnapshotObj(
		repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	client.SeedRpmRepo(testRpmRepoObj(""))
	git.repo(gitURL1, "c1", map[string]string{"a.spec": specText("a"), "bad.spec": "ignored"})
	git.fail(gitURL1, "git-show c1:bad.spec", gitserver.ErrorPermanent, errors.New("blob missing"))

	reconcileOnce(t, c)

	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoProcessing)
	cond := requireCondition(t, bi.Status.Conditions, ConditionSpecDependsFillFailed, ReasonSpecParseFailed)
	if !strings.Contains(cond.Message, "repo1/bad.spec") {
		t.Fatalf("condition message = %q, want repo1/bad.spec", cond.Message)
	}
	requireSpecNames(t, bi, "a")
	if got := bi.Status.FailedPackages; len(got) != 1 || got[0] != "repo1" {
		t.Fatalf("failedPackages = %v, want [repo1]", got)
	}
	if got := jobSpecNames(t, client); len(got) != 1 || !got["a"] {
		t.Fatalf("jobs = %v, want {a}", got)
	}
}

func TestInitE23SpecifiedParseDegrades(t *testing.T) {
	c, client, git, _ := newTestController(t)
	seedHealthyBasics(client, "specified", "repo1")
	client.SeedSnapshot(testSnapshotObj(
		repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	client.SeedRpmRepo(testRpmRepoObj(""))
	git.repo(gitURL1, "c1", map[string]string{"a.spec": specText("a"), "bad.spec": "ignored"})
	git.fail(gitURL1, "git-show c1:bad.spec", gitserver.ErrorPermanent, errors.New("blob missing"))

	reconcileOnce(t, c)

	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoProcessing)
	requireCondition(t, bi.Status.Conditions, ConditionSpecDependsFillFailed, ReasonSpecParseFailed)
	requireSpecNames(t, bi, "a")
	if got := bi.Status.FailedPackages; len(got) != 1 || got[0] != "repo1" {
		t.Fatalf("failedPackages = %v, want [repo1]", got)
	}
	if got := jobSpecNames(t, client); len(got) != 1 || !got["a"] {
		t.Fatalf("jobs = %v, want {a}", got)
	}
}

func TestInitE23TransientStaysPending(t *testing.T) {
	c, client, git, _ := newTestController(t)
	seedHealthyBasics(client, "full")
	client.SeedSnapshot(testSnapshotObj(
		repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	client.SeedRpmRepo(testRpmRepoObj(""))
	git.fail(gitURL1, "git-ls-tree --name-only c1", gitserver.ErrorTemporary, errors.New("timeout"))

	reconcileOnce(t, c)

	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoPending)
	if len(bi.Status.Conditions) != 0 {
		t.Fatalf("conditions = %v, want none (transient assembly gap)", bi.Status.Conditions)
	}
	if got := len(listJobs(t, client)); got != 0 {
		t.Fatalf("jobs = %d, want 0", got)
	}
}

func TestInitE16RpmRepoNotHeldDefers(t *testing.T) {
	c, client, git, _ := newTestController(t)
	seedHealthyBasics(client, "full")
	client.SeedSnapshot(testSnapshotObj(
		repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	git.repo(gitURL1, "c1", map[string]string{"a.spec": specText("a")})
	// No RpmRepo seeded: E-16 below the threshold — assembly completes but the
	// verdict/dispatch steps wait for the next round.

	reconcileOnce(t, c)

	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoPending)
	if len(bi.Status.Conditions) != 0 {
		t.Fatalf("conditions = %v, want none", bi.Status.Conditions)
	}
	if got := len(listJobs(t, client)); got != 0 {
		t.Fatalf("jobs = %d, want 0", got)
	}
}

func TestInitE19ArchUnsupported(t *testing.T) {
	c, client, git, _ := newTestController(t)
	seedHealthyBasics(client, "full")
	client.SeedSnapshot(testSnapshotObj(
		repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	client.SeedRpmRepo(testRpmRepoObj(""))
	armSpec := "Name: a\nVersion: 1.0\nRelease: 1\nSummary: a\nLicense: MIT\nExclusiveArch: aarch64\n\n%description\ntest\n"
	git.repo(gitURL1, "c1", map[string]string{"a.spec": armSpec, "b.spec": specText("b")})

	reconcileOnce(t, c)

	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoProcessing)
	ss := bi.Status.SpecStatus["a"]
	if ss.Build.Status != SpecBuildFailed {
		t.Fatalf("specStatus[a].Build.Status = %q, want Failed", ss.Build.Status)
	}
	requireCondition(t, ss.Build.Conditions, ConditionArchUnsupported, ReasonArchUnsupported)
	if got := jobSpecNames(t, client); len(got) != 1 || !got["b"] {
		t.Fatalf("jobs = %v, want {b} only", got)
	}
}

func TestInitE26ImageMappingMissingPauses(t *testing.T) {
	c, client, git, _ := newTestController(t)
	seedHealthyBasics(client, "full")
	// build-target Config without the os/arch mapping: E-26 pauses the round (plain
	// error backoff, no Failed marking, no condition).
	client.SetBuildTargetContent(&ebsv1.BuildTargetContent{
		Targets: map[string]ebsv1.BuildTargetConfigEntry{},
	})
	client.SeedSnapshot(testSnapshotObj(
		repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	client.SeedRpmRepo(testRpmRepoObj(""))
	git.repo(gitURL1, "c1", map[string]string{"a.spec": specText("a")})

	if _, err := c.reconcile(context.Background(), testNS+"/"+testBuild); err == nil {
		t.Fatal("reconcile() error = nil, want build-target Config mapping failure")
	}
	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoPending)
	requireSpecNames(t, bi)
	if len(bi.Status.PendingJobCreates) != 0 {
		t.Fatalf("pendingJobCreates = %v, want empty (image resolves before registration)", bi.Status.PendingJobCreates)
	}
	if got := len(listJobs(t, client)); got != 0 {
		t.Fatalf("jobs = %d, want 0", got)
	}
}

func TestInitE27BuildResourceConfigMissing(t *testing.T) {
	c, client, git, _ := newTestController(t)
	// Seed everything except any BuildResourceConfig (project table and the
	// cluster-wide default resource table is absent).
	client.SeedProject(testProjectObj(ebsv1.ProjectActive))
	client.SeedBuild(testBuildObj("full"))
	client.SetBuildTargetContent(testBuildTargetContent())
	client.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoPending))
	client.SeedSnapshot(testSnapshotObj(
		repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	client.SeedRpmRepo(testRpmRepoObj(""))
	git.repo(gitURL1, "c1", map[string]string{"a.spec": specText("a")})

	reconcileOnce(t, c)

	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoProcessing)
	ss := bi.Status.SpecStatus["a"]
	if ss.Build.Status != SpecBuildFailed {
		t.Fatalf("specStatus[a].Build.Status = %q, want Failed", ss.Build.Status)
	}
	requireCondition(t, ss.Build.Conditions, ConditionDefaultBuildResourceConfigNotFound, ReasonDefaultBuildResourceConfigNotFound)
	if len(bi.Status.PendingJobCreates) != 0 {
		t.Fatalf("pendingJobCreates = %v, want empty (this-round registration removed)", bi.Status.PendingJobCreates)
	}
	if got := len(listJobs(t, client)); got != 0 {
		t.Fatalf("jobs = %d, want 0", got)
	}
}

func TestInitBackfillsExistingJob(t *testing.T) {
	c, client, git, _ := newTestController(t)
	bi := seedHealthyBasics(client, "full")
	client.SeedSnapshot(testSnapshotObj(
		repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	client.SeedRpmRepo(testRpmRepoObj(""))
	git.repo(gitURL1, "c1", map[string]string{"a.spec": specText("a")})
	// E-11: the Job was created but the dispatch write-back was lost.
	existing := client.SeedJob(testJobObj(bi, "a", 1, ebsv1.JobRunning))

	reconcileOnce(t, c)

	bi = getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoProcessing)
	ss := bi.Status.SpecStatus["a"]
	if ss.DispatchCount != 1 || ss.Build.Status != SpecBuildRunning || ss.Build.JobName != existing.Name {
		t.Fatalf("specStatus[a] = %+v, want backfilled gen-1 Running (%s)", ss, existing.Name)
	}
	if got := len(listJobs(t, client)); got != 1 {
		t.Fatalf("jobs = %d, want 1 (no re-creation)", got)
	}
}

// --- 7.2.3 single直通 ---

func TestSinglePassThrough(t *testing.T) {
	c, client, git, _ := newTestController(t)
	seedHealthyBasics(client, "single", "repo1", "repo2")
	client.SeedSnapshot(testSnapshotObj(
		repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true},
		repoEntry{name: "repo2", cloneURL: gitURL2, commitID: "c2", declare: true}))
	git.repo(gitURL1, "c1", map[string]string{"a.spec": specText("a")})
	// An unavailable BuildRequires does not gate single (no condition-2 check).
	git.repo(gitURL2, "c2", map[string]string{"b.spec": specText("b", "missing-dep")})

	reconcileOnce(t, c)

	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoProcessing)
	requireSpecNames(t, bi, "a", "b")
	for _, name := range []string{"a", "b"} {
		ss := bi.Status.SpecStatus[name]
		if ss.DispatchCount != 1 || ss.Build.JobName == "" {
			t.Fatalf("specStatus[%s] = %+v, want dispatched (gen 1, job named)", name, ss)
		}
	}
	if got := jobSpecNames(t, client); len(got) != 2 {
		t.Fatalf("jobs = %v, want {a,b} (直通无顺序)", got)
	}
	if len(bi.Status.Dcg) != 0 {
		t.Fatalf("status.dcg = %v, want empty for single", bi.Status.Dcg)
	}
	if got := c.dcgDict.Len(); got != 0 {
		t.Fatalf("dcgDict len = %d, want 0 for single", got)
	}
	if len(bi.Status.Conditions) != 0 {
		t.Fatalf("conditions = %v, want none", bi.Status.Conditions)
	}
	for _, job := range listJobs(t, client) {
		if job.Labels[ebsv1.JobSpecNameLabel] != "b" {
			continue
		}
		if !strings.Contains(job.Spec.Payload, "spec_name: b") || !strings.Contains(job.Spec.Payload, "commitId: c2") {
			t.Fatalf("payload = %q, want spec_name/commitId injected", job.Spec.Payload)
		}
		if strings.Contains(job.Spec.Payload, "Repo: ") {
			t.Fatalf("payload = %q, want no Repo injection (404 RpmRepo, no bootstrap)", job.Spec.Payload)
		}
	}
}

func TestSingleRepoInjection(t *testing.T) {
	c, client, git, _ := newTestController(t)
	client.SeedProject(testProjectObj(ebsv1.ProjectActive))
	client.SeedBuild(testBuildObj("single", "repo1"))
	client.SeedBuildResourceRules(testBuildResourceRules())
	client.SetBuildTargetContent(testBuildTargetContent())
	bi := testBuildInfoObj(ebsv1.BuildInfoPending)
	bi.Spec.BootstrapRepo = []ebsv1.BootstrapRepo{{Name: "base", Repo: "http://bootstrap.local/base"}}
	client.SeedBuildInfo(bi)
	client.SeedSnapshot(testSnapshotObj(
		repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	// Same-name RpmRepo contentURL is the creation-time inherited baseline.
	client.SeedRpmRepo(testRpmRepoObj(testRepoURL))
	git.repo(gitURL1, "c1", map[string]string{"a.spec": specText("a")})

	reconcileOnce(t, c)

	jobs := listJobs(t, client)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	payload := jobs[0].Spec.Payload
	if !strings.Contains(payload, "Repo: "+testRepoURL+" http://bootstrap.local/base") {
		t.Fatalf("payload = %q, want contentURL first + bootstrap repo", payload)
	}
	if !strings.Contains(payload, "repo_priority: 10 10") {
		t.Fatalf("payload = %q, want repo_priority aligned (10 10)", payload)
	}
}

func TestSingleEmptyPackagesCloseout(t *testing.T) {
	c, client, _, _ := newTestController(t)
	seedHealthyBasics(client, "single")

	reconcileOnce(t, c)

	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoCompleted)
	requireCondition(t, bi.Status.Conditions, ConditionSpecDependsFillFailed, ReasonSpecifiedBuildSetEmpty)
	requireSpecNames(t, bi)
}

func TestSingleDegradedPerPackage(t *testing.T) {
	c, client, git, _ := newTestController(t)
	seedHealthyBasics(client, "single", "repo1", "repo2", "ghost")
	snapshot := testSnapshotObj(
		repoEntry{name: "repo2", cloneURL: gitURL2, commitID: "c2", declare: true})
	snapshot.Spec.PackageRepos = append(snapshot.Spec.PackageRepos, ebsv1.PackageRepo{Name: "repo1", URL: gitURL1})
	snapshot.Status.PackageRepoStatuses["repo1"] = ebsv1.PackageRepoStatus{
		CloneURL: gitURL1, Error: &ebsv1.SpecCommitError{Message: "gone", Retryable: false}}
	client.SeedSnapshot(snapshot)
	git.repo(gitURL2, "c2", map[string]string{"b.spec": specText("b")})

	reconcileOnce(t, c)

	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoProcessing)
	cond := requireCondition(t, bi.Status.Conditions, ConditionSpecCommitMissing, ReasonSpecCommitMissing)
	if !strings.Contains(cond.Message, "repo1 (gone)") || !strings.Contains(cond.Message, "ghost (not in packageRepos)") {
		t.Fatalf("condition message = %q, want both skipped repos", cond.Message)
	}
	requireSpecNames(t, bi, "b")
	if got := jobSpecNames(t, client); len(got) != 1 || !got["b"] {
		t.Fatalf("jobs = %v, want {b}", got)
	}
}

func TestSingleAllSkippedCloseout(t *testing.T) {
	c, client, _, _ := newTestController(t)
	seedHealthyBasics(client, "single", "repo1")
	snapshot := testSnapshotObj()
	snapshot.Spec.PackageRepos = append(snapshot.Spec.PackageRepos, ebsv1.PackageRepo{Name: "repo1", URL: gitURL1})
	snapshot.Status.PackageRepoStatuses["repo1"] = ebsv1.PackageRepoStatus{
		CloneURL: gitURL1, Error: &ebsv1.SpecCommitError{Message: "gone", Retryable: false}}
	client.SeedSnapshot(snapshot)

	reconcileOnce(t, c)

	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoCompleted)
	requireCondition(t, bi.Status.Conditions, ConditionSpecDependsFillFailed, ReasonSpecifiedBuildSetEmpty)
	requireCondition(t, bi.Status.Conditions, ConditionSpecCommitMissing, ReasonSpecCommitMissing)
	requireSpecNames(t, bi)
	if got := len(listJobs(t, client)); got != 0 {
		t.Fatalf("jobs = %d, want 0", got)
	}
}

func TestSingleDeterministicFailures(t *testing.T) {
	t.Run("E-19 arch unsupported marks Failed", func(t *testing.T) {
		c, client, git, _ := newTestController(t)
		seedHealthyBasics(client, "single", "repo1")
		client.SeedSnapshot(testSnapshotObj(
			repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
		armSpec := "Name: a\nVersion: 1.0\nRelease: 1\nSummary: a\nLicense: MIT\nExclusiveArch: aarch64\n\n%description\ntest\n"
		git.repo(gitURL1, "c1", map[string]string{"a.spec": armSpec})

		reconcileOnce(t, c)

		bi := getBuildInfo(t, client)
		requirePhase(t, bi, ebsv1.BuildInfoProcessing)
		ss := bi.Status.SpecStatus["a"]
		if ss.Build.Status != SpecBuildFailed {
			t.Fatalf("specStatus[a].Build.Status = %q, want Failed", ss.Build.Status)
		}
		requireCondition(t, ss.Build.Conditions, ConditionArchUnsupported, ReasonArchUnsupported)
		if got := len(listJobs(t, client)); got != 0 {
			t.Fatalf("jobs = %d, want 0", got)
		}
	})

	t.Run("E-27 BuildResourceConfig missing marks Failed", func(t *testing.T) {
		c, client, git, _ := newTestController(t)
		client.SeedProject(testProjectObj(ebsv1.ProjectActive))
		client.SeedBuild(testBuildObj("single", "repo1"))
		client.SetBuildTargetContent(testBuildTargetContent())
		client.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoPending))
		client.SeedSnapshot(testSnapshotObj(
			repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
		git.repo(gitURL1, "c1", map[string]string{"a.spec": specText("a")})

		reconcileOnce(t, c)

		bi := getBuildInfo(t, client)
		requirePhase(t, bi, ebsv1.BuildInfoProcessing)
		ss := bi.Status.SpecStatus["a"]
		if ss.Build.Status != SpecBuildFailed {
			t.Fatalf("specStatus[a].Build.Status = %q, want Failed", ss.Build.Status)
		}
		requireCondition(t, ss.Build.Conditions, ConditionDefaultBuildResourceConfigNotFound, ReasonDefaultBuildResourceConfigNotFound)
	})
}

func TestSingleE26Pauses(t *testing.T) {
	c, client, git, _ := newTestController(t)
	seedHealthyBasics(client, "single", "repo1")
	client.SetBuildTargetContent(&ebsv1.BuildTargetContent{
		Targets: map[string]ebsv1.BuildTargetConfigEntry{},
	})
	client.SeedSnapshot(testSnapshotObj(
		repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	git.repo(gitURL1, "c1", map[string]string{"a.spec": specText("a")})

	if _, err := c.reconcile(context.Background(), testNS+"/"+testBuild); err == nil {
		t.Fatal("reconcile() error = nil, want build-target Config mapping failure (E-26 pause)")
	}
	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoPending)
	if got := len(listJobs(t, client)); got != 0 {
		t.Fatalf("jobs = %d, want 0", got)
	}
}
