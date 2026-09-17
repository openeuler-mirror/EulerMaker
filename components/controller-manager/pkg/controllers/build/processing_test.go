package build

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func specStatus(build, install string) ebsv1.SpecStatus {
	return ebsv1.SpecStatus{
		Build:   ebsv1.SpecBuildStatus{Status: build},
		Install: ebsv1.SpecInstallStatus{Status: install},
	}
}

func newBuildInfoObject(project, name string, phase ebsv1.BuildInfoPhase, statuses map[string]ebsv1.SpecStatus) *ebsv1.BuildInfo {
	return &ebsv1.BuildInfo{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: project},
		Status:     ebsv1.BuildInfoStatus{Phase: phase, SpecStatus: statuses},
	}
}

func TestProcessingBuildWaitsForCompletedBuildInfo(t *testing.T) {
	for _, phase := range []ebsv1.BuildInfoPhase{ebsv1.BuildInfoPending, ebsv1.BuildInfoProcessing, ebsv1.BuildInfoPhase("Unknown")} {
		t.Run(string(phase), func(t *testing.T) {
			api := newFakeAPI()
			api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildProcessing, stageBuild)
			api.buildInfos[key("project-a", "build-a")] = newBuildInfoObject("project-a", "build-a", phase, map[string]ebsv1.SpecStatus{"gcc": specStatus(specStatusSucceeded, specStatusSucceeded)})
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

func TestProcessingBuildFailsWhenBuildInfoIsMissing(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildProcessing, stageBuild)
	c := newTestController(t, api, newTestClock())
	_, err := c.sync(context.Background(), "project-a/build-a")
	if !controller.IsPermanent(err) {
		t.Fatalf("want permanent error, got %v", err)
	}
	stored := api.build("project-a", "build-a")
	if stored.Status.Phase != ebsv1.BuildFailed || stored.Status.Stage != stageBuild || stored.Status.EndTime.IsZero() {
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
		statuses  map[string]ebsv1.SpecStatus
		succeeded bool
		wantStage string
	}{
		{name: "all succeeded", statuses: map[string]ebsv1.SpecStatus{"gcc": specStatus(specStatusSucceeded, specStatusSucceeded)}, succeeded: true, wantStage: stagePublish},
		{name: "multiple specs", statuses: map[string]ebsv1.SpecStatus{"gcc": specStatus(specStatusSucceeded, specStatusSucceeded), "glibc": specStatus(specStatusSucceeded, specStatusSucceeded)}, succeeded: true, wantStage: stagePublish},
		{name: "build failed", statuses: map[string]ebsv1.SpecStatus{"gcc": specStatus("Failed", specStatusSucceeded)}, wantStage: stageBuild},
		{name: "install failed", statuses: map[string]ebsv1.SpecStatus{"gcc": specStatus(specStatusSucceeded, "Failed")}, wantStage: stageBuild},
		{name: "install missing", statuses: map[string]ebsv1.SpecStatus{"gcc": specStatus(specStatusSucceeded, "")}, wantStage: stageBuild},
		{name: "build running", statuses: map[string]ebsv1.SpecStatus{"gcc": specStatus("Running", specStatusSucceeded)}, wantStage: stageBuild},
		{name: "one of many failed", statuses: map[string]ebsv1.SpecStatus{"gcc": specStatus(specStatusSucceeded, specStatusSucceeded), "glibc": specStatus("Aborted", specStatusSucceeded)}, wantStage: stageBuild},
		{name: "empty result set", statuses: map[string]ebsv1.SpecStatus{}, wantStage: stageBuild},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI()
			api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", "single", []string{"gcc"}), ebsv1.BuildProcessing, stageBuild)
			api.buildInfos[key("project-a", "build-a")] = newBuildInfoObject("project-a", "build-a", ebsv1.BuildInfoCompleted, tc.statuses)
			clk := newTestClock()
			c := newTestController(t, api, clk)
			result, err := c.sync(context.Background(), "project-a/build-a")
			if tc.succeeded {
				if err != nil || result != (controller.ReconcileResult{}) {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			} else if !controller.IsPermanent(err) {
				t.Fatalf("want permanent error, got %v", err)
			}
			stored := api.build("project-a", "build-a")
			wantPhase := ebsv1.BuildFailed
			wantReason := ReasonBuildFailed
			if tc.succeeded {
				wantPhase, wantReason = ebsv1.BuildSkipped, ReasonBuildSucceeded
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
			if stored.Status.Repo != "" {
				t.Fatalf("repo = %q", stored.Status.Repo)
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

// TestSingleBuildFailureKeepsBuildStage pins the state machine rule of the design: a failed single build is a
// build stage failure, so its stage must stay build instead of advancing to publish.
func TestSingleBuildFailureKeepsBuildStage(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", "single", []string{"gcc"}), ebsv1.BuildProcessing, stageBuild)
	api.buildInfos[key("project-a", "build-a")] = newBuildInfoObject("project-a", "build-a", ebsv1.BuildInfoCompleted,
		map[string]ebsv1.SpecStatus{"gcc": specStatus("Failed", specStatusSucceeded)})
	clk := newTestClock()
	c := newTestController(t, api, clk)

	result, err := c.sync(context.Background(), "project-a/build-a")
	if !controller.IsPermanent(err) {
		t.Fatalf("want a permanent error, got %v", err)
	}
	if result != (controller.ReconcileResult{}) {
		t.Fatalf("result=%+v", result)
	}
	stored := api.build("project-a", "build-a")
	if stored.Status.Phase != ebsv1.BuildFailed || stored.Status.Stage != stageBuild {
		t.Fatalf("phase/stage = %q/%q, want %q/%q", stored.Status.Phase, stored.Status.Stage, ebsv1.BuildFailed, stageBuild)
	}
	if stored.Status.EndTime.IsZero() || !stored.Status.EndTime.Time.Equal(clk.Now().UTC()) {
		t.Fatalf("endTime = %+v", stored.Status.EndTime)
	}
	condition := findCondition(t, stored.Status.Conditions, ConditionBuildSucceed)
	if condition.Status != metav1.ConditionFalse || condition.Reason != ReasonBuildFailed || condition.Message != ReasonBuildFailed {
		t.Fatalf("condition = %+v", condition)
	}
	if _, ok := testCondition(stored.Status.Conditions, ConditionPublishSucceed); ok {
		t.Fatal("a single build must not record a publish result")
	}
	if stored.Status.Repo != "" {
		t.Fatalf("repo = %q", stored.Status.Repo)
	}
	if api.CallCount("GetRpmRepo") != 0 || api.CallCount("CreateRpmRepo") != 0 {
		t.Fatal("a single build must never touch the RpmRepo of this round")
	}
}

func TestAggregateSpecStatusIsIndependentOfMapOrder(t *testing.T) {
	first := map[string]ebsv1.SpecStatus{}
	first["gcc"] = specStatus(specStatusSucceeded, specStatusSucceeded)
	first["glibc"] = specStatus("Failed", specStatusSucceeded)
	second := map[string]ebsv1.SpecStatus{}
	second["glibc"] = specStatus("Failed", specStatusSucceeded)
	second["gcc"] = specStatus(specStatusSucceeded, specStatusSucceeded)
	firstResult, firstSpec := aggregateSpecStatus(first)
	secondResult, secondSpec := aggregateSpecStatus(second)
	if firstResult != secondResult || firstSpec != secondSpec || firstResult || firstSpec != "glibc" {
		t.Fatalf("aggregation depends on map order: %v/%q vs %v/%q", firstResult, firstSpec, secondResult, secondSpec)
	}
}

func TestProcessingBuildNonSingleResults(t *testing.T) {
	for _, tc := range []struct {
		name          string
		statuses      map[string]ebsv1.SpecStatus
		publishFlag   bool
		wantPhase     ebsv1.BuildPhase
		wantStage     string
		wantEndTime   bool
		wantSucceeded bool
		wantPermanent bool
	}{
		{
			name: "success publishes", statuses: map[string]ebsv1.SpecStatus{"gcc": specStatus(specStatusSucceeded, specStatusSucceeded)},
			publishFlag: true, wantPhase: ebsv1.BuildProcessing, wantStage: stagePublish, wantSucceeded: true,
		},
		{
			name: "failure still publishes", statuses: map[string]ebsv1.SpecStatus{"gcc": specStatus("Failed", specStatusSucceeded)},
			publishFlag: true, wantPhase: ebsv1.BuildProcessing, wantStage: stagePublish,
		},
		{
			name: "success skips publishing", statuses: map[string]ebsv1.SpecStatus{"gcc": specStatus(specStatusSucceeded, specStatusSucceeded)},
			wantPhase: ebsv1.BuildSkipped, wantStage: stagePublish, wantEndTime: true, wantSucceeded: true,
		},
		{
			name: "failure skips publishing", statuses: map[string]ebsv1.SpecStatus{"gcc": specStatus(specStatusSucceeded, "Failed")},
			wantPhase: ebsv1.BuildSkipped, wantStage: stagePublish, wantEndTime: true, wantPermanent: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			build := withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildProcessing, stageBuild)
			build.Spec.BuildTarget.PublishFlag = tc.publishFlag
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
		{Phase: ebsv1.RpmRepoReleasePrepared},
	} {
		name := "missing"
		if release != nil {
			name = string(release.Phase)
		}
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI()
			api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildProcessing, stagePublish)
			api.rpmRepos[key("project-a", "build-a")] = &ebsv1.RpmRepo{
				ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a"},
				Status:     ebsv1.RpmRepoStatus{Release: release},
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

func TestProcessingPublishWaitsWhileRepositoryFailed(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildProcessing, stagePublish)
	api.rpmRepos[key("project-a", "build-a")] = &ebsv1.RpmRepo{
		ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a"},
		Status:     ebsv1.RpmRepoStatus{Repository: &ebsv1.RpmRepoRepositoryStatus{Phase: ebsv1.RpmRepoFailed}},
	}
	c := newTestController(t, api, newTestClock())
	result, err := c.sync(context.Background(), "project-a/build-a")
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if api.CallCount("UpdateBuildStatus") != 0 {
		t.Fatal("the process repository failure is not a release outcome")
	}
}

func TestProcessingPublishReadyWritesSuccess(t *testing.T) {
	api := newFakeAPI()
	build := withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildProcessing, stagePublish)
	build.Status.Conditions = []metav1.Condition{{
		Type: ConditionBuildSucceed, Status: metav1.ConditionTrue, Reason: ReasonBuildSucceeded,
		Message: ReasonBuildSucceeded, ObservedGeneration: build.Generation, LastTransitionTime: testTime(9),
	}}
	api.builds[key("project-a", "build-a")] = build
	api.rpmRepos[key("project-a", "build-a")] = &ebsv1.RpmRepo{
		ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a"},
		Status: ebsv1.RpmRepoStatus{
			Repository: &ebsv1.RpmRepoRepositoryStatus{Phase: ebsv1.RpmRepoReady, ContentURL: "https://internal.example.com/repo"},
			Release:    &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseReady, ContentURL: "https://release.example.com/project-a/aarch64"},
		},
	}
	clk := newTestClock()
	c := newTestController(t, api, clk)
	result, err := c.sync(context.Background(), "project-a/build-a")
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	stored := api.build("project-a", "build-a")
	if stored.Status.Phase != ebsv1.BuildSuccess || stored.Status.Stage != stagePublish {
		t.Fatalf("phase/stage = %q/%q", stored.Status.Phase, stored.Status.Stage)
	}
	if stored.Status.Repo != "https://release.example.com/project-a/aarch64" {
		t.Fatalf("repo = %q", stored.Status.Repo)
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

func TestProcessingPublishFailedKeepsRepositoryEmpty(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildProcessing, stagePublish)
	api.rpmRepos[key("project-a", "build-a")] = &ebsv1.RpmRepo{
		ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a"},
		Status: ebsv1.RpmRepoStatus{
			Release: &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseFailed, ContentURL: "https://release.example.com/stale"},
		},
	}
	clk := newTestClock()
	c := newTestController(t, api, clk)
	_, err := c.sync(context.Background(), "project-a/build-a")
	if !controller.IsPermanent(err) {
		t.Fatalf("want permanent error, got %v", err)
	}
	stored := api.build("project-a", "build-a")
	if stored.Status.Phase != ebsv1.BuildFailed || stored.Status.Stage != stagePublish {
		t.Fatalf("phase/stage = %q/%q", stored.Status.Phase, stored.Status.Stage)
	}
	if stored.Status.Repo != "" {
		t.Fatalf("repo = %q", stored.Status.Repo)
	}
	condition := findCondition(t, stored.Status.Conditions, ConditionPublishSucceed)
	if condition.Status != metav1.ConditionFalse || condition.Reason != ReasonPublishFailed {
		t.Fatalf("condition = %+v", condition)
	}
	assertConditionTime(t, condition, clk.Now().UTC(), stored.Status.EndTime)
}

func TestProcessingPublishFailsWhenRpmRepoIsMissing(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildProcessing, stagePublish)
	c := newTestController(t, api, newTestClock())
	_, err := c.sync(context.Background(), "project-a/build-a")
	if !controller.IsPermanent(err) {
		t.Fatalf("want permanent error, got %v", err)
	}
	stored := api.build("project-a", "build-a")
	if stored.Status.Phase != ebsv1.BuildFailed || stored.Status.Stage != stagePublish {
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
	api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildProcessing, stagePublish)
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

func testCondition(conditions []metav1.Condition, condType string) (metav1.Condition, bool) {
	for _, condition := range conditions {
		if condition.Type == condType {
			return condition, true
		}
	}
	return metav1.Condition{}, false
}

// assertConditionTime checks that a freshly written condition carries the round's injected clock value, that
// the round reused a single now for every new timestamp, and that the condition persists as UTC. metav1.Time
// restores local time when it is read back (its UnmarshalJSON calls Local), so UTC is asserted on the
// serialized form instead of the in-memory location.
func assertConditionTime(t *testing.T, condition metav1.Condition, now time.Time, endTime metav1.Time) {
	t.Helper()
	if !condition.LastTransitionTime.Time.Equal(now) {
		t.Fatalf("lastTransitionTime = %+v, want %v", condition.LastTransitionTime, now)
	}
	if !condition.LastTransitionTime.Time.Equal(endTime.Time) {
		t.Fatalf("one round must reuse a single now: condition=%+v endTime=%+v", condition.LastTransitionTime, endTime)
	}
	encoded, err := condition.LastTransitionTime.MarshalJSON()
	if err != nil || !strings.HasSuffix(string(encoded), `Z"`) {
		t.Fatalf("persisted lastTransitionTime must be UTC: %s (err=%v)", encoded, err)
	}
}
