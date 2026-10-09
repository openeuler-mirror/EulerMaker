package buildinfo

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"controller-manager/pkg/controller"
	"controller-manager/pkg/controllers/buildinfo/rpmver"
	ebsv1 "ebs-api/ebs/v1"
)

// advanceBuildInfo advances a Processing BuildInfo. Single builds use a
// simplified path without dependency graph or repository metadata reads.
func (c *Controller) advanceBuildInfo(ctx context.Context, round *reconcileRound) (controller.ReconcileResult, error) {
	if round.isSingle() {
		return c.advanceSingle(ctx, round)
	}

	// Do not advance an incomplete or inconsistent status snapshot.
	if round.current.Status.SpecStatus.Len() == 0 {
		c.logf(round.key, "SpecStatusEmpty", "processing buildinfo with empty specStatus (E-01); waiting for manual intervention, phase kept")
		return controller.ReconcileResult{}, nil
	}

	jobs, err := c.listRoundJobs(ctx, round)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	next := round.current.DeepCopy()
	bySpec := c.backfillJobs(round, next, jobs, specStatusScope(round.current), false)
	if result, err := c.writeStatusIfChanged(ctx, round, next); err != nil || result != (controller.ReconcileResult{}) {
		return result, err
	}

	snapshot, stop, result, err := c.currentSnapshot(ctx, round)
	if stop {
		return result, err
	}
	asm := c.assembleSpecDepends(ctx, round, snapshot)
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
		if err != nil || result != (controller.ReconcileResult{}) {
			return result, err
		}
		if result, err = c.advanceDownstream(ctx, round, dcg, asm, snapshot, sources, bySpec); err != nil || result != (controller.ReconcileResult{}) {
			return result, err
		}
	}

	return c.checkCompletion(ctx, round, dcg, asm)
}

// advanceDcg loads the graph from cache or BuildInfo status. A missing graph
// in Processing is an anomaly; it is not rebuilt in this phase.
func (c *Controller) advanceDcg(ctx context.Context, round *reconcileRound) (*DcgDict, controller.ReconcileResult, error) {
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
	c.logf(round.key, "DcgMissing", "processing buildinfo without a persisted dcg; no first build in advance (15.9), waiting")
	next := round.current.DeepCopy()
	upsertCondition(&next.Status.Conditions, ConditionDcgBuildFailed, ReasonDcgBuildFailed, "processing buildinfo without a persisted dcg")
	result, err := c.writeStatusIfChanged(ctx, round, next)
	return nil, result, err
}

