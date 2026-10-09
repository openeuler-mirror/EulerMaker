package buildinfo

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/controllers/buildinfo/rpmver"
	ebsv1 "ebs-api/ebs/v1"
)

// conflictRequeueDelay is the delayed requeue after a 409 Conflict or
// an unmatched Unknown confirmation: the next round re-GETs the object and
// recomputes the target status; replaying the old intent is forbidden.
const conflictRequeueDelay = time.Second

// reconcileRound carries the per-round shared state: the
// current BuildInfo object — replaced after confirmed writes and updated
// locally for Jobs awaiting batch confirmation — plus the guard-held objects
// later steps reuse and the readiness counters.
type reconcileRound struct {
	key     string
	current *ebsv1.BuildInfo

	project           *ebsv1.Project
	build             *ebsv1.Build
	rpmRepo           *ebsv1.RpmRepo
	rpmRepoHeld       bool
	scriptRef         *ebsv1.ScriptRef
	resourceRules     *buildResourceRules
	jobCreateRequests int
	createdJobs       map[string]createdJob

	failures *roundFailures
}

// isSingle reports whether the parent Build is a single build. The guard-held
// Build is the only source of the build type.
func (r *reconcileRound) isSingle() bool { return r.build.Spec.BuildType == "single" }

// sync is the BaseController SyncFunc. It guards against invalid results
// so a programming error is counted before the framework forgets the item.
func (c *Controller) sync(ctx context.Context, key string) (controller.ReconcileResult, error) {
	result, err := c.reconcile(ctx, key)
	if err == nil && !result.Valid() {
		invalidResults.Inc()
	}
	return result, err
}

// reconcile drives one BuildInfo round.
func (c *Controller) reconcile(ctx context.Context, key string) (controller.ReconcileResult, error) {
	namespace, name, err := splitKey(key)
	if err != nil {
		return controller.ReconcileResult{}, controller.NewPermanentError(err)
	}
	// Entry re-get: the queued object may be stale; the entry GET is the
	// optimistic-lock base for every write this round.
	buildInfo, err := c.client.GetBuildInfo(ctx, namespace, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// A deleted object needs no further reconciliation.
			c.logf(key, "BuildInfoDeleted", "buildinfo removed before reconcile")
			c.invalidateCaches(key)
			return controller.ReconcileResult{}, nil
		}
		return controller.ReconcileResult{}, err
	}
	if buildInfo.DeletionTimestamp != nil {
		// A deleting object is never advanced; its caches, counters and dedup
		// entries are dropped immediately so a
		// recreated same-name BuildInfo starts from zero.
		c.invalidateCaches(key)
		return controller.ReconcileResult{}, nil
	}
	if buildInfo.Status.Phase.IsTerminal() {
		// Terminal objects never advance; release their caches and counters.
		c.invalidateCaches(key)
		return controller.ReconcileResult{}, nil
	}

	round := &reconcileRound{key: key, current: buildInfo, failures: c.newRoundFailures(key)}

	if stop, result, err := c.parentAbortGuard(ctx, round); stop {
		return result, err
	}

	// A persisted stop marker goes directly to convergence without reading
	// child resources, parsing specs, or working on the dependency graph.
	if marker := stopCondition(round.current.Status.Conditions); marker != nil {
		return c.convergeToCompleted(ctx, round)
	}

	// releaseFailedGuard runs every round for non-single builds; the held
	// RpmRepo object is reused by initialization and advancement.
	if !round.isSingle() {
		if stop, result, err := c.releaseFailedGuard(ctx, round); stop {
			return result, err
		}
	}

	var result controller.ReconcileResult
	switch round.current.Status.Phase {
	case ebsv1.BuildInfoPending:
		result, err = c.initBuildInfo(ctx, round)
	case ebsv1.BuildInfoProcessing:
		result, err = c.advanceBuildInfo(ctx, round)
	default:
		// An unknown phase is logged but not retried.
		log.Printf("controller=%s key=%q reason=UnknownPhase phase=%q", Name, key, round.current.Status.Phase)
		return controller.ReconcileResult{}, nil
	}
	if len(round.createdJobs) > 0 && err == nil && (result == (controller.ReconcileResult{}) || result.Requeue) && ctx.Err() == nil {
		flushResult, flushErr := c.flushCreatedJobs(ctx, round)
		if flushErr != nil || flushResult != (controller.ReconcileResult{}) {
			return flushResult, flushErr
		}
	}
	if err == nil {
		c.logReconciledAfterStart(round.current)
	}
	return result, err
}

