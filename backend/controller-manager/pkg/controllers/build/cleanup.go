package build

import (
	"context"
	"fmt"
	"log"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
)

func (c *Controller) cleanupBuild(ctx context.Context, build *ebsv1.Build) (controller.ReconcileResult, error) {
	key := build.Namespace + "/" + build.Name
	if !slices.Contains(build.Finalizers, ebsv1.BuildCleanupFinalizer) {
		return controller.ReconcileResult{}, nil
	}
	wait := controller.ReconcileResult{RequeueAfter: time.Second}
	// Delegate to BuildInfo whenever it exists; its finalizer owns Jobs and BuildGraph.
	infoDone, err := c.cleanupBuildInfo(ctx, build)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	// Snapshot deletion is independent of BuildInfo descendant cleanup.
	snapshotDone, err := c.cleanupSnapshot(ctx, build)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	// Keep repositories available until BuildInfo and its descendants have disappeared.
	if !infoDone {
		return wait, nil
	}
	repoDone, err := c.cleanupRpmRepo(ctx, build)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	if !snapshotDone || !repoDone {
		return wait, nil
	}
	// Finalize only after all directly created resources have disappeared.
	latest, err := c.client.GetBuild(ctx, build.Namespace, build.Name)
	if apierrors.IsNotFound(err) {
		return controller.ReconcileResult{}, nil
	}
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	if latest.UID != build.UID || latest.DeletionTimestamp == nil {
		return controller.ReconcileResult{}, nil
	}
	latest = latest.DeepCopy()
	latest.Finalizers = slices.DeleteFunc(latest.Finalizers, func(value string) bool { return value == ebsv1.BuildCleanupFinalizer })
	if _, err := c.client.UpdateBuild(ctx, latest); err != nil {
		return controller.ReconcileResult{}, err
	}
	log.Printf("controller=build key=%q reason=BuildCleanupCompleted", key)
	return controller.ReconcileResult{}, nil
}

// cleanupBuildInfo delegates descendant cleanup and waits until BuildInfo is absent.
func (c *Controller) cleanupBuildInfo(ctx context.Context, build *ebsv1.Build) (bool, error) {
	info, err := c.client.GetBuildInfo(ctx, build.Namespace, build.Name)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if info.DeletionTimestamp != nil {
		return false, nil
	}
	if !slices.Contains(info.Finalizers, ebsv1.BuildInfoCleanupFinalizer) {
		return false, fmt.Errorf("BuildInfo cleanup finalizer is missing")
	}
	if err := c.client.DeleteBuildInfo(ctx, info); apierrors.IsNotFound(err) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	log.Printf("controller=build key=%q resource=buildinfos uid=%q reason=ChildDeletionRequested",
		info.Namespace+"/"+info.Name, info.UID)
	return false, nil
}

// cleanupSnapshot requests deletion and confirms absence on a subsequent reconcile.
func (c *Controller) cleanupSnapshot(ctx context.Context, build *ebsv1.Build) (bool, error) {
	snapshot, err := c.client.GetSnapshot(ctx, build.Namespace, build.Name)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if snapshot.DeletionTimestamp != nil {
		return false, nil
	}
	if err := c.client.DeleteSnapshot(ctx, snapshot); apierrors.IsNotFound(err) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	log.Printf("controller=build key=%q resource=snapshots uid=%q reason=ChildDeletionRequested",
		snapshot.Namespace+"/"+snapshot.Name, snapshot.UID)
	return false, nil
}

// cleanupRpmRepo delegates repository cleanup and waits until RpmRepo is absent.
func (c *Controller) cleanupRpmRepo(ctx context.Context, build *ebsv1.Build) (bool, error) {
	repo, err := c.client.GetRpmRepo(ctx, build.Namespace, build.Name)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if repo.DeletionTimestamp != nil {
		return false, nil
	}
	if err := c.client.DeleteRpmRepo(ctx, repo); apierrors.IsNotFound(err) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	log.Printf("controller=build key=%q resource=rpmrepos uid=%q reason=ChildDeletionRequested",
		repo.Namespace+"/"+repo.Name, repo.UID)
	return false, nil
}
