package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"ebs-runner/pkg/gateway"
)

const artifactCleanupDiskThreshold = 0.85

type cleanupMarker struct {
	SchemaVersion int       `json:"schemaVersion"`
	Project       string    `json:"project"`
	JobName       string    `json:"jobName"`
	Outcome       string    `json:"outcome"`
	CreatedAt     time.Time `json:"createdAt"`
	NotBefore     time.Time `json:"notBefore"`
}

type CleanupManager struct {
	RootDir         string
	FailedRetention time.Duration
	Now             func() time.Time
	DiskUsage       func(string) (float64, error)
	writeMarker     func(string, []byte) error
}

func (m *CleanupManager) MarkSuccess(job gateway.JobResource) error {
	return m.mark(job, "Completed", 0)
}

func (m *CleanupManager) MarkFailure(job gateway.JobResource) error {
	return m.mark(job, "Failed", m.FailedRetention)
}

// RecoverTerminal fills the gap between a durable terminal status and its local cleanup marker.
// The caller must exclude active execution and uploading for this Job.
func (m *CleanupManager) RecoverTerminal(job gateway.JobResource) error {
	if err := ValidateJobIdentity(job); err != nil {
		return err
	}
	if job.Status.Phase == "Succeeded" {
		return m.Remove(job.Metadata.Namespace, job.Metadata.Name)
	}
	if job.Status.Phase != "Failed" {
		return nil
	}
	now := m.now()
	path := m.markerPath(job.Metadata.Namespace, job.Metadata.Name)
	marker, err := m.readMarker(path)
	if os.IsNotExist(err) {
		if job.Status.EndTime == nil || job.Status.EndTime.IsZero() {
			return fmt.Errorf("cannot recover cleanup for %s/%s: Failed Job has no endTime", job.Metadata.Namespace, job.Metadata.Name)
		}
		end := job.Status.EndTime.UTC()
		marker = cleanupMarker{
			SchemaVersion: 1, Project: job.Metadata.Namespace, JobName: job.Metadata.Name,
			Outcome: "Failed", CreatedAt: end, NotBefore: end.Add(m.FailedRetention),
		}
		data, err := json.Marshal(marker)
		if err != nil {
			return err
		}
		if err := m.writeMarkerWithReclaim(marker, data, now); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return m.clean(marker, path, now, false)
}

func (m *CleanupManager) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

// Remove discards stopped Job data without requiring a new marker on a potentially full disk.
// The caller must confirm that execution and all local upload workers have exited.
func (m *CleanupManager) Remove(project, name string) error {
	if !ValidLocalPathSegment(project) || !ValidLocalPathSegment(name) {
		return fmt.Errorf("invalid artifact cleanup identity")
	}
	marker := cleanupMarker{Project: project, JobName: name}
	if err := m.clean(marker, m.markerPath(project, name), time.Now(), true); err != nil {
		return err
	}
	if err := os.Remove(m.markerPath(project, name) + ".tmp"); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (m *CleanupManager) mark(job gateway.JobResource, outcome string, retention time.Duration) error {
	if err := ValidateJobIdentity(job); err != nil {
		return err
	}
	now := m.now()
	marker := cleanupMarker{
		SchemaVersion: 1, Project: job.Metadata.Namespace, JobName: job.Metadata.Name,
		Outcome: outcome, CreatedAt: now, NotBefore: now.Add(retention),
	}
	path := m.markerPath(marker.Project, marker.JobName)
	data, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	if err := m.writeMarkerWithReclaim(marker, data, now); err != nil {
		return err
	}
	if retention <= 0 {
		return m.clean(marker, path, now, false)
	}
	return nil
}

func (m *CleanupManager) writeMarkerWithReclaim(marker cleanupMarker, data []byte, now time.Time) error {
	path := m.markerPath(marker.Project, marker.JobName)
	write := m.writeMarker
	if write == nil {
		write = writeAtomicFile
	}
	tryWrite := func() error {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		return write(path, data)
	}
	err := tryWrite()
	if !isNoSpace(err) {
		return err
	}
	candidates, cleanupErr := m.cleanupCandidates()
	for _, item := range candidates {
		if item.path == path || item.marker.Outcome != "Failed" {
			continue
		}
		log.Printf("artifact cleanup to make room for marker: project=%s job=%s", item.marker.Project, item.marker.JobName)
		if cleanErr := m.clean(item.marker, item.path, now, true); cleanErr != nil {
			cleanupErr = errors.Join(cleanupErr, cleanErr)
			continue
		}
		err = tryWrite()
		if !isNoSpace(err) {
			return errors.Join(err, cleanupErr)
		}
	}
	// The Job is already terminal. If no older failed result can make room for
	// its marker, reclaim this Job as the last resort rather than leave an
	// untracked result directory on a full filesystem.
	log.Printf("artifact cleanup to make room for marker: project=%s job=%s (current Job)", marker.Project, marker.JobName)
	if removeErr := os.Remove(path + ".tmp"); removeErr != nil && !os.IsNotExist(removeErr) {
		return errors.Join(err, cleanupErr, removeErr)
	}
	if cleanErr := m.clean(marker, path, now, true); cleanErr != nil {
		return errors.Join(err, cleanupErr, cleanErr)
	}
	return cleanupErr
}

func isNoSpace(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}

type cleanupCandidate struct {
	marker cleanupMarker
	path   string
}

func (m *CleanupManager) readMarker(path string) (cleanupMarker, error) {
	var marker cleanupMarker
	data, err := os.ReadFile(path)
	if err != nil {
		return marker, err
	}
	if err := json.Unmarshal(data, &marker); err != nil || marker.SchemaVersion != 1 ||
		!ValidLocalPathSegment(marker.Project) || !ValidLocalPathSegment(marker.JobName) ||
		(marker.Outcome != "Failed" && marker.Outcome != "Completed") {
		return marker, fmt.Errorf("invalid artifact cleanup marker %s", path)
	}
	if path != m.markerPath(marker.Project, marker.JobName) {
		return marker, fmt.Errorf("artifact cleanup marker identity mismatch: %s", path)
	}
	return marker, nil
}

// Return every valid candidate even when individual records cannot be read.
func (m *CleanupManager) cleanupCandidates() ([]cleanupCandidate, error) {
	var candidates []cleanupCandidate
	var scanErr error
	err := filepath.WalkDir(filepath.Join(m.RootDir, "cleanup"), func(path string, entry os.DirEntry, walkErr error) error {
		if os.IsNotExist(walkErr) {
			return nil
		}
		if walkErr != nil {
			scanErr = errors.Join(scanErr, walkErr)
			return nil
		}
		if entry.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		marker, err := m.readMarker(path)
		if os.IsNotExist(err) {
			return nil // Concurrent cleanup already removed this record.
		}
		if err != nil {
			scanErr = errors.Join(scanErr, err)
			return nil
		}
		candidates = append(candidates, cleanupCandidate{marker: marker, path: path})
		return nil
	})
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].marker.CreatedAt.Equal(candidates[j].marker.CreatedAt) {
			return candidates[i].path < candidates[j].path
		}
		return candidates[i].marker.CreatedAt.Before(candidates[j].marker.CreatedAt)
	})
	return candidates, errors.Join(scanErr, err)
}

