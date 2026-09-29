package runner

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

type ArtifactCleanupManager struct {
	RootDir         string
	FailedRetention time.Duration
	Now             func() time.Time
	DiskUsage       func(string) (float64, error)
	writeMarker     func(string, []byte) error
}

func (m *ArtifactCleanupManager) MarkSuccess(job JobResource) error {
	return m.mark(job, "Completed", 0)
}

func (m *ArtifactCleanupManager) MarkFailure(job JobResource) error {
	return m.mark(job, "Failed", m.FailedRetention)
}

func (m *ArtifactCleanupManager) mark(job JobResource, outcome string, retention time.Duration) error {
	if err := validateJobIdentity(job); err != nil {
		return err
	}
	now := time.Now().UTC()
	if m.Now != nil {
		now = m.Now().UTC()
	}
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

func (m *ArtifactCleanupManager) writeMarkerWithReclaim(marker cleanupMarker, data []byte, now time.Time) error {
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
	candidates, scanErr := m.failedCleanupCandidates(path)
	if scanErr != nil {
		return errors.Join(err, scanErr)
	}
	for _, item := range candidates {
		log.Printf("artifact cleanup to make room for marker: project=%s job=%s", item.marker.Project, item.marker.JobName)
		if cleanErr := m.clean(item.marker, item.path, now, true); cleanErr != nil {
			return errors.Join(err, cleanErr)
		}
		err = tryWrite()
		if !isNoSpace(err) {
			return err
		}
	}
	// The Job is already terminal. If no older failed result can make room for
	// its marker, reclaim this Job as the last resort rather than leave an
	// untracked result directory on a full filesystem.
	log.Printf("artifact cleanup to make room for marker: project=%s job=%s (current Job)", marker.Project, marker.JobName)
	if removeErr := os.Remove(path + ".tmp"); removeErr != nil && !os.IsNotExist(removeErr) {
		return errors.Join(err, removeErr)
	}
	if cleanErr := m.clean(marker, path, now, true); cleanErr != nil {
		return errors.Join(err, cleanErr)
	}
	return nil
}

func isNoSpace(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}

type cleanupCandidate struct {
	marker cleanupMarker
	path   string
}

func (m *ArtifactCleanupManager) failedCleanupCandidates(excludePath string) ([]cleanupCandidate, error) {
	var candidates []cleanupCandidate
	err := filepath.WalkDir(filepath.Join(m.RootDir, "cleanup"), func(path string, entry os.DirEntry, walkErr error) error {
		if os.IsNotExist(walkErr) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".json" || path == excludePath {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var marker cleanupMarker
		if err := json.Unmarshal(data, &marker); err != nil || marker.SchemaVersion != 1 || marker.Project == "" || marker.JobName == "" {
			return fmt.Errorf("invalid artifact cleanup marker %s", path)
		}
		if path != m.markerPath(marker.Project, marker.JobName) {
			return fmt.Errorf("artifact cleanup marker identity mismatch: %s", path)
		}
		if marker.Outcome == "Failed" {
			candidates = append(candidates, cleanupCandidate{marker: marker, path: path})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].marker.CreatedAt.Equal(candidates[j].marker.CreatedAt) {
			return candidates[i].path < candidates[j].path
		}
		return candidates[i].marker.CreatedAt.Before(candidates[j].marker.CreatedAt)
	})
	return candidates, nil
}

func (m *ArtifactCleanupManager) Run(ctx context.Context) {
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

func (m *ArtifactCleanupManager) Sweep() error {
	root := filepath.Join(m.RootDir, "cleanup")
	now := time.Now().UTC()
	if m.Now != nil {
		now = m.Now().UTC()
	}
	type pendingCleanup struct {
		marker cleanupMarker
		path   string
	}
	var pending []pendingCleanup
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if os.IsNotExist(walkErr) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var marker cleanupMarker
		if err := json.Unmarshal(data, &marker); err != nil || marker.SchemaVersion != 1 || marker.Project == "" || marker.JobName == "" {
			return fmt.Errorf("invalid artifact cleanup marker %s", path)
		}
		if path != m.markerPath(marker.Project, marker.JobName) {
			return fmt.Errorf("artifact cleanup marker identity mismatch: %s", path)
		}
		if now.Before(marker.NotBefore) {
			if marker.Outcome == "Failed" {
				pending = append(pending, pendingCleanup{marker: marker, path: path})
			}
			return nil
		}
		return m.clean(marker, path, now, false)
	})
	if err != nil || len(pending) == 0 {
		return err
	}
	usage := m.DiskUsage
	if usage == nil {
		usage = filesystemUsage
	}
	used, err := usage(m.RootDir)
	if err != nil {
		return fmt.Errorf("check artifact cleanup disk usage: %w", err)
	}
	if used <= artifactCleanupDiskThreshold {
		return nil
	}
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].marker.CreatedAt.Equal(pending[j].marker.CreatedAt) {
			return pending[i].path < pending[j].path
		}
		return pending[i].marker.CreatedAt.Before(pending[j].marker.CreatedAt)
	})
	for _, item := range pending {
		log.Printf("artifact cleanup under disk pressure: project=%s job=%s usage=%.1f%%", item.marker.Project, item.marker.JobName, used*100)
		if err := m.clean(item.marker, item.path, now, true); err != nil {
			return err
		}
		used, err = usage(m.RootDir)
		if err != nil {
			return fmt.Errorf("check artifact cleanup disk usage: %w", err)
		}
		if used <= artifactCleanupDiskThreshold {
			break
		}
	}
	return nil
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

func (m *ArtifactCleanupManager) sweepAndLog() {
	if err := m.Sweep(); err != nil {
		log.Printf("artifact cleanup sweep failed: %v", err)
	}
}

func (m *ArtifactCleanupManager) clean(marker cleanupMarker, markerPath string, now time.Time, diskPressure bool) error {
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

func (m *ArtifactCleanupManager) markerPath(project, name string) string {
	return filepath.Join(m.RootDir, "cleanup", project, name+".json")
}
