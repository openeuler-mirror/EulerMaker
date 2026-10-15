package artifact

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"artifact-manager/pkg/upload"
)

type badManifestMaterializer struct{}

func (badManifestMaterializer) Materialize(context.Context, RepositoryRecord) (repositoryResult, error) {
	return repositoryResult{}, &repositoryError{code: "ManifestInvalid", status: 422, jobName: "job-1"}
}

func TestRepositoryFailurePersistsOffendingJobName(t *testing.T) {
	server, request := newRepositoryTestServer(t, badManifestMaterializer{})
	if _, _, err := server.repositories.submit(request); err != nil {
		t.Fatalf("submit: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		record, ok, err := server.repositories.get(request.RepositoryUID)
		if err != nil {
			t.Fatal(err)
		}
		if ok && record.State == RepositoryFailed {
			if record.Failure == nil || record.Failure.JobName != "job-1" {
				t.Fatalf("failure lost offending Job name: %+v", record.Failure)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("repository did not fail")
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
			var typed *repositoryError
			if !errors.As(err, &typed) || typed.code != tt.code || typed.jobName != job {
				t.Fatalf("error = %v, want code %q and job name %q", err, tt.code, job)
			}
		})
	}
}
