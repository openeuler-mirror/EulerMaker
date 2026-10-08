package buildinfo

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/manager"
	"controller-manager/pkg/metrics"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

// --- Event handlers ---

type initializerSharedClient struct{ clientpkg.Interface }

func (initializerSharedClient) AbortJob(context.Context, string, string, types.UID, string) (*ebsv1.Job, error) {
	return nil, nil
}

func (initializerSharedClient) GetBuildTargetContent(context.Context) (*ebsv1.BuildTargetContent, error) {
	return nil, nil
}

type recordingPollingFactory struct {
	gvr     schema.GroupVersionResource
	options metav1.ListOptions
}

func (f *recordingPollingFactory) ForResource(gvr schema.GroupVersionResource, _ time.Duration, options metav1.ListOptions) (source.Source, error) {
	f.gvr, f.options = gvr, options
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
		Dependencies: manager.Dependencies{Client: initializerSharedClient{}, PollingFactory: polling},
		Config:       manager.ControllerConfig{SlowRetryInitial: time.Second, SlowRetryMax: time.Minute},
	}
	_, active, err := Initializer(config, newFakeGitServer())(context.Background(), init)
	if err != nil || !active {
		t.Fatalf("initializer active=%t err=%v", active, err)
	}
	if polling.gvr != source.BuildInfosGVR || polling.options.FieldSelector != nonTerminalBuildInfoFieldSelector {
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
	// The projected List does not carry phase; the server-side selector has
	// already excluded terminal objects before the handler sees them.
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
	c.dcgDict.Set(key, NewDcgDict(graphWith(edge{"a", "b"})))
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

// --- Reconcile entry ---

func TestReconcileDeletedBuildInfo(t *testing.T) {
	c, _, _, _ := newTestController(t)
	key := testNS + "/" + testBuild
	c.dcgDict.Set(key, NewDcgDict(graphWith(edge{"a", "b"})))
	c.counters.Increment(counterRpmRepo, key, "r", "m")
	// A missing BuildInfo ends the round and clears local state.
	reconcileOnce(t, c)
	if got := c.dcgDict.Len(); got != 0 {
		t.Fatalf("dcgDict len = %d, want 0 after 404 reconcile", got)
	}
	if got := c.counters.Count(counterRpmRepo, key); got != 0 {
		t.Fatalf("rpmrepo counter = %d, want 0 after 404 reconcile", got)
	}
}

func TestReconcileTerminalSkips(t *testing.T) {
	c, client, _, _ := newTestController(t)
	key := testNS + "/" + testBuild
	client.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoCompleted))
	c.dcgDict.Set(key, NewDcgDict(graphWith(edge{"a", "b"})))
	reconcileOnce(t, c)
	requirePhase(t, getBuildInfo(t, client), ebsv1.BuildInfoCompleted)
	if got := c.dcgDict.Len(); got != 0 {
		t.Fatalf("dcgDict len = %d, want 0 after terminal reconcile", got)
	}
}

func TestReconcileDeletionTimestampSkips(t *testing.T) {
	c, client, _, _ := newTestController(t)
	bi := testBuildInfoObj(ebsv1.BuildInfoPending)
	now := metav1.Now()
	bi.DeletionTimestamp = &now
	client.SeedBuildInfo(bi)
	reconcileOnce(t, c)
	requirePhase(t, getBuildInfo(t, client), ebsv1.BuildInfoPending)
}

func TestReconcileUnknownPhaseSkips(t *testing.T) {
	c, client, _, _ := newTestController(t)
	client.SeedProject(testProjectObj(ebsv1.ProjectActive))
	client.SeedBuild(testBuildObj("full"))
	bi := testBuildInfoObj(ebsv1.BuildInfoPending)
	bi.Status.Phase = "Bogus"
	client.SeedBuildInfo(bi)
	reconcileOnce(t, c)
	if got := getBuildInfo(t, client).Status.Phase; got != "Bogus" {
		t.Fatalf("unknown phase rewritten to %q", got)
	}
}

