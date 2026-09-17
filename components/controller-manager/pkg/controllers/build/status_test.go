package build

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// pendingWriteRound builds a round that always wants to move Pending to Prepared, so the status write protocol
// can be exercised in isolation.
func pendingWriteRound(t *testing.T) (*fakeAPI, *Controller) {
	t.Helper()
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
	api.storeSnapshot(&ebsv1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a"},
		Status:     ebsv1.SnapshotStatus{Phase: ebsv1.SnapshotActive},
	})
	return api, newTestController(t, api, newTestClock())
}

// buildStageWriteRound builds a round that advances a non single build to the publish stage, so the status write
// protocol can be exercised with a condition intent whose target phase stays non terminal.
func buildStageWriteRound(t *testing.T) (*fakeAPI, *Controller) {
	t.Helper()
	api := newFakeAPI()
	build := withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildProcessing, stageBuild)
	build.Status.StartTime = testTime(9)
	api.builds[key("project-a", "build-a")] = build
	api.buildInfos[key("project-a", "build-a")] = newBuildInfoObject("project-a", "build-a", ebsv1.BuildInfoCompleted,
		map[string]ebsv1.SpecStatus{"gcc": specStatus(specStatusSucceeded, specStatusSucceeded)})
	return api, newTestController(t, api, newTestClock())
}

func (f *fakeAPI) interceptBuild(t *testing.T, latest func(project, name string, call int) (*ebsv1.Build, error)) {
	t.Helper()
	calls := 0
	f.hooks.getBuild = func(project, name string) (*ebsv1.Build, error) {
		calls++
		if calls == 1 {
			return f.build(project, name), nil
		}
		return latest(project, name, calls)
	}
}

func (f *fakeAPI) deleteBuild(project, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.builds, key(project, name))
}

func TestPreWriteConcurrencyGuards(t *testing.T) {
	deletion := metav1.NewTime(time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC))
	for _, tc := range []struct {
		name          string
		mutate        func(*ebsv1.Build)
		wantRequeue   time.Duration
		wantConflicts uint64
	}{
		{name: "resourceVersion changed", mutate: func(build *ebsv1.Build) { build.ResourceVersion = "999" }, wantRequeue: time.Second, wantConflicts: 1},
		{name: "uid changed", mutate: func(build *ebsv1.Build) { build.UID = "other-uid" }},
		{name: "object aborted", mutate: func(build *ebsv1.Build) { build.Status.Phase = ebsv1.BuildAborted }},
		{name: "object deleting", mutate: func(build *ebsv1.Build) { build.DeletionTimestamp = &deletion }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, c := pendingWriteRound(t)
			before := counterValue(t, metricStatusUpdateConflicts)
			api.interceptBuild(t, func(project, name string, _ int) (*ebsv1.Build, error) {
				latest := api.build(project, name)
				tc.mutate(latest)
				return latest, nil
			})
			result, err := c.sync(context.Background(), "project-a/build-a")
			if err != nil {
				t.Fatalf("sync: %v", err)
			}
			if result.RequeueAfter != tc.wantRequeue {
				t.Fatalf("result=%+v, want requeue after %s", result, tc.wantRequeue)
			}
			if api.CallCount("UpdateBuildStatus") != 0 {
				t.Fatal("a stale decision must not be written")
			}
			assertCounterDelta(t, metricStatusUpdateConflicts, before, tc.wantConflicts)
		})
	}

	t.Run("object deleted", func(t *testing.T) {
		api, c := pendingWriteRound(t)
		before := counterValue(t, metricStatusUpdateConflicts)
		api.interceptBuild(t, func(project, name string, _ int) (*ebsv1.Build, error) {
			return nil, apierrors.NewNotFound(buildsResource, name)
		})
		result, err := c.sync(context.Background(), "project-a/build-a")
		if err != nil || result != (controller.ReconcileResult{}) {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if api.CallCount("UpdateBuildStatus") != 0 {
			t.Fatal("a deleted Build must not be written")
		}
		assertCounterDelta(t, metricStatusUpdateConflicts, before, 0)
	})
}

