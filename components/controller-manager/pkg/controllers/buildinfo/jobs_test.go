// jobs_test.go covers the 19.1 Job-construction group (design 15.3.1): the
// deterministic name, the package-name label encoding, the resource merge,
// the payload contract, the build-target Config per-round image snapshot (E-26), and
// the dispatchSpec create outcomes (AlreadyExists identity verification with
// the GET-404 retry, the Unknown late-landing confirmation).
package buildinfo

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/controllers/buildinfo/rpmver"
	ebsv1 "ebs-api/ebs/v1"
)

// --- deterministic naming & label encoding ---

func TestJobNameForDeterministic(t *testing.T) {
	first := jobNameFor("uid-1", "a", 2)
	if first != jobNameFor("uid-1", "a", 2) {
		t.Fatal("jobNameFor not deterministic")
	}
	if !strings.HasPrefix(first, "a-2-") {
		t.Fatalf("jobNameFor = %q, want specName-generation- prefix", first)
	}
	if got := len(first) - len("a-2-"); got != 16 {
		t.Fatalf("hash suffix length = %d, want 16 lowercase hex chars", got)
	}
	previous := previousJobNameFor("uid-1", "a", 2)
	if len(previous)-len("a-2-") != 20 || !strings.HasPrefix(previous, first) {
		t.Fatalf("previous name = %q, want 20-character hash with new name as prefix", previous)
	}
	legacy := legacyJobNameFor("uid-1", "a", 2)
	if len(legacy)-len("a-2-") != 64 || !strings.HasPrefix(legacy, previous) {
		t.Fatalf("legacy name = %q, want full hash with previous name as prefix", legacy)
	}
	if jobNameFor("uid-1", "a", 3) == first || jobNameFor("uid-2", "a", 2) == first || jobNameFor("uid-1", "b", 2) == first {
		t.Fatal("jobNameFor must vary with generation, uid and spec")
	}
}

func TestFilterJobsByIdentityRequiresBuildInfoUIDInName(t *testing.T) {
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	bi.UID = "current-buildinfo"
	job := testJobObj(bi, "a", 1, ebsv1.JobRunning)
	if got := filterJobsByIdentity([]ebsv1.Job{*job}, string(bi.UID)); len(got) != 1 {
		t.Fatalf("matching Job count = %d, want 1", len(got))
	}
	job.Name = previousJobNameFor(string(bi.UID), "a", 1)
	if err := verifyJobIdentity(job, bi, "a", 1); err != nil {
		t.Fatalf("previous Job identity rejected: %v", err)
	}
	if got := filterJobsByIdentity([]ebsv1.Job{*job}, string(bi.UID)); len(got) != 1 {
		t.Fatalf("matching previous Job count = %d, want 1", len(got))
	}
	job.Name = legacyJobNameFor(string(bi.UID), "a", 1)
	if err := verifyJobIdentity(job, bi, "a", 1); err != nil {
		t.Fatalf("legacy Job identity rejected: %v", err)
	}
	if got := filterJobsByIdentity([]ebsv1.Job{*job}, string(bi.UID)); len(got) != 1 {
		t.Fatalf("matching legacy Job count = %d, want 1", len(got))
	}
	job.Name = "a-1-unrelated"
	if err := verifyJobIdentity(job, bi, "a", 1); err == nil {
		t.Fatal("unrelated Job name accepted")
	}
	if got := filterJobsByIdentity([]ebsv1.Job{*job}, string(bi.UID)); len(got) != 0 {
		t.Fatalf("unrelated Job count = %d, want 0", len(got))
	}
	job.Name = legacyJobNameFor("previous-buildinfo", "a", 1)
	if err := verifyJobIdentity(job, bi, "a", 1); err == nil {
		t.Fatal("foreign Job name accepted")
	}
	if got := filterJobsByIdentity([]ebsv1.Job{*job}, string(bi.UID)); len(got) != 0 {
		t.Fatalf("foreign Job count = %d, want 0", len(got))
	}
}

