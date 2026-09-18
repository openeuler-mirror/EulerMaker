package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"
	"testing"
	"time"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func repoNames(repos []ebsv1.PackageRepo) []string {
	names := make([]string, 0, len(repos))
	for _, repo := range repos {
		names = append(names, repo.Name)
	}
	return names
}

func TestSyncIgnoresMissingAndFinishedBuilds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build *ebsv1.Build
	}{
		{name: "missing build"},
		{name: "success", build: withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildSuccess, stagePublish)},
		{name: "failed", build: withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildFailed, stageBuild)},
		{name: "skipped", build: withPhaseStage(newBuild("project-a", "build-a", "single", []string{"gcc"}), ebsv1.BuildSkipped, stagePublish)},
		{name: "aborted", build: withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildAborted, "")},
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
		})
	}
}

func TestSyncStopsWhenBuildIsDeleting(t *testing.T) {
	build := withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildPending, "")
	deletion := metav1.NewTime(time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC))
	build.DeletionTimestamp = &deletion
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
		{name: "pending publish", build: withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildPending, stagePublish)},
		{name: "prepared build", build: withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildPrepared, stageBuild)},
		{name: "processing without stage", build: withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildProcessing, "")},
		{name: "single publish", build: withPhaseStage(newBuild("project-a", "build-a", "single", []string{"gcc"}), ebsv1.BuildProcessing, stagePublish)},
		{name: "unknown phase", build: withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildPhase("Bogus"), "")},
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
			build: withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildProcessing, stagePublish),
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

func TestPendingRecordsBaseBuildRefFromHistory(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = newBuild("project-a", "build-a", "full", []string{"gcc"})
	previous := withPhaseStage(newBuild("project-a", "build-prev", "full", []string{"gcc"}), ebsv1.BuildSuccess, stagePublish)
	previous.Status.Repo = "https://release.example.com/project-a/aarch64"
	api.hooks.lastPublished = func(project, targetOS, targetArch string) (*ebsv1.Build, error) {
		if project != "project-a" || targetOS != "openEuler-22.03-LTS" || targetArch != "aarch64" {
			t.Errorf("unexpected history query %q %q %q", project, targetOS, targetArch)
		}
		return previous, nil
	}
	c := newTestController(t, api, newTestClock())
	result, err := c.sync(context.Background(), "project-a/build-a")
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	stored := api.build("project-a", "build-a")
	if stored.Status.BaseBuildRef == nil || stored.Status.BaseBuildRef.Name != "build-prev" {
		t.Fatalf("baseBuildRef = %+v", stored.Status.BaseBuildRef)
	}
	if stored.Status.Phase != ebsv1.BuildPending || api.CallCount("UpdateBuildStatus") != 1 {
		t.Fatalf("phase=%q writes=%d", stored.Status.Phase, api.CallCount("UpdateBuildStatus"))
	}
	if api.CallCount("CreateSnapshot") != 0 || api.CallCount("CreateRpmRepo") != 0 {
		t.Fatal("the base reference write must end the round before any ensure")
	}
}

func TestPendingRecordsEmptyBaseBuildRefWhenNoHistoryExists(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = newBuild("project-a", "build-a", "full", []string{"gcc"})
	c := newTestController(t, api, newTestClock())
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	stored := api.build("project-a", "build-a")
	if stored.Status.BaseBuildRef == nil {
		t.Fatal("an empty resolution must be recorded as an empty object, not nil")
	}
	if stored.Status.BaseBuildRef.Name != "" {
		t.Fatalf("baseBuildRef = %+v", stored.Status.BaseBuildRef)
	}
}

