// reconcile.go implements the reconcile entry, the front guards and the
// status write helpers (design 7.1, 10.2, 10.3). The business sync returns
// only (ReconcileResult, error); queue operations stay in BaseController
// (7.5).
package buildinfo

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
)

// conflictRequeueDelay is the 10.2 delayed-requeue after a 409 Conflict or
// an unmatched Unknown confirmation: the next round re-GETs the object and
// recomputes the target status; replaying the old intent is forbidden.
const conflictRequeueDelay = time.Second

// reconcileRound carries the per-round shared state (design 10.1/10.2): the
// current BuildInfo object — replaced wholesale after every confirmed write
// — plus the guard-held objects the later steps reuse (15.4: single GET per
// round) and the readiness counters (5.4).
type reconcileRound struct {
	key     string
	current *ebsv1.BuildInfo

	project     *ebsv1.Project
	build       *ebsv1.Build
	rpmRepo     *ebsv1.RpmRepo
	rpmRepoHeld bool

	failures *roundFailures
}

// isSingle reports whether the parent Build is a single-type直通 build
// (7.2.3): the guard-held Build is the only source of the build type.
func (r *reconcileRound) isSingle() bool { return r.build.Spec.BuildType == "single" }

// sync is the BaseController SyncFunc. It guards the 7.5 invalid-result rule
// so a programming error is counted before the framework forgets the item.
func (c *Controller) sync(ctx context.Context, key string) (controller.ReconcileResult, error) {
	result, err := c.reconcile(ctx, key)
	if err == nil && !result.Valid() {
		invalidResults.Inc()
	}
	return result, err
}

// reconcile drives one BuildInfo round (design 7.1).
func (c *Controller) reconcile(ctx context.Context, key string) (controller.ReconcileResult, error) {
	namespace, name, err := splitKey(key)
	if err != nil {
		return controller.ReconcileResult{}, controller.NewPermanentError(err)
	}
	// Entry re-get: the queued object may be stale; the entry GET is the
	// optimistic-lock base for every write this round (7.1/10.2).
	buildInfo, err := c.client.GetBuildInfo(ctx, namespace, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// E-10: deleted externally — silent exit, invalidate caches.
			c.logf(key, "BuildInfoDeleted", "buildinfo removed before reconcile")
			c.invalidateCaches(key)
			return controller.ReconcileResult{}, nil
		}
		return controller.ReconcileResult{}, err
	}
	if buildInfo.DeletionTimestamp != nil {
		// A deleting object is never advanced; its caches, counters and dedup
		// entries are dropped immediately (5.4: no tombstone grace) so a
		// recreated same-name BuildInfo starts from zero.
		c.invalidateCaches(key)
		return controller.ReconcileResult{}, nil
	}
	if buildInfo.Status.Phase.IsTerminal() {
		// G-05 defensive check: terminal objects never advance (5.4 also
		// invalidates the per-BuildInfo caches and counters here).
		c.invalidateCaches(key)
		return controller.ReconcileResult{}, nil
	}

	round := &reconcileRound{key: key, current: buildInfo, failures: c.newRoundFailures(key)}

	if stop, result, err := c.parentAbortGuard(ctx, round); stop {
		return result, err
	}

	// Persisted stop-dispatch marker (E-28/E-29/E-30): the round joins the
	// 6.5 convergence path directly — no Snapshot/RpmRepo/BuildConf reads,
	// no parsing, no graph work (6.5 step 2).
	if marker := stopCondition(round.current.Status.Conditions); marker != nil {
		return c.convergeToCompleted(ctx, round)
	}

	// releaseFailedGuard runs every round for non-single builds; the held
	// RpmRepo object is reused by init/advance (7.1/15.4).
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
		// E-12: unknown phase — skip with an error log, no retry.
		log.Printf("controller=%s key=%q reason=UnknownPhase phase=%q", Name, key, round.current.Status.Phase)
		return controller.ReconcileResult{}, nil
	}
	if err == nil {
		c.logReconciledAfterStart(round.current)
	}
	return result, err
}

// splitKey parses the <namespace>/<name> queue key (design 5.1).
func splitKey(key string) (string, string, error) {
	parts := strings.SplitN(key, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid queue key %q", key)
	}
	return parts[0], parts[1], nil
}

