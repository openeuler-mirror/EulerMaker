package runner

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"

	"ebs-runner/pkg/artifact"
	"ebs-runner/pkg/gateway"
)

const jobDataSweepInterval = 10 * time.Minute

type localJobKey struct{ project, name string }

// sweepJobDataLoop runs after container recovery has confirmed old containers are gone.
func (a *Agent) sweepJobDataLoop(ctx context.Context) {
	a.sweepJobData(ctx)
	ticker := time.NewTicker(jobDataSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			a.sweepJobData(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (a *Agent) sweepJobData(ctx context.Context) {
	seen := make(map[localJobKey]bool)
	for _, category := range []string{"work", "results", "logs", "uploads"} {
		root := filepath.Join(a.cfg.RootDir, category)
		projects, err := os.ReadDir(root)
		if err != nil {
			if !os.IsNotExist(err) {
				log.Printf("scan Job directories %s: %v", root, err)
			}
			continue
		}
		for _, project := range projects {
			if ctx.Err() != nil {
				return
			}
			if !project.IsDir() || !artifact.ValidLocalPathSegment(project.Name()) {
				continue
			}
			projectPath := filepath.Join(root, project.Name())
			jobs, err := os.ReadDir(projectPath)
			if err != nil {
				log.Printf("scan Job directory %s: %v", projectPath, err)
				continue
			}
			for _, entry := range jobs {
				if ctx.Err() != nil {
					return
				}
				key := localJobKey{project.Name(), entry.Name()}
				if !entry.IsDir() || !artifact.ValidLocalPathSegment(entry.Name()) || seen[key] {
					continue
				}
				seen[key] = true
				a.cleanupInactiveJob(ctx, key.project, key.name)
			}
			// Remove only empty project directories; concurrent Jobs may create new children.
			_ = os.Remove(projectPath)
		}
	}
}

func (a *Agent) cleanupInactiveJob(ctx context.Context, project, name string) {
	if !artifact.ValidLocalPathSegment(project) || !artifact.ValidLocalPathSegment(name) {
		return
	}
	key := localJobKey{project, name}
	if !a.beginJobCleanup(key) {
		return
	}
	defer a.endJobCleanup(key)
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	job, err := a.client.GetJob(checkCtx, project, name)
	cancel()
	var statusErr gateway.StatusError
	missing := errors.As(err, &statusErr) && statusErr.Code == 404
	if err != nil && !missing {
		log.Printf("confirm Job cleanup %s/%s: %v", project, name, err)
		return
	}
	if !missing && (job == nil || !terminalJob(job.Status.Phase)) {
		return
	}
	cleaner := a.cleanup
	if cleaner == nil {
		cleaner = &artifact.CleanupManager{RootDir: a.cfg.RootDir, FailedRetention: a.cfg.Artifact.Upload.FailedRetention}
	}
	if missing || job.Status.Phase == "Aborted" {
		err = cleaner.Remove(project, name)
	} else {
		err = errors.Join(os.RemoveAll(filepath.Join(workDir(a.cfg.RootDir), project, name)), cleaner.RecoverTerminal(*job))
	}
	if err != nil {
		log.Printf("clean inactive Job %s/%s: %v", project, name, err)
	}
}

// Reserve this name before checking status; filesystem and network operations run without the Agent lock.
func (a *Agent) beginJobCleanup(key localJobKey) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cleaning[key] {
		return false
	}
	for _, execution := range a.executions {
		if execution.job.Metadata.Namespace == key.project && execution.job.Metadata.Name == key.name {
			return false
		}
	}
	if a.cleaning == nil {
		a.cleaning = make(map[localJobKey]bool)
	}
	a.cleaning[key] = true
	return true
}

func (a *Agent) endJobCleanup(key localJobKey) {
	a.mu.Lock()
	delete(a.cleaning, key)
	a.mu.Unlock()
}
