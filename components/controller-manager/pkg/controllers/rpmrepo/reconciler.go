package rpmrepo

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
)

// conflictRequeueDelay is used when the round must be recomputed from a fresh object.
const conflictRequeueDelay = time.Second

// reconciler carries the state of one queue key round.
type reconciler struct {
	controller *Controller
	ctx        context.Context
	key        string
	project    string
	now        metav1.Time
	nowTime    time.Time

	// sameRoundAttempts bounds outbound replays and activations inside one round so a misbehaving Artifact
	// Manager cannot turn a single reconcile into an unbounded request loop.
	sameRoundAttempts int
}

// takeSameRoundAttempt reserves the single same-round replay or activation this round allows.
func (r *reconciler) takeSameRoundAttempt() bool {
	if r.sameRoundAttempts <= 0 {
		return false
	}
	r.sameRoundAttempts--
	return true
}

func (c *Controller) sync(ctx context.Context, key string) (controller.ReconcileResult, error) {
	kind, project, rest, ok := splitKey(key)
	if !ok {
		return controller.ReconcileResult{}, controller.NewPermanentError(fmt.Errorf("invalid RpmRepo key %q", key))
	}
	now := c.clock.Now().UTC()
	r := &reconciler{controller: c, ctx: ctx, key: key, project: project, now: metav1.NewTime(now), nowTime: now, sameRoundAttempts: 1}
	var result controller.ReconcileResult
	var err error
	switch kind {
	case buildKeyPrefix:
		result, err = r.reconcileBuild(rest[0])
	default:
		result, err = r.reconcileRelease(rest[0], rest[1])
	}
	return c.plan(result, err)
}

// reconcileBuild drives one RpmRepo: repository materialization, then the release trigger.
func (r *reconciler) reconcileBuild(name string) (controller.ReconcileResult, error) {
	repo, err := r.controller.client.GetRpmRepo(r.ctx, r.project, name)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return controller.ReconcileResult{}, classifyReadError(err)
		}
		// The RpmRepo is missing: only the Build decides whether this key converges or keeps retrying.
		if _, buildErr := r.controller.client.GetBuild(r.ctx, r.project, name); buildErr != nil {
			if apierrors.IsNotFound(buildErr) {
				return controller.ReconcileResult{}, nil
			}
			return controller.ReconcileResult{}, classifyReadError(buildErr)
		}
		return controller.ReconcileResult{}, fmt.Errorf("RpmRepo %s/%s does not exist yet", r.project, name)
	}
	if repo.DeletionTimestamp != nil || releaseTerminal(repo) {
		return controller.ReconcileResult{}, nil
	}
	build, err := r.controller.client.GetBuild(r.ctx, r.project, name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			buildMissing.Inc()
			log.Printf("controller=%s key=%q uid=%q reason=BuildMissing", Name, r.key, repo.UID)
			return controller.ReconcileResult{}, nil
		}
		return controller.ReconcileResult{}, classifyReadError(err)
	}
	if build.Status.Phase == ebsv1.BuildAborted {
		return r.collectBuildAborted(repo)
	}
	// The release key only needs the build target. Enqueue it before any other dependency is read so a failing
	// BuildInfo read cannot stall an in-flight release.
	if repo.Status.Release != nil {
		r.controller.Enqueue(releaseKey(r.project, build.Spec.BuildTarget.Os, build.Spec.BuildTarget.Arch))
		return controller.ReconcileResult{}, nil
	}
	info, err := r.controller.client.GetBuildInfo(r.ctx, r.project, name)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return controller.ReconcileResult{}, classifyReadError(err)
		}
		log.Printf("controller=%s key=%q uid=%q reason=BuildInfoNotReady", Name, r.key, repo.UID)
		info = nil
	}
	return r.advanceRepository(repo, build, info)
}

// commitStatus performs the single CAS write of a round. A nil result with a nil error means "stop this round
// without an error" (object gone, UID changed, or the decision was abandoned).
func (r *reconciler) commitStatus(target *ebsv1.RpmRepo, satisfied func(*ebsv1.RpmRepo) bool, kind string) (*ebsv1.RpmRepo, error) {
	if err := r.ctx.Err(); err != nil {
		return nil, err
	}
	request := target.DeepCopy()
	updated, err := r.controller.client.UpdateRpmRepoStatus(r.ctx, request)
	if err == nil {
		log.Printf("controller=%s key=%q uid=%q resourceVersion=%q kind=%s result=StatusWritten", Name, r.key, updated.UID, updated.ResourceVersion, kind)
		return updated, nil
	}
	return r.confirmUnknownWrite(err, target, satisfied, kind)
}