func TestPendingCreatesSnapshotAndWaitsForActive(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
	api.projects["project-a"] = newProject("project-a", newPackageRepo("gcc"), newPackageRepo("glibc"))
	c := newTestController(t, api, newTestClock())
	result, err := c.sync(context.Background(), "project-a/build-a")
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	snapshot := api.snapshot("project-a", "build-a")
	if snapshot == nil {
		t.Fatal("Snapshot was not created")
	}
	if snapshot.Spec.DefaultRef != api.getProject("project-a").Spec.DefaultRef {
		t.Fatalf("defaultRef = %+v", snapshot.Spec.DefaultRef)
	}
	if !reflect.DeepEqual(repoNames(snapshot.Spec.PackageRepos), []string{"gcc", "glibc"}) {
		t.Fatalf("packageRepos = %v", repoNames(snapshot.Spec.PackageRepos))
	}
	if got := api.CallCount("GetProject project-a"); got != 1 {
		t.Fatalf("project reads = %d, want 1", got)
	}
	if api.CallCount("UpdateBuildStatus") != 0 {
		t.Fatal("a Pending Snapshot must not advance the Build")
	}
	if api.CallCount("CreateRpmRepo") != 0 || api.CallCount("GetRpmRepo") != 0 {
		t.Fatal("RpmRepo must wait for an active Snapshot")
	}
}

func TestPendingReusesExistingSnapshotWithoutReadingProject(t *testing.T) {
	build := withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = build
	api.storeSnapshot(&ebsv1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a"},
		Status:     ebsv1.SnapshotStatus{Phase: ebsv1.SnapshotPending},
	})
	c := newTestController(t, api, newTestClock())
	beforeTerminating := counterValue(t, metricEnsureTerminating)
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if api.CallCount("GetProject") != 0 || api.CallCount("CreateSnapshot") != 0 {
		t.Fatal("an existing Snapshot must not be filtered or recreated")
	}
	if api.CallCount("UpdateBuildStatus") != 0 {
		t.Fatal("Build must keep waiting")
	}
	assertCounterDelta(t, metricEnsureTerminating, beforeTerminating, 0)
}

func TestPendingWaitsForTerminatingSnapshot(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
	deletion := metav1.NewTime(time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC))
	api.storeSnapshot(&ebsv1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a", DeletionTimestamp: &deletion},
		Status:     ebsv1.SnapshotStatus{Phase: ebsv1.SnapshotActive},
	})
	beforeTerminating := counterValue(t, metricEnsureTerminating)
	c := newTestController(t, api, newTestClock())
	result, err := c.sync(context.Background(), "project-a/build-a")
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if api.CallCount("UpdateBuildStatus") != 0 {
		t.Fatal("a terminating Snapshot must not advance the Build")
	}
	assertCounterDelta(t, metricEnsureTerminating, beforeTerminating, 1)
}

func TestPendingProjectReadFailures(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		api := newFakeAPI()
		api.builds[key("project-a", "build-a")] = withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
		c := newTestController(t, api, newTestClock())
		_, err := c.sync(context.Background(), "project-a/build-a")
		if !controller.IsPermanent(err) {
			t.Fatalf("want permanent error, got %v", err)
		}
		if api.CallCount("CreateSnapshot") != 0 || api.CallCount("UpdateBuildStatus") != 0 {
			t.Fatal("a missing Project must not create resources or write status")
		}
	})

	t.Run("temporary", func(t *testing.T) {
		api := newFakeAPI()
		api.builds[key("project-a", "build-a")] = withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
		temporary := errors.New("connection reset")
		api.hooks.getProject = func(string) (*ebsv1.Project, error) { return nil, temporary }
		c := newTestController(t, api, newTestClock())
		result, err := c.sync(context.Background(), "project-a/build-a")
		if !errors.Is(err, temporary) || controller.IsPermanent(err) {
			t.Fatalf("want transient error, got %v", err)
		}
		if result != (controller.ReconcileResult{}) {
			t.Fatalf("result=%+v", result)
		}
		if api.CallCount("CreateSnapshot") != 0 || api.CallCount("UpdateBuildStatus") != 0 {
			t.Fatal("a transient read failure must not create resources or write status")
		}
	})
}

