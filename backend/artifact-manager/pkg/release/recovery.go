package release

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func (m *Manager) scanRecords(visit func(*ReleaseRecord) error) error {
	entries, err := os.ReadDir(filepath.Join(m.root, ".metadata/releases"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		name := entry.Name()[:len(entry.Name())-len(".json")]
		record, err := m.readRecord(name)
		if err != nil {
			return fmt.Errorf("load release metadata %s: %w", entry.Name(), err)
		}
		if record != nil {
			if err := visit(record); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) recover(workTTL time.Duration) error {
	now := time.Now().UTC()
	if err := m.scanRecords(func(record *ReleaseRecord) error {
		if err := m.recoverRecord(record, now); err != nil {
			return err
		}
		m.cacheActive(record)
		return nil
	}); err != nil {
		return err
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

func (m *Manager) recoverRecord(record *ReleaseRecord, now time.Time) error {
	if record.State != ReleaseCreating && record.State != ReleasePrepared && record.State != ReleaseReady {
		return nil
	}
	index, err := readReleaseIndex(m.Path(record))
	if err != nil || index.BuildName != record.BuildName || index.RequestDigest != record.RequestDigest {
		if record.State == ReleaseReady {
			record.State, record.UpdatedAt = ReleaseFailed, now
			record.Failure = &FailureInfo{Code: "ReleaseContentMissing", Message: "ReleaseContentMissing", Time: now}
			return m.persist(record)
		}
		return nil
	}
	digest := index.ReleaseDigest
	if record.State == ReleaseReady {
		// Ready releases need only an index check, not a full RPM digest calculation.
		if !validHash(digest) || record.ReleaseDigest != digest {
			record.State, record.UpdatedAt = ReleaseFailed, now
			record.Failure = &FailureInfo{Code: "ReleaseContentInvalid", Message: "ReleaseContentInvalid", Time: now}
			return m.persist(record)
		}
	} else {
		digest, err = digestReleaseDirectory(m.Path(record))
		if err != nil || digest != index.ReleaseDigest {
			return nil
		}
	}
	if record.State == ReleaseCreating {
		record.State, record.ReleaseDigest, record.UpdatedAt = ReleasePrepared, digest, now
		if err := m.persist(record); err != nil {
			return err
		}
	}
	current := filepath.Join(m.releaseTargetDir(record), "current")
	if target, err := os.Readlink(
		current,
	); err == nil &&
		target == filepath.ToSlash(filepath.Join("releases", record.BuildName)) {
		record.State, record.UpdatedAt, record.CompletedAt = ReleaseReady, now, &now
		return m.persist(record)
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
