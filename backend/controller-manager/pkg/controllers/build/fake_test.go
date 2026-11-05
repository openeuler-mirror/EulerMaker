package build

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/clock"
	clocktesting "k8s.io/utils/clock/testing"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

var (
	buildsResource     = schema.GroupResource{Group: "ebs", Resource: "builds"}
	snapshotsResource  = schema.GroupResource{Group: "ebs", Resource: "snapshots"}
	rpmReposResource   = schema.GroupResource{Group: "ebs", Resource: "rpmrepos"}
	buildInfosResource = schema.GroupResource{Group: "ebs", Resource: "buildinfos"}
)

type fakeSource struct{ handler source.ResourceEventHandler }

func (s *fakeSource) Name() string { return "builds" }

func (s *fakeSource) AddEventHandler(handler source.ResourceEventHandler) error {
	s.handler = handler
	return nil
}

func (s *fakeSource) Run(context.Context) error { return nil }

func (s *fakeSource) HasSynced() bool { return true }

func (s *fakeSource) Ready() bool { return true }

type fakeHooks struct {
	getBuildTargetContent func() (*ebsv1.BuildTargetContent, error)
	getProject            func(project string) (*ebsv1.Project, error)
	getBuild              func(project, name string) (*ebsv1.Build, error)
	updateBuild           func(request *ebsv1.Build) (*ebsv1.Build, error)
	updateStatus          func(request *ebsv1.Build) (*ebsv1.Build, error)
	getSnapshot           func(project, name string) (*ebsv1.Snapshot, error)
	createSnapshot        func(request *ebsv1.Snapshot) (*ebsv1.Snapshot, error)
	getRpmRepo            func(project, name string) (*ebsv1.RpmRepo, error)
	createRpmRepo         func(request *ebsv1.RpmRepo) (*ebsv1.RpmRepo, error)
	getBuildInfo          func(project, name string) (*ebsv1.BuildInfo, error)
	createBuildInfo       func(request *ebsv1.BuildInfo) (*ebsv1.BuildInfo, error)
	lastPublished         func(project, targetArch string) (*ebsv1.Build, error)
	deleteResource        func(gvr schema.GroupVersionResource, project, name string, preconditions clientpkg.DeletePreconditions) error
}

// fakeAPI is an in-memory implementation of Client that also records every call, so tests can assert the
// exact API interaction of one reconcile round.
type fakeAPI struct {
	mu            sync.Mutex
	hooks         fakeHooks
	projects      map[string]*ebsv1.Project
	builds        map[string]*ebsv1.Build
	snapshots     map[string]*ebsv1.Snapshot
	rpmRepos      map[string]*ebsv1.RpmRepo
	buildInfos    map[string]*ebsv1.BuildInfo
	revision      int
	calls         []string
	cleanupWrites []string
	cleanupFail   string
}

func (f *fakeAPI) DeleteSnapshot(_ context.Context, obj *ebsv1.Snapshot) error {
	return f.deleteResource(source.SnapshotsGVR, obj.Namespace, obj.Name,
		clientpkg.DeletePreconditions{UID: obj.UID, ResourceVersion: obj.ResourceVersion})
}

func (f *fakeAPI) DeleteBuildInfo(_ context.Context, obj *ebsv1.BuildInfo) error {
	return f.deleteResource(source.BuildInfosGVR, obj.Namespace, obj.Name,
		clientpkg.DeletePreconditions{UID: obj.UID, ResourceVersion: obj.ResourceVersion})
}

func (f *fakeAPI) DeleteRpmRepo(_ context.Context, obj *ebsv1.RpmRepo) error {
	return f.deleteResource(source.RpmReposGVR, obj.Namespace, obj.Name,
		clientpkg.DeletePreconditions{UID: obj.UID, ResourceVersion: obj.ResourceVersion})
}

