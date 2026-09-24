// init.go implements the Pending-phase initialization (design 7.2 steps 0~6)
// and the single-type直通 path (7.2.3): assembly and build-set determination,
// existing-Job backfill, DCG obtain-and-persist (G-02), zero-indegree and
// bootstrap dispatches, initialization closeout (empty set / pre-create
// missing entries -> Processing), and the dirty-check write.
package buildinfo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/controllers/buildinfo/rpmver"
	"controller-manager/pkg/controllers/buildinfo/specparse"
	ebsv1 "ebs-api/ebs/v1"
)

// rpmMetaFetch downloads the repository XML metadata (design 15.10). The
// download timeout is a code constant: 12.1 defines no dedicated flag and the
// git-server timeout does not apply to repository URLs.
var rpmMetaFetch = rpmver.HTTPFetcher(&http.Client{Timeout: 60 * time.Second})

// initBuildInfo runs the 7.2 init flow. single takes the 7.2.3直通 path.
func (c *Controller) initBuildInfo(ctx context.Context, round *reconcileRound) (controller.ReconcileResult, error) {
	if round.isSingle() {
		return c.initSingle(ctx, round)
	}

	// Step 0: current Snapshot (E-30 counting) + full assembly (7.2.2 阶段一).
	snapshot, stop, result, err := c.currentSnapshot(ctx, round)
	if stop {
		return result, err
	}
	asm := c.assembleSpecDepends(ctx, round, snapshot)
	if result, err = c.persistAssemblyFailures(ctx, round, asm); err != nil || result != (controller.ReconcileResult{}) {
		return result, err
	}
	if asm.incomplete {
		// Transient assembly gap: stay Pending and re-assemble next round.
		return controller.ReconcileResult{}, nil
	}

	// RpmRepo must be held for every non-single verdict and the first graph
	// build (E-16: 404 below the threshold leaves the round unheld — dependency
	// verdicts and the first build are pending, wait for the next round).
	if !round.rpmRepoHeld {
		c.logf(round.key, "RpmRepoNotHeld", "rpmrepo not held this round; init dispatch deferred (E-16)")
		return controller.ReconcileResult{}, nil
	}
	// RPM metadata refresh (15.10): layered sources for expansion, edge
	// building and the 7.4.1 availability verdict.
	sources, result, err := c.refreshRpmMetaSources(ctx, round)
	if err != nil || result != (controller.ReconcileResult{}) || sources == nil {
		return result, err
	}

	// Build-set determination (7.2.2 阶段二).
	buildSet, err := c.determineBuildSet(round, asm, sources.RepoLayer)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	if len(buildSet) == 0 {
		// Step 5: empty build set (full/incremental no-change) -> Completed.
		return c.completeInitEmpty(ctx, round, asm.degraded)
	}

	// Step 1: backfill existing Jobs (covers E-11: created but write lost).
	jobs, err := c.listRoundJobs(ctx, round)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	next := round.current.DeepCopy()
	applyDegradedConditions(&next.Status.Conditions, asm.degraded)
	c.backfillJobs(round, next, jobs, specNameSet(buildSet), true)
	if result, err = c.writeStatusIfChanged(ctx, round, next); err != nil || result != (controller.ReconcileResult{}) {
		return result, err
	}

	// Step 1.5: target arch (E-19 empty-arch defense runs at the check point).
	arch := round.build.Spec.BuildTarget.Arch

	// Step 2: obtain and persist the DCG (5.4/15.9; G-02: persist precedes the
	// cache update and every graph-based Job creation).
	prefer := payloadPrefer(c.parseBuildPayload(round.key, round.current.Spec.BuildPayload))
	dcg, result, err := c.obtainDcg(ctx, round, buildSet, sources, prefer)
	if err != nil || result != (controller.ReconcileResult{}) || dcg == nil {
		return result, err
	}

	// Steps 3+4: dispatch zero-indegree specs and bootstrap break points. The
	// BuildConf snapshot resolves lazily on the first actual Job creation and
	// is shared by every creation of this round (E-26).
	dispatch := &roundDispatch{arch: arch, contentURL: heldContentURL(round)}
	for _, name := range dcg.SortedNodes() {
		if dcg.Node(name).InDegree() != 0 {
			continue
		}
		if result, err = c.dispatchInitSpec(ctx, round, dispatch, name, buildSet[name], snapshot, sources); err != nil || result != (controller.ReconcileResult{}) {
			return result, err
		}
	}
	for _, name := range dcg.GetBootstrapBreaks() {
		if result, err = c.dispatchInitSpec(ctx, round, dispatch, name, buildSet[name], snapshot, sources); err != nil || result != (controller.ReconcileResult{}) {
			return result, err
		}
	}

	// Step 5+6: pre-create missing entries (never overwrite backfilled or
	// freshly written ones) and flip to Processing; dirty-checked write.
	next = round.current.DeepCopy()
	for name := range buildSet {
		if _, ok := next.Status.SpecStatus[name]; !ok {
			if next.Status.SpecStatus == nil {
				next.Status.SpecStatus = map[string]ebsv1.SpecStatus{}
			}
			next.Status.SpecStatus[name] = ebsv1.SpecStatus{}
		}
	}
	next.Status.Phase = ebsv1.BuildInfoProcessing
	return c.writeStatusIfChanged(ctx, round, next)
}

