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

	"controller-manager/pkg/artifacturl"
	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/controllers/buildinfo/rpmver"
	"controller-manager/pkg/controllers/buildinfo/specparse"
	ebsv1 "ebs-api/ebs/v1"
)

// rpmMetaFetch downloads repository XML metadata. The git-server timeout does not apply to repository URLs.
var rpmMetaFetch = rpmver.HTTPFetcher(&http.Client{Timeout: 60 * time.Second})

// initBuildInfo initializes a Pending BuildInfo; single builds use direct dispatch.
func (c *Controller) initBuildInfo(ctx context.Context, round *reconcileRound) (controller.ReconcileResult, error) {
	if round.isSingle() {
		return c.initSingle(ctx, round)
	}

	snapshot, stop, result, err := c.currentSnapshot(ctx, round)
	if stop {
		return result, err
	}
	asm := c.assembleSpecDepends(ctx, round, snapshot)
	if result, err = c.persistAssemblyFailures(ctx, round, asm); err != nil ||
		result != (controller.ReconcileResult{}) {
		return result, err
	}
	if asm.incomplete {
		// Transient assembly gap: stay Pending and re-assemble next round.
		return controller.ReconcileResult{}, nil
	}

	// RpmRepo must be held for every non-single verdict and the first graph build (404 below the threshold leaves the
	// round unheld — dependency verdicts and the first build are pending, wait for the next round).
	if !round.rpmRepoHeld {
		c.logf(round.key, "RpmRepoNotHeld", "rpmrepo not held this round; init dispatch deferred (E-16)")
		return controller.ReconcileResult{}, nil
	}
	sources, result, err := c.refreshRpmMetaSources(ctx, round)
	if err != nil || result != (controller.ReconcileResult{}) || sources == nil {
		return result, err
	}

	buildSet, err := c.determineBuildSet(round, asm, sources.RepoLayer)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	if len(buildSet) == 0 {
		return c.completeInitEmpty(ctx, round, asm.degraded)
	}

	// Backfill Jobs that may have been created before a lost status write.
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

	arch := round.build.Spec.BuildTarget.Arch

	// Persist the graph before caching it or creating graph-based Jobs.
	prefer := payloadPrefer(c.parseBuildPayload(round.key, round.current.Spec.BuildPayload))
	dcg, result, err := c.obtainDcg(ctx, round, buildSet, sources, prefer)
	if err != nil || result != (controller.ReconcileResult{}) || dcg == nil {
		return result, err
	}
	if result, err = c.persistBuildSetSpecStatuses(ctx, round, buildSet); err != nil ||
		result != (controller.ReconcileResult{}) {
		return result, err
	}

	// Resolve the build-target Config only when the first Job needs it, then reuse that image for the rest of this round.
	contentURL, err := c.resolveContentURL(heldContentURL(round))
	if err != nil {
		return c.escalateStop(ctx, round, ConditionRpmRepoUnavailable, ReasonRpmRepoConfigInvalid, err.Error())
	}
	dispatch := &roundDispatch{arch: arch, contentURL: contentURL}
	for _, name := range dcg.SortedNodes() {
		if dcg.Node(name).InDegree() != 0 {
			continue
		}
		if result, err = c.dispatchInitSpec(ctx, round, dispatch, name, buildSet[name], snapshot, sources); err != nil ||
			result != (controller.ReconcileResult{}) {
			return result, err
		}
	}
	for _, name := range dcg.GetBootstrapBreaks() {
		if result, err = c.dispatchInitSpec(ctx, round, dispatch, name, buildSet[name], snapshot, sources); err != nil ||
			result != (controller.ReconcileResult{}) {
			return result, err
		}
	}

	// All build-set entries were persisted before dispatch; flip to Processing only after every initial dispatch candidate
	// was considered.
	next = round.current.DeepCopy()
	next.Status.Phase = ebsv1.BuildInfoProcessing
	return c.writeStatusIfChanged(ctx, round, next)
}

