package build

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
)

func activeSnapshot(project, name string) *ebsv1.Snapshot {
	return &ebsv1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: project},
		Status:     ebsv1.SnapshotStatus{Phase: ebsv1.SnapshotActive},
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
