package runner

import (
	"context"
	"os"

	"ebs-runner/pkg/artifact"
	"ebs-runner/pkg/gateway"
)

type fakeArtifactRemote struct{}

func (f *fakeArtifactRemote) LogStatus(context.Context, string, string) (artifact.LogStatus, error) {
	size := int64(12)
	return artifact.LogStatus{State: "Completed", ArtifactID: "log-1", FinalSize: &size, FinalSHA256: "log-sha"}, nil
}

func (f *fakeArtifactRemote) UploadArtifact(_ context.Context, project, job, key string, _ string, input artifact.UploadArtifactInput) (artifact.ArtifactRecord, error) {
	return artifact.ArtifactRecord{
		ID:       "artifact-" + input.FileName,
		Category: input.Category, FileName: input.FileName, RelativePath: input.RelativePath,
		ContentType: input.ContentType, Size: input.Size, SHA256: input.SHA256, State: "Completed",
	}, nil
}

func (f *fakeArtifactRemote) CompleteManifest(_ context.Context, _, _ string, input artifact.CompleteManifestInput) (artifact.CompletedManifest, error) {
	return artifact.CompletedManifest{State: "Completed", ArtifactCount: len(input.Files)}, nil
}

func (f *fakeArtifactRemote) GetManifest(context.Context, string, string) (artifact.CompletedManifest, error) {
	return artifact.CompletedManifest{}, os.ErrNotExist
}

type fakeExecutor struct {
	called     bool
	resultRoot string
	err        error
}

func (f *fakeExecutor) Execute(context.Context, gateway.JobResource) (string, error) {
	f.called = true
	return f.resultRoot, f.err
}
