package buildinfo

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	clientpkg "controller-manager/pkg/clients/apiserver"
	ebsv1 "ebs-api/ebs/v1"
)

// fakeClient is the in-package fake for the typed apiserver Client (design
// 4.3). It keeps objects in memory maps and reproduces the server behaviors
// the controller relies on: NotFound, 409 Conflict on stale resourceVersion,
// 409 AlreadyExists on duplicate CreateJob, resourceVersion increments,
// /status writes preserving the old spec, WriteError three-outcome injection
// (Unknown may optionally persist the write, so confirmation reads return the
// actual persisted state), and label-filtered ListJobs.
type fakeClient struct {
	mu                 sync.Mutex
	buildinfos         map[string]*ebsv1.BuildInfo
	jobs               map[string]*ebsv1.Job
	builds             map[string]*ebsv1.Build
	projects           map[string]*ebsv1.Project
	scripts            map[string]*ebsv1.Script
	scriptReads        int
	snapshots          map[string]*ebsv1.Snapshot
	rpmrepos           map[string]*ebsv1.RpmRepo
	buildResourceRules map[string]*buildResourceRules
	buildTargetContent *ebsv1.BuildTargetContent
	buildTargetFailed  bool

	// injected per-operation write failures, consumed once each.
	injectedWrites map[string]*injectedWrite
	// injected per-resource-kind read failures with a countdown.
	injectedReads map[string]*injectedRead

	rv  int
	uid int
}

type injectedWrite struct {
	outcome    clientpkg.WriteOutcome
	statusCode int
	// persist applies the write to storage before returning the injected
	// error (used to simulate an Unknown outcome whose intent landed).
	persist bool
	err     error
}

type injectedRead struct {
	times int
	err   error
}

var _ Client = (*fakeClient)(nil)

func newFakeClient() *fakeClient {
	return &fakeClient{
		buildinfos: make(map[string]*ebsv1.BuildInfo),
		jobs:       make(map[string]*ebsv1.Job),
		builds:     make(map[string]*ebsv1.Build),
		projects:   make(map[string]*ebsv1.Project),
		scripts: map[string]*ebsv1.Script{
			"rpmbuild": {ObjectMeta: metav1.ObjectMeta{Name: "rpmbuild", UID: "61304b92-72cf-4a41-8bf7-8e0a9d14f6a5", ResourceVersion: "1"}, Spec: ebsv1.ScriptSpec{Content: "#!/bin/sh\n"}},
		},
		snapshots:          make(map[string]*ebsv1.Snapshot),
		rpmrepos:           make(map[string]*ebsv1.RpmRepo),
		buildResourceRules: make(map[string]*buildResourceRules),
		injectedWrites:     make(map[string]*injectedWrite),
		injectedReads:      make(map[string]*injectedRead),
		rv:                 100,
	}
}

// InjectWrite makes the next write with the given operation ("update-status"
// or "create") return a WriteError with the injected outcome instead of
// performing the write. For Unknown, persist controls whether the write lands
// in storage anyway, so a follow-up confirmation read observes the intent.
func (f *fakeClient) InjectWrite(operation string, outcome clientpkg.WriteOutcome, statusCode int, persist bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.injectedWrites[operation] = &injectedWrite{outcome: outcome, statusCode: statusCode, persist: persist}
}

// InjectRead makes the next times reads of the given resource kind ("buildinfos",
// "jobs", "rpmrepos", ...) return err instead of hitting storage.
func (f *fakeClient) InjectRead(kind string, times int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.injectedReads[kind] = &injectedRead{times: times, err: err}
}

// SetBuildTargetContent injects the build-target Config singleton (nil restores not-found).
func (f *fakeClient) SetBuildTargetContent(conf *ebsv1.BuildTargetContent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buildTargetContent = conf
}

// FailBuildTargetContent makes GetBuildTargetContent return a query failure (E-26).
func (f *fakeClient) FailBuildTargetContent() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buildTargetFailed = true
}

// Seed helpers pre-populate storage with server-assigned metadata and return
// the stored copy.

