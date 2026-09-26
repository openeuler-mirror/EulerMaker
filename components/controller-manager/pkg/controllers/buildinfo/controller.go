// Package buildinfo implements the BuildInfo controller (design
// controller-manager~buildinfo-controller.md): it assembles the spec
// dependency graph, determines the build set, dispatches Jobs under the
// ordering gates and aggregates build results into BuildInfo.status.
package buildinfo

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/clock"

	"controller-manager/pkg/clients/gitserver"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/controllers/buildinfo/rpmver"
	"controller-manager/pkg/controllers/buildinfo/specparse"
	"controller-manager/pkg/manager"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

// Name is the controller identifier used in logs, metrics and --controllers.
const Name = "buildinfo"

// Config carries the BuildInfo controller settings (design 12.1).
type Config struct {
	// PollPeriod is the PollingSource resync period (--poll-period shared).
	PollPeriod time.Duration
	// MaxRetries is the fast-backoff budget before slow retry (shared).
	MaxRetries int
	// DcgPruneGrace is the tombstone grace of the per-BuildInfo caches
	// (--build-info-dcg-prune-grace, default 3*PollPeriod; design 5.4).
	DcgPruneGrace time.Duration
	// RpmRepoReadyRetryLimit is the consecutive-failure threshold that
	// escalates to RpmRepoUnavailable (--rpmrepo-ready-retry-limit,
	// default 3; design E-29).
	RpmRepoReadyRetryLimit int
	// SnapshotReadyRetryLimit is the consecutive-failure threshold that
	// escalates to SnapshotUnavailable (--snapshot-ready-retry-limit,
	// default 3; design E-30).
	SnapshotReadyRetryLimit int
	// SpecFileCacheSize is the global spec file LRU capacity
	// (--specfile-cache-size, default 10000; design 15.11).
	SpecFileCacheSize int
	// SpecParseEngine selects the spec parse engine (--spec-parse-engine,
	// "text" or "rpmspec"; empty defaults to "text"). text parses with the
	// in-package grammar only and is safe for untrusted repo content;
	// rpmspec expands the spec through a local rpmspec subprocess, whose macro
	// expansion evaluates %(...) shell escapes and %{lua:...} from the repo
	// content on this host — enable only for trusted package sources.
	SpecParseEngine string
}

func (c Config) validate() error {
	if c.PollPeriod <= 0 || c.MaxRetries < 0 {
		return fmt.Errorf("BuildInfo controller poll period must be positive and retry count must not be negative")
	}
	if c.DcgPruneGrace <= 0 {
		return fmt.Errorf("BuildInfo controller dcg prune grace must be positive")
	}
	if c.RpmRepoReadyRetryLimit <= 0 || c.SnapshotReadyRetryLimit <= 0 {
		return fmt.Errorf("BuildInfo controller ready retry limits must be positive")
	}
	if c.SpecFileCacheSize <= 0 {
		return fmt.Errorf("BuildInfo controller spec file cache size must be positive")
	}
	if !specparse.Engine(c.SpecParseEngine).Valid() {
		return fmt.Errorf("BuildInfo controller spec parse engine must be %q or %q", specparse.EngineText, specparse.EngineRpmspec)
	}
	return nil
}

// Controller assembles, dispatches and aggregates one BuildInfo.
type Controller struct {
	*controller.BaseController
	buildInfos source.Source
	client     Client
	gitServer  gitserver.GitServerClient
	clock      clock.Clock
	config     Config

	// Per-BuildInfo caches keyed by <namespace>/<name> with the shared
	// tombstone lifecycle (design 5.4/15.9-15.11).
	dcgDict          *perBuildInfoCache[*DcgDict]
	rpmMetaSources   *perBuildInfoCache[*rpmver.RpmMetaSources]
	specDependsCache *perBuildInfoCache[map[string]specparse.SpecDepend]
	// specFiles is the global spec file LRU (design 15.11.2); it survives
	// BuildInfo terminal states and is only capacity-evicted.
	specFiles *specFileCache
	// specEngine is the normalized parse engine passed to specparse.Parse
	// (design 16.3, --spec-parse-engine).
	specEngine specparse.Engine

	counters *failureCounters
}