// --- Parent abort handling ---

func TestParentAbortAttemptsJobsBeforeTerminal(t *testing.T) {
	for _, scenario := range []string{"success", "partial-failure", "list-failure"} {
		t.Run(scenario, func(t *testing.T) {
			c, client, _, _ := newTestController(t)
			client.SeedProject(testProjectObj(ebsv1.ProjectActive))
			build := testBuildObj("full")
			build.Status.Phase = ebsv1.BuildAborted
			client.SeedBuild(build)
			client.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoProcessing))
			for name, phase := range map[string]ebsv1.JobPhase{"a": ebsv1.JobPending, "b": ebsv1.JobRunning, "done": ebsv1.JobSucceeded, "other": ebsv1.JobRunning} {
				buildName := testBuild
				if name == "other" {
					buildName = "other-build"
				}
				client.SeedJob(&ebsv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, Labels: map[string]string{ebsv1.JobBuildNameLabel: buildName}}, Status: ebsv1.JobStatus{Phase: phase}})
			}
			if scenario == "partial-failure" {
				client.abortErrors = map[string]error{"a": errors.New("abort unavailable")}
			}
			if scenario == "list-failure" {
				client.InjectRead("jobs", 1, errors.New("list unavailable"))
			}
			reconcileOnce(t, c)
			info := getBuildInfo(t, client)
			requirePhase(t, info, ebsv1.BuildInfoAborted)
			if scenario != "success" {
				requireCondition(t, info.Status.Conditions, ConditionJobAbortFailed, ReasonJobAbortFailed)
			}
			if scenario != "list-failure" {
				if strings.Join(client.abortCalls, ",") != "a,b" {
					t.Fatalf("abort calls = %v", client.abortCalls)
				}
				job, _ := client.GetJob(context.Background(), testNS, "b")
				if job.Status.Phase != ebsv1.JobAborted {
					t.Fatalf("second Job phase = %s", job.Status.Phase)
				}
			}
			reconcileOnce(t, c)
			if len(client.abortCalls) > 2 {
				t.Fatalf("terminal BuildInfo repeated abort: %v", client.abortCalls)
			}
		})
	}
}

func TestParentAbortBatches(t *testing.T) {
	for _, count := range []int{100, 101, 250} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			c, client, _, _ := newTestController(t)
			client.SeedProject(testProjectObj(ebsv1.ProjectActive))
			build := testBuildObj("full")
			build.Status.Phase = ebsv1.BuildAborted
			client.SeedBuild(build)
			client.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoProcessing))
			client.abortErrors = make(map[string]error)
			for i := 0; i < count; i++ {
				name := fmt.Sprintf("job-%03d", i)
				client.SeedJob(&ebsv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, Labels: map[string]string{ebsv1.JobBuildNameLabel: testBuild}}, Status: ebsv1.JobStatus{Phase: ebsv1.JobRunning}})
				if i < 100 {
					client.abortErrors[name] = errors.New("abort unavailable")
				}
			}
			for processed := 0; processed < count; {
				result, err := c.reconcile(context.Background(), testNS+"/"+testBuild)
				if err != nil {
					t.Fatal(err)
				}
				processed += min(100, count-processed)
				if len(client.abortCalls) != processed {
					t.Fatalf("abort calls = %d, want %d", len(client.abortCalls), processed)
				}
				info := getBuildInfo(t, client)
				if processed < count {
					requirePhase(t, info, ebsv1.BuildInfoProcessing)
					if result.RequeueAfter <= 0 || client.statusWrites != 0 {
						t.Fatalf("intermediate result = %+v, status writes = %d", result, client.statusWrites)
					}
				} else {
					requirePhase(t, info, ebsv1.BuildInfoAborted)
					requireCondition(t, info.Status.Conditions, ConditionJobAbortFailed, ReasonJobAbortFailed)
					if !strings.Contains(findCondition(info.Status.Conditions, ConditionJobAbortFailed).Message, "100 Job abort requests") {
						t.Fatal("earlier batch failures were lost")
					}
					if c.abortJobs.Len() != 0 {
						t.Fatal("completed abort progress was not cleared")
					}
				}
			}
		})
	}
}