func TestPendingCreateSnapshotAlreadyExistsReusesObject(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
	api.projects["project-a"] = newProject("project-a", newPackageRepo("gcc"))
	beforeConflicts := counterValue(t, metricEnsureConflicts)
	api.hooks.createSnapshot = func(request *ebsv1.Snapshot) (*ebsv1.Snapshot, error) {
		api.storeSnapshot(request)
		return nil, &clientpkg.WriteError{Operation: "create", Outcome: clientpkg.WriteRejected, StatusCode: 409, Err: apierrors.NewAlreadyExists(snapshotsResource, request.Name)}
	}
	c := newTestController(t, api, newTestClock())
	result, err := c.sync(context.Background(), "project-a/build-a")
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if api.CallCount("CreateSnapshot") != 1 {
		t.Fatalf("create calls = %d", api.CallCount("CreateSnapshot"))
	}
	if api.CallCount("GetSnapshot") < 2 {
		t.Fatal("the existing Snapshot must be read back")
	}
	if got := api.CallCount("GetProject project-a"); got != 1 {
		t.Fatalf("project reads = %d, want 1", got)
	}
	reused := api.snapshot("project-a", "build-a")
	if reused == nil {
		t.Fatal("the reused Snapshot is missing")
	}
	project := api.getProject("project-a")
	if reused.Spec.DefaultRef != project.Spec.DefaultRef || !reflect.DeepEqual(repoNames(reused.Spec.PackageRepos), []string{"gcc"}) {
		t.Fatalf("the reused Snapshot must be built from the single Project read: %+v", reused.Spec)
	}
	if api.CallCount("UpdateBuildStatus") != 0 {
		t.Fatal("AlreadyExists must not be reported as a generic conflict requeue")
	}
	assertCounterDelta(t, metricEnsureConflicts, beforeConflicts, 1)
}