func (m *CleanupManager) Run(ctx context.Context) {
	m.sweepAndLog()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.sweepAndLog()
		case <-ctx.Done():
			return
		}
	}
}

func (m *CleanupManager) Sweep() error {
	now := m.now()
	candidates, cleanupErr := m.cleanupCandidates()
	var pending []cleanupCandidate
	for _, item := range candidates {
		if now.Before(item.marker.NotBefore) {
			if item.marker.Outcome == "Failed" {
				pending = append(pending, item)
			}
			continue
		}
		cleanupErr = errors.Join(cleanupErr, m.clean(item.marker, item.path, now, false))
	}
	if len(pending) == 0 {
		return cleanupErr
	}
	usage := m.DiskUsage
	if usage == nil {
		usage = filesystemUsage
	}
	used, err := usage(m.RootDir)
	if err != nil {
		return errors.Join(cleanupErr, fmt.Errorf("check artifact cleanup disk usage: %w", err))
	}
	if used <= artifactCleanupDiskThreshold {
		return cleanupErr
	}
	for _, item := range pending {
		log.Printf("artifact cleanup under disk pressure: project=%s job=%s usage=%.1f%%", item.marker.Project, item.marker.JobName, used*100)
		cleanupErr = errors.Join(cleanupErr, m.clean(item.marker, item.path, now, true))
		used, err = usage(m.RootDir)
		if err != nil {
			return errors.Join(cleanupErr, fmt.Errorf("check artifact cleanup disk usage: %w", err))
		}
		if used <= artifactCleanupDiskThreshold {
			break
		}
	}
	return cleanupErr
}

func filesystemUsage(path string) (float64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	if stat.Blocks == 0 {
		return 0, fmt.Errorf("filesystem at %s has no blocks", path)
	}
	return 1 - float64(stat.Bavail)/float64(stat.Blocks), nil
}

func (m *CleanupManager) sweepAndLog() {
	if err := m.Sweep(); err != nil {
		log.Printf("artifact cleanup sweep failed: %v", err)
	}
}

func (m *CleanupManager) clean(marker cleanupMarker, markerPath string, now time.Time, diskPressure bool) error {
	if !diskPressure && now.Before(marker.NotBefore) {
		return nil
	}
	name := marker.JobName
	if strings.ContainsAny(marker.Project, `/\\`) || strings.ContainsAny(name, `/\\`) || name == "." || name == ".." {
		return fmt.Errorf("invalid artifact cleanup identity")
	}
	for _, category := range []string{"results", "logs", "uploads", "work"} {
		path := filepath.Join(m.RootDir, category, marker.Project, name)
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove(filepath.Dir(markerPath))
	return nil
}

func (m *CleanupManager) markerPath(project, name string) string {
	return filepath.Join(m.RootDir, "cleanup", project, name+".json")
}