// appendInstallEdges checks every spec
// with install.status=Failed (cycle nodes already at 2 dispatches excluded)
// reverse-looks-up a provider per missing dep against the current-round
// layered sources; an edge is appended on the candidate graph only when the
// provider is a build-set spec, non-terminal and not already an install
// upstream. New cycles get additional break points (initial picks never
// re-selected). The candidate state is persisted before the in-memory
// graph is replaced; a failed write leaves both untouched.
func (c *Controller) appendInstallEdges(ctx context.Context, round *reconcileRound, dcg *DcgDict, sources *rpmver.RpmMetaSources) (*DcgDict, controller.ReconcileResult, error) {
	prefer := payloadPrefer(c.parseBuildPayload(round.key, round.current.Spec.BuildPayload))
	var candidate *DcgDict
	changed := 0
	for _, spec := range sortedSpecNames(round.current.Status.SpecStatus.Build) {
		ss := round.current.Status.SpecStatus.Entry(spec)
		if ss.Install.Status != SpecBuildFailed {
			continue
		}
		if dcg.IsCycleNode(spec) && ss.DispatchCount >= 2 {
			continue // terminal cycle node; the second-layer fallback owns the residue
		}
		node := dcg.Node(spec)
		if node == nil {
			continue
		}
		for _, depName := range sortedSpecNames(ss.Install.MissingDeps) {
			vc := ss.Install.MissingDeps[depName].VersionRequests
			selection, ok := sources.FindProvider(depName, vc, prefer)
			if !ok {
				continue // unresolved provider: no edge
			}
			provider := selection.Provider.SpecName
			if dcg.Node(provider) == nil {
				continue // provider not in this round's build set
			}
			if pSS := round.current.Status.SpecStatus.Entry(provider); terminalSpecBuildStatus(pSS.Build.Status) {
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
	next := round.current.DeepCopy()
	next.Status.Dcg = candidate.ToState()
	result, err := c.writeStatus(ctx, round, next)
	if err != nil || result != (controller.ReconcileResult{}) {
		// Keep the in-memory graph unchanged if persistence fails.
		return dcg, result, err
	}
	c.dcgDict.Set(round.key, candidate)
	edgesAdded.Add(uint64(changed))
	if len(newBreaks) > 0 {
		bootstrapBreaks.Add(uint64(len(newBreaks)))
		c.logBreakPoints(round.key, "RuntimeBootstrapBreaks", fmt.Sprintf("install edge appends added %d edges; new cycle break points: %d", changed, len(newBreaks)), newBreaks)
	} else {
		c.logOnce(round.key, "InstallEdgesAdded", "install edge appends added %d edges", changed)
	}
	return candidate, controller.ReconcileResult{}, nil
}

// advanceDownstream traverses specs in graph order, skipping those already
// complete or running. An initial bootstrap dispatch skips upstream gates but
// still checks architecture and build-requires availability.
func (c *Controller) advanceDownstream(ctx context.Context, round *reconcileRound, dcg *DcgDict, asm *specAssembly, snapshot *ebsv1.Snapshot, sources *rpmver.RpmMetaSources, bySpec map[string][]ebsv1.Job) (controller.ReconcileResult, error) {
	required := dcg.DispatchRequirements()
	contentURL, err := c.resolveContentURL(heldContentURL(round))
	if err != nil {
		return c.escalateStop(ctx, round, ConditionRpmRepoUnavailable, ReasonRpmRepoConfigInvalid, err.Error())
	}
	dispatch := &roundDispatch{arch: round.build.Spec.BuildTarget.Arch, contentURL: contentURL}
	for _, name := range dcg.SortedNodes() {
		ss := round.current.Status.SpecStatus.Entry(name)
		node := dcg.Node(name)
		if ss.DispatchCount >= effectiveRequired(dcg, round, name, required) {
			continue
		}
		if ss.Build.Status == SpecBuildRunning || failedSpecBuildStatus(ss.Build.Status) {
			continue // do not redispatch an in-flight or failed spec
		}
		depend, ok := asm.depends[name]
		if !ok {
			// Defensive: graph nodes come from the build set, a subset of the
			// assembled view; a miss means the views diverged — never craft a
			// payload, wait for the next round's assembly.
			c.logf(round.key, "SpecDependMissing", "graph node %s missing from this round's assembly; dispatch skipped", name)
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
		if result, err := c.checkArchSupported(ctx, round, name, &depend, dispatch.arch); err != nil || result != (controller.ReconcileResult{}) {
			return result, err
		}
		if ss = round.current.Status.SpecStatus.Entry(name); failedSpecBuildStatus(ss.Build.Status) {
			continue
		}
		// Build-requires availability is checked even for bootstrap dispatches.
		if result, err := c.checkBuildRequires(ctx, round, name, &depend, sources); err != nil || result != (controller.ReconcileResult{}) {
			return result, err
		}
		if ss = round.current.Status.SpecStatus.Entry(name); failedSpecBuildStatus(ss.Build.Status) {
			continue
		}
		// Resolve the build-target Config once per round when needed.
		image, err := c.ensureImage(ctx, round, dispatch)
		if err != nil {
			return controller.ReconcileResult{}, err
		}
		if result, err := c.dispatchSpec(ctx, round, name, &depend, snapshot, image, dispatch.contentURL, sources); err != nil || result != (controller.ReconcileResult{}) {
			return result, err
		}
	}
	return controller.ReconcileResult{}, nil
}

// upstreamNames returns the merged direct-upstream set (inDep ∪ installInDep)
// in dictionary order.
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
		st := round.current.Status.SpecStatus.Entry(up).Build.Status
		if !terminalSpecBuildStatus(st) {
			return false
		}
	}
	return true
}

// hasFailedUpstream reports whether any direct upstream failed.
func hasFailedUpstream(dcg *DcgDict, round *reconcileRound, spec string) bool {
	for _, up := range upstreamNames(dcg.Node(spec)) {
		if failedSpecBuildStatus(round.current.Status.SpecStatus.Entry(up).Build.Status) {
			return true
		}
	}
	return false
}

// rebuildConsistencySatisfied checks whether a non-break-point node may be
// dispatched again. An off-cycle node's first
// dispatch with cycle upstreams waits for those cycle upstreams to be
// Succeeded at their effective required (off-cycle upstreams follow the
// normal terminal + publish gates). The break point's own rebuild is exempt.
func rebuildConsistencySatisfied(dcg *DcgDict, round *reconcileRound, spec string, ss ebsv1.SpecStatus, required map[string]int64) bool {
	node := dcg.Node(spec)
	if ss.DispatchCount >= 1 && !node.BootstrapBreak {
		for _, up := range upstreamNames(node) {
			upSS := round.current.Status.SpecStatus.Entry(up)
			if upSS.Build.Status != SpecBuildSucceeded || upSS.DispatchCount < effectiveRequired(dcg, round, up, required) {
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
			upSS := round.current.Status.SpecStatus.Entry(up)
			if upSS.Build.Status != SpecBuildSucceeded || upSS.DispatchCount < effectiveRequired(dcg, round, up, required) {
				return false
			}
		}
	}
	return true
}

// upstreamOutputsPublished checks publish confirmation:
// for every Succeeded direct upstream, the latest-generation Job's name must
// be a member of the same-name RpmRepo status.repository.sourceJobNames (the
// only publish credential). Failed upstreams are skipped (their Job is never
// consumed). The RpmRepo object is the guard-held one, and Jobs come
// from this round's List — no extra queries.
func upstreamOutputsPublished(round *reconcileRound, dcg *DcgDict, spec string, bySpec map[string][]ebsv1.Job) bool {
	published := map[string]bool{}
	if round.rpmRepoHeld && round.rpmRepo.Status.Repository != nil {
		for _, name := range round.rpmRepo.Status.Repository.SourceJobNames {
			published[name] = true
		}
	}
	for _, up := range upstreamNames(dcg.Node(spec)) {
		if round.current.Status.SpecStatus.Entry(up).Build.Status != SpecBuildSucceeded {
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
// otherwise the DispatchRequirements value (cycle 2 / normal 1, 0 defaults
// to 1 for graph-less single paths).
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
			if failedSpecBuildStatus(round.current.Status.SpecStatus.Entry(up).Build.Status) {
				return 1
			}
		}
	}
	return req
}

// checkCompletion requires every spec to be terminal, every successful spec
// to reach its required dispatch count, and no pending Job creations. It
// writes Completed; spec statuses and failedPackages carry the result.
func (c *Controller) checkCompletion(ctx context.Context, round *reconcileRound, dcg *DcgDict, asm *specAssembly) (controller.ReconcileResult, error) {
	var required map[string]int64
	if dcg != nil {
		required = dcg.DispatchRequirements()
	}
	var failed []string
	for _, spec := range sortedSpecNames(round.current.Status.SpecStatus.Build) {
		ss := round.current.Status.SpecStatus.Entry(spec)
		if !terminalSpecBuildStatus(ss.Build.Status) {
			return controller.ReconcileResult{}, nil // not all terminal
		}
		if ss.Build.Status == SpecBuildSucceeded && ss.DispatchCount < effectiveRequired(dcg, round, spec, required) {
			return controller.ReconcileResult{}, nil // terminal but short of the effective required
		}
		if failedSpecBuildStatus(ss.Build.Status) {
			failed = append(failed, spec)
		}
	}
	if blocked := c.pendingCreatesBlock(round, "completion"); blocked {
		return controller.ReconcileResult{}, nil
	}
	needsRepoMap := len(failed) > 0
	for _, ss := range round.current.Status.SpecStatus.Install {
		needsRepoMap = needsRepoMap || ss.Status == SpecBuildFailed
	}
	if asm == nil && needsRepoMap {
		// The single path deliberately skips Snapshot reads while Jobs are in
		// flight; recover the spec-to-repository map only for final failures.
		if cached, ok := c.specDependsCache.Get(round.key); ok {
			asm = &specAssembly{depends: cached}
		} else {
			snapshot, stop, result, err := c.currentSnapshot(ctx, round)
			if stop {
				return result, err
			}
			asm = c.assembleSpecDepends(ctx, round, snapshot)
			if asm.incomplete {
				return controller.ReconcileResult{}, nil
			}
		}
	}
	next := round.current.DeepCopy()
	failedRepos := map[string]struct{}{}
	if asm != nil {
		for repo := range asm.failedRepos {
			failedRepos[repo] = struct{}{}
		}
		for _, spec := range sortedSpecNames(round.current.Status.SpecStatus.Build) {
			ss := round.current.Status.SpecStatus.Entry(spec)
			if !failedSpecBuildStatus(ss.Build.Status) && ss.Install.Status != SpecBuildFailed {
				continue
			}
			depend, ok := asm.depends[spec]
			if !ok || depend.RepoName == "" {
				return controller.ReconcileResult{}, controller.NewPermanentError(fmt.Errorf("cannot resolve repository for failed spec %q", spec))
			}
			failedRepos[depend.RepoName] = struct{}{}
		}
	}
	next.Status.FailedPackages = sortedFailedPackages(next.Status.FailedPackages, failedRepos)
	removeCondition(&next.Status.Conditions, ConditionRpmRepoRetrying)
	next.Status.Phase = ebsv1.BuildInfoCompleted
	result, err := c.writeStatus(ctx, round, next)
	if err == nil && result == (controller.ReconcileResult{}) {
		c.logOnce(round.key, "BuildInfoCompleted", "completed: %d specs, %d failed", round.current.Status.SpecStatus.Len(), len(failed))
		c.invalidateCaches(round.key)
	}
	return result, err
}

// advanceSingle runs the single-type Processing path: backfill plus the
// completion check — no Snapshot/RpmMeta reads, no graph, no gates.
func (c *Controller) advanceSingle(ctx context.Context, round *reconcileRound) (controller.ReconcileResult, error) {
	if round.current.Status.SpecStatus.Len() == 0 {
		c.logf(round.key, "SpecStatusEmpty", "processing single buildinfo with empty specStatus (E-01); waiting for manual intervention, phase kept")
		return controller.ReconcileResult{}, nil
	}
	jobs, err := c.listRoundJobs(ctx, round)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	next := round.current.DeepCopy()
	c.backfillJobs(round, next, jobs, specStatusScope(round.current), false)
	if result, err := c.writeStatusIfChanged(ctx, round, next); err != nil || result != (controller.ReconcileResult{}) {
		return result, err
	}
	return c.checkCompletion(ctx, round, nil, nil)
}

// stoppedFailedPackages retains every failed or unfinished spec for the next
// incremental build. The persisted specRepoNames map supplies the repository
// mapping after a restart; the assembly cache covers older objects.
// If neither can identify a spec, conservatively retry the build's package
// scope rather than silently dropping work from the published baseline.
func (c *Controller) stoppedFailedPackages(round *reconcileRound) []string {
	failed := make(map[string]struct{})
	depends, _ := c.specDependsCache.Get(round.key)
	unmapped := len(round.current.Status.SpecStatus.Build) == 0
	addSpec := func(spec string) {
		repo := round.current.Status.SpecRepoNames[spec]
		if repo == "" {
			repo = depends[spec].RepoName
		}
		if repo == "" {
			unmapped = true
			return
		}
		failed[repo] = struct{}{}
	}
	for spec, build := range round.current.Status.SpecStatus.Build {
		if build.Status != SpecBuildSucceeded || round.current.Status.SpecStatus.Install[spec].Status != SpecBuildSucceeded {
			addSpec(spec)
		}
	}
	for spec, install := range round.current.Status.SpecStatus.Install {
		if _, exists := round.current.Status.SpecStatus.Build[spec]; !exists && install.Status != SpecBuildSucceeded {
			addSpec(spec)
		}
	}
	for spec := range round.current.Status.SpecRepoNames {
		if _, exists := round.current.Status.SpecStatus.Build[spec]; !exists {
			addSpec(spec)
		}
	}
	if unmapped {
		if round.isSingle() || (round.current.Status.Phase == ebsv1.BuildInfoPending && len(round.build.Spec.Packages) > 0) {
			for _, name := range round.build.Spec.Packages {
				if name != "" {
					failed[name] = struct{}{}
				}
			}
		} else {
			for _, repo := range round.project.Spec.PackageRepos {
				if repo.Name != "" {
					failed[repo.Name] = struct{}{}
				}
			}
		}
		c.logf(round.key, "StopFailedPackagesFallback", "missing persisted spec-to-repository mapping; using conservative package scope")
	}
	return sortedFailedPackages(round.current.Status.FailedPackages, failed)
}

// convergeToCompleted waits for created Jobs to finish without further
// dispatch. Pending creations must be confirmed before completion; a 404
// keeps the entry because the create may still land. The stop marker remains.
func (c *Controller) convergeToCompleted(ctx context.Context, round *reconcileRound) (controller.ReconcileResult, error) {
	jobs, err := c.listRoundJobs(ctx, round)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	next := round.current.DeepCopy()
	c.backfillJobs(round, next, jobs, specStatusScope(round.current), false)
	if result, err := c.writeStatusIfChanged(ctx, round, next); err != nil || result != (controller.ReconcileResult{}) {
		return result, err
	}

	// Even when the List missed an entry, every registered pending
	// create is GET-verified; re-creation is forbidden after the stop.
	var confirmed []*ebsv1.Job
	for _, spec := range sortedSpecNames(round.current.Status.PendingJobCreates) {
		pend, ok := round.current.Status.PendingJobCreates[spec]
		if !ok {
			continue // confirmed and removed by an earlier write this round
		}
		job, err := c.client.GetJob(ctx, round.current.Namespace, pend.JobName)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				// Keep the entry and wait: 404 is never a
				// non-existence proof for an in-flight request.
				c.logf(round.key, "JobCreateUnresolved", "pending job create for spec %s (job %s generation %d) unresolved after stop-dispatch; entry kept, waiting (6.5.1 #5)", spec, pend.JobName, pend.DispatchGeneration)
				continue
			}
			return controller.ReconcileResult{}, err
		}
		if verr := verifyJobIdentity(job, round.current, spec, pend.DispatchGeneration); verr != nil {
			c.logf(round.key, "JobIdentityMismatch", "job %s identity mismatch: %v", pend.JobName, verr)
			return controller.ReconcileResult{}, controller.NewPermanentError(verr)
		}
		if result, err := c.confirmDispatchedJob(ctx, round, spec, pend.DispatchGeneration, job); err != nil || result != (controller.ReconcileResult{}) {
			return result, err
		}
		confirmed = append(confirmed, job)
	}

	// Completed requires an empty pending map and every created
	// Job (all generations, including out-of-scope ones) terminal.
	if len(round.current.Status.PendingJobCreates) > 0 {
		return controller.ReconcileResult{}, nil
	}
	if c.jobsAwaitingTerminal(round, jobs, confirmed) {
		return controller.ReconcileResult{}, nil
	}
	marker := stopCondition(round.current.Status.Conditions)
	next = round.current.DeepCopy()
	next.Status.FailedPackages = c.stoppedFailedPackages(round)
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
		c.logOnce(round.key, "StopConvergedCompleted", "completed after stop-dispatch (%s): all created jobs terminal", reason)
		c.invalidateCaches(round.key)
	}
	return result, err
}

