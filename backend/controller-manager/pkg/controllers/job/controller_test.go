package job

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/utils/clock"
	clocktesting "k8s.io/utils/clock/testing"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/clients/gitserver"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

type fakeCachedSource struct {
	mu       sync.RWMutex
	objects  map[string]runtime.Object
	handlers []source.ResourceEventHandler
}

func (s *fakeCachedSource) Name() string { return "fake" }
func (s *fakeCachedSource) AddEventHandler(handler source.ResourceEventHandler) error {
	s.handlers = append(s.handlers, handler)
	return nil
}
func (s *fakeCachedSource) Run(ctx context.Context) error { <-ctx.Done(); return nil }
func (s *fakeCachedSource) HasSynced() bool               { return true }
func (s *fakeCachedSource) Ready() bool                   { return true }
func (s *fakeCachedSource) GetByKey(key string) (runtime.Object, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	obj, exists := s.objects[key]
	if !exists {
		return nil, false, nil
	}
	return obj.DeepCopyObject(), true, nil
}
func (s *fakeCachedSource) ByIndex(string, string) ([]runtime.Object, error) { return nil, nil }
func (s *fakeCachedSource) set(key string, obj runtime.Object) {
	s.mu.Lock()
	s.objects[key] = obj
	s.mu.Unlock()
}

type fakeClient struct {
	listDeletingFn  func(context.Context, bool, string, int64) (source.ListPage, error)
	archiveStateFn  func(context.Context, string, string) (*ebsv1.JobArchiveState, error)
	finalizeFn      func(context.Context, *ebsv1.Job, bool) error
	deleteOldFn     func(context.Context, *ebsv1.Job) error
	deleteJobDataFn func(context.Context, *ebsv1.Job) error
	archiveFn       func(context.Context, string, string, clientpkg.DeletePreconditions) (*ebsv1.Job, error)
	getOldJobFn     func(context.Context, string, string) (*ebsv1.Job, error)
	listHistoryFn   func(context.Context, ebsv1.JobPhase, string, int64) (source.ListPage, error)
	getJobFn        func(context.Context, string, string) (*ebsv1.Job, error)
	getRunnerFn     func(context.Context, string) (*ebsv1.Runner, error)
	updateFn        func(context.Context, *ebsv1.Job) (*ebsv1.Job, error)
	deleteFn        func(context.Context, string, string, clientpkg.DeletePreconditions) error
	getJobCalls     int
	getRunnerCalls  int
	updateCalls     int
	deleteCalls     int
}

type cleanupServices struct{ content *ebsv1.ServiceConfigContent }

func (s cleanupServices) Load(context.Context) (*ebsv1.ServiceConfigContent, error) {
	return s.content, nil
}
func (s cleanupServices) GitClient(string) (gitserver.GitServerClient, error) {
	return nil, errors.New("unexpected Git client")
}

func TestJobCleanupUsesConfiguredInstanceAndRequires204(t *testing.T) {
	status := 204
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/internal/v1/projects/project/jobs/job" {
			t.Errorf("request=%s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(status)
	}))
	defer server.Close()
	client := &apiClient{http: server.Client(), services: cleanupServices{content: &ebsv1.ServiceConfigContent{
		ArtifactManagers: map[string]ebsv1.ServiceEndpoints{"art-01": {InternalURL: server.URL}},
	}}}
	job := runningJob()
	job.Annotations = map[string]string{ebsv1.ProjectArtifactsAnnotation: "art-01"}
	for _, code := range []int{204, 200, 404, 503} {
		status = code
		err := client.DeleteJobData(context.Background(), job)
		if (err == nil) != (code == 204) {
			t.Fatalf("code=%d error=%v", code, err)
		}
		if code == 503 {
			var ce *cleanupError
			if !errors.As(err, &ce) || ce.retryAfter != 17*time.Second {
				t.Fatalf("retry error=%v", err)
			}
		}
	}
}

func (c *fakeClient) ListDeletingPage(ctx context.Context, old bool, token string, limit int64) (source.ListPage, error) {
	if c.listDeletingFn == nil {
		return source.ListPage{}, nil
	}
	return c.listDeletingFn(ctx, old, token, limit)
}

func (c *fakeClient) GetArchiveState(ctx context.Context, ns, name string) (*ebsv1.JobArchiveState, error) {
	return c.archiveStateFn(ctx, ns, name)
}

func (c *fakeClient) FinalizeJob(ctx context.Context, job *ebsv1.Job, old bool) error {
	return c.finalizeFn(ctx, job, old)
}
func (c *fakeClient) DeleteOldJob(ctx context.Context, job *ebsv1.Job) error {
	return c.deleteOldFn(ctx, job)
}

func (c *fakeClient) DeleteJobData(ctx context.Context, job *ebsv1.Job) error {
	if c.deleteJobDataFn == nil {
		return errors.New("unexpected Job data cleanup")
	}
	return c.deleteJobDataFn(ctx, job)
}

func TestHistoryRecordsLifecycleIntent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action string
		want   []string
	}{
		{name: "business deletion records intent", action: "delete", want: []string{"delete"}},
		{name: "archive migration retains data", action: "archive", want: []string{"archive"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(10_000_000, 0)
			job := runningJob()
			job.Status.Phase = ebsv1.JobSucceeded
			job.Status.EndTime = metav1.NewTime(now.Add(-31 * 24 * time.Hour))
			var actions []string
			client := &fakeClient{
				getJobFn:        func(context.Context, string, string) (*ebsv1.Job, error) { return job.DeepCopy(), nil },
				deleteJobDataFn: func(context.Context, *ebsv1.Job) error { actions = append(actions, "data"); return nil },
				archiveFn: func(context.Context, string, string, clientpkg.DeletePreconditions) (*ebsv1.Job, error) {
					actions = append(actions, "archive")
					return job.DeepCopy(), nil
				},
				deleteFn: func(_ context.Context, _, _ string, pre clientpkg.DeletePreconditions) error {
					if pre.UID != job.UID || pre.ResourceVersion != job.ResourceVersion {
						t.Fatalf("delete preconditions=%+v", pre)
					}
					actions = append(actions, "delete")
					return nil
				},
			}
			c, _, _, _ := newTestController(t, now, job, nil, client, true)
			c.config.HistoryAction = tc.action
			if err := c.reconcileHistory(context.Background(), "project/job", job, now); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actions, tc.want) {
				t.Fatalf("actions=%v, want %v", actions, tc.want)
			}
		})
	}
}

func (c *fakeClient) ArchiveJob(ctx context.Context, namespace, name string, pre clientpkg.DeletePreconditions) (*ebsv1.Job, error) {
	return c.archiveFn(ctx, namespace, name, pre)
}
func (c *fakeClient) GetOldJob(ctx context.Context, namespace, name string) (*ebsv1.Job, error) {
	return c.getOldJobFn(ctx, namespace, name)
}

