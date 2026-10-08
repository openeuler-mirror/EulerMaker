// jobs_test.go covers the 19.1 Job-construction group (design 15.3.1): the
// deterministic name, the package-name label encoding, the resource merge,
// the payload contract, the build-target Config per-round image snapshot (E-26), and
// the dispatchSpec create outcomes (AlreadyExists identity verification with
// the GET-404 retry, the Unknown late-landing confirmation).
package buildinfo

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

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
	if !strings.HasPrefix(first, "a-") {
		t.Fatalf("jobNameFor = %q, want specName- prefix", first)
	}
	if got := len(first) - len("a-"); got != 16 {
		t.Fatalf("hash suffix length = %d, want 16 lowercase hex chars", got)
	}
	former := formerJobNameFor("uid-1", "a", 2)
	if former != "a-2-"+strings.TrimPrefix(first, "a-") {
		t.Fatalf("former name = %q, want visible generation and the same hash", former)
	}
	previous := previousJobNameFor("uid-1", "a", 2)
	if len(previous)-len("a-2-") != 20 || !strings.HasPrefix(previous, former) {
		t.Fatalf("previous name = %q, want 20-character hash with former name as prefix", previous)
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
	job.Name = formerJobNameFor(string(bi.UID), "a", 1)
	if err := verifyJobIdentity(job, bi, "a", 1); err != nil {
		t.Fatalf("former Job identity rejected: %v", err)
	}
	if got := filterJobsByIdentity([]ebsv1.Job{*job}, string(bi.UID)); len(got) != 1 {
		t.Fatalf("matching former Job count = %d, want 1", len(got))
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

func TestRepoPayloadURLs(t *testing.T) {
	if got := repoPayloadURLs("", nil); len(got) != 0 {
		t.Fatalf("empty repo list = %v, want empty", got)
	}
	if got := repoPayloadURLs("", []string{"u1", "u2"}); !reflect.DeepEqual(got, []string{"u1", "u2"}) {
		t.Fatalf("bootstrap-only repos = %v, want u1/u2", got)
	}
	if got := repoPayloadURLs(testRepoURL, []string{"u1"}); !reflect.DeepEqual(got, []string{testRepoURL, "u1"}) {
		t.Fatalf("repos = %v, want contentURL first", got)
	}
}

func TestConfiguredRepoURLs(t *testing.T) {
	for _, tc := range []struct {
		input any
		want  []string
	}{
		{input: "http://repo-a/ http://repo-b/", want: []string{"http://repo-a/", "http://repo-b/"}},
		{input: []any{"http://repo-a/", "http://repo-b/"}, want: []string{"http://repo-a/", "http://repo-b/"}},
		{input: nil},
	} {
		if got := configuredRepoURLs(tc.input); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("configuredRepoURLs(%v) = %v, want %v", tc.input, got, tc.want)
		}
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

// --- Job construction (15.3.1) ---

func TestJobForSpecConstruction(t *testing.T) {
	c, client, _, _ := newTestController(t)
	key := testNS + "/" + testBuild
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	bi.UID = "bi-job-construct"
	bi.Spec.BuildPayload = "custom: omit\nRepo: http://legacy-override\nrepo: http://base-override\nrepo_priority: \"7\"\nspec_name: forged\ncommit_id: forged\npreinstall:\n- rpm-build\n- systemd-rpm-macros\n"
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
	var runtimeSpec struct {
		Image       string `json:"image"`
		NetworkMode string `json:"networkMode"`
	}
	if err := json.Unmarshal(job.Spec.RuntimeSpec.Raw, &runtimeSpec); err != nil {
		t.Fatalf("decode runtimeSpec: %v", err)
	}
	if runtimeSpec.Image != testImage || runtimeSpec.NetworkMode != "host" {
		t.Errorf("runtimeSpec = %+v, want image %q and host network", runtimeSpec, testImage)
	}
	if job.Spec.Resources.Requests["cpu"] != "1" {
		t.Errorf("resources = %+v, want the project default", job.Spec.Resources)
	}
	payload := payloadFields(t, job.Spec.Payload)
	for key, want := range map[string]any{
		"spec_name": "a", "spec_file_name": "a.spec", "spec_url": gitURL1,
		"commit_id": "c1", "package_name": "repo1",
	} {
		if got := payload[key]; got != want {
			t.Errorf("payload[%s] = %v, want %v", key, got, want)
		}
	}
	if got, want := payload["repo"], []any{
		map[string]any{"url": testRepoURL, "priority": float64(10)},
		map[string]any{"url": "http://bootstrap.local/base/" + testArch, "priority": float64(99)},
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("payload.repo = %v, want %v", got, want)
	}
	if got, want := payload["preinstall"], []any{"rpm-build", "systemd-rpm-macros"}; !reflect.DeepEqual(got, want) {
		t.Errorf("payload.preinstall = %v, want %v", got, want)
	}
	if len(payload) != 7 {
		t.Errorf("payload = %v, want only the seven recognized fields", payload)
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
			payload := payloadFields(t, job.Spec.Payload)
			value, present := payload["disable_check_path"]
			if present != tt.wantPresent || present && value != true {
				t.Fatalf("disable_check_path = %v (present=%t), want present=%t and true when present", value, present, tt.wantPresent)
			}
		})
	}
}

func TestJobPayloadUseKmodLibsMatchesPackageRepo(t *testing.T) {
	tests := []struct {
		name        string
		payload     string
		wantPresent bool
	}{
		{name: "matching repository", payload: "use_kmod_libs:\n- repo1\n", wantPresent: true},
		{name: "spec name is not repository name", payload: "use_kmod_libs:\n- a\n"},
		{name: "other repository", payload: "use_kmod_libs:\n- repo2\n"},
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
			payload := payloadFields(t, job.Spec.Payload)
			value, present := payload["use_kmod_libs"]
			if present != tt.wantPresent || present && value != true {
				t.Fatalf("use_kmod_libs = %v (present=%t), want present=%t and true when present", value, present, tt.wantPresent)
			}
		})
	}
}