// roundDispatch carries the per-round dispatch constants resolved once and
// shared by every spec dispatch of the round (design 7.2 steps 3/4).
type roundDispatch struct {
	arch       string
	contentURL string
	image      string
	imageReady bool
}

// ensureImage resolves the BuildConf image snapshot lazily (E-26: a read
// failure or a missing mapping pauses the round — plain error backoff, no
// condition, no Failed marking).
func (c *Controller) ensureImage(ctx context.Context, round *reconcileRound, dispatch *roundDispatch) (string, error) {
	if dispatch.imageReady {
		return dispatch.image, nil
	}
	conf, err := c.client.GetBuildConf(ctx)
	if err != nil {
		return "", err
	}
	image, err := clientpkg.BuildImage(conf, round.build.Spec.BuildTarget)
	if err != nil {
		return "", err
	}
	dispatch.image, dispatch.imageReady = image, true
	return image, nil
}

// dispatchInitSpec runs the init per-spec dispatch pipeline (design 7.2 steps
// 3/4): skip already-dispatched or terminal specs, E-19 arch check, the 7.4.1
// condition-2 availability verdict (the only gate kept for bootstrap breaks,
// 7.4.6 #3), then the 15.3.1 creation pipeline.
func (c *Controller) dispatchInitSpec(ctx context.Context, round *reconcileRound, dispatch *roundDispatch, specName string, depend specparse.SpecDepend, snapshot *ebsv1.Snapshot, sources *rpmver.RpmMetaSources) (controller.ReconcileResult, error) {
	ss := round.current.Status.SpecStatus[specName]
	if ss.Build.Status != "" || ss.DispatchCount > 0 {
		// Already dispatched (Job created, Running, or a Failed verdict) —
		// re-entering init never re-dispatches nor repeats bootstrap.
		return controller.ReconcileResult{}, nil
	}
	if result, err := c.checkArchSupported(ctx, round, specName, &depend, dispatch.arch); err != nil || result != (controller.ReconcileResult{}) {
		return result, err
	}
	if ss = round.current.Status.SpecStatus[specName]; ss.Build.Status == SpecBuildFailed {
		return controller.ReconcileResult{}, nil
	}
	if result, err := c.checkBuildRequires(ctx, round, specName, &depend, sources); err != nil || result != (controller.ReconcileResult{}) {
		return result, err
	}
	if ss = round.current.Status.SpecStatus[specName]; ss.Build.Status == SpecBuildFailed {
		return controller.ReconcileResult{}, nil
	}
	image, err := c.ensureImage(ctx, round, dispatch)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	return c.dispatchSpec(ctx, round, specName, &depend, snapshot, image, dispatch.contentURL)
}

