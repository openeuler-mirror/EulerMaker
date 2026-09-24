// jobs.go implements the Job lifecycle of the BuildInfo controller (design
// 15.3 / 6.5.1): deterministic naming and identity annotations, the G-08
// label set, Config content resolution, the payload construction
// contract, the register-then-create dispatch pipeline with AlreadyExists /
// Unknown confirmation, and the List backfill (7.4.2 count floor, 7.4.4
// latest pick, 7.4.5 phase mapping, 7.4.7 install backfill).
package buildinfo

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	yaml "gopkg.in/yaml.v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/controllers/buildinfo/rpmver"
	"controller-manager/pkg/controllers/buildinfo/specparse"
	ebsv1 "ebs-api/ebs/v1"
)

// Job field constants (design 15.3.1).
const (
	jobRuntime         = "ct"
	jobTimeoutSeconds  = 10800
	runnerArchSelector = "ebs.io/runner-arch"

	annBuildInfoUID           = "ebs.io/buildinfo-uid"
	annDispatchGeneration     = "ebs.io/dispatch-generation"
	annBuildResourceConfig    = "ebs.io/build-resource-config"
	annBuildResourceConfigGen = "ebs.io/build-resource-config-generation"
)

// jobNameFor derives the deterministic Job name (design 15.3.1): the hash is
// SHA-256 over json.Marshal([buildInfoUID, specName, generation]) rendered as
// 64 lowercase hex chars; the name is specName-generation-hash, untruncated.
func jobNameFor(buildInfoUID, specName string, generation int64) string {
	identity, _ := json.Marshal([]string{buildInfoUID, specName, strconv.FormatInt(generation, 10)})
	sum := sha256.Sum256(identity)
	return fmt.Sprintf("%s-%d-%x", specName, generation, sum)
}

// packageNameLabelValue encodes a package repository name for the
// ebs.io/package-name label (labels.md §7): a name made of label-value
// characters with alphanumeric ends is truncated to 63 chars with trailing
// -_. stripped and used directly, unless it collides with the reserved
// digest form; names with illegal characters or a reserved-form collision
// use sha256- plus the 52-char lowercase Base32 (no padding) SHA-256 digest.
func packageNameLabelValue(name string) string {
	if validLabelValueChars(name) {
		value := name
		if len(value) > 63 {
			value = value[:63]
		}
		value = strings.TrimRight(value, "-_.")
		if value != "" && !reservedDigestForm(value) {
			return value
		}
	}
	sum := sha256.Sum256([]byte(name))
	digest := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
	return "sha256-" + strings.ToLower(digest)
}

// validLabelValueChars reports whether name consists of Kubernetes label
// value characters and starts and ends with an alphanumeric.
func validLabelValueChars(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		ch := name[i]
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.' {
			continue
		}
		return false
	}
	return isAlphanumeric(name[0]) && isAlphanumeric(name[len(name)-1])
}

func isAlphanumeric(ch byte) bool {
	return ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9'
}

// reservedDigestForm reports whether value matches the reserved digest shape
// ^sha256-[a-z2-7]{52}$ (labels.md §7).
func reservedDigestForm(value string) bool {
	const prefix = "sha256-"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+52 {
		return false
	}
	for i := len(prefix); i < len(value); i++ {
		ch := value[i]
		if ch >= 'a' && ch <= 'z' || ch >= '2' && ch <= '7' {
			continue
		}
		return false
	}
	return true
}

// archSupported runs the E-19 exclusiveArch whitelist check; an empty
// whitelist means every arch (normalized at parse time, defensive here).
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

// missingBuildRequires returns the buildRequires entries (buildRemoves
// excluded) not available in the layered sources (design 7.4.1 condition 2),
// sorted and de-duplicated.
func missingBuildRequires(depend *specparse.SpecDepend, sources *rpmver.RpmMetaSources) []string {
	var missing []string
	for _, name := range sortedConstKeys(depend.BuildRequires, depend.BuildRemoves) {
		if !sources.Available(name, depend.BuildRequires[name]) {
			missing = append(missing, name)
		}
	}
	return missing
}

// --- dispatch pipeline (design 15.3.1 / 6.5.1) ---