func TestJobPayloadUseGitLFSMatchesPackageRepo(t *testing.T) {
	for _, tt := range []struct {
		name        string
		config      string
		wantPresent bool
	}{
		{name: "matching repository", config: "use_git_lfs:\n- repo1\n", wantPresent: true},
		{name: "spec name is not repository name", config: "use_git_lfs:\n- a\n"},
		{name: "other repository", config: "use_git_lfs:\n- repo2\n"},
		{name: "unset"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, client, _, _ := newTestController(t)
			bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
			bi.Spec.BuildPayload = tt.config
			round := &reconcileRound{current: client.SeedBuildInfo(bi), build: testBuildObj("full")}
			depend := dependEntry("a")
			job := c.jobForSpec(round, "a", &depend, testSnapshotObj(), testImage, "", testBuildResourceRules(), testScriptRef(), "job-a", 1, nil)
			payload := payloadFields(t, job.Spec.Payload)
			flag, present := payload["use_git_lfs"]
			if present != tt.wantPresent || present && flag != true {
				t.Fatalf("use_git_lfs = %v (present=%t), want present=%t and true when present", flag, present, tt.wantPresent)
			}
			if name := payload["package_name"]; name != "repo1" {
				t.Fatalf("package_name = %v, want repo1", name)
			}
		})
	}
}

func TestJobPayloadUseRootMatchesPackageRepo(t *testing.T) {
	for _, tt := range []struct {
		name        string
		config      string
		wantPresent bool
	}{
		{name: "matching repository", config: "use_root:\n- repo1\n", wantPresent: true},
		{name: "spec name is not repository name", config: "use_root:\n- a\n"},
		{name: "other repository", config: "use_root:\n- repo2\n"},
		{name: "unset"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, client, _, _ := newTestController(t)
			bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
			bi.Spec.BuildPayload = tt.config
			round := &reconcileRound{current: client.SeedBuildInfo(bi), build: testBuildObj("full")}
			depend := dependEntry("a")
			job := c.jobForSpec(round, "a", &depend, testSnapshotObj(), testImage, "", testBuildResourceRules(), testScriptRef(), "job-a", 1, nil)
			payload := payloadFields(t, job.Spec.Payload)
			flag, present := payload["use_root"]
			if present != tt.wantPresent || present && flag != true {
				t.Fatalf("use_root = %v (present=%t), want present=%t and true when present", flag, present, tt.wantPresent)
			}
		})
	}
}

