package buildinfo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	yaml "gopkg.in/yaml.v2"

	"controller-manager/pkg/clients/gitserver"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/controllers/buildinfo/rpmver"
	"controller-manager/pkg/controllers/buildinfo/specparse"
	ebsv1 "ebs-api/ebs/v1"
)

// degradedCondition is one business-degradation condition collected during assembly; entries are aggregated by (type,
// reason) before persisting.
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
	// snapshot is the current Snapshot held for the round.
	snapshot *ebsv1.Snapshot
	// degraded collects the business-degradation conditions (skip paths).
	degraded []degradedCondition
	// incomplete marks transient gaps: stay Pending and re-assemble next round (no cache write-back, no build-set
	// determination).
	incomplete bool
	// failedRepos records deterministic package-level failures independently of conditions, whose messages are not a
	// reliable source of repo names.
	failedRepos map[string]struct{}
}

const maxConcurrentRepoParses = 20

type repoParseInput struct {
	name      string
	originURL string
	status    ebsv1.PackageRepoStatus
}

type repoParseResult struct {
	specs         map[string]specparse.SpecDepend
	parseFailures []string
	failedRepos   map[string]struct{}
	ok            bool
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

// currentSnapshot fetches the same-name Snapshot. A successful GET clears the failure counter; missing objects and
// query failures count toward the SnapshotUnavailable threshold. Below the threshold, they fail this round.
func (c *Controller) currentSnapshot(
	ctx context.Context,
	round *reconcileRound,
) (*ebsv1.Snapshot, bool, controller.ReconcileResult, error) {
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

// assembleSpecDepends runs the full assembly. Pending rounds re-assemble and overwrite the cache; Processing rounds
// reuse a cache hit without any download. The caller has already obtained the current Snapshot for the round.
func (c *Controller) assembleSpecDepends(
	ctx context.Context,
	round *reconcileRound,
	snapshot *ebsv1.Snapshot,
) *specAssembly {
	asm := &specAssembly{snapshot: snapshot}
	// Processing rounds reuse the cached full view.
	if round.current.Status.Phase == ebsv1.BuildInfoProcessing {
		if cached, ok := c.specDependsCache.Get(round.key); ok {
			specDependsCacheHits.Inc()
			asm.depends = cached
			return asm
		}
	}

	repos := c.enumerateRepos(round, snapshot)
	originURLs := make(map[string]string, len(snapshot.Spec.PackageRepos))
	for _, repo := range snapshot.Spec.PackageRepos {
		originURLs[repo.Name] = repo.URL
	}
	arch := round.build.Spec.BuildTarget.Arch
	macros := payloadMacros(round.current.Spec.BuildPayload)
	merged := map[string]specparse.SpecDepend{}
	var parseFailures, commitMissing []string
	var readyRepos []repoParseInput

	for _, repo := range repos {
		originURL, declared := originURLs[repo]
		entry, ok := snapshot.Status.PackageRepoStatuses[repo]
		if !ok {
			// An undeclared seed is invalid; a declared repo without status may still be resolving.
			if !declared {
				asm.markFailedRepo(repo)
				commitMissing = append(commitMissing, repo+" (not in packageRepos)")
				continue
			}
			asm.incomplete = true
			continue
		}
		if entry.Error != nil {
			if entry.Error.Retryable {
				// Still resolving; retry in a later round.
				asm.incomplete = true
				continue
			}
			// Non-retryable resolution failure.
			asm.markFailedRepo(repo)
			commitMissing = append(commitMissing, fmt.Sprintf("%s (%s)", repo, entry.Error.Message))
			continue
		}
		if entry.CommitID == "" {
			// No error and no commitId: still resolving (defensive transient).
			asm.incomplete = true
			continue
		}
		if originURL == "" {
			asm.markFailedRepo(repo)
			commitMissing = append(commitMissing, repo+" (origin URL missing from packageRepos)")
			continue
		}
		readyRepos = append(readyRepos, repoParseInput{name: repo, originURL: originURL, status: entry})
	}

	results := make([]repoParseResult, len(readyRepos))
	indices := make(chan int)
	var group sync.WaitGroup
	for worker := 0; worker < min(len(readyRepos), maxConcurrentRepoParses); worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range indices {
				input := readyRepos[index]
				local := &specAssembly{}
				var failures []string
				specs, ok := c.fetchRepoSpecs(
					ctx,
					round,
					input.name,
					input.originURL,
					input.status,
					arch,
					macros,
					local,
					&failures,
				)
				results[index] = repoParseResult{
					specs:         specs,
					parseFailures: failures,
					failedRepos:   local.failedRepos,
					ok:            ok,
				}
			}
		}()
	}
	for index := range readyRepos {
		indices <- index
	}
	close(indices)
	group.Wait()

	for index, input := range readyRepos {
		result := results[index]
		parseFailures = append(parseFailures, result.parseFailures...)
		for failed := range result.failedRepos {
			asm.markFailedRepo(failed)
		}
		if !result.ok {
			asm.incomplete = true
			continue
		}
		for name, depend := range result.specs {
			if _, clash := merged[name]; clash {
				c.logf(
					round.key,
					"SpecNameClash",
					"spec %s produced by both %s and %s; keeping %s",
					name,
					merged[name].RepoName,
					input.name,
					input.name,
				)
			}
			merged[name] = depend
		}
	}

	if len(parseFailures) > 0 {
		asm.degraded = append(
			asm.degraded,
			degradedCondition{
				condType: ConditionSpecDependsFillFailed,
				reason:   ReasonSpecParseFailed,
				items:    parseFailures,
			},
		)
	}
	if len(commitMissing) > 0 {
		asm.degraded = append(
			asm.degraded,
			degradedCondition{
				condType: ConditionSpecCommitMissing,
				reason:   ReasonSpecCommitMissing,
				items:    commitMissing,
			},
		)
	}
	if asm.incomplete {
		// Transient gap: stay Pending without writing back.
		return asm
	}
	// Cache the completed full view only after all repos have been processed.
	c.ignoreBuildRequires(round, merged)
	c.specDependsCache.Set(round.key, merged)
	specDependsFills.Inc()
	asm.depends = merged
	return asm
}