func (c *fakeClient) ListHistoryPage(ctx context.Context, phase ebsv1.JobPhase, token string, limit int64) (source.ListPage, error) {
	return c.listHistoryFn(ctx, phase, token, limit)
}

func (c *fakeClient) GetJob(ctx context.Context, namespace, name string) (*ebsv1.Job, error) {
	c.getJobCalls++
	return c.getJobFn(ctx, namespace, name)
}
func (c *fakeClient) GetRunner(ctx context.Context, name string) (*ebsv1.Runner, error) {
	c.getRunnerCalls++
	return c.getRunnerFn(ctx, name)
}
func (c *fakeClient) UpdateJobStatus(ctx context.Context, job *ebsv1.Job) (*ebsv1.Job, error) {
	c.updateCalls++
	return c.updateFn(ctx, job)
}
func (c *fakeClient) DeleteJob(ctx context.Context, namespace, name string, preconditions clientpkg.DeletePreconditions) error {
	c.deleteCalls++
	return c.deleteFn(ctx, namespace, name, preconditions)
}

func runningJob() *ebsv1.Job {
	return &ebsv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "project", Name: "job", UID: "job-uid", ResourceVersion: "10"},
		Status:     ebsv1.JobStatus{Phase: ebsv1.JobRunning, Stage: ebsv1.JobStagePostRun, Runner: "runner", StartTime: metav1.NewTime(time.Unix(100, 0)), Message: "old"},
	}
}

func runner(phase ebsv1.RunnerPhase) *ebsv1.Runner {
	return &ebsv1.Runner{ObjectMeta: metav1.ObjectMeta{Name: "runner", UID: "runner-uid", ResourceVersion: "7"}, Status: ebsv1.RunnerStatus{Phase: phase}}
}

func newTestController(t *testing.T, now time.Time, job *ebsv1.Job, cachedRunner *ebsv1.Runner, client Client, gc bool) (*Controller, *clocktesting.FakeClock, *fakeCachedSource, *fakeCachedSource) {
	t.Helper()
	jobs := &fakeCachedSource{objects: map[string]runtime.Object{job.Namespace + "/" + job.Name: job}}
	runners := &fakeCachedSource{objects: make(map[string]runtime.Object)}
	if cachedRunner != nil {
		runners.objects[cachedRunner.Name] = cachedRunner
	}
	clk := clocktesting.NewFakeClock(now)
	c, err := New(jobs, runners, client, clk, Config{RunnerLostGracePeriod: 5 * time.Minute, HistoryGCEnabled: gc, HistoryRetention: 30 * 24 * time.Hour, MaxRetries: 3})
	if err != nil {
		t.Fatal(err)
	}
	return c, clk, jobs, runners
}

func TestJobEventsUseReconcileScope(t *testing.T) {
	now := time.Unix(10_000_000, 0)
	for _, tc := range []struct {
		name     string
		phase    ebsv1.JobPhase
		runner   string
		endTime  metav1.Time
		gc       bool
		deleting bool
		queueKey string
		indexed  bool
	}{
		{name: "pending", phase: ebsv1.JobPending, runner: "runner"},
		{name: "unassigned", phase: ebsv1.JobRunning},
		{name: "running", phase: ebsv1.JobRunning, runner: "runner", queueKey: "project/job", indexed: true},
		{name: "terminal recent", phase: ebsv1.JobSucceeded, runner: "runner",
			endTime: metav1.NewTime(now.Add(-time.Hour)), gc: true},
		{name: "terminal expired", phase: ebsv1.JobFailed, runner: "runner",
			endTime: metav1.NewTime(now.Add(-31 * 24 * time.Hour)), gc: true},
		{name: "terminal missing endTime", phase: ebsv1.JobAborted, runner: "runner", gc: true},
		{name: "terminal GC disabled", phase: ebsv1.JobFailed, runner: "runner",
			endTime: metav1.NewTime(now.Add(-31 * 24 * time.Hour))},
		{name: "deleting", phase: ebsv1.JobRunning, runner: "runner", deleting: true, queueKey: "jobs/project/job"},
	} {
		for _, event := range []string{"add", "update", "resync"} {
			t.Run(tc.name+"/"+event, func(t *testing.T) {
				previous := runningJob()
				job := previous.DeepCopy()
				job.Status.Phase, job.Status.Runner, job.Status.EndTime = tc.phase, tc.runner, tc.endTime
				if tc.deleting {
					ts := metav1.NewTime(now)
					job.DeletionTimestamp = &ts
				}
				c, _, _, _ := newTestController(t, now, previous, nil, &fakeClient{}, tc.gc)
				defer c.Queue().ShutDown()
				c.updateIndex("project/job", previous)
				c.observation("project/job", previous, now)
				switch event {
				case "add":
					c.onJobAdd(job)
				case "update":
					job.ResourceVersion = "11"
					c.onJobUpdate(previous, job)
				case "resync":
					c.onJobUpdate(job.DeepCopy(), job)
				}
				if got := len(c.index.jobs("runner")) > 0; got != tc.indexed {
					t.Fatalf("indexed=%v, want %v", got, tc.indexed)
				}
				if got := len(c.observations) > 0; got != tc.indexed {
					t.Fatalf("observation retained=%v, want %v", got, tc.indexed)
				}
				wantKey := tc.queueKey
				// An update with only resourceVersion changed is not a resync or a meaningful state change.
				if event == "update" && tc.indexed {
					wantKey = ""
				}
				if wantKey == "" {
					if c.Queue().Len() != 0 {
						t.Fatal("Job event unexpectedly enqueued")
					}
					return
				}
				if c.Queue().Len() != 1 {
					t.Fatalf("queue length=%d", c.Queue().Len())
				}
				key, _ := c.Queue().Get()
				c.Queue().Done(key)
				if key != wantKey {
					t.Fatalf("key=%v, want %s", key, wantKey)
				}
			})
		}
	}
}

func TestRunnerAvailableOnlyWhenOnline(t *testing.T) {
	for _, phase := range []ebsv1.RunnerPhase{ebsv1.RunnerOnline, ebsv1.RunnerOffline, "Unknown", ""} {
		t.Run(string(phase), func(t *testing.T) {
			job := runningJob()
			c, _, _, _ := newTestController(t, time.Unix(1000, 0), job, runner(phase), &fakeClient{}, false)
			available, err := c.runnerAvailableFromCache("runner")
			if err != nil {
				t.Fatal(err)
			}
			if available != (phase == ebsv1.RunnerOnline) {
				t.Fatalf("phase %q available=%t", phase, available)
			}
		})
	}
}

