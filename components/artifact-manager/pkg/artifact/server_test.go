package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testAuthorizer struct{}

func (testAuthorizer) Authenticate(context.Context, string) (Identity, error) {
	return Identity{Name: "runner-1", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func testHandler(t *testing.T) (http.Handler, Config) {
	t.Helper()
	c := DefaultConfig()
	c.DataDir = t.TempDir()
	c.MaxFileSize = 1 << 20
	c.MaxJobSize = 2 << 20
	c.MaxLogSize = 1 << 20
	c.LogChunkSize = 64 << 10
	h, err := NewHandler(c, testAuthorizer{})
	if err != nil {
		t.Fatal(err)
	}
	return h, c
}

func sum(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }

func uploadRequest(t *testing.T, data []byte, key string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="metadata"`)
	h.Set("Content-Type", "application/json")
	p, _ := mw.CreatePart(h)
	_ = json.NewEncoder(p).Encode(UploadMetadata{JobUID: "uid-1", Category: CategoryArtifact, Name: "packages", FileName: "kernel.rpm", RelativePath: "RPMS/kernel.rpm", ContentType: "application/x-rpm", Size: int64(len(data)), SHA256: sum(data)})
	h = make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="file"; filename="ignored"`)
	h.Set("Content-Type", "application/octet-stream")
	p, _ = mw.CreatePart(h)
	_, _ = p.Write(data)
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/artifacts/v1/projects/project-1/jobs/build/artifacts", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("Authorization", "Bearer runner-token")
	r.Header.Set("Idempotency-Key", key)
	return r
}

func decodeBody[T any](t *testing.T, r *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(r.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", r.Body.String(), err)
	}
	return v
}

func TestArtifactUploadReplayListAndDownload(t *testing.T) {
	h, cfg := testHandler(t)
	data := []byte("rpm payload")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, uploadRequest(t, data, "upload-1"))
	if w.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	created := decodeBody[struct {
		Artifact Artifact `json:"artifact"`
	}](t, w)
	wantKey := "projects/project-1/jobs/build/RPMS/kernel.rpm"
	if created.Artifact.StorageKey != wantKey {
		t.Fatalf("storage key = %q, want %q", created.Artifact.StorageKey, wantKey)
	}
	if content, err := os.ReadFile(filepath.Join(cfg.DataDir, filepath.FromSlash(wantKey))); err != nil || !bytes.Equal(content, data) {
		t.Fatalf("stored artifact: %q, %v", content, err)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, uploadRequest(t, data, "upload-1"))
	if w.Code != http.StatusOK {
		t.Fatalf("replay: %d %s", w.Code, w.Body.String())
	}
	replayed := decodeBody[struct {
		Artifact Artifact `json:"artifact"`
	}](t, w)
	if replayed.Artifact.ID != created.Artifact.ID {
		t.Fatal("idempotent replay returned another artifact")
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/artifacts/v1/projects/project-1/jobs/build/artifacts?jobUID=uid-1", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), created.Artifact.ID) {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/artifacts/v1/artifacts/"+created.Artifact.ID+"/content", nil))
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatalf("download: %d %q", w.Code, w.Body.Bytes())
	}
}

func TestJobNameDirectoryIgnoresUID(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	meta := UploadMetadata{JobUID: "uid-1", Category: CategoryArtifact, FileName: "example.rpm", RelativePath: "packages/example.rpm", Size: 1, SHA256: sum([]byte("x"))}
	if _, _, _, err := store.BeginUpload("project", "job", "runner", "key-1", meta, 1024); err != nil {
		t.Fatal(err)
	}
	store, err = NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	meta.JobUID = "uid-2"
	if _, _, _, err := store.BeginUpload("project", "job", "runner", "key-2", meta, 1024); err != nil && err.Error() == "JobIdentityConflict" {
		t.Fatalf("same name must not conflict because UID changed: %v", err)
	}
	if _, err := store.AppendLog("project", "job", "uid-2", "runner", 0, []byte("x"), sum([]byte("x"))); err != nil {
		t.Fatalf("same name log with another UID: %v", err)
	}
	if _, _, _, err := store.BeginUpload("project", "another-job", "runner", "key-2", meta, 1024); err != nil {
		t.Fatalf("different job name: %v", err)
	}
}

