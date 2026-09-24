package build

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
)

// newHistoryBuild is the last successfully published Build the baseBuildRef cases resolve against.
func newHistoryBuild(project, name string) *ebsv1.Build {
	history := withPhaseStage(newBuild(project, name, "full", []string{"gcc"}), ebsv1.BuildSuccess, stagePublish)
	return history
}

// baseBuildRefWriteRound builds a round whose only job is to resolve Build.status.baseBuildRef, so the unknown
// write confirmation can be exercised against an intent that carries setBaseBuildRef. A nil history models a
// project without any successful published Build.
func baseBuildRefWriteRound(t *testing.T, history *ebsv1.Build) (*fakeAPI, *Controller) {
	t.Helper()
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = newBuild("project-a", "build-a", "full", []string{"gcc"})
	if history != nil {
		api.hooks.lastPublished = func(string, string, string) (*ebsv1.Build, error) {
			return history, nil
		}
	}
	return api, newTestController(t, api, newTestClock())
}

func unknownStatusWrite() error {
	return &clientpkg.WriteError{Operation: "update-status", Outcome: clientpkg.WriteUnknown, Err: errors.New("lost response")}
}

// assertBaseBuildRefRound checks the round ended right after the baseBuildRef write: exactly one status write,
// exactly one history lookup, exactly one unknown confirmation and no sub resource creation in the same round.
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
			intent: writeIntent{uid: "uid", phase: ebsv1.BuildProcessing, stage: stageBuild},
			build:  withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildProcessing, stagePublish),
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
