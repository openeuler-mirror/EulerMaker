package snapshot

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/clients/gitserver"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
)

type fakeSource struct{ handler source.ResourceEventHandler }

func (s *fakeSource) Name() string { return "snapshots" }
func (s *fakeSource) AddEventHandler(handler source.ResourceEventHandler) error {
	s.handler = handler
	return nil
}
func (s *fakeSource) Run(context.Context) error { return nil }
func (s *fakeSource) HasSynced() bool           { return true }
func (s *fakeSource) Ready() bool               { return true }

type fakeClient struct {
	snapshot     *ebsv1.Snapshot
	build        *ebsv1.Build
	updates      int
	unknownFirst bool
}

func (c *fakeClient) GetSnapshot(context.Context, string, string) (*ebsv1.Snapshot, error) {
	if c.snapshot == nil {
		return nil, fmt.Errorf("missing")
	}
	return c.snapshot.DeepCopy(), nil
}
func (c *fakeClient) GetBuild(context.Context, string, string) (*ebsv1.Build, error) {
	return c.build.DeepCopy(), nil
}
func (c *fakeClient) UpdateSnapshotStatus(_ context.Context, request *ebsv1.Snapshot) (*ebsv1.Snapshot, error) {
	c.updates++
	c.snapshot = request.DeepCopy()
	c.snapshot.ResourceVersion = strconv.Itoa(c.updates + 1)
	if c.unknownFirst && c.updates == 1 {
		return nil, &clientpkg.WriteError{Operation: "update-status", Outcome: clientpkg.WriteUnknown, Err: errors.New("lost response")}
	}
	return c.snapshot.DeepCopy(), nil
}

type fakeGitClient struct {
	mu          sync.Mutex
	publishErr  error
	resolvedRef ebsv1.GitRef
}

func (f *fakeGitClient) PublishSyncTask(context.Context, string) error { return f.publishErr }
func (f *fakeGitClient) CheckSynced(context.Context, string, time.Time) (gitserver.SyncCheckResult, error) {
	return gitserver.SyncCheckResult{Synced: true, CloneURL: "git://mirror/repo"}, nil
}
func (f *fakeGitClient) ResolveCommit(_ context.Context, _ string, ref ebsv1.GitRef) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolvedRef = ref
	return "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", nil
}

