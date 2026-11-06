package snapshot

import (
	"context"
	"errors"
	"fmt"
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

const resolvedCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type fakeSource struct{ handler source.ResourceEventHandler }

func (s *fakeSource) Name() string { return "snapshots" }

func (s *fakeSource) AddEventHandler(handler source.ResourceEventHandler) error {
	s.handler = handler
	return nil
}

func (s *fakeSource) Run(context.Context) error { return nil }

func (s *fakeSource) HasSynced() bool { return true }

func (s *fakeSource) Ready() bool { return true }

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
	mu                          sync.Mutex
	checks, publishes, resolves int
	unsynced                    bool
	publishErr, resolveErr      error
	fullRef, commitID           string
	check                       func(context.Context) (gitserver.SyncCheckResult, error)
}

func (g *fakeGitClient) CheckSynced(ctx context.Context, _ string, _ time.Time) (gitserver.SyncCheckResult, error) {
	g.mu.Lock()
	g.checks++
	synced := !g.unsynced
	g.mu.Unlock()
	if g.check != nil {
		return g.check(ctx)
	}
	return gitserver.SyncCheckResult{Synced: synced, CloneURL: "git-server:///git/example.com/repo.git"}, nil
}

func (g *fakeGitClient) PublishSyncTask(context.Context, string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.publishes++
	return g.publishErr
}

func (g *fakeGitClient) ExecCommand(context.Context, string, string) (string, error) {
	panic("Snapshot Controller must not execute git commands")
}

func (g *fakeGitClient) ResolveFullRef(_ context.Context, _ string, ref string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resolves++
	g.fullRef = ref
	return resolvedCommit, g.resolveErr
}

func (g *fakeGitClient) ResolveCommitID(_ context.Context, _ string, commitID string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resolves++
	g.commitID = commitID
	return commitID, g.resolveErr
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
	snapshot := &ebsv1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name: "build-a", Namespace: "project-a", UID: "snapshot-uid", ResourceVersion: "1", CreationTimestamp: now,
		},
		Spec:   ebsv1.SnapshotSpec{PackageRepos: repos},
		Status: ebsv1.SnapshotStatus{Phase: ebsv1.SnapshotPending},
	}
	build := &ebsv1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a", UID: "build-uid", ResourceVersion: "1"},
		Spec:       ebsv1.BuildSpec{BuildType: ebsv1.BuildTypeFull, Packages: []string{"pkg"}},
	}
	return snapshot, build
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

func branchRepos(n int) []ebsv1.PackageRepo {
	repos := make([]ebsv1.PackageRepo, n)
	for i := range repos {
		repos[i] = ebsv1.PackageRepo{Name: fmt.Sprintf("pkg-%d", i), URL: fmt.Sprintf("https://example.com/pkg-%d.git", i), Ref: "refs/heads/main"}
	}
	return repos
}

var _ source.Source = (*fakeSource)(nil)
var _ Client = (*fakeClient)(nil)
var _ gitserver.GitServerClient = (*fakeGitClient)(nil)