func TestPendingCreateSnapshotUnknownConfirmsExistence(t *testing.T) {
	t.Run("object exists", func(t *testing.T) {
		api := newFakeAPI()
		api.builds[key("project-a", "build-a")] = withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
		api.projects["project-a"] = newProject("project-a", newPackageRepo("gcc"))
		api.hooks.createSnapshot = func(request *ebsv1.Snapshot) (*ebsv1.Snapshot, error) {
			api.storeSnapshot(request)
			return nil, &clientpkg.WriteError{Operation: "create", Outcome: clientpkg.WriteUnknown, Err: errors.New("lost response")}
		}
		c := newTestController(t, api, newTestClock())
		result, err := c.sync(context.Background(), "project-a/build-a")
		if err != nil || result != (controller.ReconcileResult{}) {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if api.CallCount("CreateSnapshot") != 1 {
			t.Fatalf("create calls = %d", api.CallCount("CreateSnapshot"))
		}
		if got := api.CallCount("GetProject project-a"); got != 1 {
			t.Fatalf("project reads = %d, want 1", got)
		}
		confirmed := api.snapshot("project-a", "build-a")
		if confirmed == nil {
			t.Fatal("the confirmed Snapshot is missing")
		}
		project := api.getProject("project-a")
		if confirmed.Spec.DefaultRef != project.Spec.DefaultRef || !reflect.DeepEqual(repoNames(confirmed.Spec.PackageRepos), []string{"gcc"}) {
			t.Fatalf("the confirmed Snapshot must be built from the single Project read: %+v", confirmed.Spec)
		}
	})

	t.Run("object missing", func(t *testing.T) {
		api := newFakeAPI()
		api.builds[key("project-a", "build-a")] = withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
		api.projects["project-a"] = newProject("project-a", newPackageRepo("gcc"))
		var requested *ebsv1.Snapshot
		api.hooks.createSnapshot = func(request *ebsv1.Snapshot) (*ebsv1.Snapshot, error) {
			requested = request
			return nil, &clientpkg.WriteError{Operation: "create", Outcome: clientpkg.WriteUnknown, Err: errors.New("lost response")}
		}
		c := newTestController(t, api, newTestClock())
		result, err := c.sync(context.Background(), "project-a/build-a")
		if err == nil || controller.IsPermanent(err) {
			t.Fatalf("want retryable error, got %v", err)
		}
		if result != (controller.ReconcileResult{}) {
			t.Fatalf("result=%+v", result)
		}
		if api.CallCount("CreateSnapshot") != 1 {
			t.Fatal("an unconfirmed create must not be repeated inside one round")
		}
		if got := api.CallCount("GetProject project-a"); got != 1 {
			t.Fatalf("project reads = %d, want 1", got)
		}
		if requested == nil {
			t.Fatal("the create request is missing")
		}
		project := api.getProject("project-a")
		if requested.Spec.DefaultRef != project.Spec.DefaultRef || !reflect.DeepEqual(repoNames(requested.Spec.PackageRepos), []string{"gcc"}) {
			t.Fatalf("the create request must be built from the single Project read: %+v", requested.Spec)
		}
		if api.CallCount("UpdateBuildStatus") != 0 {
			t.Fatal("an unconfirmed create must not advance the Build")
		}
	})
}

// TestPendingCreateSnapshotRejections pins the create side of the write error classification: 408/429/5xx stay
// retryable, other rejections are permanent, and a rejected create never runs a confirmation read.
func TestPendingCreateSnapshotRejections(t *testing.T) {
	for _, tc := range []struct {
		name          string
		statusCode    int
		wantPermanent bool
	}{
		{name: "request timeout", statusCode: 408},
		{name: "too many requests", statusCode: 429},
		{name: "server error", statusCode: 500},
		{name: "unavailable", statusCode: 503},
		{name: "not found", statusCode: 404},
		{name: "bad request", statusCode: 400, wantPermanent: true},
		{name: "forbidden", statusCode: 403, wantPermanent: true},
		{name: "invalid", statusCode: 422, wantPermanent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI()
			api.builds[key("project-a", "build-a")] = withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
			api.projects["project-a"] = newProject("project-a", newPackageRepo("gcc"))
			beforeConflicts := counterValue(t, metricEnsureConflicts)
			var output bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&output)
			defer log.SetOutput(previous)
			api.hooks.createSnapshot = func(*ebsv1.Snapshot) (*ebsv1.Snapshot, error) {
				return nil, &clientpkg.WriteError{
					Operation: "create", Outcome: clientpkg.WriteRejected,
					StatusCode: tc.statusCode, Err: errors.New("create rejected"),
				}
			}
			c := newTestController(t, api, newTestClock())
			result, err := c.sync(context.Background(), "project-a/build-a")
			if got := controller.IsPermanent(err); got != tc.wantPermanent {
				t.Fatalf("permanent = %v, want %v (err=%v)", got, tc.wantPermanent, err)
			}
			if !tc.wantPermanent {
				var writeErr *clientpkg.WriteError
				if !errors.As(err, &writeErr) || writeErr.StatusCode != tc.statusCode {
					t.Fatalf("a retryable rejection must be returned unchanged: %v", err)
				}
			}
			if result != (controller.ReconcileResult{}) {
				t.Fatalf("result=%+v", result)
			}
			if api.CallCount("GetSnapshot") != 1 {
				t.Fatalf("a rejected create must not run a confirmation read: %d", api.CallCount("GetSnapshot"))
			}
			if api.CallCount("UpdateBuildStatus") != 0 {
				t.Fatal("a rejected create must not write Build.status")
			}
			if stored := api.build("project-a", "build-a"); stored.Status.Phase != ebsv1.BuildPending {
				t.Fatalf("phase = %q", stored.Status.Phase)
			}
			if api.CallCount("CreateRpmRepo") != 0 {
				t.Fatal("a rejected create must not advance the Build")
			}
			logged := output.String()
			for _, want := range []string{
				"reason=CreateRejected",
				"kind=Snapshot",
				"operation=create",
				fmt.Sprintf("status=%d", tc.statusCode),
			} {
				if !strings.Contains(logged, want) {
					t.Fatalf("create rejection log misses %q: %q", want, logged)
				}
			}
			wantRetryable := fmt.Sprintf("retryable=%t", !tc.wantPermanent)
			if !strings.Contains(logged, wantRetryable) {
				t.Fatalf("create rejection log misses %q: %q", wantRetryable, logged)
			}
			assertCounterDelta(t, metricEnsureConflicts, beforeConflicts, 0)
		})
	}
}

