package job

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	clientpkg "controller-manager/pkg/client"
	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

func (c *Controller) sync(ctx context.Context, key string) (controller.ReconcileResult, error) {
	now := c.clock.Now()
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil || namespace == "" || name == "" {
		return controller.ReconcileResult{}, controller.NewPermanentError(fmt.Errorf("invalid Job key %q", key))
	}
	obj, exists, err := c.jobs.GetByKey(key)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	if !exists {
		c.index.remove(key)
		c.clearObservation(key)
		return controller.ReconcileResult{}, nil
	}
	job, ok := obj.(*ebsv1.Job)
	if !ok || job == nil {
		return controller.ReconcileResult{}, fmt.Errorf("unexpected Job cache object %T for %s", obj, key)
	}
	if terminal(job.Status.Phase) {
		c.clearObservation(key)
		return c.reconcileHistory(ctx, key, job, now)
	}
	if !processable(job) {
		c.clearObservation(key)
		return controller.ReconcileResult{}, nil
	}

	runnerAvailable, err := c.runnerAvailableFromCache(job.Status.Runner)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	if runnerAvailable {
		c.clearObservation(key)
		return controller.ReconcileResult{}, nil
	}
	observation := c.observation(key, job, now)
	remaining := observation.FirstSeen.Add(c.config.RunnerLostGracePeriod).Sub(now)
	if remaining > 0 {
		return controller.ReconcileResult{RequeueAfter: remaining}, nil
	}
	return c.failLostRunnerJob(ctx, key, job, now)
}

func (c *Controller) runnerAvailableFromCache(name string) (bool, error) {
	obj, exists, err := c.runners.GetByKey(name)
	if err != nil || !exists {
		return false, err
	}
	runner, ok := obj.(*ebsv1.Runner)
	if !ok || runner == nil {
		return false, fmt.Errorf("unexpected Runner cache object %T for %s", obj, name)
	}
	return runner.Status.Phase == "Online", nil
}

func (c *Controller) failLostRunnerJob(ctx context.Context, key string, cached *ebsv1.Job, now time.Time) (controller.ReconcileResult, error) {
	latest, err := c.client.GetJob(ctx, cached.Namespace, cached.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.clearObservation(key)
			return controller.ReconcileResult{}, nil
		}
		return controller.ReconcileResult{}, classifyReadError(err)
	}
	if err := validateJobResponse(latest, cached.Namespace, cached.Name); err != nil {
		return controller.ReconcileResult{}, err
	}
	if latest.UID != cached.UID || !processable(latest) || latest.Status.Runner != cached.Status.Runner {
		c.clearObservation(key)
		return controller.ReconcileResult{}, nil
	}
	if available, err := c.runnerAvailableFromCache(latest.Status.Runner); err != nil {
		return controller.ReconcileResult{}, err
	} else if available {
		c.clearObservation(key)
		return controller.ReconcileResult{}, nil
	}
	authoritative, err := c.client.GetRunner(ctx, latest.Status.Runner)
	if err != nil && !apierrors.IsNotFound(err) {
		return controller.ReconcileResult{}, classifyReadError(err)
	}
	if err == nil {
		if validationErr := validateRunnerResponse(authoritative, latest.Status.Runner); validationErr != nil {
			return controller.ReconcileResult{}, validationErr
		}
	}
	if err == nil && authoritative.Status.Phase == "Online" {
		c.clearObservation(key)
		log.Printf("controller=%s key=%s uid=%s runner=%s reason=RunnerRecovered", Name, key, latest.UID, latest.Status.Runner)
		return controller.ReconcileResult{}, nil
	}

	request := latest.DeepCopy()
	request.Status.Phase = "Failed"
	request.Status.EndTime = metav1.NewTime(now.UTC())
	request.Status.Message = fmt.Sprintf("runner %s is unavailable after %s grace period", latest.Status.Runner, c.config.RunnerLostGracePeriod)
	updated, err := c.client.UpdateJobStatus(ctx, request)
	if err == nil {
		err = validateFailedStatusResponse(updated, request)
	}
	if err == nil {
		c.clearObservation(key)
		runnerLostFailures.Inc()
		log.Printf("controller=%s key=%s uid=%s runner=%s phase=%s resourceVersion=%s reason=RunnerLost", Name, key, latest.UID, latest.Status.Runner, latest.Status.Phase, latest.ResourceVersion)
		return controller.ReconcileResult{}, nil
	}
	return c.handleStatusWriteError(ctx, key, latest, err)
}

