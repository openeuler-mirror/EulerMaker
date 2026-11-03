package buildinfo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	ebsv1 "ebs-api/ebs/v1"
)

// Business tests must opt into cleanupFake before exercising deletion; unexpected cleanup calls fail explicitly.
func (f *fakeClient) FinalizeBuildInfo(context.Context, *ebsv1.BuildInfo) error {
	return fmt.Errorf("unexpected FinalizeBuildInfo")
}

func (f *fakeClient) ListOldJobs(context.Context, string, labels.Selector, ...string) ([]ebsv1.Job, error) {
	return nil, fmt.Errorf("unexpected ListOldJobs")
}

func (f *fakeClient) GetOldJob(context.Context, string, string) (*ebsv1.Job, error) {
	return nil, fmt.Errorf("unexpected GetOldJob")
}

func (f *fakeClient) DeleteCleanupChild(context.Context, string, CleanupTarget, metav1.Object) error {
	return fmt.Errorf("unexpected DeleteCleanupChild")
}

type cleanupFake struct {
	*fakeClient
	children   map[CleanupTarget]runtime.Object
	writes     []CleanupTarget
	repo       *ebsv1.RpmRepo
	repoErr    error
	listErr    error
	oldListErr error
}

func (f *cleanupFake) GetRpmRepo(context.Context, string, string) (*ebsv1.RpmRepo, error) {
	if f.repoErr != nil {
		return nil, f.repoErr
	}
	if f.repo == nil {
		return nil, ErrNotFound
	}
	return f.repo.DeepCopy(), nil
}

func TestCleanupWaitsForRepositoryAcknowledgment(t *testing.T) {
	repo := &ebsv1.RpmRepo{}
	f := &cleanupFake{repo: repo}
	check := func(wantReady bool) {
		t.Helper()
		if err := checkCleanupRepository(context.Background(), f, "project", "build"); (err == nil) != wantReady {
			t.Fatalf("cleanup ready=%t, error=%v", wantReady, err)
		}
	}
	check(false)
	repo.Status.Conditions = []metav1.Condition{{
		Type: ebsv1.RpmRepoConditionCleanupReady, Status: metav1.ConditionTrue,
		Reason: ebsv1.RpmRepoReasonOperationsDrained, LastTransitionTime: metav1.Now(),
	}}
	check(true)
	repo.Status.Repository = &ebsv1.RpmRepoRepositoryStatus{Transition: &ebsv1.RepositoryTransition{}}
	check(false)
	repo.Status.Repository = nil
	repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{Transition: &ebsv1.ReleaseTransition{}}
	check(false)
	f.repo = nil
	check(true)
}

func (f *cleanupFake) FinalizeBuildInfo(_ context.Context, info *ebsv1.BuildInfo) error {
	current := f.buildinfos[info.Namespace+"/"+info.Name]
	if current.UID != info.UID || current.ResourceVersion != info.ResourceVersion {
		return fmt.Errorf("stale finalization")
	}
	f.buildinfos[info.Namespace+"/"+info.Name] = info.DeepCopy()
	return nil
}

func (f *cleanupFake) ListJobs(_ context.Context, project string, selector labels.Selector, _ ...string) ([]ebsv1.Job, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listChildren("jobs", project, selector), nil
}

func (f *cleanupFake) ListOldJobs(_ context.Context, project string, selector labels.Selector, _ ...string) ([]ebsv1.Job, error) {
	if f.oldListErr != nil {
		return nil, f.oldListErr
	}
	return f.listChildren("oldjobs", project, selector), nil
}

func (f *cleanupFake) listChildren(resource, project string, selector labels.Selector) []ebsv1.Job {
	var jobs []ebsv1.Job
	for target := range f.children {
		if target.Resource == resource {
			job := f.children[target].(*ebsv1.Job)
			if job.Namespace == project && selector.Matches(labels.Set(job.Labels)) {
				jobs = append(jobs, *job.DeepCopy())
			}
		}
	}
	return jobs
}

func (f *cleanupFake) GetBuildGraph(_ context.Context, _ string, name string, metadataOnly bool) (*ebsv1.BuildGraph, error) {
	if !metadataOnly {
		return nil, fmt.Errorf("cleanup must read graph metadata only")
	}
	if obj := f.children[CleanupTarget{"buildgraphs", name}]; obj != nil {
		return obj.(*ebsv1.BuildGraph).DeepCopy(), nil
	}
	return nil, ErrNotFound
}

func (f *cleanupFake) GetJob(_ context.Context, _ string, name string) (*ebsv1.Job, error) {
	return f.getJob("jobs", name)
}

