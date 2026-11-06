package build

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
)

// snapshotMissingTargetCondition is the Snapshot controller's condition type for a single build target that
// cannot be resolved from the snapshot package repositories. The Build Controller deliberately does not
// consume it, so the literal is pinned here to keep that boundary under test.
const snapshotMissingTargetCondition = "TargetPackagesNotFound"

func TestPendingRecordsBaseBuildRefFromHistory(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = newBuild("project-a", "build-a", "full", []string{"gcc"})
	previous := withPhaseStage(newBuild("project-a", "build-prev", "full", []string{"gcc"}), ebsv1.BuildSuccess, ebsv1.BuildStagePublish)
	api.hooks.lastPublished = func(project, targetArch string) (*ebsv1.Build, error) {
		if project != "project-a" || targetArch != "aarch64" {
			t.Errorf("unexpected history query %q %q", project, targetArch)
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

func TestPendingAdvancesToPrepared(t *testing.T) {
	for _, tc := range []struct {
		name        string
		buildType   ebsv1.BuildType
		wantRpmRepo bool
	}{
		{name: "single skips RpmRepo", buildType: "single", wantRpmRepo: false},
		{name: "full creates RpmRepo", buildType: "full", wantRpmRepo: true},
		{name: "incremental creates RpmRepo", buildType: "incremental", wantRpmRepo: true},
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

// TestPendingAdvancesWhenSnapshotReportsMissingTargets pins the boundary that missing target packages are a
// Snapshot quality record only: the Build still waits for Active only and advances to Prepared.
func TestPendingAdvancesWhenSnapshotReportsMissingTargets(t *testing.T) {
	for _, tc := range []struct {
		name            string
		buildType       ebsv1.BuildType
		wantRpmRepoCall int
	}{
		{name: "single skips RpmRepo", buildType: "single", wantRpmRepoCall: 0},
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

func TestPreparedEnsuresBuildInfoAndEntersBuildStage(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildPrepared, "")
	project := newProject("project-a", newPackageRepo("gcc"))
	project.Spec.BootstrapRepos = []ebsv1.BootstrapRepo{{Name: "current", URL: "https://example.com/current"}}
	api.projects["project-a"] = project
	clk := newTestClock()
	c := newTestController(t, api, clk)
	result, err := c.sync(context.Background(), "project-a/build-a")
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	info := api.buildInfo("project-a", "build-a")
	if info == nil || !reflect.DeepEqual(info.Spec.BootstrapRepos, api.getProject("project-a").Spec.BootstrapRepos) {
		t.Fatalf("BuildInfo = %+v", info)
	}
	stored := api.build("project-a", "build-a")
	if info.Spec.Image != "build:test" || api.CallCount("GetBuildTargetContent") != 1 {
		t.Fatalf("image = %q, config reads = %d", info.Spec.Image, api.CallCount("GetBuildTargetContent"))
	}
	if stored.Status.Phase != ebsv1.BuildProcessing || stored.Status.Stage != ebsv1.BuildStageBuild {
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
	if slices.Index(calls, "CreateBuildInfo project-a/build-a") > slices.Index(calls, "UpdateBuildStatus project-a/build-a") {
		t.Fatalf("BuildInfo must be created before the status write: %v", calls)
	}
}

func TestProcessingBuildWaitsForCompletedBuildInfo(t *testing.T) {
	for _, phase := range []ebsv1.BuildInfoPhase{ebsv1.BuildInfoPending, ebsv1.BuildInfoProcessing, ebsv1.BuildInfoPhase("Unknown")} {
		t.Run(string(phase), func(t *testing.T) {
			api := newFakeAPI()
			api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildProcessing, ebsv1.BuildStageBuild)
			api.buildInfos[key("project-a", "build-a")] = newBuildInfoObject("project-a", "build-a", phase, map[string]ebsv1.SpecResult{"gcc": {Status: ebsv1.SpecSucceeded}})
			c := newTestController(t, api, newTestClock())
			result, err := c.sync(context.Background(), "project-a/build-a")
			if err != nil || result != (controller.ReconcileResult{}) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if api.CallCount("UpdateBuildStatus") != 0 {
				t.Fatal("an unfinished BuildInfo must not advance the Build")
			}
		})
	}
}

func TestProcessingBuildFailsOnFailedBuildInfo(t *testing.T) {
	for _, buildType := range []string{"single", "full"} {
		t.Run(string(buildType), func(t *testing.T) {
			api := newFakeAPI()
			api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", ebsv1.BuildType(buildType), []string{"gcc"}), ebsv1.BuildProcessing, ebsv1.BuildStageBuild)
			api.buildInfos[key("project-a", "build-a")] = newBuildInfoObject("project-a", "build-a", ebsv1.BuildInfoFailed,
				map[string]ebsv1.SpecResult{"gcc": {Status: ebsv1.SpecSucceeded}})
			c := newTestController(t, api, newTestClock())
			if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
				t.Fatal(err)
			}
			stored := api.build("project-a", "build-a")
			if stored.Status.Phase != ebsv1.BuildFailed || stored.Status.Stage != ebsv1.BuildStageBuild || stored.Status.EndTime.IsZero() {
				t.Fatalf("status = %+v", stored.Status)
			}
			condition := findCondition(t, stored.Status.Conditions, ConditionBuildSucceed)
			if condition.Status != metav1.ConditionFalse || condition.Reason != ReasonBuildPayloadParseFailed {
				t.Fatalf("condition = %+v", condition)
			}
			if _, exists := testCondition(stored.Status.Conditions, ConditionPublishSucceed); exists {
				t.Fatal("failed BuildInfo reached publish")
			}
		})
	}
}

func TestProcessingBuildFailsWhenBuildInfoIsMissing(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildProcessing, ebsv1.BuildStageBuild)
	c := newTestController(t, api, newTestClock())
	_, err := c.sync(context.Background(), "project-a/build-a")
	if !controller.IsPermanent(err) {
		t.Fatalf("want permanent error, got %v", err)
	}
	stored := api.build("project-a", "build-a")
	if stored.Status.Phase != ebsv1.BuildFailed || stored.Status.Stage != ebsv1.BuildStageBuild || stored.Status.EndTime.IsZero() {
		t.Fatalf("status = %+v", stored.Status)
	}
	condition := findCondition(t, stored.Status.Conditions, ConditionBuildSucceed)
	if condition.Status != metav1.ConditionFalse || condition.Reason != ReasonChildResourceMissing {
		t.Fatalf("condition = %+v", condition)
	}
	if api.CallCount("CreateBuildInfo") != 0 || api.CallCount("GetProject") != 0 || api.CallCount("GetSnapshot") != 0 {
		t.Fatal("a missing direct dependency must not rebuild earlier stages")
	}
}

func TestProcessingBuildSingleAggregation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		statuses  map[string]ebsv1.SpecResult
		succeeded bool
		wantStage ebsv1.BuildStage
	}{
		{
			name:      "all succeeded",
			statuses:  map[string]ebsv1.SpecResult{"gcc": {Status: ebsv1.SpecSucceeded}},
			succeeded: true,
			wantStage: ebsv1.BuildStageBuild,
		},
		{
			name:      "multiple specs",
			statuses:  map[string]ebsv1.SpecResult{"gcc": {Status: ebsv1.SpecSucceeded}, "glibc": {Status: ebsv1.SpecSucceeded}},
			succeeded: true,
			wantStage: ebsv1.BuildStageBuild,
		},
		{
			name:      "dependencies missing",
			statuses:  map[string]ebsv1.SpecResult{"gcc": {Status: ebsv1.SpecRpmDependsMissing}},
			wantStage: ebsv1.BuildStageBuild,
		},
		{
			name:      "build failed",
			statuses:  map[string]ebsv1.SpecResult{"gcc": {Status: ebsv1.SpecBuildFailed}},
			wantStage: ebsv1.BuildStageBuild,
		},
		{
			name:      "install failed",
			statuses:  map[string]ebsv1.SpecResult{"gcc": {Status: ebsv1.SpecInstallFailed}},
			wantStage: ebsv1.BuildStageBuild,
		},
		{
			name:      "build running",
			statuses:  map[string]ebsv1.SpecResult{"gcc": {Status: ebsv1.SpecRunning}},
			wantStage: ebsv1.BuildStageBuild,
		},
		{
			name: "one of many failed",
			statuses: map[string]ebsv1.SpecResult{
				"gcc":   {Status: ebsv1.SpecSucceeded},
				"glibc": {Status: ebsv1.SpecBuildFailed},
			},
			wantStage: ebsv1.BuildStageBuild,
		},
		{
			name:      "empty result set",
			statuses:  map[string]ebsv1.SpecResult{},
			wantStage: ebsv1.BuildStageBuild,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI()
			api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", "single", []string{"gcc"}), ebsv1.BuildProcessing, ebsv1.BuildStageBuild)
			api.buildInfos[key("project-a", "build-a")] = newBuildInfoObject("project-a", "build-a", ebsv1.BuildInfoCompleted, tc.statuses)
			clk := newTestClock()
			c := newTestController(t, api, clk)
			result, err := c.sync(context.Background(), "project-a/build-a")
			if result != (controller.ReconcileResult{}) {
				t.Fatalf("result=%+v", result)
			}
			if tc.succeeded {
				if err != nil {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			} else if !controller.IsPermanent(err) {
				t.Fatalf("want permanent error, got %v", err)
			}
			stored := api.build("project-a", "build-a")
			wantPhase := ebsv1.BuildFailed
			wantReason := ReasonBuildFailed
			if tc.succeeded {
				wantPhase, wantReason = ebsv1.BuildSuccess, ReasonBuildSucceeded
			}
			if stored.Status.Phase != wantPhase || stored.Status.Stage != tc.wantStage || stored.Status.EndTime.IsZero() {
				t.Fatalf("status = %+v, want phase=%q stage=%q", stored.Status, wantPhase, tc.wantStage)
			}
			condition := findCondition(t, stored.Status.Conditions, ConditionBuildSucceed)
			if condition.Status != conditionStatus(tc.succeeded) || condition.Reason != wantReason || condition.Message != wantReason {
				t.Fatalf("condition = %+v", condition)
			}
			assertConditionTime(t, condition, clk.Now().UTC(), stored.Status.EndTime)
			if _, ok := testCondition(stored.Status.Conditions, ConditionPublishSucceed); ok {
				t.Fatal("single builds must not record a publish result")
			}
			if api.CallCount("GetRpmRepo") != 0 || api.CallCount("CreateRpmRepo") != 0 {
				t.Fatal("single builds must never touch the RpmRepo of this round")
			}
			if api.CallCount("GetSnapshot") != 0 {
				t.Fatal("Processing/build must not walk back to the Snapshot")
			}
		})
	}
}

func TestAggregateSpecResultsIsIndependentOfMapOrder(t *testing.T) {
	first := map[string]ebsv1.SpecResult{}
	first["gcc"] = ebsv1.SpecResult{Status: ebsv1.SpecSucceeded}
	first["glibc"] = ebsv1.SpecResult{Status: ebsv1.SpecBuildFailed}
	second := map[string]ebsv1.SpecResult{}
	second["glibc"] = ebsv1.SpecResult{Status: ebsv1.SpecBuildFailed}
	second["gcc"] = ebsv1.SpecResult{Status: ebsv1.SpecSucceeded}
	firstResult, firstSpec := aggregateSpecResults(testSpecResults(first))
	secondResult, secondSpec := aggregateSpecResults(testSpecResults(second))
	if firstResult != secondResult || firstSpec != secondSpec || firstResult || firstSpec != "glibc" {
		t.Fatalf("aggregation depends on map order: %v/%q vs %v/%q", firstResult, firstSpec, secondResult, secondSpec)
	}
}

func TestProcessingBuildNonSingleResults(t *testing.T) {
	for _, tc := range []struct {
		name          string
		statuses      map[string]ebsv1.SpecResult
		publish       bool
		wantPhase     ebsv1.BuildPhase
		wantStage     ebsv1.BuildStage
		wantEndTime   bool
		wantSucceeded bool
		wantPermanent bool
	}{
		{
			name: "success publishes", statuses: map[string]ebsv1.SpecResult{"gcc": {Status: ebsv1.SpecSucceeded}},
			publish: true, wantPhase: ebsv1.BuildProcessing, wantStage: ebsv1.BuildStagePublish, wantSucceeded: true,
		},
		{
			name: "failure still publishes", statuses: map[string]ebsv1.SpecResult{"gcc": {Status: ebsv1.SpecBuildFailed}},
			publish: true, wantPhase: ebsv1.BuildProcessing, wantStage: ebsv1.BuildStagePublish,
		},
		{
			name: "success skips publishing", statuses: map[string]ebsv1.SpecResult{"gcc": {Status: ebsv1.SpecSucceeded}},
			wantPhase: ebsv1.BuildSkipped, wantStage: ebsv1.BuildStagePublish, wantEndTime: true, wantSucceeded: true,
		},
		{
			name: "failure skips publishing", statuses: map[string]ebsv1.SpecResult{"gcc": {Status: ebsv1.SpecInstallFailed}},
			wantPhase: ebsv1.BuildSkipped, wantStage: ebsv1.BuildStagePublish, wantEndTime: true, wantPermanent: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			build := withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildProcessing, ebsv1.BuildStageBuild)
			build.Spec.Publish = &tc.publish
			api := newFakeAPI()
			api.builds[key("project-a", "build-a")] = build
			api.buildInfos[key("project-a", "build-a")] = newBuildInfoObject("project-a", "build-a", ebsv1.BuildInfoCompleted, tc.statuses)
			c := newTestController(t, api, newTestClock())
			result, err := c.sync(context.Background(), "project-a/build-a")
			if tc.wantPermanent {
				if !controller.IsPermanent(err) {
					t.Fatalf("want permanent error, got %v", err)
				}
			} else if err != nil || result != (controller.ReconcileResult{}) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			stored := api.build("project-a", "build-a")
			if stored.Status.Phase != tc.wantPhase || stored.Status.Stage != tc.wantStage {
				t.Fatalf("phase/stage = %q/%q, want %q/%q", stored.Status.Phase, stored.Status.Stage, tc.wantPhase, tc.wantStage)
			}
			if stored.Status.EndTime.IsZero() == tc.wantEndTime {
				t.Fatalf("endTime = %v, want set=%v", stored.Status.EndTime, tc.wantEndTime)
			}
			condition := findCondition(t, stored.Status.Conditions, ConditionBuildSucceed)
			if condition.Status != conditionStatus(tc.wantSucceeded) {
				t.Fatalf("condition = %+v", condition)
			}
			if _, ok := testCondition(stored.Status.Conditions, ConditionPublishSucceed); ok {
				t.Fatal("the build stage must not write the publish result")
			}
		})
	}
}

