package runner

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
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
)

type fakeCachedSource struct {
	mu       sync.RWMutex
	objects  map[string]runtime.Object
	handlers []source.ResourceEventHandler
}

func (s *fakeCachedSource) Name() string { return "fake-runners" }
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

type fakeClient struct {
	getFn       func(context.Context, string) (*ebsv1.Runner, error)
	updateFn    func(context.Context, *ebsv1.Runner) (*ebsv1.Runner, error)
	getCalls    int
	updateCalls int
}

func (c *fakeClient) GetRunner(ctx context.Context, name string) (*ebsv1.Runner, error) {
	c.getCalls++
	return c.getFn(ctx, name)
}
func (c *fakeClient) UpdateRunnerStatus(ctx context.Context, runner *ebsv1.Runner) (*ebsv1.Runner, error) {
	c.updateCalls++
	return c.updateFn(ctx, runner)
}

func testRunner(phase string, heartbeat time.Time) *ebsv1.Runner {
	value := &ebsv1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: "runner", UID: types.UID("runner-uid"), ResourceVersion: "7", CreationTimestamp: metav1.NewTime(time.Unix(100, 0))},
		Spec:       ebsv1.RunnerSpec{InstanceID: "instance", Type: "host", Arch: "x86_64", Taints: []ebsv1.RunnerTaint{{Key: "dedicated", Effect: "NoSchedule"}}},
		Status: ebsv1.RunnerStatus{
			Phase:       phase,
			Conditions:  []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}},
			Capacity:    map[string]string{"cpu": "8"},
			Allocatable: map[string]string{"cpu": "7"},
			Addresses:   []ebsv1.RunnerAddress{{Type: "InternalIP", Address: "127.0.0.1"}},
			Info:        ebsv1.RunnerInfo{OS: "openEuler"},
		},
	}
	if !heartbeat.IsZero() {
		value.Status.Heartbeat = metav1.NewTime(heartbeat)
	}
	return value
}

func newTestController(t *testing.T, now time.Time, cached *ebsv1.Runner, client Client) (*Controller, *fakeCachedSource) {
	t.Helper()
	objects := make(map[string]runtime.Object)
	if cached != nil {
		objects[cached.Name] = cached
	}
	s := &fakeCachedSource{objects: objects}
	c, err := New(s, client, clocktesting.NewFakeClock(now), Config{HeartbeatTimeout: 2 * time.Minute, StartupGracePeriod: 5 * time.Minute, MaxRetries: 3})
	if err != nil {
		t.Fatal(err)
	}
	return c, s
}

func TestCalculateHealthDeadlineBoundaries(t *testing.T) {
	now := time.Unix(1000, 0)
	runner := testRunner("Idle", now)
	config := Config{HeartbeatTimeout: 2 * time.Minute, StartupGracePeriod: 5 * time.Minute}
	result, err := calculateHealthDeadline(runner, now, config)
	if err != nil || result.Expired || result.RequeueAfter != 2*time.Minute || result.Basis != "heartbeat" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	result, err = calculateHealthDeadline(runner, now.Add(2*time.Minute), config)
	if err != nil || !result.Expired || result.RequeueAfter != 0 {
		t.Fatalf("deadline equality result=%+v err=%v", result, err)
	}
	runner.Status.Heartbeat = metav1.NewTime(now.Add(30 * 24 * time.Hour))
	result, err = calculateHealthDeadline(runner, now, config)
	if err != nil || result.RequeueAfter != maxHealthRequeueAfter || !result.FutureBasis {
		t.Fatalf("future result=%+v err=%v", result, err)
	}
}

