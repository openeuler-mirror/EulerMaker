package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
