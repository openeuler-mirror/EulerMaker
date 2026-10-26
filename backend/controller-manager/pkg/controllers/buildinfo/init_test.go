package buildinfo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"controller-manager/pkg/clients/gitserver"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/controllers/buildinfo/specparse"
	"controller-manager/pkg/controllers/specname"
	"controller-manager/pkg/serviceconfig"
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
		name, ok := specname.Decode(job.Labels[ebsv1.JobSpecNameLabel])
		if !ok {
			t.Fatalf("invalid spec-name label %q", job.Labels[ebsv1.JobSpecNameLabel])
		}
		names[name] = true
	}
	return names
}

func requireSpecNames(t *testing.T, bi *ebsv1.BuildInfo, want ...string) {
	t.Helper()
	if len(bi.Status.SpecResults.Entries()) != len(want) {
		t.Fatalf("specResults size = %d, want %d (%v)", len(bi.Status.SpecResults.Entries()), len(want), bi.Status.SpecResults)
	}
	for _, name := range want {
		if _, ok := bi.Status.SpecResults.Lookup(name); !ok {
			t.Fatalf("specResults[%s] missing, have %v", name, bi.Status.SpecResults)
		}
	}
}

// --- Build-set determination ---

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
		ss := bi.Status.SpecResults.Entry(name)
		// A freshly created Job carries no phase yet, so Build.Status stays empty until the first backfill maps
		// Pending/Running.
		if ss.DispatchCount != 1 {
			t.Fatalf("specResults[%s] = %+v, want dispatched (gen 1)", name, ss)
		}
	}
	if got := jobSpecNames(t, client); len(got) != 2 || !got["a"] || !got["b"] {
		t.Fatalf("jobs = %v, want {a,b}", got)
	}
	if graph := testGraph(t, client, bi); len(graph.Status.Nodes) != 2 {
		t.Fatalf("graph nodes size = %d, want 2", len(graph.Status.Nodes))
	}
	for spec, want := range map[string]string{"a": "repo1", "b": "repo2"} {
		if got := testSpecRepo(bi.Status.SpecResults, spec); got != want {
			t.Fatalf("source repository[%q] = %q, want %q", spec, got, want)
		}
	}
	if got := c.dcgDict.Len(); got != 1 {
		t.Fatalf("dcgDict len = %d, want 1", got)
	}
	if len(bi.Status.Conditions) != 0 {
		t.Fatalf("conditions = %v, want none", bi.Status.Conditions)
	}
}

