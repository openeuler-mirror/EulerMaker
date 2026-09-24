// specdepends.go implements step 0 of initBuildInfo (design 7.2.2): the
// specDepends full assembly (per-BuildInfo cache lookup / Pending re-entry /
// packageRepoStatuses enumeration / specFileCache + git-server backfill /
// E-23/E-24 failure handling / unified cache write-back), the build-set
// determination per build type from the parent Build's package seeds, and
// downstream expansion to a fixed point (RpmRepo layer only).
package buildinfo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	yaml "gopkg.in/yaml.v2"

	"controller-manager/pkg/clients/gitserver"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/controllers/buildinfo/rpmver"
	"controller-manager/pkg/controllers/buildinfo/specparse"
	ebsv1 "ebs-api/ebs/v1"
)

// degradedCondition is one business-degradation condition collected during
// assembly; aggregated by (type, reason) before persisting (design 9.1:
// messages list the affected specs/repos, sanitized and truncated).
type degradedCondition struct {
	condType string
	reason   string
	items    []string
}

// terminalVerdict is a single-build init deterministic-failure closeout:
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
	// incomplete marks transient gaps: stay Pending and re-assemble next
	// round (no cache write-back, no build-set determination).
	incomplete bool
	// failedRepos records deterministic package-level failures independently
	// of conditions, whose messages are not a reliable source of repo names.
	failedRepos map[string]struct{}
}

func (a *specAssembly) markFailedRepo(name string) {
	if name == "" {
		return
	}
	if a.failedRepos == nil {
		a.failedRepos = make(map[string]struct{})
	}
	a.failedRepos[name] = struct{}{}
}

func sortedFailedPackages(existing []string, additions map[string]struct{}) []string {
	all := make(map[string]struct{}, len(existing)+len(additions))
	for _, name := range existing {
		if name != "" {
			all[name] = struct{}{}
		}
	}
	for name := range additions {
		all[name] = struct{}{}
	}
	if len(all) == 0 {
		return nil
	}
	result := make([]string, 0, len(all))
	for name := range all {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
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

	repos := c.enumerateRepos(round, snapshot)
	arch := round.build.Spec.BuildTarget.Arch
	macros := payloadMacros(round.current.Spec.BuildPayload)
	merged := map[string]specparse.SpecDepend{}
	var parseFailures, commitMissing []string

	for _, repo := range repos {
		entry, ok := snapshot.Status.PackageRepoStatuses[repo]
		if !ok {
			// Explicit seeds may lack a status entry:
			// not in spec.packageRepos is deterministic (E-24 routing); in
			// packageRepos without an entry is the defensive transient
			// (条目就绪不变式, 7.2.2).
			if !packageRepoDeclared(snapshot, repo) {
				asm.markFailedRepo(repo)
				commitMissing = append(commitMissing, repo+" (not in packageRepos)")
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
			asm.markFailedRepo(repo)
			commitMissing = append(commitMissing, fmt.Sprintf("%s (%s)", repo, entry.Error.Message))
			continue
		}
		if entry.CommitID == "" {
			// No error and no commitId: still resolving (defensive transient).
			asm.incomplete = true
			continue
		}
		specs, repoOK := c.fetchRepoSpecs(ctx, round, repo, entry, arch, macros, asm, &parseFailures)
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

// enumerateRepos assembles the current Snapshot. Incremental and specified
// also include seeds absent from the status map so both report missing input
// repositories identically; single only assembles its selected packages.
func (c *Controller) enumerateRepos(round *reconcileRound, snapshot *ebsv1.Snapshot) []string {
	buildType := round.build.Spec.BuildType
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
		return repos
	}
	for repo := range snapshot.Status.PackageRepoStatuses {
		repos = append(repos, repo)
	}
	if buildType == "incremental" || buildType == "specified" {
		missing := make(map[string]struct{})
		for _, pkg := range round.build.Spec.Packages {
			if _, ok := snapshot.Status.PackageRepoStatuses[pkg]; !ok {
				missing[pkg] = struct{}{}
			}
		}
		for pkg := range missing {
			repos = append(repos, pkg)
		}
	}
	sort.Strings(repos)
	return repos
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
// recorded as degraded conditions. Parses run outside any lock.
func (c *Controller) fetchRepoSpecs(ctx context.Context, round *reconcileRound, repo string, entry ebsv1.PackageRepoStatus, arch string, macros []string, asm *specAssembly, parseFailures *[]string) (map[string]specparse.SpecDepend, bool) {
	listing, err := c.gitServer.ExecCommand(ctx, entry.CloneURL, "git-ls-tree --name-only "+entry.CommitID)
	if err != nil {
		return nil, c.handleGitFailure(round, repo, "", asm, parseFailures, err)
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
				if !c.handleGitFailure(round, repo, file, asm, parseFailures, err) {
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
			asm.markFailedRepo(repo)
			item := fmt.Sprintf("%s/%s (%v)", repo, file, parseErr)
			*parseFailures = append(*parseFailures, item)
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
// failures skip the affected spec (or the repository for invalid commitId).
// Returns whether the repo may
// still contribute its already-parsed specs this round.
func (c *Controller) handleGitFailure(round *reconcileRound, repo, file string, asm *specAssembly, parseFailures *[]string, err error) bool {
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
	asm.markFailedRepo(repo)
	*parseFailures = append(*parseFailures, item)
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

// determineBuildSet filters this round's full assembly into the build set
// per build type (design 7.2.2). single never reaches here (its assembly is
// the build set, 7.2.3).
func (c *Controller) determineBuildSet(round *reconcileRound, asm *specAssembly, repoLayer *rpmver.RpmMetaSource) (map[string]specparse.SpecDepend, error) {
	switch round.build.Spec.BuildType {
	case "full":
		// No base-round query, no expansion (7.2.2).
		return asm.depends, nil
	case "incremental", "specified":
		seeds := map[string]specparse.SpecDepend{}
		selected := make(map[string]struct{}, len(round.build.Spec.Packages))
		for _, repo := range round.build.Spec.Packages {
			selected[repo] = struct{}{}
		}
		for name, depend := range asm.depends {
			if _, ok := selected[depend.RepoName]; ok {
				seeds[name] = depend
			}
		}
		return expandBuildSet(seeds, asm.depends, repoLayer), nil
	default:
		return nil, fmt.Errorf("unknown buildType %q", round.build.Spec.BuildType)
	}
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
