package build

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
)

func TestSyncIgnoresMissingAndFinishedBuilds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build *ebsv1.Build
	}{
		{
			name: "missing build",
		},
		{
			name:  "success",
			build: withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildSuccess, ebsv1.BuildStagePublish),
		},
		{
			name:  "single success",
			build: withPhaseStage(newBuild("project-a", "build-a", "single", []string{"gcc"}), ebsv1.BuildSuccess, ebsv1.BuildStageBuild),
		},
		{
			name:  "failed",
			build: withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildFailed, ebsv1.BuildStageBuild),
		},
		{
			name:  "unpublished success",
			build: withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildSuccess, ebsv1.BuildStageBuild),
		},
		{
			name:  "aborted",
			build: withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildAborted, ""),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI()
			if tc.build != nil {
				api.builds[key("project-a", "build-a")] = tc.build
			}
			c := newTestController(t, api, newTestClock())
			result, err := c.sync(context.Background(), "project-a/build-a")
			if err != nil || result != (controller.ReconcileResult{}) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if api.CallCount("UpdateBuildStatus") != 0 {
				t.Fatal("terminal or missing Build must not be written")
			}
			for _, method := range []string{"GetSnapshot", "GetBuildInfo", "GetRpmRepo"} {
				if api.CallCount(method) != 0 {
					t.Fatalf("terminal or missing Build must not call %s", method)
				}
			}
		})
	}
}

func TestSyncLeavesOtherFinalizersAlone(t *testing.T) {
	build := withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildPending, "")
	deletion := metav1.NewTime(time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC))
	build.DeletionTimestamp = &deletion
	build.Finalizers = []string{"another/controller"}
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = build
	c := newTestController(t, api, newTestClock())
	result, err := c.sync(context.Background(), "project-a/build-a")
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if api.CallCount("UpdateBuildStatus") != 0 || api.CallCount("CreateSnapshot") != 0 {
		t.Fatal("deleting Build must not be advanced or recreated")
	}
}

func TestSyncRejectsInvalidPhaseStageCombinations(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build *ebsv1.Build
	}{
		{
			name:  "pending publish",
			build: withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildPending, ebsv1.BuildStagePublish),
		},
		{
			name:  "prepared build",
			build: withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildPrepared, ebsv1.BuildStageBuild),
		},
		{
			name:  "processing without stage",
			build: withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildProcessing, ""),
		},
		{
			name:  "single publish",
			build: withPhaseStage(newBuild("project-a", "build-a", "single", []string{"gcc"}), ebsv1.BuildProcessing, ebsv1.BuildStagePublish),
		},
		{
			name:  "unknown phase",
			build: withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildPhase("Bogus"), ""),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI()
			api.builds[key("project-a", "build-a")] = tc.build
			c := newTestController(t, api, newTestClock())
			result, err := c.sync(context.Background(), "project-a/build-a")
			if !controller.IsPermanent(err) {
				t.Fatalf("want permanent error, got %v", err)
			}
			if result != (controller.ReconcileResult{}) {
				t.Fatalf("result=%+v", result)
			}
			if api.CallCount("UpdateBuildStatus") != 0 {
				t.Fatal("invalid phase/stage must not write status")
			}
			stored := api.build("project-a", "build-a")
			if stored.Status.Phase != tc.build.Status.Phase || stored.Status.Stage != tc.build.Status.Stage {
				t.Fatalf("phase/stage changed to %q/%q", stored.Status.Phase, stored.Status.Stage)
			}
		})
	}
}