func (f *fakeAPI) deleteResource(gvr schema.GroupVersionResource, project, name string, preconditions clientpkg.DeletePreconditions) error {
	resource := gvr.Resource
	f.cleanupWrites = append(f.cleanupWrites, resource)
	if f.hooks.deleteResource != nil {
		return f.hooks.deleteResource(gvr, project, name, preconditions)
	}
	k := key(project, name)
	var current metav1.Object
	switch resource {
	case "buildinfos":
		current = f.buildInfos[k]
	case "snapshots":
		current = f.snapshots[k]
	case "rpmrepos":
		current = f.rpmRepos[k]
	default:
		return fmt.Errorf("unexpected direct child: %s", resource)
	}
	if current.GetUID() != preconditions.UID || current.GetResourceVersion() != preconditions.ResourceVersion {
		return fmt.Errorf("stale deletion")
	}
	if resource == f.cleanupFail {
		return fmt.Errorf("injected deletion error")
	}
	if resource == "buildinfos" {
		now := metav1.Now()
		current.SetDeletionTimestamp(&now)
		return nil // The BuildInfo Controller owns descendant cleanup and finalization.
	}
	if resource == "rpmrepos" && slices.Contains(current.GetFinalizers(), ebsv1.RpmRepoCleanupFinalizer) {
		now := metav1.Now()
		current.SetDeletionTimestamp(&now)
		return nil
	}
	if resource == "snapshots" {
		delete(f.snapshots, k)
	} else {
		delete(f.rpmRepos, k)
	}
	return nil
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		projects:   map[string]*ebsv1.Project{},
		builds:     map[string]*ebsv1.Build{},
		snapshots:  map[string]*ebsv1.Snapshot{},
		rpmRepos:   map[string]*ebsv1.RpmRepo{},
		buildInfos: map[string]*ebsv1.BuildInfo{},
		revision:   100,
	}
}

func (f *fakeAPI) GetBuildTargetContent(context.Context) (*ebsv1.BuildTargetContent, error) {
	f.record("GetBuildTargetContent")
	if f.hooks.getBuildTargetContent != nil {
		return f.hooks.getBuildTargetContent()
	}
	return &ebsv1.BuildTargetContent{Targets: map[string]ebsv1.BuildTargetConfigEntry{
		"test-distro": {Arches: map[string]ebsv1.BuildTargetArch{"aarch64": {Image: "build:test"}}},
	}}, nil
}

func (f *fakeAPI) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeAPI) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeAPI) CallCount(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, call := range f.calls {
		if strings.HasPrefix(call, prefix) {
			count++
		}
	}
	return count
}

func (f *fakeAPI) nextRevision() string {
	f.revision++
	return strconv.Itoa(f.revision)
}

func key(project, name string) string { return project + "/" + name }

func (f *fakeAPI) getProject(project string) *ebsv1.Project {
	f.mu.Lock()
	defer f.mu.Unlock()
	value := f.projects[project]
	if value == nil {
		return nil
	}
	return value.DeepCopy()
}

func (f *fakeAPI) build(project, name string) *ebsv1.Build {
	f.mu.Lock()
	defer f.mu.Unlock()
	value := f.builds[key(project, name)]
	if value == nil {
		return nil
	}
	return value.DeepCopy()
}

func (f *fakeAPI) snapshot(project, name string) *ebsv1.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	value := f.snapshots[key(project, name)]
	if value == nil {
		return nil
	}
	return value.DeepCopy()
}

func (f *fakeAPI) rpmRepo(project, name string) *ebsv1.RpmRepo {
	f.mu.Lock()
	defer f.mu.Unlock()
	value := f.rpmRepos[key(project, name)]
	if value == nil {
		return nil
	}
	return value.DeepCopy()
}

func (f *fakeAPI) buildInfo(project, name string) *ebsv1.BuildInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	value := f.buildInfos[key(project, name)]
	if value == nil {
		return nil
	}
	return value.DeepCopy()
}