func TestProcessingPublishWaitsForReleaseOutcome(t *testing.T) {
	for _, release := range []*ebsv1.RpmRepoReleaseStatus{
		nil,
		{Phase: ebsv1.RpmRepoReleasePending},
		{Phase: ebsv1.RpmRepoReleaseCreating},
	} {
		name := "missing"
		if release != nil {
			name = string(release.Phase)
		}
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI()
			api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildProcessing, ebsv1.BuildStagePublish)
			api.rpmRepos[key("project-a", "build-a")] = &ebsv1.RpmRepo{
				ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a"},
				Status: ebsv1.RpmRepoStatus{
					Repository: &ebsv1.RpmRepoRepositoryStatus{},
					Release:    release,
				},
			}
			c := newTestController(t, api, newTestClock())
			result, err := c.sync(context.Background(), "project-a/build-a")
			if err != nil || result != (controller.ReconcileResult{}) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if api.CallCount("UpdateBuildStatus") != 0 {
				t.Fatal("an unfinished release must not advance the Build")
			}
			if api.CallCount("GetBuildInfo") != 0 || api.CallCount("GetSnapshot") != 0 {
				t.Fatal("Processing/publish must read only its direct dependency")
			}
		})
	}
}

func TestProcessingPublishReadyWritesSuccess(t *testing.T) {
	api := newFakeAPI()
	build := withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildProcessing, ebsv1.BuildStagePublish)
	build.Status.Conditions = []metav1.Condition{{
		Type: ConditionBuildSucceed, Status: metav1.ConditionTrue, Reason: ReasonBuildSucceeded,
		Message: ReasonBuildSucceeded, ObservedGeneration: build.Generation, LastTransitionTime: testTime(9),
	}}
	api.builds[key("project-a", "build-a")] = build
	api.rpmRepos[key("project-a", "build-a")] = &ebsv1.RpmRepo{
		ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a"},
		Status: ebsv1.RpmRepoStatus{
			Repository: &ebsv1.RpmRepoRepositoryStatus{ContentURL: "https://internal.example.com/repo"},
			Release: &ebsv1.RpmRepoReleaseStatus{
				Phase:      ebsv1.RpmRepoReleaseReady,
				ContentURL: "https://release.example.com/project-a/aarch64",
			},
		},
	}
	clk := newTestClock()
	c := newTestController(t, api, clk)
	result, err := c.sync(context.Background(), "project-a/build-a")
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	stored := api.build("project-a", "build-a")
	if stored.Status.Phase != ebsv1.BuildSuccess || stored.Status.Stage != ebsv1.BuildStagePublish {
		t.Fatalf("phase/stage = %q/%q", stored.Status.Phase, stored.Status.Stage)
	}
	if !stored.Status.EndTime.Time.Equal(clk.Now().UTC()) {
		t.Fatalf("endTime = %+v", stored.Status.EndTime)
	}
	publish := findCondition(t, stored.Status.Conditions, ConditionPublishSucceed)
	if publish.Status != metav1.ConditionTrue || publish.Reason != ReasonPublishSucceeded {
		t.Fatalf("publish condition = %+v", publish)
	}
	assertConditionTime(t, publish, clk.Now().UTC(), stored.Status.EndTime)
	buildCondition := findCondition(t, stored.Status.Conditions, ConditionBuildSucceed)
	if buildCondition.Status != metav1.ConditionTrue || !buildCondition.LastTransitionTime.Time.Equal(testTime(9).Time) {
		t.Fatalf("the build result must be preserved: %+v", buildCondition)
	}
	if api.CallCount("UpdateBuildStatus") != 1 {
		t.Fatalf("status writes = %d", api.CallCount("UpdateBuildStatus"))
	}
}

