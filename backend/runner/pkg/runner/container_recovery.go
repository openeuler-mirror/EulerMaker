package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// Docker labels are the durable execution identity, including the window
// between daemon create and receipt of its response. Recover before watching
// new assignments. Never touch containers without our Runner and Job UID.
func (a *Agent) recoverContainers(ctx context.Context) error {
	if a.cfg.Type != "ct" {
		return nil
	}
	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	output, err := outputDocker(queryCtx, "ps", "-aq", "--filter", "label=ebs.io/runner="+a.cfg.Name, "--filter", "label=ebs.io/job-uid")
	if err != nil {
		return err
	}
	for _, id := range strings.Fields(output) {
		if err := a.recoverContainer(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) recoverContainer(ctx context.Context, id string) error {
	inspectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	output, err := outputDocker(inspectCtx, "inspect", "--format", "{{json .Config.Labels}}", id)
	cancel()
	if err != nil {
		return err
	}
	var labels map[string]string
	if err := json.Unmarshal([]byte(output), &labels); err != nil {
		return err
	}
	project, name, uid := labels["ebs.io/project"], labels["ebs.io/job"], labels["ebs.io/job-uid"]
	if labels["ebs.io/runner"] != a.cfg.Name || project == "" || name == "" || uid == "" {
		return nil
	}
	getCtx, cancelGet := context.WithTimeout(ctx, 15*time.Second)
	job, err := a.client.GetJob(getCtx, project, name)
	cancelGet()
	var statusErr StatusError
	if err != nil && !(errors.As(err, &statusErr) && statusErr.Code == 404) {
		return err
	}
	// The agent cannot reattach an executor after restart. Stop the old process
	// first; a still-Running Job will then be resumed by normal list handling.
	stopCtx, cancelStop := context.WithTimeout(ctx, 15*time.Second)
	stopErr := (DockerCLI{}).Stop(stopCtx, id, 10*time.Second)
	cancelStop()
	removeCtx, cancelRemove := context.WithTimeout(ctx, 15*time.Second)
	removeErr := (DockerCLI{}).Remove(removeCtx, id)
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
