package runner

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	clientpkg "controller-manager/pkg/client"
	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const maxHealthRequeueAfter = 24 * time.Hour

type DeadlineResult struct {
	Deadline     time.Time
	RequeueAfter time.Duration
	Expired      bool
	Basis        string
	FutureBasis  bool
}

type objectTimestampError struct{ err error }

func (e objectTimestampError) Error() string { return e.err.Error() }
func (e objectTimestampError) Unwrap() error { return e.err }

func calculateHealthDeadline(runner *ebsv1.Runner, now time.Time, config Config) (DeadlineResult, error) {
	if runner == nil || now.IsZero() {
		return DeadlineResult{}, fmt.Errorf("Runner and current time are required")
	}
	if config.HeartbeatTimeout <= 0 || config.StartupGracePeriod <= 0 {
		return DeadlineResult{}, fmt.Errorf("Runner health timeouts must be positive")
	}
	var base time.Time
	timeout := config.StartupGracePeriod
	basis := "creationTimestamp"
	if !runner.Status.Heartbeat.IsZero() {
		base = runner.Status.Heartbeat.Time
		timeout = config.HeartbeatTimeout
		basis = "heartbeat"
	} else if !runner.CreationTimestamp.IsZero() {
		base = runner.CreationTimestamp.Time
	} else {
		return DeadlineResult{}, objectTimestampError{err: fmt.Errorf("Runner has neither heartbeat nor creation timestamp")}
	}
	base = base.UTC().Round(0)
	now = now.UTC().Round(0)
	deadline := base.Add(timeout)
	if deadline.Before(base) || deadline.Year() < 1 || deadline.Year() > 9999 {
		return DeadlineResult{}, objectTimestampError{err: fmt.Errorf("Runner %s deadline is outside the supported time range", basis)}
	}
	result := DeadlineResult{Deadline: deadline, Basis: basis, FutureBasis: base.After(now)}
	if !now.Before(deadline) {
		result.Expired = true
		return result, nil
	}
	remaining := deadline.Sub(now)
	if remaining <= 0 {
		return DeadlineResult{}, fmt.Errorf("Runner deadline calculation produced a non-positive remaining duration")
	}
	if remaining > maxHealthRequeueAfter {
		remaining = maxHealthRequeueAfter
	}
	result.RequeueAfter = remaining
	return result, nil
}

func (c *Controller) sync(ctx context.Context, key string) (controller.ReconcileResult, error) {
	now := c.clock.Now()
	if key == "" || strings.Contains(key, "/") {
		return controller.ReconcileResult{}, controller.NewPermanentError(fmt.Errorf("invalid Runner key %q", key))
	}
	obj, exists, err := c.runners.GetByKey(key)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	if !exists {
		return controller.ReconcileResult{}, nil
	}
	cached, ok := obj.(*ebsv1.Runner)
	if !ok || cached == nil {
		return controller.ReconcileResult{}, fmt.Errorf("unexpected Runner cache object %T for %s", obj, key)
	}
	if cached.Name != key {
		return controller.ReconcileResult{}, fmt.Errorf("Runner cache object name %q does not match key %q", cached.Name, key)
	}
	futureObserved := false
	result, done, err := c.evaluateRunner(cached, now, &futureObserved)
	if err != nil || done || !result.Expired {
		if err != nil || done {
			return controller.ReconcileResult{}, err
		}
		return controller.ReconcileResult{RequeueAfter: result.RequeueAfter}, nil
	}
	return c.confirmAndMarkOffline(ctx, cached, now, &futureObserved)
}

func (c *Controller) evaluateRunner(runner *ebsv1.Runner, now time.Time, futureObserved *bool) (DeadlineResult, bool, error) {
	if runner.DeletionTimestamp != nil || runner.Status.Phase == "Offline" {
		return DeadlineResult{}, true, nil
	}
	if !processablePhase(runner.Status.Phase) {
		return DeadlineResult{}, false, controller.NewPermanentError(fmt.Errorf("Runner %s has unsupported phase %q", runner.Name, runner.Status.Phase))
	}
	heartbeatChecks.Inc()
	result, err := calculateHealthDeadline(runner, now, c.config)
	if err != nil {
		var timestampErr objectTimestampError
		if errors.As(err, &timestampErr) {
			invalidTimestamps.Inc()
			return DeadlineResult{}, false, controller.NewPermanentError(err)
		}
		return DeadlineResult{}, false, err
	}
	if result.FutureBasis && !*futureObserved {
		*futureObserved = true
		futureHeartbeats.Inc()
		log.Printf("controller=%s key=%s uid=%s resourceVersion=%s reason=FutureHealthTimestamp basis=%s deadline=%s", Name, runner.Name, runner.UID, runner.ResourceVersion, result.Basis, result.Deadline.Format(time.RFC3339Nano))
	}
	return result, false, nil
}

func processablePhase(phase string) bool {
	return phase == "Online"
}

