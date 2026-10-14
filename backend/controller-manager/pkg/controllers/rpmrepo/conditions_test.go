package rpmrepo

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ebsv1 "ebs-api/ebs/v1"
)

func TestMergeConditionSemantics(t *testing.T) {
	first := metav1.NewTime(time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC))
	later := metav1.NewTime(first.Add(time.Hour))

	conditions, changed := MergeCondition(nil, ebsv1.RpmRepoConditionRepositoryReady, metav1.ConditionTrue, ebsv1.RpmRepoReasonRepositoryCreated, "", 3, first)
	if !changed || len(conditions) != 1 {
		t.Fatalf("first insert must report a change: %+v", conditions)
	}
	if !conditions[0].LastTransitionTime.Equal(&first) || conditions[0].ObservedGeneration != 3 {
		t.Fatalf("first insert must use the supplied now and generation: %+v", conditions[0])
	}

	same, changed := MergeCondition(conditions, ebsv1.RpmRepoConditionRepositoryReady, metav1.ConditionTrue, ebsv1.RpmRepoReasonRepositoryCreated, "", 3, later)
	if changed {
		t.Fatalf("an identical condition must not report a change")
	}
	if !same[0].LastTransitionTime.Equal(&first) {
		t.Fatalf("an unchanged condition must keep its transition time: %+v", same[0])
	}

	reasonOnly, changed := MergeCondition(same, ebsv1.RpmRepoConditionRepositoryReady, metav1.ConditionTrue, "OtherReason", "", 3, later)
	if !changed {
		t.Fatalf("a reason change must report a change")
	}
	if !reasonOnly[0].LastTransitionTime.Equal(&first) {
		t.Fatalf("a reason-only change must keep the transition time: %+v", reasonOnly[0])
	}

	statusChanged, changed := MergeCondition(reasonOnly, ebsv1.RpmRepoConditionRepositoryReady, metav1.ConditionFalse, ebsv1.RpmRepoReasonRepositoryCreationFailed, "", 4, later)
	if !changed || !statusChanged[0].LastTransitionTime.Equal(&later) || statusChanged[0].ObservedGeneration != 4 {
		t.Fatalf("a status change must move the transition time and generation: %+v", statusChanged[0])
	}

	sorted, _ := MergeCondition(statusChanged, ebsv1.RpmRepoConditionPublishSucceed, metav1.ConditionTrue, ebsv1.RpmRepoReasonReleaseActivated, "", 4, later)
	if len(sorted) != 2 || sorted[0].Type != ebsv1.RpmRepoConditionPublishSucceed || sorted[1].Type != ebsv1.RpmRepoConditionRepositoryReady {
		t.Fatalf("conditions must be sorted by type: %+v", sorted)
	}
}

func TestBackoffDelayGrowsExponentiallyAndCaps(t *testing.T) {
	r := &reconciler{controller: &Controller{config: testConfig()}}
	r.controller.config.Backoff = Backoff{Initial: time.Second, Max: 4 * time.Second, Jitter: 0}
	for _, tc := range []struct {
		attempt int
		want    time.Duration
	}{
		{0, time.Second},
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 4 * time.Second},
		{9, 4 * time.Second},
	} {
		if got := r.backoffDelay(tc.attempt); got != tc.want {
			t.Fatalf("backoffDelay(%d) = %s, want %s", tc.attempt, got, tc.want)
		}
	}
}

func TestWindowRemainingUsesTheResponseAnchor(t *testing.T) {
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	if got := windowRemaining(time.Time{}, time.Second, now); got != 0 {
		t.Fatalf("a missing anchor must report no remaining window, got %s", got)
	}
	if got := windowRemaining(now.Add(-500*time.Millisecond), time.Second, now); got != 500*time.Millisecond {
		t.Fatalf("remaining window = %s, want 500ms", got)
	}
	if got := windowRemaining(now.Add(-2*time.Second), time.Second, now); got != 0 {
		t.Fatalf("an elapsed window must report zero, got %s", got)
	}
}

func TestNormalizeExcludeSpecsDeduplicatesAndSorts(t *testing.T) {
	got := normalizeExcludeSpecs([]string{"gcc", "kernel", "gcc", ""})
	if len(got) != 2 || got[0] != "gcc" || got[1] != "kernel" {
		t.Fatalf("normalizeExcludeSpecs = %v, want [gcc kernel]", got)
	}
	if normalizeExcludeSpecs(nil) != nil {
		t.Fatalf("an empty decision must stay nil")
	}
}

func TestDefaultPublishPolicyFollowsPublishFlag(t *testing.T) {
	policy := DefaultPublishPolicy{}
	if _, err := policy.Decide(context.Background(), PublishPolicyInput{}); err == nil {
		t.Fatalf("a missing Build must be rejected")
	}
	for _, publish := range []bool{true, false} {
		build := newBuild(testBuild)
		build.Spec.BuildTarget.PublishFlag = publish
		decision, err := policy.Decide(context.Background(), PublishPolicyInput{Project: testProject, Build: build})
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if decision.Publish != publish || len(decision.ExcludeSpecs) != 0 {
			t.Fatalf("unexpected decision %+v for publishFlag=%t", decision, publish)
		}
	}
}

func TestFakesRecordCallsAndReset(t *testing.T) {
	client := NewFakeClient()
	client.RpmRepos[key(testProject, testBuild)] = newRpmRepo(testBuild)
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	if _, err := client.GetBuild(context.Background(), testProject, testBuild); err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if _, err := client.GetRpmRepo(context.Background(), testProject, testBuild); err != nil {
		t.Fatalf("GetRpmRepo: %v", err)
	}
	if client.GetBuildCalls != 1 || client.GetRpmRepoCalls != 1 {
		t.Fatalf("calls were not recorded: %+v", client)
	}
	client.Reset()
	if client.GetBuildCalls != 0 || client.GetRpmRepoCalls != 0 || len(client.StatusWrites) != 0 {
		t.Fatalf("Reset must clear the recorded calls")
	}
	if len(client.RpmRepos) != 1 {
		t.Fatalf("Reset must keep the preset objects")
	}

	artifacts := NewFakeArtifactManager()
	artifacts.GetRepositoryFunc = func(context.Context, string) (RepositoryResponse, error) {
		return RepositoryResponse{}, nil
	}
	if _, err := artifacts.GetRepository(context.Background(), "repo-1"); err != nil {
		t.Fatalf("GetRepository: %v", err)
	}
	if len(artifacts.GetRepositoryUIDs) != 1 {
		t.Fatalf("the fake must record the request")
	}
	artifacts.Reset()
	if len(artifacts.GetRepositoryUIDs) != 0 {
		t.Fatalf("Reset must clear the recorded calls")
	}
	if artifacts.GetRepositoryFunc == nil {
		t.Fatalf("Reset must keep the scripted responses")
	}
}

func TestMetricsAreRegistered(t *testing.T) {
	for name, counter := range map[string]interface{ Value() uint64 }{
		"repository_ready":    repositoryReady,
		"repository_failed":   repositoryFailed,
		"materialize_retries": materializeRetries,
		"build_missing":       buildMissing,
		"release_ready":       releaseReady,
		"release_failed":      releaseFailed,
		"status_conflicts":    statusUpdateConflicts,
		"status_unknowns":     statusUpdateUnknowns,
	} {
		if counter == nil {
			t.Fatalf("metric %s is not registered", name)
		}
		_ = counter.Value()
	}
}
