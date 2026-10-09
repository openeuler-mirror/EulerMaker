// Job dispatch and backfill use deterministic names to recover from uncertain create results without duplicating Jobs.
package buildinfo

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	yaml "gopkg.in/yaml.v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"
	syaml "sigs.k8s.io/yaml"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/controllers/buildinfo/rpmver"
	"controller-manager/pkg/controllers/buildinfo/specparse"
	"controller-manager/pkg/controllers/specname"
	ebsv1 "ebs-api/ebs/v1"
)

const (
	jobRuntime         = "ct"
	jobTimeoutSeconds  = 10800
	runnerArchSelector = "ebs.io/runner-arch"

	annDispatchGeneration = "ebs.io/dispatch-generation"
)

// jobNameFor derives the deterministic Job name from the first 8 bytes of SHA-256 over json.Marshal([buildInfoUID,
// specName, generation]). The generation remains in the hash input, but is not shown in the name.
func jobNameFor(buildInfoUID, specName string, generation int64) string {
	sum := jobNameHash(buildInfoUID, specName, generation)
	return fmt.Sprintf("%s-%x", specname.Encode(specName), sum[:8])
}

func jobNameHash(buildInfoUID, specName string, generation int64) [sha256.Size]byte {
	identity, _ := json.Marshal([]string{buildInfoUID, specName, strconv.FormatInt(generation, 10)})
	return sha256.Sum256(identity)
}

// packageNameLabelValue uses the spec-name encoding for repository names. Values over 63 bytes are truncated, so only
// untruncated values are reversible.
func packageNameLabelValue(name string) string {
	value := specname.Encode(name)
	if len(value) > 63 {
		value = value[:63]
	}
	return strings.TrimRight(value, "-_.")
}

// archSupported checks the exclusiveArch whitelist; an empty whitelist means every arch (normalized at parse time,
// defensive here).
func archSupported(depend *specparse.SpecDepend, arch string) bool {
	if len(depend.ExclusiveArch) == 0 {
		return true
	}
	for _, allowed := range depend.ExclusiveArch {
		if allowed == arch {
			return true
		}
	}
	return false
}

// missingBuildRequires returns unavailable build requirements, excluding buildRemoves, in sorted order without
// duplicates.
func missingBuildRequires(depend *specparse.SpecDepend, sources *rpmver.RpmMetaSources) []string {
	var missing []string
	for _, name := range sortedConstKeys(depend.BuildRequires, depend.BuildRemoves) {
		if !sources.Available(name, depend.BuildRequires[name]) {
			missing = append(missing, name)
		}
	}
	return missing
}

// --- Dispatch ---

const maxJobCreatesPerReconcile = 20

type createdJob struct {
	job        *ebsv1.Job
	generation int64
}

