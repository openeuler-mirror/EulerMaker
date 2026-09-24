// specdepends.go implements step 0 of initBuildInfo (design 7.2.2): the
// specDepends full assembly (per-BuildInfo cache lookup / Pending re-entry /
// packageRepoStatuses enumeration / specFileCache + git-server backfill /
// E-23/E-24 role routing / unified cache write-back), the build-set
// determination per build type, the base-round positioning (E-22), the
// previous-round failed-package merge, and the downstream expansion to a
// fixed point (RpmRepo layer only).
package buildinfo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"controller-manager/pkg/clients/gitserver"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/controllers/buildinfo/rpmver"
	"controller-manager/pkg/controllers/buildinfo/specparse"
	ebsv1 "ebs-api/ebs/v1"
	yaml "gopkg.in/yaml.v2"
)

// repoRole classifies an enumerated package repository for the E-23/E-24
// deterministic-failure routing (design 7.2.2/7.2.3).
type repoRole int

const (
	// rolePlain: full/incremental repos and specified's non-specified repos —
	// deterministic failures degrade (skip + condition), init continues.
	rolePlain repoRole = iota
	// roleSpecifiedPackage: specified's Build.spec.packages repos —
	// deterministic failures are init-terminal (condition + Completed).
	roleSpecifiedPackage
	// roleSinglePackage: single's Build.spec.packages repos — deterministic
	// failures degrade per package (7.2.3), never init-terminal.
	roleSinglePackage
)

// degradedCondition is one business-degradation condition collected during
// assembly; aggregated by (type, reason) before persisting (design 9.1:
// messages list the affected specs/repos, sanitized and truncated).
type degradedCondition struct {
	condType string
	reason   string
	items    []string
}

// terminalVerdict is an init deterministic-failure收口 (design 7.2.2/7.2.3):
// persist condition SpecDependsFillFailed with the reason and set Completed.
type terminalVerdict struct {
	reason  string
	message string
}

// specAssembly is the outcome of one step-0 assembly round.
type specAssembly struct {
	// depends is the full view keyed by specName (nil when incomplete).
	depends map[string]specparse.SpecDepend
	// snapshot is the current Snapshot held for the round (15.7).
	snapshot *ebsv1.Snapshot
	// degraded collects the business-degradation conditions (skip paths).
	degraded []degradedCondition
	// terminal, when non-nil, is the init deterministic-failure收口: the
	// first terminal failure wins (E-23/E-24 specified-package routing).
	terminal *terminalVerdict
	// incomplete marks transient gaps: stay Pending and re-assemble next
	// round (no cache write-back, no build-set determination).
	incomplete bool
}

// currentSnapshot fetches the same-name Snapshot with E-30 counting (design
// 5.4/7.5): a successful GET clears the counter; 404/query failures count,
// escalate to the SnapshotUnavailable stop marker at the threshold (6.5), and
// below the threshold fail the round (含 404 异常瞬态, E-22 同语义).
func (c *Controller) currentSnapshot(ctx context.Context, round *reconcileRound) (*ebsv1.Snapshot, bool, controller.ReconcileResult, error) {
	snapshot, err := c.client.GetSnapshot(ctx, round.current.Namespace, round.current.Name)
	if err == nil {
		round.failures.SnapshotReady()
		return snapshot, false, controller.ReconcileResult{}, nil
	}
	reason := ReasonSnapshotQueryFailed
	if errors.Is(err, ErrNotFound) {
		reason = ReasonSnapshotNotFound
	}
	_, escalated := round.failures.SnapshotFailed(reason, err.Error())
	if escalated {
		result, escErr := c.escalateSnapshotUnavailable(ctx, round)
		return nil, true, result, escErr
	}
	return nil, true, controller.ReconcileResult{}, err
}