// TestSyncTreatsNonRetryableReadErrorsAsPermanent pins that read paths map contract violations and API
// permanent rejections to a permanent error instead of the retry backoff.
func TestSyncTreatsNonRetryableReadErrorsAsPermanent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build *ebsv1.Build
		setup func(*fakeAPI)
	}{
		{
			name:  "response contract violation at the entry read",
			build: newBuild("project-a", "build-a", "full", []string{"gcc"}),
			setup: func(api *fakeAPI) {
				api.hooks.getBuild = func(project, name string) (*ebsv1.Build, error) {
					return nil, contractErrorf("unexpected Build response for %s/%s", project, name)
				}
			},
		},
		{
			name:  "bad request while waiting for the release",
			build: withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildProcessing, ebsv1.BuildStagePublish),
			setup: func(api *fakeAPI) {
				api.hooks.getRpmRepo = func(string, string) (*ebsv1.RpmRepo, error) {
					return nil, apierrors.NewBadRequest("unsupported field selector")
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI()
			api.builds[key("project-a", "build-a")] = tc.build
			tc.setup(api)
			c := newTestController(t, api, newTestClock())
			result, err := c.sync(context.Background(), "project-a/build-a")
			if !controller.IsPermanent(err) {
				t.Fatalf("want a permanent error, got %v", err)
			}
			if result != (controller.ReconcileResult{}) || api.CallCount("UpdateBuildStatus") != 0 {
				t.Fatalf("result=%+v writes=%d", result, api.CallCount("UpdateBuildStatus"))
			}
		})
	}
}

func TestCrashRecoveryReplaysFromThePersistedPhase(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
	api.storeSnapshot(activeSnapshot("project-a", "build-a"))
	api.projects["project-a"] = newProject("project-a", newPackageRepo("gcc"))
	clk := newTestClock()
	first := newTestController(t, api, clk)
	if _, err := first.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatalf("first round: %v", err)
	}
	if stored := api.build("project-a", "build-a"); stored.Status.Phase != ebsv1.BuildPrepared {
		t.Fatalf("phase = %q", stored.Status.Phase)
	}

	// A restarted process holds no in-memory state: the persisted phase drives the next round.
	restarted := newTestController(t, api, clk)
	if _, err := restarted.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatalf("second round: %v", err)
	}
	stored := api.build("project-a", "build-a")
	if stored.Status.Phase != ebsv1.BuildProcessing || stored.Status.Stage != ebsv1.BuildStageBuild {
		t.Fatalf("phase/stage = %q/%q", stored.Status.Phase, stored.Status.Stage)
	}
	if api.CallCount("CreateSnapshot") != 0 {
		t.Fatal("a restart must not recreate the Snapshot")
	}
}

func TestTerminalBuildReconcileIsIdempotent(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", "single", []string{"gcc"}), ebsv1.BuildProcessing, ebsv1.BuildStageBuild)
	api.buildInfos[key("project-a", "build-a")] = newBuildInfoObject("project-a", "build-a", ebsv1.BuildInfoCompleted, map[string]ebsv1.SpecResult{"gcc": {Status: ebsv1.SpecBuildFailed}})
	c := newTestController(t, api, newTestClock())
	if _, err := c.sync(context.Background(), "project-a/build-a"); !controller.IsPermanent(err) {
		t.Fatalf("want permanent error, got %v", err)
	}
	first := api.build("project-a", "build-a")
	if first.Status.Phase != ebsv1.BuildFailed {
		t.Fatalf("phase = %q", first.Status.Phase)
	}
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatalf("second round: %v", err)
	}
	if api.CallCount("UpdateBuildStatus") != 1 {
		t.Fatalf("terminal Build must not be rewritten: %d writes", api.CallCount("UpdateBuildStatus"))
	}
	if second := api.build("project-a", "build-a"); second.ResourceVersion != first.ResourceVersion {
		t.Fatal("terminal Build was updated again")
	}
}

func TestSplitKeyRejectsIncompleteKeys(t *testing.T) {
	for _, key := range []string{"", "build-a", "project-a/", "/build-a", "project-a/build-a/extra"} {
		if _, _, ok := splitKey(key); ok {
			t.Fatalf("key %q must be rejected", key)
		}
	}
	project, name, ok := splitKey("project-a/build-a")
	if !ok || project != "project-a" || name != "build-a" {
		t.Fatalf("project=%q name=%q ok=%v", project, name, ok)
	}
}