// parentAbortGuard is the front guard of every round (design 7.1): Project
// Terminating (E-20), parent Build missing (E-03) or Aborted (G-06) all
// terminate the BuildInfo as Aborted. The held Project/Build objects are
// reused by the whole round.
func (c *Controller) parentAbortGuard(ctx context.Context, round *reconcileRound) (bool, controller.ReconcileResult, error) {
	project, err := c.client.GetProject(ctx, round.current.Namespace)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// E-21: Project 404 never terminates; re-evaluate next round.
			c.logf(round.key, "ProjectNotFound", "project %s not found", round.current.Namespace)
			return true, controller.ReconcileResult{}, nil
		}
		return true, controller.ReconcileResult{}, err
	}
	round.project = project
	if project.Status.Phase == ebsv1.ProjectTerminating {
		// E-20: project cascade recycling.
		result, err := c.writeAborted(ctx, round, "ProjectTerminating")
		return true, result, err
	}

	build, err := c.client.GetBuild(ctx, round.current.Namespace, round.current.Name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// E-03: a missing parent Build aborts the BuildInfo.
			result, err := c.writeAborted(ctx, round, "BuildDeleted")
			return true, result, err
		}
		// BuildQueryFailed (9.1 no-condition list): transient, error backoff.
		return true, controller.ReconcileResult{}, err
	}
	round.build = build
	if build.Status.Phase == ebsv1.BuildAborted {
		// G-06.
		result, err := c.writeAborted(ctx, round, "BuildAborted")
		return true, result, err
	}
	return false, controller.ReconcileResult{}, nil
}

// writeAborted persists the Aborted terminal phase and invalidates the
// per-BuildInfo caches after the write is confirmed (design 7.1).
func (c *Controller) writeAborted(ctx context.Context, round *reconcileRound, reason string) (controller.ReconcileResult, error) {
	next := round.current.DeepCopy()
	next.Status.Phase = ebsv1.BuildInfoAborted
	result, err := c.writeStatus(ctx, round, next)
	if err != nil || result != (controller.ReconcileResult{}) {
		return result, err
	}
	log.Printf("controller=%s key=%q reason=%s result=Aborted buildinfo_uid=%q", Name, round.key, reason, round.current.UID)
	c.invalidateCaches(round.key)
	return controller.ReconcileResult{}, nil
}

// releaseFailedGuard fetches the same-name RpmRepo once per round (design
// 7.1/15.4): query failures count towards E-29 (whole-round error below the
// threshold), 404 counts but lets the round continue without holding the
// object (E-16), release.phase=Failed escalates to E-28.
func (c *Controller) releaseFailedGuard(ctx context.Context, round *reconcileRound) (bool, controller.ReconcileResult, error) {
	repo, err := c.client.GetRpmRepo(ctx, round.current.Namespace, round.current.Name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// E-16: transient absence — count, escalate at the threshold,
			// otherwise continue the round without holding the RpmRepo.
			_, escalated := round.failures.RpmRepoFailed(ReasonRpmRepoNotFound, fmt.Sprintf("RpmRepo %s/%s not found", round.current.Namespace, round.current.Name))
			if escalated {
				result, err := c.escalateRpmRepoUnavailable(ctx, round)
				return true, result, err
			}
			c.logf(round.key, "RpmRepoNotFound", "rpmrepo %s/%s not found", round.current.Namespace, round.current.Name)
			return false, controller.ReconcileResult{}, nil
		}
		// E-08: 5xx/timeout — count and fail the whole round (unlike E-16).
		_, escalated := round.failures.RpmRepoFailed(ReasonRpmRepoQueryFailed, err.Error())
		if escalated {
			result, err := c.escalateRpmRepoUnavailable(ctx, round)
			return true, result, err
		}
		return true, controller.ReconcileResult{}, err
	}
	round.rpmRepo = repo
	round.rpmRepoHeld = true
	if repo.Status.Release != nil && repo.Status.Release.Phase == ebsv1.RpmRepoReleaseFailed {
		// E-28: stop dispatching, converge to Completed (6.5).
		result, err := c.escalateStop(ctx, round, ConditionReleaseFailed, ReasonRpmRepoReleaseFailed, fmt.Sprintf("RpmRepo %s release failed", repo.Name))
		return true, result, err
	}
	return false, controller.ReconcileResult{}, nil
}

// escalateRpmRepoUnavailable persists the E-29 stop marker with the last
// failure checkpoint of the current streak (E-29 message: object name, last
// error, consecutive failure count) and joins the 6.5 convergence path.
func (c *Controller) escalateRpmRepoUnavailable(ctx context.Context, round *reconcileRound) (controller.ReconcileResult, error) {
	entry := c.counters.Entry(counterRpmRepo, round.key)
	message := fmt.Sprintf("RpmRepo %s/%s unavailable: %s (consecutive failures: %d)", round.current.Namespace, round.current.Name, entry.LastMessage, entry.ConsecutiveFails)
	result, err := c.escalateStop(ctx, round, ConditionRpmRepoUnavailable, entry.LastReason, message)
	if err == nil && result == (controller.ReconcileResult{}) {
		round.failures.RpmRepoReady()
	}
	return result, err
}

