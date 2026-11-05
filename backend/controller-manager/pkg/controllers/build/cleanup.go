package build

import (
	"context"
	"fmt"
	"log"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	info, err := c.client.GetBuildInfo(ctx, build.Namespace, build.Name)
	if err == nil && info.DeletionTimestamp == nil && !slices.Contains(info.Finalizers, ebsv1.BuildInfoCleanupFinalizer) {
		return controller.ReconcileResult{}, fmt.Errorf("BuildInfo cleanup finalizer is missing")
	}
	infoDone, err := c.cleanupChild(ctx, "buildinfos", info, err)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	// Snapshot deletion is independent of BuildInfo descendant cleanup.
	snapshot, err := c.client.GetSnapshot(ctx, build.Namespace, build.Name)
	snapshotDone, err := c.cleanupChild(ctx, "snapshots", snapshot, err)
	if err != nil {
		return controller.ReconcileResult{}, err
	}
	// Keep repositories available until BuildInfo and its descendants have disappeared.
	if !infoDone {
		return wait, nil
	}
	repo, err := c.client.GetRpmRepo(ctx, build.Namespace, build.Name)
	repoDone, err := c.cleanupChild(ctx, "rpmrepos", repo, err)
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

// cleanupChild returns done only when GET or DELETE confirms the child is absent.
func (c *Controller) cleanupChild(ctx context.Context, resource string, obj metav1.Object, readErr error) (bool, error) {
	if apierrors.IsNotFound(readErr) {
		return true, nil
	}
	if readErr != nil {
		return false, readErr
	}
	if obj.GetDeletionTimestamp() != nil {
		return false, nil
	}
	err := c.client.DeleteCleanupChild(ctx, obj.GetNamespace(), resource, obj.GetName(), obj)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	log.Printf("controller=build key=%q resource=%q uid=%q reason=ChildDeletionRequested",
		obj.GetNamespace()+"/"+obj.GetName(), resource, obj.GetUID())
	return false, nil
}