// dispatchSpec runs the full single-spec dispatch pipeline: pending-entry
// reuse with GET verification (6.5.1 #4) or registration (先登记再请求),
// build-resource Config resolution (E-27), Job construction and CreateJob outcome
// handling (success / AlreadyExists / Unknown / NotSent / Rejected), and the
// confirmed dispatch write-back. The E-19 arch check and the 7.4.1
// dependency verdict run at the caller; image resolution happens once per
// round at the caller (E-26).
func (c *Controller) dispatchSpec(ctx context.Context, round *reconcileRound, specName string, depend *specparse.SpecDepend, snapshot *ebsv1.Snapshot, image, contentURL string) (controller.ReconcileResult, error) {
	namespace := round.current.Namespace
	var generation int64
	var name string
	entryExisted := false
	if pend, ok := round.current.Status.PendingJobCreates[specName]; ok {
		// 6.5.1 #4: registered entries are GET-verified first; a hit confirms
		// (no new generation), a 404 re-creates with the same identity.
		entryExisted = true
		generation, name = pend.DispatchGeneration, pend.JobName
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
		generation = round.current.Status.SpecStatus[specName].DispatchCount + 1
		name = jobNameFor(string(round.current.UID), specName, generation)
		if result, err := c.registerPendingCreate(ctx, round, specName, name, generation); err != nil || result != (controller.ReconcileResult{}) {
			return result, err
		}
	}

	// The cluster-wide default table is the sole source of Job resources.
	resource, err := c.client.GetBuildResourceRules(ctx)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			message := "build-resource Config not found"
			return c.markSpecFailed(ctx, round, specName, ConditionDefaultBuildResourceConfigNotFound, ReasonDefaultBuildResourceConfigNotFound, message, !entryExisted)
		}
		return controller.ReconcileResult{}, err
	}

	job := c.jobForSpec(round, specName, depend, snapshot, image, contentURL, resource, name, generation)
	created, err := c.client.CreateJob(ctx, namespace, job)
	var writeErr *clientpkg.WriteError
	isWriteErr := errors.As(err, &writeErr)
	if !isWriteErr || writeErr.Outcome != clientpkg.WriteNotSent {
		// jobCreates counts create requests actually sent (metrics.go);
		// WriteNotSent failed local validation before any request left the
		// controller, so it is not counted.
		jobCreates.Inc()
	}
	if err == nil {
		return c.confirmDispatchedJob(ctx, round, specName, generation, created)
	}
	if !isWriteErr {
		jobCreateFailures.Inc()
		return controller.ReconcileResult{}, err
	}
	switch writeErr.Outcome {
	case clientpkg.WriteNotSent:
		// Local validation failure (client contract 4.1): the identity never
		// had an in-flight request, so a this-round registration is removed
		// (6.5.1 #3); a reused entry is kept by design.
		jobCreateFailures.Inc()
		return c.failCreateUnsent(ctx, round, specName, entryExisted, err)
	case clientpkg.WriteRejected:
		switch writeErr.StatusCode {
		case 409:
			// AlreadyExists: identity verification takes precedence over the
			// generic conflict requeue (15.3.1); a 404 confirmation read is a
			// retryable error, never the main-object NotFound rule.
			existing, gerr := c.client.GetJob(ctx, namespace, name)
			if gerr != nil {
				jobCreateFailures.Inc()
				return controller.ReconcileResult{}, gerr
			}
			if verr := verifyJobIdentity(existing, round.current, specName, generation); verr != nil {
				c.logf(round.key, "JobIdentityMismatch", "job %s identity mismatch after AlreadyExists: %v", name, verr)
				return controller.ReconcileResult{}, controller.NewPermanentError(verr)
			}
			return c.confirmDispatchedJob(ctx, round, specName, generation, existing)
		case 400, 401, 403, 422:
			// Provably not created: same entry-removal rule as NotSent.
			jobCreateFailures.Inc()
			return c.failCreateUnsent(ctx, round, specName, entryExisted, err)
		default:
			// 404/408/429/5xx and other retryable rejections.
			jobCreateFailures.Inc()
			return controller.ReconcileResult{}, err
		}
	default:
		// WriteUnknown (10.3): confirm by GET on the deterministic name.
		unknownWrites.Inc()
		existing, gerr := c.client.GetJob(ctx, namespace, name)
		if gerr == nil {
			if verr := verifyJobIdentity(existing, round.current, specName, generation); verr != nil {
				c.logf(round.key, "JobIdentityMismatch", "job %s identity mismatch after Unknown: %v", name, verr)
				return controller.ReconcileResult{}, controller.NewPermanentError(verr)
			}
			return c.confirmDispatchedJob(ctx, round, specName, generation, existing)
		}
		jobCreateFailures.Inc()
		if errors.Is(gerr, ErrNotFound) {
			// Unknown with no object: the entry stays registered (6.5.1 #6 —
			// a GET can never prove the request never lands); the error
			// return re-enters with backoff and the next round re-verifies.
			return controller.ReconcileResult{}, err
		}
		return controller.ReconcileResult{}, gerr
	}
}

