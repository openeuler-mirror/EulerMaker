package release

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"artifact-manager/pkg/fileutil"
	"artifact-manager/pkg/repository"
)

func releaseStopError(err error) error {
	var typed *repository.Error
	if errors.As(err, &typed) {
		return &Error{Code: typed.Code, Status: typed.Status, Retryable: typed.Retryable}
	}
	return releaseStorageError()
}

// StopBuild blocks submissions and activation before confirming both managers have drained.
// Lock order is release then repository, as in other release operations.
func (m *Manager) StopBuild(project, arch, buildName string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	done, err := m.repositories.StopBuild(project, arch, buildName)
	if err != nil {
		return false, err
	}
	if record := m.records[buildName]; record != nil {
		if record.Project != project || record.TargetArch != arch {
			return false, &Error{Code: "ReleaseIdentityConflict", Status: http.StatusConflict}
		}
		if record.State == ReleaseCreating || record.State == ReleaseDeleting || m.running[buildName] {
			return false, nil
		}
	}
	return done, nil
}

// DeleteBuild removes a build's release, record and abandoned work, but never the current release.
// New submissions must already be stopped by the caller. Active work is reported as incomplete.
func (m *Manager) DeleteBuild(project, arch, buildName string) (bool, error) {
	return m.withBuildCleanup(project, arch, buildName, func() error {
		return m.removeBuildFiles(project, arch, buildName)
	})
}

// withBuildCleanup keeps the build reserved while filesystem work runs outside the global mutex.
func (m *Manager) withBuildCleanup(project, arch, buildName string, cleanup func() error) (done bool, err error) {
	ready, err := m.beginBuildCleanup(project, arch, buildName)
	if err != nil || !ready {
		return false, err
	}
	defer func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.cleaningBuilds, buildName)
		if done {
			delete(m.records, buildName)
			delete(m.metadataPaths, buildName)
			delete(m.queued, buildName)
		}
	}()
	if err := cleanup(); err != nil {
		return false, err
	}
	return true, nil
}

func (m *Manager) beginBuildCleanup(project, arch, buildName string) (bool, error) {
	if !validIdentifier(project) || !validIdentifier(arch) || !validIdentifier(buildName) {
		return false, &Error{Code: "InvalidRequest", Status: http.StatusBadRequest}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cleaningBuilds[buildName] {
		return false, nil
	}
	targetDir := filepath.Join(m.root, "repositories", project, arch)
	target, err := fileutil.OpenDirectory(m.root, "repositories", project, arch)
	if err != nil {
		return false, releaseStorageError()
	}
	if target != nil {
		defer target.Close()
		current, err := target.Readlink("current")
		if err != nil && !os.IsNotExist(err) {
			return false, releaseStorageError()
		}
		if current == filepath.ToSlash(filepath.Join("releases", buildName)) {
			return false, &Error{Code: "ReleaseInUse", Status: http.StatusConflict}
		}
	}
	recordPath := filepath.Join(targetDir, "releases", ".records", buildName+".json")
	if path := m.metaPath(buildName); path != "" && path != recordPath {
		return false, &Error{Code: "ReleaseIdentityConflict", Status: http.StatusConflict}
	}
	if record := m.records[buildName]; record != nil &&
		(record.State == ReleaseCreating || record.State == ReleaseDeleting || m.running[buildName]) {
		return false, nil
	}
	if m.cleaningBuilds == nil {
		m.cleaningBuilds = make(map[string]bool)
	}
	// Build names are globally unique in release metadata; also block activation and retention for this name.
	m.cleaningBuilds[buildName] = true
	return true, nil
}

func (m *Manager) removeBuildFiles(project, arch, buildName string) error {
	for _, dir := range []string{".work", ".trash", ".records"} {
		parent, err := fileutil.OpenDirectory(m.root, "repositories", project, arch, "releases", dir)
		if err != nil {
			return releaseStorageError()
		}
		if parent != nil {
			prefix := buildName
			if dir == ".records" {
				prefix += ".json.tmp"
			}
			err := removeBuildTemporary(parent, prefix)
			parent.Close()
			if err != nil {
				return releaseStorageError()
			}
		}
	}
	for _, entry := range []struct {
		parents   []string
		name      string
		recursive bool
	}{
		{[]string{"repositories", project, arch, "releases"}, buildName, true},
		{[]string{"repositories", project, arch, "releases", ".records"}, buildName + ".json", false},
	} {
		parent, err := fileutil.OpenDirectory(m.root, entry.parents...)
		if err != nil {
			return releaseStorageError()
		}
		if parent != nil {
			err := fileutil.RemoveEntry(parent, entry.name, entry.recursive)
			parent.Close()
			if err != nil {
				return releaseStorageError()
			}
		}
	}
	return nil
}

func removeBuildTemporary(dir *os.Root, buildName string) error {
	file, err := dir.Open(".")
	if err != nil {
		return err
	}
	defer file.Close()
	entries, err := file.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		// Random suffixes and the fixed "deleting" suffix contain no hyphens; match the full build name.
		name := entry.Name()
		separator := strings.LastIndexByte(name, '-')
		if separator < 0 || name[:separator] != buildName {
			continue
		}
		if err := fileutil.RemoveEntry(dir, name, true); err != nil {
			return err
		}
	}
	return nil
}
