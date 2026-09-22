package rpmrepo

import (
	"context"
	"fmt"
)

// FakeArtifactManager is an in-memory ArtifactManagerClient. Every method can be scripted, and the recorded
// requests let a test assert that a replay used exactly the frozen checkpoint.
type FakeArtifactManager struct {
	SubmitRepositoryFunc func(context.Context, CreateRepositoryRequest) (RepositoryResponse, error)
	GetRepositoryFunc    func(context.Context, string) (RepositoryResponse, error)
	GetJobManifestFunc   func(context.Context, string, string, string) (JobUploadManifest, error)
	SubmitReleaseFunc    func(context.Context, CreateReleaseRequest) (ReleaseResponse, error)
	GetReleaseFunc       func(context.Context, string) (ReleaseResponse, error)
	ActivateReleaseFunc  func(context.Context, string) (ReleaseResponse, error)

	SubmitRepositoryRequests []CreateRepositoryRequest
	GetRepositoryUIDs        []string
	ManifestRequests         [][3]string
	SubmitReleaseRequests    []CreateReleaseRequest
	GetReleaseBuildNames     []string
	ActivateReleaseNames     []string
}

func NewFakeArtifactManager() *FakeArtifactManager { return &FakeArtifactManager{} }

func (f *FakeArtifactManager) SubmitRepository(ctx context.Context, req CreateRepositoryRequest) (RepositoryResponse, error) {
	f.SubmitRepositoryRequests = append(f.SubmitRepositoryRequests, req)
	if f.SubmitRepositoryFunc == nil {
		return RepositoryResponse{}, fmt.Errorf("FakeArtifactManager.SubmitRepositoryFunc is not configured")
	}
	return f.SubmitRepositoryFunc(ctx, req)
}

func (f *FakeArtifactManager) GetRepository(ctx context.Context, repositoryUID string) (RepositoryResponse, error) {
	f.GetRepositoryUIDs = append(f.GetRepositoryUIDs, repositoryUID)
	if f.GetRepositoryFunc == nil {
		return RepositoryResponse{}, fmt.Errorf("FakeArtifactManager.GetRepositoryFunc is not configured")
	}
	return f.GetRepositoryFunc(ctx, repositoryUID)
}

func (f *FakeArtifactManager) GetJobManifest(ctx context.Context, project, jobName, jobUID string) (JobUploadManifest, error) {
	f.ManifestRequests = append(f.ManifestRequests, [3]string{project, jobName, jobUID})
	if f.GetJobManifestFunc == nil {
		return JobUploadManifest{}, &artifactError{operation: "get-manifest", kind: artifactNotFound, code: "NotFound",
			err: fmt.Errorf("no manifest configured for %s/%s", project, jobName)}
	}
	return f.GetJobManifestFunc(ctx, project, jobName, jobUID)
}

func (f *FakeArtifactManager) SubmitRelease(ctx context.Context, req CreateReleaseRequest) (ReleaseResponse, error) {
	f.SubmitReleaseRequests = append(f.SubmitReleaseRequests, req)
	if f.SubmitReleaseFunc == nil {
		return ReleaseResponse{}, fmt.Errorf("FakeArtifactManager.SubmitReleaseFunc is not configured")
	}
	return f.SubmitReleaseFunc(ctx, req)
}

func (f *FakeArtifactManager) GetRelease(ctx context.Context, buildName string) (ReleaseResponse, error) {
	f.GetReleaseBuildNames = append(f.GetReleaseBuildNames, buildName)
	if f.GetReleaseFunc == nil {
		return ReleaseResponse{}, fmt.Errorf("FakeArtifactManager.GetReleaseFunc is not configured")
	}
	return f.GetReleaseFunc(ctx, buildName)
}

func (f *FakeArtifactManager) ActivateRelease(ctx context.Context, buildName string) (ReleaseResponse, error) {
	f.ActivateReleaseNames = append(f.ActivateReleaseNames, buildName)
	if f.ActivateReleaseFunc == nil {
		return ReleaseResponse{}, fmt.Errorf("FakeArtifactManager.ActivateReleaseFunc is not configured")
	}
	return f.ActivateReleaseFunc(ctx, buildName)
}

// Reset clears the recorded calls while keeping the scripted responses.
func (f *FakeArtifactManager) Reset() {
	f.SubmitRepositoryRequests = nil
	f.GetRepositoryUIDs = nil
	f.ManifestRequests = nil
	f.SubmitReleaseRequests = nil
	f.GetReleaseBuildNames = nil
	f.ActivateReleaseNames = nil
}
