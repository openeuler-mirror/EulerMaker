package build

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"reflect"
	"sort"
	"time"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// conflictRequeueDelay is used when the current round must be recomputed from a fresh object.
const conflictRequeueDelay = time.Second

type conditionIntent struct {
	condType           string
	status             metav1.ConditionStatus
	reason             string
	message            string
	observedGeneration int64
}

// writeIntent records the exact fields one round of reconciliation wants to reach. It is kept in memory for
// the duration of the round only, so an unknown write outcome can be confirmed against the original intent
// instead of a newly recomputed target.
type writeIntent struct {
	uid   types.UID
	phase ebsv1.BuildPhase
	stage string

	setBaseBuildRef bool
	baseBuildRef    *ebsv1.BaseBuildRef

	setStartTime bool
	startTime    metav1.Time

	setEndTime bool
	endTime    metav1.Time

	setConditions    bool
	conditions       []metav1.Condition
	conditionIntents []conditionIntent
}

func applyIntent(build *ebsv1.Build, intent writeIntent) {
	build.Status.Phase = intent.phase
	build.Status.Stage = intent.stage
	if intent.setBaseBuildRef {
		build.Status.BaseBuildRef = intent.baseBuildRef
	}
	if intent.setStartTime {
		build.Status.StartTime = intent.startTime
	}
	if intent.setEndTime {
		build.Status.EndTime = intent.endTime
	}
	if intent.setConditions {
		build.Status.Conditions = intent.conditions
	}
}

// intentSatisfied compares only the target fields of the original intent, matching conditions by type and
// ignoring their order.
func intentSatisfied(build *ebsv1.Build, intent writeIntent) bool {
	if build.Status.Phase != intent.phase || build.Status.Stage != intent.stage {
		return false
	}
	if intent.setBaseBuildRef && !reflect.DeepEqual(build.Status.BaseBuildRef, intent.baseBuildRef) {
		return false
	}
	if intent.setStartTime && !build.Status.StartTime.Time.Equal(intent.startTime.Time) {
		return false
	}
	if intent.setEndTime && !build.Status.EndTime.Time.Equal(intent.endTime.Time) {
		return false
	}
	for _, want := range intent.conditionIntents {
		matched := false
		for _, got := range build.Status.Conditions {
			if got.Type != want.condType {
				continue
			}
			matched = got.Status == want.status &&
				got.Reason == want.reason &&
				got.Message == want.message &&
				got.ObservedGeneration == want.observedGeneration
			break
		}
		if !matched {
			return false
		}
	}
	return true
}

// apply commits target when it differs from the object fetched at the start of the round and returns outcome
// afterwards. When the target status is already persisted the write is skipped and outcome is returned as is.
func (r *reconciler) apply(target *ebsv1.Build, intent writeIntent, outcome controller.ReconcileResult, outcomeErr error) (controller.ReconcileResult, error) {
	if statusEquivalent(r.current.Status, target.Status) {
		return outcome, outcomeErr
	}
	confirmed, result, err := r.commit(intent)
	if !confirmed {
		return result, err
	}
	return outcome, outcomeErr
}

// commit performs the pre-write concurrency check and one /status write.
func (r *reconciler) commit(intent writeIntent) (bool, controller.ReconcileResult, error) {
	if err := r.ctx.Err(); err != nil {
		return false, controller.ReconcileResult{}, err
	}
	latest, err := r.controller.client.GetBuild(r.ctx, r.project, r.current.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, controller.ReconcileResult{}, nil
		}
		return false, controller.ReconcileResult{}, classifyReadError(err)
	}
	if latest.UID != intent.uid {
		return false, controller.ReconcileResult{}, nil
	}
	if latest.Status.Phase.IsTerminal() || latest.DeletionTimestamp != nil {
		return false, controller.ReconcileResult{}, nil
	}
	if latest.ResourceVersion != r.current.ResourceVersion {
		statusUpdateConflicts.Inc()
		log.Printf("controller=%s key=%q uid=%q resourceVersion=%q reason=PreWriteConflict", Name, r.key, latest.UID, latest.ResourceVersion)
		return false, controller.ReconcileResult{RequeueAfter: conflictRequeueDelay}, nil
	}
	request := latest.DeepCopy()
	applyIntent(request, intent)
	updated, err := r.controller.client.UpdateBuildStatus(r.ctx, request)
	if err == nil {
		log.Printf("controller=%s key=%q uid=%q resourceVersion=%q phase=%q stage=%q result=StatusWritten", Name, r.key, updated.UID, updated.ResourceVersion, updated.Status.Phase, updated.Status.Stage)
		return true, controller.ReconcileResult{}, nil
	}
	return r.handleWriteError(intent, err)
}

