package repository

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"artifact-manager/pkg/fileutil"
	"artifact-manager/pkg/upload"
)

type memoryArtifactReader struct {
	manifest *upload.JobUploadManifest
	artifact *upload.Artifact
	path     string
}

func (r *memoryArtifactReader) GetManifest(project, jobName string) (*upload.JobUploadManifest, bool) {
	return r.manifest, r.manifest.Project == project && r.manifest.JobName == jobName
}

func (r *memoryArtifactReader) GetArtifact(id string) (*upload.Artifact, bool) {
	return r.artifact, r.artifact.ID == id
}

func (r *memoryArtifactReader) ArtifactPath(_ *upload.Artifact) string {
	return r.path
}

func TestRepositoryArtifactsReadFromInterface(t *testing.T) {
	path := filepath.Join(t.TempDir(), "package.rpm")
	data := []byte("package contents")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	digest, err := fileutil.FileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	artifact := &upload.Artifact{
		ID: "artifact", Project: "project", JobName: "job", State: upload.Completed,
		Size: int64(len(data)), SHA256: digest,
	}
	manifest := &upload.JobUploadManifest{
		Project: "project", JobName: "job", State: upload.ManifestCompleted,
		Files: []upload.ManifestFile{{
			ArtifactID: artifact.ID, RelativePath: "packages/package.rpm", Category: upload.CategoryArtifact,
			Size: artifact.Size, SHA256: digest,
		}},
	}
	manifest.Digest = upload.ManifestDigest(manifest.Files)
	reader := &memoryArtifactReader{manifest: manifest, artifact: artifact, path: path}
	result, err := repositoryArtifacts(reader, "project", []ManifestReference{{JobName: "job"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].Metadata.ID != artifact.ID || result[0].Path != path || result[0].JobName != "job" {
		t.Fatalf("unexpected artifacts: %+v", result)
	}
}

func TestRepositoryArtifactsIdentifyBadManifest(t *testing.T) {
	const project, job = "project", "job"
	ref := []ManifestReference{{JobName: job}}
	tests := []struct {
		name     string
		manifest *upload.JobUploadManifest
		code     string
	}{
		{name: "missing", code: "ManifestNotReady"},
		{name: "invalid digest", manifest: &upload.JobUploadManifest{State: upload.ManifestCompleted, Digest: "bad"}, code: "ManifestInvalid"},
		{name: "no RPM", manifest: &upload.JobUploadManifest{State: upload.ManifestCompleted,
			Files: []upload.ManifestFile{{RelativePath: "logs/build.log", Category: upload.CategoryLog}}}, code: "ManifestContainsNoPackages"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if tt.manifest != nil {
				tt.manifest.Project = project
				tt.manifest.JobName = job
				if tt.name == "no RPM" {
					tt.manifest.Digest = upload.ManifestDigest(tt.manifest.Files)
				}
				if err := fileutil.AtomicJSON(filepath.Join(root, ".metadata/jobs", project, job, "manifest.json"), tt.manifest); err != nil {
					t.Fatal(err)
				}
			}
			store, err := upload.NewStore(root)
			if err != nil {
				t.Fatal(err)
			}
			_, err = repositoryArtifacts(store, project, ref)
			var typed *Error
			if !errors.As(err, &typed) || typed.Code != tt.code || typed.JobName != job {
				t.Fatalf("error = %v, want code %q and job name %q", err, tt.code, job)
			}
		})
	}
}