// checkArchSupported applies the E-19 exclusiveArch whitelist: an arch miss
// marks the spec Failed (ArchUnsupported) without a Job; an empty target arch
// (abnormal data) skips the check with a warning.
func (c *Controller) checkArchSupported(ctx context.Context, round *reconcileRound, specName string, depend *specparse.SpecDepend, arch string) (controller.ReconcileResult, error) {
	if arch == "" {
		c.logf(round.key, "TargetArchMissing", "build target arch empty; exclusiveArch check skipped for spec %s (E-19)", specName)
		return controller.ReconcileResult{}, nil
	}
	if archSupported(depend, arch) {
		return controller.ReconcileResult{}, nil
	}
	message := fmt.Sprintf("target arch %q not in exclusiveArch %v", arch, depend.ExclusiveArch)
	result, err := c.markSpecFailed(ctx, round, specName, ConditionArchUnsupported, ReasonArchUnsupported, message, false)
	if err == nil && result == (controller.ReconcileResult{}) {
		c.logOnce(round.key, "ArchUnsupported", "spec %s marked Failed: %s", specName, message)
	}
	return result, err
}

// checkBuildRequires applies the 7.4.1 condition-2 verdict: every
// buildRequires entry (buildRemoves excluded) must be available in the
// layered sources; a true miss marks the spec Failed (RpmDependsMissing)
// without a Job.
func (c *Controller) checkBuildRequires(ctx context.Context, round *reconcileRound, specName string, depend *specparse.SpecDepend, sources *rpmver.RpmMetaSources) (controller.ReconcileResult, error) {
	missing := missingBuildRequires(depend, sources)
	if len(missing) == 0 {
		return controller.ReconcileResult{}, nil
	}
	message := "build requires unavailable: " + missingDepsMessage(missing)
	result, err := c.markSpecFailed(ctx, round, specName, ConditionBuildFailed, ReasonRpmDependsMissing, message, false)
	if err == nil && result == (controller.ReconcileResult{}) {
		c.logOnce(round.key, "RpmDependsMissing", "spec %s marked Failed: %s", specName, message)
	}
	return result, err
}

// obtainDcg resolves the graph per 5.4/15.9: in-memory cache, persisted
// status.dcg (load never re-selects break points, G-09), or the first build.
// The first build persists the state BEFORE the cache update and any Job
// creation (G-02); a nil graph with zero result means "wait next round".
func (c *Controller) obtainDcg(ctx context.Context, round *reconcileRound, buildSet map[string]specparse.SpecDepend, sources *rpmver.RpmMetaSources, prefer []string) (*DcgDict, controller.ReconcileResult, error) {
	if d, ok := c.dcgDict.Get(round.key); ok {
		// Recovery clears DcgBuildFailed on any acquisition tier (9.1).
		if result, err := c.clearStaleDcgFailed(ctx, round); err != nil || result != (controller.ReconcileResult{}) {
			return nil, result, err
		}
		return d, controller.ReconcileResult{}, nil
	}
	if len(round.current.Status.Dcg) > 0 {
		d := DcgDictFromState(round.current.Status.Dcg)
		c.dcgDict.Set(round.key, d)
		if result, err := c.clearStaleDcgFailed(ctx, round); err != nil || result != (controller.ReconcileResult{}) {
			return nil, result, err
		}
		return d, controller.ReconcileResult{}, nil
	}
	// First build: requires the held RpmRepo (E-16) and ready metadata (the
	// caller refreshed the sources this round).
	nodes := BuildDcgNodes(buildSet, sources, prefer)
	d := NewDcgDict(nodes)
	next := round.current.DeepCopy()
	next.Status.Dcg = d.ToState()
	removeCondition(&next.Status.Conditions, ConditionDcgBuildFailed)
	result, err := c.writeStatus(ctx, round, next)
	if err != nil || result != (controller.ReconcileResult{}) {
		// Persist failed: no cache update, no Job creation (7.2 step 2).
		return nil, result, err
	}
	c.dcgDict.Set(round.key, d)
	breaks := d.GetBootstrapBreaks()
	if len(breaks) > 0 {
		bootstrapBreaks.Add(uint64(len(breaks)))
		c.logOnce(round.key, "BootstrapBreaks", "dcg built with %d nodes, break points: %s", d.Len(), strings.Join(breaks, ","))
	}
	return d, controller.ReconcileResult{}, nil
}