// dispatchSpec creates one deterministically named Job and stages its confirmation for a batched status write.
// Persisted pending entries from earlier rounds retain their GET-based recovery path. Build-resource Config is read
// once per round. Architecture and dependency checks and image resolution happen at the caller.
func (c *Controller) dispatchSpec(
	ctx context.Context,
	round *reconcileRound,
	specName string,
	depend *specparse.SpecDepend,
	snapshot *ebsv1.Snapshot,
	image, contentURL string,
	sources *rpmver.RpmMetaSources,
) (controller.ReconcileResult, error) {
	if _, alreadyCreated := round.createdJobs[specName]; alreadyCreated {
		return controller.ReconcileResult{}, nil
	}
	if round.jobCreateRequests >= maxJobCreatesPerReconcile {
		return controller.ReconcileResult{Requeue: true}, nil
	}
	namespace := round.current.Namespace
	var generation int64
	var name string
	var err error
	entryExisted := false
	if pend, ok := round.current.Status.PendingJobCreates[specName]; ok {
		// Registered entries are GET-verified first; a hit confirms
		// (no new generation), a 404 re-creates with the same identity.
		entryExisted = true
		generation, name = pend.DispatchGeneration, pend.JobName
		if expected := jobNameFor(string(round.current.UID), specName, generation); name != expected {
			return controller.ReconcileResult{}, controller.NewPermanentError(
				fmt.Errorf("pending Job name %q does not match current deterministic name %q", name, expected),
			)
		}
		existing, err := c.client.GetJob(ctx, namespace, name)
		if err == nil {
			if verr := verifyJobIdentity(existing, round.current, specName, generation); verr != nil {
				c.logf(round.key, "JobIdentityMismatch", "job %s identity mismatch: %v", name, verr)
				return controller.ReconcileResult{}, controller.NewPermanentError(verr)
			}
			return c.confirmDispatchedJob(ctx, round, specName, generation, existing)
		}
		if !errors.Is(err, ErrNotFound) {
			return controller.ReconcileResult{}, err
		}
	} else {
		generation = round.current.Status.SpecStatus.Entry(specName).DispatchCount + 1
		name = jobNameFor(string(round.current.UID), specName, generation)
	}

	// The cluster-wide default table is the sole source of Job resources.
	resource := round.resourceRules
	if resource == nil {
		resource, err = c.client.GetBuildResourceRules(ctx)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				message := "build-resource Config not found"
				return c.markSpecFailed(
					ctx,
					round,
					specName,
					ConditionDefaultBuildResourceConfigNotFound,
					ReasonDefaultBuildResourceConfigNotFound,
					message,
					!entryExisted,
				)
			}
			return controller.ReconcileResult{}, err
		}
		round.resourceRules = resource
	}

	if round.scriptRef == nil {
		scriptName, err := scriptNameFromPayload(round.current.Spec.BuildPayload)
		if err != nil {
			return controller.ReconcileResult{}, controller.NewPermanentError(
				fmt.Errorf("select Script for Job %s: %w", name, err),
			)
		}
		script, err := c.client.GetScript(ctx, scriptName)
		if err != nil {
			return controller.ReconcileResult{}, fmt.Errorf("get Script %q for Job %s: %w", scriptName, name, err)
		}
		round.scriptRef = &ebsv1.ScriptRef{
			Name:            script.Name,
			UID:             string(script.UID),
			ResourceVersion: script.ResourceVersion,
		}
	}
	// A Job owns its observation; the round only shares the fetched value.
	scriptRef := *round.scriptRef

	if len(payloadPrefer(c.parseBuildPayload(round.key, round.current.Spec.BuildPayload))) > 0 && sources == nil {
		if !round.isSingle() {
			return controller.ReconcileResult{}, fmt.Errorf(
				"RPM metadata unavailable for Job %s prefer selection",
				name,
			)
		}
		// Existing pending Jobs were GET-confirmed above. Only a new create needs repository metadata to derive this spec's
		// prefer payload.
		var reason string
		sources, reason, err = c.loadSinglePreferSources(ctx, round, contentURL)
		if err != nil {
			_, result, failureErr := c.rpmMetaUnavailable(ctx, round, reason, err)
			if failureErr == nil && result == (controller.ReconcileResult{}) {
				// A single build may have dispatched earlier specs this round. Requeue stops further dispatch and lets reconcile
				// flush them.
				return controller.ReconcileResult{Requeue: true}, nil
			}
			return result, failureErr
		}
		if result, err := c.clearRpmRepoRetrying(ctx, round); err != nil || result != (controller.ReconcileResult{}) {
			return result, err
		}
	}
	job := c.jobForSpec(
		round,
		specName,
		depend,
		snapshot,
		image,
		contentURL,
		resource,
		scriptRef,
		name,
		generation,
		sources,
	)
	round.jobCreateRequests++
	created, err := c.client.CreateJob(ctx, namespace, job)
	var writeErr *clientpkg.WriteError
	isWriteErr := errors.As(err, &writeErr)
	if !isWriteErr || writeErr.Outcome != clientpkg.WriteNotSent {
		// jobCreates counts create requests actually sent (metrics.go); WriteNotSent failed local validation before any
		// request left the controller, so it is not counted.
		jobCreates.Inc()
	}
	if err == nil {
		return c.stageCreatedJob(round, specName, generation, created)
	}
	if !isWriteErr {
		jobCreateFailures.Inc()
		return controller.ReconcileResult{}, err
	}
	switch writeErr.Outcome {
	case clientpkg.WriteNotSent:
		// Local validation failed before the create request was sent.
		jobCreateFailures.Inc()
		return controller.ReconcileResult{}, controller.NewPermanentError(err)
	case clientpkg.WriteRejected:
		switch writeErr.StatusCode {
		case 409:
			// AlreadyExists: identity verification takes precedence over the generic conflict requeue; a 404 confirmation read
			// is a retryable error, never the main-object NotFound rule.
			existing, gerr := c.client.GetJob(ctx, namespace, name)
			if gerr != nil {
				jobCreateFailures.Inc()
				return controller.ReconcileResult{}, gerr
			}
			if verr := verifyJobIdentity(existing, round.current, specName, generation); verr != nil {
				c.logf(round.key, "JobIdentityMismatch", "job %s identity mismatch after AlreadyExists: %v", name, verr)
				return controller.ReconcileResult{}, controller.NewPermanentError(verr)
			}
			return c.stageCreatedJob(round, specName, generation, existing)
		case 400, 422:
			// This Job was definitely rejected. Close only its spec so an independent spec can still be dispatched in the same
			// round.
			jobCreateFailures.Inc()
			c.logf(
				round.key,
				ReasonJobCreateRejected,
				"job %s for spec %s rejected with HTTP %d: %v",
				name,
				specName,
				writeErr.StatusCode,
				err,
			)
			return c.markSpecFailed(ctx, round, specName, ConditionJobCreateRejected, ReasonJobCreateRejected,
				fmt.Sprintf("Job creation rejected with HTTP %d", writeErr.StatusCode), true)
		default:
			// Let the controller handle any other rejected create.
			jobCreateFailures.Inc()
			return controller.ReconcileResult{}, err
		}
	default:
		// An unknown create result needs a GET by deterministic name.
		unknownWrites.Inc()
		existing, gerr := c.client.GetJob(ctx, namespace, name)
		if gerr == nil {
			if verr := verifyJobIdentity(existing, round.current, specName, generation); verr != nil {
				c.logf(round.key, "JobIdentityMismatch", "job %s identity mismatch after Unknown: %v", name, verr)
				return controller.ReconcileResult{}, controller.NewPermanentError(verr)
			}
			return c.stageCreatedJob(round, specName, generation, existing)
		}
		jobCreateFailures.Inc()
		if errors.Is(gerr, ErrNotFound) {
			// A late create can still land. Re-entry uses the same deterministic name; 409 and the initial Job list both recover
			// it.
			return controller.ReconcileResult{}, err
		}
		return controller.ReconcileResult{}, gerr
	}
}

