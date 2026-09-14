package artifact

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func (m *releaseManager) load() error {
	entries, err := os.ReadDir(filepath.Join(m.root, ".metadata/releases"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		var record ReleaseRecord
		data, err := os.ReadFile(filepath.Join(m.root, ".metadata/releases", entry.Name()))
		if err != nil || json.Unmarshal(data, &record) != nil || record.BuildName == "" {
			return fmt.Errorf("load release metadata %s: invalid record", entry.Name())
		}
		m.records[record.BuildName] = &record
	}
	return nil
}

func (m *releaseManager) recover(workTTL time.Duration) error {
	now := time.Now().UTC()
	for name, record := range m.records {
		if record.State != ReleaseCreating && record.State != ReleasePrepared && record.State != ReleaseReady {
			continue
		}
		index, err := readReleaseIndex(m.releasePath(record))
		if err != nil || index.BuildName != name || index.RequestDigest != record.RequestDigest {
			if record.State == ReleaseReady {
				record.State, record.ContentURL, record.UpdatedAt = ReleaseFailed, "", now
				record.Failure = &FailureInfo{Code: "ReleaseContentMissing", Message: "ReleaseContentMissing", Time: now}
				if err := m.persist(record); err != nil {
					return err
				}
			}
			continue
		}
		digest, err := digestReleaseDirectory(m.releasePath(record))
		if err != nil || digest != index.ReleaseDigest {
			if record.State == ReleaseReady {
				record.State, record.ContentURL, record.UpdatedAt = ReleaseFailed, "", now
				record.Failure = &FailureInfo{Code: "ReleaseContentInvalid", Message: "ReleaseContentInvalid", Time: now}
				if err := m.persist(record); err != nil {
					return err
				}
			}
			continue
		}
		if record.State == ReleaseCreating {
			record.State, record.ReleaseDigest, record.PackageCount, record.UpdatedAt = ReleasePrepared, digest, countReleasePackages(m.releasePath(record)), now
			if err := m.persist(record); err != nil {
				return err
			}
		}
		current := filepath.Join(m.root, "repositories", record.Project, record.TargetArch, "current")
		if target, err := os.Readlink(current); err == nil && target == filepath.ToSlash(filepath.Join("releases", name)) {
			record.State, record.ContentURL, record.UpdatedAt, record.CompletedAt = ReleaseReady, "/repositories/"+record.Project+"/"+record.TargetArch+"/", now, &now
			if err := m.persist(record); err != nil {
				return err
			}
		}
	}
	entries, err := os.ReadDir(filepath.Join(m.root, ".release-work"))
	if err != nil {
		return err
	}
	cutoff := now.Add(-workTTL)
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && info.ModTime().Before(cutoff) {
			_ = os.RemoveAll(filepath.Join(m.root, ".release-work", entry.Name()))
		}
	}
	return nil
}

func readReleaseIndex(path string) (releaseIndex, error) {
	var index releaseIndex
	data, err := os.ReadFile(filepath.Join(path, "release.json"))
	if err != nil {
		return index, err
	}
	err = json.Unmarshal(data, &index)
	return index, err
}

func countReleasePackages(path string) int {
	entries, _ := os.ReadDir(filepath.Join(path, "Packages"))
	return len(entries)
}
