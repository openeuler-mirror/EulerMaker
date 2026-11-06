package snapshot

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/clients/gitserver"
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
	snapshot  *ebsv1.Snapshot
	build     *ebsv1.Build
	updates   int
	unknownAt int
	updateErr error
}

func (c *fakeClient) GetSnapshot(_ context.Context, _, name string) (*ebsv1.Snapshot, error) {
	if c.snapshot == nil {
		return nil, apierrors.NewNotFound(source.SnapshotsGVR.GroupResource(), name)
	}
	return c.snapshot.DeepCopy(), nil
}

func (c *fakeClient) GetBuild(_ context.Context, _, name string) (*ebsv1.Build, error) {
	if c.build == nil {
		return nil, apierrors.NewNotFound(source.BuildsGVR.GroupResource(), name)
	}
	return c.build.DeepCopy(), nil
}

func (c *fakeClient) UpdateSnapshotStatus(_ context.Context, request *ebsv1.Snapshot) (*ebsv1.Snapshot, error) {
	if c.updateErr != nil {
		return nil, c.updateErr
	}
	c.updates++
	c.snapshot = request.DeepCopy()
	c.snapshot.ResourceVersion = strconv.Itoa(c.updates + 1)
	if c.unknownAt == c.updates {
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

type fakeServices struct {
	git   gitserver.GitServerClient
	err   error
	loads int
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
	if api.snapshot.Status.Phase != ebsv1.SnapshotActive || status.Error == nil || status.Error.Code != ebsv1.PackageRepoRetryExhausted || status.Error.Retryable {
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

func TestPollingDeletionClearsTracker(t *testing.T) {
	for _, event := range []string{"add-deleting", "update-deleting", "delete"} {
		t.Run(event, func(t *testing.T) {
			snapshot, build := baseObjects(nil)
			api := &fakeClient{snapshot: snapshot, build: build}
			s := &fakeSource{}
			c, err := New(s, api, &fakeServices{git: &fakeGitClient{}}, clock.RealClock{}, Config{
				PollPeriod: time.Second, ResolveWorkers: 1, ResolveBudget: time.Minute,
				SyncRequeueDelay: time.Second, FailureLimit: 1, MaxRetries: 2,
			})
			if err != nil {
				t.Fatal(err)
			}
			c.failures.observe("project-a/build-a", snapshot.UID)
			c.failures.set(snapshot.UID, "pkg", 2)
			c.failures.setCursor(snapshot.UID, 1)
			projected := snapshot.DeepCopy()
			projected.Status = ebsv1.SnapshotStatus{}
			if event == "delete" {
				s.handler.OnDelete(projected)
			} else {
				now := metav1.Now()
				projected.DeletionTimestamp = &now
				if event == "add-deleting" {
					s.handler.OnAdd(projected)
				} else {
					s.handler.OnUpdate(snapshot, projected)
				}
			}
			assertTrackerCleared(t, c.failures, snapshot.UID)
			if c.Queue().Len() != 0 {
				t.Fatalf("deleted Snapshot queued, length=%d", c.Queue().Len())
			}
		})
	}
}

var _ source.Source = (*fakeSource)(nil)
var _ Client = (*fakeClient)(nil)
var _ gitserver.GitServerClient = (*fakeGitClient)(nil)

func TestOldSnapshotDeletionPreservesReplacementTracker(t *testing.T) {
	snapshot, build := baseObjects(nil)
	c := newTestController(t, &fakeClient{snapshot: snapshot, build: build}, &fakeGitClient{}, Config{})
	c.failures.observe("project-a/build-a", snapshot.UID)
	c.failures.set(snapshot.UID, "pkg", 2)
	c.failures.observe("project-a/build-a", "replacement-uid")
	c.failures.set("replacement-uid", "pkg", 1)
	c.failures.setCursor("replacement-uid", 3)
	c.onDelete(snapshot)
	assertTrackerCleared(t, c.failures, snapshot.UID)
	if c.failures.count("replacement-uid", "pkg") != 1 || c.failures.cursor("replacement-uid") != 3 {
		t.Fatal("stale deletion cleared the replacement's tracker")
	}
}

func assertTrackerCleared(t *testing.T, tracker *failureTracker, uid types.UID) {
	t.Helper()
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if _, ok := tracker.cursors[uid]; ok {
		t.Fatalf("cursor retained for %s", uid)
	}
	for key := range tracker.counts {
		if key.uid == uid {
			t.Fatalf("failure count retained for %s", uid)
		}
	}
	for _, current := range tracker.uidByObjectKey {
		if current == uid {
			t.Fatalf("object association retained for %s", uid)
		}
	}
}