func TestJobPayloadUseXZMatchesPackageRepo(t *testing.T) {
	for _, tt := range []struct {
		name        string
		config      string
		wantPresent bool
	}{
		{name: "matching repository", config: "use_xz:\n- repo1\n", wantPresent: true},
		{name: "spec name is not repository name", config: "use_xz:\n- a\n"},
		{name: "other repository", config: "use_xz:\n- repo2\n"},
		{name: "unset"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, client, _, _ := newTestController(t)
			bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
			bi.Spec.BuildPayload = tt.config
			round := &reconcileRound{current: client.SeedBuildInfo(bi), build: testBuildObj("full")}
			depend := dependEntry("a")
			job := c.jobForSpec(round, "a", &depend, testSnapshotObj(), testImage, "", testBuildResourceRules(), testScriptRef(), "job-a", 1, nil)
			payload := payloadFields(t, job.Spec.Payload)
			flag, present := payload["use_xz"]
			if present != tt.wantPresent || present && flag != true {
				t.Fatalf("use_xz = %v (present=%t), want present=%t and true when present", flag, present, tt.wantPresent)
			}
		})
	}
}

func TestJobPayloadUnuseGCCSecureMatchesPackageRepo(t *testing.T) {
	for _, tt := range []struct {
		name        string
		config      string
		repoName    string
		wantPresent bool
	}{
		{name: "matching repository", config: "unuse_gcc_secure:\n- repo1\n", repoName: "repo1", wantPresent: true},
		{name: "spec name is not repository name", config: "unuse_gcc_secure:\n- a\n", repoName: "repo1"},
		{name: "other repository", config: "unuse_gcc_secure:\n- repo2\n", repoName: "repo1"},
		{name: "gcc-10 is handled by script", repoName: "gcc-10"},
		{name: "unset", repoName: "repo1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, client, _, _ := newTestController(t)
			bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
			bi.Spec.BuildPayload = tt.config
			round := &reconcileRound{current: client.SeedBuildInfo(bi), build: testBuildObj("full")}
			depend := dependEntry("a")
			depend.RepoName = tt.repoName
			job := c.jobForSpec(round, "a", &depend, testSnapshotObj(), testImage, "", testBuildResourceRules(), testScriptRef(), "job-a", 1, nil)
			payload := payloadFields(t, job.Spec.Payload)
			flag, present := payload["unuse_gcc_secure"]
			if present != tt.wantPresent || present && flag != true {
				t.Fatalf("unuse_gcc_secure = %v (present=%t), want present=%t and true when present", flag, present, tt.wantPresent)
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
	payload := payloadFields(t, job.Spec.Payload)
	if got := payload["prefer"]; got != "rpm-b" {
		t.Fatalf("prefer = %v, want only selected rpm-b", got)
	}
	depend.BuildRequires = map[string]ebsv1.VersionConst{"single": {}}
	job = c.jobForSpec(round, "a", &depend, testSnapshotObj(), testImage, testRepoURL, testBuildResourceRules(), testScriptRef(), "job-b", 1, sources)
	payload = payloadFields(t, job.Spec.Payload)
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
	// commit_id are skipped (log only, never blocks dispatch, 15.3.1).
	snapshot := testSnapshotObj()
	depend := dependEntry("a")

	job := c.jobForSpec(round, "a", &depend, snapshot, testImage, "", testBuildResourceRules(), testScriptRef(), "job-y", 1, nil)

	if strings.Contains(job.Spec.Payload, "spec_url") || strings.Contains(job.Spec.Payload, "commit_id") {
		t.Fatalf("payload = %q, want no spec_url/commit_id without a repo entry", job.Spec.Payload)
	}
	if got := payloadFields(t, job.Spec.Payload)["spec_name"]; got != "a" {
		t.Fatalf("payload spec_name = %v, want a", got)
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
	bi.Status.SpecStatus = ebsv1.NewSpecStatusGroup(map[string]ebsv1.SpecStatus{spec: {}})
	seeded := client.SeedBuildInfo(bi)
	client.SeedBuildResourceRules(testBuildResourceRules())
	round := &reconcileRound{key: key, current: seeded, build: testBuildObj("full"), failures: c.newRoundFailures(key)}
	return round, seeded
}

func testScriptRef() ebsv1.ScriptRef {
	return ebsv1.ScriptRef{Name: "rpmbuild", UID: "61304b92-72cf-4a41-8bf7-8e0a9d14f6a5", ResourceVersion: "1"}
}

func TestDispatchBatchWritesStatusOnce(t *testing.T) {
	c, client, _, _ := newTestController(t)
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	bi.UID = "batch-dispatch"
	bi.Status.SpecStatus = ebsv1.NewSpecStatusGroup(map[string]ebsv1.SpecStatus{"a": {}, "b": {}})
	seeded := client.SeedBuildInfo(bi)
	client.SeedBuildResourceRules(testBuildResourceRules())
	key := testNS + "/" + testBuild
	round := &reconcileRound{key: key, current: seeded, build: testBuildObj("full"), failures: c.newRoundFailures(key)}
	for _, name := range []string{"a", "b"} {
		depend := dependEntry(name)
		if _, err := c.dispatchSpec(context.Background(), round, name, &depend, testSnapshotObj(), testImage, testRepoURL, nil); err != nil {
			t.Fatalf("dispatch %s: %v", name, err)
		}
	}
	if client.statusWrites != 0 || client.resourceReads != 1 {
		t.Fatalf("before flush: status writes=%d, resource reads=%d; want 0 and 1", client.statusWrites, client.resourceReads)
	}
	if _, err := c.flushCreatedJobs(context.Background(), round); err != nil {
		t.Fatalf("flush created jobs: %v", err)
	}
	if client.statusWrites != 1 {
		t.Fatalf("status writes=%d, want one batch confirmation", client.statusWrites)
	}
	for _, name := range []string{"a", "b"} {
		if got := getBuildInfo(t, client).Status.SpecStatus.Entry(name).DispatchCount; got != 1 {
			t.Fatalf("spec %s dispatch count=%d, want 1", name, got)
		}
	}
}

func TestDispatchBatchStatusConflictRecoversFromJobList(t *testing.T) {
	c, client, _, _ := newTestController(t)
	round, _ := dispatchRound(t, c, client, "batch-conflict", "a")
	depend := dependEntry("a")
	if _, err := c.dispatchSpec(context.Background(), round, "a", &depend, testSnapshotObj(), testImage, testRepoURL, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	client.InjectWrite("update-status", clientpkg.WriteRejected, 409, false)
	result, err := c.flushCreatedJobs(context.Background(), round)
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("conflicted flush = %+v, %v, want delayed requeue", result, err)
	}
	if got := getBuildInfo(t, client).Status.SpecStatus.Entry("a").DispatchCount; got != 0 {
		t.Fatalf("dispatch count after rejected flush = %d, want 0", got)
	}

	// A new reconcile starts from the persisted BuildInfo, not the discarded
	// local batch, and recovers the confirmed Job by deterministic identity.
	fresh := &reconcileRound{key: round.key, current: getBuildInfo(t, client), build: round.build, failures: c.newRoundFailures(round.key)}
	jobs, err := c.listRoundJobs(context.Background(), fresh)
	if err != nil {
		t.Fatalf("list Jobs: %v", err)
	}
	next := fresh.current.DeepCopy()
	c.backfillJobs(fresh, next, jobs, map[string]bool{"a": true}, false)
	if _, err := c.writeStatusIfChanged(context.Background(), fresh, next); err != nil {
		t.Fatalf("backfill status: %v", err)
	}
	if got := getBuildInfo(t, client).Status.SpecStatus.Entry("a").DispatchCount; got != 1 {
		t.Fatalf("recovered dispatch count = %d, want 1", got)
	}
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
	client.buildinfos[testNS+"/"+testBuild].Status.SpecStatus.Set("b", ebsv1.SpecStatus{})
	round.current.Status.SpecStatus.Set("b", ebsv1.SpecStatus{})
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

	client.buildinfos[testNS+"/"+testBuild].Status.SpecStatus.Set("c", ebsv1.SpecStatus{})
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
	values := payloadFields(t, payload)
	name, _ := values["rpmbuild_script"].(string)
	return name
}

func payloadFields(t *testing.T, payload string) map[string]any {
	t.Helper()
	var values map[string]any
	if err := json.Unmarshal([]byte(payload), &values); err != nil {
		t.Fatalf("decode Job payload JSON: %v: %s", err, payload)
	}
	return values
}

func TestJobPayloadJSONPreservesLongRepo(t *testing.T) {
	c, client, _, _ := newTestController(t)
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	bi.Spec.BootstrapRepo = []ebsv1.BootstrapRepo{
		{Name: "local", Repo: "https://repo.example.com/openEuler-24.03-LTS-SP3/local"},
		{Name: "everything", Repo: "https://repo.example.com/openEuler-24.03-LTS-SP3/everything"},
	}
	round := &reconcileRound{current: client.SeedBuildInfo(bi), build: testBuildObj("full")}
	contentURL := "http://artifact.example.com/repositories/v1/ceff5ee3568f76de0efdc98e2783f8c1adbf61944b305f736f5152322b5f19da/"
	depend := dependEntry("a")
	job := c.jobForSpec(round, "a", &depend, testSnapshotObj(), testImage, contentURL, testBuildResourceRules(), testScriptRef(), "job-a", 1, nil)
	if strings.Contains(job.Spec.Payload, "\n") {
		t.Fatalf("Job payload should be compact JSON, got %q", job.Spec.Payload)
	}
	want := []any{
		map[string]any{"url": contentURL, "priority": float64(10)},
		map[string]any{"url": "https://repo.example.com/openEuler-24.03-LTS-SP3/local/" + round.build.Spec.BuildTarget.Arch, "priority": float64(99)},
		map[string]any{"url": "https://repo.example.com/openEuler-24.03-LTS-SP3/everything/" + round.build.Spec.BuildTarget.Arch, "priority": float64(99)},
	}
	if got := payloadFields(t, job.Spec.Payload)["repo"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("repo = %v, want %v", got, want)
	}
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

func TestDispatchSpecRejectedJobDoesNotBlockIndependentSpec(t *testing.T) {
	for _, status := range []int{400, 422} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			c, client, _, _ := newTestController(t)
			round, _ := dispatchRound(t, c, client, "rejected-job", "a")
			client.buildinfos[testNS+"/"+testBuild].Status.SpecStatus.Set("b", ebsv1.SpecStatus{})
			round.current.Status.SpecStatus.Set("b", ebsv1.SpecStatus{})
			client.InjectWrite("create", clientpkg.WriteRejected, status, false)
			for _, spec := range []string{"a", "b"} {
				depend := dependEntry(spec)
				result, err := c.dispatchSpec(context.Background(), round, spec, &depend, testSnapshotObj(), testImage, testRepoURL, nil)
				if err != nil || result != (controller.ReconcileResult{}) {
					t.Fatalf("dispatch %s = %+v, %v", spec, result, err)
				}
			}
			if _, err := c.flushCreatedJobs(context.Background(), round); err != nil {
				t.Fatalf("flush created jobs: %v", err)
			}
			stored := getBuildInfo(t, client)
			if got := stored.Status.SpecStatus.Entry("a"); got.Build.Status != SpecBuildFailed || findCondition(got.Build.Conditions, ConditionJobCreateRejected) == nil {
				t.Fatalf("rejected spec status = %+v", got)
			}
			if got := stored.Status.SpecStatus.Entry("b"); got.Build.Status == SpecBuildFailed || got.DispatchCount != 1 {
				t.Fatalf("independent spec status = %+v", got)
			}
			if jobs := listJobs(t, client); len(jobs) != 1 || jobs[0].Labels[ebsv1.JobSpecNameLabel] != "b" {
				t.Fatalf("created Jobs = %+v, want only b", jobs)
			}
		})
	}
}

func TestDispatchSpecForbiddenBlocksRoundAndRecovers(t *testing.T) {
	c, client, _, _ := newTestController(t)
	round, _ := dispatchRound(t, c, client, "forbidden-job", "a")
	client.InjectWrite("create", clientpkg.WriteRejected, 403, false)
	depend := dependEntry("a")
	_, err := c.dispatchSpec(context.Background(), round, "a", &depend, testSnapshotObj(), testImage, testRepoURL, nil)
	if err == nil || !controller.IsPermanent(err) {
		t.Fatalf("forbidden dispatch error = %v, want permanent error", err)
	}
	stored := getBuildInfo(t, client)
	if cond := findCondition(stored.Status.Conditions, ConditionJobDispatchBlocked); cond == nil || cond.Reason != ReasonJobCreateForbidden {
		t.Fatalf("dispatch-blocked condition = %+v", cond)
	}
	if got := stored.Status.SpecStatus.Entry("a").Build.Status; got == SpecBuildFailed {
		t.Fatalf("forbidden dispatch marked spec failed")
	}

	retry := &reconcileRound{key: round.key, current: stored, build: round.build, failures: c.newRoundFailures(round.key)}
	if result, err := c.dispatchSpec(context.Background(), retry, "a", &depend, testSnapshotObj(), testImage, testRepoURL, nil); err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("recovered dispatch = %+v, %v", result, err)
	}
	if _, err := c.flushCreatedJobs(context.Background(), retry); err != nil {
		t.Fatalf("flush recovered Job: %v", err)
	}
	if cond := findCondition(getBuildInfo(t, client).Status.Conditions, ConditionJobDispatchBlocked); cond != nil {
		t.Fatalf("stale dispatch-blocked condition = %+v", cond)
	}
}

func TestDispatchSpecRateLimitRetainsPendingSpec(t *testing.T) {
	c, client, _, _ := newTestController(t)
	round, _ := dispatchRound(t, c, client, "rate-limited-job", "a")
	client.InjectWrite("create", clientpkg.WriteRejected, 429, false)
	depend := dependEntry("a")
	_, err := c.dispatchSpec(context.Background(), round, "a", &depend, testSnapshotObj(), testImage, testRepoURL, nil)
	if err == nil || controller.IsPermanent(err) {
		t.Fatalf("rate-limited dispatch error = %v, want retryable error", err)
	}
	stored := getBuildInfo(t, client)
	if got := stored.Status.SpecStatus.Entry("a").Build.Status; got == SpecBuildFailed {
		t.Fatalf("rate-limited dispatch marked spec failed")
	}
	if client.statusWrites != 0 {
		t.Fatalf("status writes = %d, want none", client.statusWrites)
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
	if got := listJobs(t, client)[0].Name; got != existing.Name {
		t.Fatalf("job name = %q, want existing job %q", got, existing.Name)
	}
	if _, err := c.flushCreatedJobs(context.Background(), round); err != nil {
		t.Fatalf("flush created jobs: %v", err)
	}
	persisted := getBuildInfo(t, client)
	a := persisted.Status.SpecStatus.Entry("a")
	if a.DispatchCount != 1 || a.Build.Status != SpecBuildRunning {
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
	bi.Status.SpecStatus = ebsv1.NewSpecStatusGroup(map[string]ebsv1.SpecStatus{"a": {}})
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
	// 409 without a stored object: the confirmation GET 404 is retryable.
	client.InjectWrite("create", clientpkg.WriteRejected, 409, false)
	depend := dependEntry("a")
	snapshot := testSnapshotObj()

	_, err := c.dispatchSpec(context.Background(), round, "a", &depend, snapshot, testImage, testRepoURL, nil)
	if err == nil || controller.IsPermanent(err) {
		t.Fatalf("dispatchSpec error = %v, want a retryable confirmation-read error", err)
	}
	persisted := getBuildInfo(t, client)
	if len(persisted.Status.PendingJobCreates) != 0 {
		t.Fatalf("pendingJobCreates = %v, want no new registration", persisted.Status.PendingJobCreates)
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
	if _, err := c.flushCreatedJobs(context.Background(), round); err != nil {
		t.Fatalf("flush created jobs: %v", err)
	}
	persisted := getBuildInfo(t, client)
	a := persisted.Status.SpecStatus.Entry("a")
	if a.DispatchCount != 1 {
		t.Fatalf("specStatus[a] = %+v, want the landed job confirmed", a)
	}
	if got := len(listJobs(t, client)); got != 1 {
		t.Fatalf("jobs = %d, want 1", got)
	}
	if len(persisted.Status.PendingJobCreates) != 0 {
		t.Fatalf("pendingJobCreates = %v, want the entry confirmed and removed", persisted.Status.PendingJobCreates)
	}
}

func TestDispatchSpecUnknownMissingRetriesDeterministicName(t *testing.T) {
	c, client, _, _ := newTestController(t)
	round, _ := dispatchRound(t, c, client, "bi-dispatch-unknown-404", "a")
	// Unknown with nothing landed: no pending entry is written. The next
	// attempt uses the same deterministic name and can safely create it.
	client.InjectWrite("create", clientpkg.WriteUnknown, 0, false)
	depend := dependEntry("a")
	snapshot := testSnapshotObj()

	_, err := c.dispatchSpec(context.Background(), round, "a", &depend, snapshot, testImage, testRepoURL, nil)
	if err == nil {
		t.Fatal("dispatchSpec error = nil, want the Unknown write error")
	}
	persisted := getBuildInfo(t, client)
	if len(persisted.Status.PendingJobCreates) != 0 {
		t.Fatalf("pendingJobCreates = %v, want no new registration", persisted.Status.PendingJobCreates)
	}
	if got := len(listJobs(t, client)); got != 0 {
		t.Fatalf("jobs = %d, want 0", got)
	}
	if _, err := c.dispatchSpec(context.Background(), round, "a", &depend, snapshot, testImage, testRepoURL, nil); err != nil {
		t.Fatalf("retry deterministic create: %v", err)
	}
	if got := len(listJobs(t, client)); got != 1 {
		t.Fatalf("jobs after retry = %d, want 1", got)
	}
}