// New builds the controller and registers the BuildInfo event handler on the
// polling source (design 2.2/5.2). Event handlers are pure in-memory: type
// check, tombstone bookkeeping and enqueue; zero apiserver I/O.
func New(buildInfos source.Source, client Client, gitServer gitserver.GitServerClient, clk clock.Clock, config Config, options ...controller.Option) (*Controller, error) {
	if buildInfos == nil || client == nil || gitServer == nil || clk == nil {
		return nil, fmt.Errorf("BuildInfo source, API client, git-server client and clock are required")
	}
	if config.SpecParseEngine == "" {
		config.SpecParseEngine = string(specparse.EngineText)
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	c := &Controller{
		buildInfos:       buildInfos,
		client:           client,
		gitServer:        gitServer,
		clock:            clk,
		config:           config,
		dcgDict:          newPerBuildInfoCache[*DcgDict](clk, config.DcgPruneGrace),
		rpmMetaSources:   newPerBuildInfoCache[*rpmver.RpmMetaSources](clk, config.DcgPruneGrace),
		specDependsCache: newPerBuildInfoCache[map[string]specparse.SpecDepend](clk, config.DcgPruneGrace),
		specFiles:        newSpecFileCache(config.SpecFileCacheSize),
		specEngine:       specparse.Engine(config.SpecParseEngine),
		counters:         newFailureCounters(),
	}
	if c.specEngine == specparse.EngineRpmspec {
		log.Printf("controller=%s reason=SpecParseEngineRpmspec warning=%q", Name,
			"spec parsing via local rpmspec is enabled; %(...) and %{lua} in untrusted repo specs execute on this host")
	}
	base, err := controller.New(Name, c.sync, config.MaxRetries, options...)
	if err != nil {
		return nil, err
	}
	c.BaseController = base
	if err := buildInfos.AddEventHandler(source.ResourceEventHandlerFuncs{AddFunc: c.onAdd, UpdateFunc: c.onUpdate, DeleteFunc: c.onDelete}); err != nil {
		return nil, fmt.Errorf("register BuildInfo handler: %w", err)
	}
	return c, nil
}

// Initializer wires the controller into the manager (design 2.2/12.1): the
// BuildInfo polling source lists without a server-side selector (5.2 —
// filtering happens in the in-memory handler), the typed API client wraps
// the shared client, and the slow-retry policy reuses the global settings.
func Initializer(config Config, gitClient gitserver.GitServerClient) manager.InitFunc {
	return func(_ context.Context, init manager.InitContext) (controller.Controller, bool, error) {
		shared, ok := init.Dependencies.Client.(SharedClient)
		if !ok {
			return nil, false, fmt.Errorf("shared API client does not implement the BuildInfo SharedClient surface")
		}
		buildInfos, err := init.Dependencies.PollingFactory.ForResource(source.BuildInfosGVR, config.PollPeriod, metav1.ListOptions{})
		if err != nil {
			return nil, false, err
		}
		value, err := New(buildInfos, newAPIClient(shared), gitClient, clock.RealClock{}, config,
			controller.WithSlowRetry(init.Config.SlowRetryInitial, init.Config.SlowRetryMax, init.Config.SlowRetryJitter))
		return value, err == nil, err
	}
}

// Run starts the worker pool plus the tombstone sweeper (design 5.4).
func (c *Controller) Run(ctx context.Context, workers int) error {
	go c.sweepLoop(ctx)
	return c.BaseController.Run(ctx, workers)
}

// sweepLoop periodically prunes expired tombstones from the per-BuildInfo
// caches. Expiry is also applied lazily on access; the sweeper bounds memory
// for keys that are never touched again.
func (c *Controller) sweepLoop(ctx context.Context) {
	ticker := time.NewTicker(c.config.DcgPruneGrace)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			removed := c.dcgDict.SweepExpired() + c.rpmMetaSources.SweepExpired() + c.specDependsCache.SweepExpired()
			if removed > 0 {
				log.Printf("controller=%s event=cache-sweep removed=%d", Name, removed)
			}
		}
	}
}

