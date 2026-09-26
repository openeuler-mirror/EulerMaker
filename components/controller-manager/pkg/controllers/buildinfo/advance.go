// advance.go implements the Processing-phase advance flow (design 7.3):
// specStatus sanity (E-01), Job result sync, dispatch data preparation
// (2.2), persisted-DCG load, runtime install edge appends (3.1/7.4.7),
// downstream advance with the 7.4.6 gates and the effective-required
// relaxation (7.4.2), and the 6.4 completion check with the 9.1 terminal
// conditions. It also carries the single-type simplified path (7.2.3 #4)
// and the 6.5 stop-dispatch convergence path.
package buildinfo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"controller-manager/pkg/controller"
	"controller-manager/pkg/controllers/buildinfo/rpmver"
	ebsv1 "ebs-api/ebs/v1"
)

// advanceBuildInfo runs the 7.3 advance flow steps 1~5. single takes the
// 7.2.3 simplified path (steps 1/2/5 only).
func (c *Controller) advanceBuildInfo(ctx context.Context, round *reconcileRound) (controller.ReconcileResult, error) {
	if round.isSingle() {
		return c.advanceSingle(ctx, round)
	}

	// Step 1: specStatus sanity (E-01) and the residual-Aborted defense (6.4).
	if len(round.current.Status.SpecStatus) == 0 {
		c.logf(round.key, "SpecStatusEmpty", "processing buildinfo with empty specStatus (E-01); waiting for manual intervention, phase kept")
		return controller.ReconcileResult{}, nil
	}
	if spec, ok := residualAbortedSpec(round.current); ok {
		c.logf(round.key, "ResidualAbortedStatus", "spec %s holds a residual Aborted status; round skipped, waiting for the parent abort guard (6.4)", spec)
		return controller.ReconcileResult{}, nil
	}

	// Step 2: sync Job results (7.4.2 count floor / 7.4.4 latest pick / 7.4.5
	// phase mapping / 7.4.7 install backfill). A failed List ends the round.
	jobs, err := c.listRoundJobs(ctx, round)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	next := round.current.DeepCopy()
	bySpec := c.backfillJobs(round, next, jobs, specStatusScope(round.current), false)
	if result, err := c.writeStatusIfChanged(ctx, round, next); err != nil || result != (controller.ReconcileResult{}) {
		return result, err
	}

	// Step 2.2: dispatch data preparation — the current Snapshot (E-30), the
	// full specDepends view (15.11) and the RPM metadata refresh (15.10).
	snapshot, stop, result, err := c.currentSnapshot(ctx, round)
	if stop {
		return result, err
	}
	asm := c.assembleSpecDepends(ctx, round, snapshot)
	if asm.incomplete {
		// Transient assembly gap: wait for the next round (7.3 step 2.2).
		return controller.ReconcileResult{}, nil
	}

	// The RPM metadata refresh requires the held RpmRepo (E-16: unheld below
	// the threshold defers dispatch; the completion check still runs). A
	// below-threshold XML failure yields nil sources with the same semantics
	// (E-29 pending).
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

	// Step 3: load the persisted DCG (no first build in Processing, 5.4/15.9).
	dcg, result, err := c.advanceDcg(ctx, round)
	if err != nil || result != (controller.ReconcileResult{}) || dcg == nil {
		return result, err
	}

	if dispatchReady {
		// Step 3.1: runtime install edge appends (7.4.7); the returned graph
		// is the persisted candidate when edges changed.
		dcg, result, err = c.appendInstallEdges(ctx, round, dcg, sources)
		if err != nil || result != (controller.ReconcileResult{}) {
			return result, err
		}
		// Step 4: advance downstreams (SortedNodes traversal, 7.4.6 gates).
		if result, err = c.advanceDownstream(ctx, round, dcg, asm, snapshot, sources, bySpec); err != nil || result != (controller.ReconcileResult{}) {
			return result, err
		}
	}

	// Step 5: completion check (6.4, effective required).
	return c.checkCompletion(ctx, round, dcg, asm)
}