func TestRunnerLostGraceAndFailure(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("test", 8*60*60))
	job := runningJob()
	var request *ebsv1.Job
	client := &fakeClient{}
	client.getJobFn = func(context.Context, string, string) (*ebsv1.Job, error) { return job.DeepCopy(), nil }
	client.getRunnerFn = func(context.Context, string) (*ebsv1.Runner, error) { return runner("Offline"), nil }
	client.updateFn = func(_ context.Context, value *ebsv1.Job) (*ebsv1.Job, error) {
		request = value.DeepCopy()
		response := value.DeepCopy()
		response.ResourceVersion = "11"
		return response, nil
	}
	client.deleteFn = func(context.Context, string, string, clientpkg.DeletePreconditions) error { return nil }
	c, clk, _, _ := newTestController(t, now, job, runner("Offline"), client, true)

	result, err := c.sync(context.Background(), "project/job")
	if err != nil || result.RequeueAfter != 5*time.Minute {
		t.Fatalf("first Sync result=%+v err=%v", result, err)
	}
	if client.getJobCalls != 0 || client.updateCalls != 0 {
		t.Fatal("API called before grace period expired")
	}
	clk.Step(5 * time.Minute)
	result, err = c.sync(context.Background(), "project/job")
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("second Sync result=%+v err=%v", result, err)
	}
	if client.getRunnerCalls != 1 || client.updateCalls != 1 || request == nil {
		t.Fatalf("calls: runner=%d update=%d", client.getRunnerCalls, client.updateCalls)
	}
	if request.Status.Phase != ebsv1.JobFailed || request.Status.Stage != ebsv1.JobStagePostRun || request.Status.Runner != "runner" {
		t.Fatalf("owned fields were not preserved: %+v", request.Status)
	}
	wantEnd := now.Add(5 * time.Minute)
	if !request.Status.EndTime.Time.Equal(wantEnd) {
		t.Fatalf("endTime=%v, want %v", request.Status.EndTime, wantEnd)
	}
}

func TestAuthoritativeRunnerRecoveryPreventsFailure(t *testing.T) {
	job := runningJob()
	client := &fakeClient{}
	client.getJobFn = func(context.Context, string, string) (*ebsv1.Job, error) { return job.DeepCopy(), nil }
	client.getRunnerFn = func(context.Context, string) (*ebsv1.Runner, error) { return runner("Online"), nil }
	client.updateFn = func(context.Context, *ebsv1.Job) (*ebsv1.Job, error) {
		t.Fatal("unexpected status update")
		return nil, nil
	}
	client.deleteFn = func(context.Context, string, string, clientpkg.DeletePreconditions) error { return nil }
	c, clk, _, _ := newTestController(t, time.Unix(1000, 0), job, runner("Offline"), client, true)
	if _, err := c.sync(context.Background(), "project/job"); err != nil {
		t.Fatal(err)
	}
	clk.Step(5 * time.Minute)
	if result, err := c.sync(context.Background(), "project/job"); err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if client.updateCalls != 0 {
		t.Fatal("recovered Runner caused status update")
	}
}

func TestStatusWriteUnknownIsConfirmed(t *testing.T) {
	job := runningJob()
	getCalls := 0
	client := &fakeClient{}
	client.getJobFn = func(context.Context, string, string) (*ebsv1.Job, error) {
		getCalls++
		value := job.DeepCopy()
		if getCalls == 2 {
			value.Status.Phase = ebsv1.JobFailed
		}
		return value, nil
	}
	client.getRunnerFn = func(context.Context, string) (*ebsv1.Runner, error) { return runner("Offline"), nil }
	client.updateFn = func(context.Context, *ebsv1.Job) (*ebsv1.Job, error) { return nil, errors.New("connection reset") }
	client.deleteFn = func(context.Context, string, string, clientpkg.DeletePreconditions) error { return nil }
	c, clk, _, _ := newTestController(t, time.Unix(1000, 0), job, runner("Offline"), client, true)
	_, _ = c.sync(context.Background(), "project/job")
	clk.Step(5 * time.Minute)
	result, err := c.sync(context.Background(), "project/job")
	if err != nil || result != (controller.ReconcileResult{}) || getCalls != 2 {
		t.Fatalf("result=%+v err=%v getCalls=%d", result, err, getCalls)
	}
}