// clearStaleDcgFailed removes a stale DcgBuildFailed condition once the DCG is
// available again (design 9.1: recovery clears on ANY of the three
// acquisition tiers — in-process cache hit, status.dcg load, rebuild).
// Idempotent: an absent condition means no write; a failed or requeued
// removal write returns the round for an idempotent retry next round.
func (c *Controller) clearStaleDcgFailed(ctx context.Context, round *reconcileRound) (controller.ReconcileResult, error) {
	if findCondition(round.current.Status.Conditions, ConditionDcgBuildFailed) == nil {
		return controller.ReconcileResult{}, nil
	}
	next := round.current.DeepCopy()
	removeCondition(&next.Status.Conditions, ConditionDcgBuildFailed)
	return c.writeStatusIfChanged(ctx, round, next)
}

// refreshRpmMetaSources refreshes the layered RPM metadata (design 15.10):
// the RpmRepo layer re-downloads only on a contentURL change (an empty URL is
// a normal empty state), bootstrap layers parse once for the BuildInfo
// lifetime. Download/parse failures count towards E-29; below the threshold
// the round waits (nil sources, zero result, nil error).
func (c *Controller) refreshRpmMetaSources(ctx context.Context, round *reconcileRound) (*rpmver.RpmMetaSources, controller.ReconcileResult, error) {
	arch := round.build.Spec.BuildTarget.Arch
	sources, ok := c.rpmMetaSources.Get(round.key)
	if !ok {
		sources = &rpmver.RpmMetaSources{}
	}
	repoBefore := sources.RepoLayer
	if err := sources.EnsureRepoLayer(ctx, rpmMetaFetch, heldContentURL(round), arch); err != nil {
		reason := ReasonRpmRepoXMLDownloadFailed
		var srcErr *rpmver.SourceError
		if errors.As(err, &srcErr) && srcErr.Kind == rpmver.FailureParse {
			reason = ReasonRpmRepoXMLParseFailed
		}
		c.logf(round.key, "RpmRepoXMLFailed", "rpmrepo metadata unavailable: %v", err)
		return c.rpmMetaUnavailable(ctx, round, reason, err)
	}
	if sources.RepoLayer != repoBefore {
		rpmMetaRefreshes.Inc()
	}
	bootstrapBefore := sources.BootstrapLayer
	var urls []string
	for _, repo := range round.current.Spec.BootstrapRepo {
		urls = append(urls, repo.Repo)
	}
	if err := sources.EnsureBootstrapLayers(ctx, rpmMetaFetch, urls, arch); err != nil {
		c.logf(round.key, "BootstrapRepoXMLFailed", "bootstrap repo metadata unavailable: %v", err)
		return c.rpmMetaUnavailable(ctx, round, ReasonBootstrapRepoXMLUnavail, err)
	}
	if len(sources.BootstrapLayer) > 0 && bootstrapBefore == nil {
		rpmMetaRefreshes.Inc()
	}
	c.rpmMetaSources.Set(round.key, sources)
	// Overall ready this round: clear the E-29 streak (5.4).
	round.failures.RpmRepoReady()
	return sources, controller.ReconcileResult{}, nil
}

// rpmMetaUnavailable routes an XML metadata failure to the E-29 counting:
// below the threshold the round waits; at the threshold the stop marker is
// persisted and the round joins the 6.5 convergence path.
func (c *Controller) rpmMetaUnavailable(ctx context.Context, round *reconcileRound, reason string, cause error) (*rpmver.RpmMetaSources, controller.ReconcileResult, error) {
	_, escalated := round.failures.RpmRepoFailed(reason, cause.Error())
	if escalated {
		result, err := c.escalateRpmRepoUnavailable(ctx, round)
		return nil, result, err
	}
	return nil, controller.ReconcileResult{}, nil
}

// heldContentURL extracts the held RpmRepo contentURL (empty when unheld or
// the repository status is absent — a normal empty state, 15.4).
func heldContentURL(round *reconcileRound) string {
	if !round.rpmRepoHeld || round.rpmRepo.Status.Repository == nil {
		return ""
	}
	return round.rpmRepo.Status.Repository.ContentURL
}

// listRoundJobs pages every Job of this Build (7.2 step 1 / 7.3 step 2).
func (c *Controller) listRoundJobs(ctx context.Context, round *reconcileRound) ([]ebsv1.Job, error) {
	selector := labels.SelectorFromSet(labels.Set{ebsv1.JobBuildNameLabel: round.current.Name})
	return c.client.ListJobs(ctx, round.current.Namespace, selector)
}

