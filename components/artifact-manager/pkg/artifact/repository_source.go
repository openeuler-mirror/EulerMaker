package artifact

import "strings"

type repositoryArtifact struct {
	Metadata Artifact
	Path     string
}

func (s *Store) repositoryArtifacts(project string, refs []ManifestReference) ([]repositoryArtifact, error) {
	s.mu.RLock()
	var result []repositoryArtifact
	for _, ref := range refs {
		manifest := s.manifests[manifestKey(project, ref.JobName, ref.JobUID)]
		if manifest == nil || manifest.State != ManifestCompleted {
			s.mu.RUnlock()
			return nil, &repositoryError{code: "ManifestNotReady", status: 422}
		}
		if manifest.Digest != manifestDigest(append([]ManifestFile(nil), manifest.Files...)) {
			s.mu.RUnlock()
			return nil, &repositoryError{code: "ManifestInvalid", status: 422}
		}
		for _, file := range manifest.Files {
			if file.Category != CategoryArtifact || !strings.HasPrefix(file.RelativePath, "packages/") || !strings.HasSuffix(strings.ToLower(file.RelativePath), ".rpm") {
				continue
			}
			artifact := s.artifacts[file.ArtifactID]
			if artifact == nil || artifact.State != Completed || artifact.Project != project || artifact.JobUID != ref.JobUID || artifact.Size != file.Size || artifact.SHA256 != file.SHA256 {
				s.mu.RUnlock()
				return nil, &repositoryError{code: "MaterializationInputExpired", status: 410}
			}
			copy := *artifact
			result = append(result, repositoryArtifact{Metadata: copy, Path: s.artifactPath(&copy)})
		}
	}
	s.mu.RUnlock()
	if len(result) == 0 {
		return nil, &repositoryError{code: "ManifestContainsNoPackages", status: 422}
	}
	for _, artifact := range result {
		if err := verifyFile(artifact.Path, artifact.Metadata.Size, artifact.Metadata.SHA256); err != nil {
			return nil, &repositoryError{code: "MaterializationInputExpired", status: 410}
		}
	}
	return result, nil
}
