package snapshot

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/clients/gitserver"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/serviceconfig"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
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
	mu         sync.Mutex
	publishErr error
	fullRef    string
	commitID   string
}

func (f *fakeGitClient) PublishSyncTask(context.Context, string) error { return f.publishErr }
func (f *fakeGitClient) CheckSynced(context.Context, string, time.Time) (gitserver.SyncCheckResult, error) {
	return gitserver.SyncCheckResult{Synced: true, CloneURL: "git-server:///git/example.com/repo.git"}, nil
}
func (f *fakeGitClient) ExecCommand(context.Context, string, string) (string, error) {
	panic("Snapshot Controller must not execute git commands")
}

func (f *fakeGitClient) ResolveFullRef(_ context.Context, _ string, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fullRef = ref
	return "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", nil
}

func (f *fakeGitClient) ResolveCommitID(_ context.Context, _ string, commitID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commitID = commitID
	return commitID, nil
}

func TestResolvePinnedCommit(t *testing.T) {
	const commitID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, ref := range []string{"", "refs/heads/main"} {
		snapshot, build := baseObjects([]ebsv1.PackageRepo{{Name: "pkg", URL: "https://example.com/pkg.git", Ref: ref, CommitID: commitID}})
		git := &fakeGitClient{}
		api := &fakeClient{snapshot: snapshot, build: build}
		c := newTestController(t, api, git, Config{})
		if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
			t.Fatal(err)
		}
		status := api.snapshot.Status.PackageRepoStatuses["pkg"]
		if status.CommitID != commitID || status.CloneURL != "git-server:///git/example.com/repo.git" || git.commitID != commitID {
			t.Fatalf("status=%+v resolved commit=%q", status, git.commitID)
		}
	}
}

func TestResolvePackageRefAndReuseCommit(t *testing.T) {
	for _, ref := range []string{"refs/heads/main", "refs/tags/v1", "refs/merge-requests/501/head"} {
		t.Run(ref, func(t *testing.T) {
			snapshot, build := baseObjects([]ebsv1.PackageRepo{{Name: "pkg", URL: "https://example.com/pkg.git", Ref: ref}})
			build.Spec.BuildType = "single"
			snapshot.Spec.DefaultRef = "refs/heads/default"
			api := &fakeClient{snapshot: snapshot, build: build}
			git := &fakeGitClient{}
			c := newTestController(t, api, git, Config{})
			if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
				t.Fatal(err)
			}
			status := api.snapshot.Status.PackageRepoStatuses["pkg"]
			if api.snapshot.Status.Phase != ebsv1.SnapshotActive || status.CommitID != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" ||
				status.CloneURL != "git-server:///git/example.com/repo.git" || git.fullRef != ref {
				t.Fatalf("phase=%s status=%+v resolved ref=%q", api.snapshot.Status.Phase, status, git.fullRef)
			}
			if api.snapshot.Spec.PackageRepos[0].Ref != ref {
				t.Fatal("resolution must preserve the frozen package ref")
			}
			// A new controller also reuses a committed package result while the Snapshot is Processing.
			api.snapshot.Status.Phase = ebsv1.SnapshotProcessing
			git.fullRef = ""
			c = newTestController(t, api, git, Config{})
			if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
				t.Fatal(err)
			}
			if git.fullRef != "" || api.snapshot.Status.PackageRepoStatuses["pkg"] != status {
				t.Fatal("persisted package commit must be reused after restart")
			}
		})
	}
}

func TestResolveSnapshotDefaultRef(t *testing.T) {
	for _, tc := range []struct {
		name          string
		ref, fallback string
		skipped       bool
	}{
		{name: "branch fallback", fallback: "refs/heads/release"},
		{name: "tag fallback", fallback: "refs/tags/v1"},
		{name: "explicit wins", ref: "refs/heads/main", fallback: "refs/tags/v1"},
		{name: "both empty", skipped: true},
		{name: "explicit full ref", ref: "refs/merge-requests/501/head", fallback: "refs/tags/v1"},
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
				if want == "" {
					want = tc.fallback
				}
				if result.state != stateResolved {
					t.Fatalf("result=%+v", result)
				}
				if git.fullRef != want {
					t.Fatalf("ref=%+v want=%+v", git.fullRef, want)
				}
			}
			if snapshot.Spec.PackageRepos[0].Ref != tc.ref {
				t.Fatal("spec ref mutated")
			}
		})
	}
}

type fakeServices struct {
	git   gitserver.GitServerClient
	err   error
	loads int
}

func TestServiceConfigFailurePreservesPackageBudget(t *testing.T) {
	snapshot, build := baseObjects([]ebsv1.PackageRepo{{Name: "pkg", URL: "https://example.com/pkg.git", Ref: "refs/heads/main"}})
	api := &fakeClient{snapshot: snapshot, build: build}
	c := newTestController(t, api, &fakeGitClient{}, Config{})
	services := c.services.(*fakeServices)
	services.err = &serviceconfig.Error{Err: context.DeadlineExceeded}
	for i := 0; i < 6; i++ {
		if _, err := c.sync(context.Background(), "project-a/build-a"); !serviceconfig.IsError(err) {
			t.Fatalf("expected configuration error: %v", err)
		}
		if c.failures.count(snapshot.UID, "pkg") != 0 || len(api.snapshot.Status.PackageRepoStatuses) != 0 {
			t.Fatal("configuration failure consumed package retry budget")
		}
	}
	services.err = nil
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatal(err)
	}
	if api.snapshot.Status.Phase != ebsv1.SnapshotActive {
		t.Fatalf("phase=%s", api.snapshot.Status.Phase)
	}
	loads := services.loads
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatal(err)
	}
	if services.loads != loads {
		t.Fatal("terminal Snapshot read service configuration")
	}
}

