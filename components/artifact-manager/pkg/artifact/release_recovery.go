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
		digest := index.ReleaseDigest
		if record.State == ReleaseReady {
			// Ready releases were verified before activation. Avoid re-reading every
			// RPM on each restart; only check the persisted digest against the index.
			if !validHash(digest) || record.ReleaseDigest != digest {
				record.State, record.ContentURL, record.UpdatedAt = ReleaseFailed, "", now
				record.Failure = &FailureInfo{Code: "ReleaseContentInvalid", Message: "ReleaseContentInvalid", Time: now}
				if err := m.persist(record); err != nil {
					return err
				}
				continue
			}
		} else {
			var err error
			digest, err = digestReleaseDirectory(m.releasePath(record))
			if err != nil || digest != index.ReleaseDigest {
				continue
			}
		}
		if record.State == ReleaseCreating {
			record.State, record.ReleaseDigest, record.UpdatedAt = ReleasePrepared, digest, now
			if err := m.persist(record); err != nil {
				return err
			}
		}
		current := filepath.Join(m.releaseTargetDir(record), "current")
		if target, err := os.Readlink(current); err == nil && target == filepath.ToSlash(filepath.Join("releases", name)) {
			record.State, record.ContentURL, record.UpdatedAt, record.CompletedAt = ReleaseReady, releaseContentURL(record), now, &now
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
	rootInfo, err := os.Lstat(path)
	if err != nil {
		return index, err
	}
	if !rootInfo.IsDir() {
		return index, fmt.Errorf("invalid release directory %s", path)
	}
	indexPath := filepath.Join(path, "release.json")
	indexInfo, err := os.Lstat(indexPath)
	if err != nil {
		return index, err
	}
	if !indexInfo.Mode().IsRegular() {
		return index, fmt.Errorf("invalid release index %s", indexPath)
	}
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return index, err
	}
	err = json.Unmarshal(data, &index)
	return index, err
}
