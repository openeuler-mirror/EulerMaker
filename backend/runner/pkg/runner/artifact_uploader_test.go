package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"
)

type fakeArtifactRemote struct {
	mu          sync.Mutex
	uploads     []UploadArtifactInput
	manifest    CompleteManifestInput
	manifestErr error
}

type flakyLogStatusRemote struct {
	*fakeArtifactRemote
	calls int
	err   error
}

func (f *flakyLogStatusRemote) LogStatus(ctx context.Context, project, job string) (LogStatus, error) {
	f.calls++
	if f.calls == 1 {
		return LogStatus{}, f.err
	}
	return f.fakeArtifactRemote.LogStatus(ctx, project, job)
}

func (f *fakeArtifactRemote) LogStatus(context.Context, string, string) (LogStatus, error) {
	size := int64(12)
	return LogStatus{State: "Completed", ArtifactID: "log-1", FinalSize: &size, FinalSHA256: "log-sha"}, nil
}

func (f *fakeArtifactRemote) UploadArtifact(_ context.Context, project, job, _ string, _ string, input UploadArtifactInput) (ArtifactRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads = append(f.uploads, input)
	return ArtifactRecord{
		ID: "artifact-" + input.FileName, Project: project, JobName: job,
		Category: input.Category, FileName: input.FileName, RelativePath: input.RelativePath,
		ContentType: input.ContentType, Size: input.Size, SHA256: input.SHA256, State: "Completed",
	}, nil
}

func (f *fakeArtifactRemote) CompleteManifest(_ context.Context, _, _ string, input CompleteManifestInput) (CompletedManifest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.manifest = input
	if f.manifestErr != nil {
		return CompletedManifest{}, f.manifestErr
	}
	return CompletedManifest{State: "Completed", ArtifactCount: len(input.Files)}, nil
}

func (f *fakeArtifactRemote) GetManifest(context.Context, string, string) (CompletedManifest, error) {
	return CompletedManifest{}, os.ErrNotExist
}