// stageCreatedJob records a confirmed Job in the current batch. The batch is folded into the next status write, or
// flushed once at the end of the round.
func (c *Controller) stageCreatedJob(
	round *reconcileRound,
	specName string,
	generation int64,
	job *ebsv1.Job,
) (controller.ReconcileResult, error) {
	if round.createdJobs == nil {
		round.createdJobs = make(map[string]createdJob)
	}
	created := createdJob{job: job, generation: generation}
	round.createdJobs[specName] = created
	round.current = round.current.DeepCopy()
	applyCreatedJobs(round.current, map[string]createdJob{specName: created})
	return controller.ReconcileResult{}, nil
}

func applyCreatedJobs(next *ebsv1.BuildInfo, jobs map[string]createdJob) {
	for name, created := range jobs {
		ss := next.Status.SpecStatus.Entry(name)
		if ss.DispatchCount < created.generation {
			ss.DispatchCount = created.generation
		}
		applyJobPhase(&ss, created.job, false)
		next.Status.SpecStatus.Set(name, ss)
		delete(next.Status.PendingJobCreates, name)
	}
}

func (c *Controller) flushCreatedJobs(ctx context.Context, round *reconcileRound) (controller.ReconcileResult, error) {
	next := round.current.DeepCopy()
	return c.writeStatus(ctx, round, next)
}

func (c *Controller) confirmCreatedJobs(round *reconcileRound) {
	if len(round.createdJobs) > 0 {
		dispatches.Add(uint64(len(round.createdJobs)))
		round.createdJobs = nil
	}
}

// confirmDispatchedJob persists the confirmed generation and Job phase, then removes its pending entry in the same
// write.
func (c *Controller) confirmDispatchedJob(
	ctx context.Context,
	round *reconcileRound,
	specName string,
	generation int64,
	job *ebsv1.Job,
) (controller.ReconcileResult, error) {
	next := round.current.DeepCopy()
	ss := next.Status.SpecStatus.Entry(specName)
	if ss.DispatchCount < generation {
		ss.DispatchCount = generation
	}
	applyJobPhase(&ss, job, false)
	delete(next.Status.PendingJobCreates, specName)
	next.Status.SpecStatus.Set(specName, ss)
	result, err := c.writeStatus(ctx, round, next)
	if err == nil && result == (controller.ReconcileResult{}) {
		dispatches.Inc()
	}
	return result, err
}