// failCreateUnsent handles a provably-unsent/uncreated CreateJob outcome
// (6.5.1 #3): a this-round registration is removed before the permanent
// error is returned; a reused entry stays (an earlier request may exist).
func (c *Controller) failCreateUnsent(ctx context.Context, round *reconcileRound, specName string, entryExisted bool, createErr error) (controller.ReconcileResult, error) {
	if !entryExisted {
		if result, err := c.removePendingCreate(ctx, round, specName); err != nil || result != (controller.ReconcileResult{}) {
			return result, err
		}
	}
	return controller.ReconcileResult{}, controller.NewPermanentError(createErr)
}

// registerPendingCreate persists the creation identity before any request is
// sent (6.5.1 #1).
func (c *Controller) registerPendingCreate(ctx context.Context, round *reconcileRound, specName, jobName string, generation int64) (controller.ReconcileResult, error) {
	next := round.current.DeepCopy()
	if next.Status.PendingJobCreates == nil {
		next.Status.PendingJobCreates = map[string]ebsv1.PendingJobCreate{}
	}
	next.Status.PendingJobCreates[specName] = ebsv1.PendingJobCreate{JobName: jobName, DispatchGeneration: generation}
	return c.writeStatus(ctx, round, next)
}

// removePendingCreate drops a registered entry (6.5.1 #3 first-attempt
// proof only).
func (c *Controller) removePendingCreate(ctx context.Context, round *reconcileRound, specName string) (controller.ReconcileResult, error) {
	next := round.current.DeepCopy()
	delete(next.Status.PendingJobCreates, specName)
	return c.writeStatus(ctx, round, next)
}

// confirmDispatchedJob persists the confirmed dispatch in one write (6.5.1
// #2): jobName backfill, dispatchCount raised to the confirmed generation,
// the pending entry removed, and build.status mapped from the confirmed
// Job's phase (7.4.5, prior-generation success unknown at this point).
func (c *Controller) confirmDispatchedJob(ctx context.Context, round *reconcileRound, specName string, generation int64, job *ebsv1.Job) (controller.ReconcileResult, error) {
	next := round.current.DeepCopy()
	if next.Status.SpecStatus == nil {
		next.Status.SpecStatus = map[string]ebsv1.SpecStatus{}
	}
	ss := next.Status.SpecStatus[specName]
	if ss.DispatchCount < generation {
		ss.DispatchCount = generation
	}
	applyJobPhase(&ss, job, false)
	delete(next.Status.PendingJobCreates, specName)
	next.Status.SpecStatus[specName] = ss
	result, err := c.writeStatus(ctx, round, next)
	if err == nil && result == (controller.ReconcileResult{}) {
		dispatches.Inc()
	}
	return result, err
}