func TestCanceledContextDoesNotConfirmUnknownWrite(t *testing.T) {
	job := runningJob()
	client := &fakeClient{
		getJobFn:    func(context.Context, string, string) (*ebsv1.Job, error) { return job.DeepCopy(), nil },
		getRunnerFn: func(context.Context, string) (*ebsv1.Runner, error) { return runner("Offline"), nil },
		updateFn:    func(context.Context, *ebsv1.Job) (*ebsv1.Job, error) { return nil, nil },
		deleteFn:    func(context.Context, string, string, clientpkg.DeletePreconditions) error { return nil },
	}
	c, _, _, _ := newTestController(t, time.Unix(1000, 0), job, runner("Offline"), client, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.handleStatusWriteError(ctx, "project/job", job, errors.New("unknown"))
	if !errors.Is(err, context.Canceled) || client.getJobCalls != 0 {
		t.Fatalf("err=%v getJobCalls=%d", err, client.getJobCalls)
	}
}

func TestHistoryArchiveDefersFinalizationToLifecycle(t *testing.T) {
	for _, outcome := range []clientpkg.WriteOutcome{"", clientpkg.WriteUnknown, clientpkg.WriteRejected, clientpkg.WriteNotSent} {
		t.Run(string(outcome), func(t *testing.T) {
			now := time.Unix(10_000_000, 0)
			job := runningJob()
			job.Status.Phase = ebsv1.JobSucceeded
			job.Status.EndTime = metav1.NewTime(now.Add(-31 * 24 * time.Hour))
			job.Finalizers = []string{ebsv1.JobArchiveFinalizer}
			calls := 0
			client := &fakeClient{
				getJobFn: func(context.Context, string, string) (*ebsv1.Job, error) { return job.DeepCopy(), nil },
				archiveFn: func(_ context.Context, _, _ string, pre clientpkg.DeletePreconditions) (*ebsv1.Job, error) {
					calls++
					if pre.ResourceVersion != job.ResourceVersion || pre.UID != job.UID {
						t.Fatalf("pre=%+v", pre)
					}
					if outcome != "" {
						return nil, &clientpkg.WriteError{Outcome: outcome, StatusCode: 503, Err: errors.New("archive interrupted")}
					}
					out := job.DeepCopy()
					out.ResourceVersion = "v1:1:1"
					out.Finalizers = []string{ebsv1.JobArtifactsCleanupFinalizer}
					return out, nil
				},
			}
			c, _, _, _ := newTestController(t, now, job, nil, client, true)
			c.config.HistoryAction = "archive"
			err := c.reconcileHistory(context.Background(), "project/job", job, now)
			if calls != 1 || client.deleteCalls != 0 || (err != nil) != (outcome != "") {
				t.Fatalf("calls=%d delete=%d err=%v", calls, client.deleteCalls, err)
			}
		})
	}
}

func TestDeletingNonterminalJobReportsInvalidState(t *testing.T) {
	job := runningJob()
	now := time.Unix(1000, 0)
	timestamp := metav1.NewTime(now)
	job.DeletionTimestamp = &timestamp
	job.Finalizers = []string{ebsv1.JobArchiveFinalizer}
	client := &fakeClient{getJobFn: func(context.Context, string, string) (*ebsv1.Job, error) {
		return job.DeepCopy(), nil
	}}
	c, _, _, _ := newTestController(t, now, job, nil, client, false)
	if _, err := c.sync(context.Background(), "jobs/project/job"); !controller.IsPermanent(err) {
		t.Fatalf("invalid deleting Job: %v", err)
	}
}

func TestDeletionLifecycleResumesAndHandsOff(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0)
	job := runningJob()
	timestamp := metav1.NewTime(now)
	job.Status.Phase = ebsv1.JobAborted
	job.Status.EndTime = timestamp
	job.DeletionTimestamp = &timestamp
	job.Finalizers = []string{ebsv1.JobArchiveFinalizer}
	job.Annotations = map[string]string{ebsv1.ProjectArtifactsAnnotation: "art-01"}
	var old *ebsv1.Job
	state := &ebsv1.JobArchiveState{UID: string(job.UID), Action: "delete", Phase: "Pending"}
	var actions []string
	cleanupFails := true
	client := &fakeClient{
		getJobFn: func(context.Context, string, string) (*ebsv1.Job, error) {
			if job == nil {
				return nil, apierrors.NewNotFound(ebsv1.Resource("jobs"), "job")
			}
			return job.DeepCopy(), nil
		},
		getOldJobFn: func(context.Context, string, string) (*ebsv1.Job, error) {
			if old == nil {
				return nil, apierrors.NewNotFound(ebsv1.Resource("oldjobs"), "job")
			}
			return old.DeepCopy(), nil
		},
		archiveStateFn: func(context.Context, string, string) (*ebsv1.JobArchiveState, error) {
			out := *state
			if job == nil {
				out.Phase = "Transferred"
			}
			return &out, nil
		},
		archiveFn: func(_ context.Context, _, _ string, pre clientpkg.DeletePreconditions) (*ebsv1.Job, error) {
			if pre.ResourceVersion != job.ResourceVersion {
				t.Fatal("stale archive request")
			}
			actions = append(actions, "archive")
			old = job.DeepCopy()
			old.DeletionTimestamp = nil
			old.Finalizers = []string{ebsv1.JobArtifactsCleanupFinalizer}
			old.ResourceVersion = "v1:1:1"
			state.Phase = "Stored"
			state.Digest = "snapshot-digest"
			return nil, &clientpkg.WriteError{Outcome: clientpkg.WriteUnknown, Err: errors.New("response lost")}
		},
		deleteOldFn: func(_ context.Context, current *ebsv1.Job) error {
			actions = append(actions, "handoff")
			old.DeletionTimestamp = &timestamp
			old.ResourceVersion = "v1:2:1"
			return &clientpkg.WriteError{Outcome: clientpkg.WriteUnknown, Err: errors.New("response lost")}
		},
		finalizeFn: func(_ context.Context, current *ebsv1.Job, isOld bool) error {
			if isOld {
				actions = append(actions, "finish-old")
				old = nil
			} else {
				actions = append(actions, "finish-source")
				job = nil
			}
			return nil
		},
		deleteJobDataFn: func(_ context.Context, current *ebsv1.Job) error {
			if current.Annotations[ebsv1.ProjectArtifactsAnnotation] != "art-01" {
				t.Fatal("lost instance")
			}
			actions = append(actions, "cleanup")
			if cleanupFails {
				return errors.New("disk unavailable")
			}
			return nil
		},
	}
	for round := 0; round < 3; round++ {
		// Each round gets a new controller: no in-memory migration state is required.
		c, _, _, _ := newTestController(t, now, runningJob(), nil, client, false)
		result, err := c.sync(ctx, "jobs/project/job")
		if (round < 2) != (err != nil) || result != (controller.ReconcileResult{}) {
			t.Fatalf("round=%d result=%+v err=%v", round, result, err)
		}
		if round == 1 {
			if result, err := c.sync(ctx, "oldjobs/project/job"); err != nil || result != (controller.ReconcileResult{}) {
				t.Fatalf("waiting for source deletion: result=%+v err=%v", result, err)
			}
			if actions[len(actions)-1] != "handoff" {
				t.Fatal("cleanup before source disappeared")
			}
		}
	}
	c, _, _, _ := newTestController(t, now, runningJob(), nil, client, false)
	if _, err := c.sync(ctx, "oldjobs/project/job"); err == nil || old == nil {
		t.Fatal("cleanup failure must retain OldJob")
	}
	cleanupFails = false
	if result, err := c.sync(ctx, "oldjobs/project/job"); err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("finished cleanup: result=%+v err=%v", result, err)
	}
	want := []string{"archive", "handoff", "finish-source", "cleanup", "cleanup", "finish-old"}
	if !reflect.DeepEqual(actions, want) {
		t.Fatalf("actions=%v", actions)
	}
}

