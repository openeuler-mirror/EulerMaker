package build

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
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
	build := withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildProcessing, ebsv1.BuildStageBuild)
	build.Status.StartTime = testTime(9)
	api.builds[key("project-a", "build-a")] = build
	api.buildInfos[key("project-a", "build-a")] = newBuildInfoObject("project-a", "build-a", ebsv1.BuildInfoCompleted,
		map[string]ebsv1.SpecResult{"gcc": {Status: ebsv1.SpecSucceeded}})
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

// newHistoryBuild is the last successfully published Build the baseBuildRef cases resolve against.
func newHistoryBuild(project, name string) *ebsv1.Build {
	return withPhaseStage(newBuild(project, name, "full", []string{"gcc"}), ebsv1.BuildSuccess, ebsv1.BuildStagePublish)
}

// baseBuildRefWriteRound isolates base reference resolution; nil history represents no successfully published Build.
func baseBuildRefWriteRound(t *testing.T, history *ebsv1.Build) (*fakeAPI, *Controller) {
	t.Helper()
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = newBuild("project-a", "build-a", "full", []string{"gcc"})
	if history != nil {
		api.hooks.lastPublished = func(string, string) (*ebsv1.Build, error) {
			return history, nil
		}
	}
	return api, newTestController(t, api, newTestClock())
}

func unknownStatusWrite() error {
	return &clientpkg.WriteError{Operation: "update-status", Outcome: clientpkg.WriteUnknown, Err: errors.New("lost response")}
}

