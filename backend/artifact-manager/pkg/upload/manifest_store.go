package upload

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"artifact-manager/pkg/fileutil"
)

var ErrInvalidManifest = errors.New("invalid manifest metadata")

func ManifestDigest(files []ManifestFile) string {
	var b strings.Builder
	b.WriteString("artifact-manifest-v1\n")
	for _, f := range files {
		fmt.Fprintf(&b, "%s\x00%s\x00%s\x00%d\x00%s\x00%t\n", f.RelativePath, f.ArtifactID, f.Category, f.Size, f.SHA256, f.Required)
	}
	return "sha256:" + hashText(b.String())
}

func (s *Store) CompleteManifest(project, job, runner string, r CompleteManifestRequest) (*JobUploadManifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(r.Files) == 0 {
		return nil, errors.New("invalid manifest")
	}
	sort.Slice(r.Files, func(i, j int) bool { return r.Files[i].RelativePath < r.Files[j].RelativePath })
	seenP := map[string]bool{}
	seenA := map[string]bool{}
	for _, f := range r.Files {
		if seenP[f.RelativePath] || seenA[f.ArtifactID] {
			return nil, errors.New("duplicate manifest entry")
		}
		seenP[f.RelativePath] = true
		seenA[f.ArtifactID] = true
		a, err := s.readArtifact(project, job, f.ArtifactID)
		if os.IsNotExist(err) {
			return nil, errors.New("artifact mismatch")
		}
		if err != nil {
			return nil, fmt.Errorf("read manifest artifact: %w", err)
		}
		if a.State != Completed || a.RelativePath != f.RelativePath || a.Category != f.Category || a.Size != f.Size || a.SHA256 != f.SHA256 {
			return nil, errors.New("artifact mismatch")
		}
	}
	digest := ManifestDigest(r.Files)
	old, err := s.readManifest(project, job)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if old != nil {
		if old.Digest == digest && old.State == ManifestCompleted {
			return old, nil
		}
		return nil, errors.New("manifest conflict")
	}
	now := time.Now().UTC()
	m := &JobUploadManifest{SchemaVersion: 1, Project: project, JobName: job, RunnerName: runner, Files: r.Files, Digest: digest, State: ManifestCompleted, CreatedAt: now, UpdatedAt: now, CompletedAt: &now}
	path := filepath.Join(s.root, ".metadata/jobs", project, job, "manifest.json")
	if err := fileutil.AtomicJSON(path, m); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *Store) GetManifest(p, j string) (*JobUploadManifest, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.readManifest(p, j)
}

// readManifest is called with the store lock held; only a missing file means the Job is unsealed.
func (s *Store) readManifest(project, job string) (*JobUploadManifest, error) {
	for _, part := range []string{project, job} {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "/\\\x00") {
			return nil, ErrInvalidManifest
		}
	}
	path := filepath.Join(s.root, ".metadata/jobs", project, job, "manifest.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var manifest JobUploadManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	if manifest.Project != project || manifest.JobName != job {
		return nil, ErrInvalidManifest
	}
	return &manifest, nil
}