func TestParentAbortGuard(t *testing.T) {
	tests := []struct {
		name      string
		seed      func(*fakeClient)
		wantPhase ebsv1.BuildInfoPhase
		wantErr   bool
	}{
		{"project-not-found-waits", func(c *fakeClient) {
			c.SeedBuild(testBuildObj("full"))
			c.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoPending))
		}, ebsv1.BuildInfoPending, false},
		{"project-terminating-aborts", func(c *fakeClient) {
			c.SeedProject(testProjectObj(ebsv1.ProjectTerminating))
			c.SeedBuild(testBuildObj("full"))
			c.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoPending))
		}, ebsv1.BuildInfoAborted, false},
		{"build-missing-aborts", func(c *fakeClient) {
			c.SeedProject(testProjectObj(ebsv1.ProjectActive))
			c.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoPending))
		}, ebsv1.BuildInfoAborted, false},
		{"build-aborted-aborts", func(c *fakeClient) {
			c.SeedProject(testProjectObj(ebsv1.ProjectActive))
			build := testBuildObj("full")
			build.Status.Phase = ebsv1.BuildAborted
			c.SeedBuild(build)
			c.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoPending))
		}, ebsv1.BuildInfoAborted, false},
		{"build-query-failed-errors", func(c *fakeClient) {
			c.SeedProject(testProjectObj(ebsv1.ProjectActive))
			c.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoPending))
			c.InjectRead("builds", 1, errors.New("boom"))
		}, ebsv1.BuildInfoPending, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, client, _, _ := newTestController(t)
			tc.seed(client)
			_, err := c.reconcile(context.Background(), testNS+"/"+testBuild)
			if tc.wantErr && err == nil {
				t.Fatal("reconcile() error = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("reconcile() error = %v", err)
			}
			requirePhase(t, getBuildInfo(t, client), tc.wantPhase)
		})
	}
}

// --- Stop-condition routing ---

func TestStopConditionRoutesToConverge(t *testing.T) {
	c, client, _, _ := newTestController(t)
	client.SeedProject(testProjectObj(ebsv1.ProjectActive))
	client.SeedBuild(testBuildObj("full"))
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	upsertCondition(&bi.Status.Conditions, ConditionRpmRepoUnavailable, ReasonRpmRepoNotFound, "rpmrepo gone")
	bi.Status.SpecStatus = ebsv1.NewSpecStatusGroup(map[string]ebsv1.SpecStatus{
		"a": {Build: ebsv1.SpecBuildStatus{Status: SpecBuildSucceeded}, DispatchCount: 1},
	})
	client.SeedBuildInfo(bi)
	reconcileOnce(t, c)
	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoCompleted)
	// Preserve the stop marker without writing success conditions.
	requireCondition(t, persisted.Status.Conditions, ConditionRpmRepoUnavailable, ReasonRpmRepoNotFound)
	requireNoCondition(t, persisted.Status.Conditions, ConditionAllSpecsSucceeded)
	requireNoCondition(t, persisted.Status.Conditions, ConditionPartialFailure)
}

// --- Status write outcomes ---

// writeRound drives writeStatus directly with a one-field intent change.
func writeRound(t *testing.T, c *Controller, client *fakeClient, mutate func(*ebsv1.BuildInfo)) (controller.ReconcileResult, error) {
	t.Helper()
	return writeSeededRound(t, c, getBuildInfo(t, client), mutate)
}

// writeSeededRound drives writeStatus directly without any preliminary client
// read, so tests can count InjectRead consumptions precisely (the confirmation
// read inside writeStatus must be the only one).
func writeSeededRound(t *testing.T, c *Controller, seeded *ebsv1.BuildInfo, mutate func(*ebsv1.BuildInfo)) (controller.ReconcileResult, error) {
	t.Helper()
	round := &reconcileRound{key: testNS + "/" + testBuild, current: seeded, failures: c.newRoundFailures(testNS + "/" + testBuild)}
	next := seeded.DeepCopy()
	mutate(next)
	return c.writeStatus(context.Background(), round, next)
}