// assertBaseBuildRefRound verifies one write and confirmation, with no child creation in the same round.
func assertBaseBuildRefRound(t *testing.T, api *fakeAPI, unknownBefore, wantUnknown uint64) {
	t.Helper()
	if got := api.CallCount("UpdateBuildStatus"); got != 1 {
		t.Fatalf("status writes = %d, want 1", got)
	}
	if got := api.CallCount("GetLastPublishedBuild"); got != 1 {
		t.Fatalf("history lookups = %d, want 1", got)
	}
	if got := api.CallCount("GetBuild"); got != 3 {
		t.Fatalf("Build reads = %d, want 3 (entry, pre-write, confirmation)", got)
	}
	for _, prefix := range []string{"CreateSnapshot", "CreateRpmRepo", "CreateBuildInfo"} {
		if got := api.CallCount(prefix); got != 0 {
			t.Fatalf("%s calls = %d, want 0", prefix, got)
		}
	}
	assertCounterDelta(t, metricStatusUpdateUnknown, unknownBefore, wantUnknown)
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
		if stored.Status.Phase != ebsv1.BuildProcessing || stored.Status.Stage != ebsv1.BuildStagePublish {
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

func TestBaseBuildRefWriteUnknownConfirmation(t *testing.T) {
	t.Run("history write confirmed by the original intent", func(t *testing.T) {
		history := newHistoryBuild("project-a", "build-history")
		api, c := baseBuildRefWriteRound(t, history)
		before := counterValue(t, metricStatusUpdateUnknown)
		api.hooks.updateStatus = func(request *ebsv1.Build) (*ebsv1.Build, error) {
			if _, err := api.commitStatus(request); err != nil {
				t.Fatalf("commit: %v", err)
			}
			return nil, unknownStatusWrite()
		}

		result, err := c.sync(context.Background(), "project-a/build-a")
		if err != nil || result != (controller.ReconcileResult{}) {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		stored := api.build("project-a", "build-a")
		if stored.Status.BaseBuildRef == nil ||
			stored.Status.BaseBuildRef.Name != history.Name {
			t.Fatalf("baseBuildRef = %+v", stored.Status.BaseBuildRef)
		}
		if stored.Status.Phase != ebsv1.BuildPending {
			t.Fatalf("phase = %q", stored.Status.Phase)
		}
		assertBaseBuildRefRound(t, api, before, 1)
	})

	t.Run("empty history confirmed as an empty object", func(t *testing.T) {
		api, c := baseBuildRefWriteRound(t, nil)
		before := counterValue(t, metricStatusUpdateUnknown)
		api.hooks.updateStatus = func(request *ebsv1.Build) (*ebsv1.Build, error) {
			if _, err := api.commitStatus(request); err != nil {
				t.Fatalf("commit: %v", err)
			}
			return nil, unknownStatusWrite()
		}

		result, err := c.sync(context.Background(), "project-a/build-a")
		if err != nil || result != (controller.ReconcileResult{}) {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		stored := api.build("project-a", "build-a")
		if stored.Status.BaseBuildRef == nil {
			t.Fatal("an empty resolution must be persisted as an empty object, not nil")
		}
		if stored.Status.BaseBuildRef.Name != "" {
			t.Fatalf("baseBuildRef = %+v", stored.Status.BaseBuildRef)
		}
		assertBaseBuildRefRound(t, api, before, 1)
	})

	t.Run("empty intent is not satisfied by a nil reference", func(t *testing.T) {
		api, c := baseBuildRefWriteRound(t, nil)
		before := counterValue(t, metricStatusUpdateUnknown)
		api.hooks.updateStatus = func(*ebsv1.Build) (*ebsv1.Build, error) {
			// The write never reached the server: the persisted object keeps baseBuildRef=nil while the intent
			// wanted the resolved empty object, so the confirmation must not accept it.
			return nil, unknownStatusWrite()
		}

		result, err := c.sync(context.Background(), "project-a/build-a")
		if err != nil {
			t.Fatalf("sync: %v", err)
		}
		if result.RequeueAfter != conflictRequeueDelay {
			t.Fatalf("result=%+v, want requeue after %s", result, conflictRequeueDelay)
		}
		if stored := api.build("project-a", "build-a"); stored.Status.BaseBuildRef != nil {
			t.Fatalf("baseBuildRef = %+v, want nil", stored.Status.BaseBuildRef)
		}
		assertBaseBuildRefRound(t, api, before, 1)
	})

	t.Run("history intent is not satisfied by an empty object", func(t *testing.T) {
		history := newHistoryBuild("project-a", "build-history")
		api, c := baseBuildRefWriteRound(t, history)
		before := counterValue(t, metricStatusUpdateUnknown)
		api.hooks.updateStatus = func(*ebsv1.Build) (*ebsv1.Build, error) {
			// Another writer resolved the field as an empty object while our write outcome stayed unknown.
			latest := api.build("project-a", "build-a")
			latest.Status.BaseBuildRef = &ebsv1.BaseBuildRef{}
			api.storeBuild(latest)
			return nil, unknownStatusWrite()
		}

		result, err := c.sync(context.Background(), "project-a/build-a")
		if err != nil {
			t.Fatalf("sync: %v", err)
		}
		if result.RequeueAfter != conflictRequeueDelay {
			t.Fatalf("result=%+v, want requeue after %s", result, conflictRequeueDelay)
		}
		stored := api.build("project-a", "build-a")
		if stored.Status.BaseBuildRef == nil || stored.Status.BaseBuildRef.Name != "" {
			t.Fatalf("baseBuildRef = %+v, want the externally written empty object", stored.Status.BaseBuildRef)
		}
		assertBaseBuildRefRound(t, api, before, 1)
	})

	t.Run("concurrently aborted ends the old cycle", func(t *testing.T) {
		history := newHistoryBuild("project-a", "build-history")
		api, c := baseBuildRefWriteRound(t, history)
		before := counterValue(t, metricStatusUpdateUnknown)
		api.hooks.updateStatus = func(*ebsv1.Build) (*ebsv1.Build, error) {
			aborted := api.build("project-a", "build-a")
			aborted.Status.Phase = ebsv1.BuildAborted
			api.storeBuild(aborted)
			return nil, unknownStatusWrite()
		}

		result, err := c.sync(context.Background(), "project-a/build-a")
		if err != nil || result != (controller.ReconcileResult{}) {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		stored := api.build("project-a", "build-a")
		if stored.Status.Phase != ebsv1.BuildAborted {
			t.Fatalf("phase = %q, want %q", stored.Status.Phase, ebsv1.BuildAborted)
		}
		if stored.Status.BaseBuildRef != nil {
			t.Fatalf("baseBuildRef = %+v, want nil", stored.Status.BaseBuildRef)
		}
		assertBaseBuildRefRound(t, api, before, 1)
	})

	t.Run("concurrently deleted ends the old cycle", func(t *testing.T) {
		history := newHistoryBuild("project-a", "build-history")
		api, c := baseBuildRefWriteRound(t, history)
		before := counterValue(t, metricStatusUpdateUnknown)
		api.hooks.updateStatus = func(*ebsv1.Build) (*ebsv1.Build, error) {
			api.deleteBuild("project-a", "build-a")
			return nil, unknownStatusWrite()
		}

		result, err := c.sync(context.Background(), "project-a/build-a")
		if err != nil || result != (controller.ReconcileResult{}) {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if api.build("project-a", "build-a") != nil {
			t.Fatal("the deleted Build must not be recreated or updated")
		}
		assertBaseBuildRefRound(t, api, before, 1)
	})
}

func TestIntentSatisfiedComparesOnlyTargetFields(t *testing.T) {
	emptyRef := &ebsv1.BaseBuildRef{}
	historyRef := &ebsv1.BaseBuildRef{Name: "build-history"}
	sameHistoryRef := &ebsv1.BaseBuildRef{Name: "build-history"}
	otherHistoryRef := &ebsv1.BaseBuildRef{Name: "build-other"}

	pendingBuild := func() *ebsv1.Build {
		return withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildPending, "")
	}
	withRef := func(build *ebsv1.Build, ref *ebsv1.BaseBuildRef) *ebsv1.Build {
		build.Status.BaseBuildRef = ref
		return build
	}

	buildSucceedIntent := func(status metav1.ConditionStatus, reason string, generation int64) conditionIntent {
		return conditionIntent{
			condType: ConditionBuildSucceed, status: status,
			reason: reason, message: reason, observedGeneration: generation,
		}
	}

	for _, tc := range []struct {
		name   string
		intent writeIntent
		build  *ebsv1.Build
		want   bool
	}{
		{
			name:   "nil reference does not satisfy an empty object intent",
			intent: writeIntent{uid: "uid", phase: ebsv1.BuildPending, setBaseBuildRef: true, baseBuildRef: emptyRef},
			build:  pendingBuild(),
			want:   false,
		},
		{
			name:   "empty object satisfies an empty object intent",
			intent: writeIntent{uid: "uid", phase: ebsv1.BuildPending, setBaseBuildRef: true, baseBuildRef: emptyRef},
			build:  withRef(pendingBuild(), &ebsv1.BaseBuildRef{}),
			want:   true,
		},
		{
			name:   "equal content satisfies the intent regardless of pointer identity",
			intent: writeIntent{uid: "uid", phase: ebsv1.BuildPending, setBaseBuildRef: true, baseBuildRef: historyRef},
			build:  withRef(pendingBuild(), sameHistoryRef),
			want:   true,
		},
		{
			name:   "different history does not satisfy the intent",
			intent: writeIntent{uid: "uid", phase: ebsv1.BuildPending, setBaseBuildRef: true, baseBuildRef: historyRef},
			build:  withRef(pendingBuild(), otherHistoryRef),
			want:   false,
		},
		{
			name:   "an intent without setBaseBuildRef ignores the reference",
			intent: writeIntent{uid: "uid", phase: ebsv1.BuildPending},
			build:  withRef(pendingBuild(), historyRef),
			want:   true,
		},
		{
			name:   "phase mismatch is not satisfied",
			intent: writeIntent{uid: "uid", phase: ebsv1.BuildPending, setBaseBuildRef: true, baseBuildRef: emptyRef},
			build:  withRef(withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildPrepared, ""), emptyRef),
			want:   false,
		},
		{
			name:   "stage mismatch is not satisfied",
			intent: writeIntent{uid: "uid", phase: ebsv1.BuildProcessing, stage: ebsv1.BuildStageBuild},
			build:  withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildProcessing, ebsv1.BuildStagePublish),
			want:   false,
		},
		{
			name:   "resourceVersion is not part of the comparison",
			intent: writeIntent{uid: "uid", phase: ebsv1.BuildPending, setBaseBuildRef: true, baseBuildRef: emptyRef},
			build: func() *ebsv1.Build {
				build := withRef(pendingBuild(), emptyRef)
				build.ResourceVersion = "999"
				return build
			}(),
			want: true,
		},
		{
			name:   "unrelated status fields are not part of the comparison",
			intent: writeIntent{uid: "uid", phase: ebsv1.BuildPending, setBaseBuildRef: true, baseBuildRef: emptyRef},
			build: func() *ebsv1.Build {
				build := withRef(pendingBuild(), emptyRef)
				build.Status.StartTime = testTime(9)
				return build
			}(),
			want: true,
		},
		{
			name:   "unrelated conditions do not affect the comparison",
			intent: writeIntent{uid: "uid", phase: ebsv1.BuildPending, setBaseBuildRef: true, baseBuildRef: emptyRef},
			build: func() *ebsv1.Build {
				build := withRef(pendingBuild(), emptyRef)
				build.Status.Conditions = []metav1.Condition{{
					Type: "Unrelated", Status: metav1.ConditionTrue, Reason: "Kept", Message: "Kept",
				}}
				return build
			}(),
			want: true,
		},
		{
			name: "conditions are matched by type and ignore order",
			intent: writeIntent{
				uid: "uid", phase: ebsv1.BuildPending, setBaseBuildRef: true, baseBuildRef: emptyRef,
				setConditions: true,
				conditionIntents: []conditionIntent{
					buildSucceedIntent(metav1.ConditionTrue, ReasonBuildSucceeded, 3),
					{condType: ConditionPublishSucceed, status: metav1.ConditionTrue, reason: ReasonPublishSucceeded, message: ReasonPublishSucceeded, observedGeneration: 3},
				},
			},
			build: func() *ebsv1.Build {
				build := withRef(pendingBuild(), emptyRef)
				build.Generation = 3
				build.Status.Conditions = []metav1.Condition{
					{Type: ConditionPublishSucceed, Status: metav1.ConditionTrue, Reason: ReasonPublishSucceeded, Message: ReasonPublishSucceeded, ObservedGeneration: 3},
					{Type: "Unrelated", Status: metav1.ConditionFalse, Reason: "Kept", Message: "Kept"},
					{Type: ConditionBuildSucceed, Status: metav1.ConditionTrue, Reason: ReasonBuildSucceeded, Message: ReasonBuildSucceeded, ObservedGeneration: 3},
				}
				return build
			}(),
			want: true,
		},
		{
			name: "a condition status mismatch is not satisfied",
			intent: writeIntent{
				uid: "uid", phase: ebsv1.BuildPending, setBaseBuildRef: true, baseBuildRef: emptyRef,
				setConditions: true,
				conditionIntents: []conditionIntent{
					buildSucceedIntent(metav1.ConditionTrue, ReasonBuildSucceeded, 3),
				},
			},
			build: func() *ebsv1.Build {
				build := withRef(pendingBuild(), emptyRef)
				build.Generation = 3
				build.Status.Conditions = []metav1.Condition{{
					Type: ConditionBuildSucceed, Status: metav1.ConditionFalse,
					Reason: ReasonBuildFailed, Message: ReasonBuildFailed, ObservedGeneration: 3,
				}}
				return build
			}(),
			want: false,
		},
		{
			name: "a condition observedGeneration mismatch is not satisfied",
			intent: writeIntent{
				uid: "uid", phase: ebsv1.BuildPending, setBaseBuildRef: true, baseBuildRef: emptyRef,
				setConditions: true,
				conditionIntents: []conditionIntent{
					buildSucceedIntent(metav1.ConditionTrue, ReasonBuildSucceeded, 4),
				},
			},
			build: func() *ebsv1.Build {
				build := withRef(pendingBuild(), emptyRef)
				build.Generation = 4
				build.Status.Conditions = []metav1.Condition{{
					Type: ConditionBuildSucceed, Status: metav1.ConditionTrue,
					Reason: ReasonBuildSucceeded, Message: ReasonBuildSucceeded, ObservedGeneration: 3,
				}}
				return build
			}(),
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := intentSatisfied(tc.build, tc.intent); got != tc.want {
				t.Fatalf("intentSatisfied = %v, want %v (intent=%+v status=%+v)", got, tc.want, tc.intent, tc.build.Status)
			}
		})
	}
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
	if !stored.Status.StartTime.Time.Equal(testTime(9).Time) {
		t.Fatalf("unrelated status fields changed: %+v", stored.Status)
	}
	if _, ok := testCondition(stored.Status.Conditions, "Unrelated"); !ok {
		t.Fatalf("unrelated conditions were dropped: %+v", stored.Status.Conditions)
	}
}

// API rejections and contract violations are permanent; transient failures remain retryable.
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

// TestRetryableCreateRejection pins the create rejection matrix: 404 is retryable because it means the target
// route or its parent object is not available yet, while 400/401/403/422 stay permanent API rejections.
func TestRetryableCreateRejection(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statusCode int
		want       bool
	}{
		{name: "not found", statusCode: http.StatusNotFound, want: true},
		{name: "request timeout", statusCode: http.StatusRequestTimeout, want: true},
		{name: "conflict", statusCode: http.StatusConflict, want: true},
		{name: "precondition failed", statusCode: http.StatusPreconditionFailed, want: true},
		{name: "too many requests", statusCode: http.StatusTooManyRequests, want: true},
		{name: "server error", statusCode: http.StatusInternalServerError, want: true},
		{name: "unavailable", statusCode: http.StatusServiceUnavailable, want: true},
		{name: "bad request", statusCode: http.StatusBadRequest},
		{name: "unauthorized", statusCode: http.StatusUnauthorized},
		{name: "forbidden", statusCode: http.StatusForbidden},
		{name: "invalid", statusCode: http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryableCreateRejection(tc.statusCode); got != tc.want {
				t.Fatalf("retryableCreateRejection(%d) = %v, want %v", tc.statusCode, got, tc.want)
			}

			writeErr := &clientpkg.WriteError{
				Operation: "create", Outcome: clientpkg.WriteRejected,
				StatusCode: tc.statusCode, Err: errors.New("create rejected"),
			}
			if tc.statusCode == http.StatusTooManyRequests || tc.statusCode == http.StatusServiceUnavailable {
				writeErr.RetryAfter = 7 * time.Second
			}

			classified := classifyRejectedWrite(writeErr, writeErr)
			if !tc.want {
				if !controller.IsPermanent(classified) {
					t.Fatalf("a permanent rejection must stay permanent: %v", classified)
				}
				return
			}
			if !errors.Is(classified, writeErr) {
				t.Fatalf("a retryable rejection must be returned unchanged, got %v", classified)
			}
			if controller.IsPermanent(classified) {
				t.Fatalf("a retryable rejection must not be permanent: %v", classified)
			}
			if writeErr.RetryAfter > 0 && clientpkg.RetryAfter(classified) != writeErr.RetryAfter {
				t.Fatalf("RetryAfter = %s, want %s", clientpkg.RetryAfter(classified), writeErr.RetryAfter)
			}
		})
	}
}