// markSpecFailed records a pre-dispatch failure without creating a Job. A reused pending registration is checked before
// removal so an in-flight Job cannot be lost when the spec becomes terminal.
func (c *Controller) markSpecFailed(
	ctx context.Context,
	round *reconcileRound,
	specName, condType, reason, message string,
	clearPending bool,
) (controller.ReconcileResult, error) {
	return c.markSpecTerminal(ctx, round, specName, SpecBuildFailed, condType, reason, message, clearPending)
}

func (c *Controller) markSpecTerminal(
	ctx context.Context,
	round *reconcileRound,
	specName, status, condType, reason, message string,
	clearPending bool,
) (controller.ReconcileResult, error) {
	next := round.current.DeepCopy()
	ss := next.Status.SpecStatus.Entry(specName)
	ss.Build.Status = status
	if condType != "" {
		specCondition(&ss, condType, reason, message)
	}
	next.Status.SpecStatus.Set(specName, ss)
	if clearPending {
		delete(next.Status.PendingJobCreates, specName)
	} else if pend, ok := next.Status.PendingJobCreates[specName]; ok {
		job, err := c.client.GetJob(ctx, round.current.Namespace, pend.JobName)
		if err == nil {
			if verr := verifyJobIdentity(job, round.current, specName, pend.DispatchGeneration); verr != nil {
				c.logf(round.key, "JobIdentityMismatch", "job %s identity mismatch at spec-failed closeout: %v", pend.JobName, verr)
				return controller.ReconcileResult{}, controller.NewPermanentError(verr)
			}
			// The Job exists: keep the entry; the backfill fold resolves it.
		} else if errors.Is(err, ErrNotFound) {
			delete(next.Status.PendingJobCreates, specName)
		} else {
			return controller.ReconcileResult{}, err
		}
	}
	return c.writeStatus(ctx, round, next)
}

// verifyJobIdentity checks a found Job against the creation identity using its namespace, recomputed name, build/spec
// labels, and the dispatch-generation annotation must all match.
func verifyJobIdentity(job *ebsv1.Job, buildInfo *ebsv1.BuildInfo, specName string, generation int64) error {
	if job.Namespace != buildInfo.Namespace {
		return fmt.Errorf("namespace %q != %q", job.Namespace, buildInfo.Namespace)
	}
	if job.Name != jobNameFor(string(buildInfo.UID), specName, generation) {
		return fmt.Errorf(
			"name %q does not match the deterministic name for spec %q generation %d",
			job.Name,
			specName,
			generation,
		)
	}
	if job.Labels[ebsv1.JobBuildNameLabel] != buildInfo.Name {
		return fmt.Errorf("build-name label %q != %q", job.Labels[ebsv1.JobBuildNameLabel], buildInfo.Name)
	}
	if job.Labels[ebsv1.JobSpecNameLabel] != specname.Encode(specName) {
		return fmt.Errorf("spec-name label %q != %q", job.Labels[ebsv1.JobSpecNameLabel], specname.Encode(specName))
	}
	if job.Annotations[annDispatchGeneration] != strconv.FormatInt(generation, 10) {
		return fmt.Errorf("dispatch-generation annotation %q != %d", job.Annotations[annDispatchGeneration], generation)
	}
	return nil
}

// --- Job construction ---

// scriptNameFromPayload selects the script to observe before creating a Job. A malformed selection must not silently
// fall back to the default.
func scriptNameFromPayload(raw string) (string, error) {
	name := "rpmbuild"
	if strings.TrimSpace(raw) != "" {
		var payload map[string]any
		if err := yaml.Unmarshal([]byte(raw), &payload); err != nil {
			return "", fmt.Errorf("decode buildPayload: %w", err)
		}
		if value, exists := payload["rpmbuild_script"]; exists {
			selected, ok := value.(string)
			if !ok {
				return "", fmt.Errorf("buildPayload.rpmbuild_script must be a string")
			}
			if strings.TrimSpace(selected) != "" {
				name = selected
			}
		}
	}
	if reasons := kvalidation.IsDNS1123Subdomain(name); len(reasons) > 0 {
		return "", fmt.Errorf("invalid buildPayload.rpmbuild_script %q: %s", name, strings.Join(reasons, ", "))
	}
	return name, nil
}

