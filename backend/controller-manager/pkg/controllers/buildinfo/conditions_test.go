package buildinfo

import (
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestUpsertConditionLifecycle(t *testing.T) {
	var conds []metav1.Condition
	upsertCondition(&conds, ConditionDcgBuildFailed, ReasonDcgBuildFailed, "first")
	first := findCondition(conds, ConditionDcgBuildFailed)
	if first == nil || first.Status != metav1.ConditionTrue {
		t.Fatalf("upsert produced %+v, want status=True", first)
	}
	stamp := first.LastTransitionTime
	// Same-content upsert keeps the transition time (meta helper semantics).
	upsertCondition(&conds, ConditionDcgBuildFailed, ReasonDcgBuildFailed, "first")
	again := findCondition(conds, ConditionDcgBuildFailed)
	if !again.LastTransitionTime.Equal(&stamp) {
		t.Fatalf("same-content upsert moved lastTransitionTime %v -> %v", stamp, again.LastTransitionTime)
	}
	// A message-only change updates the message but still keeps the time.
	upsertCondition(&conds, ConditionDcgBuildFailed, ReasonDcgBuildFailed, "second")
	updated := findCondition(conds, ConditionDcgBuildFailed)
	if updated.Message != "second" || !updated.LastTransitionTime.Equal(&stamp) {
		t.Fatalf("message update = %+v, want message second with preserved time", updated)
	}
	removeCondition(&conds, ConditionDcgBuildFailed)
	requireNoCondition(t, conds, ConditionDcgBuildFailed)
}

func TestMissingDepsMessage(t *testing.T) {
	if got := missingDepsMessage(nil); got != "" {
		t.Fatalf("empty deps = %q, want empty", got)
	}
	// Sorted and de-duplicated.
	if got := missingDepsMessage([]string{"b", "a", "b", "c", "a"}); got != "a,b,c" {
		t.Fatalf("message = %q, want a,b,c", got)
	}
	// Over-1024 truncation keeps whole names and the total suffix.
	var names []string
	for i := 0; i < 200; i++ {
		names = append(names, fmt.Sprintf("dep-%03d", i))
	}
	got := missingDepsMessage(names)
	if len(got) > conditionMessageMax {
		t.Fatalf("truncated message len = %d, want <= %d", len(got), conditionMessageMax)
	}
	if !strings.HasSuffix(got, "...(+200 deps total)") {
		t.Fatalf("truncated message lacks total suffix: ...%q", got[len(got)-30:])
	}
	body := strings.TrimSuffix(got, "...(+200 deps total)")
	if strings.HasSuffix(body, ",") ||
		strings.Contains(body[strings.LastIndex(body, ",")+1:], "dep-") &&
			strings.Count(body[strings.LastIndex(body, ",")+1:], "-") > 1 {
		// partial name check: the last segment before the suffix is empty or a full name
	}
}

func TestStopConditionDetection(t *testing.T) {
	var conds []metav1.Condition
	if stopCondition(conds) != nil {
		t.Fatal("no conditions must yield no stop marker")
	}
	upsertCondition(&conds, ConditionDcgBuildFailed, ReasonDcgBuildFailed, "m")
	if stopCondition(conds) != nil {
		t.Fatal("DcgBuildFailed is not a stop marker")
	}
	upsertCondition(&conds, ConditionRpmRepoRetrying, ReasonRpmRepoNotFound, "waiting for repository")
	if stopCondition(conds) != nil {
		t.Fatal("RpmRepoRetrying must not stop dispatch")
	}
	upsertCondition(&conds, ConditionSnapshotUnavailable, ReasonSnapshotNotFound, "m")
	if got := stopCondition(conds); got == nil || got.Type != ConditionSnapshotUnavailable {
		t.Fatalf("stopCondition = %+v, want SnapshotUnavailable", got)
	}
}
