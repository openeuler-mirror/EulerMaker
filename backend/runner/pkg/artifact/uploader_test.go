package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"ebs-runner/pkg/gateway"
)

type fakeArtifactRemote struct {
	mu          sync.Mutex
	uploads     []UploadArtifactInput
	keys        []string
	manifest    CompleteManifestInput
	manifestErr error
}

type manifestConfirmationRemote struct {
	*fakeArtifactRemote
	response CompletedManifest
	firstErr error
	getErr   error
	calls    int
}

func (f *manifestConfirmationRemote) CompleteManifest(context.Context, string, string, CompleteManifestInput) (CompletedManifest, error) {
	f.calls++
	if f.calls == 1 && f.firstErr != nil {
		return CompletedManifest{}, f.firstErr
	}
	return f.response, nil
}

func (f *manifestConfirmationRemote) GetManifest(context.Context, string, string) (CompletedManifest, error) {
	return f.response, f.getErr
}

func TestManifestConfirmationUsesUploadDigest(t *testing.T) {
	input := CompleteManifestInput{Files: []ManifestFile{{
		ArtifactID: "rpm-1", RelativePath: "packages/pkg.rpm", Category: "artifact", Size: 3, SHA256: "raw-sha", Required: true,
	}}}
	for _, test := range []struct {
		name             string
		firstErr, getErr error
		wrongDigest      bool
		wantCalls        int
	}{
		{name: "signed completion", wantCalls: 1},
		{name: "lost response confirmed by GET", firstErr: errors.New("response lost"), wantCalls: 1},
		{name: "same count different input", wrongDigest: true, wantCalls: 1},
		{name: "GET must confirm original input", firstErr: errors.New("response lost"), wrongDigest: true, wantCalls: 1},
		{name: "signing busy retries original intent", firstErr: APIError{StatusCode: 503, Code: "SigningBusy", Retryable: true},
			getErr: os.ErrNotExist, wantCalls: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := CompletedManifest{
				State: "Completed", ArtifactCount: 1, UploadDigest: input.UploadDigest(), Digest: "signed-digest",
				SigningFingerprint: "key", Files: []ManifestFile{{ArtifactID: "rpm-1", Size: 10, SHA256: "signed-sha"}},
			}
			if test.wrongDigest {
				response.UploadDigest = "another-input"
			}
			remote := &manifestConfirmationRemote{fakeArtifactRemote: &fakeArtifactRemote{}, response: response,
				firstErr: test.firstErr, getErr: test.getErr}
			p := &Processor{Remote: remote}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := p.completeManifestWithRetry(ctx, gateway.JobResource{
				Metadata: gateway.ObjectMeta{Namespace: "project", Name: "job"},
			}, input)
			if (err != nil) != test.wrongDigest {
				t.Fatalf("confirmation: %+v, %v", result, err)
			}
			if remote.calls != test.wantCalls {
				t.Fatalf("POST calls=%d", remote.calls)
			}
			if input.Files[0].Size != 3 || input.Files[0].SHA256 != "raw-sha" {
				t.Fatal("original receipt was mutated")
			}
		})
	}
}

func TestManifestUploadDigestContractVector(t *testing.T) {
	input := CompleteManifestInput{Files: []ManifestFile{
		{ArtifactID: "rpm-1", RelativePath: "packages/pkg.rpm", Category: "artifact", Size: 3, SHA256: "rpm-sha", Required: true},
		{ArtifactID: "log-1", RelativePath: "logs/container.log", Category: "log", Size: 12, SHA256: "log-sha", Required: true},
	}}
	const expected = "sha256:bef0ca66e6a994208d195fa6a279f83f4c0e944cb2b38fd4a818686dba8de8a7"
	if actual := input.UploadDigest(); actual != expected {
		t.Fatalf("digest=%s", actual)
	}
	if input.Files[0].ArtifactID != "rpm-1" {
		t.Fatal("input order mutated")
	}
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

func (f *fakeArtifactRemote) UploadArtifact(_ context.Context, project, job, key string, _ string, input UploadArtifactInput) (ArtifactRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads = append(f.uploads, input)
	f.keys = append(f.keys, key)
	return ArtifactRecord{
		ID:       "artifact-" + input.FileName,
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
	return CompletedManifest{State: "Completed", ArtifactCount: len(input.Files), UploadDigest: input.UploadDigest()}, nil
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
	processor := &Processor{Remote: remote, RootDir: root, MaxFileSize: 1024, MaxJobSize: 2048, MaxFiles: 10, Concurrency: 2}
	job := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "job-a", Namespace: "project-a", UID: "uid-a"}}

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
	for i, upload := range remote.uploads {
		sum := sha256.Sum256([]byte(upload.RelativePath))
		if want := "artifact-" + hex.EncodeToString(sum[:]); remote.keys[i] != want {
			t.Fatalf("upload key = %q, want %q", remote.keys[i], want)
		}
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
	processor := &Processor{Remote: remote}
	job := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "job", Namespace: "project", UID: "uid"}}
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
	remote := &flakyLogStatusRemote{fakeArtifactRemote: &fakeArtifactRemote{}, err: APIError{StatusCode: 400}}
	processor := &Processor{Remote: remote}
	job := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "job", Namespace: "project", UID: "uid"}}

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
	processor := &Processor{Remote: &fakeArtifactRemote{}, RootDir: root}
	_, err := processor.Finalize(context.Background(), gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "job", Namespace: "project", UID: "uid"}}, results, true)
	if err == nil {
		t.Fatal("expected symlink scan to fail")
	}
}