func TestPackageNameLabelValue(t *testing.T) {
	digest := func(name string) string {
		value := packageNameLabelValue(name)
		if !strings.HasPrefix(value, "sha256-") || len(value) != len("sha256-")+52 {
			t.Fatalf("packageNameLabelValue(%q) = %q, want sha256- plus 52-char digest", name, value)
		}
		return value
	}
	tests := []struct {
		name, want string
	}{
		{"a", "a"},
		{"my-pkg_1.0", "my-pkg_1.0"},
		{strings.Repeat("x", 70), strings.Repeat("x", 63)}, // truncated to 63
		// Truncation landing on a strippable tail: 63 chars, then strip -_..
		{strings.Repeat("a", 62) + "-" + strings.Repeat("b", 10), strings.Repeat("a", 62)},
	}
	for _, tc := range tests {
		if got := packageNameLabelValue(tc.name); got != tc.want {
			t.Errorf("packageNameLabelValue(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
	// Illegal characters (/), illegal ends (trailing dot/dash) and the empty
	// name all take the digest form.
	digest("my_pkg/name")
	digest("pkg.")
	digest(strings.Repeat("a", 62) + "-")
	digest("")
	// A valid name colliding with the reserved digest form never passes
	// through (labels.md §7).
	reserved := "sha256-" + strings.Repeat("a", 52)
	if got := digest(reserved); got == reserved {
		t.Fatal("reserved digest form must be re-encoded, not passed through")
	}
}

// --- resource merge (data-models~config.md 3.2) ---

func TestResolveResourcesMerge(t *testing.T) {
	resource := &buildResourceRules{
		Spec: ebsv1.BuildResourceContent{
			Default: ebsv1.ResourceRequirements{Requests: map[string]string{"cpu": "1", "memory": "2Gi"}},
			Packages: map[string]ebsv1.PackageResourceConfig{
				"a": {
					Default: ebsv1.ResourceRequirements{Requests: map[string]string{"cpu": "2"}},
					Arches: map[string]ebsv1.ResourceRequirements{
						testArch: {Requests: map[string]string{"memory": "4Gi"}},
					},
				},
			},
		},
	}
	merged := resolveResources(resource, "a", testArch)
	if merged.Requests["cpu"] != "2" || merged.Requests["memory"] != "4Gi" {
		t.Fatalf("requests = %v, want cpu=2 memory=4Gi (three-level merge)", merged.Requests)
	}
	// Each level's unset limits take the same level's requests.
	if merged.Limits["cpu"] != "2" || merged.Limits["memory"] != "4Gi" {
		t.Fatalf("limits = %v, want cpu=2 memory=4Gi", merged.Limits)
	}
	// An arch miss stops at the package default level.
	merged = resolveResources(resource, "a", "aarch64")
	if merged.Requests["cpu"] != "2" || merged.Requests["memory"] != "2Gi" {
		t.Fatalf("arch-miss requests = %v, want cpu=2 memory=2Gi", merged.Requests)
	}
	// A spec without a package entry gets the table default.
	merged = resolveResources(resource, "b", testArch)
	if merged.Requests["cpu"] != "1" || merged.Limits["memory"] != "2Gi" {
		t.Fatalf("default requests/limits = %+v, want cpu=1 memory=2Gi", merged)
	}
}

// --- repo payload helpers ---

func TestJoinRepoPayload(t *testing.T) {
	if got := joinRepoPayload("", nil); got != "" {
		t.Fatalf("empty join = %q, want empty", got)
	}
	if got := joinRepoPayload("", []string{"u1", "u2"}); got != "u1 u2" {
		t.Fatalf("bootstrap-only join = %q, want u1 u2", got)
	}
	if got := joinRepoPayload(testRepoURL, []string{"u1"}); got != testRepoURL+" u1" {
		t.Fatalf("contentURL join = %q, want contentURL first", got)
	}
}

func TestBootstrapRepoURLs(t *testing.T) {
	repos := []ebsv1.BootstrapRepo{
		{Name: "base", Repo: "https://example.com/base"},
		{Name: "updates", Repo: "https://example.com/updates/"},
	}
	got := bootstrapRepoURLs(repos, "aarch64")
	want := []string{"https://example.com/base/aarch64", "https://example.com/updates/aarch64"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bootstrapRepoURLs = %v, want %v", got, want)
	}
	if repos[0].Repo != "https://example.com/base" {
		t.Fatalf("bootstrapRepoURLs modified input: %+v", repos)
	}
}

func TestNormalizeRepoPriority(t *testing.T) {
	c, _, _, _ := newTestController(t)
	round := &reconcileRound{key: testNS + "/" + testBuild}
	tests := []struct {
		base  any
		count int
		want  string
	}{
		{nil, 2, "10 10"},
		{"", 1, "10"},
		{"  ", 1, "10"},
		{"5", 3, "5 10 10"}, // padded
		{"1 2 3", 2, "1 2"}, // truncated
		{"7 8", 2, "7 8"},   // aligned
		{42, 2, "10 10"},    // non-string base
	}
	for _, tc := range tests {
		if got := c.normalizeRepoPriority(round, tc.base, tc.count); got != tc.want {
			t.Errorf("normalizeRepoPriority(%v, %d) = %q, want %q", tc.base, tc.count, got, tc.want)
		}
	}
}

// --- Job construction (15.3.1) ---

func TestJobForSpecConstruction(t *testing.T) {
	c, client, _, _ := newTestController(t)
	key := testNS + "/" + testBuild
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	bi.UID = "bi-job-construct"
	bi.Spec.BuildPayload = "custom: keep\nRepo: http://legacy-override\nrepo: http://base-override\nrepo_priority: \"7\"\n"
	bi.Spec.BootstrapRepo = []ebsv1.BootstrapRepo{{Name: "base", Repo: "http://bootstrap.local/base"}}
	seeded := client.SeedBuildInfo(bi)
	round := &reconcileRound{key: key, current: seeded, build: testBuildObj("full"), failures: c.newRoundFailures(key)}
	snapshot := testSnapshotObj(repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true})
	depend := dependEntry("a")
	resource := testBuildResourceRules()

	job := c.jobForSpec(round, "a", &depend, snapshot, testImage, testRepoURL, resource, testScriptRef(), "job-x", 2, nil)
	if len(job.Spec.ScriptRefs) != 1 || job.Spec.ScriptRefs[0].Name != "rpmbuild" {
		t.Fatalf("scriptRefs = %+v", job.Spec.ScriptRefs)
	}

	if job.Name != "job-x" || job.Namespace != testNS {
		t.Fatalf("job meta = %s/%s, want %s/job-x", job.Namespace, job.Name, testNS)
	}
	wantLabels := map[string]string{
		ebsv1.JobBuildNameLabel:    testBuild,
		ebsv1.JobSpecNameLabel:     "a",
		ebsv1.JobPackageNameLabel:  packageNameLabelValue("repo1"),
		ebsv1.BuildTargetOSLabel:   testOS,
		ebsv1.BuildTargetArchLabel: testArch,
	}
	for k, want := range wantLabels {
		if job.Labels[k] != want {
			t.Errorf("label %s = %q, want %q", k, job.Labels[k], want)
		}
	}
	wantAnnotations := map[string]string{
		annDispatchGeneration: "2",
	}
	if len(job.Annotations) != len(wantAnnotations) {
		t.Fatalf("annotations = %v, want only %v", job.Annotations, wantAnnotations)
	}
	for k, want := range wantAnnotations {
		if job.Annotations[k] != want {
			t.Errorf("annotation %s = %q, want %q", k, job.Annotations[k], want)
		}
	}
	if job.Spec.Runtime != jobRuntime || job.Spec.TimeoutSeconds != jobTimeoutSeconds {
		t.Errorf("runtime/timeout = %q/%d, want %q/%d", job.Spec.Runtime, job.Spec.TimeoutSeconds, jobRuntime, jobTimeoutSeconds)
	}
	if job.Spec.NodeSelector[runnerArchSelector] != testArch {
		t.Errorf("nodeSelector = %v, want runner arch %q", job.Spec.NodeSelector, testArch)
	}
	if !strings.Contains(string(job.Spec.RuntimeSpec.Raw), testImage) {
		t.Errorf("runtimeSpec = %s, want image %q", job.Spec.RuntimeSpec.Raw, testImage)
	}
	if job.Spec.Resources.Requests["cpu"] != "1" {
		t.Errorf("resources = %+v, want the project default", job.Spec.Resources)
	}
	payload := job.Spec.Payload
	for _, fragment := range []string{
		"spec_name: a",
		"spec_file_name: a.spec",
		"spec_url: " + gitURL1,
		"commitId: c1",
		"custom: keep",
		// The injected build-level keys override the base ones (15.3.1).
		"repo: " + testRepoURL + " http://bootstrap.local/base/" + testArch,
		"repo_priority: 7 10",
	} {
		if !strings.Contains(payload, fragment) {
			t.Errorf("payload missing %q:\n%s", fragment, payload)
		}
	}
	if strings.Contains(payload, "base-override") || strings.Contains(payload, "legacy-override") || strings.Contains(payload, "Repo:") {
		t.Errorf("payload kept a base repo key:\n%s", payload)
	}
}

func TestJobPayloadDisableCheckPathMatchesPackageRepo(t *testing.T) {
	tests := []struct {
		name        string
		payload     string
		wantPresent bool
	}{
		{name: "matching repository", payload: "disable_check_path:\n- repo1\n", wantPresent: true},
		{name: "spec name is not repository name", payload: "disable_check_path:\n- a\n"},
		{name: "other repository", payload: "disable_check_path:\n- repo2\n"},
		{name: "unset"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, client, _, _ := newTestController(t)
			bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
			bi.Spec.BuildPayload = tt.payload
			round := &reconcileRound{current: client.SeedBuildInfo(bi), build: testBuildObj("full")}
			depend := dependEntry("a")
			job := c.jobForSpec(round, "a", &depend, testSnapshotObj(), testImage, "", testBuildResourceRules(), testScriptRef(), "job-a", 1, nil)
			var payload map[string]any
			if err := yaml.Unmarshal([]byte(job.Spec.Payload), &payload); err != nil {
				t.Fatal(err)
			}
			value, present := payload["disable_check_path"]
			if present != tt.wantPresent || present && value != true {
				t.Fatalf("disable_check_path = %v (present=%t), want present=%t and true when present", value, present, tt.wantPresent)
			}
		})
	}
}

func TestJobPayloadPreferIsPerSpec(t *testing.T) {
	c, client, _, _ := newTestController(t)
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	bi.Spec.BuildPayload = "prefer:\n- rpm-b\n- rpm-a\n- unused\n"
	round := &reconcileRound{key: testNS + "/" + testBuild, current: client.SeedBuildInfo(bi), build: testBuildObj("full")}
	sources := &rpmver.RpmMetaSources{
		RepoLayer: &rpmver.RpmMetaSource{ProvidesInfo: map[string]map[string]rpmver.ProvideEntry{
			"cap-a": {
				"rpm-a@spec-a": {Version: "0:1.0-1", SpecName: "spec-a"},
				"rpm-b@spec-b": {Version: "0:2.0-1", SpecName: "spec-b"},
			},
			"cap-b": {
				"rpm-b": {Version: "0:1.0-1", SpecName: "spec-b"},
				"rpm-c": {Version: "0:2.0-1", SpecName: "spec-c"},
			},
			"single": {"rpm-a": {Version: "0:1.0-1", SpecName: "spec-a"}},
		}},
		BootstrapLayer: []*rpmver.RpmMetaSource{{ProvidesInfo: map[string]map[string]rpmver.ProvideEntry{
			"cap-a": {"unused": {Version: "0:9.0-1", SpecName: "other"}},
		}}},
	}
	depend := dependEntry("a")
	depend.BuildRequires = map[string]ebsv1.VersionConst{
		"cap-b": {}, "cap-a": {}, "single": {}, "removed": {},
	}
	depend.BuildRemoves = map[string]ebsv1.VersionConst{"removed": {}}
	job := c.jobForSpec(round, "a", &depend, testSnapshotObj(), testImage, testRepoURL, testBuildResourceRules(), testScriptRef(), "job-a", 1, sources)
	var payload map[string]any
	if err := yaml.Unmarshal([]byte(job.Spec.Payload), &payload); err != nil {
		t.Fatal(err)
	}
	if got := payload["prefer"]; got != "rpm-b" {
		t.Fatalf("prefer = %v, want only selected rpm-b", got)
	}
	depend.BuildRequires = map[string]ebsv1.VersionConst{"single": {}}
	job = c.jobForSpec(round, "a", &depend, testSnapshotObj(), testImage, testRepoURL, testBuildResourceRules(), testScriptRef(), "job-b", 1, sources)
	payload = nil
	if err := yaml.Unmarshal([]byte(job.Spec.Payload), &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["prefer"]; ok {
		t.Fatalf("single-candidate payload kept prefer: %s", job.Spec.Payload)
	}
}

func TestJobForSpecRepoEntryMissing(t *testing.T) {
	c, client, _, _ := newTestController(t)
	key := testNS + "/" + testBuild
	seeded := client.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoProcessing))
	round := &reconcileRound{key: key, current: seeded, build: testBuildObj("full"), failures: c.newRoundFailures(key)}
	// The snapshot has no packageRepoStatuses entry for repo1: spec_url and
	// commitId are skipped (log only, never blocks dispatch, 15.3.1).
	snapshot := testSnapshotObj()
	depend := dependEntry("a")

	job := c.jobForSpec(round, "a", &depend, snapshot, testImage, "", testBuildResourceRules(), testScriptRef(), "job-y", 1, nil)

	if strings.Contains(job.Spec.Payload, "spec_url") || strings.Contains(job.Spec.Payload, "commitId") {
		t.Fatalf("payload = %q, want no spec_url/commitId without a repo entry", job.Spec.Payload)
	}
	if !strings.Contains(job.Spec.Payload, "spec_name: a") {
		t.Fatalf("payload = %q, want the per-spec keys", job.Spec.Payload)
	}
}

