package snapshot

import (
	"testing"
)

func TestTrackerLifecycle(t *testing.T) {
	tracker := newFailureTracker()
	tracker.observe("p/s", "old")
	tracker.set("old", "pkg", 2)
	tracker.setCursor("old", 42)
	tracker.observe("p/s", "new")
	tracker.setCursor("old", 43) // Late result must not resurrect old UID state.
	tracker.set("old", "pkg", 3)
	assertTrackerCleared(t, tracker, "old")
	tracker.set("new", "pkg", 1)
	tracker.setCursor("new", 2)
	if tracker.count("new", "pkg") != 1 || tracker.cursor("new") != 2 {
		t.Fatal("current UID state was not recorded")
	}
	tracker.clearObject("p/s")
	tracker.set("new", "pkg", 2)
	tracker.setCursor("new", 3)
	assertTrackerCleared(t, tracker, "new")
	tracker.observe("p/s", "new")
	tracker.set("new", "pkg", 1)
	tracker.setCursor("new", 3)
	tracker.clearUID("new")
	tracker.set("new", "pkg", 2)
	tracker.setCursor("new", 4)
	assertTrackerCleared(t, tracker, "new")
}