// splitKey parses the <namespace>/<name> queue key.
func splitKey(key string) (string, string, error) {
	parts := strings.SplitN(key, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid queue key %q", key)
	}
	return parts[0], parts[1], nil
}

// parentAbortGuard runs before business reconciliation: Project Terminating,
// parent Build missing or Aborted all
// terminate the BuildInfo as Aborted. The held Project/Build objects are
// reused by the whole round.
func (c *Controller) parentAbortGuard(ctx context.Context, round *reconcileRound) (bool, controller.ReconcileResult, error) {
	project, err := c.client.GetProject(ctx, round.current.Namespace)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// A missing Project is retried rather than aborting BuildInfo.
			c.logf(round.key, "ProjectNotFound", "project %s not found", round.current.Namespace)
			return true, controller.ReconcileResult{}, nil
		}
		return true, controller.ReconcileResult{}, err
	}
	round.project = project
	if project.Status.Phase == ebsv1.ProjectTerminating {
		// Project termination cascades to BuildInfo.
		result, err := c.writeAborted(ctx, round, "ProjectTerminating")
		return true, result, err
	}

	build, err := c.client.GetBuild(ctx, round.current.Namespace, round.current.Name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// A missing parent Build aborts BuildInfo.
			result, err := c.writeAborted(ctx, round, "BuildDeleted")
			return true, result, err
		}
		// A Build query failure is retried.
		return true, controller.ReconcileResult{}, err
	}
	round.build = build
	if build.Status.Phase == ebsv1.BuildAborted {
		// Follow the parent Build abort.
		result, err := c.writeAborted(ctx, round, "BuildAborted")
		return true, result, err
	}
	return false, controller.ReconcileResult{}, nil
}

// writeAborted persists the Aborted terminal phase and invalidates the
// per-BuildInfo caches after the write is confirmed.
func (c *Controller) writeAborted(ctx context.Context, round *reconcileRound, reason string) (controller.ReconcileResult, error) {
	next := round.current.DeepCopy()
	removeCondition(&next.Status.Conditions, ConditionRpmRepoRetrying)
	if reason == "BuildAborted" {
		message, more := c.abortBuildJobs(ctx, round)
		if err := ctx.Err(); err != nil {
			return controller.ReconcileResult{}, err
		}
		if more {
			return controller.ReconcileResult{RequeueAfter: time.Second}, nil
		}
		if message != "" {
			upsertCondition(&next.Status.Conditions, ConditionJobAbortFailed, ReasonJobAbortFailed, message)
		} else {
			removeCondition(&next.Status.Conditions, ConditionJobAbortFailed)
		}
	}
	next.Status.Phase = ebsv1.BuildInfoAborted
	result, err := c.writeStatus(ctx, round, next)
	if err != nil || result != (controller.ReconcileResult{}) {
		return result, err
	}
	log.Printf("controller=%s key=%q reason=%s result=Aborted buildinfo_uid=%q", Name, round.key, reason, round.current.UID)
	c.invalidateCaches(round.key)
	return controller.ReconcileResult{}, nil
}

const jobAbortBatchSize = 100

// Progress is local to a BuildInfo UID. Restarting may retry previously failed
// requests; UID-guarded abort is idempotent. The queue serializes each key.
type jobAbortProgress struct {
	uid       string
	attempted map[string]bool
	failed    int
	examples  []string
}

