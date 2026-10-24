package buildinfo

import (
	"context"
	"fmt"
	"sort"

	"controller-manager/pkg/controller"
	"controller-manager/pkg/controllers/buildinfo/rpmver"
	ebsv1 "ebs-api/ebs/v1"
)

// advanceBuildInfo advances a Processing BuildInfo. Single builds use a simplified path without dependency graph or
// repository metadata reads.
func (c *Controller) advanceBuildInfo(ctx context.Context, round *reconcileRound) (controller.ReconcileResult, error) {
	if round.isSingle() {
		return c.advanceSingle(ctx, round)
	}

	// Do not advance an incomplete or inconsistent status snapshot.
	if round.current.Status.SpecResults.Len() == 0 {
		c.logf(
			round.key,
			"SpecResultsEmpty",
			"processing buildinfo with empty specResults (E-01); waiting for manual intervention, phase kept",
		)
		return controller.ReconcileResult{}, nil
	}

	jobs, err := c.listRoundJobs(ctx, round)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	next := round.current.DeepCopy()
	bySpec := c.backfillJobs(round, next, jobs, specResultsScope(round.current))
	if result, err := c.writeStatusIfChanged(ctx, round, next); err != nil || result != (controller.ReconcileResult{}) {
		return result, err
	}

	snapshot, stop, result, err := c.currentSnapshot(ctx, round)
	if stop {
		return result, err
	}
	asm := c.assembleSpecDepends(ctx, round, snapshot)
	if asm.err != nil {
		return controller.ReconcileResult{}, asm.err
	}
	if asm.incomplete {
		return controller.ReconcileResult{}, nil
	}

	// Without a usable RpmRepo, defer dispatch but still check completion.
	var sources *rpmver.RpmMetaSources
	dispatchReady := false
	if round.rpmRepoHeld {
		sources, result, err = c.refreshRpmMetaSources(ctx, round)
		if err != nil || result != (controller.ReconcileResult{}) {
			return result, err
		}
		dispatchReady = sources != nil
	} else {
		c.logf(round.key, "RpmRepoNotHeld", "rpmrepo not held this round; advance dispatch deferred (E-16)")
	}

	// Processing requires a graph built in an earlier phase.
	dcg, result, err := c.advanceDcg(ctx, round)
	if err != nil || result != (controller.ReconcileResult{}) || dcg == nil {
		return result, err
	}

	if dispatchReady {
		// Persist new install edges before using them for dispatch.
		dcg, result, err = c.appendInstallEdges(ctx, round, dcg, sources)
		if err != nil || result != (controller.ReconcileResult{}) || dcg == nil {
			return result, err
		}
		if result, err = c.advanceDownstream(ctx, round, dcg, asm, snapshot, sources, bySpec); err != nil ||
			result != (controller.ReconcileResult{}) {
			return result, err
		}
	}

	return c.checkCompletion(ctx, round, dcg)
}

// advanceDcg loads the persisted graph. Processing never creates a new graph.
func (c *Controller) advanceDcg(
	ctx context.Context,
	round *reconcileRound,
) (*DcgDict, controller.ReconcileResult, error) {
	d, _, result, err := c.loadGraph(ctx, round)
	return d, result, err
}