// writeStatusIfChanged implements the 7.3 dirty-check write convention: only
// actual status changes are PUT (semantic comparison per 10.3).
func (c *Controller) writeStatusIfChanged(ctx context.Context, round *reconcileRound, next *ebsv1.BuildInfo) (controller.ReconcileResult, error) {
	if statusMatchesIntent(&round.current.Status, &next.Status) {
		return controller.ReconcileResult{}, nil
	}
	return c.writeStatus(ctx, round, next)
}

// persistAssemblyFailures records deterministic repository failures before
// dispatch, including rounds that must wait for transient repository inputs.
func (c *Controller) persistAssemblyFailures(ctx context.Context, round *reconcileRound, asm *specAssembly) (controller.ReconcileResult, error) {
	next := round.current.DeepCopy()
	var existing []string
	if asm.incomplete {
		existing = next.Status.FailedPackages
	}
	next.Status.FailedPackages = sortedFailedPackages(existing, asm.failedRepos)
	return c.writeStatusIfChanged(ctx, round, next)
}

// applyDegradedConditions upserts this round's step-0 degradation conditions
// and removes the two degradation types absent this round (each Pending
// assembly fully re-evaluates them; terminal closeouts re-add their own).
func applyDegradedConditions(conditions *[]metav1.Condition, degraded []degradedCondition) {
	present := map[string]bool{}
	for _, item := range degraded {
		present[item.condType] = true
		upsertCondition(conditions, item.condType, item.reason, strings.Join(item.items, "; "))
	}
	for _, condType := range []string{ConditionSpecDependsFillFailed, ConditionSpecCommitMissing} {
		if !present[condType] {
			removeCondition(conditions, condType)
		}
	}
}

// closeoutInit persists an init deterministic-failure收口 (6.1): condition
// SpecDependsFillFailed + phase Completed in one write; specStatus stays
// untouched by the closeout itself (no pre-creation, no flipping; a leftover
// pending entry whose Job landed is confirmed first, 6.5.1 #4). The write is
// idempotent — a failed write ends the round and the next round rewrites.
func (c *Controller) closeoutInit(ctx context.Context, round *reconcileRound, verdict *terminalVerdict, degraded []degradedCondition) (controller.ReconcileResult, error) {
	result, blocked, err := c.resolvePendingCreates(ctx, round, "init closeout")
	if err != nil || result != (controller.ReconcileResult{}) {
		return result, err
	}
	if blocked {
		return controller.ReconcileResult{}, nil
	}
	next := round.current.DeepCopy()
	applyDegradedConditions(&next.Status.Conditions, degraded)
	upsertCondition(&next.Status.Conditions, ConditionSpecDependsFillFailed, verdict.reason, verdict.message)
	next.Status.Phase = ebsv1.BuildInfoCompleted
	result, err = c.writeStatus(ctx, round, next)
	if err == nil && result == (controller.ReconcileResult{}) {
		c.logOnce(round.key, "InitDeterministicFailure", "completed: %s (%s)", verdict.reason, verdict.message)
		c.invalidateCaches(round.key)
	}
	return result, err
}

// completeInitEmpty persists the empty-build-set Completed (7.2 step 5):
// full/incremental with nothing to build flips terminal directly; degraded
// conditions are kept, AllSpecsSucceeded is left to the vacuous-success rule.
func (c *Controller) completeInitEmpty(ctx context.Context, round *reconcileRound, degraded []degradedCondition) (controller.ReconcileResult, error) {
	result, blocked, err := c.resolvePendingCreates(ctx, round, "empty build set")
	if err != nil || result != (controller.ReconcileResult{}) {
		return result, err
	}
	if blocked {
		return controller.ReconcileResult{}, nil
	}
	next := round.current.DeepCopy()
	applyDegradedConditions(&next.Status.Conditions, degraded)
	next.Status.Phase = ebsv1.BuildInfoCompleted
	result, err = c.writeStatus(ctx, round, next)
	if err == nil && result == (controller.ReconcileResult{}) {
		c.logOnce(round.key, "EmptyBuildSet", "completed: nothing to build")
		c.invalidateCaches(round.key)
	}
	return result, err
}

