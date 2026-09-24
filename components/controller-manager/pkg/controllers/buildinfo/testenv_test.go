// testenv_test.go is the shared reconcile-level test harness (design 19.1):
// an in-memory fakeSource, a scripted fake git-server, per-test object
// builders and assertion helpers. Tests drive the controller through
// reconcile() with the fakeClient storage of fake_test.go.
package buildinfo

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clocktesting "k8s.io/utils/clock/testing"

	"controller-manager/pkg/clients/gitserver"
	"controller-manager/pkg/controllers/buildinfo/rpmver"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

const (
	testNS      = "proj1"
	testBuild   = "build1"
	testOS      = "openEuler"
	testArch    = "x86_64"
	testRepoURL = "http://repo.local/rpms"
	testImage   = "img:latest"
)

// --- fakeSource ---

type fakeSource struct {
	handler source.ResourceEventHandler
}

func (s *fakeSource) Name() string { return "fake-buildinfos" }
func (s *fakeSource) AddEventHandler(h source.ResourceEventHandler) error {
	s.handler = h
	return nil
}
func (s *fakeSource) Run(context.Context) error { return nil }
func (s *fakeSource) HasSynced() bool           { return true }
func (s *fakeSource) Ready() bool               { return true }

// --- fakeGitServer ---

type gitStub struct {
	out string
	err error
}

type fakeGitServer struct {
	mu    sync.Mutex
	stubs map[string]gitStub
	calls []string
}

func newFakeGitServer() *fakeGitServer {
	return &fakeGitServer{stubs: map[string]gitStub{}}
}

func gitStubKey(cloneURL, command string) string { return cloneURL + "\x00" + command }

// on scripts a successful ExecCommand response.
func (g *fakeGitServer) on(cloneURL, command, out string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stubs[gitStubKey(cloneURL, command)] = gitStub{out: out}
}

// fail scripts a failing ExecCommand response of the given E-23 kind.
func (g *fakeGitServer) fail(cloneURL, command string, kind gitserver.ErrorKind, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stubs[gitStubKey(cloneURL, command)] = gitStub{err: &gitserver.Error{Operation: "exec", Kind: kind, Err: err}}
}

func (g *fakeGitServer) ExecCommand(_ context.Context, cloneURL, command string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, command)
	stub, ok := g.stubs[gitStubKey(cloneURL, command)]
	if !ok {
		return "", &gitserver.Error{Operation: "exec", Kind: gitserver.ErrorPermanent,
			Err: fmt.Errorf("unexpected git command %q on %s", command, cloneURL)}
	}
	return stub.out, stub.err
}

// repo scripts one repository mirror: the ls-tree listing plus one git-show
// per file.
func (g *fakeGitServer) repo(cloneURL, commitID string, files map[string]string) {
	names := make([]string, 0, len(files))
	for file := range files {
		names = append(names, file)
	}
	sort.Strings(names)
	g.on(cloneURL, "git-ls-tree --name-only "+commitID, strings.Join(names, "\n"))
	for file, content := range files {
		g.on(cloneURL, "git-show "+commitID+":"+file, content)
	}
}

func (g *fakeGitServer) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.calls)
}

// --- controller construction ---

func newTestController(t *testing.T) (*Controller, *fakeClient, *fakeGitServer, *clocktesting.FakeClock) {
	t.Helper()
	client := newFakeClient()
	git := newFakeGitServer()
	clk := clocktesting.NewFakeClock(testStart)
	c, err := New(&fakeSource{}, client, git, clk, Config{
		PollPeriod:              time.Minute,
		MaxRetries:              5,
		DcgPruneGrace:           3 * time.Minute,
		RpmRepoReadyRetryLimit:  3,
		SnapshotReadyRetryLimit: 3,
		SpecFileCacheSize:       100,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return c, client, git, clk
}

func reconcileOnce(t *testing.T, c *Controller) {
	t.Helper()
	if _, err := c.reconcile(context.Background(), testNS+"/"+testBuild); err != nil {
		t.Fatalf("reconcile() error = %v", err)
	}
}

// --- object builders ---

func testProjectObj(phase ebsv1.ProjectPhase) *ebsv1.Project {
	return &ebsv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: testNS},
		Status:     ebsv1.ProjectStatus{Phase: phase},
	}
}

