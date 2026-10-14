package runner

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

const jobWorkSweepInterval = 10 * time.Minute

// sweepJobWorkLoop recovers directories left by interrupted executions. It
// runs after container recovery, which must confirm old containers are gone.
func (a *Agent) sweepJobWorkLoop(ctx context.Context) {
	a.sweepJobWork(ctx)
	ticker := time.NewTicker(jobWorkSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			a.sweepJobWork(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (a *Agent) sweepJobWork(ctx context.Context) {
	root := workDir(a.cfg.RootDir)
	projects, err := os.ReadDir(root)
	if err != nil {
		log.Printf("scan Job work directories: %v", err)
		return
	}
	for _, project := range projects {
		if ctx.Err() != nil {
			return
		}
		if !project.IsDir() || !validLocalPathSegment(project.Name()) {
			continue
		}
		projectPath := filepath.Join(root, project.Name())
		jobs, err := os.ReadDir(projectPath)
		if err != nil {
			log.Printf("scan Job work directory %s: %v", projectPath, err)
			continue
		}
		for _, entry := range jobs {
			if ctx.Err() != nil {
				return
			}
			if !entry.IsDir() || !validLocalPathSegment(entry.Name()) {
				continue
			}
			checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			job, getErr := a.client.GetJob(checkCtx, project.Name(), entry.Name())
			cancel()
			var statusErr StatusError
			if getErr != nil && !(errors.As(getErr, &statusErr) && statusErr.Code == 404) {
				log.Printf("confirm Job work directory %s/%s: %v", project.Name(), entry.Name(), getErr)
				continue
			}
			if getErr == nil && (job == nil || !terminalJob(job.Status.Phase)) {
				continue
			}
			if err := a.removeInactiveJobWork(project.Name(), entry.Name()); err != nil {
				log.Printf("clean orphaned Job work directory: %v", err)
			}
		}
		// Only empty project directories can be removed. A concurrent Job may
		// have created a new work directory in the meantime.
		_ = os.Remove(projectPath)
	}
}

func (a *Agent) removeInactiveJobWork(project, name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, execution := range a.executions {
		if execution.job.Metadata.Namespace == project && execution.job.Metadata.Name == name {
			return nil
		}
	}
	path := filepath.Join(workDir(a.cfg.RootDir), project, name)
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}