func TestOldJobHandoffFinalizesSourceInSameReconcile(t *testing.T) {
	for _, outcome := range []string{"success", "conflict", "unknown"} {
		t.Run(outcome, func(t *testing.T) {
			now := time.Unix(1000, 0)
			job := runningJob()
			ts := metav1.NewTime(now)
			job.Status.Phase, job.Status.EndTime = ebsv1.JobSucceeded, ts
			job.DeletionTimestamp = &ts
			job.Finalizers = []string{ebsv1.JobArchiveFinalizer}
			old := job.DeepCopy()
			old.DeletionTimestamp = nil
			old.ResourceVersion = "old-version"
			var actions []string
			client := &fakeClient{
				getJobFn: func(context.Context, string, string) (*ebsv1.Job, error) { return job.DeepCopy(), nil },
				archiveStateFn: func(context.Context, string, string) (*ebsv1.JobArchiveState, error) {
					return &ebsv1.JobArchiveState{UID: string(job.UID), Action: "delete", Phase: "Stored"}, nil
				},
				getOldJobFn: func(context.Context, string, string) (*ebsv1.Job, error) { return old.DeepCopy(), nil },
				deleteOldFn: func(_ context.Context, current *ebsv1.Job) error {
					if current.ResourceVersion != old.ResourceVersion {
						t.Fatal("handoff used source version")
					}
					actions = append(actions, "handoff")
					switch outcome {
					case "conflict":
						return &clientpkg.WriteError{Outcome: clientpkg.WriteRejected, StatusCode: 409}
					case "unknown":
						return &clientpkg.WriteError{Outcome: clientpkg.WriteUnknown, Err: errors.New("response lost")}
					}
					return nil
				},
				finalizeFn: func(_ context.Context, current *ebsv1.Job, isOld bool) error {
					if isOld || current.UID != job.UID || current.ResourceVersion != job.ResourceVersion {
						t.Fatal("finalize must retain source UID and resourceVersion")
					}
					actions = append(actions, "finalize")
					return nil
				},
			}
			c, _, _, _ := newTestController(t, now, job, nil, client, false)
			defer c.Queue().ShutDown()
			result, err := c.sync(context.Background(), "jobs/project/job")
			if (err == nil) != (outcome == "success") || result != (controller.ReconcileResult{}) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			want := []string{"handoff"}
			if outcome == "success" {
				want = append(want, "finalize")
			}
			if !reflect.DeepEqual(actions, want) || client.getJobCalls != 1 {
				t.Fatalf("actions=%v source reads=%d", actions, client.getJobCalls)
			}
		})
	}
}

func TestHistoryFailureLoggedOnce(t *testing.T) {
	for _, action := range []string{"archive", "delete"} {
		t.Run(action, func(t *testing.T) {
			now := time.Unix(10_000_000, 0)
			job := runningJob()
			job.Status.Phase = ebsv1.JobSucceeded
			job.Status.EndTime = metav1.NewTime(now.Add(-31 * 24 * time.Hour))
			failure := &clientpkg.WriteError{Outcome: clientpkg.WriteUnknown, Err: errors.New("response lost")}
			client := &fakeClient{
				listHistoryFn: func(_ context.Context, phase ebsv1.JobPhase, _ string, _ int64) (source.ListPage, error) {
					if phase == job.Status.Phase {
						return source.ListPage{Items: []runtime.Object{job.DeepCopy()}}, nil
					}
					return source.ListPage{}, nil
				},
				getJobFn: func(context.Context, string, string) (*ebsv1.Job, error) { return job.DeepCopy(), nil },
				archiveFn: func(context.Context, string, string, clientpkg.DeletePreconditions) (*ebsv1.Job, error) {
					return nil, failure
				},
				deleteFn: func(context.Context, string, string, clientpkg.DeletePreconditions) error { return failure },
			}
			c, _, _, _ := newTestController(t, now, job, nil, client, true)
			defer c.Queue().ShutDown()
			c.config.HistoryAction = action
			var output bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&output)
			defer log.SetOutput(previous)
			if err := c.scanHistory(context.Background()); err != nil {
				t.Fatal(err)
			}
			if strings.Count(output.String(), "\n") != 1 ||
				!strings.Contains(output.String(), "reason=HistoryJobDeferred") ||
				!strings.Contains(output.String(), "action="+action) {
				t.Fatalf("unexpected failure logs: %s", output.String())
			}
		})
	}
}

func TestLifecyclePollingWorksWithoutHistoryGC(t *testing.T) {
	now := time.Unix(1000, 0)
	deleting := runningJob()
	ts := metav1.NewTime(now)
	deleting.DeletionTimestamp = &ts
	var scopes []bool
	client := &fakeClient{listDeletingFn: func(_ context.Context, old bool, token string, limit int64) (source.ListPage, error) {
		scopes = append(scopes, old)
		if limit != 500 {
			t.Fatal(limit)
		}
		return source.ListPage{Items: []runtime.Object{deleting.DeepCopy()}}, nil
	}}
	c, _, _, _ := newTestController(t, now, runningJob(), nil, client, false)
	if err := c.scanLifecycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(scopes, []bool{false, true}) {
		t.Fatalf("scopes=%v", scopes)
	}
}

func TestHistoryDeleteUnknownLeavesRecoveryToLifecycle(t *testing.T) {
	now := time.Unix(10_000_000, 0)
	job := runningJob()
	job.Status.Phase = ebsv1.JobSucceeded
	job.Status.EndTime = metav1.NewTime(now.Add(-31 * 24 * time.Hour))
	unknown := &clientpkg.WriteError{Outcome: clientpkg.WriteUnknown, Err: errors.New("response lost")}
	client := &fakeClient{
		getJobFn: func(context.Context, string, string) (*ebsv1.Job, error) { return job.DeepCopy(), nil },
		deleteFn: func(context.Context, string, string, clientpkg.DeletePreconditions) error { return unknown },
	}
	c, _, _, _ := newTestController(t, now, job, nil, client, true)
	defer c.Queue().ShutDown()
	if err := c.reconcileHistory(context.Background(), "project/job", job, now); !errors.Is(err, unknown) {
		t.Fatalf("history error=%v", err)
	}
	if client.getJobCalls != 1 || client.deleteCalls != 1 || c.Queue().Len() != 0 {
		t.Fatalf("gets=%d deletes=%d queued=%d", client.getJobCalls, client.deleteCalls, c.Queue().Len())
	}
}

func TestLifecycleContinuesConfirmedArchiveImmediately(t *testing.T) {
	now := time.Unix(1000, 0)
	job := runningJob()
	ts := metav1.NewTime(now)
	job.Status.Phase, job.Status.EndTime = ebsv1.JobSucceeded, ts
	job.DeletionTimestamp = &ts
	job.Finalizers = []string{ebsv1.JobArchiveFinalizer, "example.com/other"}
	phase := "Pending"
	var actions []string
	client := &fakeClient{
		getJobFn: func(context.Context, string, string) (*ebsv1.Job, error) { return job.DeepCopy(), nil },
		archiveStateFn: func(context.Context, string, string) (*ebsv1.JobArchiveState, error) {
			actions = append(actions, "state")
			return &ebsv1.JobArchiveState{UID: string(job.UID), Action: "retain", Phase: phase}, nil
		},
		archiveFn: func(context.Context, string, string, clientpkg.DeletePreconditions) (*ebsv1.Job, error) {
			actions = append(actions, "archive")
			phase = "Stored"
			return job.DeepCopy(), nil
		},
		finalizeFn: func(context.Context, *ebsv1.Job, bool) error {
			actions = append(actions, "finalize")
			job.Finalizers = []string{"example.com/other"}
			return nil
		},
	}
	c, _, _, _ := newTestController(t, now, job, nil, client, false)
	defer c.Queue().ShutDown()
	for round, want := range []controller.ReconcileResult{{Requeue: true}, {}, {}} {
		result, err := c.sync(context.Background(), "jobs/project/job")
		if err != nil || result != want {
			t.Fatalf("round=%d result=%+v err=%v", round, result, err)
		}
	}
	if want := []string{"state", "archive", "state", "finalize"}; !reflect.DeepEqual(actions, want) {
		t.Fatalf("actions=%v, want=%v", actions, want)
	}
}