func (f *cleanupFake) GetOldJob(_ context.Context, _ string, name string) (*ebsv1.Job, error) {
	return f.getJob("oldjobs", name)
}

func (f *cleanupFake) getJob(resource, name string) (*ebsv1.Job, error) {
	if obj := f.children[CleanupTarget{resource, name}]; obj != nil {
		return obj.(*ebsv1.Job).DeepCopy(), nil
	}
	return nil, ErrNotFound
}

func (f *cleanupFake) AbortJob(_ context.Context, project, name string, uid types.UID, reason string) (*ebsv1.Job, error) {
	target := CleanupTarget{"jobs", name}
	job := f.children[target].(*ebsv1.Job)
	if job.Namespace != project || job.UID != uid {
		return nil, fmt.Errorf("unexpected Job abort identity")
	}
	f.writes = append(f.writes, target)
	job.Status.Phase = ebsv1.JobAborted
	job.Status.Message = reason
	return job.DeepCopy(), nil
}

func (f *cleanupFake) DeleteCleanupChild(_ context.Context, _ string, target CleanupTarget, obj metav1.Object) error {
	f.writes = append(f.writes, target)
	if target.Resource == "jobs" {
		now := metav1.Now()
		f.children[target].(*ebsv1.Job).DeletionTimestamp = &now
		return nil
	}
	if target.Resource == "oldjobs" {
		return nil
	} // Wait for the independent Job Controller.
	delete(f.children, target)
	return nil
}

func TestDeletingTerminalBuildInfoCleansChildrenBeforeFinalization(t *testing.T) {
	c, base, _, _ := newTestController(t)
	info := testBuildInfoObj(ebsv1.BuildInfoCompleted)
	now := metav1.Now()
	info.UID, info.ResourceVersion, info.DeletionTimestamp = "info", "1", &now
	info.Finalizers = []string{ebsv1.BuildInfoCleanupFinalizer, "other/controller"}
	key := info.Namespace + "/" + info.Name
	base.buildinfos[key] = info
	// Standalone BuildInfo deletion does not depend on the parent's deletion marker.
	base.builds[key] = testBuildObj("full")
	f := &cleanupFake{fakeClient: base, children: map[CleanupTarget]runtime.Object{}}
	c.client = f
	jobTarget := CleanupTarget{"jobs", "job"}
	job := &ebsv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: info.Namespace, UID: types.UID("job"), ResourceVersion: "1",
		Labels: map[string]string{ebsv1.JobBuildNameLabel: info.Name}}, Status: ebsv1.JobStatus{Phase: ebsv1.JobRunning}}
	f.children[jobTarget] = job
	graphTarget := CleanupTarget{"buildgraphs", info.Name}
	f.children[graphTarget] = &ebsv1.BuildGraph{ObjectMeta: metav1.ObjectMeta{Name: info.Name, Namespace: info.Namespace, UID: "graph",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(info, ebsv1.SchemeGroupVersion.WithKind("BuildInfo"))}}}
	run := func() {
		t.Helper()
		if _, err := c.reconcile(context.Background(), key); err != nil {
			t.Fatal(err)
		}
	}
	f.repoErr = fmt.Errorf("checkpoint still running")
	if _, err := c.reconcile(context.Background(), key); err == nil {
		t.Fatal("RpmRepo checkpoint must block Job cleanup")
	}
	if f.children[graphTarget] != nil || job.Status.Phase != ebsv1.JobRunning || len(f.writes) != 1 {
		t.Fatal("graph should disappear while Job waits for checkpoint drain")
	}
	f.repoErr = nil
	f.oldListErr = fmt.Errorf("archive list unavailable")
	if _, err := c.reconcile(context.Background(), key); err == nil || len(f.writes) != 1 || job.Status.Phase != ebsv1.JobRunning {
		t.Fatal("partial lists must not trigger Job mutations")
	}
	f.oldListErr = nil
	run() // Abort only.
	if job.Status.Phase != ebsv1.JobAborted || job.Status.Message != "BuildDeleted" || job.DeletionTimestamp != nil {
		t.Fatal("abort must precede a fresh terminal GET")
	}
	run() // Delete the source.
	run() // Wait for archive migration.
	if len(f.writes) != 3 {
		t.Fatalf("writes=%v", f.writes)
	}
	delete(f.children, jobTarget)
	oldTarget := CleanupTarget{"oldjobs", job.Name}
	f.children[oldTarget] = job.DeepCopy()
	run()
	if !slices.Contains(base.buildinfos[key].Finalizers, ebsv1.BuildInfoCleanupFinalizer) {
		t.Fatal("finalizer removed before OldJob disappeared")
	}
	delete(f.children, oldTarget)
	f.listErr = fmt.Errorf("incomplete list")
	if _, err := c.reconcile(context.Background(), key); err == nil {
		t.Fatal("incomplete list accepted")
	}
	f.listErr = nil
	run() // Confirm both branches are empty and finalize.
	got := base.buildinfos[key]
	if !slices.Equal(got.Finalizers, []string{"other/controller"}) || got.Status.Phase != ebsv1.BuildInfoCompleted {
		t.Fatalf("finalization changed business state or other finalizers: %+v", got)
	}
	if base.builds[key].DeletionTimestamp != nil {
		t.Fatal("standalone cleanup deleted parent")
	}
}