// handleWriteError maps one failed write to the queue result contract of the design.
func (r *reconciler) handleWriteError(intent writeIntent, err error) (bool, controller.ReconcileResult, error) {
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return false, controller.ReconcileResult{}, ctxErr
	}
	var writeErr *clientpkg.WriteError
	if !errors.As(err, &writeErr) {
		return false, controller.ReconcileResult{}, err
	}
	switch writeErr.Outcome {
	case clientpkg.WriteUnknown:
		statusUpdateUnknowns.Inc()
		latest, getErr := r.controller.client.GetBuild(r.ctx, r.project, r.current.Name)
		if apierrors.IsNotFound(getErr) {
			return false, controller.ReconcileResult{}, nil
		}
		if getErr != nil {
			return false, controller.ReconcileResult{}, classifyReadError(getErr)
		}
		if latest.UID != intent.uid {
			return false, controller.ReconcileResult{}, nil
		}
		if intentSatisfied(latest, intent) {
			log.Printf("controller=%s key=%q uid=%q reason=StatusWriteConfirmed", Name, r.key, latest.UID)
			return true, controller.ReconcileResult{}, nil
		}
		if latest.Status.Phase.IsTerminal() || latest.DeletionTimestamp != nil {
			return false, controller.ReconcileResult{}, nil
		}
		return false, controller.ReconcileResult{RequeueAfter: conflictRequeueDelay}, nil
	case clientpkg.WriteRejected:
		switch writeErr.StatusCode {
		case 404:
			return false, controller.ReconcileResult{}, nil
		case 409, 412:
			statusUpdateConflicts.Inc()
			return false, controller.ReconcileResult{RequeueAfter: conflictRequeueDelay}, nil
		case 408, 429:
			return false, controller.ReconcileResult{}, err
		}
		if writeErr.StatusCode >= 500 && writeErr.StatusCode < 600 {
			return false, controller.ReconcileResult{}, err
		}
		return false, controller.ReconcileResult{}, controller.NewPermanentError(err)
	case clientpkg.WriteNotSent:
		return false, controller.ReconcileResult{}, classifyNotSentWrite(err)
	default:
		return false, controller.ReconcileResult{}, err
	}
}

// classifyNotSentWrite keeps client-side validation errors permanent while letting transient network failures
// and timeouts return to the framework backoff.
func classifyNotSentWrite(err error) error {
	var networkError net.Error
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) {
		return err
	}
	return controller.NewPermanentError(err)
}

// retryableCreateRejection reports whether an explicit rejection of a create request returns to the framework
// backoff instead of being treated as a permanent API error. 404 is retryable on purpose: it means the target
// route or its parent object is not available yet (apiserver version skew, Project not created yet), which the
// environment can fix. 409 and 412 are listed defensively so that a future reordering of the ensure protocol
// cannot turn an AlreadyExists / precondition conflict into a permanent error.
func retryableCreateRejection(statusCode int) bool {
	switch statusCode {
	case http.StatusNotFound, http.StatusRequestTimeout, http.StatusConflict,
		http.StatusPreconditionFailed, http.StatusTooManyRequests:
		return true
	}
	return statusCode >= 500 && statusCode < 600
}

// classifyRejectedWrite maps an explicit rejection of a create request.
func classifyRejectedWrite(writeErr *clientpkg.WriteError, err error) error {
	if retryableCreateRejection(writeErr.StatusCode) {
		return err
	}
	return controller.NewPermanentError(err)
}

// classifyReadError maps a failed read request to the queue result contract: API permanent rejections
// (401/403/400/422), bad client input and responses that violate the API contract are permanent, everything
// else (404, 408, 429, 5xx, network errors and timeouts) stays retryable. NotFound is deliberately not
// permanent here: the call sites decide what a missing object means.
func classifyReadError(err error) error {
	if apierrors.IsUnauthorized(err) || apierrors.IsForbidden(err) ||
		apierrors.IsBadRequest(err) || apierrors.IsInvalid(err) {
		return controller.NewPermanentError(err)
	}
	var contract contractError
	if errors.As(err, &contract) {
		return controller.NewPermanentError(err)
	}
	return err
}

// statusEquivalent compares the status fields this controller owns. Conditions are matched by type, so their
// slice order does not create spurious writes.
func statusEquivalent(a, b ebsv1.BuildStatus) bool {
	if a.Phase != b.Phase || a.Stage != b.Stage {
		return false
	}
	if !reflect.DeepEqual(a.BaseBuildRef, b.BaseBuildRef) {
		return false
	}
	if !a.StartTime.Time.Equal(b.StartTime.Time) || !a.EndTime.Time.Equal(b.EndTime.Time) {
		return false
	}
	return reflect.DeepEqual(normalizeConditions(a.Conditions), normalizeConditions(b.Conditions))
}

type comparableCondition struct {
	Type, Status, Reason, Message string
	ObservedGeneration            int64
}

func normalizeConditions(value []metav1.Condition) []comparableCondition {
	result := make([]comparableCondition, 0, len(value))
	for _, item := range value {
		result = append(result, comparableCondition{
			Type:               item.Type,
			Status:             string(item.Status),
			Reason:             item.Reason,
			Message:            item.Message,
			ObservedGeneration: item.ObservedGeneration,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Type != result[j].Type {
			return result[i].Type < result[j].Type
		}
		return result[i].Status < result[j].Status
	})
	return result
}
