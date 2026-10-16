package artifact

import (
	"context"
	"testing"
	"time"

	"artifact-manager/pkg/repository"
)

type badManifestMaterializer struct{}

func (badManifestMaterializer) Materialize(context.Context, repository.RepositoryRecord) (repository.Result, error) {
	return repository.Result{}, &repository.Error{Code: "ManifestInvalid", Status: 422, JobName: "job-1"}
}

func TestRepositoryFailurePersistsOffendingJobName(t *testing.T) {
	server, request := newRepositoryTestServer(t, badManifestMaterializer{})
	if _, _, err := server.repositories.Submit(request); err != nil {
		t.Fatalf("submit: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		record, ok, err := server.repositories.Get(request.RepositoryUID)
		if err != nil {
			t.Fatal(err)
		}
		if ok && record.State == repository.RepositoryFailed {
			if record.Failure == nil || record.Failure.JobName != "job-1" {
				t.Fatalf("failure lost offending Job name: %+v", record.Failure)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("repository did not fail")
}