func TestBuildInfoCleanupRescansAfterParentDisappears(t *testing.T) {
	c, base, _, _ := newTestController(t)
	info := testBuildInfoObj(ebsv1.BuildInfoCompleted)
	now := metav1.Now()
	info.UID, info.ResourceVersion, info.DeletionTimestamp = "info", "1", &now
	info.Finalizers = []string{ebsv1.BuildInfoCleanupFinalizer}
	key := info.Namespace + "/" + info.Name
	base.buildinfos[key] = info
	delete(base.builds, key)
	f := &cleanupFake{fakeClient: base, children: map[CleanupTarget]runtime.Object{}}
	c.client = f
	archived := CleanupTarget{"oldjobs", "previous"}
	f.children[archived] = &ebsv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: archived.Name, Namespace: info.Namespace, UID: "previous",
		Labels: map[string]string{ebsv1.JobBuildNameLabel: info.Name},
	}, Status: ebsv1.JobStatus{Phase: ebsv1.JobSucceeded}}
	if _, err := c.reconcile(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	delete(f.children, archived)
	late := CleanupTarget{"jobs", "late"}
	f.children[late] = &ebsv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: late.Name, Namespace: info.Namespace, UID: "late",
		Labels: map[string]string{ebsv1.JobBuildNameLabel: info.Name},
	}, Status: ebsv1.JobStatus{Phase: ebsv1.JobRunning}}
	if _, err := c.reconcile(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if f.children[late].(*ebsv1.Job).Status.Phase != ebsv1.JobAborted ||
		!slices.Contains(base.buildinfos[key].Finalizers, ebsv1.BuildInfoCleanupFinalizer) {
		t.Fatal("fresh scan must process newly visible Jobs before finalizing")
	}
	delete(f.children, late)
	if _, err := c.reconcile(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if len(base.buildinfos[key].Finalizers) != 0 {
		t.Fatal("cleanup still depends on the missing parent Build")
	}
}

type budgetCleanupFake struct {
	*cleanupFake
	failDeletes bool
}

func (f *budgetCleanupFake) DeleteCleanupChild(_ context.Context, _ string, target CleanupTarget, _ metav1.Object) error {
	f.writes = append(f.writes, target)
	if f.failDeletes && target.Name < "job-100" {
		return fmt.Errorf("injected deletion error")
	}
	delete(f.children, target)
	return nil
}

func TestBuildInfoCleanupBudgetRestartsFromBeginning(t *testing.T) {
	c, base, _, _ := newTestController(t)
	info := testBuildInfoObj(ebsv1.BuildInfoCompleted)
	now := metav1.Now()
	info.UID, info.ResourceVersion, info.DeletionTimestamp = "info", "1", &now
	info.Finalizers = []string{ebsv1.BuildInfoCleanupFinalizer}
	key := info.Namespace + "/" + info.Name
	base.buildinfos[key] = info
	f := &budgetCleanupFake{
		cleanupFake: &cleanupFake{fakeClient: base, children: map[CleanupTarget]runtime.Object{}},
		failDeletes: true,
	}
	c.client = f
	for i := range 105 {
		name := fmt.Sprintf("job-%03d", i)
		f.children[CleanupTarget{"oldjobs", name}] = &ebsv1.Job{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: info.Namespace, UID: types.UID(name), ResourceVersion: "1",
			Labels: map[string]string{ebsv1.JobBuildNameLabel: info.Name},
		}, Status: ebsv1.JobStatus{Phase: ebsv1.JobSucceeded}}
	}
	_, err := c.reconcile(context.Background(), key)
	if err == nil || len(f.writes) != 100 {
		t.Fatalf("writes = %d", len(f.writes))
	}
	_, err = c.reconcile(context.Background(), key)
	if err == nil || len(f.writes) != 200 || len(f.children) != 105 || !slices.Equal(f.writes[:100], f.writes[100:]) {
		t.Fatalf("each round must retry from the beginning: writes=%d remaining=%d", len(f.writes), len(f.children))
	}
	if !slices.Contains(base.buildinfos[key].Finalizers, ebsv1.BuildInfoCleanupFinalizer) {
		t.Fatal("unfinished cleanup finalized")
	}
	f.failDeletes = false
	for _, remaining := range []int{5, 0} {
		if _, err := c.reconcile(context.Background(), key); err != nil || len(f.children) != remaining {
			t.Fatalf("cleanup after recovery: remaining=%d, error=%v", len(f.children), err)
		}
	}
	if _, err := c.reconcile(context.Background(), key); err != nil || len(base.buildinfos[key].Finalizers) != 0 {
		t.Fatalf("cleanup finalization: error=%v", err)
	}
}