func TestArtifactProcessorUploadsResultsAndCompletesManifest(t *testing.T) {
	root := t.TempDir()
	results := filepath.Join(root, "results", "project-a", "job-a")
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
	if manifest.ArtifactCount != 3 {
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

func TestArtifactProcessorRetriesLogStatusBeforeUpload(t *testing.T) {
	remote := &flakyLogStatusRemote{fakeArtifactRemote: &fakeArtifactRemote{}, err: errors.New("connection refused")}
	processor := &ArtifactProcessor{Remote: remote}
	job := JobResource{Metadata: ObjectMeta{Name: "job", Namespace: "project", UID: "uid"}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	manifest, err := processor.Finalize(ctx, job, "", false)
	if err != nil {
		t.Fatalf("finalize after temporary log status failure: %v", err)
	}
	if remote.calls != 2 || manifest.ArtifactCount != 1 {
		t.Fatalf("log status calls = %d, manifest = %#v", remote.calls, manifest)
	}
}

func TestArtifactProcessorDoesNotRetryPermanentLogStatusError(t *testing.T) {
	remote := &flakyLogStatusRemote{fakeArtifactRemote: &fakeArtifactRemote{}, err: ArtifactAPIError{StatusCode: 400}}
	processor := &ArtifactProcessor{Remote: remote}
	job := JobResource{Metadata: ObjectMeta{Name: "job", Namespace: "project", UID: "uid"}}

	if _, err := processor.Finalize(context.Background(), job, "", false); err == nil {
		t.Fatal("expected permanent log status error")
	}
	if remote.calls != 1 {
		t.Fatalf("log status calls = %d, want 1", remote.calls)
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

func TestArtifactCleanupDiskPressureRemovesOldestFailedJobsFirst(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	checks := 0
	manager := &ArtifactCleanupManager{
		RootDir: root, FailedRetention: 24 * time.Hour,
		Now: func() time.Time { return now },
		DiskUsage: func(string) (float64, error) {
			checks++
			if checks == 1 {
				return 0.86, nil
			}
			return 0.85, nil
		},
	}
	oldest := JobResource{Metadata: ObjectMeta{Name: "oldest", Namespace: "project"}}
	newer := JobResource{Metadata: ObjectMeta{Name: "newer", Namespace: "project"}}
	active := JobResource{Metadata: ObjectMeta{Name: "active", Namespace: "project"}}
	for _, job := range []JobResource{oldest, newer, active} {
		createLocalArtifactState(t, root, job)
	}
	if err := manager.MarkFailure(oldest); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := manager.MarkFailure(newer); err != nil {
		t.Fatal(err)
	}
	if err := manager.Sweep(); err != nil {
		t.Fatal(err)
	}
	assertLocalArtifactState(t, root, oldest, false)
	assertLocalArtifactState(t, root, newer, true)
	assertLocalArtifactState(t, root, active, true)
	if checks != 2 {
		t.Fatalf("disk usage checks = %d, want 2", checks)
	}
}

func TestArtifactCleanupDoesNotShortenRetentionBelowThreshold(t *testing.T) {
	root := t.TempDir()
	manager := &ArtifactCleanupManager{
		RootDir: root, FailedRetention: 24 * time.Hour,
		DiskUsage: func(string) (float64, error) { return 0.85, nil },
	}
	job := JobResource{Metadata: ObjectMeta{Name: "job", Namespace: "project"}}
	createLocalArtifactState(t, root, job)
	if err := manager.MarkFailure(job); err != nil {
		t.Fatal(err)
	}
	if err := manager.Sweep(); err != nil {
		t.Fatal(err)
	}
	assertLocalArtifactState(t, root, job, true)
}

func TestArtifactCleanupReclaimsOldestFailedJobWhenMarkerWriteRunsOutOfSpace(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	manager := &ArtifactCleanupManager{RootDir: root, FailedRetention: 24 * time.Hour, Now: func() time.Time { return now }}
	oldest := JobResource{Metadata: ObjectMeta{Name: "oldest", Namespace: "project"}}
	newer := JobResource{Metadata: ObjectMeta{Name: "newer", Namespace: "project"}}
	current := JobResource{Metadata: ObjectMeta{Name: "current", Namespace: "project"}}
	for _, job := range []JobResource{oldest, newer, current} {
		createLocalArtifactState(t, root, job)
	}
	if err := manager.MarkFailure(oldest); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := manager.MarkFailure(newer); err != nil {
		t.Fatal(err)
	}
	manager.writeMarker = func(path string, data []byte) error {
		if _, err := os.Stat(filepath.Join(root, "results", "project", "oldest")); err == nil {
			return syscall.ENOSPC
		}
		return writeAtomicFile(path, data)
	}
	now = now.Add(time.Minute)
	if err := manager.MarkFailure(current); err != nil {
		t.Fatalf("mark failure after reclaim: %v", err)
	}
	assertLocalArtifactState(t, root, oldest, false)
	assertLocalArtifactState(t, root, newer, true)
	assertLocalArtifactState(t, root, current, true)
	if _, err := os.Stat(manager.markerPath("project", "current")); err != nil {
		t.Fatalf("current cleanup marker: %v", err)
	}
}

func TestArtifactCleanupMarkerWriteNoSpaceReclaimsCurrentTerminalJob(t *testing.T) {
	root := t.TempDir()
	manager := &ArtifactCleanupManager{
		RootDir:     root,
		writeMarker: func(string, []byte) error { return syscall.ENOSPC },
	}
	job := JobResource{Metadata: ObjectMeta{Name: "job", Namespace: "project"}}
	createLocalArtifactState(t, root, job)
	if err := manager.MarkFailure(job); err != nil {
		t.Fatalf("mark failure after emergency reclaim: %v", err)
	}
	assertLocalArtifactState(t, root, job, false)
}

func TestArtifactCleanupMarkerWriteOtherErrorDoesNotReclaim(t *testing.T) {
	root := t.TempDir()
	manager := &ArtifactCleanupManager{
		RootDir:     root,
		writeMarker: func(string, []byte) error { return syscall.EACCES },
	}
	job := JobResource{Metadata: ObjectMeta{Name: "job", Namespace: "project"}}
	createLocalArtifactState(t, root, job)
	if err := manager.MarkFailure(job); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("mark failure error = %v, want EACCES", err)
	}
	assertLocalArtifactState(t, root, job, true)
}

func TestArtifactCleanupUsesJobNameAcrossUIDChanges(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	manager := &ArtifactCleanupManager{RootDir: root, FailedRetention: time.Hour, Now: func() time.Time { return now }}
	first := JobResource{Metadata: ObjectMeta{Name: "job", Namespace: "project", UID: "uid-1"}}
	if err := manager.MarkFailure(first); err != nil {
		t.Fatal(err)
	}
	second := JobResource{Metadata: ObjectMeta{Name: "job", Namespace: "project", UID: "uid-2"}}
	if err := manager.MarkFailure(second); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(root, "logs", "project", "job")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if err := manager.Sweep(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logDir); !os.IsNotExist(err) {
		t.Fatalf("log spool was not removed: %v", err)
	}
}

func createLocalArtifactState(t *testing.T, root string, job JobResource) {
	t.Helper()
	for _, category := range []string{"results", "logs", "uploads"} {
		dir := filepath.Join(root, category, job.Metadata.Namespace, job.Metadata.Name)
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
		_, err := os.Stat(filepath.Join(root, category, job.Metadata.Namespace, job.Metadata.Name))
		if got := err == nil; got != want {
			t.Fatalf("%s existence = %v, want %v (err=%v)", category, got, want, err)
		}
	}
}