// jobForSpec builds the Job object with every controller-filled field.
func (c *Controller) jobForSpec(
	round *reconcileRound,
	specName string,
	depend *specparse.SpecDepend,
	snapshot *ebsv1.Snapshot,
	image, contentURL string,
	resource *buildResourceRules,
	scriptRef ebsv1.ScriptRef,
	name string,
	generation int64,
	sources *rpmver.RpmMetaSources,
) *ebsv1.Job {
	buildInfo := round.current
	target := round.build.Spec.BuildTarget
	runtimeSpec, _ := json.Marshal(map[string]string{"image": image, "networkMode": "host"})
	return &ebsv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: ebsv1.SchemeGroupVersion.String(), Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: buildInfo.Namespace,
			Labels: map[string]string{
				ebsv1.JobBuildNameLabel:    buildInfo.Name,
				ebsv1.JobSpecNameLabel:     specname.Encode(specName),
				ebsv1.JobPackageNameLabel:  packageNameLabelValue(depend.RepoName),
				ebsv1.BuildTargetOSLabel:   target.Os,
				ebsv1.BuildTargetArchLabel: target.Arch,
			},
			Annotations: map[string]string{
				annDispatchGeneration: strconv.FormatInt(generation, 10),
			},
		},
		Spec: ebsv1.JobSpec{
			ScriptRefs:     []ebsv1.ScriptRef{scriptRef},
			Runtime:        jobRuntime,
			RuntimeSpec:    runtime.RawExtension{Raw: runtimeSpec},
			TimeoutSeconds: jobTimeoutSeconds,
			Resources:      resolveResources(resource, specName, target.Arch),
			NodeSelector:   map[string]string{runnerArchSelector: target.Arch},
			Payload:        c.jobPayload(round, specName, depend, snapshot, contentURL, sources),
		},
	}
}

// jobPayload assembles only the recognized per-Job fields from buildPayload and the resolved spec and repository
// inputs.
func (c *Controller) jobPayload(
	round *reconcileRound,
	specName string,
	depend *specparse.SpecDepend,
	snapshot *ebsv1.Snapshot,
	contentURL string,
	sources *rpmver.RpmMetaSources,
) string {
	configured := c.parseBuildPayload(round.key, round.current.Spec.BuildPayload)
	payload := map[string]any{
		"spec_name":      specName,
		"spec_file_name": depend.SpecFileName,
		"package_name":   depend.RepoName,
	}
	if preinstall, exists := configured["preinstall"]; exists {
		// Keep the declared value so the script can reject malformed input.
		payload["preinstall"] = preinstall
	}
	configuredPrefer := payloadPrefer(configured)
	if matched := jobPrefer(depend, sources, configuredPrefer); len(matched) > 0 {
		payload["prefer"] = strings.Join(matched, " ")
	}
	// The project-level list names package repositories, not spec files. Only a matching Job receives the flag; otherwise
	// omit the base list.
	if packageRepoListed(configured["disable_check_path"], depend.RepoName) {
		payload["disable_check_path"] = true
	}
	if packageRepoListed(configured["use_kmod_libs"], depend.RepoName) {
		payload["use_kmod_libs"] = true
	}
	if packageRepoListed(configured["use_git_lfs"], depend.RepoName) {
		payload["use_git_lfs"] = true
	}
	if packageRepoListed(configured["use_root"], depend.RepoName) {
		payload["use_root"] = true
	}
	if packageRepoListed(configured["use_xz"], depend.RepoName) {
		payload["use_xz"] = true
	}
	if packageRepoListed(configured["unuse_gcc_secure"], depend.RepoName) {
		payload["unuse_gcc_secure"] = true
	}
	if entry, ok := snapshot.Status.PackageRepoStatuses[depend.RepoName]; !ok || entry.CommitID == "" {
		// Missing snapshot data does not block dispatch.
		c.logf(
			round.key,
			"SpecRepoEntryMissing",
			"packageRepoStatuses entry for repo %s missing or without commitId; spec_url/commit_id not injected",
			depend.RepoName,
		)
	} else {
		payload["spec_url"] = entry.CloneURL
		payload["commit_id"] = entry.CommitID
	}
	bootstrapRepos := bootstrapRepoURLs(round.current.Spec.BootstrapRepo, round.build.Spec.BuildTarget.Arch)
	repoURLs := repoPayloadURLs(contentURL, bootstrapRepos)
	if len(repoURLs) == 0 {
		// Use an explicitly configured fallback, but emit the same array shape as repositories resolved by the controller.
		repoURLs = configuredRepoURLs(configured["repo"])
	}
	if len(repoURLs) == 0 {
		return c.marshalPayload(round, payload)
	}
	repos := make([]payloadRepo, len(repoURLs))
	for i, url := range repoURLs {
		priority := 99
		if i == 0 && contentURL != "" {
			priority = 10
		}
		repos[i] = payloadRepo{URL: url, Priority: priority}
	}
	payload["repo"] = repos
	return c.marshalPayload(round, payload)
}