// markSpecFailed persists a pre-dispatch Failed verdict (E-19/E-27/
// RpmDependsMissing): build.status=Failed plus the condition, no Job. The
// this-round pending registration is removed when clearPending holds (6.5.1
// #3: the identity provably had no in-flight request). A reused registration
// is GET-verified once: G-03 disables the dispatchSpec re-create self-heal
// for a now-terminal spec, so a lost request (Unknown + 404) would otherwise
// orphan the entry and block the Completed write forever — a listed Job
// keeps the entry (the backfill fold resolves it next round), a 404 drops it
// together with the Failed verdict.
func (c *Controller) markSpecFailed(ctx context.Context, round *reconcileRound, specName, condType, reason, message string, clearPending bool) (controller.ReconcileResult, error) {
	next := round.current.DeepCopy()
	if next.Status.SpecStatus == nil {
		next.Status.SpecStatus = map[string]ebsv1.SpecStatus{}
	}
	ss := next.Status.SpecStatus[specName]
	ss.Build.Status = SpecBuildFailed
	specCondition(&ss, condType, reason, message)
	next.Status.SpecStatus[specName] = ss
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

// verifyJobIdentity checks a found Job against the creation identity
// (15.3.1): namespace, recomputed name, build/spec labels and the identity
// annotations must all match.
func verifyJobIdentity(job *ebsv1.Job, buildInfo *ebsv1.BuildInfo, specName string, generation int64) error {
	if job.Namespace != buildInfo.Namespace {
		return fmt.Errorf("namespace %q != %q", job.Namespace, buildInfo.Namespace)
	}
	if want := jobNameFor(string(buildInfo.UID), specName, generation); job.Name != want {
		return fmt.Errorf("name %q != %q", job.Name, want)
	}
	if job.Labels[ebsv1.JobBuildNameLabel] != buildInfo.Name {
		return fmt.Errorf("build-name label %q != %q", job.Labels[ebsv1.JobBuildNameLabel], buildInfo.Name)
	}
	if job.Labels[ebsv1.JobSpecNameLabel] != specName {
		return fmt.Errorf("spec-name label %q != %q", job.Labels[ebsv1.JobSpecNameLabel], specName)
	}
	if job.Annotations[annBuildInfoUID] != string(buildInfo.UID) {
		return fmt.Errorf("buildinfo-uid annotation %q != %q", job.Annotations[annBuildInfoUID], buildInfo.UID)
	}
	if job.Annotations[annDispatchGeneration] != strconv.FormatInt(generation, 10) {
		return fmt.Errorf("dispatch-generation annotation %q != %d", job.Annotations[annDispatchGeneration], generation)
	}
	return nil
}

// --- Job construction (design 15.3.1) ---

// jobForSpec builds the Job object with every controller-filled field.
func (c *Controller) jobForSpec(round *reconcileRound, specName string, depend *specparse.SpecDepend, snapshot *ebsv1.Snapshot, image, contentURL string, resource *buildResourceRules, name string, generation int64) *ebsv1.Job {
	buildInfo := round.current
	target := round.build.Spec.BuildTarget
	runtimeSpec, _ := json.Marshal(map[string]string{"image": image})
	return &ebsv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: ebsv1.SchemeGroupVersion.String(), Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: buildInfo.Namespace,
			Labels: map[string]string{
				ebsv1.JobBuildNameLabel:    buildInfo.Name,
				ebsv1.JobSpecNameLabel:     specName,
				ebsv1.JobPackageNameLabel:  packageNameLabelValue(depend.RepoName),
				ebsv1.BuildTargetOSLabel:   target.Os,
				ebsv1.BuildTargetArchLabel: target.Arch,
			},
			Annotations: map[string]string{
				annBuildInfoUID:           string(buildInfo.UID),
				annDispatchGeneration:     strconv.FormatInt(generation, 10),
				annBuildResourceConfig:    resource.Name,
				annBuildResourceConfigGen: strconv.FormatInt(resource.Generation, 10),
			},
		},
		Spec: ebsv1.JobSpec{
			Runtime:        jobRuntime,
			RuntimeSpec:    runtime.RawExtension{Raw: runtimeSpec},
			TimeoutSeconds: jobTimeoutSeconds,
			Resources:      resolveResources(resource, specName, target.Arch),
			NodeSelector:   map[string]string{runnerArchSelector: target.Arch},
			Payload:        c.jobPayload(round, specName, depend, snapshot, contentURL),
		},
	}
}