// --- build-target Config per-round image snapshot (E-26) ---

func TestEnsureImageRoundSnapshot(t *testing.T) {
	c, client, _, _ := newTestController(t)
	client.SetBuildTargetContent(testBuildTargetContent())
	round := &reconcileRound{key: testNS + "/" + testBuild, build: testBuildObj("full")}
	dispatch := &roundDispatch{arch: testArch}

	image, err := c.ensureImage(context.Background(), round, dispatch)
	if err != nil || image != testImage {
		t.Fatalf("ensureImage = %q, %v, want %q", image, err, testImage)
	}
	// A build-target Config change inside the round is invisible: the image snapshot is
	// resolved once and shared round-wide (E-26).
	client.SetBuildTargetContent(&ebsv1.BuildTargetContent{
		Targets: map[string]ebsv1.BuildTargetConfigEntry{
			testOS: {Arches: map[string]ebsv1.BuildTargetArch{testArch: {Image: "img:v2"}}},
		},
	})
	again, err := c.ensureImage(context.Background(), round, dispatch)
	if err != nil || again != testImage {
		t.Fatalf("ensureImage second call = %q, %v, want the round snapshot %q", again, err, testImage)
	}
	// A fresh round observes the new mapping; a read failure pauses (error).
	fresh := &roundDispatch{arch: testArch}
	if image, err = c.ensureImage(context.Background(), round, fresh); err != nil || image != "img:v2" {
		t.Fatalf("ensureImage fresh round = %q, %v, want img:v2", image, err)
	}
	client.FailBuildTargetContent()
	if _, err = c.ensureImage(context.Background(), round, &roundDispatch{arch: testArch}); err == nil {
		t.Fatal("ensureImage error = nil, want the build-target Config read failure (E-26 pause)")
	}
}