// advanceDcg loads the persisted graph for the advance flow: the in-memory
// cache, then BuildInfo.status.dcg (G-09: load never re-selects break
// points). Processing without a persisted graph is an anomaly (init persists
// the graph before the phase flip, G-02): record DcgBuildFailed and wait —
// no first build here.
func (c *Controller) advanceDcg(ctx context.Context, round *reconcileRound) (*DcgDict, controller.ReconcileResult, error) {
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
	c.logf(round.key, "DcgMissing", "processing buildinfo without a persisted dcg; no first build in advance (15.9), waiting")
	next := round.current.DeepCopy()
	upsertCondition(&next.Status.Conditions, ConditionDcgBuildFailed, ReasonDcgBuildFailed, "processing buildinfo without a persisted dcg")
	result, err := c.writeStatusIfChanged(ctx, round, next)
	return nil, result, err
}

// appendInstallEdges runs the 7.4.7 runtime edge-append pass: every spec
// with install.status=Failed (cycle nodes already at 2 dispatches excluded)
// reverse-looks-up a provider per missing dep against the current-round
// layered sources; an edge is appended on the candidate graph only when the
// provider is a build-set spec, non-terminal and not already an install
// upstream. New cycles get additional break points (initial picks never
// re-selected, G-09). The candidate state is persisted BEFORE the in-memory
// graph is replaced (G-02); a failed write leaves both untouched.
func (c *Controller) appendInstallEdges(ctx context.Context, round *reconcileRound, dcg *DcgDict, sources *rpmver.RpmMetaSources) (*DcgDict, controller.ReconcileResult, error) {
	prefer := payloadPrefer(c.parseBuildPayload(round.key, round.current.Spec.BuildPayload))
	var candidate *DcgDict
	changed := 0
	for _, spec := range sortedSpecNames(round.current.Status.SpecStatus) {
		ss := round.current.Status.SpecStatus[spec]
		if ss.Install.Status != SpecBuildFailed {
			continue
		}
		if dcg.IsCycleNode(spec) && ss.DispatchCount >= 2 {
			continue // 7.4.7 #1: terminal cycle node, second-layer fallback owns the residue
		}
		node := dcg.Node(spec)
		if node == nil {
			continue
		}
		for _, depName := range sortedSpecNames(ss.Install.MissingDeps) {
			vc := ss.Install.MissingDeps[depName].VersionRequests
			selection, ok := sources.FindProvider(depName, vc, prefer)
			if !ok {
				continue // provider unresolvable: no edge (7.4.7 #8)
			}
			provider := selection.Provider.SpecName
			if dcg.Node(provider) == nil {
				continue // provider not in this round's build set
			}
			if pSS := round.current.Status.SpecStatus[provider]; pSS.Build.Status == SpecBuildSucceeded || pSS.Build.Status == SpecBuildFailed {
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
		// 落盘失败: 内存图不更新，不基于候选图派发 (7.4.7 #6)。
		return dcg, result, err
	}
	c.dcgDict.Set(round.key, candidate)
	edgesAdded.Add(uint64(changed))
	if len(newBreaks) > 0 {
		bootstrapBreaks.Add(uint64(len(newBreaks)))
		c.logOnce(round.key, "RuntimeBootstrapBreaks", "install edge appends added %d edges; new cycle break points: %s", changed, strings.Join(newBreaks, ","))
	} else {
		c.logOnce(round.key, "InstallEdgesAdded", "install edge appends added %d edges", changed)
	}
	return candidate, controller.ReconcileResult{}, nil
}

// advanceDownstream runs 7.3 step 4: the SortedNodes traversal skips specs
// at their effective required, with an in-flight Job or a terminal verdict;
// the bootstrap exemption (BootstrapBreak && DispatchCount=0, 7.4.6 #3)
// bypasses the upstream checks and both gates, keeping only the E-19 check
// and the 7.4.1 condition-2 verdict. Gate dissatisfaction skips the spec for
// this round (no condition, 7.4.6).
func (c *Controller) advanceDownstream(ctx context.Context, round *reconcileRound, dcg *DcgDict, asm *specAssembly, snapshot *ebsv1.Snapshot, sources *rpmver.RpmMetaSources, bySpec map[string][]ebsv1.Job) (controller.ReconcileResult, error) {
	required := dcg.DispatchRequirements()
	dispatch := &roundDispatch{arch: round.build.Spec.BuildTarget.Arch, contentURL: heldContentURL(round)}
	for _, name := range dcg.SortedNodes() {
		ss := round.current.Status.SpecStatus[name]
		node := dcg.Node(name)
		if ss.DispatchCount >= effectiveRequired(dcg, round, name, required) {
			continue
		}
		if ss.Build.Status == SpecBuildRunning || ss.Build.Status == SpecBuildFailed {
			continue // in-flight Job or terminal verdict (G-03: no re-dispatch on Failed)
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
			// 7.4.1 condition 1: every direct upstream (inDep ∪ installInDep)
			// must be terminal; a non-terminal upstream waits (no condition).
			if !upstreamsTerminal(dcg, round, name) {
				continue
			}
			if !hasFailedUpstream(dcg, round, name) {
				// 7.4.6 gate 1 (rebuild consistency): exempt for nodes with a
				// Failed upstream (their rebuild is cancelled, 7.4.2).
				if !rebuildConsistencySatisfied(dcg, round, name, ss, required) {
					continue
				}
			}
			// 7.4.6 gate 2 (publish confirmation): every Succeeded direct
			// upstream's latest-generation Job UID must be in the RpmRepo
			// sourceJobUIDs set; Failed upstreams are skipped.
			if !upstreamOutputsPublished(round, dcg, name, bySpec) {
				continue
			}
		}
		// E-19 exclusiveArch check (deterministic verdict, no Job on a miss).
		if result, err := c.checkArchSupported(ctx, round, name, &depend, dispatch.arch); err != nil || result != (controller.ReconcileResult{}) {
			return result, err
		}
		if ss = round.current.Status.SpecStatus[name]; ss.Build.Status == SpecBuildFailed {
			continue
		}
		// 7.4.1 condition 2: build-requires availability verdict (the only
		// gate kept for bootstrap dispatches, 7.4.6 #3).
		if result, err := c.checkBuildRequires(ctx, round, name, &depend, sources); err != nil || result != (controller.ReconcileResult{}) {
			return result, err
		}
		if ss = round.current.Status.SpecStatus[name]; ss.Build.Status == SpecBuildFailed {
			continue
		}
		// E-26: the build-target Config snapshot resolves lazily, shared round-wide.
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

// upstreamsTerminal reports 7.4.1 condition 1: every direct upstream is in a
// terminal state (Succeeded/Failed).
func upstreamsTerminal(dcg *DcgDict, round *reconcileRound, spec string) bool {
	for _, up := range upstreamNames(dcg.Node(spec)) {
		st := round.current.Status.SpecStatus[up].Build.Status
		if st != SpecBuildSucceeded && st != SpecBuildFailed {
			return false
		}
	}
	return true
}

// hasFailedUpstream reports whether any direct upstream is Failed (7.4.2:
// the node's rebuild is cancelled; 7.4.6 gate 1 is exempt).
func hasFailedUpstream(dcg *DcgDict, round *reconcileRound, spec string) bool {
	for _, up := range upstreamNames(dcg.Node(spec)) {
		if round.current.Status.SpecStatus[up].Build.Status == SpecBuildFailed {
			return true
		}
	}
	return false
}

// rebuildConsistencySatisfied evaluates the 7.4.6 gate-1 variants:
// ① a non-break-point node's 2nd+ dispatch waits for every upstream to be
// Succeeded at its own effective required; ② an off-cycle node's first
// dispatch with cycle upstreams waits for those cycle upstreams to be
// Succeeded at their effective required (off-cycle upstreams follow the
// normal terminal + publish gates). The break point's own rebuild is exempt.
func rebuildConsistencySatisfied(dcg *DcgDict, round *reconcileRound, spec string, ss ebsv1.SpecStatus, required map[string]int64) bool {
	node := dcg.Node(spec)
	if ss.DispatchCount >= 1 && !node.BootstrapBreak {
		for _, up := range upstreamNames(node) {
			upSS := round.current.Status.SpecStatus[up]
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
			upSS := round.current.Status.SpecStatus[up]
			if upSS.Build.Status != SpecBuildSucceeded || upSS.DispatchCount < effectiveRequired(dcg, round, up, required) {
				return false
			}
		}
	}
	return true
}

// upstreamOutputsPublished evaluates the 7.4.6 gate-2 publish confirmation:
// for every Succeeded direct upstream, the latest-generation Job's UID must
// be a member of the same-name RpmRepo status.repository.sourceJobUIDs (the
// only publish credential). Failed upstreams are skipped (their Job is never
// consumed). The RpmRepo object is the guard-held one (15.4), the Jobs come
// from this round's List — no extra queries.
func upstreamOutputsPublished(round *reconcileRound, dcg *DcgDict, spec string, bySpec map[string][]ebsv1.Job) bool {
	published := map[string]bool{}
	if round.rpmRepoHeld && round.rpmRepo.Status.Repository != nil {
		for _, uid := range round.rpmRepo.Status.Repository.SourceJobUIDs {
			published[uid] = true
		}
	}
	for _, up := range upstreamNames(dcg.Node(spec)) {
		if round.current.Status.SpecStatus[up].Build.Status != SpecBuildSucceeded {
			continue
		}
		group := bySpec[up]
		if len(group) == 0 {
			return false
		}
		if !published[string(latestJob(group).UID)] {
			return false
		}
	}
	return true
}

// effectiveRequired applies the 7.4.2 relaxation: any Failed direct upstream
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
			if round.current.Status.SpecStatus[up].Build.Status == SpecBuildFailed {
				return 1
			}
		}
	}
	return req
}

// checkCompletion runs the 6.4 allTerminal verdict: every spec terminal and
// every Succeeded spec at its effective required, plus an empty
// pendingJobCreates map (6.5.1: every Completed path requires it). A
// completed BuildInfo flips to Completed with PartialFailure (any Failed
// spec) or AllSpecsSucceeded (9.1).
func (c *Controller) checkCompletion(ctx context.Context, round *reconcileRound, dcg *DcgDict, asm *specAssembly) (controller.ReconcileResult, error) {
	var required map[string]int64
	if dcg != nil {
		required = dcg.DispatchRequirements()
	}
	var failed []string
	for _, spec := range sortedSpecNames(round.current.Status.SpecStatus) {
		ss := round.current.Status.SpecStatus[spec]
		if ss.Build.Status != SpecBuildSucceeded && ss.Build.Status != SpecBuildFailed {
			return controller.ReconcileResult{}, nil // not all terminal
		}
		if ss.Build.Status == SpecBuildSucceeded && ss.DispatchCount < effectiveRequired(dcg, round, spec, required) {
			return controller.ReconcileResult{}, nil // terminal but short of the effective required
		}
		if ss.Build.Status == SpecBuildFailed {
			failed = append(failed, spec)
		}
	}
	if blocked := c.pendingCreatesBlock(round, "completion"); blocked {
		return controller.ReconcileResult{}, nil
	}
	needsRepoMap := len(failed) > 0
	for _, ss := range round.current.Status.SpecStatus {
		needsRepoMap = needsRepoMap || ss.Install.Status == SpecBuildFailed
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
		for _, spec := range sortedSpecNames(round.current.Status.SpecStatus) {
			ss := round.current.Status.SpecStatus[spec]
			if ss.Build.Status != SpecBuildFailed && ss.Install.Status != SpecBuildFailed {
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
	if len(failed) > 0 {
		upsertCondition(&next.Status.Conditions, ConditionPartialFailure, ReasonPartialFailure, "failed specs: "+strings.Join(failed, ","))
	} else {
		upsertCondition(&next.Status.Conditions, ConditionAllSpecsSucceeded, ReasonAllSpecsSucceeded, "all specs succeeded")
	}
	next.Status.Phase = ebsv1.BuildInfoCompleted
	result, err := c.writeStatus(ctx, round, next)
	if err == nil && result == (controller.ReconcileResult{}) {
		c.logOnce(round.key, "BuildInfoCompleted", "completed: %d specs, %d failed", len(round.current.Status.SpecStatus), len(failed))
		c.invalidateCaches(round.key)
	}
	return result, err
}

// --- single 简化路径 (design 7.2.3 #4: steps 1/2/5 only) ---

// advanceSingle runs the single-type Processing path: backfill plus the
// completion check — no Snapshot/RpmMeta reads, no graph, no gates.
func (c *Controller) advanceSingle(ctx context.Context, round *reconcileRound) (controller.ReconcileResult, error) {
	if len(round.current.Status.SpecStatus) == 0 {
		c.logf(round.key, "SpecStatusEmpty", "processing single buildinfo with empty specStatus (E-01); waiting for manual intervention, phase kept")
		return controller.ReconcileResult{}, nil
	}
	if spec, ok := residualAbortedSpec(round.current); ok {
		c.logf(round.key, "ResidualAbortedStatus", "spec %s holds a residual Aborted status; round skipped, waiting for the parent abort guard (6.4)", spec)
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

// --- 6.5 停止派发收敛路径 ---

// convergeToCompleted runs the 6.5 convergence: no new Job creation, no
// Snapshot/RpmRepo/build-target Config reads. The full Job List is backfilled (a
// failed page retried, never partially used), registered pending creates are
// GET-confirmed one by one (stopped 404 keeps the entry with a rate-limited
// JobCreateUnresolved warning, 6.5.1 #5), and Completed is written only when
// every created Job is terminal and the pending map is empty. The stop
// marker is preserved; AllSpecsSucceeded/PartialFailure are never written.
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

	// 6.5.1 #4: even when the List missed an entry, every registered pending
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
				// 6.5.1 #5: keep the entry, warn, wait — 404 is never a
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

	// 6.5 step 4: Completed requires an empty pending map and every created
	// Job (all generations, including out-of-scope ones) terminal.
	if len(round.current.Status.PendingJobCreates) > 0 {
		return controller.ReconcileResult{}, nil
	}
	if c.jobsAwaitingTerminal(round, jobs, confirmed) {
		return controller.ReconcileResult{}, nil
	}
	marker := stopCondition(round.current.Status.Conditions)
	next = round.current.DeepCopy()
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

// jobsAwaitingTerminal reports whether any created Job is still in flight
// (6.5 step 4): Pending/Running, an empty phase or an unknown phase value
// all keep the round waiting; unknown values are warned. Only this
// incarnation's Jobs count (15.3.1 identity): a lingering Job of a previous
// same-name BuildInfo must not block the convergence.
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

// --- shared helpers ---

// specStatusScope flattens the specStatus key set (Processing backfill
// scope, E-05).
func specStatusScope(buildInfo *ebsv1.BuildInfo) map[string]bool {
	scope := make(map[string]bool, len(buildInfo.Status.SpecStatus))
	for name := range buildInfo.Status.SpecStatus {
		scope[name] = true
	}
	return scope
}

// residualAbortedSpec finds the first spec holding a residual Aborted status
// value (6.4: legacy dirty data — the round returns nil and waits for the
// parentAbortGuard, the value never joins the allTerminal set).
func residualAbortedSpec(buildInfo *ebsv1.BuildInfo) (string, bool) {
	for _, spec := range sortedSpecNames(buildInfo.Status.SpecStatus) {
		if buildInfo.Status.SpecStatus[spec].Build.Status == SpecBuildAborted {
			return spec, true
		}
	}
	return "", false
}