// onAdd handles a BuildInfo entering the polling snapshot (design 5.2).
func (c *Controller) onAdd(obj runtime.Object) { c.enqueueBuildInfo(obj) }

// onUpdate handles a BuildInfo snapshot refresh. The same resourceVersion is
// re-enqueued on purpose: polling Updates are the resync fallback for lost
// events, slow retries and recovered external dependencies (5.2).
func (c *Controller) onUpdate(_, newObj runtime.Object) { c.enqueueBuildInfo(newObj) }

// onDelete only tombstones the per-BuildInfo caches: disappearing from the
// polling snapshot does not prove a physical delete, so the entry survives a
// spurious empty list for the grace period (5.2/5.4). The failure counters
// and the log-dedup entries carry no tombstone semantics (5.4: cleared on
// delete, no grace) — a recreated same-name BuildInfo must start from zero.
func (c *Controller) onDelete(obj runtime.Object) {
	buildInfo, ok := obj.(*ebsv1.BuildInfo)
	if !ok || buildInfo == nil || buildInfo.Name == "" || buildInfo.Namespace == "" {
		log.Printf("controller=%s reason=UnexpectedBuildInfoEvent type=%T", Name, obj)
		return
	}
	key := cacheKey(buildInfo.Namespace, buildInfo.Name)
	c.tombstoneCaches(key)
	c.counters.Clear(key)
	dedup.forgetKey(key)
}

// enqueueBuildInfo revokes pending tombstones and enqueues non-terminal
// BuildInfos (G-05: Completed/Aborted never re-enter the queue).
func (c *Controller) enqueueBuildInfo(obj runtime.Object) {
	buildInfo, ok := obj.(*ebsv1.BuildInfo)
	if !ok || buildInfo == nil || buildInfo.Name == "" || buildInfo.Namespace == "" {
		log.Printf("controller=%s reason=UnexpectedBuildInfoEvent type=%T", Name, obj)
		return
	}
	key := cacheKey(buildInfo.Namespace, buildInfo.Name)
	c.revokeTombstones(key)
	if buildInfo.Status.Phase != ebsv1.BuildInfoPending && buildInfo.Status.Phase != ebsv1.BuildInfoProcessing {
		return
	}
	c.Enqueue(key)
}

// invalidateCaches drops the per-BuildInfo caches, counters and log-dedup
// entries immediately: terminal phase observed or re-get 404 during
// reconcile (design 5.4). The global spec file LRU is untouched.
func (c *Controller) invalidateCaches(key string) {
	c.dcgDict.Invalidate(key)
	c.rpmMetaSources.Invalidate(key)
	c.specDependsCache.Invalidate(key)
	c.counters.Clear(key)
	dedup.forgetKey(key)
}

func (c *Controller) tombstoneCaches(key string) {
	c.dcgDict.Tombstone(key)
	c.rpmMetaSources.Tombstone(key)
	c.specDependsCache.Tombstone(key)
}

func (c *Controller) revokeTombstones(key string) {
	c.dcgDict.RevokeTombstone(key)
	c.rpmMetaSources.RevokeTombstone(key)
	c.specDependsCache.RevokeTombstone(key)
}

// counterKind selects which readiness failure counter a checkpoint feeds.
type counterKind int

const (
	counterRpmRepo counterKind = iota
	counterSnapshot
)

// failureCounter is one per-BuildInfo readiness counter entry (design 5.4):
// consecutive failures plus the last failure checkpoint of the current
// streak for the escalated condition message.
type failureCounter struct {
	ConsecutiveFails int
	LastReason       string
	LastMessage      string
}

