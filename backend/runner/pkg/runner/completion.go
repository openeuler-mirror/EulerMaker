package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"ebs-runner/pkg/artifact"
	"ebs-runner/pkg/gateway"
)

type completionRecord struct {
	Version      int               `json:"version"`
	UID          string            `json:"uid"`
	Runner       string            `json:"runner"`
	Status       gateway.JobStatus `json:"status"`
	FinishedAt   time.Time         `json:"finishedAt"`
	LogsPending  bool              `json:"logsPending"`
	UploadFailed bool              `json:"uploadFailed"`
}

// Retry persisted completions even when recovering API permissions produces no
// watch event. The normal execution registration serializes this with watch.
func (a *Agent) recoverCompletionsLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			list, err := a.client.ListAssignedJobs(checkCtx, a.cfg.Name)
			cancel()
			if err != nil || list == nil {
				continue
			}
			for _, job := range list.Items {
				if job.Status.Phase != "Running" {
					continue
				}
				record, err := loadCompletion(a.cfg.RootDir, job)
				if err != nil {
					log.Printf("load pending Job completion %s: %v", jobKey(job), err)
				} else if record != nil {
					a.handleEvent(ctx, gateway.WatchEvent{Type: "MODIFIED", Object: job})
				}
			}
		}
	}
}

func completionPath(root string, job gateway.JobResource) (string, error) {
	if err := artifact.ValidateJobIdentity(job); err != nil {
		return "", err
	}
	if !artifact.ValidLocalPathSegment(job.Metadata.UID) {
		return "", errors.New("invalid completion Job UID")
	}
	return filepath.Join(root, "executions", job.Metadata.Namespace, job.Metadata.Name, job.Metadata.UID+".json"), nil
}

func saveCompletion(root string, job gateway.JobResource, record completionRecord) error {
	path, err := completionPath(root, job)
	if err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".completion-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func loadCompletion(root string, job gateway.JobResource) (*completionRecord, error) {
	path, err := completionPath(root, job)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record completionRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, err
	}
	if record.Version != 1 || record.UID != job.Metadata.UID || record.Runner != job.Status.Runner || record.Status.Runner != job.Status.Runner || record.Status.Stage != "PostRun" || (record.Status.Phase != "Running" && record.Status.Phase != "Succeeded" && record.Status.Phase != "Failed") {
		return nil, errors.New("invalid completion identity or status")
	}
	return &record, nil
}

func recordExecution(root string, job gateway.JobResource, executionErr error, logsPending bool) error {
	status := job.Status
	result, resultErr := readJobResult(filepath.Join(workDir(root), job.Metadata.Namespace, job.Metadata.Name, "job-result.json"))
	status.Result = result
	executionErr = errors.Join(executionErr, resultErr)
	if result != nil && result.Status == "BuildFailed" && executionErr == nil {
		executionErr = errors.New("build script reported failure")
	}
	status.Phase, status.Stage, status.EndTime = "Running", "PostRun", nil
	status.Message = ""
	if executionErr != nil {
		status.Message = executionErr.Error()
	}
	return saveCompletion(root, job, completionRecord{Version: 1, UID: job.Metadata.UID, Runner: job.Status.Runner, Status: status, FinishedAt: time.Now().UTC(), LogsPending: logsPending})
}

func (a *Agent) resumeCompletion(ctx context.Context, key string, job gateway.JobResource, record *completionRecord) {
	defer a.finishJob(key)
	if record.LogsPending && a.logFactory == nil {
		log.Printf("cannot recover log for Job %s: log factory unavailable", key)
		return
	}
	if err := a.writeCompletionStatus(ctx, &job, record.Status); err != nil {
		log.Printf("recover Job completion %s: %v", key, err)
		return
	}
	if terminalJob(record.Status.Phase) {
		a.cleanCompletion(job, record.UploadFailed)
		return
	}
	a.cleanupJobWork(job)
	var executionErr error
	if record.Status.Message != "" {
		executionErr = errors.New(record.Status.Message)
	}
	a.finalizeArtifacts(ctx, job, record.Status, filepath.Join(resultRoot(a.cfg.RootDir), job.Metadata.Namespace, job.Metadata.Name), executionErr)
}

func (a *Agent) completeLog(ctx context.Context, job gateway.JobResource) error {
	sink, err := a.logFactory.Open(job)
	if err != nil {
		return fmt.Errorf("open completed log: %w", err)
	}
	defer sink.Abort()
	if _, err := sink.Complete(ctx); err != nil {
		return fmt.Errorf("complete artifact log: %w", err)
	}
	return nil
}

func (a *Agent) writeCompletionStatus(ctx context.Context, job *gateway.JobResource, status gateway.JobStatus) error {
	if status.StartTime != nil {
		value := status.StartTime.UTC().Truncate(time.Second)
		status.StartTime = &value
	}
	if status.EndTime != nil {
		value := status.EndTime.UTC().Truncate(time.Second)
		status.EndTime = &value
	}
	backoff := 200 * time.Millisecond
	canWrite := true
	for {
		if canWrite {
			if err := a.writeJobStatus(ctx, job, status); err == nil {
				return nil
			} else {
				var statusErr gateway.StatusError
				if errors.As(err, &statusErr) && (statusErr.Code == 400 || statusErr.Code == 403 || statusErr.Code == 422) {
					return err
				}
			}
		}
		canWrite = false
		if err := ctx.Err(); err != nil {
			return err
		}
		checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		current, err := a.client.GetJob(checkCtx, job.Metadata.Namespace, job.Metadata.Name)
		cancel()
		var statusErr gateway.StatusError
		if errors.As(err, &statusErr) && statusErr.Code == 404 {
			a.cancelExecution(job.Metadata.UID)
			return err
		}
		if err == nil && current != nil {
			if current.Metadata.UID != job.Metadata.UID || current.Status.Runner != job.Status.Runner || current.Metadata.DeletionTimestamp != nil {
				a.cancelExecution(job.Metadata.UID)
				return errors.New("Job completion ownership changed")
			}
			if reflect.DeepEqual(current.Status, status) {
				*job = *current
				return nil
			}
			if terminalJob(current.Status.Phase) {
				a.cancelExecution(job.Metadata.UID)
				return fmt.Errorf("Job already %s", current.Status.Phase)
			}
			if current.Status.Phase != "Running" {
				return errors.New("Job is no longer Running")
			}
			*job = *current
			canWrite = true
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

func (a *Agent) cleanCompletion(job gateway.JobResource, uploadFailed bool) {
	var err error
	if uploadFailed {
		err = a.cleanup.MarkFailure(job)
	} else {
		err = a.cleanup.MarkSuccess(job)
	}
	if err != nil {
		log.Printf("clean completed Job state: %v", err)
		return
	}
	path, err := completionPath(a.cfg.RootDir, job)
	if err == nil {
		if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("remove Job completion: %v", err)
		}
	}
}
