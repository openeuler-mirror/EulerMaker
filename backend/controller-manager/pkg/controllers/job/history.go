package job

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"

	clientpkg "controller-manager/pkg/clients/apiserver"
	ebsv1 "ebs-api/ebs/v1"
)

const (
	historyScanInterval = 24 * time.Hour
	historyPageSize     = 500
)

// archiveHistory confirms the exact snapshot that the caller will conditionally delete.
func (c *Controller) archiveHistory(ctx context.Context, job *ebsv1.Job) (bool, error) {
	deferred := func(result string, err error) (bool, error) {
		historyArchive[result].Inc()
		log.Printf("controller=%s key=%s/%s action=archive uid=%s resourceVersion=%s reason=HistoryJobArchiveDeferred error=%v",
			Name, job.Namespace, job.Name, job.UID, job.ResourceVersion, err)
		return false, err
	}
	if len(job.Finalizers) > 0 {
		return deferred("error", fmt.Errorf("Job has finalizers"))
	}
	archived, err := c.client.ArchiveJob(ctx, job.Namespace, job.Name, clientpkg.DeletePreconditions{
		UID: job.UID, ResourceVersion: job.ResourceVersion,
	})
	if err == nil && !sameArchivedJob(job, archived) {
		err = writeUnknown("archive", fmt.Errorf("archive response does not match source snapshot"))
	}
	if err != nil {
		var writeErr *clientpkg.WriteError
		if errors.As(err, &writeErr) && writeErr.Outcome != clientpkg.WriteUnknown {
			if writeErr.Outcome == clientpkg.WriteRejected && writeErr.StatusCode == 404 {
				return false, nil
			}
			result := "error"
			if writeErr.Outcome == clientpkg.WriteRejected && (writeErr.StatusCode == 409 || writeErr.StatusCode == 412) {
				result = "conflict"
			}
			return deferred(result, err)
		}
		if ctx.Err() != nil {
			return deferred("unknown", err)
		}
		confirmed, getErr := c.client.GetOldJob(ctx, job.Namespace, job.Name)
		if getErr != nil || !sameArchivedJob(job, confirmed) {
			return deferred("unknown", fmt.Errorf("archive unconfirmed: write=%v, confirmation=%v", err, getErr))
		}
	}
	historyArchive["confirmed"].Inc()
	log.Printf("controller=%s key=%s/%s action=archive uid=%s resourceVersion=%s reason=HistoryJobArchived",
		Name, job.Namespace, job.Name, job.UID, job.ResourceVersion)
	return true, nil
}

func sameArchivedJob(source, archived *ebsv1.Job) bool {
	return source != nil && archived != nil &&
		equality.Semantic.DeepEqual(source.ObjectMeta, archived.ObjectMeta) &&
		equality.Semantic.DeepEqual(source.Spec, archived.Spec) &&
		equality.Semantic.DeepEqual(source.Status, archived.Status)
}

// Run keeps history scanning separate from the live Job queue and waits for both on shutdown.
func (c *Controller) Run(ctx context.Context, workers int) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if c.config.HistoryGCEnabled {
			c.historyLoop(ctx)
		}
	}()
	err := c.BaseController.Run(ctx, workers)
	cancel()
	<-done
	return err
}

func (c *Controller) historyLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := c.scanHistory(ctx); err != nil && ctx.Err() == nil {
			log.Printf("controller=%s reason=HistoryScanFailed error=%v", Name, err)
		}
		timer := c.clock.NewTimer(historyScanInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C():
		}
	}
}

// scanHistory processes one page at a time without retaining historical Jobs or delayed queue entries.
func (c *Controller) scanHistory(ctx context.Context) error {
	historyGCScans.Inc()
	now := c.clock.Now()
	for _, phase := range []ebsv1.JobPhase{ebsv1.JobSucceeded, ebsv1.JobFailed, ebsv1.JobAborted} {
		token := ""
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			page, err := c.client.ListHistoryPage(ctx, phase, token, historyPageSize)
			if err != nil {
				return fmt.Errorf("list historical Jobs in phase %s: %w", phase, err)
			}
			for _, obj := range page.Items {
				if err := ctx.Err(); err != nil {
					return err
				}
				job, ok := obj.(*ebsv1.Job)
				if !ok || job == nil || job.Namespace == "" || job.Name == "" || job.UID == "" || job.ResourceVersion == "" {
					return fmt.Errorf("unexpected historical Job response: %T", obj)
				}
				key := job.Namespace + "/" + job.Name
				result, err := c.reconcileHistory(ctx, key, job, now)
				if err != nil || result.Requeue || result.RequeueAfter > 0 {
					// The next daily scan rechecks current state; never retain or replay a stale delete request.
					log.Printf("controller=%s key=%s reason=HistoryDeleteDeferred error=%v", Name, key, err)
				}
			}
			if page.Continue == "" {
				break
			}
			if page.Continue == token {
				return fmt.Errorf("historical Job list returned a repeated continue token")
			}
			token = page.Continue
		}
	}
	return nil
}