// abortBuildJobs attempts at most 100 requests per round. Failed requests are
// recorded and skipped in later batches so they cannot starve remaining Jobs.
func (c *Controller) abortBuildJobs(ctx context.Context, round *reconcileRound) (string, bool) {
	progress, ok := c.abortJobs.Get(round.key)
	if !ok || progress.uid != string(round.current.UID) {
		progress = &jobAbortProgress{uid: string(round.current.UID), attempted: make(map[string]bool)}
		c.abortJobs.Set(round.key, progress)
	}
	jobs, err := c.client.ListJobs(ctx, round.current.Namespace, labels.SelectorFromSet(labels.Set{ebsv1.JobBuildNameLabel: round.current.Name}))
	if err != nil {
		c.logf(round.key, ReasonJobAbortFailed, "list jobs for abort: %v", err)
		return fmt.Sprintf("list jobs for abort failed: %v; prior abort failures: %d", err, progress.failed), false
	}
	slices.SortFunc(jobs, func(a, b ebsv1.Job) int { return strings.Compare(a.Name, b.Name) })
	attempts := 0
	for _, job := range jobs {
		if ctx.Err() != nil {
			break
		}
		identity := job.Name + "/" + string(job.UID)
		if job.Status.Phase.IsTerminal() || progress.attempted[identity] {
			continue
		}
		if attempts == jobAbortBatchSize {
			return "", true
		}
		attempts++
		_, err := c.client.AbortJob(ctx, job.Namespace, job.Name, job.UID, "parent Build aborted")
		if ctx.Err() != nil {
			break
		}
		progress.attempted[identity] = true
		if err == nil || apierrors.IsNotFound(err) || errors.Is(err, ErrNotFound) {
			continue
		}
		progress.failed++
		c.logf(round.key, ReasonJobAbortFailed, "job=%s abort failed or result unknown: %v", job.Name, err)
		if len(progress.examples) < 3 {
			progress.examples = append(progress.examples, fmt.Sprintf("%s: %v", job.Name, err))
		}
	}
	if progress.failed > 0 {
		return fmt.Sprintf("%d Job abort requests failed or remain unconfirmed; %s", progress.failed, strings.Join(progress.examples, "; ")), false
	}
	return "", false
}

// releaseFailedGuard fetches the same-name RpmRepo once per round. Temporary
// absence and read failures retry; deterministic request failures stop dispatch.
func (c *Controller) releaseFailedGuard(ctx context.Context, round *reconcileRound) (bool, controller.ReconcileResult, error) {
	repo, err := c.client.GetRpmRepo(ctx, round.current.Namespace, round.current.Name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// A missing RpmRepo may still be created by the Build controller.
			round.failures.RpmRepoReady()
			result, writeErr := c.recordRpmRepoRetrying(ctx, round, ReasonRpmRepoNotFound, fmt.Sprintf("RpmRepo %s/%s not found", round.current.Namespace, round.current.Name))
			if writeErr != nil || result != (controller.ReconcileResult{}) {
				return true, result, writeErr
			}
			c.logf(round.key, "RpmRepoNotFound", "rpmrepo %s/%s not found", round.current.Namespace, round.current.Name)
			return false, controller.ReconcileResult{}, nil
		}
		if deterministicRpmRepoError(err) {
			result, err := c.escalateStop(ctx, round, ConditionRpmRepoUnavailable, ReasonRpmRepoQueryRejected, fmt.Sprintf("RpmRepo %s/%s query rejected: %v", round.current.Namespace, round.current.Name, err))
			return true, result, err
		}
		round.failures.RpmRepoReady()
		result, writeErr := c.recordRpmRepoRetrying(ctx, round, ReasonRpmRepoQueryFailed, err.Error())
		if writeErr != nil || result != (controller.ReconcileResult{}) {
			return true, result, writeErr
		}
		return true, controller.ReconcileResult{}, err
	}
	round.rpmRepo = repo
	round.rpmRepoHeld = true
	if repo.Status.Release != nil && repo.Status.Release.Phase == ebsv1.RpmRepoReleaseFailed {
		// A failed release stops dispatch and converges to Completed.
		result, err := c.escalateStop(ctx, round, ConditionReleaseUnavailable, ReasonRpmRepoReleaseFailed, fmt.Sprintf("RpmRepo %s release failed", repo.Name))
		return true, result, err
	}
	return false, controller.ReconcileResult{}, nil
}