func (f *fakeClient) SeedBuildInfo(value *ebsv1.BuildInfo) *ebsv1.BuildInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	if value.UID == "" {
		value.UID = types.UID(f.nextUIDLocked())
	}
	if value.ResourceVersion == "" {
		value.ResourceVersion = f.nextRVLocked()
	}
	f.buildinfos[value.Namespace+"/"+value.Name] = value.DeepCopy()
	return value.DeepCopy()
}

func (f *fakeClient) SeedJob(value *ebsv1.Job) *ebsv1.Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	stored := value.DeepCopy()
	if stored.UID == "" {
		stored.UID = types.UID(f.nextUIDLocked())
	}
	if stored.ResourceVersion == "" {
		stored.ResourceVersion = f.nextRVLocked()
	}
	if stored.CreationTimestamp.IsZero() {
		stored.CreationTimestamp = metav1.NewTime(time.Now())
	}
	f.jobs[stored.Namespace+"/"+stored.Name] = stored
	return stored.DeepCopy()
}

func (f *fakeClient) SeedBuild(value *ebsv1.Build) *ebsv1.Build {
	f.mu.Lock()
	defer f.mu.Unlock()
	stored := value.DeepCopy()
	if stored.UID == "" {
		stored.UID = types.UID(f.nextUIDLocked())
	}
	if stored.ResourceVersion == "" {
		stored.ResourceVersion = f.nextRVLocked()
	}
	if stored.CreationTimestamp.IsZero() {
		stored.CreationTimestamp = metav1.NewTime(time.Now())
	}
	f.builds[stored.Namespace+"/"+stored.Name] = stored
	return stored.DeepCopy()
}

func (f *fakeClient) SeedProject(value *ebsv1.Project) *ebsv1.Project {
	f.mu.Lock()
	defer f.mu.Unlock()
	stored := value.DeepCopy()
	if stored.UID == "" {
		stored.UID = types.UID(f.nextUIDLocked())
	}
	if stored.ResourceVersion == "" {
		stored.ResourceVersion = f.nextRVLocked()
	}
	f.projects[stored.Name] = stored
	return stored.DeepCopy()
}

func (f *fakeClient) SeedSnapshot(value *ebsv1.Snapshot) *ebsv1.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	stored := value.DeepCopy()
	if stored.UID == "" {
		stored.UID = types.UID(f.nextUIDLocked())
	}
	if stored.ResourceVersion == "" {
		stored.ResourceVersion = f.nextRVLocked()
	}
	f.snapshots[stored.Namespace+"/"+stored.Name] = stored
	return stored.DeepCopy()
}

func (f *fakeClient) SeedRpmRepo(value *ebsv1.RpmRepo) *ebsv1.RpmRepo {
	f.mu.Lock()
	defer f.mu.Unlock()
	stored := value.DeepCopy()
	if stored.UID == "" {
		stored.UID = types.UID(f.nextUIDLocked())
	}
	if stored.ResourceVersion == "" {
		stored.ResourceVersion = f.nextRVLocked()
	}
	f.rpmrepos[stored.Namespace+"/"+stored.Name] = stored
	return stored.DeepCopy()
}

func (f *fakeClient) SeedBuildResourceRules(value *buildResourceRules) *buildResourceRules {
	f.mu.Lock()
	defer f.mu.Unlock()
	stored := value.DeepCopy()
	if stored.UID == "" {
		stored.UID = types.UID(f.nextUIDLocked())
	}
	if stored.ResourceVersion == "" {
		stored.ResourceVersion = f.nextRVLocked()
	}
	f.buildResourceRules[stored.Name] = stored
	return stored.DeepCopy()
}

func (f *fakeClient) nextRVLocked() string { f.rv++; return strconv.Itoa(f.rv) }

func (f *fakeClient) nextUIDLocked() string { f.uid++; return fmt.Sprintf("fake-uid-%d", f.uid) }

// consumeInjectedReadLocked returns the injected read error for the kind, or nil.
func (f *fakeClient) consumeInjectedReadLocked(kind string) error {
	injected := f.injectedReads[kind]
	if injected == nil || injected.times <= 0 {
		return nil
	}
	injected.times--
	if injected.times == 0 {
		delete(f.injectedReads, kind)
	}
	return injected.err
}