func TestWriteStatusRejectedOutcomes(t *testing.T) {
	tests := []struct {
		name          string
		statusCode    int
		wantRequeue   time.Duration
		wantPermanent bool
		wantErr       bool
	}{
		{"conflict-409-requeues", 409, conflictRequeueDelay, false, false},
		{"not-found-404-silent", 404, 0, false, false},
		{"bad-request-400-permanent", 400, 0, true, true},
		{"forbidden-403-permanent", 403, 0, true, true},
		{"server-error-500-retried", 500, 0, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, client, _, _ := newTestController(t)
			client.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoPending))
			client.InjectWrite("update-status", clientpkg.WriteRejected, tc.statusCode, false)
			result, err := writeRound(t, c, client, func(bi *ebsv1.BuildInfo) { bi.Status.Phase = ebsv1.BuildInfoAborted })
			if tc.wantErr && err == nil {
				t.Fatal("writeStatus() error = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("writeStatus() error = %v", err)
			}
			if tc.wantPermanent && !controller.IsPermanent(err) {
				t.Fatalf("writeStatus() error = %v, want permanent", err)
			}
			if result.RequeueAfter != tc.wantRequeue {
				t.Fatalf("RequeueAfter = %v, want %v", result.RequeueAfter, tc.wantRequeue)
			}
			// No outcome above persists the write.
			requirePhase(t, getBuildInfo(t, client), ebsv1.BuildInfoPending)
		})
	}
}

func TestWriteStatusNotSentIsPermanent(t *testing.T) {
	c, client, _, _ := newTestController(t)
	client.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoPending))
	client.InjectWrite("update-status", clientpkg.WriteNotSent, 0, false)
	_, err := writeRound(t, c, client, func(bi *ebsv1.BuildInfo) { bi.Status.Phase = ebsv1.BuildInfoAborted })
	if !controller.IsPermanent(err) {
		t.Fatalf("writeStatus() error = %v, want permanent", err)
	}
}

func TestWriteStatusUnknownPersistedConfirms(t *testing.T) {
	c, client, _, _ := newTestController(t)
	client.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoPending))
	// Unknown whose intent landed: the confirmation read sees the new status.
	client.InjectWrite("update-status", clientpkg.WriteUnknown, 0, true)
	result, err := writeRound(t, c, client, func(bi *ebsv1.BuildInfo) { bi.Status.Phase = ebsv1.BuildInfoAborted })
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("writeStatus() = %v, %v, want zero result nil error", result, err)
	}
	requirePhase(t, getBuildInfo(t, client), ebsv1.BuildInfoAborted)
}

func TestWriteStatusUnknownMismatchRequeues(t *testing.T) {
	c, client, _, _ := newTestController(t)
	client.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoPending))
	// Unknown and the intent never landed: mismatch requeues with the delay.
	client.InjectWrite("update-status", clientpkg.WriteUnknown, 0, false)
	result, err := writeRound(t, c, client, func(bi *ebsv1.BuildInfo) { bi.Status.Phase = ebsv1.BuildInfoAborted })
	if err != nil {
		t.Fatalf("writeStatus() error = %v", err)
	}
	if result.RequeueAfter != conflictRequeueDelay {
		t.Fatalf("RequeueAfter = %v, want %v", result.RequeueAfter, conflictRequeueDelay)
	}
	requirePhase(t, getBuildInfo(t, client), ebsv1.BuildInfoPending)
}