// failureCounters holds the rpmRepoReadyFailures and snapshotReadyFailures
// maps (design 5.4): RWMutex-guarded, keyed by <namespace>/<name>, no TTL,
// never persisted. Entries are cleared on success, after the escalated stop
// marker write is confirmed, and on terminal/delete (no tombstone grace).
type failureCounters struct {
	mu       sync.RWMutex
	rpmRepo  map[string]*failureCounter
	snapshot map[string]*failureCounter
}

func newFailureCounters() *failureCounters {
	return &failureCounters{
		rpmRepo:  map[string]*failureCounter{},
		snapshot: map[string]*failureCounter{},
	}
}

func (c *failureCounters) bucket(kind counterKind) map[string]*failureCounter {
	if kind == counterSnapshot {
		return c.snapshot
	}
	return c.rpmRepo
}

// Increment records one consecutive failure and returns the new count.
// Callers route through roundFailures so a round increments at most once.
func (c *failureCounters) Increment(kind counterKind, key, reason, message string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	bucket := c.bucket(kind)
	entry, ok := bucket[key]
	if !ok {
		entry = &failureCounter{}
		bucket[key] = entry
	}
	entry.ConsecutiveFails++
	entry.LastReason = reason
	entry.LastMessage = message
	return entry.ConsecutiveFails
}

// Count reports the current consecutive-failure count (0 when absent).
func (c *failureCounters) Count(kind counterKind, key string) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.bucket(kind)[key]
	if !ok {
		return 0
	}
	return entry.ConsecutiveFails
}

// Entry snapshots the current counter for escalation message building
// (E-29/E-30); a missing entry yields the zero value.
func (c *failureCounters) Entry(kind counterKind, key string) failureCounter {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.bucket(kind)[key]
	if !ok {
		return failureCounter{}
	}
	return *entry
}

// Success clears one counter after a ready observation (design 5.4).
func (c *failureCounters) Success(kind counterKind, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.bucket(kind), key)
}

// Clear drops both counters: terminal phase, delete, or after the stop
// marker write is confirmed (design 5.4).
func (c *failureCounters) Clear(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.rpmRepo, key)
	delete(c.snapshot, key)
}

// roundFailures tracks which readiness counters a single reconcile round has
// already bumped, enforcing "at most one increment per round, first failure
// checkpoint wins" (design 5.4). One instance per reconcile round.
type roundFailures struct {
	key            string
	counters       *failureCounters
	limitRpmRepo   int
	limitSnapshot  int
	rpmRepoBumped  bool
	snapshotBumped bool
}

func (c *Controller) newRoundFailures(key string) *roundFailures {
	return &roundFailures{
		key:           key,
		counters:      c.counters,
		limitRpmRepo:  c.config.RpmRepoReadyRetryLimit,
		limitSnapshot: c.config.SnapshotReadyRetryLimit,
	}
}

// RpmRepoFailed records an RpmRepo readiness failure checkpoint and reports
// whether the escalation threshold (E-29) is now reached.
func (r *roundFailures) RpmRepoFailed(reason, message string) (count int, escalated bool) {
	if r.rpmRepoBumped {
		return r.counters.Count(counterRpmRepo, r.key), false
	}
	r.rpmRepoBumped = true
	count = r.counters.Increment(counterRpmRepo, r.key, reason, message)
	return count, count >= r.limitRpmRepo
}

// SnapshotFailed records a current-Snapshot query failure checkpoint and
// reports whether the escalation threshold (E-30) is now reached.
func (r *roundFailures) SnapshotFailed(reason, message string) (count int, escalated bool) {
	if r.snapshotBumped {
		return r.counters.Count(counterSnapshot, r.key), false
	}
	r.snapshotBumped = true
	count = r.counters.Increment(counterSnapshot, r.key, reason, message)
	return count, count >= r.limitSnapshot
}

// RpmRepoReady clears the RpmRepo counter after a ready observation.
func (r *roundFailures) RpmRepoReady() { r.counters.Success(counterRpmRepo, r.key) }

// SnapshotReady clears the Snapshot counter after a successful GET.
func (r *roundFailures) SnapshotReady() { r.counters.Success(counterSnapshot, r.key) }