func (c *Controller) handleStatusWriteError(ctx context.Context, key string, request *ebsv1.Job, err error) (controller.ReconcileResult, error) {
	var writeErr *clientpkg.WriteError
	if !errors.As(err, &writeErr) || writeErr.Outcome == clientpkg.WriteUnknown {
		statusUpdateUnknown.Inc()
		return c.confirmUnknownStatus(ctx, key, request, err)
	}
	if writeErr.Outcome == clientpkg.WriteNotSent {
		if temporaryNotSent(writeErr.Err) {
			return controller.ReconcileResult{}, err
		}
		return controller.ReconcileResult{}, controller.NewPermanentError(err)
	}
	switch writeErr.StatusCode {
	case http.StatusNotFound:
		c.clearObservation(key)
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

func (c *Controller) confirmUnknownStatus(ctx context.Context, key string, request *ebsv1.Job, original error) (controller.ReconcileResult, error) {
	if ctx.Err() != nil {
		return controller.ReconcileResult{}, ctx.Err()
	}
	latest, err := c.client.GetJob(ctx, request.Namespace, request.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.clearObservation(key)
			return controller.ReconcileResult{}, nil
		}
		log.Printf("controller=%s key=%s reason=StatusWriteUnknown writeError=%v confirmError=%v", Name, key, original, err)
		return controller.ReconcileResult{}, classifyReadError(err)
	}
	if err := validateJobResponse(latest, request.Namespace, request.Name); err != nil {
		return controller.ReconcileResult{}, err
	}
	if latest.UID != request.UID || latest.Status.Phase == "Failed" || !processable(latest) || latest.Status.Runner != request.Status.Runner {
		c.clearObservation(key)
		return controller.ReconcileResult{}, nil
	}
	return controller.ReconcileResult{Requeue: true}, nil
}

func (c *Controller) reconcileHistory(ctx context.Context, key string, cached *ebsv1.Job, now time.Time) (controller.ReconcileResult, error) {
	if !c.config.HistoryGCEnabled || cached.DeletionTimestamp != nil {
		return controller.ReconcileResult{}, nil
	}
	if cached.Status.EndTime.IsZero() {
		historyMissingEndTime.Inc()
		log.Printf("controller=%s key=%s uid=%s reason=HistoryMissingEndTime", Name, key, cached.UID)
		return controller.ReconcileResult{}, nil
	}
	remaining := cached.Status.EndTime.Time.Add(c.config.HistoryRetention).Sub(now)
	if remaining > 0 {
		historyGCScheduled.Inc()
		return controller.ReconcileResult{RequeueAfter: remaining}, nil
	}
	latest, err := c.client.GetJob(ctx, cached.Namespace, cached.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return controller.ReconcileResult{}, nil
		}
		return controller.ReconcileResult{}, classifyReadError(err)
	}
	if err := validateJobResponse(latest, cached.Namespace, cached.Name); err != nil {
		return controller.ReconcileResult{}, err
	}
	if latest.UID != cached.UID || !eligibleForDeletion(latest, now, c.config.HistoryRetention) {
		return controller.ReconcileResult{}, nil
	}
	preconditions := clientpkg.DeletePreconditions{UID: latest.UID, ResourceVersion: latest.ResourceVersion}
	err = c.client.DeleteJob(ctx, latest.Namespace, latest.Name, preconditions)
	if err == nil {
		historyDeleted.Inc()
		log.Printf("controller=%s key=%s uid=%s resourceVersion=%s reason=HistoryJobDeleted", Name, key, latest.UID, latest.ResourceVersion)
		return controller.ReconcileResult{}, nil
	}
	return c.handleDeleteError(ctx, latest, err, now)
}

func eligibleForDeletion(job *ebsv1.Job, now time.Time, retention time.Duration) bool {
	return job != nil && job.DeletionTimestamp == nil && terminal(job.Status.Phase) && !job.Status.EndTime.IsZero() && !job.Status.EndTime.Time.Add(retention).After(now)
}

func (c *Controller) handleDeleteError(ctx context.Context, request *ebsv1.Job, err error, now time.Time) (controller.ReconcileResult, error) {
	var writeErr *clientpkg.WriteError
	if !errors.As(err, &writeErr) || writeErr.Outcome == clientpkg.WriteUnknown {
		historyDeleteUnknown.Inc()
		return c.confirmUnknownDelete(ctx, request, err, now)
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

func (c *Controller) confirmUnknownDelete(ctx context.Context, request *ebsv1.Job, original error, now time.Time) (controller.ReconcileResult, error) {
	if ctx.Err() != nil {
		return controller.ReconcileResult{}, ctx.Err()
	}
	latest, err := c.client.GetJob(ctx, request.Namespace, request.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return controller.ReconcileResult{}, nil
		}
		log.Printf("controller=%s key=%s/%s reason=HistoryDeleteUnknown writeError=%v confirmError=%v", Name, request.Namespace, request.Name, original, err)
		return controller.ReconcileResult{}, classifyReadError(err)
	}
	if err := validateJobResponse(latest, request.Namespace, request.Name); err != nil {
		return controller.ReconcileResult{}, err
	}
	if latest.UID != request.UID || !eligibleForDeletion(latest, now, c.config.HistoryRetention) {
		return controller.ReconcileResult{}, nil
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

func validateJobResponse(job *ebsv1.Job, namespace, name string) error {
	if job == nil || job.Namespace != namespace || job.Name != name || job.UID == "" || job.ResourceVersion == "" {
		return fmt.Errorf("unexpected Job response for %s/%s", namespace, name)
	}
	return nil
}

func validateRunnerResponse(runner *ebsv1.Runner, name string) error {
	if runner == nil || runner.Name != name || runner.UID == "" || runner.ResourceVersion == "" {
		return fmt.Errorf("unexpected Runner response for %s", name)
	}
	return nil
}

func validateFailedStatusResponse(updated, request *ebsv1.Job) error {
	if updated == nil || updated.Namespace != request.Namespace || updated.Name != request.Name || updated.UID != request.UID || updated.ResourceVersion == "" ||
		updated.Status.Phase != "Failed" || updated.Status.Runner != request.Status.Runner || updated.Status.Stage != request.Status.Stage {
		return writeUnknown("update-status", fmt.Errorf("unexpected Job status response"))
	}
	return nil
}
