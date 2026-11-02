package job

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"

	clientpkg "controller-manager/pkg/clients/apiserver"
	ebsv1 "ebs-api/ebs/v1"
)

const (
	historyScanInterval = 24 * time.Hour
	historyPageSize     = 500
)

// archiveHistory starts migration; the lifecycle loop finishes it without issuing a delete intent.
func (c *Controller) archiveHistory(ctx context.Context, job *ebsv1.Job) (bool, error) {
	deferred := func(result string, err error) (bool, error) {
		historyArchive[result].Inc()
		log.Printf("controller=%s key=%s/%s action=archive uid=%s resourceVersion=%s reason=HistoryJobArchiveDeferred error=%v",
			Name, job.Namespace, job.Name, job.UID, job.ResourceVersion, err)
		return false, err
	}
	archived, err := c.client.ArchiveJob(ctx, job.Namespace, job.Name, clientpkg.DeletePreconditions{
		UID: job.UID, ResourceVersion: job.ResourceVersion,
	})
	if err == nil && !sameArchivedJob(job, archived) {
		err = writeUnknown("archive", fmt.Errorf("archive response does not match source snapshot"))
	}
	if err != nil {
		// An accepted request leaves a deleting Job for the lifecycle poller, including after a lost response.
		result := "unknown"
		var write *clientpkg.WriteError
		if errors.As(err, &write) && write.Outcome != clientpkg.WriteUnknown {
			result = "error"
			if write.Outcome == clientpkg.WriteRejected && (write.StatusCode == 409 || write.StatusCode == 412) {
				result = "conflict"
			}
		}
		return deferred(result, err)
	}
	historyArchive["confirmed"].Inc()
	log.Printf("controller=%s key=%s/%s action=archive uid=%s resourceVersion=%s reason=HistoryJobArchived",
		Name, job.Namespace, job.Name, job.UID, job.ResourceVersion)
	return true, nil
}

func sameArchivedJob(source, archived *ebsv1.Job) bool {
	return source != nil && archived != nil &&
		source.Namespace == archived.Namespace && source.Name == archived.Name && source.UID == archived.UID && archived.ResourceVersion != "" &&
		equality.Semantic.DeepEqual(source.Spec, archived.Spec) &&
		equality.Semantic.DeepEqual(source.Status, archived.Status)
}

// Run keeps history scanning separate from the live Job queue and waits for both on shutdown.
func (c *Controller) Run(ctx context.Context, workers int) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var loops sync.WaitGroup
	loops.Add(2)
	go func() {
		defer loops.Done()
		if c.config.HistoryGCEnabled {
			c.historyLoop(ctx)
		}
	}()
	go func() { defer loops.Done(); c.lifecycleLoop(ctx) }()
	err := c.BaseController.Run(ctx, workers)
	cancel()
	loops.Wait()
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