type payloadRepo struct {
	URL      string `json:"url"`
	Priority int    `json:"priority"`
}

func packageRepoListed(value any, repoName string) bool {
	for _, configuredName := range stringList(value) {
		if configuredName == repoName {
			return true
		}
	}
	return false
}

// jobPrefer uses the same layered provider choice as dependency graph construction, but records only choices made by
// the prefer step. Dependency names are sorted so payloads are stable across reconciles.
func jobPrefer(depend *specparse.SpecDepend, sources *rpmver.RpmMetaSources, configured []string) []string {
	if sources == nil || len(configured) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	var matched []string
	for _, name := range sortedConstKeys(depend.BuildRequires, depend.BuildRemoves) {
		selection, ok := sources.FindProvider(name, depend.BuildRequires[name], configured)
		if ok && selection.Reason == rpmver.SelectionPrefer && !seen[selection.RPMName] {
			seen[selection.RPMName] = true
			matched = append(matched, selection.RPMName)
		}
	}
	return matched
}

// repoPayloadURLs orders the RpmRepo contentURL (first when non-empty) before the bootstrap repo URLs in declaration
// order.
func repoPayloadURLs(contentURL string, bootstrapRepos []string) []string {
	parts := make([]string, 0, len(bootstrapRepos)+1)
	if contentURL != "" {
		parts = append(parts, contentURL)
	}
	parts = append(parts, bootstrapRepos...)
	return parts
}

func configuredRepoURLs(value any) []string {
	switch value := value.(type) {
	case string:
		return strings.Fields(value)
	case []any:
		urls := make([]string, 0, len(value))
		for _, item := range value {
			if url, ok := item.(string); ok && strings.TrimSpace(url) != "" {
				urls = append(urls, url)
			}
		}
		return urls
	default:
		return nil
	}
}

func (c *Controller) marshalPayload(round *reconcileRound, fields map[string]any) string {
	payloadYAML, err := yaml.Marshal(fields)
	if err != nil {
		// Defensive: the fields are YAML-decoded values plus derived values, so a marshal failure is a programming error;
		// keep a trace.
		c.logf(round.key, "PayloadMarshalFailed", "payload marshal failed: %v", err)
		return ""
	}
	// buildPayload accepts YAML, including nested maps. Convert the assembled representation to JSON so scripts can parse
	// long scalar values reliably.
	payload, err := syaml.YAMLToJSON(payloadYAML)
	if err != nil {
		c.logf(round.key, "PayloadMarshalFailed", "payload JSON conversion failed: %v", err)
		return ""
	}
	return string(payload)
}

// resolveResources overlays spec defaults, package defaults, then architecture settings. Missing limits inherit
// requests from the same level.
func resolveResources(resource *buildResourceRules, specName, arch string) ebsv1.ResourceRequirements {
	merged := normalizeResourceLevel(resource.Spec.Default)
	if pkg, ok := resource.Spec.Packages[specName]; ok {
		merged = overlayResources(merged, pkg.Default)
		if archLevel, ok := pkg.Arches[arch]; ok {
			merged = overlayResources(merged, archLevel)
		}
	}
	return merged
}

// normalizeResourceLevel fills a level's missing limits from its own requests.
func normalizeResourceLevel(level ebsv1.ResourceRequirements) ebsv1.ResourceRequirements {
	out := deepCopyResources(level)
	for _, key := range []string{"cpu", "memory"} {
		if out.Limits[key] == "" {
			if request := out.Requests[key]; request != "" {
				if out.Limits == nil {
					out.Limits = map[string]string{}
				}
				out.Limits[key] = request
			}
		}
	}
	return out
}

