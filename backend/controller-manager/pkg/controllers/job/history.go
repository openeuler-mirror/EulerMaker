package job

import (
	"context"
	"fmt"
	"log"
	"time"

	ebsv1 "ebs-api/ebs/v1"
)

const (
	historyScanInterval = 24 * time.Hour
	historyPageSize     = 500
)

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