type branchesCleanupFake struct {
	Client
	graph        *ebsv1.BuildGraph
	graphErr     error
	repoErr      error
	jobsErr      error
	graphDeletes int
	jobLists     int
}

func (f *branchesCleanupFake) GetRpmRepo(context.Context, string, string) (*ebsv1.RpmRepo, error) {
	if f.repoErr != nil {
		return nil, f.repoErr
	}
	return nil, ErrNotFound
}

func (f *branchesCleanupFake) ListJobs(context.Context, string, labels.Selector, ...string) ([]ebsv1.Job, error) {
	f.jobLists++
	return nil, f.jobsErr
}

func (f *branchesCleanupFake) ListOldJobs(context.Context, string, labels.Selector, ...string) ([]ebsv1.Job, error) {
	return nil, nil
}

func (f *branchesCleanupFake) GetBuildGraph(context.Context, string, string, bool) (*ebsv1.BuildGraph, error) {
	if f.graphErr != nil {
		return nil, f.graphErr
	}
	if f.graph == nil {
		return nil, ErrNotFound
	}
	return f.graph.DeepCopy(), nil
}

func (f *branchesCleanupFake) DeleteCleanupChild(context.Context, string, CleanupTarget, metav1.Object) error {
	f.graphDeletes++
	f.graph = nil
	return nil
}

func TestIndependentCleanupBranches(t *testing.T) {
	readErr := errors.New("graph read failed")
	repoErr := errors.New("repository still processing")
	listErr := errors.New("job list failed")
	for _, test := range []struct {
		name         string
		graphErr     error
		repoErr      error
		jobsErr      error
		deleting     bool
		graphDeletes int
		jobLists     int
	}{
		{name: "graph read failure", graphErr: readErr, jobLists: 1},
		{name: "graph still deleting", deleting: true, jobLists: 1},
		{name: "job list failure", jobsErr: listErr, graphDeletes: 1, jobLists: 1},
		{name: "repository pending", repoErr: repoErr, graphDeletes: 1},
		{name: "both fail", graphErr: readErr, jobsErr: listErr, jobLists: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			info := &ebsv1.BuildInfo{ObjectMeta: metav1.ObjectMeta{Name: "build", Namespace: "project", UID: "info"}}
			graph := &ebsv1.BuildGraph{ObjectMeta: metav1.ObjectMeta{
				Name: info.Name, Namespace: info.Namespace, UID: "graph",
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(info, ebsv1.SchemeGroupVersion.WithKind("BuildInfo")),
				},
			}}
			if test.deleting {
				now := metav1.Now()
				graph.DeletionTimestamp = &now
			}
			f := &branchesCleanupFake{graph: graph, graphErr: test.graphErr, repoErr: test.repoErr, jobsErr: test.jobsErr}
			c := &Controller{client: f}
			done, err := c.cleanupChildren(context.Background(), info)
			if done || f.graphDeletes != test.graphDeletes || f.jobLists != test.jobLists {
				t.Fatalf("done=%t deletes=%d lists=%d error=%v", done, f.graphDeletes, f.jobLists, err)
			}
			for _, want := range []error{test.graphErr, test.repoErr, test.jobsErr} {
				if want != nil && !errors.Is(err, want) {
					t.Fatalf("missing error %v in %v", want, err)
				}
			}
			// A fresh controller confirms both branches disappeared without relying on cached progress.
			f.graph, f.graphErr, f.repoErr, f.jobsErr = nil, nil, nil, nil
			done, err = (&Controller{client: f}).cleanupChildren(context.Background(), info)
			if !done || err != nil {
				t.Fatalf("completion after recovery: done=%t err=%v", done, err)
			}
		})
	}
}

func (f *branchesCleanupFake) FinalizeBuildInfo(context.Context, *ebsv1.BuildInfo) error { return nil }
