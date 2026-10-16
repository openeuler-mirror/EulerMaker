package repository

import (
	"errors"
	"path/filepath"
	"testing"

	"artifact-manager/pkg/upload"
)

func TestRepositoryArtifactsIdentifyBadManifest(t *testing.T) {
	const project, job = "project", "job"
	ref := []ManifestReference{{JobName: job}}
	tests := []struct {
		name     string
		manifest *upload.JobUploadManifest
		code     string
	}{
		{name: "missing", code: "ManifestNotReady"},
		{name: "open", manifest: &upload.JobUploadManifest{State: upload.ManifestOpen}, code: "ManifestNotReady"},
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
				if err := upload.AtomicJSON(filepath.Join(root, ".metadata/jobs", project, job, "manifest.json"), tt.manifest); err != nil {
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