func TestSourceDeletionRetriesAfterFinalizerRemoval(t *testing.T) {
	for _, action := range []string{"retain", "delete"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			ts := metav1.NewTime(time.Unix(1000, 0))
			job := runningJob()
			job.Status.Phase, job.Status.EndTime = ebsv1.JobSucceeded, ts
			job.DeletionTimestamp = &ts
			job.Finalizers = []string{ebsv1.JobArchiveFinalizer}
			old := job.DeepCopy()
			old.ResourceVersion = "v1:2:1"
			old.Finalizers = []string{ebsv1.JobArtifactsCleanupFinalizer}
			if action == "retain" {
				old.DeletionTimestamp = nil
			}
			finalizeCalls, cleanupCalls := 0, 0
			client := &fakeClient{
				getJobFn: func(context.Context, string, string) (*ebsv1.Job, error) {
					if job == nil {
						return nil, apierrors.NewNotFound(ebsv1.Resource("jobs"), "job")
					}
					return job.DeepCopy(), nil
				},
				getOldJobFn: func(context.Context, string, string) (*ebsv1.Job, error) { return old.DeepCopy(), nil },
				archiveStateFn: func(context.Context, string, string) (*ebsv1.JobArchiveState, error) {
					phase := "Stored"
					if job == nil {
						phase = "Transferred"
					}
					return &ebsv1.JobArchiveState{UID: string(old.UID), Action: action, Phase: phase, Digest: "digest"}, nil
				},
				finalizeFn: func(_ context.Context, current *ebsv1.Job, isOld bool) error {
					if isOld {
						old.Finalizers = nil
						return nil
					}
					finalizeCalls++
					if current.UID != job.UID || current.ResourceVersion != job.ResourceVersion {
						t.Fatal("finalization must use the latest source identity and version")
					}
					if finalizeCalls == 1 {
						job.Finalizers, job.ResourceVersion = nil, "11"
						return &clientpkg.WriteError{Outcome: clientpkg.WriteUnknown, Err: errors.New("physical delete interrupted")}
					}
					job = nil
					return nil
				},
				deleteJobDataFn: func(context.Context, *ebsv1.Job) error { cleanupCalls++; return nil },
			}
			c, _, _, _ := newTestController(t, ts.Time, runningJob(), nil, client, false)
			defer c.Queue().ShutDown()
			if _, err := c.sync(ctx, "jobs/project/job"); err == nil || job == nil || len(job.Finalizers) != 0 {
				t.Fatalf("interrupted finalization: job=%+v err=%v", job, err)
			}
			if _, err := c.sync(ctx, "oldjobs/project/job"); err != nil || cleanupCalls != 0 {
				t.Fatalf("cleanup before source deletion: calls=%d err=%v", cleanupCalls, err)
			}
			// Recover with a fresh controller to ensure the retry does not depend on in-memory state.
			restarted, _, _, _ := newTestController(t, ts.Time, runningJob(), nil, client, false)
			defer restarted.Queue().ShutDown()
			if result, err := restarted.sync(ctx, "jobs/project/job"); err != nil || result != (controller.ReconcileResult{}) || job != nil || finalizeCalls != 2 {
				t.Fatalf("recovery: result=%+v err=%v source=%+v calls=%d", result, err, job, finalizeCalls)
			}
			if _, err := restarted.sync(ctx, "oldjobs/project/job"); err != nil {
				t.Fatal(err)
			}
			if (cleanupCalls == 1) != (action == "delete") {
				t.Fatalf("action=%s cleanup calls=%d", action, cleanupCalls)
			}
		})
	}
}

func TestOldJobCleanupScheduling(t *testing.T) {
	for _, tc := range []struct {
		name      string
		finalizer bool
		cleanup   error
		want      controller.ReconcileResult
		permanent bool
	}{
		{name: "already finalized"},
		{name: "finished", finalizer: true},
		{name: "retry after", finalizer: true, cleanup: &cleanupError{status: 503, retryAfter: 17 * time.Second},
			want: controller.ReconcileResult{RequeueAfter: 17 * time.Second}},
		{name: "temporary failure", finalizer: true, cleanup: errors.New("connection reset")},
		{name: "permission failure", finalizer: true, cleanup: &cleanupError{status: 403}, permanent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := runningJob()
			ts := metav1.NewTime(time.Unix(1000, 0))
			job.DeletionTimestamp = &ts
			if tc.finalizer {
				job.Finalizers = []string{ebsv1.JobArtifactsCleanupFinalizer}
			}
			finalized := false
			client := &fakeClient{
				getOldJobFn: func(context.Context, string, string) (*ebsv1.Job, error) { return job.DeepCopy(), nil },
				archiveStateFn: func(context.Context, string, string) (*ebsv1.JobArchiveState, error) {
					return &ebsv1.JobArchiveState{UID: string(job.UID), Phase: "Transferred"}, nil
				},
				deleteJobDataFn: func(context.Context, *ebsv1.Job) error { return tc.cleanup },
				finalizeFn:      func(context.Context, *ebsv1.Job, bool) error { finalized = true; return nil },
			}
			c, _, _, _ := newTestController(t, ts.Time, runningJob(), nil, client, false)
			defer c.Queue().ShutDown()
			result, err := c.sync(context.Background(), "oldjobs/project/job")
			wantError := tc.cleanup != nil && tc.want.RequeueAfter == 0
			if result != tc.want || (err != nil) != wantError || controller.IsPermanent(err) != tc.permanent {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if finalized != (tc.finalizer && tc.cleanup == nil) {
				t.Fatalf("finalized=%t", finalized)
			}
		})
	}
}

func TestAPIClientCleanupTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		want    time.Duration
	}{
		{name: "default", want: 30 * time.Second},
		{name: "nonpositive", timeout: -time.Second, want: 30 * time.Second},
		{name: "configured", timeout: time.Minute, want: time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newAPIClient(nil, nil, tc.timeout).(*apiClient)
			if client.http.Timeout != tc.want {
				t.Fatalf("timeout=%s, want %s", client.http.Timeout, tc.want)
			}
		})
	}
}