// assembleSpecDepends runs the full assembly (design 7.2.2/15.11.3). Pending
// rounds always re-assemble and overwrite the cache; Processing rounds reuse
// a cache hit without any download. The caller has already obtained the
// current Snapshot for the round.
func (c *Controller) assembleSpecDepends(ctx context.Context, round *reconcileRound, snapshot *ebsv1.Snapshot) *specAssembly {
	asm := &specAssembly{snapshot: snapshot}
	// Step 1: cache lookup (15.11.3). Processing hits reuse the full view.
	if round.current.Status.Phase == ebsv1.BuildInfoProcessing {
		if cached, ok := c.specDependsCache.Get(round.key); ok {
			specDependsCacheHits.Inc()
			asm.depends = cached
			return asm
		}
	}

	repos, roles := c.enumerateRepos(round, snapshot)
	arch := round.build.Spec.BuildTarget.Arch
	macros := payloadMacros(round.current.Spec.BuildPayload)
	merged := map[string]specparse.SpecDepend{}
	var parseFailures, commitMissing []string

	for _, repo := range repos {
		entry, ok := snapshot.Status.PackageRepoStatuses[repo]
		if !ok {
			// Only single/specified enumerated packages can miss an entry:
			// not in spec.packageRepos is deterministic (E-24 routing); in
			// packageRepos without an entry is the defensive transient
			// (条目就绪不变式, 7.2.2).
			if !packageRepoDeclared(snapshot, repo) {
				switch roles[repo] {
				case roleSpecifiedPackage:
					asm.setTerminal(ReasonSpecifiedSpecCommitMissing, fmt.Sprintf("specified package repo %s not in packageRepos", repo))
				default:
					commitMissing = append(commitMissing, repo+" (not in packageRepos)")
				}
				continue
			}
			asm.incomplete = true
			continue
		}
		if entry.Error != nil {
			if entry.Error.Retryable {
				// Still resolving: defensive transient (E-24 ①).
				asm.incomplete = true
				continue
			}
			// E-24: non-retryable resolution failure.
			if roles[repo] == roleSpecifiedPackage {
				asm.setTerminal(ReasonSpecifiedSpecCommitMissing, fmt.Sprintf("specified package repo %s unavailable: %s", repo, entry.Error.Message))
				continue
			}
			commitMissing = append(commitMissing, fmt.Sprintf("%s (%s)", repo, entry.Error.Message))
			continue
		}
		if entry.CommitID == "" {
			// No error and no commitId: still resolving (defensive transient).
			asm.incomplete = true
			continue
		}
		specs, repoOK := c.fetchRepoSpecs(ctx, round, repo, entry, arch, macros, roles[repo], asm, &parseFailures)
		if !repoOK {
			asm.incomplete = true
			continue
		}
		for name, depend := range specs {
			if _, clash := merged[name]; clash {
				c.logf(round.key, "SpecNameClash", "spec %s produced by both %s and %s; keeping %s", name, merged[name].RepoName, repo, repo)
			}
			merged[name] = depend
		}
	}

	if len(parseFailures) > 0 {
		asm.degraded = append(asm.degraded, degradedCondition{condType: ConditionSpecDependsFillFailed, reason: ReasonSpecParseFailed, items: parseFailures})
	}
	if len(commitMissing) > 0 {
		asm.degraded = append(asm.degraded, degradedCondition{condType: ConditionSpecCommitMissing, reason: ReasonSpecCommitMissing, items: commitMissing})
	}
	if asm.incomplete {
		// Transient gap: stay Pending; nothing is written back (15.11.3).
		return asm
	}
	// Step 4: unified write-back of the completed full view (15.11.3).
	c.specDependsCache.Set(round.key, merged)
	specDependsFills.Inc()
	asm.depends = merged
	return asm
}

// setTerminal records the init deterministic-failure收口; the first terminal
// failure wins (E-23/E-24 specified-package先行收口).
func (a *specAssembly) setTerminal(reason, message string) {
	if a.terminal != nil {
		return
	}
	a.terminal = &terminalVerdict{reason: reason, message: message}
}

