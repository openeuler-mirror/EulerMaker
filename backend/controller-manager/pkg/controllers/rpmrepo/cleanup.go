package rpmrepo

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
)

// cleanupRpmRepo drains work and deletes build repositories before releasing the deletion finalizer.
func (r *reconciler) cleanupRpmRepo(repo *ebsv1.RpmRepo) (controller.ReconcileResult, error) {
	if !slices.Contains(repo.Finalizers, ebsv1.RpmRepoCleanupFinalizer) {
		return controller.ReconcileResult{}, nil
	}
	if !meta.IsStatusConditionTrue(repo.Status.Conditions, ebsv1.RpmRepoConditionCleanupReady) ||
		(repo.Status.Repository != nil && repo.Status.Repository.Transition != nil) ||
		(repo.Status.Release != nil && repo.Status.Release.Transition != nil) {
		result, err := r.drainCheckpoints(repo)
		if err == nil && result.RequeueAfter == 0 {
			result.RequeueAfter = time.Second
		}
		return result, err
	}
	if err := r.selectArtifactManager(repo); err != nil {
		return controller.ReconcileResult{}, err
	}
	done, err := r.artifactClient().DeleteBuildRepositories(r.ctx, r.project, repo.Labels[ebsv1.BuildTargetArchLabel], repo.Name)
	if err != nil {
		return controller.ReconcileResult{}, classifyArtifactReadError(err)
	}
	if !done {
		return controller.ReconcileResult{RequeueAfter: 5 * time.Second}, nil
	}
	target := repo.DeepCopy()
	target.Finalizers = slices.DeleteFunc(target.Finalizers, func(value string) bool { return value == ebsv1.RpmRepoCleanupFinalizer })
	updated, err := r.controller.client.UpdateRpmRepo(r.ctx, target)
	if apierrors.IsNotFound(err) {
		return controller.ReconcileResult{}, nil
	}
	if err == nil {
		if slices.Contains(updated.Finalizers, ebsv1.RpmRepoCleanupFinalizer) {
			return controller.ReconcileResult{}, fmt.Errorf("RpmRepo cleanup finalizer removal was not confirmed")
		}
		return controller.ReconcileResult{}, nil
	}
	var writeErr *clientpkg.WriteError
	if errors.As(err, &writeErr) && writeErr.Outcome == clientpkg.WriteUnknown {
		latest, getErr := r.controller.client.GetRpmRepo(r.ctx, r.project, repo.Name)
		if apierrors.IsNotFound(getErr) {
			return controller.ReconcileResult{}, nil
		}
		if getErr != nil {
			return controller.ReconcileResult{}, getErr
		}
		if latest.UID != repo.UID || !slices.Contains(latest.Finalizers, ebsv1.RpmRepoCleanupFinalizer) {
			return controller.ReconcileResult{}, nil
		}
	}
	// Leave conflicts and unconfirmed writes for a fresh reconcile; never replay the old target.
	return controller.ReconcileResult{}, err
}

// drainCheckpoints only observes registered work; 404 or transport failures never discard an unknown submission.
func (r *reconciler) drainCheckpoints(repo *ebsv1.RpmRepo) (controller.ReconcileResult, error) {
	hasRepository := repo.Status.Repository != nil && repo.Status.Repository.Transition != nil
	hasRelease := repo.Status.Release != nil && repo.Status.Release.Transition != nil
	if hasRepository || hasRelease {
		if err := r.selectArtifactManager(repo); err != nil {
			return controller.ReconcileResult{}, err
		}
	}
	target := repo.DeepCopy()
	if hasRepository {
		transition := repo.Status.Repository.Transition
		response, err := r.artifactClient().GetRepository(r.ctx, transition.RepositoryUID)
		if err != nil {
			return controller.ReconcileResult{}, err
		}
		if violation := repositoryContractViolation(transition, response); violation != "" {
			return controller.ReconcileResult{}, fmt.Errorf("repository cleanup response: %s", violation)
		}
		switch response.State {
		case RepositoryFailed:
			if response.Failure == nil || response.Failure.Retryable {
				return controller.ReconcileResult{}, fmt.Errorf("repository cleanup cannot confirm retryable failure has no late replay")
			}
			fallthrough
		case RepositoryReady:
			target.Status.Repository.Transition = nil
		case RepositoryCreating, RepositoryDeleting:
			return controller.ReconcileResult{RequeueAfter: pollAfter(response.PollAfterSeconds)}, nil
		default:
			return controller.ReconcileResult{}, fmt.Errorf("unknown repository cleanup state %q", response.State)
		}
	}
	if hasRelease {
		response, err := r.artifactClient().GetRelease(r.ctx, repo.Name)
		if err != nil {
			return controller.ReconcileResult{}, err
		}
		if violation := releaseContractViolation(repo, response); violation != "" {
			return controller.ReconcileResult{}, fmt.Errorf("release cleanup response: %s", violation)
		}
		switch response.State {
		case ReleaseFailed:
			if response.Failure == nil || response.Failure.Retryable {
				return controller.ReconcileResult{}, fmt.Errorf("release cleanup cannot confirm retryable failure has no late replay")
			}
			fallthrough
		case ReleaseReady, ReleasePrepared:
			// Prepared work is already idle. Cleanup must not activate a release and make it current.
			target.Status.Release.Transition = nil
		case ReleaseCreating, ReleaseDeleting:
			return controller.ReconcileResult{RequeueAfter: pollAfter(response.PollAfterSeconds)}, nil
		default:
			return controller.ReconcileResult{}, fmt.Errorf("unknown release cleanup state %q", response.State)
		}
	}
	// This acknowledgment is written by the sole producer, never inferred by the cleanup consumer.
	target.Status.Conditions, _ = MergeCondition(target.Status.Conditions,
		ebsv1.RpmRepoConditionCleanupReady, metav1.ConditionTrue, ebsv1.RpmRepoReasonOperationsDrained,
		"New submissions stopped and all registered operations drained", target.Generation, r.now)
	_, err := r.commitStatus(target, func(current *ebsv1.RpmRepo) bool { return reflect.DeepEqual(current.Status, target.Status) }, "BuildCleanupCheckpointDrained")
	return controller.ReconcileResult{}, err
}