// overlayResources applies one more specific level over the merged base.
func overlayResources(base, over ebsv1.ResourceRequirements) ebsv1.ResourceRequirements {
	over = normalizeResourceLevel(over)
	out := deepCopyResources(base)
	for key, value := range over.Requests {
		if out.Requests == nil {
			out.Requests = map[string]string{}
		}
		out.Requests[key] = value
	}
	for key, value := range over.Limits {
		if out.Limits == nil {
			out.Limits = map[string]string{}
		}
		out.Limits[key] = value
	}
	return out
}

func deepCopyResources(in ebsv1.ResourceRequirements) ebsv1.ResourceRequirements {
	out := ebsv1.ResourceRequirements{}
	if in.Requests != nil {
		out.Requests = make(map[string]string, len(in.Requests))
		for key, value := range in.Requests {
			out.Requests[key] = value
		}
	}
	if in.Limits != nil {
		out.Limits = make(map[string]string, len(in.Limits))
		for key, value := range in.Limits {
			out.Limits[key] = value
		}
	}
	return out
}

// --- Job backfill ---

// groupJobsBySpec groups listed Jobs by their spec-name label; Jobs without a label or outside the scope set are
// skipped.
func (c *Controller) groupJobsBySpec(
	round *reconcileRound,
	jobs []ebsv1.Job,
	scope map[string]bool,
) map[string][]ebsv1.Job {
	bySpec := map[string][]ebsv1.Job{}
	for i := range jobs {
		job := jobs[i]
		encoded := job.Labels[ebsv1.JobSpecNameLabel]
		spec, valid := specname.Decode(encoded)
		if !valid || !scope[spec] {
			c.logf(round.key, "OrphanJob", "job %s spec-name %q out of scope, skipped", job.Name, encoded)
			continue
		}
		bySpec[spec] = append(bySpec[spec], job)
	}
	return bySpec
}

// backfillJobs folds listed Jobs into next.Status. Only Jobs named for this BuildInfo UID count, so a replacement with
// the same name cannot inherit old phases or dispatch counts. createMissing also recovers Jobs created before their
// initial spec status was persisted.
func (c *Controller) backfillJobs(
	round *reconcileRound,
	next *ebsv1.BuildInfo,
	jobs []ebsv1.Job,
	scope map[string]bool,
	createMissing bool,
) map[string][]ebsv1.Job {
	bySpec := c.groupJobsBySpec(round, jobs, scope)
	uid := string(round.current.UID)
	for spec, group := range bySpec {
		ss, exists := next.Status.SpecStatus.Lookup(spec)
		if !exists {
			if !createMissing {
				continue
			}
			ss = ebsv1.SpecStatus{}
		}
		own := filterJobsByIdentity(group, uid)
		if len(own) < len(group) {
			c.logf(
				round.key,
				"ForeignIncarnationJob",
				"spec %s: %d of %d listed jobs belong to a previous same-name buildinfo; excluded from folding",
				spec,
				len(group)-len(own),
				len(group),
			)
		}
		if len(own) > 0 {
			// Preserve the highest confirmed generation even if older Jobs were cleaned up.
			for i := range own {
				job := &own[i]
				if gen, err := strconv.ParseInt(job.Annotations[annDispatchGeneration], 10, 64); err == nil &&
					gen > ss.DispatchCount {
					ss.DispatchCount = gen
				}
			}
			latest := latestJob(own)
			if applyJobPhase(&ss, latest, priorSucceeded(own, latest)) {
				if latest.Status.Phase == ebsv1.JobSucceeded {
					c.backfillInstall(round, &ss, latest)
				}
			} else {
				c.logf(
					round.key, "UnknownJobPhase", "job %s phase %q unknown, status mapping skipped",
					latest.Name, latest.Status.Phase,
				)
			}
		}
		// An identity-matched Job in the list confirms the pending entry (removed in the same write, no double counting).
		if pend, ok := next.Status.PendingJobCreates[spec]; ok {
			for i := range own {
				job := &own[i]
				if job.Name == pend.JobName &&
					job.Annotations[annDispatchGeneration] == strconv.FormatInt(pend.DispatchGeneration, 10) {
					delete(next.Status.PendingJobCreates, spec)
					break
				}
			}
		}
		next.Status.SpecStatus.Set(spec, ss)
		bySpec[spec] = own
	}
	return bySpec
}

