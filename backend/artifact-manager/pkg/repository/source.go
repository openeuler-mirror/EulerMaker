package repository

import (
	"strings"

	"artifact-manager/pkg/fileutil"
	"artifact-manager/pkg/upload"
)

type repositoryArtifact struct {
	Metadata upload.Artifact
	Path     string
	JobName  string
}

func repositoryArtifacts(s *upload.Store, project string, refs []ManifestReference) ([]repositoryArtifact, error) {
	var result []repositoryArtifact
	for _, ref := range refs {
		manifest, ok := s.GetManifest(project, ref.JobName)
		if !ok || manifest.State != upload.ManifestCompleted {
			return nil, &Error{Code: "ManifestNotReady", Status: 422, JobName: ref.JobName}
		}
		if manifest.Digest != upload.ManifestDigest(append([]upload.ManifestFile(nil), manifest.Files...)) {
			return nil, &Error{Code: "ManifestInvalid", Status: 422, JobName: ref.JobName}
		}
		packages := 0
		for _, file := range manifest.Files {
			if file.Category != upload.CategoryArtifact || !strings.HasPrefix(file.RelativePath, "packages/") || !strings.HasSuffix(strings.ToLower(file.RelativePath), ".rpm") {
				continue
			}
			artifact, ok := s.GetArtifact(file.ArtifactID)
			if !ok || artifact.State != upload.Completed || artifact.Project != project || artifact.JobName != ref.JobName || artifact.Size != file.Size || artifact.SHA256 != file.SHA256 {
				return nil, &Error{Code: "MaterializationInputExpired", Status: 410, JobName: ref.JobName}
			}
			copy := *artifact
			result = append(result, repositoryArtifact{Metadata: copy, Path: s.ArtifactPath(&copy), JobName: ref.JobName})
			packages++
		}
		if packages == 0 {
			return nil, &Error{Code: "ManifestContainsNoPackages", Status: 422, JobName: ref.JobName}
		}
	}
	if len(result) == 0 {
		return nil, &Error{Code: "ManifestContainsNoPackages", Status: 422}
	}
	for _, artifact := range result {
		if err := fileutil.VerifyFile(artifact.Path, artifact.Metadata.Size, artifact.Metadata.SHA256); err != nil {
			return nil, &Error{Code: "MaterializationInputExpired", Status: 410, JobName: artifact.JobName}
		}
	}
	return result, nil
}