func TestServiceConfigFailureDoesNotFailPackages(t *testing.T) {
	c, client, git, _ := newTestController(t)
	seedHealthyBasics(client, "single", "repo1")
	client.SeedSnapshot(testSnapshotObj(repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	git.repo(gitURL1, "c1", map[string]string{"a.spec": specText("a")})
	c.services.(*fakeServices).err = &serviceconfig.Error{Err: context.DeadlineExceeded}
	for i := 0; i < 6; i++ {
		if _, err := c.reconcile(context.Background(), testNS+"/"+testBuild); !serviceconfig.IsError(err) {
			t.Fatalf("expected configuration error: %v", err)
		}
		bi := getBuildInfo(t, client)
		if bi.Status.Phase != ebsv1.BuildInfoPending || len(bi.Status.SpecResults.FailedPackages()) != 0 || len(listJobs(t, client)) != 0 {
			t.Fatalf("configuration failure changed business outcome: %+v", bi.Status)
		}
	}
	c.services.(*fakeServices).err = nil
	reconcileOnce(t, c)
	if len(listJobs(t, client)) != 1 {
		t.Fatal("dispatch did not recover")
	}
}

func TestInitDispatchesAtMostTwentyJobsPerReconcile(t *testing.T) {
	c, client, git, _ := newTestController(t)
	seedHealthyBasics(client, "full")
	client.SeedSnapshot(testSnapshotObj(repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	client.SeedRpmRepo(testRpmRepoObj(""))
	files := make(map[string]string, maxJobCreatesPerReconcile+1)
	for i := 0; i <= maxJobCreatesPerReconcile; i++ {
		name := fmt.Sprintf("pkg%02d", i)
		files[name+".spec"] = specText(name)
	}
	git.repo(gitURL1, "c1", files)

	result, err := c.reconcile(context.Background(), testNS+"/"+testBuild)
	if err != nil || !result.Requeue {
		t.Fatalf("first reconcile = %+v, %v, want immediate requeue", result, err)
	}
	if got := len(listJobs(t, client)); got != maxJobCreatesPerReconcile {
		t.Fatalf("first reconcile created %d Jobs, want %d", got, maxJobCreatesPerReconcile)
	}
	if phase := getBuildInfo(t, client).Status.Phase; phase != ebsv1.BuildInfoPending {
		t.Fatalf("phase after partial dispatch = %s, want Pending", phase)
	}
	if got := getBuildInfo(t, client).Status.SpecResults.Len(); got != maxJobCreatesPerReconcile+1 {
		t.Fatalf("first reconcile persisted %d spec entries, want %d", got, maxJobCreatesPerReconcile+1)
	}
	if got := client.statusWrites; got > 5 {
		t.Fatalf(
			"first reconcile wrote status %d times for %d Jobs, want batched confirmation",
			got,
			maxJobCreatesPerReconcile,
		)
	}
	for i := 0; i < maxJobCreatesPerReconcile; i++ {
		name := fmt.Sprintf("pkg%02d", i)
		if got := getBuildInfo(t, client).Status.SpecResults.Entry(name).DispatchCount; got != 1 {
			t.Fatalf("spec %s dispatch count = %d, want 1 after batch flush", name, got)
		}
	}
	if ss := getBuildInfo(t, client).Status.SpecResults.Entry("pkg20"); ss.DispatchCount != 0 || ss.Status != ebsv1.SpecPrepared {
		t.Fatalf("not-yet-dispatched spec status = %+v, want an empty entry", ss)
	}

	result, err = c.reconcile(context.Background(), testNS+"/"+testBuild)
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("second reconcile = %+v, %v", result, err)
	}
	if got := len(listJobs(t, client)); got != maxJobCreatesPerReconcile+1 {
		t.Fatalf("second reconcile has %d Jobs, want %d", got, maxJobCreatesPerReconcile+1)
	}
	if phase := getBuildInfo(t, client).Status.Phase; phase != ebsv1.BuildInfoProcessing {
		t.Fatalf("phase after complete dispatch = %s, want Processing", phase)
	}
}

func TestSingleInitPersistsAllSpecsBeforeBatchedDispatch(t *testing.T) {
	c, client, git, _ := newTestController(t)
	seedHealthyBasics(client, "single", "repo1")
	client.SeedSnapshot(testSnapshotObj(repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	client.SeedRpmRepo(testRpmRepoObj(""))
	files := make(map[string]string, maxJobCreatesPerReconcile+1)
	for i := 0; i <= maxJobCreatesPerReconcile; i++ {
		name := fmt.Sprintf("pkg%02d", i)
		files[name+".spec"] = specText(name)
	}
	git.repo(gitURL1, "c1", files)

	result, err := c.reconcile(context.Background(), testNS+"/"+testBuild)
	if err != nil || !result.Requeue {
		t.Fatalf("first reconcile = %+v, %v, want immediate requeue", result, err)
	}
	if got := len(listJobs(t, client)); got != maxJobCreatesPerReconcile {
		t.Fatalf("first reconcile created %d Jobs, want %d", got, maxJobCreatesPerReconcile)
	}
	if bi := getBuildInfo(t, client); bi.Status.Phase != ebsv1.BuildInfoPending ||
		bi.Status.SpecResults.Len() != maxJobCreatesPerReconcile+1 {
		t.Fatalf("partial single status = %+v, want Pending with all spec entries", bi.Status)
	}

	result, err = c.reconcile(context.Background(), testNS+"/"+testBuild)
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("second reconcile = %+v, %v", result, err)
	}
	if got := len(listJobs(t, client)); got != maxJobCreatesPerReconcile+1 {
		t.Fatalf("second reconcile has %d Jobs, want %d", got, maxJobCreatesPerReconcile+1)
	}
	if phase := getBuildInfo(t, client).Status.Phase; phase != ebsv1.BuildInfoProcessing {
		t.Fatalf("phase after complete single dispatch = %s, want Processing", phase)
	}
}

func TestMissingBuildDependenciesConvergeWithoutJobs(t *testing.T) {
	c, client, git, _ := newTestController(t)
	seedHealthyBasics(client, "full")
	client.SeedSnapshot(testSnapshotObj(repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	client.SeedRpmRepo(testRpmRepoObj(""))
	git.repo(gitURL1, "c1", map[string]string{
		"a.spec": specText("a", "missing-dependency"),
		"b.spec": specText("b", "a"),
	})
	for range 3 {
		reconcileOnce(t, c)
	}
	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoCompleted)
	for _, spec := range []string{"a", "b"} {
		result := bi.Status.SpecResults.Entry(spec)
		if result.Status != ebsv1.SpecRpmDependsMissing || result.DispatchCount != 0 ||
			!strings.Contains(result.Message, "build requires unavailable:") {
			t.Fatalf("spec %s result = %+v", spec, result)
		}
	}
	if got := len(listJobs(t, client)); got != 0 {
		t.Fatalf("Jobs = %d, want 0", got)
	}
	if got := bi.Status.SpecResults.FailedPackages(); len(got) != 1 || got[0] != "repo1" {
		t.Fatalf("failed packages = %v", got)
	}
}

func TestInitUnparsableSpecMatchesSpecName(t *testing.T) {
	c, client, git, _ := newTestController(t)
	bi := seedHealthyBasics(client, "full")
	bi.Spec.BuildPayload = "unparsable_spec:\n- a\n- repo1\n"
	client.SeedBuildInfo(bi)
	client.SeedSnapshot(testSnapshotObj(repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	client.SeedRpmRepo(testRpmRepoObj(""))
	git.repo(gitURL1, "c1", map[string]string{
		"a.spec": specText("a", "missing-build-dependency"),
		"b.spec": specText("b", "a"),
	})

	reconcileOnce(t, c)

	got := getBuildInfo(t, client)
	requirePhase(t, got, ebsv1.BuildInfoProcessing)
	requireSpecNames(t, got, "a", "b")
	if jobs := jobSpecNames(t, client); len(jobs) != 1 || !jobs["a"] {
		t.Fatalf("jobs = %v, want only a; b must retain its build dependency", jobs)
	}
	view, ok := c.specDependsCache.Get(testNS + "/" + testBuild)
	if !ok || len(view["a"].BuildRequires) != 0 || len(view["b"].BuildRequires) != 1 {
		t.Fatalf("cached depends = %v, want only a's BuildRequires cleared", view)
	}
	raw, ok := c.specFiles.Get("c1", "a.spec")
	if !ok || !strings.Contains(raw, "missing-build-dependency") {
		t.Fatalf("raw spec cache = %q (found=%t), want original BuildRequires", raw, ok)
	}
	if payload := listJobs(t, client)[0].Spec.Payload; strings.Contains(payload, "unparsable_spec") {
		t.Fatalf("Job payload leaked controller-only field: %s", payload)
	}
}

func TestUnparsableSpecConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		clearA  bool
	}{
		{name: "missing key"},
		{name: "empty list", payload: "unparsable_spec: []\n"},
		{name: "non-list", payload: "unparsable_spec: a\n"},
		{name: "mixed list", payload: "unparsable_spec: [42, a]\n", clearA: true},
		{name: "unknown spec", payload: "unparsable_spec: [other]\n"},
		{name: "case sensitive", payload: "unparsable_spec: [A]\n"},
		{name: "repository name is not spec name", payload: "unparsable_spec: [repo1]\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _, _, _ := newTestController(t)
			bi := testBuildInfoObj(ebsv1.BuildInfoPending)
			bi.Spec.BuildPayload = tt.payload
			round := &reconcileRound{key: testNS + "/" + testBuild, current: bi}
			a := dependEntry("a")
			a.BuildRequires = map[string]ebsv1.VersionConstraint{"dependency": {}}
			b := dependEntry("b")
			b.BuildRequires = map[string]ebsv1.VersionConstraint{"dependency": {}}
			depends := map[string]specparse.SpecDepend{"a": a, "b": b}

			c.ignoreBuildRequires(round, depends)

			if cleared := len(depends["a"].BuildRequires) == 0; cleared != tt.clearA {
				t.Fatalf("a BuildRequires cleared=%t, want %t", cleared, tt.clearA)
			}
			if len(depends["b"].BuildRequires) != 1 {
				t.Fatalf("b BuildRequires = %v, want unchanged", depends["b"].BuildRequires)
			}
		})
	}
}

func TestInitGitCommandsUseOriginURL(t *testing.T) {
	const originURL = "https://gitee.com/src-openeuler/repo1.git"
	const cloneURL = "http://git-server:8080/git/gitee.com/src-openeuler/repo1.git"
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

func TestInitSpecifiedIncludesOnlyDirectDependents(t *testing.T) {
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
	// Pre-populated repo layer (URL match -> no download): b joins the build set via the buildRequires reverse lookup and
	// c via install requires. d only depends on c, so it is not a direct dependent of the seed a.
	c.rpmMetaSources.Set(key, testSources(
		testRpm("a", "a", "1.0"),
		testRpm("b", "b", "1.0"),
		testRpm("c", "c", "1.0", "a"),
		testRpm("d", "d", "1.0", "c")))

	reconcileOnce(t, c)

	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoProcessing)
	requireSpecNames(t, bi, "a", "b", "c")
	// Edges: b -build-> a, c -install-> a; only a dispatches.
	if got := jobSpecNames(t, client); len(got) != 1 || !got["a"] {
		t.Fatalf("jobs = %v, want only {a} (zero indegree)", got)
	}
	dcg := testGraph(t, client, bi).Status.Nodes
	if _, ok := dcg["b"].InDep["a"]; !ok {
		t.Fatalf("dcg[b].InDep = %v, want edge on a", dcg["b"].InDep)
	}
	if _, ok := dcg["c"].InstallInDep["a"]; !ok {
		t.Fatalf("dcg[c].InstallInDep = %v, want edge on a", dcg["c"].InstallInDep)
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

func TestInitIncrementalIncludesOnlyDirectDependents(t *testing.T) {
	c, client, git, _ := newTestController(t)
	seedHealthyBasics(client, "incremental", "repo1")
	client.SeedSnapshot(testSnapshotObj(
		repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true},
		repoEntry{name: "repo2", cloneURL: gitURL2, commitID: "c2", declare: true},
		repoEntry{name: "repo3", cloneURL: gitURL3, commitID: "c3", declare: true}))
	client.SeedRpmRepo(testRpmRepoObj(""))
	git.repo(gitURL1, "c1", map[string]string{"a.spec": specText("a")})
	git.repo(gitURL2, "c2", map[string]string{"b.spec": specText("b", "a")})
	git.repo(gitURL3, "c3", map[string]string{"c.spec": specText("c", "b")})

	reconcileOnce(t, c)

	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoProcessing)
	requireSpecNames(t, bi, "a", "b")
	if got := jobSpecNames(t, client); len(got) != 1 || !got["a"] {
		t.Fatalf("jobs = %v, want only {a}", got)
	}
}

func TestExpandBuildSetUsesOnlyOriginalSeeds(t *testing.T) {
	full := map[string]specparse.SpecDepend{
		"a": {SpecName: "a", Provides: []string{"virtual-a"}},
		"b": {SpecName: "b", BuildRequires: map[string]ebsv1.VersionConstraint{"virtual-a": {}}},
		"c": {SpecName: "c", BuildRequires: map[string]ebsv1.VersionConstraint{"b": {}}},
	}

	got := expandBuildSet(map[string]specparse.SpecDepend{"a": full["a"]}, full, nil)
	if len(got) != 2 || got["a"].SpecName != "a" || got["b"].SpecName != "b" {
		t.Fatalf("build set = %v, want only a and b", got)
	}

	got = expandBuildSet(map[string]specparse.SpecDepend{"a": full["a"], "b": full["b"]}, full, nil)
	if len(got) != 3 || got["c"].SpecName != "c" {
		t.Fatalf("build set with two seeds = %v, want a, b and c", got)
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
	// A repository without root specs is recorded as a preparation failure.
	git.repo(gitURL1, "c1", map[string]string{})

	reconcileOnce(t, c)

	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoCompleted)
	if result := bi.Status.SpecResults["repo1/"]; result.Status != ebsv1.SpecPrepareFailed || result.SpecName != "" {
		t.Fatalf("repository result = %+v, want unnamed PrepareFailed", result)
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
			if got := bi.Status.SpecResults.FailedPackages(); len(got) != 1 || got[0] != "ghost" {
				t.Fatalf("failedPackages = %v, want [ghost]", got)
			}
			if got := len(listJobs(t, client)); got != 1 {
				t.Fatalf("jobs = %d, want 1", got)
			}
		})
	}
}

func TestInitNonRetryableSnapshotRepoErrorDegrades(t *testing.T) {
	c, client, git, _ := newTestController(t)
	seedHealthyBasics(client, "full")
	snapshot := testSnapshotObj(
		repoEntry{name: "repo2", cloneURL: gitURL2, commitID: "c2", declare: true})
	snapshot.Spec.PackageRepos = append(snapshot.Spec.PackageRepos, ebsv1.PackageRepo{Name: "repo1", URL: gitURL1})
	snapshot.Status.PackageRepoStatuses["repo1"] = ebsv1.PackageRepoStatus{
		CloneURL: gitURL1, Error: &ebsv1.PackageRepoError{Message: "gone", Retryable: false}}
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

func TestInitRetryableSnapshotRepoErrorStaysPending(t *testing.T) {
	c, client, _, _ := newTestController(t)
	seedHealthyBasics(client, "full")
	snapshot := testSnapshotObj()
	snapshot.Spec.PackageRepos = append(snapshot.Spec.PackageRepos, ebsv1.PackageRepo{Name: "repo1", URL: gitURL1})
	snapshot.Status.PackageRepoStatuses["repo1"] = ebsv1.PackageRepoStatus{
		CloneURL: gitURL1, Error: &ebsv1.PackageRepoError{Message: "resolving", Retryable: true}}
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

func TestInitSpecParseFailuresStayInSpecResults(t *testing.T) {
	for _, buildType := range []string{"full", "single"} {
		for _, diagnostic := range []string{
			"No compatible architectures found for build",
			"line 19: Unknown tag: %systemd_requires",
			"lua script failed: attempt to index a nil value",
		} {
			t.Run(buildType+"/"+diagnostic, func(t *testing.T) {
				c, client, git, _ := newTestController(t)
				path, err := exec.LookPath("rpmspec")
				if err != nil {
					t.Fatal(err)
				}
				stub := fmt.Sprintf(`#!/bin/sh
case "$(cat "$3")" in
  *"Name: ubturbo"*)
    echo 'error: %s' >&2
    exit 1 ;;
esac
cat "$3"
`, diagnostic)
				if err := os.WriteFile(path, []byte(stub), 0o755); err != nil {
					t.Fatal(err)
				}
				seedHealthyBasics(client, buildType, "ubturbo")
				client.SeedSnapshot(testSnapshotObj(
					repoEntry{name: "ubturbo", cloneURL: gitURL1, commitID: "c1", declare: true}))
				client.SeedRpmRepo(testRpmRepoObj(""))
				git.repo(gitURL1, "c1", map[string]string{"ubturbo.spec": specText("ubturbo"), "a.spec": specText("a")})

				reconcileOnce(t, c)

				bi := getBuildInfo(t, client)
				requirePhase(t, bi, ebsv1.BuildInfoProcessing)
				result := bi.Status.SpecResults["ubturbo/ubturbo.spec"]
				if result.Status != ebsv1.SpecPrepareFailed || result.SpecName != "" {
					t.Fatalf("parse failure result = %+v", result)
				}
				if !strings.Contains(result.Message, "rpmspec ubturbo.spec: exit status 1: error: "+diagnostic) {
					t.Fatalf("spec diagnostic = %q", result.Message)
				}
				if findCondition(bi.Status.Conditions, ConditionSpecDependsFillFailed) != nil {
					t.Fatalf("spec diagnostic was aggregated: %+v", bi.Status.Conditions)
				}
				if jobs := jobSpecNames(t, client); len(jobs) != 1 || !jobs["a"] {
					t.Fatalf("jobs = %v, want only a", jobs)
				}
			})
		}
	}
}

func TestInitPermanentSpecReadFailureSkipsSpec(t *testing.T) {
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
	cond := bi.Status.SpecResults["repo1/bad.spec"]
	if !strings.Contains(cond.Message, "blob missing") {
		t.Fatalf("condition message = %q, want blob missing", cond.Message)
	}
	if findCondition(bi.Status.Conditions, ConditionSpecDependsFillFailed) != nil {
		t.Fatalf("spec diagnostic was aggregated: %+v", bi.Status.Conditions)
	}
	requireSpecNames(t, bi, "a")
	if got := bi.Status.SpecResults.FailedPackages(); len(got) != 1 || got[0] != "repo1" {
		t.Fatalf("failedPackages = %v, want [repo1]", got)
	}
	if got := jobSpecNames(t, client); len(got) != 1 || !got["a"] {
		t.Fatalf("jobs = %v, want {a}", got)
	}
}

func TestInitSpecifiedPermanentSpecReadFailureDegrades(t *testing.T) {
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
	requireSpecNames(t, bi, "a")
	if got := bi.Status.SpecResults.FailedPackages(); len(got) != 1 || got[0] != "repo1" {
		t.Fatalf("failedPackages = %v, want [repo1]", got)
	}
	if got := jobSpecNames(t, client); len(got) != 1 || !got["a"] {
		t.Fatalf("jobs = %v, want {a}", got)
	}
}

func TestInitTransientGitErrorStaysPending(t *testing.T) {
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

func TestInitMissingRpmRepoDefersDispatch(t *testing.T) {
	c, client, git, _ := newTestController(t)
	seedHealthyBasics(client, "full")
	client.SeedSnapshot(testSnapshotObj(
		repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	git.repo(gitURL1, "c1", map[string]string{"a.spec": specText("a")})
	// Without an RpmRepo, assembly completes but verdict and dispatch wait.

	reconcileOnce(t, c)

	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoPending)
	requireCondition(t, bi.Status.Conditions, ConditionRpmRepoRetrying, ReasonRpmRepoNotFound)
	requireNoCondition(t, bi.Status.Conditions, ConditionRpmRepoUnavailable)
	if got := len(listJobs(t, client)); got != 0 {
		t.Fatalf("jobs = %d, want 0", got)
	}
}

func TestInitArchUnsupportedSkipsJob(t *testing.T) {
	c, client, git, _ := newTestController(t)
	seedHealthyBasics(client, "full")
	client.SeedSnapshot(testSnapshotObj(
		repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	client.SeedRpmRepo(testRpmRepoObj(""))
	armSpec := "Name: a\nVersion: 1.0\nRelease: 1\nSummary: a\nLicense: MIT\n" +
		"ExclusiveArch: aarch64\n\n%description\ntest\n"
	git.repo(gitURL1, "c1", map[string]string{"a.spec": armSpec, "b.spec": specText("b")})

	reconcileOnce(t, c)

	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoProcessing)
	ss := bi.Status.SpecResults.Entry("a")
	if ss.Status != ebsv1.SpecArchUnsupported {
		t.Fatalf("specResults[a].Status = %q, want ArchUnsupported", ss.Status)
	}
	if got := jobSpecNames(t, client); len(got) != 1 || !got["b"] {
		t.Fatalf("jobs = %v, want {b} only", got)
	}
}

func TestInitMissingImageMappingPauses(t *testing.T) {
	c, client, git, _ := newTestController(t)
	seedHealthyBasics(client, "full")
	// A missing image mapping pauses dispatch without marking the spec Failed.
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
	requireSpecNames(t, bi, "a")
	if ss := bi.Status.SpecResults.Entry("a"); ss.Status != ebsv1.SpecPrepared || ss.DispatchCount != 0 {
		t.Fatalf("specResults[a] = %+v, want an undispatched entry", ss)
	}
	if got := len(listJobs(t, client)); got != 0 {
		t.Fatalf("jobs = %d, want 0", got)
	}
}

func TestMissingBuildResourceConfigPausesAndResumesDispatch(t *testing.T) {
	for _, buildType := range []string{"full", "single"} {
		t.Run(buildType, func(t *testing.T) {
			c, client, git, _ := newTestController(t)
			seedHealthyBasics(client, buildType, "repo1")
			client.SetBuildResourceContent(nil)
			client.SeedSnapshot(testSnapshotObj(
				repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
			client.SeedRpmRepo(testRpmRepoObj(""))
			git.repo(gitURL1, "c1", map[string]string{"a.spec": specText("a"), "b.spec": specText("b")})

			result, err := c.reconcile(context.Background(), testNS+"/"+testBuild)
			if err != nil || result.RequeueAfter != 5*time.Second {
				t.Fatalf("reconcile() = %+v, %v, want retry after 5s", result, err)
			}
			bi := getBuildInfo(t, client)
			requirePhase(t, bi, ebsv1.BuildInfoPending)
			requireCondition(t, bi.Status.Conditions, ConditionResourceConfigUnavailable, ReasonResourceConfigMissing)
			requireSpecNames(t, bi, "a", "b")
			for _, name := range []string{"a", "b"} {
				ss := bi.Status.SpecResults.Entry(name)
				if ss.Status != ebsv1.SpecPrepared || ss.DispatchCount != 0 || ss.Message != "" {
					t.Fatalf("specResults[%s] = %+v, want untouched Prepared entry", name, ss)
				}
			}
			if got := len(listJobs(t, client)); got != 0 {
				t.Fatalf("jobs = %d, want 0", got)
			}

			writes := client.statusWrites
			reconcileOnce(t, c)
			if client.statusWrites != writes {
				t.Fatalf("unchanged missing configuration wrote status again: %d -> %d", writes, client.statusWrites)
			}

			client.SetBuildResourceContent(testBuildResourceContent())
			reads := client.resourceReads
			reconcileOnce(t, c)
			bi = getBuildInfo(t, client)
			requirePhase(t, bi, ebsv1.BuildInfoProcessing)
			if findCondition(bi.Status.Conditions, ConditionResourceConfigUnavailable) != nil {
				t.Fatal("configuration recovery retained the pause condition")
			}
			if got := len(listJobs(t, client)); got != 2 {
				t.Fatalf("jobs = %d, want 2 after recovery", got)
			}
			if client.resourceReads != reads+1 {
				t.Fatalf("resource reads = %d, want %d for one dispatch round", client.resourceReads, reads+1)
			}
			for _, name := range []string{"a", "b"} {
				if ss := bi.Status.SpecResults.Entry(name); ss.DispatchCount != 1 {
					t.Fatalf("specResults[%s] = %+v, want first dispatch", name, ss)
				}
			}
		})
	}
}

func TestInitBackfillsExistingJob(t *testing.T) {
	c, client, git, _ := newTestController(t)
	bi := seedHealthyBasics(client, "full")
	client.SeedBuildGraph(bi, map[string]ebsv1.BuildGraphNode{"a": {Version: "1.0"}})
	client.SeedSnapshot(testSnapshotObj(
		repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	client.SeedRpmRepo(testRpmRepoObj(""))
	git.repo(gitURL1, "c1", map[string]string{"a.spec": specText("a")})
	// The Job was created but the dispatch write-back was lost.
	existing := client.SeedJob(testJobObj(bi, "a", 1, ebsv1.JobRunning))

	reconcileOnce(t, c)

	bi = getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoProcessing)
	ss := bi.Status.SpecResults.Entry("a")
	if ss.DispatchCount != 1 || ss.Status != ebsv1.SpecRunning {
		t.Fatalf("specResults[a] = %+v, want backfilled gen-1 Running (%s)", ss, existing.Name)
	}
	if got := len(listJobs(t, client)); got != 1 {
		t.Fatalf("jobs = %d, want 1 (no re-creation)", got)
	}
}

// --- Single-build dispatch ---

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
		ss := bi.Status.SpecResults.Entry(name)
		if ss.DispatchCount != 1 {
			t.Fatalf("specResults[%s] = %+v, want dispatched (gen 1)", name, ss)
		}
	}
	if got := jobSpecNames(t, client); len(got) != 2 {
		t.Fatalf("jobs = %v, want {a,b} (直通无顺序)", got)
	}
	if len(client.graphs) != 0 {
		t.Fatalf("single build created graphs: %v", client.graphs)
	}
	for spec, want := range map[string]string{"a": "repo1", "b": "repo2"} {
		if got := testSpecRepo(bi.Status.SpecResults, spec); got != want {
			t.Fatalf("source repository[%q] = %q, want %q", spec, got, want)
		}
	}
	if got := c.dcgDict.Len(); got != 0 {
		t.Fatalf("dcgDict len = %d, want 0 for single", got)
	}
	if len(bi.Status.Conditions) != 0 {
		t.Fatalf("conditions = %v, want none", bi.Status.Conditions)
	}
	for _, job := range listJobs(t, client) {
		if job.Labels[ebsv1.JobSpecNameLabel] != specname.Encode("b") {
			continue
		}
		payload := payloadFields(t, job.Spec.Payload)
		if payload["spec_name"] != "b" || payload["commit_id"] != "c2" {
			t.Fatalf("payload = %v, want spec_name/commit_id injected", payload)
		}
		if got, want := payload["repo"], []any{map[string]any{"url": testReleaseReference, "priority": float64(10)}}; !reflect.DeepEqual(got, want) {
			t.Fatalf("payload repo = %v, want %v", got, want)
		}
	}
}

func TestSingleRepoInjection(t *testing.T) {
	c, client, git, _ := newTestController(t)
	client.SeedProject(testProjectObj(ebsv1.ProjectActive))
	client.SeedBuild(testBuildObj("single", "repo1"))
	client.SetBuildResourceContent(testBuildResourceContent())
	client.SetBuildTargetContent(testBuildTargetContent())
	bi := testBuildInfoObj(ebsv1.BuildInfoPending)
	bi.Spec.BootstrapRepos = []ebsv1.BootstrapRepo{{Name: "base", URL: "http://bootstrap.local/base"}}
	client.SeedBuildInfo(bi)
	client.SeedSnapshot(testSnapshotObj(
		repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	git.repo(gitURL1, "c1", map[string]string{"a.spec": specText("a")})

	reconcileOnce(t, c)

	jobs := listJobs(t, client)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	payload := payloadFields(t, jobs[0].Spec.Payload)
	if got, want := payload["repo"], []any{
		map[string]any{"url": testReleaseReference, "priority": float64(10)},
		map[string]any{"url": "http://bootstrap.local/base/" + testArch, "priority": float64(99)},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("payload repo = %v, want %v", got, want)
	}
}

func TestSingleArtifactRepoInjection(t *testing.T) {
	c, client, git, _ := newTestController(t)
	client.SeedProject(testProjectObj(ebsv1.ProjectActive))
	client.SeedBuild(testBuildObj("single", "repo1"))
	client.SetBuildResourceContent(testBuildResourceContent())
	client.SetBuildTargetContent(testBuildTargetContent())
	client.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoPending))
	client.SeedSnapshot(testSnapshotObj(repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	git.repo(gitURL1, "c1", map[string]string{"a.spec": specText("a")})

	reconcileOnce(t, c)

	jobs := listJobs(t, client)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	want := []any{map[string]any{"url": testReleaseReference, "priority": float64(10)}}
	if !reflect.DeepEqual(
		payloadFields(t, jobs[0].Spec.Payload)["repo"],
		want,
	) {
		t.Fatalf("payload = %q, want repo %v", jobs[0].Spec.Payload, want)
	}
}

func TestResolveContentURLUsesConfiguredInstance(t *testing.T) {
	c, client, _, _ := newTestController(t)
	client.artifactEndpoints["art-01"] = "http://artifact-manager.art-01:8081"
	round := &reconcileRound{}
	if _, err := c.roundServices(round).GitClient(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		reference string
		want      string
	}{
		{"artifact://art-01/repositories/v1/repo-1/", "http://artifact-manager.art-01:8081/repositories/v1/repo-1/"},
		{"artifact:///repositories/v1/repo-2/", "http://artifact.example:8081/repositories/v1/repo-2/"},
	} {
		got, err := c.resolveContentURL(context.Background(), round, tc.reference)
		if err != nil || got != tc.want {
			t.Fatalf("resolveContentURL(%q) = %q, %v; want %q", tc.reference, got, err, tc.want)
		}
	}
	if client.artifactReads != 1 {
		t.Fatalf("Config reads = %d, want 1 for this reconcile", client.artifactReads)
	}
	for _, reference := range []string{"", "https://repo.example/repositories/v1/repo-3/"} {
		if _, err := c.resolveContentURL(context.Background(), &reconcileRound{}, reference); err != nil {
			t.Fatal(err)
		}
	}
	if client.artifactReads != 1 {
		t.Fatalf("direct or empty URL read Config %d times", client.artifactReads-1)
	}
}

func TestSinglePreferWithoutDependencyGate(t *testing.T) {
	c, client, git, _ := newTestController(t)
	client.SeedProject(testProjectObj(ebsv1.ProjectActive))
	client.SeedBuild(testBuildObj("single", "repo1"))
	client.SetBuildResourceContent(testBuildResourceContent())
	client.SetBuildTargetContent(testBuildTargetContent())
	bi := testBuildInfoObj(ebsv1.BuildInfoPending)
	bi.Spec.BuildPayload = "prefer:\n- rpm-a\n"
	client.SeedBuildInfo(bi)
	client.SeedSnapshot(testSnapshotObj(repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	git.repo(gitURL1, "c1", map[string]string{"a.spec": specText("a", "cap", "missing")})
	oldFetch := rpmMetaFetch
	t.Cleanup(func() { rpmMetaFetch = oldFetch })
	rpmMetaFetch = singleMetadataFixture

	reconcileOnce(t, c)
	jobs := listJobs(t, client)
	if len(jobs) != 1 {
		t.Fatalf("single Jobs = %d, want 1 despite missing BuildRequires", len(jobs))
	}
	payload := payloadFields(t, jobs[0].Spec.Payload)
	if got := payload["prefer"]; got != "rpm-a" {
		t.Fatalf("single Job prefer = %v, want rpm-a", got)
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
		CloneURL: gitURL1, Error: &ebsv1.PackageRepoError{Message: "gone", Retryable: false}}
	client.SeedSnapshot(snapshot)
	git.repo(gitURL2, "c2", map[string]string{"b.spec": specText("b")})

	reconcileOnce(t, c)

	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoProcessing)
	cond := requireCondition(t, bi.Status.Conditions, ConditionSpecCommitMissing, ReasonSpecCommitMissing)
	if !strings.Contains(cond.Message, "repo1 (gone)") ||
		!strings.Contains(cond.Message, "ghost (not in packageRepos)") {
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
		CloneURL: gitURL1, Error: &ebsv1.PackageRepoError{Message: "gone", Retryable: false}}
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
	t.Run("unsupported arch has its own status", func(t *testing.T) {
		c, client, git, _ := newTestController(t)
		seedHealthyBasics(client, "single", "repo1")
		client.SeedSnapshot(testSnapshotObj(
			repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
		armSpec := "Name: a\nVersion: 1.0\nRelease: 1\nSummary: a\nLicense: MIT\n" +
			"ExclusiveArch: aarch64\n\n%description\ntest\n"
		git.repo(gitURL1, "c1", map[string]string{"a.spec": armSpec})

		reconcileOnce(t, c)

		bi := getBuildInfo(t, client)
		requirePhase(t, bi, ebsv1.BuildInfoProcessing)
		ss := bi.Status.SpecResults.Entry("a")
		if ss.Status != ebsv1.SpecArchUnsupported {
			t.Fatalf("specResults[a].Status = %q, want ArchUnsupported", ss.Status)
		}
		if got := len(listJobs(t, client)); got != 0 {
			t.Fatalf("jobs = %d, want 0", got)
		}
		reconcileOnce(t, c)
		bi = getBuildInfo(t, client)
		requirePhase(t, bi, ebsv1.BuildInfoCompleted)
		if len(bi.Status.SpecResults.FailedPackages()) != 1 || bi.Status.SpecResults.FailedPackages()[0] != "repo1" {
			t.Fatalf("failedPackages = %v, want [repo1]", bi.Status.SpecResults.FailedPackages())
		}
	})

}

func TestSingleMissingImageMappingPauses(t *testing.T) {
	c, client, git, _ := newTestController(t)
	seedHealthyBasics(client, "single", "repo1")
	client.SetBuildTargetContent(&ebsv1.BuildTargetContent{
		Targets: map[string]ebsv1.BuildTargetConfigEntry{},
	})
	client.SeedSnapshot(testSnapshotObj(
		repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	git.repo(gitURL1, "c1", map[string]string{"a.spec": specText("a")})

	if _, err := c.reconcile(context.Background(), testNS+"/"+testBuild); err == nil {
		t.Fatal("reconcile() error = nil, want build-target Config mapping failure")
	}
	bi := getBuildInfo(t, client)
	requirePhase(t, bi, ebsv1.BuildInfoPending)
	if got := len(listJobs(t, client)); got != 0 {
		t.Fatalf("jobs = %d, want 0", got)
	}
}