// filterJobsByIdentity checks the deterministic name against the current BuildInfo UID, spec label and dispatch
// generation.
func filterJobsByIdentity(jobs []ebsv1.Job, uid string) []ebsv1.Job {
	out := make([]ebsv1.Job, 0, len(jobs))
	for i := range jobs {
		job := &jobs[i]
		generation, err := strconv.ParseInt(job.Annotations[annDispatchGeneration], 10, 64)
		spec, valid := specname.Decode(job.Labels[ebsv1.JobSpecNameLabel])
		if err == nil && generation > 0 && valid && job.Name == jobNameFor(uid, spec, generation) {
			out = append(out, jobs[i])
		}
	}
	return out
}

// latestJob picks the multi-generation target Job by the greatest (creationTimestamp, name) pair; a zero timestamp
// sorts earliest. An empty group yields nil — callers dispatching on the result must guard.
func latestJob(group []ebsv1.Job) *ebsv1.Job {
	if len(group) == 0 {
		return nil
	}
	best := -1
	for i := range group {
		if best < 0 || jobLess(&group[best], &group[i]) {
			best = i
		}
	}
	return &group[best]
}

func jobLess(a, b *ebsv1.Job) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Name < b.Name
}

// priorSucceeded detects a previous successful generation so a failed rebuild receives RebuildFailed instead of
// BuildFailed.
func priorSucceeded(group []ebsv1.Job, latest *ebsv1.Job) bool {
	for i := range group {
		job := &group[i]
		if job.Name != latest.Name && job.Status.Phase == ebsv1.JobSucceeded {
			return true
		}
	}
	return false
}

// applyJobPhase maps the target Job phase onto build.status; the bool reports a known phase. Pending forces Running
// (never inherit a previous generation's terminal state). An unknown phase leaves the build status unchanged.
func applyJobPhase(ss *ebsv1.SpecStatus, job *ebsv1.Job, succeededPrior bool) bool {
	switch job.Status.Phase {
	case ebsv1.JobPending, ebsv1.JobRunning:
		ss.Build.Status = SpecBuildRunning
	case ebsv1.JobSucceeded:
		if job.Status.Build != nil && job.Status.Build.Status == ebsv1.JobResultFailed {
			ss.Build.Status = SpecBuildFailed
			specCondition(ss, ConditionBuildFailed, ReasonJobFailed, "job "+job.Name+" reported build failure")
		} else {
			ss.Build.Status = SpecBuildSucceeded
		}
	case ebsv1.JobFailed:
		ss.Build.Status = SpecBuildFailed
		if succeededPrior {
			specCondition(ss, ConditionRebuildFailed, ReasonRebuildJobFailed, "job "+job.Name+" rebuild failed")
		} else {
			specCondition(ss, ConditionBuildFailed, ReasonJobFailed, "job "+job.Name+" failed")
		}
	case ebsv1.JobAborted:
		ss.Build.Status = SpecBuildFailed
		specCondition(
			ss,
			ConditionBuildAborted,
			ReasonBuildAborted,
			"job "+job.Name+" aborted (defensive: treated as Failed; parent Build is not Aborted)",
		)
	default:
		return false
	}
	return true
}

// backfillInstall copies the runner's structured install-check verdict. A missing result is not evidence of
// installability.
func (c *Controller) backfillInstall(round *reconcileRound, ss *ebsv1.SpecStatus, job *ebsv1.Job) {
	result := job.Status.Install
	if result == nil {
		ss.Install.Status = SpecBuildFailed
		upsertCondition(
			&ss.Install.Conditions,
			ConditionInstall,
			ReasonInstallResultMissing,
			"job "+job.Name+" has no install-check result",
		)
		return
	}
	if result.Status == ebsv1.JobResultSucceeded {
		ss.Install.Status = SpecBuildSucceeded
		ss.Install.MissingDeps = nil
		removeCondition(&ss.Install.Conditions, ConditionInstall)
		return
	}
	if result.Status != ebsv1.JobResultFailed {
		ss.Install.Status = SpecBuildFailed
		upsertCondition(
			&ss.Install.Conditions,
			ConditionInstall,
			ReasonInstallResultInvalid,
			"job "+job.Name+" has invalid install-check result",
		)
		return
	}
	ss.Install.Status = SpecBuildFailed
	ss.Install.MissingDeps = nil
	if len(result.MissingDeps) > 0 {
		ss.Install.MissingDeps = make(map[string]ebsv1.MissingDep, len(result.MissingDeps))
		for name, dep := range result.MissingDeps {
			ss.Install.MissingDeps[name] = dep
		}
	}
	installCondition(ss, job.Name)
}