func TestProcessingPublishFailureConverges(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildProcessing, ebsv1.BuildStagePublish)
	api.rpmRepos[key("project-a", "build-a")] = &ebsv1.RpmRepo{
		ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a"},
		Status: ebsv1.RpmRepoStatus{
			Release: &ebsv1.RpmRepoReleaseStatus{
				Phase:      ebsv1.RpmRepoReleaseFailed,
				ContentURL: "https://release.example.com/stale",
			},
		},
	}
	clk := newTestClock()
	c := newTestController(t, api, clk)
	_, err := c.sync(context.Background(), "project-a/build-a")
	if !controller.IsPermanent(err) {
		t.Fatalf("want permanent error, got %v", err)
	}
	stored := api.build("project-a", "build-a")
	if stored.Status.Phase != ebsv1.BuildFailed || stored.Status.Stage != ebsv1.BuildStagePublish {
		t.Fatalf("phase/stage = %q/%q", stored.Status.Phase, stored.Status.Stage)
	}
	condition := findCondition(t, stored.Status.Conditions, ConditionPublishSucceed)
	if condition.Status != metav1.ConditionFalse || condition.Reason != ReasonPublishFailed {
		t.Fatalf("condition = %+v", condition)
	}
	assertConditionTime(t, condition, clk.Now().UTC(), stored.Status.EndTime)
}

