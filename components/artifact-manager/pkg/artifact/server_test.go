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
	h, _ := testHandler(t)
	data := []byte("rpm payload")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, uploadRequest(t, data, "upload-1"))
	if w.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	created := decodeBody[struct {
		Artifact Artifact `json:"artifact"`
	}](t, w)

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

func TestManifestAndRealtimeLog(t *testing.T) {
	h, _ := testHandler(t)
	data := []byte("rpm payload")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, uploadRequest(t, data, "upload-1"))
	a := decodeBody[struct {
		Artifact Artifact `json:"artifact"`
	}](t, w).Artifact

	manifest := CompleteManifestRequest{JobUID: "uid-1", Generation: 1, Files: []ManifestFile{{ArtifactID: a.ID, RelativePath: a.RelativePath, Category: a.Category, Size: a.Size, SHA256: a.SHA256, Required: true}}}
	b, _ := json.Marshal(manifest)
	r := httptest.NewRequest(http.MethodPost, "/artifacts/v1/projects/project-1/jobs/build/manifest/complete", bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer runner-token")
	r.Header.Set("Idempotency-Key", "manifest-1")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("manifest: %d %s", w.Code, w.Body.String())
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
	if _, err := s.AppendLog("project", "job", "uid-2", "runner", 0, chunk, sum(chunk)); err != nil {
		t.Fatal(err)
	}
	body, _, _ := s.logPaths("project", "uid-2")
	f, err := os.OpenFile(body, os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(f, "uncommitted-tail")
	_ = f.Close()
	s, err = NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	l, ok := s.GetLog("project", "job", "uid-2")
	if !ok || l.CommittedBytes != int64(len(chunk)) {
		t.Fatalf("log was not recovered: %#v", l)
	}
	content, _ := os.ReadFile(body)
	if !bytes.Equal(content, chunk) {
		t.Fatalf("uncommitted tail was not truncated: %q", content)
	}
}
