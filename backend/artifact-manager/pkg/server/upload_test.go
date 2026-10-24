package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"artifact-manager/pkg/auth"
	"artifact-manager/pkg/repository"
	"artifact-manager/pkg/upload"
)

type testAuthorizer struct{}

func (testAuthorizer) Authenticate(context.Context, string) (auth.Identity, error) {
	return auth.Identity{Name: "runner-1", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func testHandler(t *testing.T) (http.Handler, Config) {
	t.Helper()
	c := DefaultConfig()
	c.DataDir = t.TempDir()
	c.Upload.MaxFileSize = 1 << 20
	c.Upload.MaxJobSize = 2 << 20
	c.Upload.MaxLogSize = 1 << 20
	c.Upload.LogChunkSize = 64 << 10
	h, err := NewHandler(c, testAuthorizer{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h, c
}

func sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func uploadRequest(t *testing.T, data []byte, key string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="metadata"`)
	h.Set("Content-Type", "application/json")
	p, _ := mw.CreatePart(h)
	_ = json.NewEncoder(p).Encode(upload.UploadMetadata{
		Category:     upload.CategoryArtifact,
		FileName:     "kernel.rpm",
		RelativePath: "packages/kernel.rpm",
		ContentType:  "application/x-rpm",
		Size:         int64(len(data)),
		SHA256:       sum(data),
	})
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
		Artifact upload.Artifact `json:"artifact"`
	}](t, w)
	wantKey := "projects/project-1/jobs/build/packages/kernel.rpm"
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
		Artifact upload.Artifact `json:"artifact"`
	}](t, w)
	if replayed.Artifact.ID != created.Artifact.ID {
		t.Fatal("idempotent replay returned another artifact")
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/artifacts/v1/projects/project-1/jobs/build/artifacts", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), created.Artifact.ID) {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/artifacts/v1/projects/project-1/jobs/build/artifacts/"+created.Artifact.ID+"/content", nil))
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatalf("download: %d %q", w.Code, w.Body.Bytes())
	}
	for _, scope := range []string{"project-2/jobs/build", "project-1/jobs/another-job"} {
		w = httptest.NewRecorder()
		path := "/artifacts/v1/projects/" + scope + "/artifacts/" + created.Artifact.ID + "/content"
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("download outside artifact scope %s: %d %s", scope, w.Code, w.Body.String())
		}
	}
}

func TestJobNameDirectoryPersistsAcrossRestart(t *testing.T) {
	root := t.TempDir()
	store, err := upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	meta := upload.UploadMetadata{
		Category:     upload.CategoryArtifact,
		FileName:     "example.rpm",
		RelativePath: "packages/example.rpm",
		Size:         1,
		SHA256:       sum([]byte("x")),
	}
	if _, _, err := store.BeginUpload("project", "job", "key-1", meta, 1024); err != nil {
		t.Fatal(err)
	}
	store, err = upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.BeginUpload("project", "job", "key-2", meta, 1024); err != nil {
		t.Fatalf("same job name should remain valid after restart: %v", err)
	}
	if _, err := store.AppendLog("project", "job", "runner", 0, []byte("x"), sum([]byte("x"))); err != nil {
		t.Fatalf("same job name log: %v", err)
	}
	if _, _, err := store.BeginUpload("project", "another-job", "key-2", meta, 1024); err != nil {
		t.Fatalf("different job name: %v", err)
	}
}

func TestManifestAndRealtimeLog(t *testing.T) {
	h, cfg := testHandler(t)
	data := []byte("rpm payload")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, uploadRequest(t, data, "upload-1"))
	a := decodeBody[struct {
		Artifact upload.Artifact `json:"artifact"`
	}](t, w).Artifact

	manifest := upload.CompleteManifestRequest{
		Files: []upload.ManifestFile{
			{
				ArtifactID:   a.ID,
				RelativePath: a.RelativePath,
				Category:     a.Category,
				Size:         a.Size,
				SHA256:       a.SHA256,
				Required:     true,
			},
		},
	}
	b, _ := json.Marshal(manifest)
	r := httptest.NewRequest(http.MethodPost, "/artifacts/v1/projects/project-1/jobs/build/manifest/complete", bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer runner-token")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("manifest: %d %s", w.Code, w.Body.String())
	}

	r = httptest.NewRequest(http.MethodGet, "/artifacts/v1/projects/project-1/jobs/build/manifest", nil)
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
		r = httptest.NewRequest(http.MethodPost, "/artifacts/v1/projects/project-1/jobs/log-job/logs/chunks", bytes.NewReader(chunk))
		r.Header.Set("Authorization", "Bearer runner-token")
		r.Header.Set("Content-Type", "application/octet-stream")
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
	r = httptest.NewRequest(http.MethodPost, "/artifacts/v1/projects/project-1/jobs/log-job/logs/chunks", bytes.NewReader(chunks[1]))
	r.Header.Set("Authorization", "Bearer runner-token")
	r.Header.Set("Content-Type", "application/octet-stream")
	r.Header.Set("X-Log-Stream", "combined")
	r.Header.Set("X-Log-Sequence", "1")
	r.Header.Set("X-Content-SHA256", sum(chunks[1]))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("chunk replay: %d %s", w.Code, w.Body.String())
	}

	all := bytes.Join(chunks, nil)
	activeDir := filepath.Join(cfg.DataDir, ".logs", "project-1", "log-job")
	if content, err := os.ReadFile(filepath.Join(activeDir, "combined.log")); err != nil || !bytes.Equal(content, all) {
		t.Fatalf("active log at Job name path: %q, %v", content, err)
	}
	if _, err := os.Stat(filepath.Join(activeDir, "combined.index.jsonl")); err != nil {
		t.Fatalf("active log index at Job name path: %v", err)
	}
	complete := upload.CompleteLogRequest{Stream: "combined", LastSequence: 1, Size: int64(len(all)), SHA256: sum(all)}
	b, _ = json.Marshal(complete)
	r = httptest.NewRequest(http.MethodPost, "/artifacts/v1/projects/project-1/jobs/log-job/logs/complete", bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer runner-token")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("complete log: %d %s", w.Code, w.Body.String())
	}
	result := decodeBody[struct {
		ArtifactID string `json:"artifactID"`
	}](t, w)
	if content, err := os.ReadFile(filepath.Join(cfg.DataDir, "projects", "project-1", "jobs", "log-job", "logs", "container.log")); err != nil || !bytes.Equal(content, all) {
		t.Fatalf("stored log: %q, %v", content, err)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/artifacts/v1/projects/project-1/jobs/log-job/artifacts/"+result.ArtifactID+"/content", nil))
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), all) {
		t.Fatalf("log artifact: %d %q", w.Code, w.Body.Bytes())
	}
}

type manifestSigner struct{ calls int }

func (s *manifestSigner) Fingerprint() string { return "test-key" }

func (s *manifestSigner) Sign(_ context.Context, path string) error {
	s.calls++
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = file.WriteString("-signed")
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (*manifestSigner) Verify(_ context.Context, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.HasSuffix(data, []byte("-signed")) {
		return fmt.Errorf("signature missing")
	}
	return nil
}

func TestSignedManifestDownloadAndRepository(t *testing.T) {
	handler, cfg := testHandler(t)
	s := handler.(*Server)
	s.sealer.Close()
	query := filepath.Join(cfg.DataDir, "rpm-query")
	script := `#!/bin/sh
case "$2" in
--qf)
  case "$3" in
  *PAYLOADDIGEST*) printf 'pkg\t0\t1\t1\tx86_64\tpkg-1-1.src.rpm\tpayload';;
  *) printf 'pkg\t0\t1\t1\tx86_64\tpkg-1-1.src.rpm';;
  esac;;
--provides|--requires) ;;
*) exit 1;;
esac
`
	if err := os.WriteFile(query, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	signer := &manifestSigner{}
	pool, err := upload.NewSealer(cfg.Upload, signer, query, 2, time.Second*5)
	if err != nil {
		t.Fatal(err)
	}
	s.sealer = pool
	s.store.SetSealer(pool)
	data := []byte("rpm")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, uploadRequest(t, data, "signed-upload"))
	if w.Code != 201 {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	a := decodeBody[struct {
		Artifact upload.Artifact `json:"artifact"`
	}](t, w).Artifact
	input := upload.CompleteManifestRequest{Files: []upload.ManifestFile{{
		ArtifactID: a.ID, RelativePath: a.RelativePath, Category: a.Category, Size: a.Size, SHA256: a.SHA256, Required: true,
	}}}
	request, _ := json.Marshal(input)
	complete := func() *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/artifacts/v1/projects/project-1/jobs/build/manifest/complete", bytes.NewReader(request))
		r.Header.Set("Authorization", "Bearer runner-token")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	w = complete()
	if w.Code != 200 {
		t.Fatalf("complete: %d %s", w.Code, w.Body.String())
	}
	response := decodeBody[upload.JobUploadManifest](t, w)
	if response.UploadDigest != upload.ManifestDigest(input.Files) || response.Digest == response.UploadDigest {
		t.Fatalf("response = %+v", response)
	}
	if w := complete(); w.Code != 200 || signer.calls != 1 {
		t.Fatalf("replay: %d, calls=%d", w.Code, signer.calls)
	}
	contentURL := "/artifacts/v1/projects/project-1/jobs/build/artifacts/" + a.ID + "/content"
	for _, test := range []struct {
		byteRange, body string
		status          int
	}{
		{"", "rpm-signed", 200}, {"bytes=4-9", "signed", 206},
	} {
		r := httptest.NewRequest(http.MethodGet, contentURL, nil)
		if test.byteRange != "" {
			r.Header.Set("Range", test.byteRange)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != test.status || w.Body.String() != test.body || w.Header().Get("ETag") != `"`+sum([]byte("rpm-signed"))+`"` {
			t.Fatalf("download: %d %s %+v", w.Code, w.Body.String(), w.Header())
		}
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/artifacts/v1/projects/project-1/jobs/build/artifacts", nil))
	list := decodeBody[struct {
		Items []upload.Artifact `json:"items"`
	}](t, w)
	if len(list.Items) != 1 || list.Items[0].SHA256 != sum([]byte("rpm-signed")) || list.Items[0].Size != 10 {
		t.Fatalf("signed list = %+v", list)
	}

	createRepo := filepath.Join(cfg.DataDir, "createrepo")
	if err := os.WriteFile(createRepo, []byte("#!/bin/sh\nfor work; do :; done\nmkdir -p \"$work/repodata\"\nprintf metadata > \"$work/repodata/repomd.xml\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	cfg.Repository.CreateRepoCommand, cfg.Repository.RPMQueryCommand = createRepo, query
	m := repository.NewFilesystemMaterializer(cfg.DataDir, cfg.Repository, s.store, "test-key")
	record := repository.RepositoryRecord{
		RepositoryUID: "test-repo", Project: "project-1", BuildName: "build", TargetOS: "os", TargetArch: "x86_64",
		SigningFingerprint: "test-key", Manifests: []repository.ManifestReference{{JobName: "build"}},
	}
	if _, err := m.Materialize(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	path := repository.VersionPath(cfg.DataDir, record.Project, record.TargetOS, record.TargetArch, record.BuildName, record.RepositoryUID)
	repoFile := filepath.Join(path, "Packages", a.FileName)
	prepared, _ := s.store.GetJobArtifact("project-1", "build", a.ID)
	signedInfo, _ := os.Stat(s.store.ArtifactPath(prepared))
	repoInfo, err := os.Stat(repoFile)
	if err != nil || !os.SameFile(signedInfo, repoInfo) || signer.calls != 1 {
		t.Fatalf("repository did not reuse sealed inode: %v, calls=%d", err, signer.calls)
	}
	if err := os.Remove(s.store.ArtifactPath(prepared)); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, contentURL, nil))
	if w.Code != 503 {
		t.Fatalf("missing signed file: %d %s", w.Code, w.Body.String())
	}
	if content, err := os.ReadFile(repoFile); err != nil || string(content) != "rpm-signed" {
		t.Fatalf("repository changed: %s, %v", content, err)
	}
}