func TestAPIClientValidatesResponses(t *testing.T) {
	job := runningJob()
	job.TypeMeta = metav1.TypeMeta{APIVersion: "ebs/v1", Kind: "Job"}
	job.Status.Phase = ebsv1.JobFailed
	runnerObject := runner(ebsv1.RunnerOffline)
	runnerObject.TypeMeta = metav1.TypeMeta{APIVersion: "ebs/v1", Kind: "Runner"}
	for _, tc := range []struct {
		name  string
		path  string
		value runtime.Object
		write bool
		call  func(*apiClient) error
	}{
		{name: "Job", path: "/apis/ebs/v1/projects/project/jobs/job", value: job, call: func(c *apiClient) error {
			_, err := c.GetJob(context.Background(), "project", "job")
			return err
		}},
		{name: "OldJob", path: "/apis/ebs/v1/projects/project/oldjobs/job", value: job, call: func(c *apiClient) error {
			_, err := c.GetOldJob(context.Background(), "project", "job")
			return err
		}},
		{name: "Runner", value: runnerObject, call: func(c *apiClient) error {
			_, err := c.GetRunner(context.Background(), "runner")
			return err
		}},
		{name: "status", value: job, write: true, call: func(c *apiClient) error {
			_, err := c.UpdateJobStatus(context.Background(), job)
			return err
		}},
		{name: "archive", value: job, write: true, call: func(c *apiClient) error {
			_, err := c.ArchiveJob(context.Background(), "project", "job", clientpkg.DeletePreconditions{
				UID: job.UID, ResourceVersion: job.ResourceVersion,
			})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := tc.value.DeepCopyObject()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.path != "" && (r.Method != http.MethodGet || r.URL.Path != tc.path) {
					t.Errorf("request=%s %s, want GET %s", r.Method, r.URL.Path, tc.path)
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(value); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			shared, err := clientpkg.New(&rest.Config{Host: server.URL}, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			client := &apiClient{client: shared}
			if err := tc.call(client); err != nil {
				t.Fatalf("valid response: %v", err)
			}
			value.(metav1.Object).SetResourceVersion("")
			err = tc.call(client)
			if err == nil {
				t.Fatal("missing resourceVersion accepted")
			}
			if tc.write {
				var write *clientpkg.WriteError
				if !errors.As(err, &write) || write.Outcome != clientpkg.WriteUnknown {
					t.Fatalf("malformed write response: %v", err)
				}
			}
		})
	}
}

func TestHistoryScanPagesAndContinuesAfterDeleteFailure(t *testing.T) {
	now := time.Unix(10_000_000, 0)
	expired := runningJob()
	expired.Status.Phase = ebsv1.JobSucceeded
	expired.Status.EndTime = metav1.NewTime(now.Add(-31 * 24 * time.Hour))
	recent := expired.DeepCopy()
	recent.Name = "recent"
	recent.Status.EndTime = metav1.NewTime(now.Add(-time.Hour))
	second := expired.DeepCopy()
	second.Name = "second"
	client := &fakeClient{}
	var calls []string
	client.listHistoryFn = func(_ context.Context, phase ebsv1.JobPhase, token string, limit int64) (source.ListPage, error) {
		if limit != 500 {
			t.Fatalf("page size = %d", limit)
		}
		calls = append(calls, string(phase)+":"+token)
		if phase == ebsv1.JobSucceeded {
			if token == "" {
				return source.ListPage{Items: []runtime.Object{expired, recent}, Continue: "next"}, nil
			}
			return source.ListPage{Items: []runtime.Object{second}}, nil
		}
		return source.ListPage{}, nil
	}
	client.getJobFn = func(_ context.Context, _, name string) (*ebsv1.Job, error) {
		latest := expired.DeepCopy()
		latest.Name, latest.ResourceVersion = name, "22"
		return latest, nil
	}
	client.deleteFn = func(_ context.Context, _, name string, pre clientpkg.DeletePreconditions) error {
		if pre.UID != expired.UID || pre.ResourceVersion != "22" {
			t.Fatalf("delete preconditions = %+v", pre)
		}
		if name == expired.Name {
			return &clientpkg.WriteError{Outcome: clientpkg.WriteRejected, StatusCode: http.StatusConflict}
		}
		return nil
	}
	c, _, _, _ := newTestController(t, now, expired, nil, client, true)
	defer c.Queue().ShutDown()
	if err := c.scanHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(calls); got != "[Succeeded: Succeeded:next Failed: Aborted:]" {
		t.Fatalf("list calls = %s", got)
	}
	if client.deleteCalls != 2 || client.getJobCalls != 2 || c.Queue().Len() != 0 {
		t.Fatalf("deletes=%d gets=%d queued=%d", client.deleteCalls, client.getJobCalls, c.Queue().Len())
	}
}

func TestHistoryLoopScansAtStartupAndDaily(t *testing.T) {
	scans := make(chan struct{}, 2)
	client := &fakeClient{listHistoryFn: func(_ context.Context, phase ebsv1.JobPhase, _ string, _ int64) (source.ListPage, error) {
		if phase == ebsv1.JobSucceeded {
			scans <- struct{}{}
		}
		return source.ListPage{}, nil
	}}
	c, clk, _, _ := newTestController(t, time.Unix(1000, 0), runningJob(), nil, client, true)
	defer c.Queue().ShutDown()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); c.historyLoop(ctx) }()
	waitScan := func() {
		t.Helper()
		select {
		case <-scans:
		case <-time.After(time.Second):
			t.Fatal("history scan did not start")
		}
	}
	waitScan()
	deadline := time.Now().Add(time.Second)
	for !clk.HasWaiters() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !clk.HasWaiters() {
		t.Fatal("daily timer was not started")
	}
	clk.Step(23 * time.Hour)
	select {
	case <-scans:
		t.Fatal("history scanned before 24 hours")
	default:
	}
	clk.Step(time.Hour)
	waitScan()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("history scan did not stop")
	}
}

func TestHistoryGCRetentionAndEnablement(t *testing.T) {
	now := time.Unix(10_000_000, 0)
	retention := 30 * 24 * time.Hour
	for _, tc := range []struct {
		name string
		age  time.Duration
		gc   bool
		want int
	}{
		{name: "before deadline", age: retention - time.Second, gc: true},
		{name: "at deadline", age: retention, gc: true, want: 1},
		{name: "after deadline", age: retention + time.Second, gc: true, want: 1},
		{name: "disabled with expired Job", age: retention + time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := runningJob()
			job.Status.Phase = ebsv1.JobFailed
			job.Status.EndTime = metav1.NewTime(now.Add(-tc.age))
			client := &fakeClient{
				getJobFn: func(context.Context, string, string) (*ebsv1.Job, error) { return job.DeepCopy(), nil },
				deleteFn: func(context.Context, string, string, clientpkg.DeletePreconditions) error { return nil },
			}
			c, _, _, _ := newTestController(t, now, job, nil, client, tc.gc)
			defer c.Queue().ShutDown()
			if err := c.reconcileHistory(context.Background(), "project/job", job, now); err != nil {
				t.Fatal(err)
			}
			if client.getJobCalls != tc.want || client.deleteCalls != tc.want {
				t.Fatalf("gets=%d deletes=%d, want %d each", client.getJobCalls, client.deleteCalls, tc.want)
			}
		})
	}
}