// --- dispatchSpec create outcomes (15.3.1/6.5.1) ---

// dispatchRound builds a minimal Processing round for direct dispatchSpec
// calls: the seeded BuildInfo (specStatus entry for the spec), the parent
// Build and the project BuildResourceConfig table.
func dispatchRound(t *testing.T, c *Controller, client *fakeClient, uid, spec string) (*reconcileRound, *ebsv1.BuildInfo) {
	t.Helper()
	key := testNS + "/" + testBuild
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	bi.UID = types.UID(uid)
	bi.Status.SpecStatus = map[string]ebsv1.SpecStatus{spec: {}}
	seeded := client.SeedBuildInfo(bi)
	client.SeedBuildResourceRules(testBuildResourceRules())
	round := &reconcileRound{key: key, current: seeded, build: testBuildObj("full"), failures: c.newRoundFailures(key)}
	return round, seeded
}

func testScriptRef() ebsv1.ScriptRef {
	return ebsv1.ScriptRef{Name: "rpmbuild", UID: "61304b92-72cf-4a41-8bf7-8e0a9d14f6a5", ResourceVersion: "1"}
}

func TestDispatchSpecRecordsScriptObservation(t *testing.T) {
	c, client, _, _ := newTestController(t)
	round, _ := dispatchRound(t, c, client, "bi-script", "a")
	round.current.Spec.BuildPayload = "rpmbuild_script: custom-script\n"
	client.buildinfos[testNS+"/"+testBuild].Spec.BuildPayload = round.current.Spec.BuildPayload
	client.scripts["custom-script"] = &ebsv1.Script{ObjectMeta: metav1.ObjectMeta{Name: "custom-script", UID: "a56761f8-2058-4210-814b-3b8858508232", ResourceVersion: "v1:2:3"}, Spec: ebsv1.ScriptSpec{Content: "#!/bin/sh\n"}}
	depend := dependEntry("a")
	if _, err := c.dispatchSpec(context.Background(), round, "a", &depend, testSnapshotObj(), testImage, testRepoURL, nil); err != nil {
		t.Fatal(err)
	}
	jobs := listJobs(t, client)
	want := ebsv1.ScriptRef{Name: "custom-script", UID: "a56761f8-2058-4210-814b-3b8858508232", ResourceVersion: "v1:2:3"}
	if len(jobs) != 1 || len(jobs[0].Spec.ScriptRefs) != 1 || jobs[0].Spec.ScriptRefs[0] != want {
		t.Fatalf("Jobs and script refs = %+v", jobs)
	}
	if got := payloadScriptName(t, jobs[0].Spec.Payload); got != "" {
		t.Fatalf("payload still carries rpmbuild_script = %q", got)
	}
}