// pendingCreatesBlock reports whether unresolved pendingJobCreates block a
// Completed write (6.5.1: every Completed path requires an empty map; never
// misreport Completed — wait for the normal dispatch path to resolve them).
func (c *Controller) pendingCreatesBlock(round *reconcileRound, where string) bool {
	if len(round.current.Status.PendingJobCreates) == 0 {
		return false
	}
	c.logf(round.key, "PendingCreatesBlock", "%s completed write blocked by %d unresolved pending job creates", where, len(round.current.Status.PendingJobCreates))
	return true
}

// resolvePendingCreates GET-verifies every registered pending entry before an
// init closeout writes Completed. The closeout path short-circuits before the
// Step 1 backfill, so without this pass a leftover entry (an Unknown create
// that actually landed, or a registration followed by a mid-round crash)
// would block the Completed write forever — nothing else revisits it once the
// terminal verdict persists. A landed Job is identity-checked and confirmed in
// one write (entry removed, dispatch recorded); re-creation is forbidden on a
// terminal verdict (G-03). A 404 keeps the entry with a warning — 404 is never
// a non-existence proof for an in-flight request (6.5.1 #5, same rule as the
// stop path in convergeToCompleted); the next round's GET resolves it once it
// lands. Other GET errors end the round and retry. Returns whether
// unresolved entries still block the write.
func (c *Controller) resolvePendingCreates(ctx context.Context, round *reconcileRound, where string) (controller.ReconcileResult, bool, error) {
	if len(round.current.Status.PendingJobCreates) == 0 {
		return controller.ReconcileResult{}, false, nil
	}
	for _, spec := range sortedSpecNames(round.current.Status.PendingJobCreates) {
		pend, ok := round.current.Status.PendingJobCreates[spec]
		if !ok {
			continue // resolved by an earlier confirm write this round
		}
		job, err := c.client.GetJob(ctx, round.current.Namespace, pend.JobName)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				c.logf(round.key, "JobCreateUnresolved", "pending job create for spec %s (job %s generation %d) unresolved at %s; entry kept, waiting (6.5.1 #5)", spec, pend.JobName, pend.DispatchGeneration, where)
				continue
			}
			return controller.ReconcileResult{}, true, err
		}
		if verr := verifyJobIdentity(job, round.current, spec, pend.DispatchGeneration); verr != nil {
			c.logf(round.key, "JobIdentityMismatch", "job %s identity mismatch: %v", pend.JobName, verr)
			return controller.ReconcileResult{}, true, controller.NewPermanentError(verr)
		}
		result, err := c.confirmDispatchedJob(ctx, round, spec, pend.DispatchGeneration, job)
		if err != nil || result != (controller.ReconcileResult{}) {
			return result, true, err
		}
	}
	if len(round.current.Status.PendingJobCreates) > 0 {
		c.logf(round.key, "PendingCreatesBlock", "%s completed write blocked by %d unresolved pending job creates", where, len(round.current.Status.PendingJobCreates))
		return controller.ReconcileResult{}, true, nil
	}
	return controller.ReconcileResult{}, false, nil
}

// specNameSet flattens a build set to its spec-name scope (Job grouping).
func specNameSet(buildSet map[string]specparse.SpecDepend) map[string]bool {
	scope := make(map[string]bool, len(buildSet))
	for name := range buildSet {
		scope[name] = true
	}
	return scope
}