// deterministicRpmRepoError identifies failures that another reconcile cannot
// repair without changing the request, permissions, or repository configuration.
func deterministicRpmRepoError(err error) bool {
	var contract contractError
	if errors.As(err, &contract) {
		return true
	}
	var source *rpmver.SourceError
	if errors.As(err, &source) && source.Kind == rpmver.FailureConfig {
		return true
	}
	var apiStatus apierrors.APIStatus
	if errors.As(err, &apiStatus) && deterministicHTTPStatus(int(apiStatus.Status().Code)) {
		return true
	}
	var httpStatus *rpmver.HTTPStatusError
	return errors.As(err, &httpStatus) && deterministicHTTPStatus(httpStatus.StatusCode)
}

func deterministicHTTPStatus(code int) bool {
	return code >= http.StatusBadRequest && code < http.StatusInternalServerError &&
		code != http.StatusNotFound && code != http.StatusRequestTimeout && code != http.StatusTooManyRequests
}

// recordRpmRepoRetrying preserves the first message for an unchanged reason,
// avoiding a status write on every retry while keeping the current diagnosis.
func (c *Controller) recordRpmRepoRetrying(ctx context.Context, round *reconcileRound, reason, message string) (controller.ReconcileResult, error) {
	if existing := findCondition(round.current.Status.Conditions, ConditionRpmRepoRetrying); existing != nil && existing.Status == metav1.ConditionTrue && existing.Reason == reason {
		return controller.ReconcileResult{}, nil
	}
	next := round.current.DeepCopy()
	upsertCondition(&next.Status.Conditions, ConditionRpmRepoRetrying, reason, message)
	return c.writeStatusIfChanged(ctx, round, next)
}

func (c *Controller) clearRpmRepoRetrying(ctx context.Context, round *reconcileRound) (controller.ReconcileResult, error) {
	if findCondition(round.current.Status.Conditions, ConditionRpmRepoRetrying) == nil {
		return controller.ReconcileResult{}, nil
	}
	next := round.current.DeepCopy()
	removeCondition(&next.Status.Conditions, ConditionRpmRepoRetrying)
	return c.writeStatusIfChanged(ctx, round, next)
}

// escalateRpmRepoUnavailable persists a stop marker with the last error and
// consecutive failure count, then joins stop-dispatch convergence.
func (c *Controller) escalateRpmRepoUnavailable(ctx context.Context, round *reconcileRound) (controller.ReconcileResult, error) {
	entry := c.counters.Entry(counterRpmRepo, round.key)
	message := fmt.Sprintf("RpmRepo %s/%s unavailable: %s (consecutive failures: %d)", round.current.Namespace, round.current.Name, entry.LastMessage, entry.ConsecutiveFails)
	result, err := c.escalateStop(ctx, round, ConditionRpmRepoUnavailable, entry.LastReason, message)
	if err == nil && result == (controller.ReconcileResult{}) {
		round.failures.RpmRepoReady()
	}
	return result, err
}

// escalateSnapshotUnavailable persists a stop marker and joins convergence.
func (c *Controller) escalateSnapshotUnavailable(ctx context.Context, round *reconcileRound) (controller.ReconcileResult, error) {
	entry := c.counters.Entry(counterSnapshot, round.key)
	message := fmt.Sprintf("Snapshot %s/%s unavailable: %s (consecutive failures: %d)", round.current.Namespace, round.current.Name, entry.LastMessage, entry.ConsecutiveFails)
	result, err := c.escalateStop(ctx, round, ConditionSnapshotUnavailable, entry.LastReason, message)
	if err == nil && result == (controller.ReconcileResult{}) {
		round.failures.SnapshotReady()
	}
	return result, err
}

// escalateStop persists a stop-dispatch marker: the
// condition write must be confirmed before the round joins the convergence
// path; a failed write ends the round and forbids further dispatch.
func (c *Controller) escalateStop(ctx context.Context, round *reconcileRound, condType, reason, message string) (controller.ReconcileResult, error) {
	next := round.current.DeepCopy()
	removeCondition(&next.Status.Conditions, ConditionRpmRepoRetrying)
	upsertCondition(&next.Status.Conditions, condType, reason, message)
	result, err := c.writeStatus(ctx, round, next)
	if err != nil || result != (controller.ReconcileResult{}) {
		return result, err
	}
	log.Printf("controller=%s key=%q reason=%s result=StopDispatch condition=%q", Name, round.key, reason, condType)
	return c.convergeToCompleted(ctx, round)
}