func TestDispatchSpecDefaultsScriptName(t *testing.T) {
	c, client, _, _ := newTestController(t)
	round, _ := dispatchRound(t, c, client, "bi-script-default", "a")
	depend := dependEntry("a")
	if _, err := c.dispatchSpec(context.Background(), round, "a", &depend, testSnapshotObj(), testImage, testRepoURL, nil); err != nil {
		t.Fatal(err)
	}
	jobs := listJobs(t, client)
	if len(jobs) != 1 || len(jobs[0].Spec.ScriptRefs) != 1 || jobs[0].Spec.ScriptRefs[0] != testScriptRef() {
		t.Fatalf("Jobs and script refs = %+v", jobs)
	}
}

func TestDispatchSpecSharesScriptObservationWithinRound(t *testing.T) {
	c, client, _, _ := newTestController(t)
	round, _ := dispatchRound(t, c, client, "bi-script-shared", "a")
	client.buildinfos[testNS+"/"+testBuild].Status.SpecStatus["b"] = ebsv1.SpecStatus{}
	round.current.Status.SpecStatus["b"] = ebsv1.SpecStatus{}
	snapshot := testSnapshotObj()
	for _, specName := range []string{"a", "b"} {
		depend := dependEntry(specName)
		if _, err := c.dispatchSpec(context.Background(), round, specName, &depend, snapshot, testImage, testRepoURL, nil); err != nil {
			t.Fatalf("dispatch %s: %v", specName, err)
		}
		if specName == "a" {
			client.scripts["rpmbuild"].ResourceVersion = "2"
		}
	}
	if client.scriptReads != 1 {
		t.Fatalf("Script reads in one round = %d, want 1", client.scriptReads)
	}
	for _, job := range listJobs(t, client) {
		if len(job.Spec.ScriptRefs) != 1 || job.Spec.ScriptRefs[0].ResourceVersion != "1" {
			t.Fatalf("Job %s ScriptRefs = %+v, want first round observation", job.Name, job.Spec.ScriptRefs)
		}
	}

	client.buildinfos[testNS+"/"+testBuild].Status.SpecStatus["c"] = ebsv1.SpecStatus{}
	newRound := &reconcileRound{
		key: round.key, current: getBuildInfo(t, client), build: round.build,
		failures: c.newRoundFailures(round.key),
	}
	depend := dependEntry("c")
	if _, err := c.dispatchSpec(context.Background(), newRound, "c", &depend, snapshot, testImage, testRepoURL, nil); err != nil {
		t.Fatalf("dispatch c in new round: %v", err)
	}
	if client.scriptReads != 2 {
		t.Fatalf("Script reads across rounds = %d, want 2", client.scriptReads)
	}
	for _, job := range listJobs(t, client) {
		if job.Labels[ebsv1.JobSpecNameLabel] == "c" && (len(job.Spec.ScriptRefs) != 1 || job.Spec.ScriptRefs[0].ResourceVersion != "2") {
			t.Fatalf("new round Job ScriptRefs = %+v, want current observation", job.Spec.ScriptRefs)
		}
	}
}

