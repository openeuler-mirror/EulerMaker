package rpmrepo

import (
	"context"
	"testing"
)

func TestNormalizeExcludeSpecsDeduplicatesAndSorts(t *testing.T) {
	got := normalizeExcludeSpecs([]string{"gcc", "kernel", "gcc", ""})
	if len(got) != 2 || got[0] != "gcc" || got[1] != "kernel" {
		t.Fatalf("normalizeExcludeSpecs = %v, want [gcc kernel]", got)
	}
	if normalizeExcludeSpecs(nil) != nil {
		t.Fatalf("an empty decision must stay nil")
	}
}

func TestDefaultPublishPolicyFollowsPublish(t *testing.T) {
	policy := DefaultPublishPolicy{}
	if _, err := policy.Decide(context.Background(), PublishPolicyInput{}); err == nil {
		t.Fatalf("a missing Build must be rejected")
	}
	for _, publish := range []bool{true, false} {
		build := newBuild(testBuild)
		build.Spec.Publish = &publish
		decision, err := policy.Decide(context.Background(), PublishPolicyInput{Project: testProject, Build: build})
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if decision.Publish != publish || len(decision.ExcludeSpecs) != 0 {
			t.Fatalf("unexpected decision %+v for publish=%t", decision, publish)
		}
	}
}
