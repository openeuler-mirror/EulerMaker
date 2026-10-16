package repository

import (
	"errors"
	"os"
	"strings"

	"artifact-manager/pkg/fileutil"
	"artifact-manager/pkg/upload"
)

// ArtifactReader provides sealed upload metadata and local artifact paths.
type ArtifactReader interface {
	GetManifest(project, jobName string) (*upload.JobUploadManifest, error)
	GetJobArtifact(project, jobName, id string) (*upload.Artifact, bool)
	ArtifactPath(artifact *upload.Artifact) string
}

type repositoryArtifact struct {
	Metadata upload.Artifact
	Path     string
	JobName  string
}

func repositoryArtifacts(s ArtifactReader, project string, refs []ManifestReference) ([]repositoryArtifact, error) {
	var result []repositoryArtifact
	for _, ref := range refs {
		manifest, err := s.GetManifest(project, ref.JobName)
		if os.IsNotExist(err) {
			return nil, &Error{Code: "ManifestNotReady", Status: 422, JobName: ref.JobName}
		}
		if errors.Is(err, upload.ErrInvalidManifest) {
			return nil, &Error{Code: "ManifestInvalid", Status: 422, JobName: ref.JobName}
		}
		if err != nil {
			return nil, &Error{Code: "StorageError", Status: 503, Retryable: true, JobName: ref.JobName}
		}
		if manifest.State != upload.ManifestCompleted {
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
			artifact, ok := s.GetJobArtifact(project, ref.JobName, file.ArtifactID)
			if !ok || artifact.State != upload.Completed || artifact.Size != file.Size || artifact.SHA256 != file.SHA256 {
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
