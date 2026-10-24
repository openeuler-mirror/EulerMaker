package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"artifact-manager/pkg/fileutil"
	"artifact-manager/pkg/signing"
)

type sealTestSigner struct {
	key     string
	calls   atomic.Int32
	active  atomic.Int32
	maximum atomic.Int32
	failAt  int32
	started chan struct{}
	release <-chan struct{}
	invalid bool
}

func (s *sealTestSigner) Fingerprint() string { return s.key }

func (s *sealTestSigner) Sign(ctx context.Context, path string) error {
	call := s.calls.Add(1)
	active := s.active.Add(1)
	defer s.active.Add(-1)
	for old := s.maximum.Load(); active > old; old = s.maximum.Load() {
		if s.maximum.CompareAndSwap(old, active) {
			break
		}
	}
	if s.started != nil {
		s.started <- struct{}{}
	}
	if s.release != nil {
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if call == s.failAt {
		return errors.New("backend unavailable")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString("-signed")
	return errors.Join(err, f.Close())
}

func (s *sealTestSigner) Verify(_ context.Context, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if s.invalid || !bytes.HasSuffix(data, []byte("-signed")) {
		return signing.ErrVerification
	}
	return nil
}

func sealSum(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }

func newSealTestStore(t *testing.T, root string, signer *sealTestSigner, workers int) (*Store, *Sealer) {
	t.Helper()
	query := filepath.Join(root, "rpm-query")
	script := `#!/bin/sh
case "$2" in
--qf) printf 'pkg\t0\t1\t1\tx86_64\tpkg-1-1.src.rpm\tpayload-digest';;
--provides) printf 'pkg = 1-1\n';;
--requires) printf 'libc.so.6\n';;
*) exit 1;;
esac
`
	if err := os.WriteFile(query, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := NewSealer(Config{MaxFileSize: 1024, MaxJobSize: 4096}, signer, query, workers, time.Second*5)
	if err != nil {
		t.Fatal(err)
	}
	s.SetSealer(sealer)
	t.Cleanup(sealer.Close)
	return s, sealer
}

func sealUpload(t *testing.T, s *Store, job, path string) (Artifact, ManifestFile) {
	t.Helper()
	data := []byte("original-" + path)
	meta := UploadMetadata{Category: CategoryArtifact, FileName: filepath.Base(path), RelativePath: path,
		Size: int64(len(data)), SHA256: sealSum(data)}
	if strings.HasPrefix(path, "logs/") {
		meta.Category = CategoryLog
	}
	a, _, err := s.BeginUpload("project", job, path, meta, 4096)
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(s.root, ".uploads", a.ID+".tmp")
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteUpload("project", job, a, tmp); err != nil {
		t.Fatal(err)
	}
	return *a, ManifestFile{ArtifactID: a.ID, RelativePath: path, Category: a.Category,
		Size: a.Size, SHA256: a.SHA256, Required: true}
}

func assertSealCode(t *testing.T, err error, code string) {
	t.Helper()
	var typed *SealError
	if !errors.As(err, &typed) || typed.Code != code {
		t.Fatalf("error = %v, want %s", err, code)
	}
}

func TestSealSignsPrivateCopiesAndPublishesFinalMetadata(t *testing.T) {
	signer := &sealTestSigner{key: "key"}
	s, _ := newSealTestStore(t, t.TempDir(), signer, 2)
	a, rpm := sealUpload(t, s, "job", "packages/pkg.rpm")
	_, log := sealUpload(t, s, "job", "logs/container.log")
	in := CompleteManifestRequest{Files: []ManifestFile{rpm, log}}
	m, err := s.CompleteManifest(context.Background(), "project", "job", "runner", in)
	if err != nil {
		t.Fatal(err)
	}
	if m.UploadDigest != ManifestDigest(in.Files) || m.Digest == m.UploadDigest || m.SigningFingerprint != "key" {
		t.Fatalf("manifest digests = %+v", m)
	}
	if signer.calls.Load() != 1 {
		t.Fatalf("signed %d files", signer.calls.Load())
	}
	if err := fileutil.VerifyFile(s.ArtifactPath(&a), a.Size, a.SHA256); err != nil {
		t.Fatal("original changed", err)
	}
	effective, ok := s.GetJobArtifact("project", "job", a.ID)
	if !ok || effective.SHA256 == a.SHA256 || effective.Size != a.Size+7 {
		t.Fatalf("final artifact = %+v", effective)
	}
	rawInfo, _ := os.Stat(s.ArtifactPath(&a))
	signedInfo, _ := os.Stat(s.ArtifactPath(effective))
	if os.SameFile(rawInfo, signedInfo) {
		t.Fatal("signing mutated shared inode")
	}
	items, err := s.ListArtifacts("project", "job", CategoryArtifact)
	if err != nil || len(items) != 1 || items[0].SHA256 != effective.SHA256 {
		t.Fatalf("list = %+v, %v", items, err)
	}
	meta := UploadMetadata{Category: a.Category, FileName: a.FileName, RelativePath: a.RelativePath,
		Size: a.Size, SHA256: a.SHA256}
	receipt, reused, err := s.BeginUpload("project", "job", a.RelativePath, meta, 4096)
	if err != nil || !reused || receipt.SHA256 != a.SHA256 {
		t.Fatalf("original upload replay: %+v, %v", receipt, err)
	}
	replayed, err := s.CompleteManifest(context.Background(), "project", "job", "runner", in)
	if err != nil || replayed.Digest != m.Digest || signer.calls.Load() != 1 {
		t.Fatalf("seal replay: %+v, %v", replayed, err)
	}
	if err := os.Remove(s.ArtifactPath(effective)); err != nil {
		t.Fatal(err)
	}
	projected, ok := s.GetJobArtifact("project", "job", a.ID)
	if !ok || projected.StorageKey != effective.StorageKey {
		t.Fatal("fell back to raw content")
	}
}

func TestSealDisabledAndLogOnly(t *testing.T) {
	for _, key := range []string{"", "key"} {
		t.Run(key, func(t *testing.T) {
			signer := &sealTestSigner{key: key}
			s, _ := newSealTestStore(t, t.TempDir(), signer, 1)
			_, file := sealUpload(t, s, "job", "logs/container.log")
			if key == "" {
				_, file = sealUpload(t, s, "job", "packages/pkg.rpm")
			}
			m, err := s.CompleteManifest(context.Background(), "project", "job", "runner", CompleteManifestRequest{Files: []ManifestFile{file}})
			if err != nil || m.Digest != m.UploadDigest || signer.calls.Load() != 0 {
				t.Fatalf("manifest = %+v, %v", m, err)
			}
		})
	}
}

func TestSealRestartReusesCommittedCopy(t *testing.T) {
	for _, loseProgress := range []bool{false, true} {
		t.Run(fmt.Sprint(loseProgress), func(t *testing.T) {
			root := t.TempDir()
			s, pool := newSealTestStore(t, root, &sealTestSigner{key: "key", failAt: 2}, 1)
			_, first := sealUpload(t, s, "job", "packages/a.rpm")
			_, second := sealUpload(t, s, "job", "packages/b.rpm")
			in := CompleteManifestRequest{Files: []ManifestFile{first, second}}
			_, err := s.CompleteManifest(context.Background(), "project", "job", "runner", in)
			assertSealCode(t, err, "SigningBackendUnavailable")
			if _, err := s.GetManifest("project", "job"); !os.IsNotExist(err) {
				t.Fatalf("failed sealing published: %v", err)
			}
			pool.Close()
			if loseProgress {
				record, err := s.readSeal("project", "job")
				if err != nil {
					t.Fatal(err)
				}
				record.Done = nil
				if err := fileutil.AtomicJSON(s.sealPath("project", "job"), record); err != nil {
					t.Fatal(err)
				}
			}
			signer := &sealTestSigner{key: "key"}
			restarted, _ := newSealTestStore(t, root, signer, 1)
			m, err := restarted.CompleteManifest(context.Background(), "project", "job", "runner", in)
			if err != nil || signer.calls.Load() != 1 || len(m.Files) != 2 {
				t.Fatalf("resume: %+v, %v, calls=%d", m, err, signer.calls.Load())
			}
		})
	}
}

func TestSealFreezesPolicyAndValidatesOutput(t *testing.T) {
	root := t.TempDir()
	s, pool := newSealTestStore(t, root, &sealTestSigner{key: "key", invalid: true}, 1)
	_, file := sealUpload(t, s, "job", "packages/a.rpm")
	in := CompleteManifestRequest{Files: []ManifestFile{file}}
	_, err := s.CompleteManifest(context.Background(), "project", "job", "runner", in)
	assertSealCode(t, err, "SignedPackageVerificationFailed")
	if _, err := s.GetManifest("project", "job"); !os.IsNotExist(err) {
		t.Fatal("invalid signature sealed")
	}
	pool.Close()
	s, _ = newSealTestStore(t, root, &sealTestSigner{key: "other"}, 1)
	_, err = s.CompleteManifest(context.Background(), "project", "job", "runner", in)
	assertSealCode(t, err, "SigningConfigurationChanged")
	in.Files[0].Required = false
	_, err = s.CompleteManifest(context.Background(), "project", "job", "runner", in)
	assertSealCode(t, err, "ManifestConflict")
}

func TestSealGlobalConcurrencyAndJobDeduplication(t *testing.T) {
	release := make(chan struct{})
	signer := &sealTestSigner{key: "key", started: make(chan struct{}, 8), release: release}
	s, _ := newSealTestStore(t, t.TempDir(), signer, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 4)
	inputs := make([]CompleteManifestRequest, 4)
	for i := range inputs {
		_, f := sealUpload(t, s, fmt.Sprint(i), "packages/pkg.rpm")
		inputs[i] = CompleteManifestRequest{Files: []ManifestFile{f}}
		go func() { _, err := s.CompleteManifest(ctx, "project", fmt.Sprint(i), "runner", inputs[i]); done <- err }()
		if i == 0 {
			select {
			case <-signer.started:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
	select {
	case <-signer.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// The Store lock is not held by either signing worker.
	if _, err := s.ListArtifacts("project", "0", ""); err != nil {
		t.Fatal(err)
	}
	_, err := s.CompleteManifest(ctx, "project", "0", "runner", inputs[0])
	assertSealCode(t, err, "ManifestInProgress")
	_, _, err = s.BeginUpload("project", "0", "new", UploadMetadata{RelativePath: "new", Size: 1}, 4096)
	assertSealCode(t, err, "ManifestSealing")
	close(release)
	for range inputs {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if signer.maximum.Load() != 2 {
		t.Fatalf("maximum = %d", signer.maximum.Load())
	}
}

func TestSealCancellationCanRetry(t *testing.T) {
	release := make(chan struct{})
	signer := &sealTestSigner{key: "key", started: make(chan struct{}, 1), release: release}
	s, _ := newSealTestStore(t, t.TempDir(), signer, 1)
	_, f := sealUpload(t, s, "job", "packages/pkg.rpm")
	in := CompleteManifestRequest{Files: []ManifestFile{f}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := s.CompleteManifest(ctx, "project", "job", "runner", in); done <- err }()
	select {
	case <-signer.started:
	case <-time.After(time.Second * 5):
		t.Fatal("signing not started")
	}
	cancel()
	assertSealCode(t, <-done, "SigningBackendUnavailable")
	close(release)
	if _, err := s.CompleteManifest(context.Background(), "project", "job", "runner", in); err != nil {
		t.Fatal(err)
	}
}

func TestManifestDigestContractVector(t *testing.T) {
	files := []ManifestFile{
		{ArtifactID: "rpm-1", RelativePath: "packages/pkg.rpm", Category: CategoryArtifact, Size: 3, SHA256: "rpm-sha", Required: true},
		{ArtifactID: "log-1", RelativePath: "logs/container.log", Category: CategoryLog, Size: 12, SHA256: "log-sha", Required: true},
	}
	const expected = "sha256:bef0ca66e6a994208d195fa6a279f83f4c0e944cb2b38fd4a818686dba8de8a7"
	if actual := ManifestDigest(files); actual != expected {
		t.Fatalf("digest=%s", actual)
	}
	if files[0].ArtifactID != "rpm-1" {
		t.Fatal("input order mutated")
	}
}

func TestSealPreservesCheckpointWhenSignedFileIsMissing(t *testing.T) {
	signer := &sealTestSigner{key: "key", failAt: 2}
	s, _ := newSealTestStore(t, t.TempDir(), signer, 1)
	_, first := sealUpload(t, s, "job", "packages/a.rpm")
	_, second := sealUpload(t, s, "job", "packages/b.rpm")
	in := CompleteManifestRequest{Files: []ManifestFile{first, second}}
	_, err := s.CompleteManifest(context.Background(), "project", "job", "runner", in)
	assertSealCode(t, err, "SigningBackendUnavailable")
	if err := os.Remove(filepath.Join(s.root, signedStorageKey("project", "job", first.ArtifactID))); err != nil {
		t.Fatal(err)
	}
	_, err = s.CompleteManifest(context.Background(), "project", "job", "runner", in)
	assertSealCode(t, err, "SignedPackageVerificationFailed")
	if signer.calls.Load() != 2 {
		t.Fatal("replaced previously checkpointed bytes")
	}
}

func TestSealChecksFinalSizeLimits(t *testing.T) {
	for _, limit := range []string{"file", "job"} {
		t.Run(limit, func(t *testing.T) {
			s, pool := newSealTestStore(t, t.TempDir(), &sealTestSigner{key: "key"}, 1)
			_, f := sealUpload(t, s, "job", "packages/a.rpm")
			if limit == "file" {
				pool.config.MaxFileSize = f.Size
			} else {
				pool.config.MaxJobSize = f.Size
			}
			_, err := s.CompleteManifest(context.Background(), "project", "job", "runner",
				CompleteManifestRequest{Files: []ManifestFile{f}})
			assertSealCode(t, err, "ArtifactLimitExceeded")
			if _, err := s.GetManifest("project", "job"); !os.IsNotExist(err) {
				t.Fatalf("oversized manifest published: %v", err)
			}
		})
	}
}

func TestSealValidatesPackageIdentityAfterSigning(t *testing.T) {
	s, pool := newSealTestStore(t, t.TempDir(), &sealTestSigner{key: "key"}, 1)
	query := `#!/bin/sh
case "$2" in
--qf)
  case "$4" in
  */.signing/*) version=2;;
  *) version=1;;
  esac
  printf 'pkg\t0\t%s\t1\tx86_64\tpkg-1-1.src.rpm\tpayload-digest' "$version";;
--provides) printf 'pkg = 1-1\n';;
--requires) printf 'libc.so.6\n';;
esac
`
	if err := os.WriteFile(pool.query, []byte(query), 0755); err != nil {
		t.Fatal(err)
	}
	_, f := sealUpload(t, s, "job", "packages/pkg.rpm")
	_, err := s.CompleteManifest(context.Background(), "project", "job", "runner",
		CompleteManifestRequest{Files: []ManifestFile{f}})
	assertSealCode(t, err, "SignedPackageMetadataMismatch")
}

func TestSealerQueueBackpressure(t *testing.T) {
	_, pool := newSealTestStore(t, t.TempDir(), &sealTestSigner{key: "key"}, 1)
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	done := make(chan error, cap(pool.tasks)+1)
	pool.tasks <- signingTask{ctx: context.Background(), done: done, run: func(context.Context) error {
		close(started)
		<-release
		return nil
	}}
	<-started
	for i := 0; i < cap(pool.tasks); i++ {
		pool.tasks <- signingTask{ctx: context.Background(), done: done, run: func(context.Context) error { return nil }}
	}
	err := pool.run(context.Background(), []func(context.Context) error{func(context.Context) error { return nil }})
	assertSealCode(t, err, "SigningBusy")
}

func TestSealerShutdownDrainsActiveRequests(t *testing.T) {
	_, pool := newSealTestStore(t, t.TempDir(), &sealTestSigner{key: "key"}, 1)
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- pool.run(context.Background(), []func(context.Context) error{func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}})
	}()
	<-started
	pool.Close()
	assertSealCode(t, <-done, "SigningBackendUnavailable")
}
