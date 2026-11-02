package job

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/cache"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
)

const lifecyclePollInterval = 10 * time.Second

func (c *Controller) lifecycleLoop(ctx context.Context) {
	for ctx.Err() == nil {
		if err := c.scanLifecycle(ctx); err != nil && ctx.Err() == nil {
			log.Printf("controller=%s reason=LifecycleScanFailed error=%v", Name, err)
		}
		timer := c.clock.NewTimer(lifecyclePollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C():
		}
	}
}

// scanLifecycle keeps deleting objects discoverable even when history GC is disabled or a watch excludes terminal Jobs.
func (c *Controller) scanLifecycle(ctx context.Context) error {
	for _, oldJob := range []bool{false, true} {
		prefix := "jobs/"
		if oldJob {
			prefix = "oldjobs/"
		}
		token := ""
		for {
			page, err := c.client.ListDeletingPage(ctx, oldJob, token, historyPageSize)
			if err != nil {
				return err
			}
			for _, obj := range page.Items {
				job, ok := obj.(*ebsv1.Job)
				if !ok || job == nil || job.Namespace == "" || job.Name == "" || job.UID == "" || job.ResourceVersion == "" || job.DeletionTimestamp == nil {
					return fmt.Errorf("unexpected deleting Job projection: %T", obj)
				}
				c.Enqueue(prefix + job.Namespace + "/" + job.Name)
			}
			if page.Continue == "" {
				break
			}
			if page.Continue == token {
				return fmt.Errorf("deleting Job list repeated its continuation token")
			}
			token = page.Continue
		}
	}
	return nil
}

func (c *Controller) syncLifecycle(ctx context.Context, key string) (controller.ReconcileResult, error) {
	kind, identity, _ := strings.Cut(key, "/")
	project, name, err := cache.SplitMetaNamespaceKey(identity)
	if err != nil || project == "" || name == "" {
		return controller.ReconcileResult{}, controller.NewPermanentError(fmt.Errorf("invalid lifecycle key %q", key))
	}
	var job *ebsv1.Job
	if kind == "oldjobs" {
		job, err = c.client.GetOldJob(ctx, project, name)
	} else {
		job, err = c.client.GetJob(ctx, project, name)
	}
	if apierrors.IsNotFound(err) {
		return controller.ReconcileResult{}, nil
	}
	if err != nil {
		return controller.ReconcileResult{}, classifyReadError(err)
	}
	if err := validateJobResponse(job, project, name); err != nil {
		return controller.ReconcileResult{}, err
	}
	if job.DeletionTimestamp == nil {
		return controller.ReconcileResult{}, nil
	}
	if kind == "oldjobs" {
		err = c.cleanupOldJob(ctx, job)
	} else {
		err = c.migrateJob(ctx, job)
	}
	if err != nil {
		// All operations are idempotent; re-read authoritative state before any further decision.
		var cleanup *cleanupError
		if errors.As(err, &cleanup) {
			if cleanup.status == 429 || cleanup.status == 503 {
				log.Printf("controller=%s key=%s reason=JobDataCleanupDeferred retryAfter=%s error=%v", Name, key, cleanup.retryAfter, err)
				return controller.ReconcileResult{RequeueAfter: cleanup.retryAfter}, nil
			}
			if cleanup.status >= 400 && cleanup.status < 500 && cleanup.status != 408 {
				return controller.ReconcileResult{}, controller.NewPermanentError(err)
			}
		}
		var write *clientpkg.WriteError
		if errors.As(err, &write) {
			if write.Outcome == clientpkg.WriteNotSent && !temporaryNotSent(write.Err) {
				return controller.ReconcileResult{}, controller.NewPermanentError(err)
			}
			if write.Outcome == clientpkg.WriteRejected && write.StatusCode >= 400 && write.StatusCode < 500 &&
				write.StatusCode != 404 && write.StatusCode != 408 && write.StatusCode != 409 && write.StatusCode != 412 && write.StatusCode != 429 {
				return controller.ReconcileResult{}, controller.NewPermanentError(err)
			}
		}
		return controller.ReconcileResult{}, err
	}
	return controller.ReconcileResult{RequeueAfter: lifecyclePollInterval}, nil
}

func (c *Controller) migrateJob(ctx context.Context, job *ebsv1.Job) error {
	if !job.Status.Phase.IsTerminal() {
		return controller.NewPermanentError(fmt.Errorf("deleting Job %s/%s is not terminal", job.Namespace, job.Name))
	}
	if job.Status.EndTime.IsZero() {
		return fmt.Errorf("terminal deleting Job %s/%s has no endTime", job.Namespace, job.Name)
	}
	state, err := c.client.GetArchiveState(ctx, job.Namespace, job.Name)
	if err != nil {
		return err
	}
	if state.UID != string(job.UID) {
		return fmt.Errorf("archive UID mismatch for %s/%s", job.Namespace, job.Name)
	}
	if state.Phase == "Pending" {
		_, err := c.client.ArchiveJob(ctx, job.Namespace, job.Name, clientpkg.DeletePreconditions{UID: job.UID, ResourceVersion: job.ResourceVersion})
		return err
	}
	if state.Phase != "Stored" {
		return fmt.Errorf("source Job still exists in archive phase %s", state.Phase)
	}
	if state.Action == "delete" {
		old, err := c.client.GetOldJob(ctx, job.Namespace, job.Name)
		if err != nil {
			return err
		}
		if old.UID != job.UID {
			return fmt.Errorf("OldJob UID mismatch")
		}
		if old.DeletionTimestamp == nil {
			return c.client.DeleteOldJob(ctx, old)
		}
	}
	if err := c.client.FinalizeJob(ctx, job, false); err != nil {
		return err
	}
	log.Printf("controller=%s key=%s/%s action=%s reason=JobArchiveFinalized", Name, job.Namespace, job.Name, state.Action)
	return nil
}

func (c *Controller) cleanupOldJob(ctx context.Context, job *ebsv1.Job) error {
	if !slices.Contains(job.Finalizers, ebsv1.JobArtifactsCleanupFinalizer) {
		return nil
	}
	state, err := c.client.GetArchiveState(ctx, job.Namespace, job.Name)
	if err != nil {
		return err
	}
	if state.UID != string(job.UID) {
		return fmt.Errorf("OldJob archive UID mismatch")
	}
	if state.Phase != "Transferred" {
		return nil
	}
	// The caller guarantees producers and materialization inputs have stopped using these files.
	if err := c.client.DeleteJobData(ctx, job); err != nil {
		return err
	}
	if err := c.client.FinalizeJob(ctx, job, true); err != nil {
		return err
	}
	log.Printf("controller=%s key=%s/%s reason=JobDataDeleted", Name, job.Namespace, job.Name)
	return nil
}