func TestPendingAdvancesToPrepared(t *testing.T) {
	for _, tc := range []struct {
		name        string
		buildType   string
		wantRpmRepo bool
	}{
		{name: "single skips RpmRepo", buildType: "single"},
		{name: "full creates RpmRepo", buildType: "full", wantRpmRepo: true},
		{name: "specified creates RpmRepo", buildType: "specified", wantRpmRepo: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI()
			api.builds[key("project-a", "build-a")] = withBaseBuildRef(newBuild("project-a", "build-a", tc.buildType, []string{"gcc"}))
			api.projects["project-a"] = newProject("project-a", newPackageRepo("gcc"))
			api.storeSnapshot(&ebsv1.Snapshot{
				ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a"},
				Status:     ebsv1.SnapshotStatus{Phase: ebsv1.SnapshotActive},
			})
			c := newTestController(t, api, newTestClock())
			result, err := c.sync(context.Background(), "project-a/build-a")
			if err != nil || result != (controller.ReconcileResult{}) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			stored := api.build("project-a", "build-a")
			if stored.Status.Phase != ebsv1.BuildPrepared || stored.Status.Stage != "" {
				t.Fatalf("phase/stage = %q/%q", stored.Status.Phase, stored.Status.Stage)
			}
			if got := api.CallCount("CreateRpmRepo") > 0; got != tc.wantRpmRepo {
				t.Fatalf("RpmRepo created = %v, want %v", got, tc.wantRpmRepo)
			}
			if api.rpmRepo("project-a", "build-a") != nil != tc.wantRpmRepo {
				t.Fatal("unexpected RpmRepo state")
			}
		})
	}
}

func TestPendingCreateRpmRepoAlreadyExistsReusesObject(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
	api.storeSnapshot(activeSnapshot("project-a", "build-a"))
	api.hooks.createRpmRepo = func(request *ebsv1.RpmRepo) (*ebsv1.RpmRepo, error) {
		api.storeRpmRepo(request)
		return nil, &clientpkg.WriteError{Operation: "create", Outcome: clientpkg.WriteRejected, StatusCode: 409, Err: apierrors.NewAlreadyExists(rpmReposResource, request.Name)}
	}
	c := newTestController(t, api, newTestClock())
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if api.CallCount("CreateRpmRepo") != 1 {
		t.Fatalf("create calls = %d", api.CallCount("CreateRpmRepo"))
	}
	if stored := api.build("project-a", "build-a"); stored.Status.Phase != ebsv1.BuildPrepared {
		t.Fatalf("phase = %q", stored.Status.Phase)
	}
}

func TestPendingWaitsForTerminatingRpmRepo(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withBaseBuildRefName(newBuild("project-a", "build-a", "full", []string{"gcc"}), "build-prev")
	api.storeSnapshot(activeSnapshot("project-a", "build-a"))
	deletion := metav1.NewTime(time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC))
	api.storeRpmRepo(&ebsv1.RpmRepo{
		ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a", DeletionTimestamp: &deletion},
		Spec:       ebsv1.RpmRepoSpec{},
	})
	c := newTestController(t, api, newTestClock())
	result, err := c.sync(context.Background(), "project-a/build-a")
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if api.CallCount("CreateRpmRepo") != 0 || api.CallCount("UpdateBuildStatus") != 0 {
		t.Fatal("a terminating RpmRepo must keep the Build waiting")
	}
	if api.CallCount("GetRpmRepo project-a/build-prev") != 0 {
		t.Fatal("an existing RpmRepo must not trigger the historical base read")
	}
}

// snapshotMissingTargetCondition is the Snapshot controller's condition type for a single build target that
// cannot be resolved from the snapshot package repositories. The Build Controller deliberately does not
// consume it, so the literal is pinned here to keep that boundary under test.
const snapshotMissingTargetCondition = "TargetPackagesNotFound"

// TestPendingAdvancesWhenSnapshotReportsMissingTargets pins the boundary that missing target packages are a
// Snapshot quality record only: the Build still waits for Active only and advances to Prepared.
func TestPendingAdvancesWhenSnapshotReportsMissingTargets(t *testing.T) {
	for _, tc := range []struct {
		name            string
		buildType       string
		wantRpmRepoCall int
	}{
		{name: "single skips RpmRepo", buildType: "single"},
		{name: "full creates RpmRepo", buildType: "full", wantRpmRepoCall: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI()
			api.builds[key("project-a", "build-a")] = withBaseBuildRef(newBuild("project-a", "build-a", tc.buildType, []string{"gcc"}))
			api.storeSnapshot(&ebsv1.Snapshot{
				ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a"},
				Status: ebsv1.SnapshotStatus{
					Phase: ebsv1.SnapshotActive,
					Conditions: []metav1.Condition{{
						Type: snapshotMissingTargetCondition, Status: metav1.ConditionTrue,
						Reason: snapshotMissingTargetCondition,
					}},
				},
			})
			c := newTestController(t, api, newTestClock())
			result, err := c.sync(context.Background(), "project-a/build-a")
			if err != nil || result != (controller.ReconcileResult{}) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			stored := api.build("project-a", "build-a")
			if stored.Status.Phase != ebsv1.BuildPrepared || stored.Status.Stage != "" {
				t.Fatalf("phase/stage = %q/%q", stored.Status.Phase, stored.Status.Stage)
			}
			if !stored.Status.EndTime.IsZero() {
				t.Fatalf("a missing target package must not finish the Build: %+v", stored.Status)
			}
			if len(stored.Status.Conditions) != 0 {
				t.Fatalf("conditions = %+v", stored.Status.Conditions)
			}
			if got := api.CallCount("CreateRpmRepo"); got != tc.wantRpmRepoCall {
				t.Fatalf("CreateRpmRepo calls = %d, want %d", got, tc.wantRpmRepoCall)
			}
			if api.CallCount("CreateBuildInfo") != 0 || api.CallCount("GetProject") != 0 {
				t.Fatal("Pending must not create BuildInfo or read Project")
			}
			if api.CallCount("UpdateBuildStatus") != 1 {
				t.Fatalf("status writes = %d", api.CallCount("UpdateBuildStatus"))
			}
		})
	}
}