func TestDispatchSpecMissingScriptDoesNotCreateJob(t *testing.T) {
	c, client, _, _ := newTestController(t)
	round, _ := dispatchRound(t, c, client, "bi-script-missing", "a")
	round.current.Spec.BuildPayload = "rpmbuild_script: missing-script\n"
	client.buildinfos[testNS+"/"+testBuild].Spec.BuildPayload = round.current.Spec.BuildPayload
	depend := dependEntry("a")
	if _, err := c.dispatchSpec(context.Background(), round, "a", &depend, testSnapshotObj(), testImage, testRepoURL, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dispatchSpec error = %v, want ErrNotFound", err)
	}
	if got := len(listJobs(t, client)); got != 0 {
		t.Fatalf("Jobs = %d, want none", got)
	}
}

func TestScriptNameFromPayload(t *testing.T) {
	for _, tc := range []struct {
		payload string
		want    string
		bad     bool
	}{
		{payload: "", want: "rpmbuild"},
		{payload: "rpmbuild_script: ''\n", want: "rpmbuild"},
		{payload: "rpmbuild_script: custom-script\n", want: "custom-script"},
		{payload: "rpmbuild_script: [bad]\n", bad: true},
		{payload: "rpmbuild_script: ../bad\n", bad: true},
		{payload: "rpmbuild_script: [\n", bad: true},
	} {
		got, err := scriptNameFromPayload(tc.payload)
		if (err != nil) != tc.bad || (!tc.bad && got != tc.want) {
			t.Errorf("scriptNameFromPayload(%q) = %q, %v; want %q, bad=%v", tc.payload, got, err, tc.want, tc.bad)
		}
	}
}

