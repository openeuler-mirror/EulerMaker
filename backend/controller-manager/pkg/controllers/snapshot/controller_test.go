package snapshot

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"

	ebsv1 "ebs-api/ebs/v1"
)

func TestPollingUpdateWithSameResourceVersionEnqueues(t *testing.T) {
	snapshot, build := baseObjects(nil)
	api := &fakeClient{snapshot: snapshot, build: build}
	s := &fakeSource{}
	c, err := New(s, api, &fakeServices{git: &fakeGitClient{}}, clock.RealClock{}, Config{PollPeriod: time.Second, ResolveWorkers: 1, ResolveBudget: time.Minute, SyncRequeueDelay: time.Second, FailureLimit: 1, MaxRetries: 2})
	if err != nil {
		t.Fatal(err)
	}
	projected := snapshot.DeepCopy()
	projected.Status = ebsv1.SnapshotStatus{} // includeFields=metadata omits phase.
	s.handler.OnUpdate(projected.DeepCopy(), projected)
	if c.Queue().Len() != 1 {
		t.Fatalf("queue length=%d, want 1", c.Queue().Len())
	}
}

func TestPollingDeletionClearsTracker(t *testing.T) {
	for _, event := range []string{"add-deleting", "update-deleting", "delete"} {
		t.Run(event, func(t *testing.T) {
			snapshot, build := baseObjects(nil)
			api := &fakeClient{snapshot: snapshot, build: build}
			s := &fakeSource{}
			c, err := New(s, api, &fakeServices{git: &fakeGitClient{}}, clock.RealClock{}, Config{
				PollPeriod: time.Second, ResolveWorkers: 1, ResolveBudget: time.Minute,
				SyncRequeueDelay: time.Second, FailureLimit: 1, MaxRetries: 2,
			})
			if err != nil {
				t.Fatal(err)
			}
			c.failures.observe("project-a/build-a", snapshot.UID)
			c.failures.set(snapshot.UID, "pkg", 2)
			c.failures.setCursor(snapshot.UID, 1)
			projected := snapshot.DeepCopy()
			projected.Status = ebsv1.SnapshotStatus{}
			if event == "delete" {
				s.handler.OnDelete(projected)
			} else {
				now := metav1.Now()
				projected.DeletionTimestamp = &now
				if event == "add-deleting" {
					s.handler.OnAdd(projected)
				} else {
					s.handler.OnUpdate(snapshot, projected)
				}
			}
			assertTrackerCleared(t, c.failures, snapshot.UID)
			if c.Queue().Len() != 0 {
				t.Fatalf("deleted Snapshot queued, length=%d", c.Queue().Len())
			}
		})
	}
}

func TestOldSnapshotDeletionPreservesReplacementTracker(t *testing.T) {
	snapshot, build := baseObjects(nil)
	c := newTestController(t, &fakeClient{snapshot: snapshot, build: build}, &fakeGitClient{}, Config{})
	c.failures.observe("project-a/build-a", snapshot.UID)
	c.failures.set(snapshot.UID, "pkg", 2)
	c.failures.observe("project-a/build-a", "replacement-uid")
	c.failures.set("replacement-uid", "pkg", 1)
	c.failures.setCursor("replacement-uid", 3)
	c.onDelete(snapshot)
	assertTrackerCleared(t, c.failures, snapshot.UID)
	if c.failures.count("replacement-uid", "pkg") != 1 || c.failures.cursor("replacement-uid") != 3 {
		t.Fatal("stale deletion cleared the replacement's tracker")
	}
}