func TestNewSnapshotCopiesProjectInputWithoutSharingState(t *testing.T) {
	project := newProject("project-a", newPackageRepo("gcc"), newPackageRepo("glibc"))
	build := withBaseBuildRef(newBuild("project-a", "build-a", "single", []string{"glibc", "gcc", "glibc"}))
	snapshot := newSnapshot(build, project)
	if !reflect.DeepEqual(repoNames(snapshot.Spec.PackageRepos), []string{"gcc", "glibc"}) {
		t.Fatalf("packageRepos = %v", repoNames(snapshot.Spec.PackageRepos))
	}
	if snapshot.Spec.DefaultRef != project.Spec.DefaultRef {
		t.Fatalf("defaultRef = %+v", snapshot.Spec.DefaultRef)
	}
	snapshot.Spec.PackageRepos[0].Name = "mutated"
	snapshot.Spec.PackageRepos[0].BuildTargets[0].Arch = "mutated"
	snapshot.Spec.DefaultRef.Value = "mutated"
	if project.Spec.PackageRepos[0].Name != "gcc" || project.Spec.PackageRepos[0].BuildTargets[0].Arch != "aarch64" || project.Spec.DefaultRef.Value != "master" {
		t.Fatal("Snapshot construction must not share state with the Project")
	}
}

func TestNewSnapshotKeepsFullRepositoryListForNonSingleBuilds(t *testing.T) {
	project := newProject("project-a", newPackageRepo("gcc"), newPackageRepo("glibc"))
	build := withBaseBuildRef(newBuild("project-a", "build-a", "incremental", []string{"gcc"}))
	snapshot := newSnapshot(build, project)
	if !reflect.DeepEqual(repoNames(snapshot.Spec.PackageRepos), []string{"gcc", "glibc"}) {
		t.Fatalf("packageRepos = %v", repoNames(snapshot.Spec.PackageRepos))
	}
}

func TestNewBuildInfoCopiesBootstrapRepos(t *testing.T) {
	project := newProject("project-a", newPackageRepo("gcc"))
	build := withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
	info := newBuildInfo(build, project)
	if !reflect.DeepEqual(info.Spec.BootstrapRepo, project.Spec.BootstrapRepo) {
		t.Fatalf("bootstrapRepo = %+v", info.Spec.BootstrapRepo)
	}
	if len(info.Spec.SpecDepends) != 0 {
		t.Fatalf("specDepends = %+v", info.Spec.SpecDepends)
	}
	info.Spec.BootstrapRepo[0].Repo = "mutated"
	if project.Spec.BootstrapRepo[0].Repo != "https://example.com/repo/everything" {
		t.Fatal("BuildInfo construction must not share state with the Project")
	}
}

