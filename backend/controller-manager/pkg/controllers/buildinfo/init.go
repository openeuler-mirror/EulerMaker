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
	"controller-manager/pkg/controller"
	"controller-manager/pkg/controllers/buildinfo/rpmver"
	"controller-manager/pkg/controllers/buildinfo/specparse"
	"controller-manager/pkg/serviceconfig"
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

	// Backfill Jobs that may have been created before a lost status write.
	jobs, err := c.listRoundJobs(ctx, round)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	arch := round.build.Spec.Arch

	// Persist the graph before caching it or creating graph-based Jobs.
	prefer := payloadPrefer(c.parseBuildPayload(round.key, round.current.Spec.BuildPayload))
	dcg, result, err := c.obtainDcg(ctx, round, buildSet, sources, prefer)
	if err != nil || result != (controller.ReconcileResult{}) || dcg == nil {
		return result, err
	}
	// Once persisted, the graph defines the build set even if current repository metadata has changed.
	buildSet = make(map[string]specparse.SpecDepend, dcg.Len())
	for _, name := range dcg.SortedNodes() {
		dep, ok := asm.depends[name]
		if !ok {
			return controller.ReconcileResult{}, fmt.Errorf("BuildGraph node %s has no parsed spec", name)
		}
		buildSet[name] = dep
	}
	if len(buildSet) == 0 {
		return c.completeInitEmpty(ctx, round, asm.degraded)
	}
	if result, err = c.persistBuildSetResults(ctx, round, buildSet); err != nil ||
		result != (controller.ReconcileResult{}) {
		return result, err
	}
	next := round.current.DeepCopy()
	applyDegradedConditions(&next.Status.Conditions, asm.degraded)
	c.backfillJobs(round, next, jobs, specNameSet(buildSet))
	if result, err = c.writeStatusIfChanged(ctx, round, next); err != nil || result != (controller.ReconcileResult{}) {
		return result, err
	}

	// Resolve the build-target Config only when the first Job needs it, then reuse that image for the rest of this round.
	contentURL, err := c.resolveContentURL(ctx, round, heldContentURL(round))
	if err != nil {
		return c.handleArtifactURLFailure(ctx, round, err)
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

// persistBuildSetResults establishes the complete traversal base before the first Job is created. Re-entry only
// adds missing keys; it never resets confirmed dispatches or terminal verdicts.
func (c *Controller) persistBuildSetResults(
	ctx context.Context,
	round *reconcileRound,
	buildSet map[string]specparse.SpecDepend,
) (controller.ReconcileResult, error) {
	next := round.current.DeepCopy()
	if next.Status.SpecResults == nil {
		next.Status.SpecResults = ebsv1.SpecResults{}
	}
	for name, depend := range buildSet {
		if depend.RepoName == "" || depend.SpecFileName == "" {
			return controller.ReconcileResult{}, fmt.Errorf("spec %s has no repository/file identity", name)
		}
		key := depend.RepoName + "/" + depend.SpecFileName
		if existing, ok := next.Status.SpecResults[key]; !ok || existing.Status == ebsv1.SpecPrepareFailed {
			next.Status.SpecResults[key] = ebsv1.SpecResult{SpecName: name, Status: ebsv1.SpecPrepared}
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
	image, err := buildImage(conf, round.project.Spec.Distro, round.build.Spec.Arch)
	if err != nil {
		return "", err
	}
	dispatch.image, dispatch.imageReady = image, true
	return image, nil
}

func buildImage(conf *ebsv1.BuildTargetContent, distro, arch string) (string, error) {
	if conf == nil {
		return "", fmt.Errorf("build-target Config is unavailable")
	}
	image := conf.Targets[distro].Arches[arch].Image
	if image == "" {
		return "", fmt.Errorf("build-target Config has no image for %s/%s", distro, arch)
	}
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
	ss := round.specResult(specName)
	if ss.Status != ebsv1.SpecPrepared || ss.DispatchCount > 0 {
		// Already dispatched (Job created, Running, or a Failed verdict) — re-entering init never re-dispatches nor repeats
		// bootstrap.
		return controller.ReconcileResult{}, nil
	}
	if result, err := c.checkArchSupported(ctx, round, specName, &depend, dispatch.arch); err != nil ||
		result != (controller.ReconcileResult{}) {
		return result, err
	}
	if ss = round.specResult(specName); failedSpecBuildStatus(ss.Status) {
		return controller.ReconcileResult{}, nil
	}
	if result, err := c.checkBuildRequires(ctx, round, specName, &depend, sources); err != nil ||
		result != (controller.ReconcileResult{}) {
		return result, err
	}
	if ss = round.specResult(specName); failedSpecBuildStatus(ss.Status) {
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
	result, err := c.markSpecTerminal(ctx, round, specName, ebsv1.SpecArchUnsupported, "")
	if err == nil && result == (controller.ReconcileResult{}) {
		c.logOnce(round.key, "ArchUnsupported", "spec %s marked ArchUnsupported: %s", specName, message)
	}
	return result, err
}

// checkBuildRequires checks that every buildRequires entry (buildRemoves excluded) must be available in the layered
// sources; a true miss marks the spec RpmDependsMissing without a Job.
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
	result, err := c.markSpecTerminal(ctx, round, specName, ebsv1.SpecRpmDependsMissing, message)
	if err == nil && result == (controller.ReconcileResult{}) {
		c.logOnce(round.key, "RpmDependsMissing", "spec %s marked RpmDependsMissing: %s", specName, message)
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
	if d, stop, result, err := c.loadGraph(ctx, round); stop {
		return d, result, err
	}
	// First build requires the held RpmRepo and ready metadata (the caller refreshed the sources this round).
	nodes := BuildDcgNodes(buildSet, sources, prefer)
	d := NewDcgDict(nodes)
	persisted, result, err := c.writeGraph(ctx, round, d)
	if err != nil || result != (controller.ReconcileResult{}) || persisted == nil {
		// A failed write leaves the cache untouched and prevents Job creation.
		return nil, result, err
	}
	breaks := persisted.GetBootstrapBreaks()
	if len(breaks) > 0 {
		bootstrapBreaks.Add(uint64(len(breaks)))
		c.logBreakPoints(
			round.key,
			"BootstrapBreaks",
			fmt.Sprintf("dcg built with %d nodes, break points: %d", d.Len(), len(breaks)),
			breaks,
		)
	}
	return persisted, controller.ReconcileResult{}, nil
}

// refreshRpmMetaSources refreshes the layered RPM metadata:
// the RpmRepo layer re-downloads only on a contentURL change (an empty URL is a normal empty state), bootstrap layers
// parse once for the BuildInfo lifetime. Downloads wait for recovery; consecutive parse failures stop at the configured
// threshold, while invalid configuration stops immediately.
func (c *Controller) refreshRpmMetaSources(
	ctx context.Context,
	round *reconcileRound,
) (*rpmver.RpmMetaSources, controller.ReconcileResult, error) {
	arch := round.build.Spec.Arch
	sources, ok := c.rpmMetaSources.Get(round.key)
	if !ok {
		sources = &rpmver.RpmMetaSources{}
	}
	repoBefore := sources.RepoLayer
	contentURL, err := c.resolveContentURL(ctx, round, heldContentURL(round))
	if err != nil {
		result, stopErr := c.handleArtifactURLFailure(ctx, round, err)
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
	urls, err := c.resolveBootstrapRepoURLs(ctx, round, arch)
	if err != nil {
		return c.rpmMetaUnavailable(ctx, round, metadataFailureReason(err, true), err)
	}
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
	if serviceconfig.IsError(cause) {
		return nil, controller.ReconcileResult{}, cause
	}
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

func (c *Controller) roundServices(round *reconcileRound) *serviceconfig.Round {
	if round.services == nil {
		round.services = serviceconfig.NewRound(c.services)
	}
	return round.services
}

func (c *Controller) resolveContentURL(ctx context.Context, round *reconcileRound, reference string) (string, error) {
	var readErr error
	resolved, err := artifacturl.Resolve(func(instance string) (string, error) {
		var address string
		address, readErr = c.roundServices(round).ArtifactURL(ctx, instance)
		return address, readErr
	}, reference)
	if readErr != nil {
		return "", readErr
	}
	if err != nil {
		return "", contractError{err: err}
	}
	return resolved, nil
}

func (c *Controller) handleArtifactURLFailure(ctx context.Context, round *reconcileRound, err error) (controller.ReconcileResult, error) {
	if serviceconfig.IsError(err) {
		return controller.ReconcileResult{}, err
	}
	if errors.Is(err, ErrNotFound) || !deterministicRpmRepoError(err) {
		return controller.ReconcileResult{}, err
	}
	return c.escalateStop(ctx, round, ConditionRpmRepoUnavailable, ReasonRpmRepoConfigInvalid, err.Error())
}

// listRoundJobs pages every Job of this Build.
func (c *Controller) listRoundJobs(ctx context.Context, round *reconcileRound) ([]ebsv1.Job, error) {
	if round.jobsLoaded {
		return round.jobs, nil
	}
	selector := labels.SelectorFromSet(labels.Set{ebsv1.JobBuildNameLabel: round.current.Name})
	jobs, err := c.client.ListJobs(ctx, round.current.Namespace, selector)
	if err != nil {
		return nil, err
	}
	round.jobs, round.jobsLoaded = jobs, true
	return jobs, nil
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
	if asm.err != nil {
		return controller.ReconcileResult{}, asm.err
	}
	next := round.current.DeepCopy()
	if next.Status.SpecResults == nil {
		next.Status.SpecResults = ebsv1.SpecResults{}
	}
	if !asm.incomplete && next.Status.Phase == ebsv1.BuildInfoPending {
		for key, result := range next.Status.SpecResults {
			if result.Status == ebsv1.SpecPrepareFailed {
				delete(next.Status.SpecResults, key)
			}
		}
	}
	for key, failure := range asm.failures {
		if existing, ok := next.Status.SpecResults[key]; !ok || existing.Status == ebsv1.SpecPrepareFailed {
			next.Status.SpecResults[key] = failure
		}
	}
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

// closeoutInit writes the failure condition and Completed together, leaving specResults untouched.
func (c *Controller) closeoutInit(
	ctx context.Context,
	round *reconcileRound,
	verdict *terminalVerdict,
	degraded []degradedCondition,
) (controller.ReconcileResult, error) {
	next := round.current.DeepCopy()
	applyDegradedConditions(&next.Status.Conditions, degraded)
	upsertCondition(&next.Status.Conditions, ConditionSpecDependsFillFailed, verdict.reason, verdict.message)
	next.Status.Phase = ebsv1.BuildInfoCompleted
	result, err := c.writeStatus(ctx, round, next)
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
	next := round.current.DeepCopy()
	applyDegradedConditions(&next.Status.Conditions, degraded)
	next.Status.Phase = ebsv1.BuildInfoCompleted
	result, err := c.writeStatus(ctx, round, next)
	if err == nil && result == (controller.ReconcileResult{}) {
		c.logOnce(round.key, "EmptyBuildSet", "completed: nothing to build")
		c.invalidateCaches(round.key)
	}
	return result, err
}

// specNameSet flattens a build set to its spec-name scope (Job grouping).
func specNameSet(buildSet map[string]specparse.SpecDepend) map[string]bool {
	scope := make(map[string]bool, len(buildSet))
	for name := range buildSet {
		scope[name] = true
	}
	return scope
}

// sortedSpecNames returns the specResults keys in dictionary order.
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

	jobs, err := c.listRoundJobs(ctx, round)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	if result, err = c.persistBuildSetResults(ctx, round, buildSet); err != nil ||
		result != (controller.ReconcileResult{}) {
		return result, err
	}
	next := round.current.DeepCopy()
	applyDegradedConditions(&next.Status.Conditions, asm.degraded)
	c.backfillJobs(round, next, jobs, specNameSet(buildSet))
	if result, err = c.writeStatusIfChanged(ctx, round, next); err != nil || result != (controller.ReconcileResult{}) {
		return result, err
	}

	// Single builds dispatch each spec directly, without dependency gates.
	dispatch := &roundDispatch{arch: round.build.Spec.Arch}
	for _, name := range sortedSpecNames(buildSet) {
		depend := buildSet[name]
		ss := round.specResult(name)
		if ss.Status != ebsv1.SpecPrepared || ss.DispatchCount > 0 {
			continue
		}
		if result, err = c.checkArchSupported(ctx, round, name, &depend, dispatch.arch); err != nil ||
			result != (controller.ReconcileResult{}) {
			return result, err
		}
		if ss = round.specResult(name); failedSpecBuildStatus(ss.Status) {
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
	if round.singlePreferSources != nil {
		return round.singlePreferSources, "", nil
	}
	sources := &rpmver.RpmMetaSources{}
	arch := round.build.Spec.Arch
	if err := sources.EnsureRepoLayer(ctx, rpmMetaFetch, contentURL, arch); err != nil {
		return nil, metadataFailureReason(err, false), err
	}
	urls, err := c.resolveBootstrapRepoURLs(ctx, round, arch)
	if err != nil {
		return nil, metadataFailureReason(err, true), err
	}
	if err := sources.EnsureBootstrapLayers(ctx, rpmMetaFetch, urls, arch); err != nil {
		return nil, metadataFailureReason(err, true), err
	}
	round.singlePreferSources = sources
	round.failures.RpmRepoReady()
	return sources, "", nil
}