func TestStatusWriteUnknownConfirmation(t *testing.T) {
	t.Run("confirmed by the original intent", func(t *testing.T) {
		api, c := pendingWriteRound(t)
		before := counterValue(t, metricStatusUpdateUnknown)
		api.hooks.updateStatus = func(request *ebsv1.Build) (*ebsv1.Build, error) {
			if _, err := api.commitStatus(request); err != nil {
				t.Fatalf("commit: %v", err)
			}
			return nil, &clientpkg.WriteError{Operation: "update-status", Outcome: clientpkg.WriteUnknown, Err: errors.New("lost response")}
		}
		result, err := c.sync(context.Background(), "project-a/build-a")
		if err != nil || result != (controller.ReconcileResult{}) {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if stored := api.build("project-a", "build-a"); stored.Status.Phase != ebsv1.BuildPrepared {
			t.Fatalf("phase = %q", stored.Status.Phase)
		}
		if api.CallCount("UpdateBuildStatus") != 1 {
			t.Fatal("the original write must not be replayed")
		}
		if api.CallCount("CreateBuildInfo") != 0 {
			t.Fatal("a confirmed write must end the round instead of entering the next stage")
		}
		assertCounterDelta(t, metricStatusUpdateUnknown, before, 1)
	})

	t.Run("condition order and unrelated fields do not affect confirmation", func(t *testing.T) {
		api, c := buildStageWriteRound(t)
		before := counterValue(t, metricStatusUpdateUnknown)
		api.hooks.updateStatus = func(request *ebsv1.Build) (*ebsv1.Build, error) {
			stored, err := api.commitStatus(request)
			if err != nil {
				t.Fatalf("commit: %v", err)
			}
			// The server answers with the persisted write, but its condition list is reordered and padded with
			// unrelated conditions and an unrelated status field changed: the confirmation must still accept it.
			written := findCondition(t, stored.Status.Conditions, ConditionBuildSucceed)
			mutated := stored.DeepCopy()
			mutated.Status.Conditions = []metav1.Condition{
				{Type: "ZUnrelated", Status: metav1.ConditionTrue, Reason: "Kept", Message: "Kept"},
				{Type: "AUnrelated", Status: metav1.ConditionFalse, Reason: "Kept", Message: "Kept"},
				written,
			}
			mutated.Status.StartTime = testTime(8)
			api.storeBuild(mutated)
			return nil, &clientpkg.WriteError{Operation: "update-status", Outcome: clientpkg.WriteUnknown, Err: errors.New("lost response")}
		}
		result, err := c.sync(context.Background(), "project-a/build-a")
		if err != nil || result != (controller.ReconcileResult{}) {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if api.CallCount("UpdateBuildStatus") != 1 {
			t.Fatal("a confirmed write must not be replayed")
		}
		stored := api.build("project-a", "build-a")
		if stored.Status.Phase != ebsv1.BuildProcessing || stored.Status.Stage != stagePublish {
			t.Fatalf("phase/stage = %q/%q", stored.Status.Phase, stored.Status.Stage)
		}
		if !stored.Status.StartTime.Time.Equal(testTime(8).Time) {
			t.Fatalf("unrelated server fields must be preserved: %+v", stored.Status.StartTime)
		}
		for _, unrelated := range []string{"ZUnrelated", "AUnrelated"} {
			if _, ok := testCondition(stored.Status.Conditions, unrelated); !ok {
				t.Fatalf("unrelated condition %s must be preserved: %+v", unrelated, stored.Status.Conditions)
			}
		}
		if buildCondition := findCondition(t, stored.Status.Conditions, ConditionBuildSucceed); buildCondition.Status != metav1.ConditionTrue {
			t.Fatalf("build condition = %+v", buildCondition)
		}
		assertCounterDelta(t, metricStatusUpdateUnknown, before, 1)
	})

	t.Run("condition mismatch keeps the round requeued", func(t *testing.T) {
		api, c := buildStageWriteRound(t)
		before := counterValue(t, metricStatusUpdateUnknown)
		api.hooks.updateStatus = func(request *ebsv1.Build) (*ebsv1.Build, error) {
			stored, err := api.commitStatus(request)
			if err != nil {
				t.Fatalf("commit: %v", err)
			}
			// The server persisted a different build result than the one this round intended, while the target
			// phase/stage stay non terminal: the confirmation must not accept the write.
			mutated := stored.DeepCopy()
			for i := range mutated.Status.Conditions {
				if mutated.Status.Conditions[i].Type != ConditionBuildSucceed {
					continue
				}
				mutated.Status.Conditions[i].Status = metav1.ConditionFalse
				mutated.Status.Conditions[i].Reason = ReasonBuildFailed
				mutated.Status.Conditions[i].Message = ReasonBuildFailed
			}
			api.storeBuild(mutated)
			return nil, &clientpkg.WriteError{Operation: "update-status", Outcome: clientpkg.WriteUnknown, Err: errors.New("lost response")}
		}
		result, err := c.sync(context.Background(), "project-a/build-a")
		if err != nil {
			t.Fatalf("sync: %v", err)
		}
		if result.RequeueAfter != time.Second {
			t.Fatalf("result=%+v", result)
		}
		if api.CallCount("UpdateBuildStatus") != 1 {
			t.Fatal("a mismatched confirmation must not replay the write")
		}
		assertCounterDelta(t, metricStatusUpdateUnknown, before, 1)
	})

	t.Run("target not reached", func(t *testing.T) {
		api, c := pendingWriteRound(t)
		before := counterValue(t, metricStatusUpdateUnknown)
		api.hooks.updateStatus = func(*ebsv1.Build) (*ebsv1.Build, error) {
			return nil, &clientpkg.WriteError{Operation: "update-status", Outcome: clientpkg.WriteUnknown, Err: errors.New("lost response")}
		}
		result, err := c.sync(context.Background(), "project-a/build-a")
		if err != nil {
			t.Fatalf("sync: %v", err)
		}
		if result.RequeueAfter != time.Second {
			t.Fatalf("result=%+v", result)
		}
		if stored := api.build("project-a", "build-a"); stored.Status.Phase != ebsv1.BuildPending {
			t.Fatalf("phase = %q", stored.Status.Phase)
		}
		assertCounterDelta(t, metricStatusUpdateUnknown, before, 1)
	})

	t.Run("object deleted", func(t *testing.T) {
		api, c := pendingWriteRound(t)
		before := counterValue(t, metricStatusUpdateUnknown)
		api.hooks.updateStatus = func(request *ebsv1.Build) (*ebsv1.Build, error) {
			api.deleteBuild(request.Namespace, request.Name)
			return nil, &clientpkg.WriteError{Operation: "update-status", Outcome: clientpkg.WriteUnknown, Err: errors.New("lost response")}
		}
		result, err := c.sync(context.Background(), "project-a/build-a")
		if err != nil || result != (controller.ReconcileResult{}) {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		assertCounterDelta(t, metricStatusUpdateUnknown, before, 1)
	})

	t.Run("concurrently aborted", func(t *testing.T) {
		api, c := pendingWriteRound(t)
		before := counterValue(t, metricStatusUpdateUnknown)
		api.hooks.updateStatus = func(request *ebsv1.Build) (*ebsv1.Build, error) {
			aborted := api.build(request.Namespace, request.Name)
			aborted.Status.Phase = ebsv1.BuildAborted
			api.storeBuild(aborted)
			return nil, &clientpkg.WriteError{Operation: "update-status", Outcome: clientpkg.WriteUnknown, Err: errors.New("lost response")}
		}
		result, err := c.sync(context.Background(), "project-a/build-a")
		if err != nil || result != (controller.ReconcileResult{}) {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if stored := api.build("project-a", "build-a"); stored.Status.Phase != ebsv1.BuildAborted {
			t.Fatalf("external Aborted was overwritten: %q", stored.Status.Phase)
		}
		assertCounterDelta(t, metricStatusUpdateUnknown, before, 1)
	})
}

func TestStatusWriteRejections(t *testing.T) {
	for _, tc := range []struct {
		name          string
		statusCode    int
		wantRequeue   time.Duration
		wantPermanent bool
		wantError     bool
		wantConflicts uint64
	}{
		{name: "conflict", statusCode: 409, wantRequeue: time.Second, wantConflicts: 1},
		{name: "precondition failed", statusCode: 412, wantRequeue: time.Second, wantConflicts: 1},
		{name: "deleted", statusCode: 404},
		{name: "request timeout", statusCode: 408, wantError: true},
		{name: "too many requests", statusCode: 429, wantError: true},
		{name: "server error", statusCode: 500, wantError: true},
		{name: "unavailable", statusCode: 503, wantError: true},
		{name: "invalid", statusCode: 422, wantPermanent: true},
		{name: "forbidden", statusCode: 403, wantPermanent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, c := pendingWriteRound(t)
			beforeConflicts := counterValue(t, metricStatusUpdateConflicts)
			beforeUnknown := counterValue(t, metricStatusUpdateUnknown)
			api.hooks.updateStatus = func(*ebsv1.Build) (*ebsv1.Build, error) {
				return nil, &clientpkg.WriteError{Operation: "update-status", Outcome: clientpkg.WriteRejected, StatusCode: tc.statusCode, Err: errors.New("rejected")}
			}
			result, err := c.sync(context.Background(), "project-a/build-a")
			switch {
			case tc.wantPermanent:
				if !controller.IsPermanent(err) {
					t.Fatalf("want permanent error, got %v", err)
				}
			case tc.wantError:
				if err == nil || controller.IsPermanent(err) {
					t.Fatalf("want transient error, got %v", err)
				}
			case tc.statusCode == 404:
				if err != nil {
					t.Fatalf("want nil error, got %v", err)
				}
			}
			if result.RequeueAfter != tc.wantRequeue {
				t.Fatalf("result=%+v, want requeue after %s", result, tc.wantRequeue)
			}
			if err != nil && result != (controller.ReconcileResult{}) {
				t.Fatalf("an error must not carry a requeue result: %+v", result)
			}
			if api.CallCount("GetBuild") != 2 {
				t.Fatalf("rejected writes must not run a confirmation read: %d reads", api.CallCount("GetBuild"))
			}
			assertCounterDelta(t, metricStatusUpdateConflicts, beforeConflicts, tc.wantConflicts)
			assertCounterDelta(t, metricStatusUpdateUnknown, beforeUnknown, 0)
		})
	}
}

func TestStatusWriteNotSentClassification(t *testing.T) {
	t.Run("client validation error", func(t *testing.T) {
		api, c := pendingWriteRound(t)
		api.hooks.updateStatus = func(*ebsv1.Build) (*ebsv1.Build, error) {
			return nil, &clientpkg.WriteError{Operation: "update-status", Outcome: clientpkg.WriteNotSent, Err: errors.New("object metadata does not match target")}
		}
		result, err := c.sync(context.Background(), "project-a/build-a")
		if !controller.IsPermanent(err) {
			t.Fatalf("want permanent error, got %v", err)
		}
		if result != (controller.ReconcileResult{}) || api.CallCount("GetBuild") != 2 {
			t.Fatalf("result=%+v reads=%d", result, api.CallCount("GetBuild"))
		}
	})

	t.Run("network failure", func(t *testing.T) {
		api, c := pendingWriteRound(t)
		networkErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		api.hooks.updateStatus = func(*ebsv1.Build) (*ebsv1.Build, error) {
			return nil, &clientpkg.WriteError{Operation: "update-status", Outcome: clientpkg.WriteNotSent, Err: networkErr}
		}
		result, err := c.sync(context.Background(), "project-a/build-a")
		if !errors.Is(err, networkErr) || controller.IsPermanent(err) {
			t.Fatalf("want transient network error, got %v", err)
		}
		if result != (controller.ReconcileResult{}) {
			t.Fatalf("result=%+v", result)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		api, c := pendingWriteRound(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err := c.sync(ctx, "project-a/build-a")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
		if result != (controller.ReconcileResult{}) {
			t.Fatalf("result=%+v", result)
		}
		if api.CallCount("UpdateBuildStatus") != 0 {
			t.Fatal("a cancelled round must not write")
		}
	})
}

func TestApplySkipsWriteWhenStatusAlreadyPersisted(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
	c := newTestController(t, api, newTestClock())
	r := &reconciler{
		controller: c,
		ctx:        context.Background(),
		key:        "project-a/build-a",
		project:    "project-a",
		now:        testTime(10),
		current:    api.build("project-a", "build-a"),
	}
	target := r.current.DeepCopy()
	intent := writeIntent{uid: target.UID, phase: target.Status.Phase, stage: target.Status.Stage}
	result, err := r.apply(target, intent, controller.ReconcileResult{}, nil)
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if api.CallCount("GetBuild") != 0 || api.CallCount("UpdateBuildStatus") != 0 {
		t.Fatal("an unchanged status must not trigger a read or a write")
	}
}

func TestStatusWriteKeepsFieldsOutsideTheIntent(t *testing.T) {
	api := newFakeAPI()
	build := withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
	build.Status.StartTime = testTime(9)
	build.Status.Repo = "https://existing.example.com"
	build.Status.Conditions = []metav1.Condition{{
		Type: "Unrelated", Status: metav1.ConditionTrue, Reason: "Kept", Message: "Kept", LastTransitionTime: testTime(9),
	}}
	api.builds[key("project-a", "build-a")] = build
	api.storeSnapshot(&ebsv1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a"},
		Status:     ebsv1.SnapshotStatus{Phase: ebsv1.SnapshotActive},
	})
	c := newTestController(t, api, newTestClock())
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	stored := api.build("project-a", "build-a")
	if stored.Status.Phase != ebsv1.BuildPrepared {
		t.Fatalf("phase = %q", stored.Status.Phase)
	}
	if !stored.Status.StartTime.Time.Equal(testTime(9).Time) || stored.Status.Repo != "https://existing.example.com" {
		t.Fatalf("unrelated status fields changed: %+v", stored.Status)
	}
	if _, ok := testCondition(stored.Status.Conditions, "Unrelated"); !ok {
		t.Fatalf("unrelated conditions were dropped: %+v", stored.Status.Conditions)
	}
}

// TestClassifyReadError pins the read side of the design's queue result contract: API permanent rejections,
// bad input and response contract violations are permanent; everything else keeps the retry backoff.
func TestClassifyReadError(t *testing.T) {
	resource := schema.GroupResource{Group: "ebs", Resource: "builds"}
	kind := schema.GroupKind{Group: "ebs", Kind: "Build"}
	for _, tc := range []struct {
		name          string
		err           error
		wantPermanent bool
	}{
		{name: "unauthorized", err: apierrors.NewUnauthorized("no token"), wantPermanent: true},
		{name: "forbidden", err: apierrors.NewForbidden(resource, "build-a", errors.New("denied")), wantPermanent: true},
		{name: "bad request", err: apierrors.NewBadRequest("unsupported field selector"), wantPermanent: true},
		{name: "invalid", err: apierrors.NewInvalid(kind, "build-a", field.ErrorList{}), wantPermanent: true},
		{name: "response contract violation", err: contractErrorf("unexpected Snapshot response for %s/%s", "project-a", "build-a"), wantPermanent: true},
		{name: "wrapped contract violation", err: fmt.Errorf("read Snapshot: %w", contractErrorf("unexpected Snapshot response")), wantPermanent: true},
		{name: "not found", err: apierrors.NewNotFound(resource, "build-a")},
		{name: "server error", err: apierrors.NewInternalError(errors.New("boom"))},
		{name: "unavailable", err: apierrors.NewServiceUnavailable("down")},
		{name: "too many requests", err: apierrors.NewTooManyRequests("slow down", 1)},
		{name: "network error", err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}},
		{name: "plain error", err: errors.New("connection reset")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			classified := classifyReadError(tc.err)
			if got := controller.IsPermanent(classified); got != tc.wantPermanent {
				t.Fatalf("permanent = %v, want %v (err=%v)", got, tc.wantPermanent, classified)
			}
			if !tc.wantPermanent && !errors.Is(classified, tc.err) {
				t.Fatalf("a retryable error must be returned unchanged, got %v", classified)
			}
		})
	}
}