// appendInstallEdges checks every spec with install.status=Failed (cycle nodes already at 2 dispatches excluded)
// reverse-looks-up a provider per missing dep against the current-round layered sources; an edge is appended on the
// candidate graph only when the provider is a build-set spec, non-terminal and not already an install upstream. New
// cycles get additional break points (initial picks never re-selected). The candidate state is persisted before the
// in-memory graph is replaced; a failed write leaves both untouched.
func (c *Controller) appendInstallEdges(
	ctx context.Context,
	round *reconcileRound,
	dcg *DcgDict,
	sources *rpmver.RpmMetaSources,
) (*DcgDict, controller.ReconcileResult, error) {
	prefer := payloadPrefer(c.parseBuildPayload(round.key, round.current.Spec.BuildPayload))
	var candidate *DcgDict
	changed := 0
	for _, spec := range sortedSpecNames(round.current.Status.SpecResults.Entries()) {
		ss := round.specResult(spec)
		if ss.Status != ebsv1.SpecInstallFailed {
			continue
		}
		if dcg.IsCycleNode(spec) && ss.DispatchCount >= 2 {
			continue // terminal cycle node; the second-layer fallback owns the residue
		}
		node := dcg.Node(spec)
		if node == nil {
			continue
		}
		for _, depName := range sortedSpecNames(ss.MissingDeps) {
			vc := ss.MissingDeps[depName].VersionRequests
			selection, ok := sources.FindProvider(depName, vc, prefer)
			if !ok {
				continue // unresolved provider: no edge
			}
			provider := selection.Provider.SpecName
			if dcg.Node(provider) == nil {
				continue // provider not in this round's build set
			}
			if pSS := round.specResult(provider); terminalSpecBuildStatus(pSS.Status) {
				continue // provider already terminal: no edge, no re-dispatch
			}
			if _, dup := node.InstallInDep[provider]; dup {
				continue // idempotence: the edge already exists
			}
			if candidate == nil {
				candidate = dcg.Clone()
			}
			if candidate.AddInstallEdge(spec, provider, vc) {
				changed++
			}
		}
	}
	if candidate == nil || changed == 0 {
		return dcg, controller.ReconcileResult{}, nil
	}
	newBreaks := candidate.RefreshCyclesAndBreaks()
	persisted, result, err := c.writeGraph(ctx, round, candidate)
	if err != nil || result != (controller.ReconcileResult{}) || persisted == nil {
		// Keep the in-memory graph unchanged if persistence fails.
		return nil, result, err
	}
	edgesAdded.Add(uint64(changed))
	if len(newBreaks) > 0 {
		bootstrapBreaks.Add(uint64(len(newBreaks)))
		c.logBreakPoints(
			round.key,
			"RuntimeBootstrapBreaks",
			fmt.Sprintf("install edge appends added %d edges; new cycle break points: %d", changed, len(newBreaks)),
			newBreaks,
		)
	} else {
		c.logOnce(round.key, "InstallEdgesAdded", "install edge appends added %d edges", changed)
	}
	return persisted, controller.ReconcileResult{}, nil
}

// advanceDownstream traverses specs in graph order, skipping those already complete or running. An initial bootstrap
// dispatch skips upstream gates but still checks architecture and build-requires availability.
func (c *Controller) advanceDownstream(
	ctx context.Context,
	round *reconcileRound,
	dcg *DcgDict,
	asm *specAssembly,
	snapshot *ebsv1.Snapshot,
	sources *rpmver.RpmMetaSources,
	bySpec map[string][]ebsv1.Job,
) (controller.ReconcileResult, error) {
	required := dcg.DispatchRequirements()
	contentURL, err := c.resolveContentURL(ctx, round, heldContentURL(round))
	if err != nil {
		return c.handleArtifactURLFailure(ctx, round, err)
	}
	dispatch := &roundDispatch{arch: round.build.Spec.BuildTarget.Arch, contentURL: contentURL}
	for _, name := range dcg.SortedNodes() {
		ss := round.specResult(name)
		node := dcg.Node(name)
		if ss.DispatchCount >= effectiveRequired(dcg, round, name, required) {
			continue
		}
		if ss.Status == ebsv1.SpecRunning || failedSpecBuildStatus(ss.Status) {
			continue // do not redispatch an in-flight or failed spec
		}
		depend, ok := asm.depends[name]
		if !ok {
			// Defensive: graph nodes come from the build set, a subset of the assembled view; a miss means the views diverged —
			// never craft a payload, wait for the next round's assembly.
			c.logf(
				round.key,
				"SpecDependMissing",
				"graph node %s missing from this round's assembly; dispatch skipped",
				name,
			)
			continue
		}
		if !(node.BootstrapBreak && ss.DispatchCount == 0) {
			// Wait until every direct upstream is terminal.
			if !upstreamsTerminal(dcg, round, name) {
				continue
			}
			if !hasFailedUpstream(dcg, round, name) {
				// A failed upstream cancels the rebuild consistency requirement.
				if !rebuildConsistencySatisfied(dcg, round, name, ss, required) {
					continue
				}
			}
			// Successful upstream outputs must be published before dispatch.
			if !upstreamOutputsPublished(round, dcg, name, bySpec) {
				continue
			}
		}
		// Unsupported architectures fail without creating a Job.
		if result, err := c.checkArchSupported(ctx, round, name, &depend, dispatch.arch); err != nil ||
			result != (controller.ReconcileResult{}) {
			return result, err
		}
		if ss = round.specResult(name); failedSpecBuildStatus(ss.Status) {
			continue
		}
		// Build-requires availability is checked even for bootstrap dispatches.
		if result, err := c.checkBuildRequires(ctx, round, name, &depend, sources); err != nil ||
			result != (controller.ReconcileResult{}) {
			return result, err
		}
		if ss = round.specResult(name); failedSpecBuildStatus(ss.Status) {
			continue
		}
		// Resolve the build-target Config once per round when needed.
		image, err := c.ensureImage(ctx, round, dispatch)
		if err != nil {
			return controller.ReconcileResult{}, err
		}
		if result, err := c.dispatchSpec(
			ctx, round, name, &depend, snapshot, image, dispatch.contentURL, sources,
		); err != nil ||
			result != (controller.ReconcileResult{}) {
			return result, err
		}
	}
	return controller.ReconcileResult{}, nil
}