// enumerateRepos returns the repositories to assemble and their E-23/E-24
// roles: single narrows the enumeration to Build.spec.packages (7.2.3);
// specified assembles all repos but tags its packages with the terminal role;
// full/incremental assemble all repos as plain.
func (c *Controller) enumerateRepos(round *reconcileRound, snapshot *ebsv1.Snapshot) ([]string, map[string]repoRole) {
	buildType := round.build.Spec.BuildType
	specified := map[string]bool{}
	for _, pkg := range round.build.Spec.Packages {
		specified[pkg] = true
	}
	roles := map[string]repoRole{}
	var repos []string
	if buildType == "single" {
		// ebs-apiserver allows duplicate package names in Build.spec.packages;
		// dedup here so one repo is assembled once per round (7.2.3).
		seen := make(map[string]bool, len(round.build.Spec.Packages))
		for _, pkg := range round.build.Spec.Packages {
			if seen[pkg] {
				continue
			}
			seen[pkg] = true
			repos = append(repos, pkg)
		}
		sort.Strings(repos)
		for _, repo := range repos {
			roles[repo] = roleSinglePackage
		}
		return repos, roles
	}
	for repo := range snapshot.Status.PackageRepoStatuses {
		repos = append(repos, repo)
	}
	if buildType == "specified" {
		// Specified packages missing from packageRepoStatuses are enumerated
		// too, so the E-24 specified-package routing (not in packageRepos →
		// terminal; in packageRepos without an entry → defensive transient)
		// fires inside the assembly loop.
		for pkg := range specified {
			if _, ok := snapshot.Status.PackageRepoStatuses[pkg]; !ok {
				repos = append(repos, pkg)
			}
		}
	}
	sort.Strings(repos)
	for _, repo := range repos {
		if buildType == "specified" && specified[repo] {
			roles[repo] = roleSpecifiedPackage
		} else {
			roles[repo] = rolePlain
		}
	}
	return repos, roles
}

// packageRepoDeclared reports whether repo is declared in
// snapshot.spec.packageRepos (design 15.7: existence verdict input).
func packageRepoDeclared(snapshot *ebsv1.Snapshot, repo string) bool {
	for _, declared := range snapshot.Spec.PackageRepos {
		if declared.Name == repo {
			return true
		}
	}
	return false
}

// fetchRepoSpecs downloads and parses every root-level *.spec of one
// repository (design 7.2.2 spec 下载解析). A transient failure anywhere in the
// repo drops the whole repo from this round (ok=false); deterministic
// failures skip per spec (or the whole repo on an invalid commitId) and are
// routed by role via asm.degraded/asm.terminal. Parses run outside any lock.
func (c *Controller) fetchRepoSpecs(ctx context.Context, round *reconcileRound, repo string, entry ebsv1.PackageRepoStatus, arch string, macros []string, role repoRole, asm *specAssembly, parseFailures *[]string) (map[string]specparse.SpecDepend, bool) {
	listing, err := c.gitServer.ExecCommand(ctx, entry.CloneURL, "git-ls-tree --name-only "+entry.CommitID)
	if err != nil {
		return nil, c.handleGitFailure(round, repo, "", role, asm, parseFailures, err)
	}
	var files []string
	for _, line := range strings.Split(listing, "\n") {
		line = strings.TrimSpace(line)
		// Root-level direct entries only, no subdirectories (7.2.2).
		if line == "" || strings.Contains(line, "/") || !strings.HasSuffix(line, ".spec") {
			continue
		}
		files = append(files, line)
	}
	sort.Strings(files)

	specs := map[string]specparse.SpecDepend{}
	for _, file := range files {
		content, hit := c.specFiles.Get(entry.CommitID, file)
		if hit {
			specFileCacheHits.Inc()
		} else {
			content, err = c.gitServer.ExecCommand(ctx, entry.CloneURL, "git-show "+entry.CommitID+":"+file)
			if err != nil {
				if !c.handleGitFailure(round, repo, file, role, asm, parseFailures, err) {
					return nil, false
				}
				continue
			}
			// Write-through before parsing: the raw content stays valid for
			// the same commit even when parsing fails (15.11).
			c.specFiles.Add(entry.CommitID, file, content)
		}
		depend, parseErr := specparse.Parse(content, file, repo, arch, macros, c.specEngine)
		if parseErr != nil {
			item := fmt.Sprintf("%s/%s (%v)", repo, file, parseErr)
			if role == roleSpecifiedPackage {
				asm.setTerminal(ReasonSpecParseFailed, "specified package spec parse failed: "+item)
			} else {
				*parseFailures = append(*parseFailures, item)
			}
			continue
		}
		if _, clash := specs[depend.SpecName]; clash {
			// Same as the cross-repo clash below: files iterate in dictionary
			// order, so the lexicographically later file wins — keep a trace.
			c.logf(round.key, "SpecNameClash", "spec %s produced by multiple files in repo %s; keeping %s", depend.SpecName, repo, file)
		}
		specs[depend.SpecName] = *depend
	}
	return specs, true
}

