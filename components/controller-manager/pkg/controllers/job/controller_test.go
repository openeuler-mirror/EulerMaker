package job

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	clientpkg "controller-manager/pkg/client"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/clock"
	clocktesting "k8s.io/utils/clock/testing"
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
	getJobFn       func(context.Context, string, string) (*ebsv1.Job, error)
	getRunnerFn    func(context.Context, string) (*ebsv1.Runner, error)
	updateFn       func(context.Context, *ebsv1.Job) (*ebsv1.Job, error)
	deleteFn       func(context.Context, string, string, clientpkg.DeletePreconditions) error
	getJobCalls    int
	getRunnerCalls int
	updateCalls    int
	deleteCalls    int
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
		Status:     ebsv1.JobStatus{Phase: "Running", Stage: "PostRun", Runner: "runner", StartTime: metav1.NewTime(time.Unix(100, 0)), ResultRoot: "result", Message: "old", RestartCount: 3},
	}
}

func runner(phase string) *ebsv1.Runner {
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

func TestRunnerAvailableOnlyWhenOnline(t *testing.T) {
	for _, phase := range []string{"Online", "Offline", "Unknown", ""} {
		t.Run(phase, func(t *testing.T) {
			job := runningJob()
			c, _, _, _ := newTestController(t, time.Unix(1000, 0), job, runner(phase), &fakeClient{}, false)
			available, err := c.runnerAvailableFromCache("runner")
			if err != nil {
				t.Fatal(err)
			}
			if available != (phase == "Online") {
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
	if request.Status.Phase != "Failed" || request.Status.Stage != "PostRun" || request.Status.Runner != "runner" || request.Status.ResultRoot != "result" || request.Status.RestartCount != 3 {
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
			value.Status.Phase = "Failed"
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

func TestHistoryGCUsesLatestPreconditions(t *testing.T) {
	now := time.Unix(10_000_000, 0)
	job := runningJob()
	job.Status.Phase = "Completed"
	job.Status.EndTime = metav1.NewTime(now.Add(-30 * 24 * time.Hour))
	latest := job.DeepCopy()
	latest.ResourceVersion = "22"
	var got clientpkg.DeletePreconditions
	client := &fakeClient{}
	client.getJobFn = func(context.Context, string, string) (*ebsv1.Job, error) { return latest.DeepCopy(), nil }
	client.getRunnerFn = func(context.Context, string) (*ebsv1.Runner, error) { return nil, errors.New("unused") }
	client.updateFn = func(context.Context, *ebsv1.Job) (*ebsv1.Job, error) { return nil, errors.New("unused") }
	client.deleteFn = func(_ context.Context, _, _ string, value clientpkg.DeletePreconditions) error {
		got = value
		return nil
	}
	c, _, _, _ := newTestController(t, now, job, nil, client, true)
	result, err := c.sync(context.Background(), "project/job")
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if got.UID != latest.UID || got.ResourceVersion != "22" {
		t.Fatalf("preconditions=%+v", got)
	}
}

func TestHistoryGCWaitsAndCanBeDisabled(t *testing.T) {
	now := time.Unix(10_000_000, 0)
	job := runningJob()
	job.Status.Phase = "Failed"
	job.Status.EndTime = metav1.NewTime(now.Add(-time.Hour))
	client := &fakeClient{
		getJobFn:    func(context.Context, string, string) (*ebsv1.Job, error) { t.Fatal("unexpected GET"); return nil, nil },
		getRunnerFn: func(context.Context, string) (*ebsv1.Runner, error) { return nil, nil },
		updateFn:    func(context.Context, *ebsv1.Job) (*ebsv1.Job, error) { return nil, nil },
		deleteFn:    func(context.Context, string, string, clientpkg.DeletePreconditions) error { return nil },
	}
	c, _, _, _ := newTestController(t, now, job, nil, client, true)
	result, err := c.sync(context.Background(), "project/job")
	if err != nil || result.RequeueAfter != 30*24*time.Hour-time.Hour {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	c.config.HistoryGCEnabled = false
	result, err = c.sync(context.Background(), "project/job")
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("disabled GC result=%+v err=%v", result, err)
	}
}

func TestReadAndWriteErrorClassification(t *testing.T) {
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
	newRunner.Status.Phase = "Offline"
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