func testBuildObj(buildType string, packages ...string) *ebsv1.Build {
	return &ebsv1.Build{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNS,
			Name:      testBuild,
			Labels: map[string]string{
				ebsv1.BuildTargetOSLabel:   testOS,
				ebsv1.BuildTargetArchLabel: testArch,
				ebsv1.BuildTypeLabel:       buildType,
			},
		},
		Spec: ebsv1.BuildSpec{
			BuildType:   buildType,
			Packages:    packages,
			BuildTarget: ebsv1.BuildTarget{Os: testOS, Arch: testArch, BuildFlag: true},
		},
		Status: ebsv1.BuildStatus{Phase: ebsv1.BuildProcessing},
	}
}

func testBuildInfoObj(phase ebsv1.BuildInfoPhase) *ebsv1.BuildInfo {
	return &ebsv1.BuildInfo{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: testBuild},
		Status:     ebsv1.BuildInfoStatus{Phase: phase},
	}
}

// repoEntry describes one snapshot package repository for testSnapshotObj.
type repoEntry struct {
	name     string
	cloneURL string
	commitID string
	declare  bool // present in snapshot.spec.packageRepos
}

func testSnapshotObj(repos ...repoEntry) *ebsv1.Snapshot {
	s := &ebsv1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: testBuild},
		Status:     ebsv1.SnapshotStatus{PackageRepoStatuses: map[string]ebsv1.PackageRepoStatus{}},
	}
	for _, r := range repos {
		if r.declare {
			s.Spec.PackageRepos = append(s.Spec.PackageRepos, ebsv1.PackageRepo{Name: r.name, URL: r.cloneURL})
		}
		if r.commitID != "" {
			s.Status.PackageRepoStatuses[r.name] = ebsv1.PackageRepoStatus{CloneURL: r.cloneURL, CommitID: r.commitID}
		}
	}
	return s
}

func testRpmRepoObj(contentURL string) *ebsv1.RpmRepo {
	return &ebsv1.RpmRepo{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: testBuild},
		Status: ebsv1.RpmRepoStatus{
			Repository: &ebsv1.RpmRepoRepositoryStatus{ContentURL: contentURL},
		},
	}
}

func testBuildTargetContent() *ebsv1.BuildTargetContent {
	return &ebsv1.BuildTargetContent{
		Targets: map[string]ebsv1.BuildTargetConfigEntry{
			testOS: {Arches: map[string]ebsv1.BuildTargetArch{testArch: {Image: testImage}}},
		},
	}
}

func testBuildResourceRules() *buildResourceRules {
	return &buildResourceRules{
		ObjectMeta: metav1.ObjectMeta{Name: ebsv1.BuildResourceConfigName},
		Spec: ebsv1.BuildResourceContent{
			Default:  ebsv1.ResourceRequirements{Requests: map[string]string{"cpu": "1", "memory": "2Gi"}},
			Packages: map[string]ebsv1.PackageResourceConfig{},
		},
	}
}

// seedHealthyBasics seeds the objects every non-single init round needs:
// active project, processing build, build resource and build conf.
func seedHealthyBasics(client *fakeClient, buildType string, packages ...string) *ebsv1.BuildInfo {
	client.SeedProject(testProjectObj(ebsv1.ProjectActive))
	client.SeedBuild(testBuildObj(buildType, packages...))
	client.SeedBuildResourceRules(testBuildResourceRules())
	client.SetBuildTargetContent(testBuildTargetContent())
	return client.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoPending))
}

// specText renders a minimal spec file the fallback parser accepts.
func specText(name string, buildRequires ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Name: %s\nVersion: 1.0\nRelease: 1\nSummary: %s\nLicense: MIT\n", name, name)
	for _, req := range buildRequires {
		fmt.Fprintf(&b, "BuildRequires: %s\n", req)
	}
	b.WriteString("\n%description\ntest\n")
	return b.String()
}