// storeBuild writes a Build into the store, assigning a fresh resourceVersion.
func (f *fakeAPI) storeBuild(build *ebsv1.Build) *ebsv1.Build {
	f.mu.Lock()
	defer f.mu.Unlock()
	stored := build.DeepCopy()
	stored.ResourceVersion = f.nextRevision()
	f.builds[key(stored.Namespace, stored.Name)] = stored
	return stored.DeepCopy()
}

func (f *fakeAPI) storeSnapshot(snapshot *ebsv1.Snapshot) *ebsv1.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	stored := snapshot.DeepCopy()
	stored.UID = types.UID("snapshot-uid-" + key(stored.Namespace, stored.Name))
	stored.ResourceVersion = f.nextRevision()
	f.snapshots[key(stored.Namespace, stored.Name)] = stored
	return stored.DeepCopy()
}

func (f *fakeAPI) storeRpmRepo(repo *ebsv1.RpmRepo) *ebsv1.RpmRepo {
	f.mu.Lock()
	defer f.mu.Unlock()
	stored := repo.DeepCopy()
	stored.UID = types.UID("rpmrepo-uid-" + key(stored.Namespace, stored.Name))
	stored.ResourceVersion = f.nextRevision()
	f.rpmRepos[key(stored.Namespace, stored.Name)] = stored
	return stored.DeepCopy()
}

func (f *fakeAPI) storeBuildInfo(info *ebsv1.BuildInfo) *ebsv1.BuildInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	stored := info.DeepCopy()
	stored.UID = types.UID("buildinfo-uid-" + key(stored.Namespace, stored.Name))
	stored.ResourceVersion = f.nextRevision()
	f.buildInfos[key(stored.Namespace, stored.Name)] = stored
	return stored.DeepCopy()
}

// commitStatus applies a status write the way the apiserver would: spec and metadata are preserved.
func (f *fakeAPI) commitStatus(request *ebsv1.Build) (*ebsv1.Build, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	stored := f.builds[key(request.Namespace, request.Name)]
	if stored == nil {
		return nil, apierrors.NewNotFound(buildsResource, request.Name)
	}
	if stored.ResourceVersion != request.ResourceVersion {
		return nil, apierrors.NewConflict(buildsResource, request.Name, fmt.Errorf("stale resourceVersion"))
	}
	next := stored.DeepCopy()
	next.Status = request.DeepCopy().Status
	next.ResourceVersion = f.nextRevision()
	f.builds[key(next.Namespace, next.Name)] = next
	return next.DeepCopy(), nil
}

func (f *fakeAPI) commitBuild(request *ebsv1.Build) (*ebsv1.Build, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	stored := f.builds[key(request.Namespace, request.Name)]
	if stored == nil {
		return nil, apierrors.NewNotFound(buildsResource, request.Name)
	}
	if stored.ResourceVersion != request.ResourceVersion {
		return nil, apierrors.NewConflict(buildsResource, request.Name, fmt.Errorf("stale resourceVersion"))
	}
	next := stored.DeepCopy()
	next.Spec = request.DeepCopy().Spec
	next.Finalizers = append([]string(nil), request.Finalizers...)
	next.ResourceVersion = f.nextRevision()
	f.builds[key(next.Namespace, next.Name)] = next
	return next.DeepCopy(), nil
}

func (f *fakeAPI) GetProject(_ context.Context, project string) (*ebsv1.Project, error) {
	f.record("GetProject " + project)
	if f.hooks.getProject != nil {
		return f.hooks.getProject(project)
	}
	value := f.getProject(project)
	if value == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "ebs", Resource: "projects"}, project)
	}
	return value, nil
}

func (f *fakeAPI) GetBuild(_ context.Context, project, name string) (*ebsv1.Build, error) {
	f.record("GetBuild " + key(project, name))
	if f.hooks.getBuild != nil {
		return f.hooks.getBuild(project, name)
	}
	value := f.build(project, name)
	if value == nil {
		return nil, apierrors.NewNotFound(buildsResource, name)
	}
	return value, nil
}

