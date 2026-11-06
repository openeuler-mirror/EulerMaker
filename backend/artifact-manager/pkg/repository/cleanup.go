package repository

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"artifact-manager/pkg/fileutil"
)

// CheckBuildActive rejects submissions after a durable stop, including after repository deletion or restart.
func CheckBuildActive(root, project, buildName string) error {
	marker, err := fileutil.OpenDirectory(root, "repositories", project, ".stopped", buildName)
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
func (m *Manager) StopBuild(project, buildName string) (bool, error) {
	if !validIdentifier(project) || !validIdentifier(buildName) {
		return false, &Error{Code: "InvalidRequest", Status: http.StatusBadRequest}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	parts := []string{"repositories", project, ".stopped", buildName}
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
		if record.Project == project && record.BuildName == buildName &&
			(record.State == RepositoryCreating || record.State == RepositoryDeleting || m.running[uid]) {
			return false, nil
		}
	}
	return true, nil
}

// DeleteBuild removes all process repository versions and records belonging to one build.
// The caller must stop new submissions before cleanup. False means existing work is still active.
func (m *Manager) DeleteBuild(project, buildName string) (bool, error) {
	return m.withBuildCleanup(project, buildName, func() error {
		arches, err := fileutil.DirectoryNames(m.root, "repositories", project)
		if err != nil {
			return &Error{Code: "RepositoryCleanupFailed", Status: http.StatusServiceUnavailable, Retryable: true}
		}
		for _, arch := range arches {
			if arch == ".stopped" {
				continue
			}
			parent, err := fileutil.OpenDirectory(m.root, "repositories", project, arch, "history")
			if err != nil {
				return &Error{Code: "RepositoryCleanupFailed", Status: http.StatusServiceUnavailable, Retryable: true}
			}
			if parent == nil {
				continue
			}
			err = fileutil.RemoveEntry(parent, buildName, true)
			parent.Close()
			if err != nil {
				return &Error{Code: "RepositoryCleanupFailed", Status: http.StatusServiceUnavailable, Retryable: true}
			}
		}
		return nil
	})
}

func buildCleanupKey(project, buildName string) string {
	return project + "/" + buildName
}

// withBuildCleanup reserves one build under the mutex, then performs filesystem work without it.
func (m *Manager) withBuildCleanup(project, buildName string, cleanup func() error) (done bool, err error) {
	if !validIdentifier(project) || !validIdentifier(buildName) {
		return false, &Error{Code: "InvalidRequest", Status: http.StatusBadRequest}
	}
	scope := buildCleanupKey(project, buildName)
	m.mu.Lock()
	if m.cleaningBuilds[scope] {
		m.mu.Unlock()
		return false, nil
	}
	for uid, record := range m.records {
		if record.Project == project && record.BuildName == buildName &&
			(record.State == RepositoryCreating || record.State == RepositoryDeleting || m.running[uid]) {
			m.mu.Unlock()
			return false, nil
		}
	}
	if m.cleaningBuilds == nil {
		m.cleaningBuilds = make(map[string]bool)
	}
	m.cleaningBuilds[scope] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.cleaningBuilds, scope)
		if !done {
			return
		}
		// Evict only this build's entries after all filesystem operations succeed.
		projectDir := filepath.Join(m.root, "repositories", project)
		for uid, path := range m.metadataPaths {
			relative, err := filepath.Rel(projectDir, path)
			parts := strings.Split(filepath.ToSlash(relative), "/")
			if err == nil && len(parts) == 5 && parts[0] != ".." &&
				parts[1] == "history" && parts[2] == buildName && parts[3] == ".records" {
				delete(m.metadataPaths, uid)
				delete(m.records, uid)
				delete(m.queued, uid)
			}
		}
	}()
	if err := cleanup(); err != nil {
		return false, err
	}
	return true, nil
}
