package artifact

import (
	"context"
	"errors"
	"testing"
	"time"
)

type badManifestMaterializer struct{}

func (badManifestMaterializer) Materialize(context.Context, RepositoryRecord) (repositoryResult, error) {
	return repositoryResult{}, &repositoryError{code: "ManifestInvalid", status: 422, jobUID: "job-uid-1"}
}

func TestRepositoryFailurePersistsOffendingJobUID(t *testing.T) {
	server, request := newRepositoryTestServer(t, badManifestMaterializer{})
	if _, _, err := server.repositories.submit(request); err != nil {
		t.Fatalf("submit: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		record, ok := server.repositories.get(request.RepositoryUID)
		if ok && record.State == RepositoryFailed {
			if record.Failure == nil || record.Failure.JobUID != "job-uid-1" {
				t.Fatalf("failure lost offending Job UID: %+v", record.Failure)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("repository did not fail")
}

func TestRepositoryArtifactsIdentifyBadManifest(t *testing.T) {
	const project, job, uid = "project", "job", "job-uid"
	ref := []ManifestReference{{JobName: job, JobUID: uid}}
	tests := []struct {
		name     string
		manifest *JobUploadManifest
		code     string
	}{
		{name: "missing", code: "ManifestNotReady"},
		{name: "open", manifest: &JobUploadManifest{State: ManifestOpen}, code: "ManifestNotReady"},
		{name: "invalid digest", manifest: &JobUploadManifest{State: ManifestCompleted, Digest: "bad"}, code: "ManifestInvalid"},
		{name: "no RPM", manifest: &JobUploadManifest{State: ManifestCompleted,
			Files: []ManifestFile{{RelativePath: "logs/build.log", Category: CategoryLog}}}, code: "ManifestContainsNoPackages"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &Store{manifests: make(map[string]*JobUploadManifest)}
			if tt.manifest != nil {
				if tt.name == "no RPM" {
					tt.manifest.Digest = manifestDigest(tt.manifest.Files)
				}
				store.manifests[manifestKey(project, job, uid)] = tt.manifest
			}
			_, err := store.repositoryArtifacts(project, ref)
			var typed *repositoryError
			if !errors.As(err, &typed) || typed.code != tt.code || typed.jobUID != uid {
				t.Fatalf("error = %v, want code %q and job UID %q", err, tt.code, uid)
			}
		})
	}
}
