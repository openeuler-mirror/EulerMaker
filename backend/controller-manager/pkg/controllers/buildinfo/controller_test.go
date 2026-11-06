package buildinfo

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/manager"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

// --- Event handlers ---

type recordingPollingFactory struct {
	selectors []string
	gvr       schema.GroupVersionResource
	options   metav1.ListOptions
}

func (f *recordingPollingFactory) ForResource(gvr schema.GroupVersionResource, _ time.Duration, options metav1.ListOptions) (source.Source, error) {
	f.gvr, f.options = gvr, options
	f.selectors = append(f.selectors, options.FieldSelector)
	return &fakeSource{}, nil
}

func (*recordingPollingFactory) Sources() []source.Source { return nil }

func TestInitializerFiltersTerminalBuildInfos(t *testing.T) {
	polling := &recordingPollingFactory{}
	config := Config{
		PollPeriod: time.Second, MaxRetries: 2, DcgPruneGrace: time.Minute,
		RpmRepoReadyRetryLimit: 3, SnapshotReadyRetryLimit: 3, SpecFileCacheSize: 100,
	}
	init := manager.InitContext{
		Dependencies: manager.Dependencies{Client: &clientpkg.Client{}, PollingFactory: polling},
		Config:       manager.ControllerConfig{SlowRetryInitial: time.Second, SlowRetryMax: time.Minute},
	}
	_, active, err := Initializer(config, &fakeServices{fakeClient: newFakeClient(), git: newFakeGitServer()})(context.Background(), init)
	if err != nil || !active {
		t.Fatalf("initializer active=%t err=%v", active, err)
	}
	if polling.gvr != source.BuildInfosGVR || len(polling.selectors) != 2 || polling.selectors[0] != nonTerminalBuildInfoFieldSelector || polling.selectors[1] != "metadata.deletionTimestamp!=null" {
		t.Fatalf("polling source=%s options=%#v", polling.gvr, polling.options)
	}
}

func TestEventAddUpdateEnqueueFilteredListObjects(t *testing.T) {
	c, _, _, _ := newTestController(t)
	c.onAdd(testBuildInfoObj(ebsv1.BuildInfoPending))
	c.onAdd(testBuildInfoObj(ebsv1.BuildInfoProcessing))
	if got := c.Queue().Len(); got != 1 {
		t.Fatalf("queue len after add pending+processing same key = %d, want 1 (dedup)", got)
	}
	c.onUpdate(nil, testBuildInfoObj(ebsv1.BuildInfoPending))
	if got := c.Queue().Len(); got != 1 {
		t.Fatalf("queue len after update = %d, want 1", got)
	}
	// The projected List does not carry phase; the server-side selector has already excluded terminal objects before the
	// handler sees them.
	c2, _, _, _ := newTestController(t)
	projected := testBuildInfoObj(ebsv1.BuildInfoPending)
	projected.Status = ebsv1.BuildInfoStatus{}
	c2.onAdd(projected)
	if got := c2.Queue().Len(); got != 1 {
		t.Fatalf("projected buildinfo queue len = %d, want 1", got)
	}
}

func TestEventWrongTypeNoPanic(t *testing.T) {
	c, _, _, _ := newTestController(t)
	c.onAdd(testBuildObj("full"))
	c.onUpdate(nil, testBuildObj("full"))
	c.onDelete(testBuildObj("full"))
	if got := c.Queue().Len(); got != 0 {
		t.Fatalf("wrong-type object enqueued, queue len = %d", got)
	}
}

func TestEventDeleteTombstoneAndRevoke(t *testing.T) {
	c, _, _, _ := newTestController(t)
	key := testNS + "/" + testBuild
	c.dcgDict.Set(key, &graphCacheEntry{dcg: NewDcgDict(graphWith(edge{"a", "b"}))})
	if got := c.dcgDict.Len(); got != 1 {
		t.Fatalf("dcgDict len = %d, want 1", got)
	}
	c.onDelete(testBuildInfoObj(ebsv1.BuildInfoPending))
	if got := c.dcgDict.Len(); got != 0 {
		t.Fatalf("dcgDict len after delete = %d, want 0 (tombstoned)", got)
	}
	// Re-add within the grace period revokes the tombstone.
	c.onAdd(testBuildInfoObj(ebsv1.BuildInfoPending))
	if got := c.dcgDict.Len(); got != 1 {
		t.Fatalf("dcgDict len after re-add = %d, want 1 (revoked)", got)
	}
}

func TestRoundFailuresBumpOncePerRound(t *testing.T) {
	c, _, _, _ := newTestController(t)
	key := testNS + "/" + testBuild
	rf := c.newRoundFailures(key)
	count, escalated := rf.RpmRepoFailed("r1", "m1")
	if count != 1 || escalated {
		t.Fatalf("first failure = %d,%v, want 1,false", count, escalated)
	}
	// Second checkpoint of the same round does not bump again.
	count, escalated = rf.RpmRepoFailed("r2", "m2")
	if count != 1 || escalated {
		t.Fatalf("same-round second failure = %d,%v, want 1,false", count, escalated)
	}
	// The first failure checkpoint is kept for the escalation message.
	entry := c.counters.Entry(counterRpmRepo, key)
	if entry.LastReason != "r1" || entry.LastMessage != "m1" {
		t.Fatalf("checkpoint = %q/%q, want r1/m1", entry.LastReason, entry.LastMessage)
	}
	// Next round bumps to the threshold and escalates (limit 3).
	for i := 0; i < 2; i++ {
		rf = c.newRoundFailures(key)
		_, escalated = rf.RpmRepoFailed("r1", "m1")
	}
	if !escalated {
		t.Fatal("third consecutive failure must escalate")
	}
	rf.RpmRepoReady()
	if got := c.counters.Count(counterRpmRepo, key); got != 0 {
		t.Fatalf("counter after ready = %d, want 0", got)
	}
}