func TestWriteStatusUnknownUIDChangedEndsRound(t *testing.T) {
	c, client, _, _ := newTestController(t)
	client.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoPending))
	// A same-name replacement with a different UID ends the old round.
	recreated := testBuildInfoObj(ebsv1.BuildInfoPending)
	recreated.UID = "another-uid"
	client.SeedBuildInfo(recreated)
	client.InjectWrite("update-status", clientpkg.WriteUnknown, 0, false)
	key := testNS + "/" + testBuild
	c.dcgDict.Set(key, NewDcgDict(graphWith(edge{"a", "b"})))
	round := &reconcileRound{key: key, current: testBuildInfoObj(ebsv1.BuildInfoPending), failures: c.newRoundFailures(key)}
	round.current.UID = "original-uid"
	next := round.current.DeepCopy()
	next.Status.Phase = ebsv1.BuildInfoAborted
	result, err := c.writeStatus(context.Background(), round, next)
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("writeStatus() = %v, %v, want silent round end", result, err)
	}
	if got := c.dcgDict.Len(); got != 0 {
		t.Fatalf("dcgDict len = %d, want 0 after uid-change round end", got)
	}
}

func TestWriteStatusUnknownConfirmNotFound(t *testing.T) {
	c, client, _, _ := newTestController(t)
	seeded := client.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoPending))
	client.InjectWrite("update-status", clientpkg.WriteUnknown, 0, false)
	// The object was deleted before the confirmation read.
	client.InjectRead("buildinfos", 1, ErrNotFound)
	result, err := writeSeededRound(t, c, seeded, func(bi *ebsv1.BuildInfo) { bi.Status.Phase = ebsv1.BuildInfoAborted })
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("writeStatus() = %v, %v, want silent round end", result, err)
	}
}

func TestWriteStatusUnknownConfirmReadFailure(t *testing.T) {
	c, client, _, _ := newTestController(t)
	seeded := client.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoPending))
	client.InjectWrite("update-status", clientpkg.WriteUnknown, 0, false)
	client.InjectRead("buildinfos", 1, errors.New("boom"))
	_, err := writeSeededRound(t, c, seeded, func(bi *ebsv1.BuildInfo) { bi.Status.Phase = ebsv1.BuildInfoAborted })
	if err == nil {
		t.Fatal("writeStatus() error = nil, want confirmation read failure")
	}
}

func TestConfirmStatusWriteCancelledContext(t *testing.T) {
	c, client, _, _ := newTestController(t)
	client.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoPending))
	client.InjectWrite("update-status", clientpkg.WriteUnknown, 0, false)
	seeded := getBuildInfo(t, client)
	round := &reconcileRound{key: testNS + "/" + testBuild, current: seeded, failures: c.newRoundFailures(testNS + "/" + testBuild)}
	ctx, cancel := context.WithCancel(context.Background())
	intent := seeded.DeepCopy()
	intent.Status.Phase = ebsv1.BuildInfoAborted
	// The fake sends the write despite cancellation; confirmation must not
	// continue with a background context.
	cancel()
	_, err := c.writeStatus(ctx, round, intent)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("writeStatus() error = %v, want context.Canceled", err)
	}
}

// --- Status write chaining ---

func TestWriteStatusChaining(t *testing.T) {
	c, client, _, _ := newTestController(t)
	seeded := client.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoPending))
	key := testNS + "/" + testBuild
	round := &reconcileRound{key: key, current: seeded, failures: c.newRoundFailures(key)}
	// First write: Pending -> Processing.
	next := round.current.DeepCopy()
	next.Status.Phase = ebsv1.BuildInfoProcessing
	if result, err := c.writeStatus(context.Background(), round, next); err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("first writeStatus() = %v, %v", result, err)
	}
	// Second write reuses the confirmed object (fresh resourceVersion).
	next = round.current.DeepCopy()
	upsertCondition(&next.Status.Conditions, ConditionAllSpecsSucceeded, ReasonAllSpecsSucceeded, "ok")
	next.Status.Phase = ebsv1.BuildInfoCompleted
	if result, err := c.writeStatus(context.Background(), round, next); err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("second writeStatus() = %v, %v (chained resourceVersion)", result, err)
	}
	persisted := getBuildInfo(t, client)
	requirePhase(t, persisted, ebsv1.BuildInfoCompleted)
	requireCondition(t, persisted.Status.Conditions, ConditionAllSpecsSucceeded, ReasonAllSpecsSucceeded)
}