func TestProcessingPublishFailsWhenRpmRepoIsMissing(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildProcessing, ebsv1.BuildStagePublish)
	c := newTestController(t, api, newTestClock())
	_, err := c.sync(context.Background(), "project-a/build-a")
	if !controller.IsPermanent(err) {
		t.Fatalf("want permanent error, got %v", err)
	}
	stored := api.build("project-a", "build-a")
	if stored.Status.Phase != ebsv1.BuildFailed || stored.Status.Stage != ebsv1.BuildStagePublish {
		t.Fatalf("phase/stage = %q/%q", stored.Status.Phase, stored.Status.Stage)
	}
	condition := findCondition(t, stored.Status.Conditions, ConditionPublishSucceed)
	if condition.Status != metav1.ConditionFalse || condition.Reason != ReasonChildResourceMissing {
		t.Fatalf("condition = %+v", condition)
	}
	if api.CallCount("CreateRpmRepo") != 0 || api.CallCount("GetProject") != 0 {
		t.Fatal("a missing direct dependency must not be recreated")
	}
}

func TestProcessingReadFailuresStayTransient(t *testing.T) {
	temporary := errors.New("connection reset")
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildProcessing, ebsv1.BuildStagePublish)
	api.hooks.getRpmRepo = func(string, string) (*ebsv1.RpmRepo, error) { return nil, temporary }
	c := newTestController(t, api, newTestClock())
	result, err := c.sync(context.Background(), "project-a/build-a")
	if !errors.Is(err, temporary) || controller.IsPermanent(err) {
		t.Fatalf("want transient error, got %v", err)
	}
	if result != (controller.ReconcileResult{}) || api.CallCount("UpdateBuildStatus") != 0 {
		t.Fatalf("result=%+v writes=%d", result, api.CallCount("UpdateBuildStatus"))
	}
}

func TestPendingSingleBuildPreparesWithoutRepository(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = newBuild("project-a", "build-a", "single", []string{"gcc"})
	api.storeSnapshot(activeSnapshot("project-a", "build-a"))
	c := newTestController(t, api, newTestClock())
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if api.CallCount("GetRpmRepo") != 0 || api.CallCount("CreateRpmRepo") != 0 ||
		api.CallCount("GetLastPublishedBuild") != 0 {
		t.Fatalf("single build queried repository history: %v", api.calls)
	}
	if stored := api.build("project-a", "build-a"); stored.Status.Phase != ebsv1.BuildPrepared {
		t.Fatalf("phase = %q", stored.Status.Phase)
	}
}
