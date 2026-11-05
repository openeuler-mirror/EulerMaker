package repository

import (
	"net/http"
	"path/filepath"

	"artifact-manager/pkg/fileutil"
)

// DeleteBuild removes all process repository versions and records belonging to one build.
// The caller must stop new submissions before cleanup. False means existing work is still active.
func (m *Manager) DeleteBuild(project, arch, buildName string) (bool, error) {
	if !validIdentifier(project) || !validIdentifier(arch) || !validIdentifier(buildName) {
		return false, &Error{Code: "InvalidRequest", Status: http.StatusBadRequest}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for uid, record := range m.records {
		if record.Project == project && record.TargetArch == arch && record.BuildName == buildName &&
			(record.State == RepositoryCreating || record.State == RepositoryDeleting || m.running[uid]) {
			return false, nil
		}
	}
	buildDir := filepath.Join(m.root, "repositories", project, arch, "history", buildName)
	parent, err := fileutil.OpenDirectory(m.root, "repositories", project, arch, "history")
	if err != nil {
		return false, &Error{Code: "RepositoryCleanupFailed", Status: http.StatusServiceUnavailable, Retryable: true}
	}
	if parent != nil {
		defer parent.Close()
		if err := fileutil.RemoveEntry(parent, buildName, true); err != nil {
			return false, &Error{Code: "RepositoryCleanupFailed", Status: http.StatusServiceUnavailable, Retryable: true}
		}
	}
	// Evict only this build's index entries, including records already removed by an interrupted cleanup.
	for uid, path := range m.metadataPaths {
		if filepath.Dir(path) == filepath.Join(buildDir, ".records") {
			delete(m.metadataPaths, uid)
			delete(m.records, uid)
			delete(m.queued, uid)
		}
	}
	return true, nil
}