// jobPayload assembles the payload YAML (design 15.3.1 payload 构造契约):
// the BuildInfo.spec.buildPayload base map with the per-spec four keys and
// the build-level Repo/repo_priority keys injected (overriding base keys).
func (c *Controller) jobPayload(round *reconcileRound, specName string, depend *specparse.SpecDepend, snapshot *ebsv1.Snapshot, contentURL string) string {
	base := c.parseBuildPayload(round.key, round.current.Spec.BuildPayload)
	base["spec_name"] = specName
	base["spec_file_name"] = depend.SpecFileName
	if entry, ok := snapshot.Status.PackageRepoStatuses[depend.RepoName]; !ok || entry.CommitID == "" {
		// Cannot happen for assembled specs (15.3.1): never blocks dispatch.
		c.logf(round.key, "SpecRepoEntryMissing", "packageRepoStatuses entry for repo %s missing or without commitId; spec_url/commitId not injected", depend.RepoName)
	} else {
		base["spec_url"] = entry.CloneURL
		base["commitId"] = entry.CommitID
	}
	var bootstrapRepos []string
	for _, repo := range round.current.Spec.BootstrapRepo {
		bootstrapRepos = append(bootstrapRepos, repo.Repo)
	}
	repoValue := joinRepoPayload(contentURL, bootstrapRepos)
	if repoValue == "" {
		// Nothing to inject: keep the base key as-is (15.3.1).
		return c.marshalPayload(round, base)
	}
	base["Repo"] = repoValue
	repoCount := len(strings.Fields(repoValue))
	base["repo_priority"] = c.normalizeRepoPriority(round, base["repo_priority"], repoCount)
	return c.marshalPayload(round, base)
}

// joinRepoPayload joins the RpmRepo contentURL (first when non-empty) with
// the bootstrap repo URLs in declaration order (design 15.3.1 / 7.2.3).
func joinRepoPayload(contentURL string, bootstrapRepos []string) string {
	parts := make([]string, 0, len(bootstrapRepos)+1)
	if contentURL != "" {
		parts = append(parts, contentURL)
	}
	parts = append(parts, bootstrapRepos...)
	return strings.Join(parts, " ")
}

// normalizeRepoPriority renders the priority list aligned with the Repo
// entry count (design 15.3.1): a non-empty base string is the base (padded
// with "10" or truncated with one warning), otherwise all "10".
func (c *Controller) normalizeRepoPriority(round *reconcileRound, baseValue any, repoCount int) string {
	var base []string
	if s, ok := baseValue.(string); ok && strings.TrimSpace(s) != "" {
		base = strings.Fields(s)
	}
	out := make([]string, repoCount)
	copy(out, base)
	if len(base) > repoCount {
		c.logf(round.key, "RepoPriorityTruncated", "repo_priority %d entries truncated to %d", len(base), repoCount)
	}
	if len(base) > 0 && len(base) < repoCount {
		c.logf(round.key, "RepoPriorityPadded", "repo_priority %d entries padded to %d", len(base), repoCount)
	}
	for i := range out {
		if out[i] == "" {
			out[i] = "10"
		}
	}
	return strings.Join(out, " ")
}

func (c *Controller) marshalPayload(round *reconcileRound, base map[string]any) string {
	payload, err := yaml.Marshal(base)
	if err != nil {
		// Defensive: the base is a YAML-decoded map plus string values, so a
		// marshal failure is a programming error; keep a trace.
		c.logf(round.key, "PayloadMarshalFailed", "payload marshal failed: %v", err)
		return ""
	}
	return string(payload)
}

// resolveResources merges the build-resource Config levels (design 15.3.1 /
// data-models~config.md 3.2): spec.default -> packages[spec].default ->
// packages[spec].arches[arch], per-field override; each level's unset limits
// take the same level's requests.
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

// normalizeResourceLevel fills a level's missing limits from its own
// requests (data-models~config.md 3.2).
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

// --- List backfill (design 7.2 step 1 / 7.3 step 2) ---