func payloadScriptName(t *testing.T, payload string) string {
	t.Helper()
	var values map[string]any
	if err := yaml.Unmarshal([]byte(payload), &values); err != nil {
		t.Fatal(err)
	}
	name, _ := values["rpmbuild_script"].(string)
	return name
}

func TestDispatchSpecNotSentNotCountedAsSent(t *testing.T) {
	c, client, _, _ := newTestController(t)
	round, _ := dispatchRound(t, c, client, "bi-dispatch-notsent", "a")
	// Local validation failure (WriteNotSent, client contract 4.1): no
	// request left the controller, so jobCreates must not count it
	// (metrics.go "Job create requests sent") and the this-round
	// registration is removed (6.5.1 #3).
	before := jobCreates.Value()
	client.InjectWrite("create", clientpkg.WriteNotSent, 0, false)
	depend := dependEntry("a")
	snapshot := testSnapshotObj()

	_, err := c.dispatchSpec(context.Background(), round, "a", &depend, snapshot, testImage, testRepoURL, nil)
	if err == nil || !controller.IsPermanent(err) {
		t.Fatalf("dispatchSpec error = %v, want a permanent NotSent error", err)
	}
	if delta := jobCreates.Value() - before; delta != 0 {
		t.Fatalf("jobCreates delta = %d, want 0 (WriteNotSent never sent a request)", delta)
	}
	persisted := getBuildInfo(t, client)
	if len(persisted.Status.PendingJobCreates) != 0 {
		t.Fatalf("pendingJobCreates = %v, want the this-round registration removed", persisted.Status.PendingJobCreates)
	}
}

func TestDispatchSpecAlreadyExistsConfirms(t *testing.T) {
	c, client, _, _ := newTestController(t)
	round, seeded := dispatchRound(t, c, client, "bi-dispatch-409", "a")
	// A previous round's create already landed (the List missed it): the
	// deterministic name collides, AlreadyExists triggers the identity
	// verification and the existing Job is confirmed — no duplicate (15.3.1).
	existing := seedJobAt(client, seeded, "a", 1, ebsv1.JobRunning, testStart)
	depend := dependEntry("a")
	snapshot := testSnapshotObj(repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true})

	result, err := c.dispatchSpec(context.Background(), round, "a", &depend, snapshot, testImage, testRepoURL, nil)
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("dispatchSpec = %v, %v, want success", result, err)
	}
	if got := len(listJobs(t, client)); got != 1 {
		t.Fatalf("jobs = %d, want 1 (no duplicate)", got)
	}
	persisted := getBuildInfo(t, client)
	a := persisted.Status.SpecStatus["a"]
	if a.DispatchCount != 1 || a.Build.JobName != existing.Name || a.Build.Status != SpecBuildRunning {
		t.Fatalf("specStatus[a] = %+v, want confirmed Running dispatch of the existing job", a)
	}
	if len(persisted.Status.PendingJobCreates) != 0 {
		t.Fatalf("pendingJobCreates = %v, want the entry confirmed and removed", persisted.Status.PendingJobCreates)
	}
}

func TestSinglePendingJobConfirmDoesNotLoadPreferMetadata(t *testing.T) {
	c, client, _, _ := newTestController(t)
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	bi.UID = "single-pending"
	bi.Spec.BuildPayload = "prefer:\n- rpm-a\n"
	bi.Status.SpecStatus = map[string]ebsv1.SpecStatus{"a": {}}
	bi.Status.PendingJobCreates = map[string]ebsv1.PendingJobCreate{
		"a": {JobName: jobNameFor(string(bi.UID), "a", 1), DispatchGeneration: 1},
	}
	seeded := client.SeedBuildInfo(bi)
	client.SeedJob(testJobObj(seeded, "a", 1, ebsv1.JobRunning))
	round := &reconcileRound{
		key: testNS + "/" + testBuild, current: seeded,
		build: testBuildObj("single", "repo1"), failures: c.newRoundFailures(testNS + "/" + testBuild),
	}
	depend := dependEntry("a")
	result, err := c.dispatchSpec(context.Background(), round, "a", &depend, testSnapshotObj(), testImage, testRepoURL, nil)
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("pending Job confirmation = %v, %v", result, err)
	}
	if got := len(listJobs(t, client)); got != 1 {
		t.Fatalf("Jobs = %d, want existing Job only", got)
	}
}