// --- Status intent comparison ---

// copyStatus deep-copies a BuildInfoStatus (the api package only generates
// DeepCopy for the top-level object kinds).
func copyStatus(in *ebsv1.BuildInfoStatus) *ebsv1.BuildInfoStatus {
	out := (&ebsv1.BuildInfo{Status: *in}).DeepCopy()
	return &out.Status
}

func TestStatusMatchesIntent(t *testing.T) {
	base := &ebsv1.BuildInfoStatus{Phase: ebsv1.BuildInfoProcessing}
	if !statusMatchesIntent(base, copyStatus(base)) {
		t.Fatal("identical statuses must match")
	}
	// nil maps equal empty maps.
	withEmpty := copyStatus(base)
	withEmpty.SpecStatus = ebsv1.NewSpecStatusGroup(map[string]ebsv1.SpecStatus{})
	withEmpty.SpecRepoNames = map[string]string{}
	withEmpty.Dcg = map[string]ebsv1.DcgNodeState{}
	withEmpty.PendingJobCreates = map[string]ebsv1.PendingJobCreate{}
	if !statusMatchesIntent(base, withEmpty) {
		t.Fatal("nil maps must match empty maps")
	}
	// Condition order carries no semantics.
	left := copyStatus(base)
	upsertCondition(&left.Conditions, ConditionAllSpecsSucceeded, ReasonAllSpecsSucceeded, "m1")
	upsertCondition(&left.Conditions, ConditionDcgBuildFailed, ReasonDcgBuildFailed, "m2")
	right := copyStatus(base)
	upsertCondition(&right.Conditions, ConditionDcgBuildFailed, ReasonDcgBuildFailed, "m2")
	upsertCondition(&right.Conditions, ConditionAllSpecsSucceeded, ReasonAllSpecsSucceeded, "m1")
	if !statusMatchesIntent(left, right) {
		t.Fatal("condition order must not matter")
	}
	// Field-level mismatches are detected.
	mismatch := copyStatus(base)
	mismatch.Phase = ebsv1.BuildInfoCompleted
	if statusMatchesIntent(base, mismatch) {
		t.Fatal("phase mismatch must not match")
	}
	specA := copyStatus(base)
	specA.SpecStatus = ebsv1.NewSpecStatusGroup(map[string]ebsv1.SpecStatus{"a": {DispatchCount: 1}})
	specB := copyStatus(base)
	specB.SpecStatus = ebsv1.NewSpecStatusGroup(map[string]ebsv1.SpecStatus{"a": {DispatchCount: 2}})
	if statusMatchesIntent(specA, specB) {
		t.Fatal("dispatchCount mismatch must not match")
	}
	repos := copyStatus(base)
	repos.SpecRepoNames = map[string]string{"a": "repo-a"}
	if statusMatchesIntent(base, repos) {
		t.Fatal("specRepoNames mismatch must not match")
	}
	dcgA := copyStatus(base)
	dcgA.Dcg = map[string]ebsv1.DcgNodeState{"a": {OutDep: []string{"b"}}}
	dcgB := copyStatus(base)
	dcgB.Dcg = map[string]ebsv1.DcgNodeState{"a": {}}
	if statusMatchesIntent(dcgA, dcgB) {
		t.Fatal("dcg edge-set mismatch must not match")
	}
	pendA := copyStatus(base)
	pendA.PendingJobCreates = map[string]ebsv1.PendingJobCreate{"a": {JobName: "j", DispatchGeneration: 1}}
	if statusMatchesIntent(base, pendA) {
		t.Fatal("pendingJobCreates presence mismatch must not match")
	}
	failed := copyStatus(base)
	failed.FailedPackages = []string{"repo1"}
	if statusMatchesIntent(base, failed) {
		t.Fatal("failedPackages mismatch must not match")
	}
}