func (f *fakeAPI) GetLastPublishedBuild(_ context.Context, project, targetArch string) (*ebsv1.Build, error) {
	f.record("GetLastPublishedBuild " + project)
	if f.hooks.lastPublished != nil {
		return f.hooks.lastPublished(project, targetArch)
	}
	return nil, nil
}

func (f *fakeAPI) GetSnapshot(_ context.Context, project, name string) (*ebsv1.Snapshot, error) {
	f.record("GetSnapshot " + key(project, name))
	if f.hooks.getSnapshot != nil {
		return f.hooks.getSnapshot(project, name)
	}
	value := f.snapshot(project, name)
	if value == nil {
		return nil, apierrors.NewNotFound(snapshotsResource, name)
	}
	return value, nil
}

func (f *fakeAPI) GetRpmRepo(_ context.Context, project, name string) (*ebsv1.RpmRepo, error) {
	f.record("GetRpmRepo " + key(project, name))
	if f.hooks.getRpmRepo != nil {
		return f.hooks.getRpmRepo(project, name)
	}
	value := f.rpmRepo(project, name)
	if value == nil {
		return nil, apierrors.NewNotFound(rpmReposResource, name)
	}
	return value, nil
}

func (f *fakeAPI) GetBuildInfo(_ context.Context, project, name string) (*ebsv1.BuildInfo, error) {
	f.record("GetBuildInfo " + key(project, name))
	if f.hooks.getBuildInfo != nil {
		return f.hooks.getBuildInfo(project, name)
	}
	value := f.buildInfo(project, name)
	if value == nil {
		return nil, apierrors.NewNotFound(buildInfosResource, name)
	}
	return value, nil
}