// handleGitFailure routes a git-server failure (design E-23): transient
// failures drop the repo from this round (false = incomplete); deterministic
// failures skip per role — an empty file means the failure is repo-level
// (invalid commitId), otherwise spec-level. Returns whether the repo may
// still contribute its already-parsed specs this round.
func (c *Controller) handleGitFailure(round *reconcileRound, repo, file string, role repoRole, asm *specAssembly, parseFailures *[]string, err error) bool {
	gitServerFailures.Inc()
	var gitErr *gitserver.Error
	if !errors.As(err, &gitErr) || gitErr.Kind == gitserver.ErrorTemporary {
		c.logf(round.key, "GitServerTransient", "repo %s git fetch failed transiently: %v", repo, err)
		return false
	}
	where := repo
	if file != "" {
		where = repo + "/" + file
	}
	item := fmt.Sprintf("%s (%v)", where, gitErr.Err)
	if role == roleSpecifiedPackage {
		asm.setTerminal(ReasonSpecParseFailed, "specified package spec fetch failed: "+item)
	} else {
		*parseFailures = append(*parseFailures, item)
	}
	// Deterministic failures never make the round incomplete.
	return true
}

// parseBuildPayload decodes BuildInfo.spec.buildPayload (design 16.1/15.3.1):
// YAML to map; a parse failure or a non-map root yields {} plus a warning
// (prefer silently disabled would skew edge building — keep a trace).
func (c *Controller) parseBuildPayload(key, raw string) map[string]any {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}
	}
	var decoded map[string]any
	if err := yaml.Unmarshal([]byte(raw), &decoded); err != nil || decoded == nil {
		c.logf(key, "BuildPayloadParseFailed", "buildPayload decode failed, using empty base: %v", err)
		return map[string]any{}
	}
	return decoded
}

// payloadMacros extracts the buildPayload macros list for specparse --load
// (design 16.3); a non-list macros key yields nil.
func payloadMacros(raw string) []string {
	var decoded map[string]any
	if err := yaml.Unmarshal([]byte(raw), &decoded); err != nil || decoded == nil {
		return nil
	}
	return stringList(decoded["macros"])
}

// payloadPrefer extracts the buildPayload prefer list (design 16.1); a
// non-list prefer key yields nil.
func payloadPrefer(base map[string]any) []string { return stringList(base["prefer"]) }

