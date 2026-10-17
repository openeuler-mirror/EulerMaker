package runner

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"ebs-runner/pkg/gateway"
)

// Container labels are the durable execution identity, including the window
// between daemon create and receipt of its response. Recover before watching
// new assignments. Never touch containers without our Runner and Job UID.
func (a *Agent) recoverContainers(ctx context.Context) error {
	if a.cfg.Type != "ct" {
		return nil
	}
	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ids, err := a.containers.List(queryCtx, map[string]string{
		"ebs.io/runner":  a.cfg.Name,
		"ebs.io/job-uid": "",
	})
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := a.recoverContainer(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) recoverContainer(ctx context.Context, id string) error {
	inspectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	labels, err := a.containers.Labels(inspectCtx, id)
	cancel()
	if err != nil {
		return err
	}
	project, name, uid := labels["ebs.io/project"], labels["ebs.io/job"], labels["ebs.io/job-uid"]
	if labels["ebs.io/runner"] != a.cfg.Name || project == "" || name == "" || uid == "" {
		return nil
	}
	getCtx, cancelGet := context.WithTimeout(ctx, 15*time.Second)
	job, err := a.client.GetJob(getCtx, project, name)
	cancelGet()
	var statusErr gateway.StatusError
	if err != nil && !(errors.As(err, &statusErr) && statusErr.Code == 404) {
		return err
	}
	// The agent cannot reattach an executor after restart. Stop the old process
	// first; a still-Running Job will then be resumed by normal list handling.
	stopCtx, cancelStop := context.WithTimeout(ctx, 15*time.Second)
	stopErr := a.containers.Stop(stopCtx, id, 10*time.Second)
	cancelStop()
	removeCtx, cancelRemove := context.WithTimeout(ctx, 15*time.Second)
	removeErr := a.containers.Remove(removeCtx, id)
	cancelRemove()
	if removeErr != nil {
		return fmt.Errorf("container %s cleanup unconfirmed: stop=%v remove=%w", id, stopErr, removeErr)
	}
	phase := "missing"
	if job != nil && err == nil {
		phase = job.Status.Phase
	}
	log.Printf("recovered managed container project=%q job=%q uid=%q current_phase=%q", project, name, uid, phase)
	return nil
}