// groupJobsBySpec groups listed Jobs by their spec-name label; Jobs without
// a label or outside the scope set are skipped (E-05, OrphanJob log).
func (c *Controller) groupJobsBySpec(round *reconcileRound, jobs []ebsv1.Job, scope map[string]bool) map[string][]ebsv1.Job {
	bySpec := map[string][]ebsv1.Job{}
	for i := range jobs {
		job := jobs[i]
		spec := job.Labels[ebsv1.JobSpecNameLabel]
		if spec == "" || !scope[spec] {
			c.logf(round.key, "OrphanJob", "job %s spec-name %q out of scope, skipped", job.Name, spec)
			continue
		}
		bySpec[spec] = append(bySpec[spec], job)
	}
	return bySpec
}

// backfillJobs folds one listed Job batch into next.Status in memory (design
// 7.4.2 count floor, 7.4.4 latest pick, 7.4.5 mapping, 7.4.7 install
// backfill, 6.5.1 #2 pending-entry confirmation). scope is the init build
// set or the Processing specStatus key set (E-05); createMissing controls
// whether scoped specs without an entry get one (init yes — covers Jobs
// created while the status write failed; Processing no, the init step-5
// invariant already covers every build-set spec). Only Jobs carrying this
// incarnation's ebs.io/buildinfo-uid annotation fold: a recreated same-name
// BuildInfo never inherits a previous incarnation's phases or counts
// (15.3.1 identity; the returned groups are uid-filtered for the same
// reason — the 7.4.6 gate inputs must not either).
func (c *Controller) backfillJobs(round *reconcileRound, next *ebsv1.BuildInfo, jobs []ebsv1.Job, scope map[string]bool, createMissing bool) map[string][]ebsv1.Job {
	bySpec := c.groupJobsBySpec(round, jobs, scope)
	uid := string(round.current.UID)
	if next.Status.SpecStatus == nil && createMissing {
		next.Status.SpecStatus = map[string]ebsv1.SpecStatus{}
	}
	for spec, group := range bySpec {
		ss, exists := next.Status.SpecStatus[spec]
		if !exists {
			if !createMissing {
				continue
			}
			ss = ebsv1.SpecStatus{}
		}
		own := filterJobsByUID(group, uid)
		if len(own) < len(group) {
			c.logf(round.key, "ForeignIncarnationJob", "spec %s: %d of %d listed jobs belong to a previous same-name buildinfo; excluded from folding", spec, len(group)-len(own), len(group))
		}
		if len(own) > 0 {
			// 7.4.2 floor: max(DispatchCount, listed generation count, max
			// confirmed identity generation) — old data without the field and
			// cleaned-up old generations never regress the count (15.3.1).
			floor := int64(len(own))
			for i := range own {
				job := &own[i]
				if gen, err := strconv.ParseInt(job.Annotations[annDispatchGeneration], 10, 64); err == nil && gen > floor {
					floor = gen
				}
			}
			if ss.DispatchCount < floor {
				ss.DispatchCount = floor
			}
			latest := latestJob(own)
			if applyJobPhase(&ss, latest, priorSucceeded(own, latest)) {
				if latest.Status.Phase == ebsv1.JobSucceeded {
					c.backfillInstall(round, &ss, latest)
				}
			} else {
				c.logf(round.key, "UnknownJobPhase", "job %s phase %q unknown, status mapping skipped", latest.Name, latest.Status.Phase)
			}
		}
		// 6.5.1 #2: an identity-matched Job in the list confirms the pending
		// entry (removed in the same write, no double counting).
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
		next.Status.SpecStatus[spec] = ss
		bySpec[spec] = own
	}
	return bySpec
}

// filterJobsByUID keeps only the Jobs carrying the given
// ebs.io/buildinfo-uid annotation (15.3.1 incarnation identity).
func filterJobsByUID(jobs []ebsv1.Job, uid string) []ebsv1.Job {
	out := make([]ebsv1.Job, 0, len(jobs))
	for i := range jobs {
		if jobs[i].Annotations[annBuildInfoUID] == uid {
			out = append(out, jobs[i])
		}
	}
	return out
}

// latestJob picks the multi-generation target Job (design 7.4.4): the
// greatest (creationTimestamp, name) pair; a zero timestamp sorts earliest.
// An empty group yields nil — callers dispatching on the result must guard.
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