// persistBuildSetSpecStatuses establishes the complete traversal base before the first Job is created. Re-entry only
// adds missing keys; it never resets confirmed dispatches or terminal verdicts.
func (c *Controller) persistBuildSetSpecStatuses(
	ctx context.Context,
	round *reconcileRound,
	buildSet map[string]specparse.SpecDepend,
) (controller.ReconcileResult, error) {
	next := round.current.DeepCopy()
	if next.Status.SpecRepoNames == nil {
		next.Status.SpecRepoNames = make(map[string]string, len(buildSet))
	}
	for name, depend := range buildSet {
		if _, exists := next.Status.SpecStatus.Lookup(name); !exists {
			next.Status.SpecStatus.Set(name, ebsv1.SpecStatus{})
		}
		if _, exists := next.Status.SpecRepoNames[name]; !exists {
			next.Status.SpecRepoNames[name] = depend.RepoName
		}
	}
	return c.writeStatusIfChanged(ctx, round, next)
}

// roundDispatch carries the per-round dispatch constants resolved once and shared by every spec dispatch of the round.
type roundDispatch struct {
	arch       string
	contentURL string
	image      string
	imageReady bool
}

// ensureImage resolves the build-target Config image snapshot lazily (a read failure or a missing mapping pauses the
// round — plain error backoff, no condition, no Failed marking).
func (c *Controller) ensureImage(ctx context.Context, round *reconcileRound, dispatch *roundDispatch) (string, error) {
	if dispatch.imageReady {
		return dispatch.image, nil
	}
	conf, err := c.client.GetBuildTargetContent(ctx)
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

// dispatchInitSpec skips already-dispatched or terminal specs, checks the target architecture and build dependencies,
// then creates the Job.
func (c *Controller) dispatchInitSpec(
	ctx context.Context,
	round *reconcileRound,
	dispatch *roundDispatch,
	specName string,
	depend specparse.SpecDepend,
	snapshot *ebsv1.Snapshot,
	sources *rpmver.RpmMetaSources,
) (controller.ReconcileResult, error) {
	ss := round.current.Status.SpecStatus.Entry(specName)
	if ss.Build.Status != "" || ss.DispatchCount > 0 {
		// Already dispatched (Job created, Running, or a Failed verdict) — re-entering init never re-dispatches nor repeats
		// bootstrap.
		return controller.ReconcileResult{}, nil
	}
	if result, err := c.checkArchSupported(ctx, round, specName, &depend, dispatch.arch); err != nil ||
		result != (controller.ReconcileResult{}) {
		return result, err
	}
	if ss = round.current.Status.SpecStatus.Entry(specName); failedSpecBuildStatus(ss.Build.Status) {
		return controller.ReconcileResult{}, nil
	}
	if result, err := c.checkBuildRequires(ctx, round, specName, &depend, sources); err != nil ||
		result != (controller.ReconcileResult{}) {
		return result, err
	}
	if ss = round.current.Status.SpecStatus.Entry(specName); failedSpecBuildStatus(ss.Build.Status) {
		return controller.ReconcileResult{}, nil
	}
	image, err := c.ensureImage(ctx, round, dispatch)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	return c.dispatchSpec(ctx, round, specName, &depend, snapshot, image, dispatch.contentURL, sources)
}

// checkArchSupported applies the exclusiveArch whitelist: an arch miss marks the spec ArchUnsupported without a Job; an
// empty target arch
// (abnormal data) skips the check with a warning.
func (c *Controller) checkArchSupported(
	ctx context.Context,
	round *reconcileRound,
	specName string,
	depend *specparse.SpecDepend,
	arch string,
) (controller.ReconcileResult, error) {
	if arch == "" {
		c.logf(
			round.key,
			"TargetArchMissing",
			"build target arch empty; exclusiveArch check skipped for spec %s (E-19)",
			specName,
		)
		return controller.ReconcileResult{}, nil
	}
	if archSupported(depend, arch) {
		return controller.ReconcileResult{}, nil
	}
	message := fmt.Sprintf("target arch %q not in exclusiveArch %v", arch, depend.ExclusiveArch)
	result, err := c.markSpecTerminal(ctx, round, specName, SpecBuildArchUnsupported, "", "", "", false)
	if err == nil && result == (controller.ReconcileResult{}) {
		c.logOnce(round.key, "ArchUnsupported", "spec %s marked ArchUnsupported: %s", specName, message)
	}
	return result, err
}

// checkBuildRequires checks that every buildRequires entry (buildRemoves excluded) must be available in the layered
// sources; a true miss marks the spec Failed (RpmDependsMissing) without a Job.
func (c *Controller) checkBuildRequires(
	ctx context.Context,
	round *reconcileRound,
	specName string,
	depend *specparse.SpecDepend,
	sources *rpmver.RpmMetaSources,
) (controller.ReconcileResult, error) {
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

// obtainDcg resolves the graph from memory, persisted status, or a first build. Loading persisted state never
// re-selects break points. The first build persists the state BEFORE the cache update and any Job creation; a nil graph
// with zero result means "wait next round".
func (c *Controller) obtainDcg(
	ctx context.Context,
	round *reconcileRound,
	buildSet map[string]specparse.SpecDepend,
	sources *rpmver.RpmMetaSources,
	prefer []string,
) (*DcgDict, controller.ReconcileResult, error) {
	if d, ok := c.dcgDict.Get(round.key); ok {
		// A recovered graph clears the stale failure condition.
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
	// First build requires the held RpmRepo and ready metadata (the caller refreshed the sources this round).
	nodes := BuildDcgNodes(buildSet, sources, prefer)
	d := NewDcgDict(nodes)
	next := round.current.DeepCopy()
	next.Status.Dcg = d.ToState()
	removeCondition(&next.Status.Conditions, ConditionDcgBuildFailed)
	result, err := c.writeStatus(ctx, round, next)
	if err != nil || result != (controller.ReconcileResult{}) {
		// A failed write leaves the cache untouched and prevents Job creation.
		return nil, result, err
	}
	c.dcgDict.Set(round.key, d)
	breaks := d.GetBootstrapBreaks()
	if len(breaks) > 0 {
		bootstrapBreaks.Add(uint64(len(breaks)))
		c.logBreakPoints(
			round.key,
			"BootstrapBreaks",
			fmt.Sprintf("dcg built with %d nodes, break points: %d", d.Len(), len(breaks)),
			breaks,
		)
	}
	return d, controller.ReconcileResult{}, nil
}

// clearStaleDcgFailed removes the stale condition after any successful graph lookup or rebuild.
func (c *Controller) clearStaleDcgFailed(
	ctx context.Context,
	round *reconcileRound,
) (controller.ReconcileResult, error) {
	if findCondition(round.current.Status.Conditions, ConditionDcgBuildFailed) == nil {
		return controller.ReconcileResult{}, nil
	}
	next := round.current.DeepCopy()
	removeCondition(&next.Status.Conditions, ConditionDcgBuildFailed)
	return c.writeStatusIfChanged(ctx, round, next)
}

// refreshRpmMetaSources refreshes the layered RPM metadata:
// the RpmRepo layer re-downloads only on a contentURL change (an empty URL is a normal empty state), bootstrap layers
// parse once for the BuildInfo lifetime. Downloads wait for recovery; consecutive parse failures stop at the configured
// threshold, while invalid configuration stops immediately.
func (c *Controller) refreshRpmMetaSources(
	ctx context.Context,
	round *reconcileRound,
) (*rpmver.RpmMetaSources, controller.ReconcileResult, error) {
	arch := round.build.Spec.BuildTarget.Arch
	sources, ok := c.rpmMetaSources.Get(round.key)
	if !ok {
		sources = &rpmver.RpmMetaSources{}
	}
	repoBefore := sources.RepoLayer
	contentURL, err := c.resolveContentURL(heldContentURL(round))
	if err != nil {
		result, stopErr := c.escalateStop(
			ctx,
			round,
			ConditionRpmRepoUnavailable,
			ReasonRpmRepoConfigInvalid,
			err.Error(),
		)
		return nil, result, stopErr
	}
	if err := sources.EnsureRepoLayer(ctx, rpmMetaFetch, contentURL, arch); err != nil {
		c.logf(round.key, "RpmRepoXMLFailed", "rpmrepo metadata unavailable: %v", err)
		return c.rpmMetaUnavailable(ctx, round, metadataFailureReason(err, false), err)
	}
	if sources.RepoLayer != repoBefore {
		rpmMetaRefreshes.Inc()
	}
	bootstrapBefore := sources.BootstrapLayer
	urls := bootstrapRepoURLs(round.current.Spec.BootstrapRepo, arch)
	if err := sources.EnsureBootstrapLayers(ctx, rpmMetaFetch, urls, arch); err != nil {
		c.logf(round.key, "BootstrapRepoXMLFailed", "bootstrap repo metadata unavailable: %v", err)
		return c.rpmMetaUnavailable(ctx, round, metadataFailureReason(err, true), err)
	}
	if len(sources.BootstrapLayer) > 0 && bootstrapBefore == nil {
		rpmMetaRefreshes.Inc()
	}
	c.rpmMetaSources.Set(round.key, sources)
	round.failures.RpmRepoReady()
	if result, err := c.clearRpmRepoRetrying(ctx, round); err != nil || result != (controller.ReconcileResult{}) {
		return nil, result, err
	}
	return sources, controller.ReconcileResult{}, nil
}

func metadataFailureReason(err error, bootstrap bool) string {
	var source *rpmver.SourceError
	parse := errors.As(err, &source) && source.Kind == rpmver.FailureParse
	if bootstrap {
		if parse {
			return ReasonBootstrapRepoXMLParseFailed
		}
		return ReasonBootstrapRepoXMLUnavail
	}
	if parse {
		return ReasonRpmRepoXMLParseFailed
	}
	return ReasonRpmRepoXMLDownloadFailed
}

// rpmMetaUnavailable retries download failures, immediately stops on bad configuration, and escalates only consecutive
// XML parse failures.
func (c *Controller) rpmMetaUnavailable(
	ctx context.Context,
	round *reconcileRound,
	reason string,
	cause error,
) (*rpmver.RpmMetaSources, controller.ReconcileResult, error) {
	if deterministicRpmRepoError(cause) {
		configReason := ReasonRpmRepoConfigInvalid
		if reason == ReasonBootstrapRepoXMLUnavail || reason == ReasonBootstrapRepoXMLParseFailed {
			configReason = ReasonBootstrapRepoConfigInvalid
		}
		result, err := c.escalateStop(ctx, round, ConditionRpmRepoUnavailable, configReason, cause.Error())
		return nil, result, err
	}
	var source *rpmver.SourceError
	if !errors.As(cause, &source) || source.Kind != rpmver.FailureParse {
		round.failures.RpmRepoReady()
		result, err := c.recordRpmRepoRetrying(ctx, round, reason, cause.Error())
		return nil, result, err
	}
	_, escalated := round.failures.RpmRepoFailed(reason, cause.Error())
	if escalated {
		result, err := c.escalateRpmRepoUnavailable(ctx, round)
		return nil, result, err
	}
	result, err := c.recordRpmRepoRetrying(ctx, round, reason, cause.Error())
	return nil, result, err
}

// heldContentURL extracts the held RpmRepo contentURL (empty when unheld or the repository status is absent — a normal
// empty state).
func heldContentURL(round *reconcileRound) string {
	if !round.rpmRepoHeld || round.rpmRepo.Status.Repository == nil {
		return ""
	}
	return round.rpmRepo.Status.Repository.ContentURL
}

func (c *Controller) resolveContentURL(reference string) (string, error) {
	return artifacturl.Resolve(c.config.ArtifactManagerAddr, reference)
}

// listRoundJobs pages every Job of this Build.
func (c *Controller) listRoundJobs(ctx context.Context, round *reconcileRound) ([]ebsv1.Job, error) {
	selector := labels.SelectorFromSet(labels.Set{ebsv1.JobBuildNameLabel: round.current.Name})
	return c.client.ListJobs(ctx, round.current.Namespace, selector)
}

// writeStatusIfChanged writes only semantic status changes.
func (c *Controller) writeStatusIfChanged(
	ctx context.Context,
	round *reconcileRound,
	next *ebsv1.BuildInfo,
) (controller.ReconcileResult, error) {
	if statusMatchesIntent(&round.current.Status, &next.Status) {
		return controller.ReconcileResult{}, nil
	}
	return c.writeStatus(ctx, round, next)
}

// persistAssemblyFailures records deterministic repository failures before dispatch, including rounds that must wait
// for transient repository inputs.
func (c *Controller) persistAssemblyFailures(
	ctx context.Context,
	round *reconcileRound,
	asm *specAssembly,
) (controller.ReconcileResult, error) {
	next := round.current.DeepCopy()
	var existing []string
	if asm.incomplete {
		existing = next.Status.FailedPackages
	}
	next.Status.FailedPackages = sortedFailedPackages(existing, asm.failedRepos)
	return c.writeStatusIfChanged(ctx, round, next)
}

// applyDegradedConditions upserts this round's degradation conditions and removes the two degradation types absent this
// round (each Pending assembly fully re-evaluates them; terminal closeouts re-add their own).
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

// closeoutInit writes the failure condition and Completed together, after confirming any pending Job creation. It
// leaves specStatus untouched.
func (c *Controller) closeoutInit(
	ctx context.Context,
	round *reconcileRound,
	verdict *terminalVerdict,
	degraded []degradedCondition,
) (controller.ReconcileResult, error) {
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

// completeInitEmpty completes an empty build set and keeps degradation conditions when present.
func (c *Controller) completeInitEmpty(
	ctx context.Context,
	round *reconcileRound,
	degraded []degradedCondition,
) (controller.ReconcileResult, error) {
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

// pendingCreatesBlock prevents completion while Job creations remain uncertain.
func (c *Controller) pendingCreatesBlock(round *reconcileRound, where string) bool {
	if len(round.current.Status.PendingJobCreates) == 0 {
		return false
	}
	c.logf(
		round.key,
		"PendingCreatesBlock",
		"%s completed write blocked by %d unresolved pending job creates",
		where,
		len(round.current.Status.PendingJobCreates),
	)
	return true
}

// resolvePendingCreates checks registrations before an init closeout, which bypasses normal Job backfill. A found Job
// confirms its dispatch; a 404 leaves the entry because an in-flight create may still land. It reports whether any
// unresolved entry blocks completion.
func (c *Controller) resolvePendingCreates(
	ctx context.Context,
	round *reconcileRound,
	where string,
) (controller.ReconcileResult, bool, error) {
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
				c.logf(
					round.key,
					"JobCreateUnresolved",
					"pending job create for spec %s (job %s generation %d) unresolved at %s; entry kept, waiting (6.5.1 #5)",
					spec,
					pend.JobName,
					pend.DispatchGeneration,
					where,
				)
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
		c.logf(
			round.key,
			"PendingCreatesBlock",
			"%s completed write blocked by %d unresolved pending job creates",
			where,
			len(round.current.Status.PendingJobCreates),
		)
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

// initSingle dispatches the designated repository specs directly without a dependency graph or dependency gates.
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
	if result, err = c.persistAssemblyFailures(ctx, round, asm); err != nil ||
		result != (controller.ReconcileResult{}) {
		return result, err
	}
	if asm.incomplete {
		return controller.ReconcileResult{}, nil
	}
	buildSet := asm.depends
	if len(buildSet) == 0 {
		return c.closeoutInit(ctx, round, &terminalVerdict{
			reason: ReasonSpecifiedBuildSetEmpty,
			message: fmt.Sprintf(
				"all designated packages %v were skipped: no buildable spec",
				round.build.Spec.Packages,
			),
		}, asm.degraded)
	}

	// Repo injection source: the current same-name RpmRepo contentURL (creation-time inherited baseline; single never
	// materializes).
	contentURL, result, err := c.singleContentURL(ctx, round)
	if err != nil || result != (controller.ReconcileResult{}) {
		return result, err
	}

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
	if result, err = c.persistBuildSetSpecStatuses(ctx, round, buildSet); err != nil ||
		result != (controller.ReconcileResult{}) {
		return result, err
	}

	// Single builds dispatch each spec directly, without dependency gates.
	dispatch := &roundDispatch{arch: round.build.Spec.BuildTarget.Arch, contentURL: contentURL}
	for _, name := range sortedSpecNames(buildSet) {
		depend := buildSet[name]
		ss := round.current.Status.SpecStatus.Entry(name)
		if ss.Build.Status != "" || ss.DispatchCount > 0 {
			continue
		}
		if result, err = c.checkArchSupported(ctx, round, name, &depend, dispatch.arch); err != nil ||
			result != (controller.ReconcileResult{}) {
			return result, err
		}
		if ss = round.current.Status.SpecStatus.Entry(name); failedSpecBuildStatus(ss.Build.Status) {
			continue
		}
		image, err := c.ensureImage(ctx, round, dispatch)
		if err != nil {
			return controller.ReconcileResult{}, err
		}
		if result, err = c.dispatchSpec(ctx, round, name, &depend, snapshot, image, dispatch.contentURL, nil); err != nil ||
			result != (controller.ReconcileResult{}) {
			return result, err
		}
	}

	// Every build-set entry was persisted before dispatch; only the phase transition remains after all single specs were
	// considered.
	next = round.current.DeepCopy()
	next.Status.Phase = ebsv1.BuildInfoProcessing
	return c.writeStatusIfChanged(ctx, round, next)
}

// loadSinglePreferSources is only used when single has a configured prefer. It reads the repositories already injected
// into that Job without enabling DCG construction or dependency gates.
func (c *Controller) loadSinglePreferSources(
	ctx context.Context,
	round *reconcileRound,
	contentURL string,
) (*rpmver.RpmMetaSources, string, error) {
	sources, ok := c.rpmMetaSources.Get(round.key)
	if !ok {
		sources = &rpmver.RpmMetaSources{}
	}
	arch := round.build.Spec.BuildTarget.Arch
	if err := sources.EnsureRepoLayer(ctx, rpmMetaFetch, contentURL, arch); err != nil {
		return nil, metadataFailureReason(err, false), err
	}
	urls := bootstrapRepoURLs(round.current.Spec.BootstrapRepo, arch)
	if err := sources.EnsureBootstrapLayers(ctx, rpmMetaFetch, urls, arch); err != nil {
		return nil, metadataFailureReason(err, true), err
	}
	c.rpmMetaSources.Set(round.key, sources)
	round.failures.RpmRepoReady()
	return sources, "", nil
}

// singleContentURL reads the current same-name RpmRepo for the single Repo injection: 404 or an empty contentURL
// injects nothing; temporary query failures retry, while rejected requests and invalid URLs stop dispatch.
func (c *Controller) singleContentURL(
	ctx context.Context,
	round *reconcileRound,
) (string, controller.ReconcileResult, error) {
	repo, err := c.client.GetRpmRepo(ctx, round.current.Namespace, round.current.Name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			round.failures.RpmRepoReady()
			return c.singleContentURLReady(ctx, round, "")
		}
		if deterministicRpmRepoError(err) {
			result, stopErr := c.escalateStop(
				ctx,
				round,
				ConditionRpmRepoUnavailable,
				ReasonRpmRepoQueryRejected,
				err.Error(),
			)
			return "", result, stopErr
		}
		round.failures.RpmRepoReady()
		result, writeErr := c.recordRpmRepoRetrying(ctx, round, ReasonRpmRepoQueryFailed, err.Error())
		if writeErr != nil || result != (controller.ReconcileResult{}) {
			return "", result, writeErr
		}
		return "", controller.ReconcileResult{}, err
	}
	if repo.Status.Repository == nil {
		return c.singleContentURLReady(ctx, round, "")
	}
	contentURL, err := c.resolveContentURL(repo.Status.Repository.ContentURL)
	if err != nil {
		result, stopErr := c.escalateStop(
			ctx,
			round,
			ConditionRpmRepoUnavailable,
			ReasonRpmRepoConfigInvalid,
			err.Error(),
		)
		return "", result, stopErr
	}
	return c.singleContentURLReady(ctx, round, contentURL)
}

// Single builds with prefer clear the retry condition only after metadata is ready; without prefer, a successful
// repository lookup is enough.
func (c *Controller) singleContentURLReady(
	ctx context.Context,
	round *reconcileRound,
	contentURL string,
) (string, controller.ReconcileResult, error) {
	if len(payloadPrefer(c.parseBuildPayload(round.key, round.current.Spec.BuildPayload))) > 0 {
		return contentURL, controller.ReconcileResult{}, nil
	}
	result, err := c.clearRpmRepoRetrying(ctx, round)
	return contentURL, result, err
}