func TestReadErrorClassification(t *testing.T) {
	for _, code := range []int{401, 403, 409, 410, 412, 422} {
		err := apierrors.NewGenericServerResponse(code, "get", schema.GroupResource{Group: "ebs", Resource: "jobs"}, "job", "failure", 0, false)
		if !controller.IsPermanent(classifyReadError(err)) {
			t.Fatalf("read status %d was not permanent", code)
		}
	}
	for _, code := range []int{408, 429, 500, 503} {
		err := apierrors.NewGenericServerResponse(code, "get", schema.GroupResource{Group: "ebs", Resource: "jobs"}, "job", "failure", 0, false)
		if controller.IsPermanent(classifyReadError(err)) {
			t.Fatalf("read status %d was permanent", code)
		}
	}
}

func TestStatusWriteResponseMatrix(t *testing.T) {
	request := runningJob()
	client := &fakeClient{
		getJobFn:    func(context.Context, string, string) (*ebsv1.Job, error) { return request.DeepCopy(), nil },
		getRunnerFn: func(context.Context, string) (*ebsv1.Runner, error) { return runner("Offline"), nil },
		updateFn:    func(context.Context, *ebsv1.Job) (*ebsv1.Job, error) { return nil, nil },
		deleteFn:    func(context.Context, string, string, clientpkg.DeletePreconditions) error { return nil },
	}
	c, _, _, _ := newTestController(t, time.Unix(1000, 0), request, runner("Offline"), client, true)
	tests := []struct {
		code      int
		requeue   bool
		wantError bool
		permanent bool
	}{
		{code: 404},
		{code: 409, requeue: true},
		{code: 412, requeue: true},
		{code: 408, wantError: true},
		{code: 429, wantError: true},
		{code: 500, wantError: true},
		{code: 503, wantError: true},
		{code: 302, wantError: true, permanent: true},
		{code: 400, wantError: true, permanent: true},
		{code: 401, wantError: true, permanent: true},
		{code: 403, wantError: true, permanent: true},
		{code: 422, wantError: true, permanent: true},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.code), func(t *testing.T) {
			err := &clientpkg.WriteError{Outcome: clientpkg.WriteRejected, StatusCode: tt.code, Err: errors.New("rejected")}
			result, gotErr := c.handleStatusWriteError(context.Background(), "project/job", request, err)
			if result.Requeue != tt.requeue || (gotErr != nil) != tt.wantError || controller.IsPermanent(gotErr) != tt.permanent {
				t.Fatalf("code=%d result=%+v err=%v permanent=%t", tt.code, result, gotErr, controller.IsPermanent(gotErr))
			}
		})
	}
}

func TestRunnerIndexConcurrentAccess(t *testing.T) {
	index := newRunnerIndex()
	var group sync.WaitGroup
	for n := 0; n < 20; n++ {
		group.Add(1)
		go func(n int) {
			defer group.Done()
			key := "project/job-" + string(rune('a'+n))
			index.set(key, "runner")
			_ = index.jobs("runner")
			index.remove(key)
		}(n)
	}
	group.Wait()
}

func TestEventHandlersMaintainIndexAndIgnoreHeartbeat(t *testing.T) {
	job := runningJob()
	client := &fakeClient{
		getJobFn:    func(context.Context, string, string) (*ebsv1.Job, error) { return job, nil },
		getRunnerFn: func(context.Context, string) (*ebsv1.Runner, error) { return runner("Online"), nil },
		updateFn:    func(context.Context, *ebsv1.Job) (*ebsv1.Job, error) { return nil, nil },
		deleteFn:    func(context.Context, string, string, clientpkg.DeletePreconditions) error { return nil },
	}
	c, _, _, _ := newTestController(t, time.Unix(1000, 0), job, runner("Online"), client, true)
	c.onJobAdd(job)
	item, _ := c.Queue().Get()
	c.Queue().Done(item)
	c.Queue().Forget(item)

	oldRunner := runner("Online")
	newRunner := oldRunner.DeepCopy()
	newRunner.ResourceVersion = "8"
	newRunner.Status.Heartbeat = metav1.NewTime(time.Unix(2000, 0))
	c.onRunnerUpdate(oldRunner, newRunner)
	if c.Queue().Len() != 0 {
		t.Fatal("ordinary heartbeat enqueued bound Jobs")
	}
	newRunner.Status.Phase = ebsv1.RunnerOffline
	c.onRunnerUpdate(oldRunner, newRunner)
	if c.Queue().Len() != 1 {
		t.Fatal("Offline transition did not enqueue bound Job")
	}
	c.onJobDelete(job)
	if jobs := c.index.jobs("runner"); len(jobs) != 0 {
		t.Fatalf("deleted Job remains indexed: %v", jobs)
	}
}

type countingClock struct {
	clock.Clock
	mu    sync.Mutex
	calls int
}

func (c *countingClock) Now() time.Time {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.Clock.Now()
}

func TestSyncReadsClockOnce(t *testing.T) {
	now := time.Unix(1000, 0)
	job := runningJob()
	sourceJobs := &fakeCachedSource{objects: map[string]runtime.Object{"project/job": job}}
	sourceRunners := &fakeCachedSource{objects: map[string]runtime.Object{"runner": runner("Online")}}
	client := &fakeClient{
		getJobFn:    func(context.Context, string, string) (*ebsv1.Job, error) { return job, nil },
		getRunnerFn: func(context.Context, string) (*ebsv1.Runner, error) { return runner("Online"), nil },
		updateFn:    func(context.Context, *ebsv1.Job) (*ebsv1.Job, error) { return nil, nil },
		deleteFn:    func(context.Context, string, string, clientpkg.DeletePreconditions) error { return nil },
	}
	clk := &countingClock{Clock: clocktesting.NewFakeClock(now)}
	c, err := New(sourceJobs, sourceRunners, client, clk, Config{RunnerLostGracePeriod: time.Minute, HistoryGCEnabled: true, HistoryRetention: time.Hour, MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.sync(context.Background(), "project/job"); err != nil {
		t.Fatal(err)
	}
	if clk.calls != 1 {
		t.Fatalf("Now called %d times", clk.calls)
	}
}