func TestLogPathsIgnoreUnsafeLegacyUID(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body, _, _ := store.logPaths("project", "job", "../../escape")
	if !strings.Contains(body, filepath.Join(".logs", "project", "job")) {
		t.Fatalf("unsafe UID affected log path: %s", body)
	}
}

func TestExistingUIDStorageKeyRemainsReadable(t *testing.T) {
	root := t.TempDir()
	oldKey := "projects/project/jobs/uid-1/packages/example.rpm"
	oldPath := filepath.Join(root, filepath.FromSlash(oldKey))
	if err := os.MkdirAll(filepath.Dir(oldPath), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, []byte("x"), 0640); err != nil {
		t.Fatal(err)
	}
	old := &Artifact{ID: "old", Project: "project", JobName: "job", JobUID: "uid-1", StorageKey: oldKey, State: Completed}
	if err := atomicJSON(filepath.Join(root, ".metadata/artifacts/old.json"), old); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	loaded, ok := store.GetArtifact("old")
	if !ok {
		t.Fatal("old artifact missing")
	}
	if content, err := os.ReadFile(store.artifactPath(loaded)); err != nil || string(content) != "x" {
		t.Fatalf("old artifact content: %q, %v", content, err)
	}
}

func TestManifestAndRealtimeLog(t *testing.T) {
	h, cfg := testHandler(t)
	data := []byte("rpm payload")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, uploadRequest(t, data, "upload-1"))
	a := decodeBody[struct {
		Artifact Artifact `json:"artifact"`
	}](t, w).Artifact

	manifest := CompleteManifestRequest{JobUID: "uid-1", Files: []ManifestFile{{ArtifactID: a.ID, RelativePath: a.RelativePath, Category: a.Category, Size: a.Size, SHA256: a.SHA256, Required: true}}}
	b, _ := json.Marshal(manifest)
	r := httptest.NewRequest(http.MethodPost, "/artifacts/v1/projects/project-1/jobs/build/manifest/complete", bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer runner-token")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("manifest: %d %s", w.Code, w.Body.String())
	}

	r = httptest.NewRequest(http.MethodGet, "/artifacts/v1/projects/project-1/jobs/build/manifest?jobUID=uid-1", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("get manifest: %d %s", w.Code, w.Body.String())
	}

	manifest.Files[0].Required = false
	b, _ = json.Marshal(manifest)
	r = httptest.NewRequest(http.MethodPost, "/artifacts/v1/projects/project-1/jobs/build/manifest/complete", bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer runner-token")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("replace completed manifest: %d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, uploadRequest(t, data, "upload-after-manifest"))
	if w.Code != http.StatusConflict {
		t.Fatalf("upload after completed manifest: %d %s", w.Code, w.Body.String())
	}

	chunks := [][]byte{[]byte("hello\n"), []byte("world\n")}
	for sequence, chunk := range chunks {
		r = httptest.NewRequest(http.MethodPost, "/artifacts/v1/projects/project-1/jobs/build/logs/chunks", bytes.NewReader(chunk))
		r.Header.Set("Authorization", "Bearer runner-token")
		r.Header.Set("Content-Type", "application/octet-stream")
		r.Header.Set("X-Job-UID", "uid-1")
		r.Header.Set("X-Log-Stream", "combined")
		r.Header.Set("X-Log-Sequence", fmt.Sprint(sequence))
		r.Header.Set("X-Content-SHA256", sum(chunk))
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("chunk %d: %d %s", sequence, w.Code, w.Body.String())
		}
	}
	// An acknowledged chunk can be retried without appending it twice.
	r = httptest.NewRequest(http.MethodPost, "/artifacts/v1/projects/project-1/jobs/build/logs/chunks", bytes.NewReader(chunks[1]))
	r.Header.Set("Authorization", "Bearer runner-token")
	r.Header.Set("Content-Type", "application/octet-stream")
	r.Header.Set("X-Job-UID", "uid-1")
	r.Header.Set("X-Log-Stream", "combined")
	r.Header.Set("X-Log-Sequence", "1")
	r.Header.Set("X-Content-SHA256", sum(chunks[1]))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("chunk replay: %d %s", w.Code, w.Body.String())
	}

	all := bytes.Join(chunks, nil)
	activeDir := filepath.Join(cfg.DataDir, ".logs", "project-1", "build")
	if content, err := os.ReadFile(filepath.Join(activeDir, "combined.log")); err != nil || !bytes.Equal(content, all) {
		t.Fatalf("active log at Job name path: %q, %v", content, err)
	}
	if _, err := os.Stat(filepath.Join(activeDir, "combined.index.jsonl")); err != nil {
		t.Fatalf("active log index at Job name path: %v", err)
	}
	complete := CompleteLogRequest{JobUID: "uid-1", Stream: "combined", LastSequence: 1, Size: int64(len(all)), SHA256: sum(all)}
	b, _ = json.Marshal(complete)
	r = httptest.NewRequest(http.MethodPost, "/artifacts/v1/projects/project-1/jobs/build/logs/complete", bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer runner-token")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("complete log: %d %s", w.Code, w.Body.String())
	}
	result := decodeBody[struct {
		ArtifactID string `json:"artifactID"`
	}](t, w)
	if content, err := os.ReadFile(filepath.Join(cfg.DataDir, "projects", "project-1", "jobs", "build", "logs", "container.log")); err != nil || !bytes.Equal(content, all) {
		t.Fatalf("stored log: %q, %v", content, err)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/artifacts/v1/artifacts/"+result.ArtifactID+"/content", nil))
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), all) {
		t.Fatalf("log artifact: %d %q", w.Code, w.Body.Bytes())
	}
}

