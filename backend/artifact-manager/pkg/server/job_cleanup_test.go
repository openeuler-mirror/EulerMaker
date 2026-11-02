package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"artifact-manager/pkg/upload"
)

func TestDeleteJobDataAbsentIsIdempotent(t *testing.T) {
	h, _ := testHandler(t)
	for range 2 {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/internal/v1/projects/project-1/jobs/build", nil))
		if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
			t.Fatalf("delete absent Job data: status=%d body=%s", response.Code, response.Body.String())
		}
	}
}

func TestDeleteJobDataStorageFailureCanRetry(t *testing.T) {
	h, cfg := testHandler(t)
	parent := filepath.Join(cfg.DataDir, "artifacts/project-1/jobs")
	if err := os.MkdirAll(filepath.Dir(parent), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parent, []byte("invalid directory"), 0600); err != nil {
		t.Fatal(err)
	}
	response := repositoryRequest(t, h, http.MethodDelete, "/internal/v1/projects/project-1/jobs/build", nil)
	var failure struct {
		Code      string `json:"code"`
		Retryable bool   `json:"retryable"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusServiceUnavailable || failure.Code != "JobDataCleanupFailed" ||
		!failure.Retryable || response.Header().Get("Retry-After") != "5" {
		t.Fatalf("cleanup failure = %d %s", response.Code, response.Body.String())
	}
	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	jobDir := filepath.Join(parent, "build")
	if err := os.MkdirAll(jobDir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "residue"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	response = repositoryRequest(t, h, http.MethodDelete, "/internal/v1/projects/project-1/jobs/build", nil)
	if response.Code != http.StatusNoContent {
		t.Fatalf("retry = %d %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
		t.Fatalf("Job directory remains after retry: %v", err)
	}
}

func TestDeleteJobDataRequestValidation(t *testing.T) {
	h, _ := testHandler(t)
	for _, test := range []struct {
		name, path, body string
		length           int64
	}{
		{name: "invalid project", path: "/internal/v1/projects/../jobs/build"},
		{name: "invalid job", path: "/internal/v1/projects/project-1/jobs/.."},
		{name: "body", path: "/internal/v1/projects/project-1/jobs/build", body: "{}", length: 2},
		{name: "unknown body length", path: "/internal/v1/projects/project-1/jobs/build", body: "{}", length: -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodDelete, test.path, bytes.NewBufferString(test.body))
			request.ContentLength = test.length
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("invalid request = %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestDeleteJobDataCleansUploadsAndPreservesRepositories(t *testing.T) {
	h, cfg := testHandler(t)
	data := []byte("rpm payload")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, uploadRequest(t, data, "cleanup-upload"))
	if response.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", response.Code, response.Body.String())
	}
	artifact := decodeBody[struct {
		Artifact upload.Artifact `json:"artifact"`
	}](t, response).Artifact
	manifest := upload.CompleteManifestRequest{Files: []upload.ManifestFile{
		{
			ArtifactID: artifact.ID, RelativePath: artifact.RelativePath, Category: artifact.Category,
			Size: artifact.Size, SHA256: artifact.SHA256, Required: true,
		},
	}}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost,
		"/artifacts/v1/projects/project-1/jobs/build/manifest/complete", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer runner-token")
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("complete manifest: %d %s", response.Code, response.Body.String())
	}
	chunk := []byte("build log\n")
	request = httptest.NewRequest(http.MethodPost,
		"/artifacts/v1/projects/project-1/jobs/build/logs/chunks", bytes.NewReader(chunk))
	request.Header.Set("Authorization", "Bearer runner-token")
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-Log-Stream", "combined")
	request.Header.Set("X-Log-Sequence", "0")
	request.Header.Set("X-Content-SHA256", sum(chunk))
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("append log: %d %s", response.Code, response.Body.String())
	}
	write := func(relative string, content []byte) string {
		t.Helper()
		path := filepath.Join(cfg.DataDir, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0640); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// Persisted residue must be removed even without an active upload or signing worker.
	write("artifacts/project-1/jobs/build/.uploads/"+artifact.ID+".tmp", data)
	write("artifacts/project-1/jobs/build/.signed/"+artifact.ID+".rpm", data)
	write("artifacts/project-1/jobs/build/.signing/work.rpm", data)
	write("artifacts/project-1/jobs/build/.seal.json", []byte("{}"))
	write("artifacts/project-1/jobs/build/.quarantine/"+artifact.ID+".corrupted", data)
	write("artifacts/project-1/jobs/build/logs/container.log", chunk)
	retained := map[string][]byte{
		"artifacts/project-1/jobs/other/packages/other.rpm":                          data,
		"artifacts/project-2/jobs/build/packages/other.rpm":                          data,
		"artifacts/project-1/jobs/other/.uploads/unrelated.tmp":                      data,
		"artifacts/project-1/jobs/other/.artifacts/marker":                           data,
		"artifacts/project-1/jobs/other/logs/combined.log":                           chunk,
		"artifacts/project-1/jobs/other/.signed/other.rpm":                           data,
		"artifacts/project-1/jobs/other/.signing/work.rpm":                           data,
		"artifacts/project-1/jobs/other/.quarantine/other.corrupted":                 data,
		"repositories/project-1/x86_64/history/build/steps/repo/repodata/repomd.xml": []byte("process metadata"),
		"repositories/project-1/x86_64/releases/build/repodata/repomd.xml":           []byte("release metadata"),
	}
	for path, content := range retained {
		write(path, content)
	}
	for _, path := range []string{
		"repositories/project-1/x86_64/history/build/steps/repo/Packages/kernel.rpm",
		"repositories/project-1/x86_64/releases/build/Packages/kernel.rpm",
	} {
		full := filepath.Join(cfg.DataDir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(cfg.DataDir, artifact.StorageKey), full); err != nil {
			t.Fatal(err)
		}
		retained[path] = data
	}
	current := filepath.Join(cfg.DataDir, "repositories/project-1/x86_64/current")
	if err := os.Symlink("releases/build", current); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		response = httptest.NewRecorder()
		h.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/internal/v1/projects/project-1/jobs/build", nil))
		if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
			t.Fatalf("delete Job data: status=%d body=%s", response.Code, response.Body.String())
		}
	}
	for _, relative := range []string{
		"artifacts/project-1/jobs/build",
	} {
		if _, err := os.Stat(filepath.Join(cfg.DataDir, relative)); !os.IsNotExist(err) {
			t.Errorf("Job data remains at %s: %v", relative, err)
		}
	}
	for relative, want := range retained {
		got, err := os.ReadFile(filepath.Join(cfg.DataDir, relative))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("retained %s changed: %q, %v", relative, got, err)
		}
	}
	if target, err := os.Readlink(current); err != nil || target != "releases/build" {
		t.Fatalf("current changed: %q, %v", target, err)
	}
	for _, suffix := range []string{"manifest", "logs/content", "artifacts/" + artifact.ID + "/content"} {
		response = httptest.NewRecorder()
		h.ServeHTTP(response, httptest.NewRequest(http.MethodGet,
			"/artifacts/v1/projects/project-1/jobs/build/"+suffix, nil))
		if response.Code != http.StatusNotFound {
			t.Errorf("deleted %s: status=%d body=%s", suffix, response.Code, response.Body.String())
		}
	}
}
