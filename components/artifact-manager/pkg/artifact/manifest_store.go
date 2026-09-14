package artifact

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func manifestDigest(files []ManifestFile) string {
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
		a := s.artifacts[f.ArtifactID]
		if a == nil || a.State != Completed || a.Project != project || a.JobName != job || a.JobUID != r.JobUID || a.RelativePath != f.RelativePath || a.Category != f.Category || a.Size != f.Size || a.SHA256 != f.SHA256 {
			return nil, errors.New("artifact mismatch")
		}
	}
	k := manifestKey(project, job, r.JobUID)
	digest := manifestDigest(r.Files)
	if old := s.manifests[k]; old != nil {
		if old.Digest == digest && old.State == ManifestCompleted {
			return old, nil
		}
		return nil, errors.New("manifest conflict")
	}
	now := time.Now().UTC()
	m := &JobUploadManifest{SchemaVersion: 1, Project: project, JobName: job, JobUID: r.JobUID, RunnerName: runner, Files: r.Files, Digest: digest, State: ManifestCompleted, CreatedAt: now, UpdatedAt: now, CompletedAt: &now}
	path := filepath.Join(s.root, ".metadata/jobs", project, r.JobUID, "manifest.json")
	if err := atomicJSON(path, m); err != nil {
		return nil, err
	}
	s.manifests[k] = m
	return m, nil
}
func (s *Store) GetManifest(p, j, u string) (*JobUploadManifest, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.manifests[manifestKey(p, j, u)]
	if !ok {
		return nil, false
	}
	return m, true
}