// ignoreBuildRequires applies the BuildInfo's frozen spec-name list to the assembled view, not to the raw spec-file
// cache. Parsing still happens for every spec; only the controller's build-dependency decisions change.
func (c *Controller) ignoreBuildRequires(round *reconcileRound, depends map[string]specparse.SpecDepend) {
	value, exists := c.parseBuildPayload(round.key, round.current.Spec.BuildPayload)["unparsable_spec"]
	if !exists {
		return
	}
	items, ok := value.([]any)
	if !ok {
		c.logf(round.key, "UnparsableSpecInvalid", "buildPayload.unparsable_spec must be a list of spec names")
		return
	}
	for _, item := range items {
		name, ok := item.(string)
		if !ok {
			c.logf(round.key, "UnparsableSpecInvalid", "buildPayload.unparsable_spec contains a non-string item")
			continue
		}
		depend, found := depends[name]
		if !found {
			continue
		}
		depend.BuildRequires = map[string]ebsv1.VersionConst{}
		depends[name] = depend
	}
}

// enumerateRepos assembles the current Snapshot. Incremental and specified also include seeds absent from the status
// map so both report missing input repositories identically; single only assembles its selected packages.
func (c *Controller) enumerateRepos(round *reconcileRound, snapshot *ebsv1.Snapshot) []string {
	buildType := round.build.Spec.BuildType
	var repos []string
	if buildType == "single" {
		// ebs-apiserver allows duplicate package names in Build.spec.packages; Deduplicate so one repo is assembled once per
		// round.
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

// fetchRepoSpecs downloads and parses every root-level *.spec of one repository. A transient failure anywhere in the
// repo drops the whole repo from this round (ok=false); deterministic failures skip per spec (or the whole repo on an
// invalid commitId) and are recorded as degraded conditions. Specs within one repository are sequential; distinct
// repositories are processed by the bounded pool above.

type parsedRepoSpec struct {
	depend     *specparse.SpecDepend
	err        error
	gitFailure bool
}

func (c *Controller) fetchRepoSpecs(
	ctx context.Context,
	round *reconcileRound,
	repo, originURL string,
	entry ebsv1.PackageRepoStatus,
	arch string,
	macros []string,
	asm *specAssembly,
	parseFailures *[]string,
) (map[string]specparse.SpecDepend, bool) {
	listing, err := c.gitServer.ExecCommand(ctx, originURL, "git-ls-tree --name-only "+entry.CommitID)
	if err != nil {
		return nil, c.handleGitFailure(round, repo, "", asm, parseFailures, err)
	}
	var files []string
	rootFiles := make(map[string]struct{})
	for _, line := range strings.Split(listing, "\n") {
		line = strings.TrimSpace(line)
		// Only root-level entries can contain target specs.
		if line == "" || strings.Contains(line, "/") {
			continue
		}
		rootFiles[line] = struct{}{}
		if strings.HasSuffix(line, ".spec") {
			files = append(files, line)
		}
	}
	sort.Strings(files)

	specs := map[string]specparse.SpecDepend{}
	for _, file := range files {
		result := c.parseRepoSpec(ctx, file, originURL, entry.CommitID, repo, arch, macros, rootFiles)
		if result.err != nil {
			if result.gitFailure {
				if !c.handleGitFailure(round, repo, file, asm, parseFailures, result.err) {
					return nil, false
				}
				continue
			}
			asm.markFailedRepo(repo)
			item := fmt.Sprintf("%s/%s (%v)", repo, file, result.err)
			*parseFailures = append(*parseFailures, item)
			continue
		}
		depend := result.depend
		if _, clash := specs[depend.SpecName]; clash {
			// Same as the cross-repo clash below: files iterate in dictionary order, so the lexicographically later file wins —
			// keep a trace.
			c.logf(
				round.key,
				"SpecNameClash",
				"spec %s produced by multiple files in repo %s; keeping %s",
				depend.SpecName,
				repo,
				file,
			)
		}
		specs[depend.SpecName] = *depend
	}
	return specs, true
}

func (c *Controller) parseRepoSpec(
	ctx context.Context,
	file, originURL, commitID, repo, arch string,
	macros []string,
	rootFiles map[string]struct{},
) parsedRepoSpec {
	content, hit := c.specFiles.Get(commitID, file)
	if hit {
		specFileCacheHits.Inc()
	} else {
		var err error
		content, err = c.gitServer.ExecCommand(ctx, originURL, "git-show "+commitID+":"+file)
		if err != nil {
			return parsedRepoSpec{err: err, gitFailure: true}
		}
		// Raw content remains valid at this commit even if parsing fails.
		c.specFiles.Add(commitID, file, content)
	}
	depend, err := specparse.ParseWithSources(content, file, repo, arch, macros, func(name string) (string, error) {
		if _, exists := rootFiles[name]; !exists {
			return "", os.ErrNotExist
		}
		return c.gitServer.ExecCommand(ctx, originURL, "git-show "+commitID+":"+name)
	})
	if err != nil {
		var fetchErr *specparse.SourceFetchError
		if errors.As(err, &fetchErr) {
			return parsedRepoSpec{err: fetchErr.Err, gitFailure: true}
		}
		return parsedRepoSpec{err: err}
	}
	return parsedRepoSpec{depend: depend}
}

// handleGitFailure routes a git-server failure: transient failures drop the repo from this round (false = incomplete);
// deterministic failures skip the affected spec (or the repository for invalid commitId). It reports whether the repo
// can contribute already-parsed specs this round.
func (c *Controller) handleGitFailure(
	round *reconcileRound,
	repo, file string,
	asm *specAssembly,
	parseFailures *[]string,
	err error,
) bool {
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

// parseBuildPayload decodes BuildInfo.spec.buildPayload:
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

// payloadMacros extracts the buildPayload macros list for specparse --load; a non-list macros key yields nil.
func payloadMacros(raw string) []string {
	var decoded map[string]any
	if err := yaml.Unmarshal([]byte(raw), &decoded); err != nil || decoded == nil {
		return nil
	}
	return stringList(decoded["macros"])
}

// payloadPrefer extracts the buildPayload prefer list; a non-list prefer key yields nil.
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

// --- Build-set determination ---

// determineBuildSet filters this round's full assembly into the build set per build type. Single builds use their
// assembled specs directly.
func (c *Controller) determineBuildSet(
	round *reconcileRound,
	asm *specAssembly,
	repoLayer *rpmver.RpmMetaSource,
) (map[string]specparse.SpecDepend, error) {
	switch round.build.Spec.BuildType {
	case "full":
		// Full builds use the complete assembly without expansion.
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

// expandBuildSet selects only specs directly depending on a seed. BuildRequires is checked against the full assembly,
// and install Requires against RPMs from this project's repository layer. Newly selected specs do not become seeds.
func expandBuildSet(
	seeds, full map[string]specparse.SpecDepend,
	repoLayer *rpmver.RpmMetaSource,
) map[string]specparse.SpecDepend {
	buildSet := make(map[string]specparse.SpecDepend, len(seeds))
	provides := map[string]struct{}{}
	for name, depend := range seeds {
		buildSet[name] = depend
		provides[name] = struct{}{}
		for _, provide := range depend.Provides {
			provides[provide] = struct{}{}
		}
	}
	if repoLayer != nil {
		for _, rpm := range repoLayer.RpmByName {
			if _, ok := seeds[rpm.SpecName]; !ok {
				continue
			}
			for provide := range rpm.Provides {
				provides[provide] = struct{}{}
			}
		}
	}
	for name, depend := range full {
		if _, ok := seeds[name]; ok {
			continue
		}
		if keysIntersect(depend.BuildRequires, provides) {
			buildSet[name] = depend
		}
	}
	if repoLayer != nil {
		for _, rpm := range repoLayer.RpmByName {
			depend, produced := full[rpm.SpecName]
			if !produced {
				continue
			}
			if keysIntersect(rpm.Requires, provides) {
				buildSet[rpm.SpecName] = depend
			}
		}
	}
	return buildSet
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