// escalateSnapshotUnavailable persists the E-30 stop marker and joins the
// 6.5 convergence path (called from the current-Snapshot read points).
func (c *Controller) escalateSnapshotUnavailable(ctx context.Context, round *reconcileRound) (controller.ReconcileResult, error) {
	entry := c.counters.Entry(counterSnapshot, round.key)
	message := fmt.Sprintf("Snapshot %s/%s unavailable: %s (consecutive failures: %d)", round.current.Namespace, round.current.Name, entry.LastMessage, entry.ConsecutiveFails)
	result, err := c.escalateStop(ctx, round, ConditionSnapshotUnavailable, entry.LastReason, message)
	if err == nil && result == (controller.ReconcileResult{}) {
		round.failures.SnapshotReady()
	}
	return result, err
}

// escalateStop persists a stop-dispatch marker (design 6.5 step 1): the
// condition write must be confirmed before the round joins the convergence
// path; a failed write ends the round and forbids further dispatch.
func (c *Controller) escalateStop(ctx context.Context, round *reconcileRound, condType, reason, message string) (controller.ReconcileResult, error) {
	next := round.current.DeepCopy()
	upsertCondition(&next.Status.Conditions, condType, reason, message)
	result, err := c.writeStatus(ctx, round, next)
	if err != nil || result != (controller.ReconcileResult{}) {
		return result, err
	}
	log.Printf("controller=%s key=%q reason=%s result=StopDispatch condition=%q", Name, round.key, reason, condType)
	return c.convergeToCompleted(ctx, round)
}

// writeStatus persists next.Status following 10.2/10.3 and routes write
// failures per 7.5. On confirmed success round.current is replaced with the
// server-confirmed object and the zero result is returned. The caller passes
// a deep copy of round.current with the intended status mutations.
func (c *Controller) writeStatus(ctx context.Context, round *reconcileRound, next *ebsv1.BuildInfo) (controller.ReconcileResult, error) {
	// 10.3 intent snapshot: the full target status, saved before sending.
	intent := next.DeepCopy()
	updated, err := c.client.UpdateBuildInfoStatus(ctx, next)
	if err == nil {
		c.afterConfirmedWrite(round, updated, intent)
		return controller.ReconcileResult{}, nil
	}
	var writeErr *clientpkg.WriteError
	if !errors.As(err, &writeErr) {
		return controller.ReconcileResult{}, err
	}
	switch writeErr.Outcome {
	case clientpkg.WriteNotSent:
		// The typed client only reports NotSent for local validation
		// (client contract 4.1): retrying cannot fix it.
		return controller.ReconcileResult{}, controller.NewPermanentError(err)
	case clientpkg.WriteRejected:
		switch writeErr.StatusCode {
		case 409:
			// 10.2: delayed requeue, next round recomputes from a fresh GET.
			conflictRequeues.Inc()
			return controller.ReconcileResult{RequeueAfter: conflictRequeueDelay}, nil
		case 404:
			// E-10: the object was deleted externally mid-round.
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
		// WriteUnknown: 10.3 confirmation read, never a replay.
		unknownWrites.Inc()
		return c.confirmStatusWrite(ctx, round, intent)
	}
}

// confirmStatusWrite resolves an Unknown status write (design 10.3): a
// semantic match treats the write as landed (10.2 chaining continues), a
// mismatch requeues for recomputation, 404/UID-mismatch ends the round, and
// a failed confirmation GET follows the read-error classification.
func (c *Controller) confirmStatusWrite(ctx context.Context, round *reconcileRound, intent *ebsv1.BuildInfo) (controller.ReconcileResult, error) {
	if ctx.Err() != nil {
		// 10.3: no background confirmation on a cancelled context.
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
	return controller.ReconcileResult{}, nil
}

// afterConfirmedWrite chains the confirmed object into the round (10.2) and
// counts the state-change metrics only now, so retried rounds never
// double-count (11.2).
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
// semantically (design 10.3): nil maps equal empty maps, conditions match by
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

// specStatusMatches compares specStatus maps per spec entry (10.3).
func specStatusMatches(a, b map[string]ebsv1.SpecStatus) bool {
	if len(a) != len(b) {
		return false
	}
	for spec, left := range a {
		right, ok := b[spec]
		if !ok {
			return false
		}
		if left.Build.Status != right.Build.Status || left.Build.JobName != right.Build.JobName ||
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
// set (10.3): outDep order carries no semantics and matches as a set.
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

// pendingCreatesMatch compares pendingJobCreates per spec entry (10.3): a
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