// upstreamNames returns the merged direct-upstream set (inDep ∪ installInDep) in dictionary order.
func upstreamNames(node *DcgNode) []string {
	names := make([]string, 0, len(node.InDep)+len(node.InstallInDep))
	for name := range node.InDep {
		names = append(names, name)
	}
	for name := range node.InstallInDep {
		if _, dup := node.InDep[name]; !dup {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// upstreamsTerminal reports whether every direct upstream has succeeded or failed.
func upstreamsTerminal(dcg *DcgDict, round *reconcileRound, spec string) bool {
	for _, up := range upstreamNames(dcg.Node(spec)) {
		st := round.specResult(up).Status
		if !terminalSpecBuildStatus(st) {
			return false
		}
	}
	return true
}

// hasFailedUpstream reports whether any direct upstream failed.
func hasFailedUpstream(dcg *DcgDict, round *reconcileRound, spec string) bool {
	for _, up := range upstreamNames(dcg.Node(spec)) {
		if failedSpecBuildStatus(round.specResult(up).Status) {
			return true
		}
	}
	return false
}

// rebuildConsistencySatisfied checks whether a non-break-point node may be dispatched again. An off-cycle node's first
// dispatch with cycle upstreams waits for those cycle upstreams to be Succeeded at their effective required (off-cycle
// upstreams follow the normal terminal + publish gates). The break point's own rebuild is exempt.
func rebuildConsistencySatisfied(
	dcg *DcgDict,
	round *reconcileRound,
	spec string,
	ss ebsv1.SpecResult,
	required map[string]int64,
) bool {
	node := dcg.Node(spec)
	if ss.DispatchCount >= 1 && !node.BootstrapBreak {
		for _, up := range upstreamNames(node) {
			upSS := round.specResult(up)
			if !upSS.Status.BuildSucceeded() ||
				upSS.DispatchCount < effectiveRequired(dcg, round, up, required) {
				return false
			}
		}
		return true
	}
	if ss.DispatchCount == 0 && !dcg.IsCycleNode(spec) {
		for _, up := range upstreamNames(node) {
			if !dcg.IsCycleNode(up) {
				continue
			}
			upSS := round.specResult(up)
			if !upSS.Status.BuildSucceeded() ||
				upSS.DispatchCount < effectiveRequired(dcg, round, up, required) {
				return false
			}
		}
	}
	return true
}

// upstreamOutputsPublished checks publish confirmation:
// for every Succeeded direct upstream, the latest-generation Job's name must be a member of the same-name RpmRepo
// status.repository.sourceJobNames (the only publish credential). Failed upstreams are skipped (their Job is never
// consumed). The RpmRepo object is the guard-held one, and Jobs come from this round's List — no extra queries.
func upstreamOutputsPublished(round *reconcileRound, dcg *DcgDict, spec string, bySpec map[string][]ebsv1.Job) bool {
	published := map[string]bool{}
	if round.rpmRepoHeld && round.rpmRepo.Status.Repository != nil {
		for _, name := range round.rpmRepo.Status.Repository.SourceJobNames {
			published[name] = true
		}
	}
	for _, up := range upstreamNames(dcg.Node(spec)) {
		if !round.specResult(up).Status.BuildSucceeded() {
			continue
		}
		group := bySpec[up]
		if len(group) == 0 {
			return false
		}
		if !published[latestJob(group).Name] {
			return false
		}
	}
	return true
}

// effectiveRequired applies a relaxation: any Failed direct upstream
// (inDep ∪ installInDep) cancels the rebuild — the effective required is 1;
// otherwise the DispatchRequirements value (cycle 2 / normal 1, 0 defaults to 1 for graph-less single paths).
func effectiveRequired(dcg *DcgDict, round *reconcileRound, spec string, required map[string]int64) int64 {
	req := required[spec]
	if req == 0 {
		req = 1
	}
	if dcg == nil {
		return req
	}
	if node := dcg.Node(spec); node != nil {
		for _, up := range upstreamNames(node) {
			if failedSpecBuildStatus(round.specResult(up).Status) {
				return 1
			}
		}
	}
	return req
}

// checkCompletion requires every spec to be terminal and every successful spec to reach its required dispatch count.
// It writes Completed; source-keyed spec results carry the verdict.
func (c *Controller) checkCompletion(
	ctx context.Context,
	round *reconcileRound,
	dcg *DcgDict,
) (controller.ReconcileResult, error) {
	var required map[string]int64
	if dcg != nil {
		required = dcg.DispatchRequirements()
	}
	var failed []string
	for key, result := range round.current.Status.SpecResults {
		if !result.Status.IsTerminal() {
			return controller.ReconcileResult{}, nil
		}
		if result.Status.BuildSucceeded() &&
			result.DispatchCount < effectiveRequired(dcg, round, result.SpecName, required) {
			return controller.ReconcileResult{}, nil
		}
		if result.Status != ebsv1.SpecSucceeded {
			failed = append(failed, key)
		}
	}
	next := round.current.DeepCopy()
	removeCondition(&next.Status.Conditions, ConditionRpmRepoRetrying)
	next.Status.Phase = ebsv1.BuildInfoCompleted
	result, err := c.writeStatus(ctx, round, next)
	if err == nil && result == (controller.ReconcileResult{}) {
		c.logOnce(
			round.key,
			"BuildInfoCompleted",
			"completed: %d specs, %d failed",
			round.current.Status.SpecResults.Len(),
			len(failed),
		)
		c.invalidateCaches(round.key)
	}
	return result, err
}

// advanceSingle runs the single-type Processing path: backfill plus the completion check — no Snapshot/RpmMeta reads,
// no graph, no gates.
func (c *Controller) advanceSingle(ctx context.Context, round *reconcileRound) (controller.ReconcileResult, error) {
	if round.current.Status.SpecResults.Len() == 0 {
		c.logf(
			round.key,
			"SpecResultsEmpty",
			"processing single buildinfo with empty specResults (E-01); waiting for manual intervention, phase kept",
		)
		return controller.ReconcileResult{}, nil
	}
	jobs, err := c.listRoundJobs(ctx, round)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	next := round.current.DeepCopy()
	c.backfillJobs(round, next, jobs, specResultsScope(round.current))
	if result, err := c.writeStatusIfChanged(ctx, round, next); err != nil || result != (controller.ReconcileResult{}) {
		return result, err
	}
	return c.checkCompletion(ctx, round, nil)
}

// closeStoppedResults records undispatched work as failed before the stop-dispatch closeout.
func (c *Controller) closeStoppedResults(ctx context.Context, round *reconcileRound, next *ebsv1.BuildInfo) error {
	marker := stopCondition(next.Status.Conditions)
	reason, message := "DispatchStopped", "dispatch stopped before this spec completed"
	if marker != nil {
		reason, message = marker.Reason, marker.Message
	}
	if next.Status.Phase == ebsv1.BuildInfoPending || len(next.Status.SpecResults.Entries()) == 0 {
		repos := round.build.Spec.Packages
		if !round.isSingle() && len(repos) == 0 {
			snapshot, err := c.client.GetSnapshot(ctx, next.Namespace, next.Name, "metadata,spec.packageRepos.name")
			if err != nil {
				return fmt.Errorf("read stopped build repository scope: %w", err)
			}
			for _, repo := range snapshot.Spec.PackageRepos {
				repos = append(repos, repo.Name)
			}
		}
		if next.Status.SpecResults == nil {
			next.Status.SpecResults = ebsv1.SpecResults{}
		}
		represented := map[string]bool{}
		for key := range next.Status.SpecResults {
			represented[ebsv1.SpecResultRepo(key)] = true
		}
		for _, repo := range repos {
			if represented[repo] {
				continue
			}
			result := ebsv1.SpecResult{Status: ebsv1.SpecPrepareFailed}
			specCondition(&result, "PreparationStopped", reason, message)
			next.Status.SpecResults[repo+"/"] = result
		}
	}
	for key, result := range next.Status.SpecResults {
		if !result.Status.IsTerminal() {
			result.Status = ebsv1.SpecBuildFailed
			specCondition(&result, "DispatchStopped", reason, message)
			next.Status.SpecResults[key] = result
		}
	}
	return nil
}

// convergeToCompleted waits for created Jobs to finish without further dispatch, retaining the stop marker.
func (c *Controller) convergeToCompleted(
	ctx context.Context,
	round *reconcileRound,
) (controller.ReconcileResult, error) {
	jobs, err := c.listRoundJobs(ctx, round)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	next := round.current.DeepCopy()
	c.backfillJobs(round, next, jobs, specResultsScope(round.current))
	if result, err := c.writeStatusIfChanged(ctx, round, next); err != nil || result != (controller.ReconcileResult{}) {
		return result, err
	}

	if c.jobsAwaitingTerminal(round, jobs) {
		return controller.ReconcileResult{}, nil
	}
	marker := stopCondition(round.current.Status.Conditions)
	next = round.current.DeepCopy()
	if err := c.closeStoppedResults(ctx, round, next); err != nil {
		return controller.ReconcileResult{}, err
	}
	next.Status.Phase = ebsv1.BuildInfoCompleted
	result, err := c.writeStatus(ctx, round, next)
	if err == nil && result == (controller.ReconcileResult{}) {
		reason := "unknown"
		if marker != nil {
			reason = marker.Reason
			switch marker.Type {
			case ConditionRpmRepoUnavailable:
				rpmRepoEscalations.Inc()
			case ConditionSnapshotUnavailable:
				snapshotEscalations.Inc()
			}
		}
		c.logOnce(
			round.key,
			"StopConvergedCompleted",
			"completed after stop-dispatch (%s): all created jobs terminal",
			reason,
		)
		c.invalidateCaches(round.key)
	}
	return result, err
}

// jobsAwaitingTerminal waits for all Jobs of this BuildInfo incarnation to become terminal. Pending, Running, empty,
// and unknown phases keep it waiting; Jobs of an older incarnation do not block convergence.
func (c *Controller) jobsAwaitingTerminal(round *reconcileRound, jobs []ebsv1.Job) bool {
	waiting := false
	scan := func(job *ebsv1.Job) {
		switch job.Status.Phase {
		case ebsv1.JobSucceeded, ebsv1.JobFailed, ebsv1.JobAborted:
			// terminal
		case ebsv1.JobPending, ebsv1.JobRunning:
			waiting = true
		default:
			waiting = true
			c.logf(
				round.key,
				"UnknownJobPhase",
				"job %s phase %q unknown; completion waits (6.5 step 4)",
				job.Name,
				job.Status.Phase,
			)
		}
	}
	own := filterJobsByIdentity(jobs, string(round.current.UID))
	for i := range own {
		scan(&own[i])
	}
	return waiting
}

// specResultsScope returns the spec names eligible for Job result backfill.
func specResultsScope(buildInfo *ebsv1.BuildInfo) map[string]bool {
	scope := make(map[string]bool, buildInfo.Status.SpecResults.Len())
	for name := range buildInfo.Status.SpecResults.Entries() {
		scope[name] = true
	}
	return scope
}
