package repository

import (
	"net/http"
	"os"
	"path/filepath"

	"artifact-manager/pkg/fileutil"
)

// CheckBuildActive rejects submissions after a durable stop, including after repository deletion or restart.
func CheckBuildActive(root, project, arch, buildName string) error {
	marker, err := fileutil.OpenDirectory(root, "repositories", project, arch, ".stopped", buildName)
	if err != nil {
		return &Error{Code: "RepositoryStorageUnavailable", Status: http.StatusServiceUnavailable, Retryable: true}
	}
	if marker != nil {
		marker.Close()
		return &Error{Code: "BuildStopped", Status: http.StatusConflict}
	}
	return nil
}

// StopBuild serializes the durable fence with Submit, then waits for already accepted tasks to finish.
func (m *Manager) StopBuild(project, arch, buildName string) (bool, error) {
	if !validIdentifier(project) || !validIdentifier(arch) || !validIdentifier(buildName) {
		return false, &Error{Code: "InvalidRequest", Status: http.StatusBadRequest}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	parts := []string{"repositories", project, arch, ".stopped", buildName}
	for i, part := range parts {
		parent, err := fileutil.OpenDirectory(m.root, parts[:i]...)
		if err != nil || parent == nil {
			return false, &Error{Code: "RepositoryStorageUnavailable", Status: http.StatusServiceUnavailable, Retryable: true}
		}
		err = parent.Mkdir(part, 0750)
		if os.IsExist(err) {
			err = nil
		}
		if err == nil {
			var dir *os.File
			dir, err = parent.Open(".")
			if err == nil {
				err = dir.Sync()
				dir.Close()
			}
		}
		parent.Close()
		if err != nil {
			return false, &Error{Code: "RepositoryStorageUnavailable", Status: http.StatusServiceUnavailable, Retryable: true}
		}
	}
	// Also verify the final component; a file or symlink must not count as a persisted fence.
	marker, err := fileutil.OpenDirectory(m.root, parts...)
	if err != nil || marker == nil {
		return false, &Error{Code: "RepositoryStorageUnavailable", Status: http.StatusServiceUnavailable, Retryable: true}
	}
	marker.Close()
	for uid, record := range m.records {
		if record.Project == project && record.TargetArch == arch && record.BuildName == buildName &&
			(record.State == RepositoryCreating || record.State == RepositoryDeleting || m.running[uid]) {
			return false, nil
		}
	}
	return true, nil
}

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