// testJobObj builds one identity-complete Job of the given generation.
func testJobObj(bi *ebsv1.BuildInfo, spec string, generation int64, phase ebsv1.JobPhase) *ebsv1.Job {
	return &ebsv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: bi.Namespace,
			Name:      jobNameFor(string(bi.UID), spec, generation),
			Labels: map[string]string{
				ebsv1.JobBuildNameLabel:    bi.Name,
				ebsv1.JobSpecNameLabel:     spec,
				ebsv1.JobPackageNameLabel:  packageNameLabelValue(spec),
				ebsv1.BuildTargetOSLabel:   testOS,
				ebsv1.BuildTargetArchLabel: testArch,
			},
			Annotations: map[string]string{
				annBuildInfoUID:       string(bi.UID),
				annDispatchGeneration: strconv.FormatInt(generation, 10),
			},
		},
		Status: ebsv1.JobStatus{Phase: phase},
	}
}

// testSources builds one-layer RpmMetaSources whose URL matches testRepoURL,
// so EnsureRepoLayer never downloads in tests.
func testSources(rpms ...rpmver.RpmMeta) *rpmver.RpmMetaSources {
	layer := &rpmver.RpmMetaSource{
		URL:          testRepoURL,
		RpmByName:    map[string]rpmver.RpmMeta{},
		ProvidesInfo: map[string]map[string]rpmver.ProvideEntry{},
	}
	for _, rpm := range rpms {
		layer.RpmByName[rpm.Name] = rpm
		for provide, version := range rpm.Provides {
			if layer.ProvidesInfo[provide] == nil {
				layer.ProvidesInfo[provide] = map[string]rpmver.ProvideEntry{}
			}
			layer.ProvidesInfo[provide][rpm.Name] = rpmver.ProvideEntry{Version: version, SpecName: rpm.SpecName}
		}
	}
	return &rpmver.RpmMetaSources{RepoLayer: layer, BootstrapLayer: []*rpmver.RpmMetaSource{}}
}

// testRpm renders one repo-layer rpm entry providing itself.
func testRpm(name, specName, version string, requires ...string) rpmver.RpmMeta {
	meta := rpmver.RpmMeta{
		Name:     name,
		Version:  version,
		SpecName: specName,
		Provides: map[string]string{name: version},
		Requires: map[string]ebsv1.VersionConst{},
	}
	for _, req := range requires {
		meta.Requires[req] = ebsv1.VersionConst{}
	}
	return meta
}

// --- assertion helpers ---

func getBuildInfo(t *testing.T, client *fakeClient) *ebsv1.BuildInfo {
	t.Helper()
	bi, err := client.GetBuildInfo(context.Background(), testNS, testBuild)
	if err != nil {
		t.Fatalf("GetBuildInfo() error = %v", err)
	}
	return bi
}

func requireCondition(t *testing.T, conds []metav1.Condition, condType, reason string) *metav1.Condition {
	t.Helper()
	cond := findCondition(conds, condType)
	if cond == nil {
		t.Fatalf("condition %s missing, have %v", condType, conds)
	}
	if cond.Reason != reason {
		t.Fatalf("condition %s reason = %q, want %q", condType, cond.Reason, reason)
	}
	return cond
}

func requireNoCondition(t *testing.T, conds []metav1.Condition, condType string) {
	t.Helper()
	if cond := findCondition(conds, condType); cond != nil {
		t.Fatalf("condition %s unexpectedly present: %+v", condType, cond)
	}
}

func requirePhase(t *testing.T, bi *ebsv1.BuildInfo, want ebsv1.BuildInfoPhase) {
	t.Helper()
	if bi.Status.Phase != want {
		t.Fatalf("phase = %q, want %q", bi.Status.Phase, want)
	}
}

func listJobs(t *testing.T, client *fakeClient) []ebsv1.Job {
	t.Helper()
	jobs, err := client.ListJobs(context.Background(), testNS, nil)
	if err != nil {
		t.Fatalf("ListJobs() error = %v", err)
	}
	return jobs
}

// dcgState renders a two-node b-depends-on-a graph for Processing rounds.
func dcgStateAB() map[string]ebsv1.DcgNodeState {
	return map[string]ebsv1.DcgNodeState{
		"a": {Version: "1.0-1", OutDep: []string{"b"}},
		"b": {Version: "1.0-1", InDep: map[string]ebsv1.VersionConst{"a": {}}},
	}
}
