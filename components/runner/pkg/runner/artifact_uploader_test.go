package runner

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

type fakeArtifactRemote struct {
	mu          sync.Mutex
	uploads     []UploadArtifactInput
	manifest    CompleteManifestInput
	manifestErr error
}

func (f *fakeArtifactRemote) LogStatus(context.Context, string, string, string) (LogStatus, error) {
	size := int64(12)
	return LogStatus{State: "Completed", ArtifactID: "log-1", FinalSize: &size, FinalSHA256: "log-sha"}, nil
}

func (f *fakeArtifactRemote) UploadArtifact(_ context.Context, project, job, _ string, _ string, input UploadArtifactInput) (ArtifactRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads = append(f.uploads, input)
	return ArtifactRecord{
		ID: "artifact-" + input.FileName, Project: project, JobName: job, JobUID: input.JobUID,
		Category: input.Category, FileName: input.FileName, RelativePath: input.RelativePath,
		ContentType: input.ContentType, Size: input.Size, SHA256: input.SHA256, State: "Completed",
	}, nil
}

func (f *fakeArtifactRemote) CompleteManifest(_ context.Context, _, _ string, _ string, input CompleteManifestInput) (CompletedManifest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.manifest = input
	if f.manifestErr != nil {
		return CompletedManifest{}, f.manifestErr
	}
	return CompletedManifest{JobUID: input.JobUID, Generation: input.Generation, State: "Completed", ArtifactCount: len(input.Files), Digest: "sha256:manifest"}, nil
}

func (f *fakeArtifactRemote) GetManifest(context.Context, string, string, string, int64) (CompletedManifest, error) {
	return CompletedManifest{}, os.ErrNotExist
}

func TestArtifactProcessorUploadsResultsAndCompletesManifest(t *testing.T) {
	root := t.TempDir()
	results := filepath.Join(root, "results", "project-a", "uid-a")
	if err := os.MkdirAll(filepath.Join(results, "packages"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(results, "packages", "example.rpm"), []byte("rpm"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(results, "metadata.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	remote := &fakeArtifactRemote{}
	processor := &ArtifactProcessor{Remote: remote, RootDir: root, MaxFileSize: 1024, MaxJobSize: 2048, MaxFiles: 10, Concurrency: 2}
	job := JobResource{Metadata: ObjectMeta{Name: "job-a", Namespace: "project-a", UID: "uid-a"}}

	manifest, err := processor.Finalize(context.Background(), job, results, true)
	if err != nil {
		t.Fatalf("finalize artifacts: %v", err)
	}
	if manifest.ArtifactCount != 3 || manifest.Digest != "sha256:manifest" {
		t.Fatalf("unexpected manifest: %#v", manifest)
	}
	remote.mu.Lock()
	if len(remote.uploads) != 2 {
		t.Fatalf("uploads = %d, want 2", len(remote.uploads))
	}
	files := append([]ManifestFile(nil), remote.manifest.Files...)
	remote.mu.Unlock()
	paths := []string{files[0].RelativePath, files[1].RelativePath, files[2].RelativePath}
	if want := []string{"logs/container.log", "metadata.json", "packages/example.rpm"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("manifest paths = %#v, want %#v", paths, want)
	}
	if files[2].Category != "artifact" || !files[2].Required {
		t.Fatalf("unexpected RPM manifest entry: %#v", files[2])
	}

	if _, err := processor.Finalize(context.Background(), job, results, true); err != nil {
		t.Fatalf("resume artifacts: %v", err)
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if len(remote.uploads) != 2 {
		t.Fatalf("completed receipts did not suppress re-upload: uploads=%d", len(remote.uploads))
	}
}

func TestArtifactProcessorRejectsSymlinkBeforeUpload(t *testing.T) {
	root := t.TempDir()
	results := filepath.Join(root, "results")
	if err := os.MkdirAll(results, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(results, "escape")); err != nil {
		t.Fatal(err)
	}
	processor := &ArtifactProcessor{Remote: &fakeArtifactRemote{}, RootDir: root}
	_, err := processor.Finalize(context.Background(), JobResource{Metadata: ObjectMeta{Name: "job", Namespace: "project", UID: "uid"}}, results, true)
	if err == nil {
		t.Fatal("expected symlink scan to fail")
	}
}

func TestArtifactCleanupSuccessIsImmediateAndFailureIsRetained(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	manager := &ArtifactCleanupManager{RootDir: root, FailedRetention: 24 * time.Hour, Now: func() time.Time { return now }}
	job := JobResource{Metadata: ObjectMeta{Name: "job", Namespace: "project", UID: "uid"}}
	createLocalArtifactState(t, root, job)
	if err := manager.MarkSuccess(job); err != nil {
		t.Fatalf("mark success: %v", err)
	}
	assertLocalArtifactState(t, root, job, false)

	createLocalArtifactState(t, root, job)
	if err := manager.MarkFailure(job); err != nil {
		t.Fatalf("mark failure: %v", err)
	}
	assertLocalArtifactState(t, root, job, true)
	if err := manager.Sweep(); err != nil {
		t.Fatalf("early sweep: %v", err)
	}
	assertLocalArtifactState(t, root, job, true)
	now = now.Add(24 * time.Hour)
	if err := manager.Sweep(); err != nil {
		t.Fatalf("due sweep: %v", err)
	}
	assertLocalArtifactState(t, root, job, false)
}

func createLocalArtifactState(t *testing.T, root string, job JobResource) {
	t.Helper()
	for _, category := range []string{"results", "logs", "uploads"} {
		dir := filepath.Join(root, category, job.Metadata.Namespace, job.Metadata.UID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "state"), []byte("state"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func assertLocalArtifactState(t *testing.T, root string, job JobResource, want bool) {
	t.Helper()
	for _, category := range []string{"results", "logs", "uploads"} {
		_, err := os.Stat(filepath.Join(root, category, job.Metadata.Namespace, job.Metadata.UID))
		if got := err == nil; got != want {
			t.Fatalf("%s existence = %v, want %v (err=%v)", category, got, want, err)
		}
	}
}