func (f *fakeAPI) CreateSnapshot(_ context.Context, project string, request *ebsv1.Snapshot) (*ebsv1.Snapshot, error) {
	f.record("CreateSnapshot " + key(project, request.Name))
	if f.hooks.createSnapshot != nil {
		return f.hooks.createSnapshot(request)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := key(project, request.Name)
	if f.snapshots[id] != nil {
		return nil, apierrors.NewAlreadyExists(snapshotsResource, request.Name)
	}
	if request.Namespace != project {
		return nil, fmt.Errorf("Snapshot namespace %q does not match project %q", request.Namespace, project)
	}
	created := request.DeepCopy()
	created.UID = types.UID("snapshot-uid-" + id)
	created.ResourceVersion = f.nextRevision()
	f.snapshots[id] = created
	return created.DeepCopy(), nil
}

func (f *fakeAPI) CreateRpmRepo(_ context.Context, project string, request *ebsv1.RpmRepo) (*ebsv1.RpmRepo, error) {
	f.record("CreateRpmRepo " + key(project, request.Name))
	if f.hooks.createRpmRepo != nil {
		return f.hooks.createRpmRepo(request)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := key(project, request.Name)
	if f.rpmRepos[id] != nil {
		return nil, apierrors.NewAlreadyExists(rpmReposResource, request.Name)
	}
	created := request.DeepCopy()
	// Match the apiserver: preserve the baseline pair and explicit skip marker.
	seeded := created.Status.Repository
	release := created.Status.Release
	created.Status = ebsv1.RpmRepoStatus{}
	if release != nil && release.Phase == ebsv1.RpmRepoReleaseSkipped {
		created.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseSkipped}
	}
	if seeded != nil {
		created.Status.Repository = &ebsv1.RpmRepoRepositoryStatus{
			RepositoryUID: seeded.RepositoryUID,
			ContentURL:    seeded.ContentURL,
		}
	}
	repository := created.Status.Repository
	if repository != nil && (repository.RepositoryUID == "") != (repository.ContentURL == "") {
		return nil, apierrors.NewInvalid(ebsv1.SchemeGroupVersion.WithKind("RpmRepo").GroupKind(), request.Name,
			field.ErrorList{field.Invalid(field.NewPath("status", "repository"), repository, "invalid initial repository status")})
	}
	created.UID = types.UID("rpmrepo-uid-" + id)
	created.ResourceVersion = f.nextRevision()
	f.rpmRepos[id] = created
	return created.DeepCopy(), nil
}

func (f *fakeAPI) CreateBuildInfo(_ context.Context, project string, request *ebsv1.BuildInfo) (*ebsv1.BuildInfo, error) {
	f.record("CreateBuildInfo " + key(project, request.Name))
	if f.hooks.createBuildInfo != nil {
		return f.hooks.createBuildInfo(request)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := key(project, request.Name)
	if f.buildInfos[id] != nil {
		return nil, apierrors.NewAlreadyExists(buildInfosResource, request.Name)
	}
	created := request.DeepCopy()
	created.UID = types.UID("buildinfo-uid-" + id)
	created.ResourceVersion = f.nextRevision()
	f.buildInfos[id] = created
	return created.DeepCopy(), nil
}

func (f *fakeAPI) UpdateBuildStatus(_ context.Context, request *ebsv1.Build) (*ebsv1.Build, error) {
	f.record("UpdateBuildStatus " + key(request.Namespace, request.Name))
	if f.hooks.updateStatus != nil {
		return f.hooks.updateStatus(request)
	}
	return f.commitStatus(request)
}

func (f *fakeAPI) UpdateBuild(_ context.Context, request *ebsv1.Build) (*ebsv1.Build, error) {
	f.record("UpdateBuild " + key(request.Namespace, request.Name))
	if f.hooks.updateBuild != nil {
		return f.hooks.updateBuild(request)
	}
	return f.commitBuild(request)
}

// fakePollingFactory records the polling source request of an initializer.
type fakePollingFactory struct {
	selectors []string
	gvr       schema.GroupVersionResource
	period    time.Duration
	calls     int
}

func (f *fakePollingFactory) ForResource(gvr schema.GroupVersionResource, period time.Duration, options metav1.ListOptions) (source.Source, error) {
	f.selectors = append(f.selectors, options.FieldSelector)
	f.gvr, f.period = gvr, period
	f.calls++
	return &fakeSource{}, nil
}

func (f *fakePollingFactory) Sources() []source.Source { return nil }

func newTestClock() *clocktesting.FakeClock {
	return clocktesting.NewFakeClock(time.Date(2026, 9, 16, 10, 30, 0, 0, time.UTC))
}

func newTestController(t *testing.T, api Client, clk clock.Clock) *Controller {
	t.Helper()
	c, err := New(&fakeSource{}, api, clk, Config{PollPeriod: time.Second, MaxRetries: 2})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func newProject(name string, repos ...ebsv1.PackageRepo) *ebsv1.Project {
	return &ebsv1.Project{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ebs/v1", Kind: "Project"},
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("project-uid-" + name), ResourceVersion: "1"},
		Spec: ebsv1.ProjectSpec{
			Distro:         "test-distro",
			DefaultRef:     "refs/heads/master",
			PackageRepos:   repos,
			BootstrapRepos: []ebsv1.BootstrapRepo{{Name: "everything", URL: "https://example.com/repo/everything"}},
		},
	}
}

func newPackageRepo(name string) ebsv1.PackageRepo {
	return ebsv1.PackageRepo{
		Name: name,
		URL:  "https://example.com/src-openeuler/" + name + ".git",
		Ref:  "refs/heads/master",
	}
}

func newBuild(project, name string, buildType ebsv1.BuildType, packages []string) *ebsv1.Build {
	build := &ebsv1.Build{
		TypeMeta: metav1.TypeMeta{APIVersion: "ebs/v1", Kind: "Build"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: project, UID: types.UID("build-uid-" + name), ResourceVersion: "10", Generation: 3,
			Finalizers: []string{ebsv1.BuildCleanupFinalizer},
			Labels: map[string]string{
				ebsv1.BuildTargetArchLabel: "aarch64",
				ebsv1.BuildTypeLabel:       string(buildType),
			},
		},
		Spec: ebsv1.BuildSpec{
			BuildType: buildType,
			Packages:  packages,
			Arch:      "aarch64",
		},
	}
	build.Status.Phase = ebsv1.BuildPending
	return build
}

func withBaseBuildRef(build *ebsv1.Build) *ebsv1.Build {
	build.Status.BaseBuildRef = &ebsv1.BaseBuildRef{}
	return build
}

func withPhaseStage(build *ebsv1.Build, phase ebsv1.BuildPhase, stage ebsv1.BuildStage) *ebsv1.Build {
	build.Status.Phase = phase
	build.Status.Stage = stage
	return build
}

func repoNames(repos []ebsv1.PackageRepo) []string {
	names := make([]string, 0, len(repos))
	for _, repo := range repos {
		names = append(names, repo.Name)
	}
	return names
}

func findCondition(t *testing.T, conditions []metav1.Condition, condType string) metav1.Condition {
	t.Helper()
	if condition, ok := testCondition(conditions, condType); ok {
		return condition
	}
	t.Fatalf("condition %s not found in %+v", condType, conditions)
	return metav1.Condition{}
}

func testSpecResults(entries map[string]ebsv1.SpecResult) ebsv1.SpecResults {
	results := ebsv1.SpecResults{}
	for name, result := range entries {
		result.SpecName = name
		results[name+"/"+name+".spec"] = result
	}
	return results
}

func newBuildInfoObject(project, name string, phase ebsv1.BuildInfoPhase, statuses map[string]ebsv1.SpecResult) *ebsv1.BuildInfo {
	return &ebsv1.BuildInfo{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: project},
		Status:     ebsv1.BuildInfoStatus{Phase: phase, SpecResults: testSpecResults(statuses)},
	}
}

func testCondition(conditions []metav1.Condition, condType string) (metav1.Condition, bool) {
	for _, condition := range conditions {
		if condition.Type == condType {
			return condition, true
		}
	}
	return metav1.Condition{}, false
}

// assertConditionTime checks that a freshly written condition carries the round's injected clock value, that
// the round reused a single now for every new timestamp, and that the condition persists as UTC. metav1.Time
// restores local time when it is read back (its UnmarshalJSON calls Local), so UTC is asserted on the
// serialized form instead of the in-memory location.
func assertConditionTime(t *testing.T, condition metav1.Condition, now time.Time, endTime metav1.Time) {
	t.Helper()
	if !condition.LastTransitionTime.Time.Equal(now) {
		t.Fatalf("lastTransitionTime = %+v, want %v", condition.LastTransitionTime, now)
	}
	if !condition.LastTransitionTime.Time.Equal(endTime.Time) {
		t.Fatalf("one round must reuse a single now: condition=%+v endTime=%+v", condition.LastTransitionTime, endTime)
	}
	encoded, err := condition.LastTransitionTime.MarshalJSON()
	if err != nil || !strings.HasSuffix(string(encoded), `Z"`) {
		t.Fatalf("persisted lastTransitionTime must be UTC: %s (err=%v)", encoded, err)
	}
}

func activeSnapshot(project, name string) *ebsv1.Snapshot {
	return &ebsv1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: project},
		Status:     ebsv1.SnapshotStatus{Phase: ebsv1.SnapshotActive},
	}
}

func withBaseBuildRefName(build *ebsv1.Build, name string) *ebsv1.Build {
	build.Status.BaseBuildRef = &ebsv1.BaseBuildRef{Name: name}
	return build
}