func TestStoreRecoversPendingCommittedUploadAndLogTail(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	m := UploadMetadata{JobUID: "uid-1", Category: CategoryArtifact, FileName: "x", RelativePath: "x", Size: 3, SHA256: sum([]byte("abc"))}
	a, ir, _, err := s.BeginUpload("project", "job", "runner", "key", m, 1024)
	if err != nil {
		t.Fatal(err)
	}
	final := s.artifactPath(a)
	if err := os.MkdirAll(filepath.Dir(final), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(final, []byte("abc"), 0640); err != nil {
		t.Fatal(err)
	}
	if ir.State != IdempotencyProcessing {
		t.Fatal("unexpected setup state")
	}
	s, err = NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	recovered, ok := s.GetArtifact(a.ID)
	if !ok || recovered.State != Completed {
		t.Fatalf("artifact was not recovered: %#v", recovered)
	}

	chunk := []byte("committed")
	if _, err := s.AppendLog("project", "another-job", "uid-2", "runner", 0, chunk, sum(chunk)); err != nil {
		t.Fatal(err)
	}
	body, _, _ := s.logPaths("project", "another-job", "uid-2")
	f, err := os.OpenFile(body, os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(f, "uncommitted-tail")
	_ = f.Close()
	legacyDir := filepath.Join(root, ".logs", "project", "uid-2")
	if err := os.Rename(filepath.Dir(body), legacyDir); err != nil {
		t.Fatal(err)
	}
	body = filepath.Join(legacyDir, "combined.log")
	s, err = NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	l, ok := s.GetLog("project", "another-job", "uid-2")
	if !ok || l.CommittedBytes != int64(len(chunk)) {
		t.Fatalf("log was not recovered: %#v", l)
	}
	content, _ := os.ReadFile(body)
	if !bytes.Equal(content, chunk) {
		t.Fatalf("uncommitted tail was not truncated: %q", content)
	}
}
