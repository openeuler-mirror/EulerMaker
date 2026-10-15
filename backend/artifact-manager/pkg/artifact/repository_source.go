package artifact

import (
	"strings"

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
			return nil, &repositoryError{code: "ManifestNotReady", status: 422, jobName: ref.JobName}
		}
		if manifest.Digest != upload.ManifestDigest(append([]upload.ManifestFile(nil), manifest.Files...)) {
			return nil, &repositoryError{code: "ManifestInvalid", status: 422, jobName: ref.JobName}
		}
		packages := 0
		for _, file := range manifest.Files {
			if file.Category != upload.CategoryArtifact || !strings.HasPrefix(file.RelativePath, "packages/") || !strings.HasSuffix(strings.ToLower(file.RelativePath), ".rpm") {
				continue
			}
			artifact, ok := s.GetArtifact(file.ArtifactID)
			if !ok || artifact.State != upload.Completed || artifact.Project != project || artifact.JobName != ref.JobName || artifact.Size != file.Size || artifact.SHA256 != file.SHA256 {
				return nil, &repositoryError{code: "MaterializationInputExpired", status: 410, jobName: ref.JobName}
			}
			copy := *artifact
			result = append(result, repositoryArtifact{Metadata: copy, Path: s.ArtifactPath(&copy), JobName: ref.JobName})
			packages++
		}
		if packages == 0 {
			return nil, &repositoryError{code: "ManifestContainsNoPackages", status: 422, jobName: ref.JobName}
		}
	}
	if len(result) == 0 {
		return nil, &repositoryError{code: "ManifestContainsNoPackages", status: 422}
	}
	for _, artifact := range result {
		if err := upload.VerifyFile(artifact.Path, artifact.Metadata.Size, artifact.Metadata.SHA256); err != nil {
			return nil, &repositoryError{code: "MaterializationInputExpired", status: 410, jobName: artifact.JobName}
		}
	}
	return result, nil
}