// consumeInjectedWriteLocked returns the injected write for the operation, or nil.
func (f *fakeClient) consumeInjectedWriteLocked(operation string) *injectedWrite {
	injected := f.injectedWrites[operation]
	if injected != nil {
		delete(f.injectedWrites, operation)
	}
	return injected
}

func (f *fakeClient) GetBuildInfo(_ context.Context, namespace, name string) (*ebsv1.BuildInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.consumeInjectedReadLocked("buildinfos"); err != nil {
		return nil, err
	}
	value, ok := f.buildinfos[namespace+"/"+name]
	if !ok {
		return nil, ErrNotFound
	}
	return value.DeepCopy(), nil
}

func (f *fakeClient) UpdateBuildInfoStatus(_ context.Context, obj *ebsv1.BuildInfo) (*ebsv1.BuildInfo, error) {
	if obj == nil {
		return nil, notSentFake("update-status", "buildinfos", fmt.Errorf("BuildInfo request is required"))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	stored, ok := f.buildinfos[obj.Namespace+"/"+obj.Name]
	if !ok {
		return nil, rejectedFake("update-status", "buildinfos", 404, fmt.Errorf("not found"))
	}
	if injected := f.consumeInjectedWriteLocked("update-status"); injected != nil {
		if injected.persist {
			f.applyStatusLocked(stored, obj)
		}
		return nil, injectedFake("update-status", "buildinfos", injected)
	}
	if stored.ResourceVersion != obj.ResourceVersion {
		return nil, rejectedFake("update-status", "buildinfos", 409, fmt.Errorf("resourceVersion conflict"))
	}
	f.applyStatusLocked(stored, obj)
	return stored.DeepCopy(), nil
}

// applyStatusLocked replaces only the status of the stored BuildInfo (the
// /status subresource keeps the old spec) and bumps resourceVersion.
func (f *fakeClient) applyStatusLocked(stored *ebsv1.BuildInfo, intent *ebsv1.BuildInfo) {
	stored.Status = intent.DeepCopy().Status
	stored.ResourceVersion = f.nextRVLocked()
}

func (f *fakeClient) CreateJob(_ context.Context, project string, obj *ebsv1.Job) (*ebsv1.Job, error) {
	if obj == nil {
		return nil, notSentFake("create", "jobs", fmt.Errorf("Job request is required"))
	}
	if obj.Namespace != project || obj.UID != "" || obj.ResourceVersion != "" {
		return nil, notSentFake("create", "jobs", fmt.Errorf("Job metadata does not match the create target"))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := project + "/" + obj.Name
	if injected := f.consumeInjectedWriteLocked("create"); injected != nil {
		if injected.persist {
			f.createJobLocked(key, obj)
		}
		return nil, injectedFake("create", "jobs", injected)
	}
	if _, exists := f.jobs[key]; exists {
		return nil, rejectedFake("create", "jobs", 409, fmt.Errorf("already exists"))
	}
	return f.createJobLocked(key, obj), nil
}

func (f *fakeClient) createJobLocked(key string, obj *ebsv1.Job) *ebsv1.Job {
	stored := obj.DeepCopy()
	stored.UID = types.UID(f.nextUIDLocked())
	stored.ResourceVersion = f.nextRVLocked()
	if stored.CreationTimestamp.IsZero() {
		stored.CreationTimestamp = metav1.NewTime(time.Now())
	}
	f.jobs[key] = stored
	return stored.DeepCopy()
}

func (f *fakeClient) GetJob(_ context.Context, project, name string) (*ebsv1.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.consumeInjectedReadLocked("jobs"); err != nil {
		return nil, err
	}
	value, ok := f.jobs[project+"/"+name]
	if !ok {
		return nil, ErrNotFound
	}
	return value.DeepCopy(), nil
}

func (f *fakeClient) ListJobs(_ context.Context, project string, selector labels.Selector) ([]ebsv1.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.consumeInjectedReadLocked("jobs"); err != nil {
		return nil, err
	}
	if selector == nil {
		selector = labels.Everything()
	}
	var out []ebsv1.Job
	for _, job := range f.jobs {
		if job.Namespace != project {
			continue
		}
		if selector.Matches(labels.Set(job.Labels)) {
			out = append(out, *job.DeepCopy())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		left, right := out[i], out[j]
		if left.CreationTimestamp.Equal(&right.CreationTimestamp) {
			return left.Name < right.Name
		}
		return left.CreationTimestamp.Before(&right.CreationTimestamp)
	})
	return out, nil
}

func (f *fakeClient) GetBuild(_ context.Context, project, name string) (*ebsv1.Build, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.consumeInjectedReadLocked("builds"); err != nil {
		return nil, err
	}
	value, ok := f.builds[project+"/"+name]
	if !ok {
		return nil, ErrNotFound
	}
	return value.DeepCopy(), nil
}

func (f *fakeClient) GetRpmRepo(_ context.Context, project, name string) (*ebsv1.RpmRepo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.consumeInjectedReadLocked("rpmrepos"); err != nil {
		return nil, err
	}
	value, ok := f.rpmrepos[project+"/"+name]
	if !ok {
		return nil, ErrNotFound
	}
	return value.DeepCopy(), nil
}

func (f *fakeClient) GetSnapshot(_ context.Context, project, name string) (*ebsv1.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.consumeInjectedReadLocked("snapshots"); err != nil {
		return nil, err
	}
	value, ok := f.snapshots[project+"/"+name]
	if !ok {
		return nil, ErrNotFound
	}
	return value.DeepCopy(), nil
}

func (f *fakeClient) GetProject(_ context.Context, project string) (*ebsv1.Project, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.consumeInjectedReadLocked("projects"); err != nil {
		return nil, err
	}
	value, ok := f.projects[project]
	if !ok {
		return nil, ErrNotFound
	}
	return value.DeepCopy(), nil
}

func (f *fakeClient) GetScript(_ context.Context, name string) (*ebsv1.Script, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scriptReads++
	if err := f.consumeInjectedReadLocked("scripts"); err != nil {
		return nil, err
	}
	value, ok := f.scripts[name]
	if !ok {
		return nil, ErrNotFound
	}
	return value.DeepCopy(), nil
}

func (f *fakeClient) GetBuildResourceRules(_ context.Context) (*buildResourceRules, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.consumeInjectedReadLocked("buildResourceRules"); err != nil {
		return nil, err
	}
	value, ok := f.buildResourceRules[ebsv1.BuildResourceConfigName]
	if !ok {
		return nil, ErrNotFound
	}
	return value.DeepCopy(), nil
}

func (f *fakeClient) GetBuildTargetContent(_ context.Context) (*ebsv1.BuildTargetContent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.buildTargetFailed {
		return nil, fmt.Errorf("injected build-target Config query failure")
	}
	if f.buildTargetContent == nil {
		return nil, ErrNotFound
	}
	return f.buildTargetContent.DeepCopy(), nil
}

func notSentFake(operation, resource string, err error) error {
	return &clientpkg.WriteError{Operation: operation, Resource: groupResource(resource), Outcome: clientpkg.WriteNotSent, Err: err}
}

func rejectedFake(operation, resource string, statusCode int, err error) error {
	return &clientpkg.WriteError{Operation: operation, Resource: groupResource(resource), Outcome: clientpkg.WriteRejected, StatusCode: statusCode, Err: err}
}

func injectedFake(operation, resource string, injected *injectedWrite) error {
	if injected.err != nil {
		return injected.err
	}
	return &clientpkg.WriteError{
		Operation:  operation,
		Resource:   groupResource(resource),
		Outcome:    injected.outcome,
		StatusCode: injected.statusCode,
		Err:        fmt.Errorf("injected %s failure", injected.outcome),
	}
}

func groupResource(resource string) schema.GroupResource {
	return schema.GroupResource{Group: "ebs", Resource: resource}
}