func (r *reconciler) confirmUnknownWrite(err error, target *ebsv1.RpmRepo, satisfied func(*ebsv1.RpmRepo) bool, kind string) (*ebsv1.RpmRepo, error) {
	var writeErr *clientpkg.WriteError
	if !errors.As(err, &writeErr) {
		return nil, classifyReadError(err)
	}
	switch writeErr.Outcome {
	case clientpkg.WriteUnknown:
		statusUpdateUnknowns.Inc()
		latest, getErr := r.controller.client.GetRpmRepo(r.ctx, r.project, target.Name)
		if getErr != nil {
			if apierrors.IsNotFound(getErr) {
				return nil, nil
			}
			return nil, classifyReadError(getErr)
		}
		if latest.UID != target.UID {
			return nil, nil
		}
		if satisfied(latest) {
			log.Printf("controller=%s key=%q uid=%q kind=%s result=StatusWriteConfirmed", Name, r.key, latest.UID, kind)
			return latest, nil
		}
		if releaseTerminal(latest) || latest.DeletionTimestamp != nil {
			return nil, nil
		}
		// The write outcome is unknown, not rejected by optimistic concurrency: only the unknown counter moves.
		log.Printf("controller=%s key=%q uid=%q kind=%s result=StatusWriteUnconfirmed", Name, r.key, latest.UID, kind)
		return nil, errRequeueAfter(controller.ReconcileResult{RequeueAfter: conflictRequeueDelay})
	case clientpkg.WriteRejected:
		switch writeErr.StatusCode {
		case 404:
			return nil, nil
		case 409, 412:
			statusUpdateConflicts.Inc()
			log.Printf("controller=%s key=%q uid=%q kind=%s result=StatusConflict", Name, r.key, target.UID, kind)
			return nil, errRequeueAfter(controller.ReconcileResult{RequeueAfter: conflictRequeueDelay})
		case 408, 429:
			return nil, err
		}
		if writeErr.StatusCode >= 500 && writeErr.StatusCode < 600 {
			return nil, err
		}
		return nil, controller.NewPermanentError(err)
	case clientpkg.WriteNotSent:
		return nil, classifyNotSentWrite(err)
	default:
		return nil, err
	}
}

// requeueAfterError carries a required delay out of a helper without failing the round.
type requeueAfterError struct {
	result controller.ReconcileResult
}

func (e requeueAfterError) Error() string { return "requeue after delay" }

func errRequeueAfter(result controller.ReconcileResult) error {
	return requeueAfterError{result: result}
}

// plan turns the reconciler state into the queue result and error pair returned to the framework.
func (c *Controller) plan(result controller.ReconcileResult, err error) (controller.ReconcileResult, error) {
	var requeue requeueAfterError
	if errors.As(err, &requeue) {
		return requeue.result, nil
	}
	return result, err
}

// classifyReadError keeps API permanent rejections and client contract violations permanent while leaving
// NotFound to the call site and everything else retryable.
func classifyReadError(err error) error {
	if apierrors.IsUnauthorized(err) || apierrors.IsForbidden(err) || apierrors.IsBadRequest(err) || apierrors.IsInvalid(err) {
		return controller.NewPermanentError(err)
	}
	var contract contractError
	if errors.As(err, &contract) {
		return controller.NewPermanentError(err)
	}
	return err
}

// classifyArtifactReadError maps a retryable Artifact Manager read failure onto the queue contract.
func classifyArtifactReadError(err error) error {
	if delay := artifactRetryAfter(err); delay > 0 {
		return errRequeueAfter(controller.ReconcileResult{RequeueAfter: delay})
	}
	return err
}

// classifyNotSentWrite keeps client side validation permanent while transient network failures retry.
func classifyNotSentWrite(err error) error {
	var networkError net.Error
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) {
		return err
	}
	return controller.NewPermanentError(err)
}

// backoffDelay computes d = min(initial * 2^(attempt-1), max) with the configured jitter.
func (r *reconciler) backoffDelay(attempt int) time.Duration {
	initial := r.controller.config.Backoff.Initial
	maximum := r.controller.config.Backoff.Max
	jitter := r.controller.config.Backoff.Jitter
	if attempt < 1 {
		attempt = 1
	}
	delay := initial
	for i := 1; i < attempt; i++ {
		if delay >= maximum/2 {
			delay = maximum
			break
		}
		delay *= 2
	}
	if delay > maximum {
		delay = maximum
	}
	if jitter > 0 {
		delay = time.Duration(float64(delay) * (1 - jitter + rand.Float64()*2*jitter))
	}
	return delay
}

// windowRemaining reports how much of the backoff window anchored on updatedAt is still open.
func windowRemaining(updatedAt time.Time, delay time.Duration, now time.Time) time.Duration {
	if updatedAt.IsZero() {
		return 0
	}
	remaining := updatedAt.Add(delay).Sub(now)
	if remaining < 0 {
		return 0
	}
	return remaining
}

func pollAfter(seconds int) time.Duration {
	if seconds <= 0 {
		return defaultPollAfterSeconds
	}
	return time.Duration(seconds) * time.Second
}

func (r *reconciler) nowPtr() *metav1.Time {
	value := r.now
	return &value
}