// priorSucceeded reports whether the group holds a Succeeded Job older than
// latest (7.4.2 best-effort last generation: a failed rebuild with a
// previous Succeeded generation maps to the RebuildFailed condition).
func priorSucceeded(group []ebsv1.Job, latest *ebsv1.Job) bool {
	for i := range group {
		job := &group[i]
		if job.Name != latest.Name && job.Status.Phase == ebsv1.JobSucceeded {
			return true
		}
	}
	return false
}

// applyJobPhase maps the target Job phase onto build.status (design 7.4.5);
// the bool reports a known phase. Pending forces Running (never inherit a
// previous generation's terminal state). The jobName backfill is
// unconditional — an unknown phase skips only the status mapping (7.4.4),
// so the caller's "mapping skipped" log refers to build.status alone.
func applyJobPhase(ss *ebsv1.SpecStatus, job *ebsv1.Job, succeededPrior bool) bool {
	ss.Build.JobName = job.Name
	switch job.Status.Phase {
	case ebsv1.JobPending, ebsv1.JobRunning:
		ss.Build.Status = SpecBuildRunning
	case ebsv1.JobSucceeded:
		ss.Build.Status = SpecBuildSucceeded
	case ebsv1.JobFailed:
		ss.Build.Status = SpecBuildFailed
		if succeededPrior {
			specCondition(ss, ConditionRebuildFailed, ReasonRebuildJobFailed, "job "+job.Name+" rebuild failed")
		} else {
			specCondition(ss, ConditionBuildFailed, ReasonJobFailed, "job "+job.Name+" failed")
		}
	case ebsv1.JobAborted:
		ss.Build.Status = SpecBuildFailed
		specCondition(ss, ConditionBuildAborted, ReasonBuildAborted, "job "+job.Name+" aborted (defensive: treated as Failed; parent Build is not Aborted)")
	default:
		return false
	}
	return true
}

// installMessagePayload mirrors the runner's install-check JSON (7.4.7).
type installMessagePayload struct {
	MissingDeps map[string]installMissingDep `json:"missing_deps"`
}

type installMissingDep struct {
	NeededBy        string            `json:"needed_by"`
	VersionRequests map[string]string `json:"version_requests"`
}

// backfillInstall folds the Succeeded target Job's message into install
// status (design 7.4.7 three branches): empty message or empty missing_deps
// -> Succeeded; non-empty valid missing_deps -> Failed with idempotent
// missingDeps merge and the Install condition; unparseable message -> no
// rewrite.
func (c *Controller) backfillInstall(round *reconcileRound, ss *ebsv1.SpecStatus, job *ebsv1.Job) {
	message := strings.TrimSpace(job.Status.Message)
	if message == "" {
		ss.Install.Status = SpecBuildSucceeded
		return
	}
	var payload installMessagePayload
	if err := json.Unmarshal([]byte(message), &payload); err != nil {
		c.logf(round.key, "InstallMessageParseFailed", "job %s message is not install-check JSON, install status kept: %v", job.Name, err)
		return
	}
	if len(payload.MissingDeps) == 0 {
		ss.Install.Status = SpecBuildSucceeded
		return
	}
	ss.Install.Status = SpecBuildFailed
	if ss.Install.MissingDeps == nil {
		ss.Install.MissingDeps = map[string]ebsv1.MissingDep{}
	}
	names := make([]string, 0, len(payload.MissingDeps))
	for name := range payload.MissingDeps {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, ok := ss.Install.MissingDeps[name]; ok {
			continue // idempotent: existing entries are never overwritten
		}
		dep := payload.MissingDeps[name]
		ss.Install.MissingDeps[name] = ebsv1.MissingDep{
			NeededBy:        dep.NeededBy,
			VersionRequests: normalizeVersionRequests(dep.VersionRequests),
		}
	}
	installCondition(ss, job.Name)
}

// normalizeVersionRequests maps the runner's uppercase operator keys onto
// the VersionConst lowercase fields (design 7.4.7).
func normalizeVersionRequests(requests map[string]string) ebsv1.VersionConst {
	return ebsv1.VersionConst{
		GT: requests["GT"],
		GE: requests["GE"],
		EQ: requests["EQ"],
		LE: requests["LE"],
		LT: requests["LT"],
	}
}
