package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type cleanupMarker struct {
	SchemaVersion int       `json:"schemaVersion"`
	Project       string    `json:"project"`
	JobUID        string    `json:"jobUID"`
	Outcome       string    `json:"outcome"`
	CreatedAt     time.Time `json:"createdAt"`
	NotBefore     time.Time `json:"notBefore"`
}

type ArtifactCleanupManager struct {
	RootDir         string
	FailedRetention time.Duration
	Now             func() time.Time
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
		SchemaVersion: 1, Project: job.Metadata.Namespace, JobUID: job.Metadata.UID,
		Outcome: outcome, CreatedAt: now, NotBefore: now.Add(retention),
	}
	path := m.markerPath(marker.Project, marker.JobUID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	if err := writeAtomicFile(path, data); err != nil {
		return err
	}
	if retention <= 0 {
		return m.clean(marker, path, now)
	}
	return nil
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
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
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
		if err := json.Unmarshal(data, &marker); err != nil || marker.SchemaVersion != 1 || marker.Project == "" || marker.JobUID == "" {
			return fmt.Errorf("invalid artifact cleanup marker %s", path)
		}
		if path != m.markerPath(marker.Project, marker.JobUID) {
			return fmt.Errorf("artifact cleanup marker identity mismatch: %s", path)
		}
		if now.Before(marker.NotBefore) {
			return nil
		}
		return m.clean(marker, path, now)
	})
}

func (m *ArtifactCleanupManager) sweepAndLog() {
	if err := m.Sweep(); err != nil {
		log.Printf("artifact cleanup sweep failed: %v", err)
	}
}

func (m *ArtifactCleanupManager) clean(marker cleanupMarker, markerPath string, now time.Time) error {
	if now.Before(marker.NotBefore) {
		return nil
	}
	if strings.ContainsAny(marker.Project, `/\\`) || strings.ContainsAny(marker.JobUID, `/\\`) {
		return fmt.Errorf("invalid artifact cleanup identity")
	}
	for _, category := range []string{"results", "logs", "uploads"} {
		path := filepath.Join(m.RootDir, category, marker.Project, marker.JobUID)
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

func (m *ArtifactCleanupManager) markerPath(project, uid string) string {
	return filepath.Join(m.RootDir, "cleanup", project, uid+".json")
}