func TestDispatchSpecAlreadyExistsIdentityMismatch(t *testing.T) {
	c, client, _, _ := newTestController(t)
	round, seeded := dispatchRound(t, c, client, "bi-dispatch-mismatch", "a")
	clash := testJobObj(seeded, "a", 1, ebsv1.JobRunning)
	clash.Labels[ebsv1.JobSpecNameLabel] = "another-spec"
	client.SeedJob(clash)
	depend := dependEntry("a")
	snapshot := testSnapshotObj()

	_, err := c.dispatchSpec(context.Background(), round, "a", &depend, snapshot, testImage, testRepoURL, nil)
	if err == nil || !controller.IsPermanent(err) {
		t.Fatalf("dispatchSpec error = %v, want a permanent identity-mismatch error", err)
	}
}

func TestDispatchSpecAlreadyExistsConfirm404Retried(t *testing.T) {
	c, client, _, _ := newTestController(t)
	round, _ := dispatchRound(t, c, client, "bi-dispatch-409-404", "a")
	// 409 without a stored object: the confirmation GET 404 is a retryable
	// error, never the main-object NotFound rule; the entry stays registered.
	client.InjectWrite("create", clientpkg.WriteRejected, 409, false)
	depend := dependEntry("a")
	snapshot := testSnapshotObj()

	_, err := c.dispatchSpec(context.Background(), round, "a", &depend, snapshot, testImage, testRepoURL, nil)
	if err == nil || controller.IsPermanent(err) {
		t.Fatalf("dispatchSpec error = %v, want a retryable confirmation-read error", err)
	}
	persisted := getBuildInfo(t, client)
	if len(persisted.Status.PendingJobCreates) != 1 {
		t.Fatalf("pendingJobCreates = %v, want the registered entry kept", persisted.Status.PendingJobCreates)
	}
}

func TestDispatchSpecUnknownLandedConfirms(t *testing.T) {
	c, client, _, _ := newTestController(t)
	round, _ := dispatchRound(t, c, client, "bi-dispatch-unknown-ok", "a")
	// Unknown with the write landed: the deterministic-name GET confirms the
	// late-landing create (10.3, never a replay).
	client.InjectWrite("create", clientpkg.WriteUnknown, 0, true)
	depend := dependEntry("a")
	snapshot := testSnapshotObj(repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true})

	result, err := c.dispatchSpec(context.Background(), round, "a", &depend, snapshot, testImage, testRepoURL, nil)
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("dispatchSpec = %v, %v, want success after the Unknown landing", result, err)
	}
	persisted := getBuildInfo(t, client)
	a := persisted.Status.SpecStatus["a"]
	if a.DispatchCount != 1 || a.Build.JobName == "" {
		t.Fatalf("specStatus[a] = %+v, want the landed job confirmed", a)
	}
	if got := len(listJobs(t, client)); got != 1 {
		t.Fatalf("jobs = %d, want 1", got)
	}
	if len(persisted.Status.PendingJobCreates) != 0 {
		t.Fatalf("pendingJobCreates = %v, want the entry confirmed and removed", persisted.Status.PendingJobCreates)
	}
}

func TestDispatchSpecUnknownMissingKeepsEntry(t *testing.T) {
	c, client, _, _ := newTestController(t)
	round, _ := dispatchRound(t, c, client, "bi-dispatch-unknown-404", "a")
	// Unknown with nothing landed: the entry stays registered (6.5.1 #6 — a
	// GET never proves the request never lands) and the error re-enters with
	// backoff.
	client.InjectWrite("create", clientpkg.WriteUnknown, 0, false)
	depend := dependEntry("a")
	snapshot := testSnapshotObj()

	_, err := c.dispatchSpec(context.Background(), round, "a", &depend, snapshot, testImage, testRepoURL, nil)
	if err == nil {
		t.Fatal("dispatchSpec error = nil, want the Unknown write error")
	}
	persisted := getBuildInfo(t, client)
	if len(persisted.Status.PendingJobCreates) != 1 {
		t.Fatalf("pendingJobCreates = %v, want the entry kept for next-round verification", persisted.Status.PendingJobCreates)
	}
	if got := len(listJobs(t, client)); got != 0 {
		t.Fatalf("jobs = %d, want 0", got)
	}
}