// writeStatus persists next.Status and classifies write failures. After a
// confirmed write, it replaces round.current with the server-returned object.
// The caller passes a deep copy of round.current with the intended changes.
func (c *Controller) writeStatus(ctx context.Context, round *reconcileRound, next *ebsv1.BuildInfo) (controller.ReconcileResult, error) {
	if len(round.createdJobs) > 0 {
		applyCreatedJobs(next, round.createdJobs)
	}
	// Save the full target status before sending for unknown-result confirmation.
	intent := next.DeepCopy()
	updated, err := c.client.UpdateBuildInfoStatus(ctx, next)
	if err == nil {
		c.afterConfirmedWrite(round, updated, intent)
		c.confirmCreatedJobs(round)
		return controller.ReconcileResult{}, nil
	}
	var writeErr *clientpkg.WriteError
	if !errors.As(err, &writeErr) {
		return controller.ReconcileResult{}, err
	}
	switch writeErr.Outcome {
	case clientpkg.WriteNotSent:
		// NotSent means local validation failed; retrying cannot fix it.
		return controller.ReconcileResult{}, controller.NewPermanentError(err)
	case clientpkg.WriteRejected:
		switch writeErr.StatusCode {
		case 409:
			// Recompute from a fresh GET rather than replaying the old intent.
			conflictRequeues.Inc()
			return controller.ReconcileResult{RequeueAfter: conflictRequeueDelay}, nil
		case 404:
			// The object was deleted externally mid-round.
			c.logf(round.key, "BuildInfoDeleted", "status write rejected 404")
			c.invalidateCaches(round.key)
			return controller.ReconcileResult{}, nil
		case 400, 401, 403, 422:
			return controller.ReconcileResult{}, controller.NewPermanentError(err)
		default:
			// 408/429/5xx and other retryable responses; a Retry-After on
			// 429/503 is honored by the framework (clientpkg.RetryAfter).
			return controller.ReconcileResult{}, err
		}
	default:
		// Unknown result: confirm by reading, never replay the write.
		unknownWrites.Inc()
		return c.confirmStatusWrite(ctx, round, intent)
	}
}

// confirmStatusWrite resolves an Unknown write. A semantic match confirms it;
// a mismatch requeues for recomputation, a missing or replaced object ends the
// round, and a failed GET follows the read-error classification.
func (c *Controller) confirmStatusWrite(ctx context.Context, round *reconcileRound, intent *ebsv1.BuildInfo) (controller.ReconcileResult, error) {
	if ctx.Err() != nil {
		// Never confirm in the background after context cancellation.
		return controller.ReconcileResult{}, ctx.Err()
	}
	persisted, err := c.client.GetBuildInfo(ctx, round.current.Namespace, round.current.Name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			c.logf(round.key, "BuildInfoDeleted", "confirmation read found no object")
			c.invalidateCaches(round.key)
			return controller.ReconcileResult{}, nil
		}
		return controller.ReconcileResult{}, err
	}
	if persisted.UID != round.current.UID {
		c.logf(round.key, "BuildInfoRecreated", "confirmation read found a different uid")
		c.invalidateCaches(round.key)
		return controller.ReconcileResult{}, nil
	}
	if !statusMatchesIntent(&persisted.Status, &intent.Status) {
		return controller.ReconcileResult{RequeueAfter: conflictRequeueDelay}, nil
	}
	c.afterConfirmedWrite(round, persisted, intent)
	c.confirmCreatedJobs(round)
	return controller.ReconcileResult{}, nil
}

// afterConfirmedWrite chains the confirmed object into the round and counts
// state-change metrics only after confirmation to avoid double-counting.
func (c *Controller) afterConfirmedWrite(round *reconcileRound, confirmed *ebsv1.BuildInfo, intent *ebsv1.BuildInfo) {
	if round.current.Status.Phase != confirmed.Status.Phase {
		phaseTransitions.Inc()
	}
	if !conditionsMatch(round.current.Status.Conditions, confirmed.Status.Conditions) {
		conditionsUpserts.Inc()
	}
	round.current = confirmed
}