func (c *Controller) confirmAndMarkOffline(ctx context.Context, cached *ebsv1.Runner, now time.Time, futureObserved *bool) (controller.ReconcileResult, error) {
	latest, err := c.client.GetRunner(ctx, cached.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return controller.ReconcileResult{}, nil
		}
		return controller.ReconcileResult{}, classifyReadError(err)
	}
	if err := validateRunnerResponse(latest, cached.Name); err != nil {
		return controller.ReconcileResult{}, err
	}
	if latest.UID != cached.UID {
		return controller.ReconcileResult{Requeue: true}, nil
	}
	deadline, done, err := c.evaluateRunner(latest, now, futureObserved)
	if err != nil || done {
		return controller.ReconcileResult{}, err
	}
	if !deadline.Expired {
		return controller.ReconcileResult{RequeueAfter: deadline.RequeueAfter}, nil
	}
	heartbeatTimeouts.Inc()
	request := latest.DeepCopy()
	request.Status.Phase = "Offline"
	updated, err := c.client.UpdateRunnerStatus(ctx, request)
	if err == nil {
		err = validateOfflineStatusResponse(updated, request)
	}
	if err == nil {
		offlineUpdates.Inc()
		log.Printf("controller=%s key=%s uid=%s resourceVersion=%s reason=HeartbeatTimeout", Name, latest.Name, latest.UID, latest.ResourceVersion)
		return controller.ReconcileResult{}, nil
	}
	return c.handleStatusWriteError(ctx, request, now, futureObserved, err)
}

func (c *Controller) handleStatusWriteError(ctx context.Context, request *ebsv1.Runner, now time.Time, futureObserved *bool, err error) (controller.ReconcileResult, error) {
	var writeErr *clientpkg.WriteError
	if !errors.As(err, &writeErr) || writeErr.Outcome == clientpkg.WriteUnknown {
		statusUpdateUnknown.Inc()
		return c.confirmUnknownStatus(ctx, request, now, futureObserved, err)
	}
	if writeErr.Outcome == clientpkg.WriteNotSent {
		if temporaryNotSent(writeErr.Err) {
			return controller.ReconcileResult{}, err
		}
		return controller.ReconcileResult{}, controller.NewPermanentError(err)
	}
	switch writeErr.StatusCode {
	case http.StatusNotFound:
		return controller.ReconcileResult{}, nil
	case http.StatusConflict, http.StatusPreconditionFailed:
		statusUpdateConflicts.Inc()
		return controller.ReconcileResult{Requeue: true}, nil
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return controller.ReconcileResult{}, err
	case http.StatusUnauthorized, http.StatusForbidden:
		return controller.ReconcileResult{}, controller.NewPermanentError(err)
	default:
		if writeErr.StatusCode >= 500 {
			return controller.ReconcileResult{}, err
		}
		return controller.ReconcileResult{}, controller.NewPermanentError(err)
	}
}

func (c *Controller) confirmUnknownStatus(ctx context.Context, request *ebsv1.Runner, now time.Time, futureObserved *bool, original error) (controller.ReconcileResult, error) {
	if ctx.Err() != nil {
		return controller.ReconcileResult{}, ctx.Err()
	}
	latest, err := c.client.GetRunner(ctx, request.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return controller.ReconcileResult{}, nil
		}
		log.Printf("controller=%s key=%s reason=StatusWriteUnknown writeError=%v confirmError=%v", Name, request.Name, original, err)
		return controller.ReconcileResult{}, classifyReadError(err)
	}
	if err := validateRunnerResponse(latest, request.Name); err != nil {
		return controller.ReconcileResult{}, err
	}
	if latest.UID != request.UID {
		return controller.ReconcileResult{Requeue: true}, nil
	}
	if latest.Status.Phase == "Offline" {
		offlineUpdates.Inc()
		return controller.ReconcileResult{}, nil
	}
	deadline, done, err := c.evaluateRunner(latest, now, futureObserved)
	if err != nil || done {
		return controller.ReconcileResult{}, err
	}
	if !deadline.Expired {
		return controller.ReconcileResult{RequeueAfter: deadline.RequeueAfter}, nil
	}
	return controller.ReconcileResult{Requeue: true}, nil
}

func classifyReadError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return err
	}
	code := int(status.Status().Code)
	if code == http.StatusUnauthorized || code == http.StatusForbidden || (code >= 300 && code < 500 && code != http.StatusRequestTimeout && code != http.StatusTooManyRequests) {
		return controller.NewPermanentError(err)
	}
	return err
}

func temporaryNotSent(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary())
}

func validateRunnerResponse(runner *ebsv1.Runner, name string) error {
	if runner == nil || runner.Name != name || runner.Namespace != "" || runner.UID == "" || runner.ResourceVersion == "" {
		return fmt.Errorf("unexpected Runner response for %s", name)
	}
	return nil
}

func validateOfflineStatusResponse(response, request *ebsv1.Runner) error {
	if response == nil || request == nil || response.Name != request.Name || response.Namespace != request.Namespace || response.UID != request.UID || response.ResourceVersion == "" {
		return runnerWriteUnknown(fmt.Errorf("Runner status response identity is invalid"))
	}
	if !apiequality.Semantic.DeepEqual(response.Spec, request.Spec) || response.Status.Phase != "Offline" ||
		!apiequality.Semantic.DeepEqual(response.Status.Conditions, request.Status.Conditions) ||
		!apiequality.Semantic.DeepEqual(response.Status.Capacity, request.Status.Capacity) ||
		!apiequality.Semantic.DeepEqual(response.Status.Allocatable, request.Status.Allocatable) ||
		!apiequality.Semantic.DeepEqual(response.Status.Addresses, request.Status.Addresses) ||
		!apiequality.Semantic.DeepEqual(response.Status.Info, request.Status.Info) ||
		!response.Status.Heartbeat.Equal(&request.Status.Heartbeat) {
		return runnerWriteUnknown(fmt.Errorf("Runner status response differs outside phase"))
	}
	return nil
}