// jobsAwaitingTerminal waits for all Jobs of this BuildInfo incarnation to
// become terminal. Pending, Running, empty, and unknown phases keep it waiting;
// Jobs of an older incarnation do not block convergence.
func (c *Controller) jobsAwaitingTerminal(round *reconcileRound, jobs []ebsv1.Job, confirmed []*ebsv1.Job) bool {
	waiting := false
	scan := func(job *ebsv1.Job) {
		switch job.Status.Phase {
		case ebsv1.JobSucceeded, ebsv1.JobFailed, ebsv1.JobAborted:
			// terminal
		case ebsv1.JobPending, ebsv1.JobRunning:
			waiting = true
		default:
			waiting = true
			c.logf(round.key, "UnknownJobPhase", "job %s phase %q unknown; completion waits (6.5 step 4)", job.Name, job.Status.Phase)
		}
	}
	own := filterJobsByIdentity(jobs, string(round.current.UID))
	for i := range own {
		scan(&own[i])
	}
	for _, job := range confirmed {
		scan(job)
	}
	return waiting
}

// specStatusScope returns the spec names eligible for Job result backfill.
func specStatusScope(buildInfo *ebsv1.BuildInfo) map[string]bool {
	scope := make(map[string]bool, buildInfo.Status.SpecStatus.Len())
	for name := range buildInfo.Status.SpecStatus.Build {
		scope[name] = true
	}
	return scope
}