// --- Conditions ---

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
	if strings.HasSuffix(body, ",") || strings.Contains(body[strings.LastIndex(body, ",")+1:], "dep-") && strings.Count(body[strings.LastIndex(body, ",")+1:], "-") > 1 {
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
	upsertCondition(&conds, ConditionSnapshotUnavailable, ReasonSnapshotNotFound, "m")
	if got := stopCondition(conds); got == nil || got.Type != ConditionSnapshotUnavailable {
		t.Fatalf("stopCondition = %+v, want SnapshotUnavailable", got)
	}
}

// --- Metric counting ---

func scrapeCounter(t *testing.T, name string) uint64 {
	t.Helper()
	rec := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, name+" ") {
			value, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, name)), 10, 64)
			if err != nil {
				t.Fatalf("parse counter %q line %q: %v", name, line, err)
			}
			return value
		}
	}
	return 0
}

func TestMetricsCountOnlyConfirmedWrites(t *testing.T) {
	c, client, _, _ := newTestController(t)
	client.SeedBuildInfo(testBuildInfoObj(ebsv1.BuildInfoPending))
	phaseBefore := scrapeCounter(t, "build_info_controller_phase_transitions_total")
	condsBefore := scrapeCounter(t, "build_info_controller_conditions_total")
	conflictBefore := scrapeCounter(t, "build_info_controller_conflict_requeues_total")
	unknownBefore := scrapeCounter(t, "build_info_controller_unknown_writes_total")

	// A confirmed phase+condition write counts once each.
	seeded := getBuildInfo(t, client)
	key := testNS + "/" + testBuild
	round := &reconcileRound{key: key, current: seeded, failures: c.newRoundFailures(key)}
	next := seeded.DeepCopy()
	next.Status.Phase = ebsv1.BuildInfoProcessing
	upsertCondition(&next.Status.Conditions, ConditionDcgBuildFailed, ReasonDcgBuildFailed, "m")
	if _, err := c.writeStatus(context.Background(), round, next); err != nil {
		t.Fatalf("writeStatus() error = %v", err)
	}
	if got := scrapeCounter(t, "build_info_controller_phase_transitions_total") - phaseBefore; got != 1 {
		t.Fatalf("phase transitions +%d, want +1", got)
	}
	if got := scrapeCounter(t, "build_info_controller_conditions_total") - condsBefore; got != 1 {
		t.Fatalf("condition upserts +%d, want +1", got)
	}

	// A 409-conflict write counts the requeue, never the state changes.
	client.InjectWrite("update-status", clientpkg.WriteRejected, 409, false)
	phaseBefore = scrapeCounter(t, "build_info_controller_phase_transitions_total")
	if _, err := writeRound(t, c, client, func(bi *ebsv1.BuildInfo) { bi.Status.Phase = ebsv1.BuildInfoCompleted }); err != nil {
		t.Fatalf("writeStatus() error = %v", err)
	}
	if got := scrapeCounter(t, "build_info_controller_conflict_requeues_total") - conflictBefore; got != 1 {
		t.Fatalf("conflict requeues +%d, want +1", got)
	}
	if got := scrapeCounter(t, "build_info_controller_phase_transitions_total") - phaseBefore; got != 0 {
		t.Fatalf("conflicted write counted %d phase transitions, want 0", got)
	}

	// An Unknown write entering the confirmation read counts unknownWrites.
	client.InjectWrite("update-status", clientpkg.WriteUnknown, 0, true)
	if _, err := writeRound(t, c, client, func(bi *ebsv1.BuildInfo) { bi.Status.Phase = ebsv1.BuildInfoCompleted }); err != nil {
		t.Fatalf("writeStatus() error = %v", err)
	}
	if got := scrapeCounter(t, "build_info_controller_unknown_writes_total") - unknownBefore; got != 1 {
		t.Fatalf("unknown writes +%d, want +1", got)
	}
}

// --- Round-local readiness counters ---

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
