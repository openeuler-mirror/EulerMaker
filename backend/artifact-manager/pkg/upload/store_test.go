package upload_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"artifact-manager/pkg/upload"
)

func sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func TestStoreRecoversPendingCommittedUploadAndLogTail(t *testing.T) {
	root := t.TempDir()
	s, err := upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	m := upload.UploadMetadata{Category: upload.CategoryArtifact, FileName: "x", RelativePath: "x", Size: 3, SHA256: sum([]byte("abc"))}
	a, _, err := s.BeginUpload("project", "job", "runner", "key", m, 1024)
	if err != nil {
		t.Fatal(err)
	}
	final := s.ArtifactPath(a)
	if err := os.MkdirAll(filepath.Dir(final), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(final, []byte("abc"), 0640); err != nil {
		t.Fatal(err)
	}
	s, err = upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	recovered, ok := s.GetArtifact(a.ID)
	if !ok || recovered.State != upload.Completed {
		t.Fatalf("artifact was not recovered: %#v", recovered)
	}

	chunk := []byte("committed")
	if _, err := s.AppendLog("project", "another-job", "runner", 0, chunk, sum(chunk)); err != nil {
		t.Fatal(err)
	}
	body, _, _ := s.LogPaths("project", "another-job")
	f, err := os.OpenFile(body, os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(f, "uncommitted-tail")
	_ = f.Close()
	s, err = upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	l, ok := s.GetLog("project", "another-job")
	if !ok || l.CommittedBytes != int64(len(chunk)) {
		t.Fatalf("log was not recovered: %#v", l)
	}
	content, _ := os.ReadFile(body)
	if !bytes.Equal(content, chunk) {
		t.Fatalf("uncommitted tail was not truncated: %q", content)
	}
}

func TestStoreRestartOnlyVerifiesProcessingUploads(t *testing.T) {
	root := t.TempDir()
	store, err := upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	metadata := upload.UploadMetadata{Category: upload.CategoryArtifact, FileName: "package.rpm", RelativePath: "packages/package.rpm", Size: 3, SHA256: sum([]byte("abc"))}
	artifact, _, err := store.BeginUpload("project", "job", "runner", "key", metadata, 1024)
	if err != nil {
		t.Fatal(err)
	}
	temporary := filepath.Join(root, ".uploads", artifact.ID+".tmp")
	if err := os.WriteFile(temporary, []byte("abc"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteUpload(artifact, temporary); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.ArtifactPath(artifact), []byte("bad"), 0640); err != nil {
		t.Fatal(err)
	}

	store, err = upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	loaded, ok := store.GetArtifact(artifact.ID)
	if !ok || loaded.State != upload.Completed {
		t.Fatalf("completed upload changed during startup: %+v", loaded)
	}
	_, replay, err := store.BeginUpload("project", "job", "runner", "key", metadata, 1024)
	if err != nil || replay {
		t.Fatalf("corrupted completed upload replayed: replay=%t, error=%v", replay, err)
	}
}

func TestStoreUploadIdempotencySurvivesRestart(t *testing.T) {
	root := t.TempDir()
	store, err := upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	metadata := upload.UploadMetadata{Category: upload.CategoryArtifact, FileName: "package.rpm", RelativePath: "packages/package.rpm", Size: 3, SHA256: sum([]byte("abc"))}
	artifact, replay, err := store.BeginUpload("project", "job", "runner", "key", metadata, 1024)
	if err != nil || replay {
		t.Fatalf("begin upload: replay=%t, error=%v", replay, err)
	}
	temporary := filepath.Join(root, ".uploads", artifact.ID+".tmp")
	if err := os.WriteFile(temporary, []byte("abc"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteUpload(artifact, temporary); err != nil {
		t.Fatal(err)
	}
	store, err = upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	loaded, ok := store.GetArtifact(artifact.ID)
	if !ok || loaded.IdempotencyKey != "key" {
		t.Fatalf("upload identity was not persisted: %+v", loaded)
	}
	replayed, replay, err := store.BeginUpload("project", "job", "runner", "key", metadata, 1024)
	if err != nil || !replay || replayed.ID != artifact.ID {
		t.Fatalf("replay after restart: artifact=%+v, replay=%t, error=%v", replayed, replay, err)
	}
	metadata.ContentType = "application/x-rpm"
	if _, _, err := store.BeginUpload("project", "job", "runner", "key", metadata, 1024); err == nil || err.Error() != "IdempotencyConflict" {
		t.Fatalf("changed request reused key: %v", err)
	}
}

func TestStoreRetryFailedUploadAfterRestart(t *testing.T) {
	root := t.TempDir()
	store, err := upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	metadata := upload.UploadMetadata{Category: upload.CategoryArtifact, FileName: "package.rpm", RelativePath: "packages/package.rpm", Size: 3, SHA256: sum([]byte("abc"))}
	artifact, _, err := store.BeginUpload("project", "job", "runner", "key", metadata, 1024)
	if err != nil {
		t.Fatal(err)
	}
	store, err = upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	retried, replay, err := store.BeginUpload("project", "job", "runner", "key", metadata, 1024)
	if err != nil || replay || retried.ID != artifact.ID || retried.State != upload.Pending {
		t.Fatalf("retry after restart: artifact=%+v, replay=%t, error=%v", retried, replay, err)
	}
}