func TestPreparedEnsuresBuildInfoAndEntersBuildStage(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildPrepared, "")
	api.projects["project-a"] = newProject("project-a", newPackageRepo("gcc"))
	clk := newTestClock()
	c := newTestController(t, api, clk)
	result, err := c.sync(context.Background(), "project-a/build-a")
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	info := api.buildInfo("project-a", "build-a")
	if info == nil || !reflect.DeepEqual(info.Spec.BootstrapRepo, api.getProject("project-a").Spec.BootstrapRepo) {
		t.Fatalf("BuildInfo = %+v", info)
	}
	stored := api.build("project-a", "build-a")
	if stored.Status.Phase != ebsv1.BuildProcessing || stored.Status.Stage != stageBuild {
		t.Fatalf("phase/stage = %q/%q", stored.Status.Phase, stored.Status.Stage)
	}
	if stored.Spec.BuildType != "full" || !reflect.DeepEqual(stored.Spec.Packages, []string{"gcc"}) {
		t.Fatalf("the status write must preserve the Build spec: %+v", stored.Spec)
	}
	if !stored.Status.StartTime.Time.Equal(clk.Now().UTC()) {
		t.Fatalf("startTime = %+v, want %+v", stored.Status.StartTime, clk.Now().UTC())
	}
	if api.CallCount("GetSnapshot") != 0 || api.CallCount("GetRpmRepo") != 0 {
		t.Fatal("Prepared must only ensure its own BuildInfo")
	}
	calls := api.Calls()
	if indexOf(calls, "CreateBuildInfo project-a/build-a") > indexOf(calls, "UpdateBuildStatus project-a/build-a") {
		t.Fatalf("BuildInfo must be created before the status write: %v", calls)
	}
}

func TestPreparedReusesExistingBuildInfo(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildPrepared, "")
	api.storeBuildInfo(&ebsv1.BuildInfo{
		ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a"},
		Spec:       ebsv1.BuildInfoSpec{BootstrapRepo: []ebsv1.BootstrapRepo{{Name: "existing", Repo: "https://example.com/existing"}}},
	})
	c := newTestController(t, api, newTestClock())
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if api.CallCount("GetProject") != 0 || api.CallCount("CreateBuildInfo") != 0 {
		t.Fatal("an existing BuildInfo must not be recreated or re-read from the Project")
	}
	if info := api.buildInfo("project-a", "build-a"); info.Spec.BootstrapRepo[0].Name != "existing" {
		t.Fatalf("BuildInfo spec was overwritten: %+v", info.Spec)
	}
	if stored := api.build("project-a", "build-a"); stored.Status.Phase != ebsv1.BuildProcessing {
		t.Fatalf("phase = %q", stored.Status.Phase)
	}
}

func TestPreparedCreateBuildInfoAlreadyExistsReusesObject(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildPrepared, "")
	api.projects["project-a"] = newProject("project-a", newPackageRepo("gcc"))
	api.hooks.createBuildInfo = func(request *ebsv1.BuildInfo) (*ebsv1.BuildInfo, error) {
		api.storeBuildInfo(request)
		return nil, &clientpkg.WriteError{Operation: "create", Outcome: clientpkg.WriteRejected, StatusCode: 409, Err: apierrors.NewAlreadyExists(buildInfosResource, request.Name)}
	}
	c := newTestController(t, api, newTestClock())
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if api.CallCount("CreateBuildInfo") != 1 {
		t.Fatalf("create calls = %d", api.CallCount("CreateBuildInfo"))
	}
	if stored := api.build("project-a", "build-a"); stored.Status.Phase != ebsv1.BuildProcessing || stored.Status.Stage != stageBuild {
		t.Fatalf("phase/stage = %q/%q", stored.Status.Phase, stored.Status.Stage)
	}
}

func TestPreparedUsesCurrentProjectConfigurationForCreate(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildPrepared, "")
	project := newProject("project-a", newPackageRepo("gcc"))
	project.Spec.BootstrapRepo = []ebsv1.BootstrapRepo{{Name: "current", Repo: "https://example.com/current"}}
	api.projects["project-a"] = project
	c := newTestController(t, api, newTestClock())
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := api.buildInfo("project-a", "build-a").Spec.BootstrapRepo[0].Name; got != "current" {
		t.Fatalf("bootstrapRepo = %q", got)
	}
}

func findCondition(t *testing.T, conditions []metav1.Condition, condType string) metav1.Condition {
	t.Helper()
	for _, condition := range conditions {
		if condition.Type == condType {
			return condition
		}
	}
	t.Fatalf("condition %s not found in %+v", condType, conditions)
	return metav1.Condition{}
}

func indexOf(values []string, target string) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	return -1
}