// sortedSpecNames returns the specStatus keys in dictionary order.
func sortedSpecNames[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// --- single 直通路径 (design 7.2.3) ---

// initSingle runs the single-type直通 init: designated-repo assembly is the
// build set, every spec dispatches directly without any gate, and only the
// E-19/E-27 deterministic checks plus the E-26 pause semantics remain.
func (c *Controller) initSingle(ctx context.Context, round *reconcileRound) (controller.ReconcileResult, error) {
	if len(round.build.Spec.Packages) == 0 {
		// Data anomaly: packages empty -> SpecifiedBuildSetEmpty closeout.
		return c.closeoutInit(ctx, round, &terminalVerdict{
			reason:  ReasonSpecifiedBuildSetEmpty,
			message: "single build with empty Build.spec.packages",
		}, nil)
	}
	snapshot, stop, result, err := c.currentSnapshot(ctx, round)
	if stop {
		return result, err
	}
	asm := c.assembleSpecDepends(ctx, round, snapshot)
	if result, err = c.persistAssemblyFailures(ctx, round, asm); err != nil || result != (controller.ReconcileResult{}) {
		return result, err
	}
	if asm.incomplete {
		return controller.ReconcileResult{}, nil
	}
	buildSet := asm.depends
	if len(buildSet) == 0 {
		// Every designated package degraded away -> SpecifiedBuildSetEmpty.
		return c.closeoutInit(ctx, round, &terminalVerdict{
			reason:  ReasonSpecifiedBuildSetEmpty,
			message: fmt.Sprintf("all designated packages %v were skipped: no buildable spec", round.build.Spec.Packages),
		}, asm.degraded)
	}

	// Repo injection source (7.2.3 #3): the current same-name RpmRepo
	// contentURL (creation-time inherited baseline; single never materializes).
	contentURL, result, err := c.singleContentURL(ctx, round)
	if err != nil || result != (controller.ReconcileResult{}) {
		return result, err
	}

	// Backfill existing Jobs (E-11 coverage), persist degraded conditions.
	jobs, err := c.listRoundJobs(ctx, round)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	next := round.current.DeepCopy()
	applyDegradedConditions(&next.Status.Conditions, asm.degraded)
	c.backfillJobs(round, next, jobs, specNameSet(buildSet), true)
	if result, err = c.writeStatusIfChanged(ctx, round, next); err != nil || result != (controller.ReconcileResult{}) {
		return result, err
	}

	// 直通下发: no graph, no gates, no ordering — one direct dispatch per spec.
	dispatch := &roundDispatch{arch: round.build.Spec.BuildTarget.Arch, contentURL: contentURL}
	for _, name := range sortedSpecNames(buildSet) {
		depend := buildSet[name]
		ss := round.current.Status.SpecStatus[name]
		if ss.Build.Status != "" || ss.DispatchCount > 0 {
			continue
		}
		if result, err = c.checkArchSupported(ctx, round, name, &depend, dispatch.arch); err != nil || result != (controller.ReconcileResult{}) {
			return result, err
		}
		if ss = round.current.Status.SpecStatus[name]; ss.Build.Status == SpecBuildFailed {
			continue
		}
		image, err := c.ensureImage(ctx, round, dispatch)
		if err != nil {
			return controller.ReconcileResult{}, err
		}
		if result, err = c.dispatchSpec(ctx, round, name, &depend, snapshot, image, dispatch.contentURL); err != nil || result != (controller.ReconcileResult{}) {
			return result, err
		}
	}

	// Pre-create missing entries and flip to Processing (7.2.3 keeps the
	// 6.4 traversal-base invariant: every build-set spec gets an entry).
	next = round.current.DeepCopy()
	for name := range buildSet {
		if _, ok := next.Status.SpecStatus[name]; !ok {
			if next.Status.SpecStatus == nil {
				next.Status.SpecStatus = map[string]ebsv1.SpecStatus{}
			}
			next.Status.SpecStatus[name] = ebsv1.SpecStatus{}
		}
	}
	next.Status.Phase = ebsv1.BuildInfoProcessing
	return c.writeStatusIfChanged(ctx, round, next)
}

// singleContentURL reads the current same-name RpmRepo for the single Repo
// injection (7.2.3 #3): 404 or an empty contentURL injects nothing (normal),
// a query failure is a plain error (transient backoff, no E-29 counting).
func (c *Controller) singleContentURL(ctx context.Context, round *reconcileRound) (string, controller.ReconcileResult, error) {
	repo, err := c.client.GetRpmRepo(ctx, round.current.Namespace, round.current.Name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", controller.ReconcileResult{}, nil
		}
		return "", controller.ReconcileResult{}, err
	}
	if repo.Status.Repository == nil {
		return "", controller.ReconcileResult{}, nil
	}
	return repo.Status.Repository.ContentURL, controller.ReconcileResult{}, nil
}