func TestResolveSnapshotDefaultRef(t *testing.T) {
	for _, tc := range []struct {
		name          string
		ref, fallback ebsv1.GitRef
		skipped       bool
	}{
		{name: "branch fallback", fallback: ebsv1.GitRef{Type: ebsv1.GitRefBranch, Value: "release"}},
		{name: "tag fallback", fallback: ebsv1.GitRef{Type: ebsv1.GitRefTag, Value: "v1"}},
		{name: "explicit wins", ref: ebsv1.GitRef{Type: ebsv1.GitRefBranch, Value: "main"}, fallback: ebsv1.GitRef{Type: ebsv1.GitRefTag, Value: "v1"}},
		{name: "both empty", skipped: true},
		{name: "partial does not inherit", ref: ebsv1.GitRef{Type: ebsv1.GitRefTag}, fallback: ebsv1.GitRef{Type: ebsv1.GitRefBranch, Value: "main"}, skipped: true},
		{name: "explicit commit", ref: ebsv1.GitRef{Type: ebsv1.GitRefCommit, Value: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, fallback: ebsv1.GitRef{Type: ebsv1.GitRefTag, Value: "v1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, build := baseObjects([]ebsv1.PackageRepo{{Name: "pkg", URL: "https://example.com/pkg.git", Ref: tc.ref}})
			snapshot.Spec.DefaultRef = tc.fallback
			git := &fakeGitClient{}
			c := newTestController(t, &fakeClient{snapshot: snapshot, build: build}, git, Config{})
			results, err := c.resolveAll(context.Background(), snapshot, snapshot.Spec.PackageRepos)
			if err != nil || len(results) != 1 {
				t.Fatalf("results=%v err=%v", results, err)
			}
			result := results[0]
			if tc.skipped {
				if result.state != stateSkipped || result.status.Error.Code != ebsv1.SpecCommitValidationFailed {
					t.Fatalf("result=%+v", result)
				}
			} else {
				want := tc.ref
				if want == (ebsv1.GitRef{}) {
					want = tc.fallback
				}
				if result.state != stateResolved {
					t.Fatalf("result=%+v", result)
				}
				if want.Type != ebsv1.GitRefCommit && git.resolvedRef != want {
					t.Fatalf("ref=%+v want=%+v", git.resolvedRef, want)
				}
			}
			if snapshot.Spec.PackageRepos[0].Ref != tc.ref {
				t.Fatal("spec ref mutated")
			}
		})
	}
}

func newTestController(t *testing.T, api *fakeClient, git GitServerClient, config Config) *Controller {
	t.Helper()
	if config == (Config{}) {
		config = Config{PollPeriod: time.Second, ResolveWorkers: 2, ResolveBudget: time.Minute, SyncRequeueDelay: time.Second, FailureLimit: 3, MaxRetries: 2}
	}
	c, err := New(&fakeSource{}, api, git, clock.RealClock{}, config)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func baseObjects(repos []ebsv1.PackageRepo) (*ebsv1.Snapshot, *ebsv1.Build) {
	now := metav1.NewTime(time.Now().Add(-time.Minute))
	snapshot := &ebsv1.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a", UID: types.UID("snapshot-uid"), ResourceVersion: "1", CreationTimestamp: now}, Spec: ebsv1.SnapshotSpec{PackageRepos: repos}, Status: ebsv1.SnapshotStatus{Phase: ebsv1.SnapshotPending}}
	build := &ebsv1.Build{ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a", UID: types.UID("build-uid"), ResourceVersion: "1"}, Spec: ebsv1.BuildSpec{BuildType: "full", Packages: []string{"pkg"}}}
	return snapshot, build
}

func TestPendingCommitSnapshotBecomesActive(t *testing.T) {
	repo := ebsv1.PackageRepo{Name: "pkg", URL: "https://example.com/pkg.git", Ref: ebsv1.GitRef{Type: ebsv1.GitRefCommit, Value: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	snapshot, build := baseObjects([]ebsv1.PackageRepo{repo})
	api := &fakeClient{snapshot: snapshot, build: build}
	c := newTestController(t, api, &fakeGitClient{}, Config{})
	result, err := c.sync(context.Background(), "project-a/build-a")
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if api.updates != 2 || api.snapshot.Status.Phase != ebsv1.SnapshotActive || api.snapshot.Status.PackageRepoStatuses["pkg"].CommitID != repo.Ref.Value {
		t.Fatalf("unexpected snapshot after reconcile: updates=%d status=%+v", api.updates, api.snapshot.Status)
	}
}

func TestAdvancePhaseUnknownIsConfirmedAndContinues(t *testing.T) {
	repo := ebsv1.PackageRepo{Name: "pkg", URL: "https://example.com/pkg.git", Ref: ebsv1.GitRef{Type: ebsv1.GitRefCommit, Value: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	snapshot, build := baseObjects([]ebsv1.PackageRepo{repo})
	api := &fakeClient{snapshot: snapshot, build: build, unknownFirst: true}
	c := newTestController(t, api, &fakeGitClient{}, Config{})
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatal(err)
	}
	if api.updates != 2 || api.snapshot.Status.Phase != ebsv1.SnapshotActive {
		t.Fatalf("updates=%d phase=%s", api.updates, api.snapshot.Status.Phase)
	}
}

func TestOnlySingleBuildUsesPackages(t *testing.T) {
	repos := []ebsv1.PackageRepo{
		{Name: "one", Ref: ebsv1.GitRef{Type: ebsv1.GitRefCommit, Value: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
		{Name: "two", Ref: ebsv1.GitRef{Type: ebsv1.GitRefCommit, Value: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}},
	}
	snapshot, build := baseObjects(repos)
	build.Spec.BuildType, build.Spec.Packages = "single", []string{"one"}
	api := &fakeClient{snapshot: snapshot, build: build}
	c := newTestController(t, api, &fakeGitClient{}, Config{})
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatal(err)
	}
	if len(api.snapshot.Status.PackageRepoStatuses) != 1 || api.snapshot.Status.PackageRepoStatuses["one"].CommitID == "" {
		t.Fatalf("unexpected statuses: %+v", api.snapshot.Status.PackageRepoStatuses)
	}

	snapshot, build = baseObjects(repos)
	build.Spec.BuildType, build.Spec.Packages = "incremental", []string{"one"}
	api = &fakeClient{snapshot: snapshot, build: build}
	c = newTestController(t, api, &fakeGitClient{}, Config{})
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatal(err)
	}
	if len(api.snapshot.Status.PackageRepoStatuses) != 2 {
		t.Fatalf("non-single build did not resolve all repositories: %+v", api.snapshot.Status.PackageRepoStatuses)
	}
}

func TestTemporaryFailureAtLimitIsSkipped(t *testing.T) {
	repo := ebsv1.PackageRepo{Name: "pkg", URL: "https://example.com/pkg.git", Ref: ebsv1.GitRef{Type: ebsv1.GitRefBranch, Value: "main"}}
	snapshot, build := baseObjects([]ebsv1.PackageRepo{repo})
	api := &fakeClient{snapshot: snapshot, build: build}
	gitErr := &gitserver.Error{Operation: "sync", Kind: gitserver.ErrorTemporary, Err: errors.New("unavailable")}
	config := Config{PollPeriod: time.Second, ResolveWorkers: 1, ResolveBudget: time.Minute, SyncRequeueDelay: time.Second, FailureLimit: 1, MaxRetries: 2}
	c := newTestController(t, api, &fakeGitClient{publishErr: gitErr}, config)
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatal(err)
	}
	status := api.snapshot.Status.PackageRepoStatuses["pkg"]
	if api.snapshot.Status.Phase != ebsv1.SnapshotActive || status.Error == nil || status.Error.Code != ebsv1.SpecCommitRetryExhausted || status.Error.Retryable {
		t.Fatalf("unexpected exhausted status: phase=%s status=%+v", api.snapshot.Status.Phase, status)
	}
}

func TestPollingUpdateWithSameResourceVersionEnqueues(t *testing.T) {
	snapshot, build := baseObjects(nil)
	api := &fakeClient{snapshot: snapshot, build: build}
	s := &fakeSource{}
	c, err := New(s, api, &fakeGitClient{}, clock.RealClock{}, Config{PollPeriod: time.Second, ResolveWorkers: 1, ResolveBudget: time.Minute, SyncRequeueDelay: time.Second, FailureLimit: 1, MaxRetries: 2})
	if err != nil {
		t.Fatal(err)
	}
	s.handler.OnUpdate(snapshot.DeepCopy(), snapshot.DeepCopy())
	if c.Queue().Len() != 1 {
		t.Fatalf("queue length=%d, want 1", c.Queue().Len())
	}
}

var _ source.Source = (*fakeSource)(nil)
var _ Client = (*fakeClient)(nil)
var _ GitServerClient = (*fakeGitClient)(nil)