func (f *fakeServices) Load(context.Context) (*ebsv1.ServiceConfigContent, error) {
	f.loads++
	return &ebsv1.ServiceConfigContent{GitServer: ebsv1.ServiceEndpoints{InternalURL: "http://git-server:8080"}}, f.err
}

func (f *fakeServices) GitClient(string) (gitserver.GitServerClient, error) { return f.git, nil }

func newTestController(t *testing.T, api *fakeClient, git gitserver.GitServerClient, config Config) *Controller {
	t.Helper()
	if config == (Config{}) {
		config = Config{PollPeriod: time.Second, ResolveWorkers: 2, ResolveBudget: time.Minute, SyncRequeueDelay: time.Second, FailureLimit: 3, MaxRetries: 2}
	}
	c, err := New(&fakeSource{}, api, &fakeServices{git: git}, clock.RealClock{}, config)
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

func TestPendingSnapshotResolvesRefAndBecomesActive(t *testing.T) {
	repo := ebsv1.PackageRepo{Name: "pkg", URL: "https://example.com/pkg.git", Ref: "refs/heads/main"}
	snapshot, build := baseObjects([]ebsv1.PackageRepo{repo})
	api := &fakeClient{snapshot: snapshot, build: build}
	c := newTestController(t, api, &fakeGitClient{}, Config{})
	result, err := c.sync(context.Background(), "project-a/build-a")
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if api.updates != 2 || api.snapshot.Status.Phase != ebsv1.SnapshotActive || api.snapshot.Status.PackageRepoStatuses["pkg"].CommitID != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("unexpected snapshot after reconcile: updates=%d status=%+v", api.updates, api.snapshot.Status)
	}
}

func TestAdvancePhaseUnknownIsConfirmedAndContinues(t *testing.T) {
	repo := ebsv1.PackageRepo{Name: "pkg", URL: "https://example.com/pkg.git", Ref: "refs/heads/main"}
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
		{Name: "one", Ref: "refs/heads/main"},
		{Name: "two", Ref: "refs/tags/v1"},
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
	repo := ebsv1.PackageRepo{Name: "pkg", URL: "https://example.com/pkg.git", Ref: "refs/heads/main"}
	snapshot, build := baseObjects([]ebsv1.PackageRepo{repo})
	api := &fakeClient{snapshot: snapshot, build: build}
	gitErr := &gitserver.Error{Operation: "sync", Kind: gitserver.ErrorTemporary, Err: errors.New("unavailable")}
	config := Config{PollPeriod: time.Second, ResolveWorkers: 1, ResolveBudget: time.Minute, SyncRequeueDelay: time.Second, FailureLimit: 1, MaxRetries: 2}
	c := newTestController(t, api, &checkingGit{check: func(context.Context) (gitserver.SyncCheckResult, error) {
		return gitserver.SyncCheckResult{}, gitErr
	}}, config)
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
	c, err := New(s, api, &fakeServices{git: &fakeGitClient{}}, clock.RealClock{}, Config{PollPeriod: time.Second, ResolveWorkers: 1, ResolveBudget: time.Minute, SyncRequeueDelay: time.Second, FailureLimit: 1, MaxRetries: 2})
	if err != nil {
		t.Fatal(err)
	}
	projected := snapshot.DeepCopy()
	projected.Status = ebsv1.SnapshotStatus{} // includeFields=metadata omits phase.
	s.handler.OnUpdate(projected.DeepCopy(), projected)
	if c.Queue().Len() != 1 {
		t.Fatalf("queue length=%d, want 1", c.Queue().Len())
	}
}

func TestPollingDeletedSnapshotIsNotEnqueued(t *testing.T) {
	snapshot, build := baseObjects(nil)
	api := &fakeClient{snapshot: snapshot, build: build}
	s := &fakeSource{}
	c, err := New(s, api, &fakeServices{git: &fakeGitClient{}}, clock.RealClock{}, Config{PollPeriod: time.Second, ResolveWorkers: 1, ResolveBudget: time.Minute, SyncRequeueDelay: time.Second, FailureLimit: 1, MaxRetries: 2})
	if err != nil {
		t.Fatal(err)
	}
	projected := snapshot.DeepCopy()
	projected.Status = ebsv1.SnapshotStatus{}
	now := metav1.Now()
	projected.DeletionTimestamp = &now
	s.handler.OnAdd(projected)
	if c.Queue().Len() != 0 {
		t.Fatalf("deleted Snapshot queued, length=%d", c.Queue().Len())
	}
}

var _ source.Source = (*fakeSource)(nil)
var _ Client = (*fakeClient)(nil)
var _ gitserver.GitServerClient = (*fakeGitClient)(nil)