func stringList(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// --- build-set determination (design 7.2.2 阶段二) ---

// buildSetOutcome is the determined build set plus a possible terminal
// verdict (specified empty set收口).
type buildSetOutcome struct {
	set      map[string]specparse.SpecDepend
	terminal *terminalVerdict
}

// determineBuildSet filters this round's full assembly into the build set
// per build type (design 7.2.2). single never reaches here (its assembly is
// the build set, 7.2.3).
func (c *Controller) determineBuildSet(ctx context.Context, round *reconcileRound, asm *specAssembly, repoLayer *rpmver.RpmMetaSource) (*buildSetOutcome, error) {
	switch round.build.Spec.BuildType {
	case "full":
		// No base-round query, no expansion (7.2.2).
		return &buildSetOutcome{set: asm.depends}, nil
	case "incremental":
		seeds, err := c.incrementalSeeds(ctx, round, asm)
		if err != nil {
			return nil, err
		}
		return &buildSetOutcome{set: expandBuildSet(seeds, asm.depends, repoLayer)}, nil
	case "specified":
		seeds := map[string]specparse.SpecDepend{}
		specified := map[string]bool{}
		for _, pkg := range round.build.Spec.Packages {
			specified[pkg] = true
		}
		for name, depend := range asm.depends {
			if specified[depend.RepoName] {
				seeds[name] = depend
			}
		}
		if len(seeds) == 0 {
			// specified 空集收口 (7.2.2): packages empty, or every specified
			// repo ready but without any root-level *.spec.
			return &buildSetOutcome{terminal: &terminalVerdict{
				reason:  ReasonSpecifiedBuildSetEmpty,
				message: fmt.Sprintf("specified packages %v produced no buildable spec", round.build.Spec.Packages),
			}}, nil
		}
		return &buildSetOutcome{set: expandBuildSet(seeds, asm.depends, repoLayer)}, nil
	default:
		return nil, fmt.Errorf("unknown buildType %q", round.build.Spec.BuildType)
	}
}

// incrementalSeeds computes the incremental build-set seeds (design 7.2.2):
// repos added or with a changed commitId versus the base Snapshot, plus the
// base BuildInfo's failed specs merged from this round's assembly. A missing
// base round yields empty seeds (E-22: first round, init proceeds); base
// query failures are plain errors (error backoff, never E-30 counted).
func (c *Controller) incrementalSeeds(ctx context.Context, round *reconcileRound, asm *specAssembly) (map[string]specparse.SpecDepend, error) {
	base, stop, err := c.locateBaseRound(ctx, round)
	if err != nil || stop {
		return nil, err
	}
	seeds := map[string]specparse.SpecDepend{}
	if base == nil {
		// E-22 no-hit: skip seed comparison and failed-package merge.
		return seeds, nil
	}
	current := asm.snapshot.Status.PackageRepoStatuses
	for repo, entry := range current {
		baseEntry, ok := base.snapshot.Status.PackageRepoStatuses[repo]
		if !ok || baseEntry.CommitID != entry.CommitID {
			// Added repo or changed commitId: every spec of the repo is a
			// seed (entries come from this round's assembly).
			for name, depend := range asm.depends {
				if depend.RepoName == repo {
					seeds[name] = depend
				}
			}
		}
	}
	// Previous-round failed packages (incremental诱因 3/4): build Failed or
	// install Failed specs join the seeds; entries come from this round's
	// assembly, missing ones are logged and skipped (repo deleted).
	for spec, ss := range base.buildInfo.Status.SpecStatus {
		if ss.Build.Status != SpecBuildFailed && ss.Install.Status != SpecBuildFailed {
			continue
		}
		depend, ok := asm.depends[spec]
		if !ok {
			c.logf(round.key, "BaseSpecMissing", "previous-round failed spec %s not in this round's assembly, skipped", spec)
			continue
		}
		seeds[spec] = depend
	}
	return seeds, nil
}

// baseRound holds the located base-round objects (design 7.2.2 基准轮次定位).
type baseRound struct {
	buildInfo *ebsv1.BuildInfo
	snapshot  *ebsv1.Snapshot
}

// locateBaseRound lists the latest terminal non-single Build with the same
// os/arch (creationTimestamp desc, limit=1), then GETs the same-name
// BuildInfo and Snapshot (E-22). stop=false/base=nil means no base round.
func (c *Controller) locateBaseRound(ctx context.Context, round *reconcileRound) (*baseRound, bool, error) {
	os := round.build.Spec.BuildTarget.Os
	arch := round.build.Spec.BuildTarget.Arch
	labelSelector := fmt.Sprintf("%s=%s,%s=%s,%s!=single",
		ebsv1.BuildTargetOSLabel, os, ebsv1.BuildTargetArchLabel, arch, ebsv1.BuildTypeLabel)
	fieldSelector := "status.phase!=" + strings.Join([]string{
		string(ebsv1.BuildPending), string(ebsv1.BuildPrepared), string(ebsv1.BuildProcessing),
		string(ebsv1.BuildAborted), string(ebsv1.BuildSkipped),
	}, ",status.phase!=")
	builds, err := c.client.ListBuilds(ctx, round.current.Namespace, labelSelector, fieldSelector, 1)
	if err != nil {
		// E-22: base list failure — error backoff, no condition, no E-30.
		return nil, false, err
	}
	if len(builds) == 0 {
		return nil, false, nil
	}
	baseName := builds[0].Name
	baseInfo, err := c.client.GetBuildInfo(ctx, round.current.Namespace, baseName)
	if err != nil {
		return nil, false, err
	}
	baseSnapshot, err := c.client.GetSnapshot(ctx, round.current.Namespace, baseName)
	if err != nil {
		// E-22: a missing base Snapshot is an error too (not E-30 counted).
		return nil, false, err
	}
	return &baseRound{buildInfo: baseInfo, snapshot: baseSnapshot}, false, nil
}

// expandBuildSet iterates the downstream expansion to a fixed point (design
// 7.2.2 下游扩散算法): buildRequires reverse lookup over this round's full
// assembly plus install-requires reverse lookup over the RpmRepo layer (本工
// 程产出 only). Key-set intersection, no version filtering (coarse recall).
func expandBuildSet(seeds, full map[string]specparse.SpecDepend, repoLayer *rpmver.RpmMetaSource) map[string]specparse.SpecDepend {
	buildSet := make(map[string]specparse.SpecDepend, len(seeds))
	for name, depend := range seeds {
		buildSet[name] = depend
	}
	provides := map[string]struct{}{}
	collectProvides := func(specNames map[string]specparse.SpecDepend) {
		if repoLayer == nil {
			return
		}
		for _, rpm := range repoLayer.RpmByName {
			if _, ok := specNames[rpm.SpecName]; !ok {
				continue
			}
			for provide := range rpm.Provides {
				provides[provide] = struct{}{}
			}
		}
	}
	collectProvides(buildSet)
	for {
		newSpecs := map[string]specparse.SpecDepend{}
		for name, depend := range full {
			if _, ok := buildSet[name]; ok {
				continue
			}
			if keysIntersect(depend.BuildRequires, provides) {
				newSpecs[name] = depend
			}
		}
		if repoLayer != nil {
			for _, rpm := range repoLayer.RpmByName {
				depend, produced := full[rpm.SpecName]
				if !produced {
					continue // not本工程产出 (15.10)
				}
				if _, ok := buildSet[depend.SpecName]; ok {
					continue
				}
				if _, ok := newSpecs[depend.SpecName]; ok {
					continue
				}
				if keysIntersect(rpm.Requires, provides) {
					newSpecs[depend.SpecName] = depend
				}
			}
		}
		if len(newSpecs) == 0 {
			return buildSet
		}
		for name, depend := range newSpecs {
			buildSet[name] = depend
		}
		collectProvides(newSpecs)
	}
}

// keysIntersect reports whether any constraint-map key is in the provide set.
func keysIntersect(requires map[string]ebsv1.VersionConst, provides map[string]struct{}) bool {
	for name := range requires {
		if _, ok := provides[name]; ok {
			return true
		}
	}
	return false
}