// statusMatchesIntent compares a persisted status with the write intent
// semantically: nil maps equal empty maps, conditions match by
// type without order or lastTransitionTime, specStatus matches per spec
// field-by-field, the DCG matches by node and edge set, pendingJobCreates
// matches per spec entry.
func statusMatchesIntent(persisted, intent *ebsv1.BuildInfoStatus) bool {
	if persisted.Phase != intent.Phase {
		return false
	}
	if !slices.Equal(persisted.FailedPackages, intent.FailedPackages) {
		return false
	}
	if !conditionsMatch(persisted.Conditions, intent.Conditions) {
		return false
	}
	if !specStatusMatches(persisted.SpecStatus, intent.SpecStatus) {
		return false
	}
	if !maps.Equal(persisted.SpecRepoNames, intent.SpecRepoNames) {
		return false
	}
	if !dcgStateMatches(persisted.Dcg, intent.Dcg) {
		return false
	}
	return pendingCreatesMatch(persisted.PendingJobCreates, intent.PendingJobCreates)
}

// conditionsMatch compares two condition lists by type: status, reason and
// message must match; order and lastTransitionTime are ignored.
func conditionsMatch(a, b []metav1.Condition) bool {
	if len(a) != len(b) {
		return false
	}
	for _, left := range a {
		right := findCondition(b, left.Type)
		if right == nil || right.Status != left.Status || right.Reason != left.Reason || right.Message != left.Message {
			return false
		}
	}
	return true
}

// specStatusMatches compares specStatus maps per spec entry.
func specStatusMatches(a, b ebsv1.SpecStatusGroup) bool {
	if a.Len() != b.Len() {
		return false
	}
	for spec, left := range a.Entries() {
		right, ok := b.Lookup(spec)
		if !ok {
			return false
		}
		if left.Build.Status != right.Build.Status ||
			left.Install.Status != right.Install.Status || left.DispatchCount != right.DispatchCount {
			return false
		}
		if !missingDepsMatch(left.Install.MissingDeps, right.Install.MissingDeps) {
			return false
		}
		if !conditionsMatch(left.Build.Conditions, right.Build.Conditions) || !conditionsMatch(left.Install.Conditions, right.Install.Conditions) {
			return false
		}
	}
	return true
}

func missingDepsMatch(a, b map[string]ebsv1.MissingDep) bool {
	if len(a) != len(b) {
		return false
	}
	for name, left := range a {
		right, ok := b[name]
		if !ok || left != right {
			return false
		}
	}
	return true
}

// dcgStateMatches compares two persisted DCG states by node set and edge
// set: outDep order carries no semantics and matches as a set.
func dcgStateMatches(a, b map[string]ebsv1.DcgNodeState) bool {
	if len(a) != len(b) {
		return false
	}
	for node, left := range a {
		right, ok := b[node]
		if !ok {
			return false
		}
		if left.Version != right.Version || left.BootstrapBreak != right.BootstrapBreak {
			return false
		}
		if !stringSetEqual(left.OutDep, right.OutDep) {
			return false
		}
		if !versionConstMapEqual(left.InDep, right.InDep) || !versionConstMapEqual(left.InstallInDep, right.InstallInDep) {
			return false
		}
	}
	return true
}

func stringSetEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := map[string]int{}
	for _, item := range a {
		counts[item]++
	}
	for _, item := range b {
		counts[item]--
		if counts[item] < 0 {
			return false
		}
	}
	return true
}

func versionConstMapEqual(a, b map[string]ebsv1.VersionConst) bool {
	if len(a) != len(b) {
		return false
	}
	for key, left := range a {
		right, ok := b[key]
		if !ok || left != right {
			return false
		}
	}
	return true
}

// pendingCreatesMatch compares pendingJobCreates per spec entry: a
// registration intent requires the entry, a removal intent requires it gone.
func pendingCreatesMatch(a, b map[string]ebsv1.PendingJobCreate) bool {
	if len(a) != len(b) {
		return false
	}
	for spec, left := range a {
		right, ok := b[spec]
		if !ok || left != right {
			return false
		}
	}
	return true
}