func TestCalculateHealthDeadlineUsesCreationAndRejectsMissingTime(t *testing.T) {
	now := time.Unix(1000, 0)
	runner := testRunner("Booting", time.Time{})
	runner.CreationTimestamp = metav1.NewTime(now)
	result, err := calculateHealthDeadline(runner, now, Config{HeartbeatTimeout: time.Minute, StartupGracePeriod: 5 * time.Minute})
	if err != nil || result.Basis != "creationTimestamp" || result.RequeueAfter != 5*time.Minute {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	runner.CreationTimestamp = metav1.Time{}
	if _, err := calculateHealthDeadline(runner, now, Config{HeartbeatTimeout: time.Minute, StartupGracePeriod: time.Minute}); err == nil {
		t.Fatal("missing timestamps were accepted")
	} else {
		var target objectTimestampError
		if !errors.As(err, &target) {
			t.Fatalf("error %T is not an object timestamp error", err)
		}
	}
}

func TestCalculateHealthDeadlineRejectsOverflow(t *testing.T) {
	base := time.Date(9999, 12, 31, 23, 59, 0, 0, time.UTC)
	runner := testRunner("Idle", base)
	_, err := calculateHealthDeadline(runner, base, Config{HeartbeatTimeout: 2 * time.Minute, StartupGracePeriod: time.Minute})
	var target objectTimestampError
	if !errors.As(err, &target) {
		t.Fatalf("overflow error=%v, want object timestamp error", err)
	}
}

func TestSyncSchedulesBeforeDeadlineWithoutAPI(t *testing.T) {
	now := time.Unix(1000, 0)
	runner := testRunner("Idle", now)
	client := &fakeClient{
		getFn: func(context.Context, string) (*ebsv1.Runner, error) { t.Fatal("unexpected GET"); return nil, nil },
		updateFn: func(context.Context, *ebsv1.Runner) (*ebsv1.Runner, error) {
			t.Fatal("unexpected update")
			return nil, nil
		},
	}
	c, _ := newTestController(t, now, runner, client)
	result, err := c.sync(context.Background(), runner.Name)
	if err != nil || result.RequeueAfter != 2*time.Minute || client.getCalls != 0 || client.updateCalls != 0 {
		t.Fatalf("result=%+v err=%v calls=%d/%d", result, err, client.getCalls, client.updateCalls)
	}
}

func TestSyncAuthoritativeHeartbeatPreventsOffline(t *testing.T) {
	now := time.Unix(1000, 0)
	cached := testRunner("Running", now.Add(-3*time.Minute))
	latest := cached.DeepCopy()
	latest.ResourceVersion = "8"
	latest.Status.Heartbeat = metav1.NewTime(now)
	client := &fakeClient{
		getFn: func(context.Context, string) (*ebsv1.Runner, error) { return latest.DeepCopy(), nil },
		updateFn: func(context.Context, *ebsv1.Runner) (*ebsv1.Runner, error) {
			t.Fatal("unexpected update")
			return nil, nil
		},
	}
	c, _ := newTestController(t, now, cached, client)
	result, err := c.sync(context.Background(), cached.Name)
	if err != nil || result.RequeueAfter != 2*time.Minute || client.getCalls != 1 || client.updateCalls != 0 {
		t.Fatalf("result=%+v err=%v calls=%d/%d", result, err, client.getCalls, client.updateCalls)
	}
}

func TestSyncMarksExpiredRunnerOfflineAndPreservesFields(t *testing.T) {
	now := time.Unix(1000, 0)
	runner := testRunner("Running", now.Add(-3*time.Minute))
	var request *ebsv1.Runner
	client := &fakeClient{}
	client.getFn = func(context.Context, string) (*ebsv1.Runner, error) { return runner.DeepCopy(), nil }
	client.updateFn = func(_ context.Context, value *ebsv1.Runner) (*ebsv1.Runner, error) {
		request = value.DeepCopy()
		response := value.DeepCopy()
		response.ResourceVersion = "8"
		return response, nil
	}
	c, _ := newTestController(t, now, runner, client)
	result, err := c.sync(context.Background(), runner.Name)
	if err != nil || result != (controller.ReconcileResult{}) || request == nil {
		t.Fatalf("result=%+v err=%v request=%v", result, err, request)
	}
	if request.Status.Phase != "Offline" || request.Status.Heartbeat != runner.Status.Heartbeat || request.Spec.InstanceID != runner.Spec.InstanceID || request.Status.Capacity["cpu"] != "8" {
		t.Fatalf("request did not preserve Runner fields: %+v", request)
	}
}

func TestSyncConflictImmediatelyRequeues(t *testing.T) {
	now := time.Unix(1000, 0)
	runner := testRunner("Running", now.Add(-3*time.Minute))
	client := &fakeClient{}
	client.getFn = func(context.Context, string) (*ebsv1.Runner, error) { return runner.DeepCopy(), nil }
	client.updateFn = func(context.Context, *ebsv1.Runner) (*ebsv1.Runner, error) {
		return nil, &clientpkg.WriteError{Outcome: clientpkg.WriteRejected, StatusCode: http.StatusConflict, Err: apierrors.NewConflict(schema.GroupResource{Group: "ebs", Resource: "runners"}, runner.Name, errors.New("conflict"))}
	}
	c, _ := newTestController(t, now, runner, client)
	result, err := c.sync(context.Background(), runner.Name)
	if err != nil || !result.Requeue {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestSyncUnknownWriteConfirmsOffline(t *testing.T) {
	now := time.Unix(1000, 0)
	runner := testRunner("Running", now.Add(-3*time.Minute))
	gets := 0
	client := &fakeClient{}
	client.getFn = func(context.Context, string) (*ebsv1.Runner, error) {
		gets++
		value := runner.DeepCopy()
		if gets == 2 {
			value.Status.Phase = "Offline"
		}
		return value, nil
	}
	client.updateFn = func(context.Context, *ebsv1.Runner) (*ebsv1.Runner, error) {
		return nil, &clientpkg.WriteError{Outcome: clientpkg.WriteUnknown, Err: context.DeadlineExceeded}
	}
	c, _ := newTestController(t, now, runner, client)
	result, err := c.sync(context.Background(), runner.Name)
	if err != nil || result != (controller.ReconcileResult{}) || gets != 2 {
		t.Fatalf("result=%+v err=%v gets=%d", result, err, gets)
	}
}

func TestInvalidPhaseIsPermanent(t *testing.T) {
	runner := testRunner("Unknown", time.Unix(1000, 0))
	client := &fakeClient{getFn: func(context.Context, string) (*ebsv1.Runner, error) { return nil, nil }, updateFn: func(context.Context, *ebsv1.Runner) (*ebsv1.Runner, error) { return nil, nil }}
	c, _ := newTestController(t, time.Unix(1000, 0), runner, client)
	_, err := c.sync(context.Background(), runner.Name)
	if !controller.IsPermanent(err) {
		t.Fatalf("error=%v is not permanent", err)
	}
}

func TestShouldEnqueueRunnerUpdate(t *testing.T) {
	old := testRunner("Idle", time.Unix(1000, 0))
	unchanged := old.DeepCopy()
	unchanged.ResourceVersion = "8"
	if shouldEnqueueRunnerUpdate(old, unchanged) {
		t.Fatal("unrelated resourceVersion change enqueued Runner")
	}
	resync := old.DeepCopy()
	if !shouldEnqueueRunnerUpdate(old, resync) {
		t.Fatal("resync did not enqueue Runner")
	}
	heartbeat := unchanged.DeepCopy()
	heartbeat.Status.Heartbeat = metav1.NewTime(time.Unix(1001, 0))
	if !shouldEnqueueRunnerUpdate(old, heartbeat) {
		t.Fatal("heartbeat change did not enqueue Runner")
	}
}

func TestValidateOfflineStatusResponseRejectsChangedFields(t *testing.T) {
	request := testRunner("Offline", time.Unix(1000, 0))
	response := request.DeepCopy()
	response.ResourceVersion = "8"
	if err := validateOfflineStatusResponse(response, request); err != nil {
		t.Fatalf("valid response rejected: %v", err)
	}
	response.Status.Capacity["cpu"] = "9"
	err := validateOfflineStatusResponse(response, request)
	var writeErr *clientpkg.WriteError
	if !errors.As(err, &writeErr) || writeErr.Outcome != clientpkg.WriteUnknown {
		t.Fatalf("changed response error=%v, want WriteUnknown", err)
	}
}
