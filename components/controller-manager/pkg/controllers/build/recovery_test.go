package build

import (
	"context"
	"os"
	"strings"
	"testing"

	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func activeSnapshot(project, name string) *ebsv1.Snapshot {
	return &ebsv1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: project},
		Status:     ebsv1.SnapshotStatus{Phase: ebsv1.SnapshotActive},
	}
}

func TestCrashRecoveryReusesChildrenCreatedBeforeTheCrash(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withPhaseStage(withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"})), ebsv1.BuildPrepared, "")
	api.storeSnapshot(activeSnapshot("project-a", "build-a"))
	api.storeBuildInfo(newBuildInfoObject("project-a", "build-a", ebsv1.BuildInfoPending, nil))
	c := newTestController(t, api, newTestClock())
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if api.CallCount("CreateSnapshot") != 0 || api.CallCount("CreateBuildInfo") != 0 {
		t.Fatal("children created before the crash must be reused, not recreated")
	}
	if stored := api.build("project-a", "build-a"); stored.Status.Phase != ebsv1.BuildProcessing || stored.Status.Stage != stageBuild {
		t.Fatalf("phase/stage = %q/%q", stored.Status.Phase, stored.Status.Stage)
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
	if stored.Status.Phase != ebsv1.BuildProcessing || stored.Status.Stage != stageBuild {
		t.Fatalf("phase/stage = %q/%q", stored.Status.Phase, stored.Status.Stage)
	}
	if api.CallCount("CreateSnapshot") != 0 {
		t.Fatal("a restart must not recreate the Snapshot")
	}
}

func TestCrashBeforeTerminalWriteConvergesWithoutExtraWrites(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", "single", []string{"gcc"}), ebsv1.BuildProcessing, stageBuild)
	api.buildInfos[key("project-a", "build-a")] = newBuildInfoObject("project-a", "build-a", ebsv1.BuildInfoCompleted, map[string]ebsv1.SpecStatus{"gcc": specStatus("Failed", specStatusSucceeded)})
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

func TestBusinessCodeNeverReadsTheLocalClock(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		content, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, forbidden := range []string{"time.Now()", "time.Since("} {
			if strings.Contains(string(content), forbidden) {
				t.Errorf("%s must use the injected clock instead of %s", name, forbidden)
			}
		}
	}
}
