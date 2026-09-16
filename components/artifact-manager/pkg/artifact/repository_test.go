package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type testRepositoryMaterializer struct {
	started chan struct{}
	release chan struct{}
}

func (m *testRepositoryMaterializer) Materialize(ctx context.Context, record RepositoryRecord) (repositoryResult, error) {
	select {
	case m.started <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return repositoryResult{}, ctx.Err()
	case <-m.release:
	}
	return repositoryResult{Digest: "digest", RPMs: map[string]RepositoryRPMMeta{"test.rpm": {FileName: "test.rpm"}}}, nil
}

func newRepositoryTestServer(t *testing.T, materializer repositoryMaterializer) (*Server, CreateRepositoryRequest) {
	t.Helper()
	c := DefaultConfig()
	c.DataDir = t.TempDir()
	c.RepositoryWorkers = 1
	c.RepositoryQueueCapacity = 2
	store, err := NewStore(c.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	server, err := newArtifactServer(c, testAuthorizer{}, store, materializer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.repositories.stop(); server.releases.stop() })
	request := CreateRepositoryRequest{RepositoryName: "build-1", Project: "project-1", BuildName: "build-1", TargetOS: "openEuler", TargetArch: "x86_64", Manifests: []ManifestReference{{JobName: "job-1", JobUID: "job-uid-1"}}}
	request.RepositoryUID = repositoryUID(request.Project, request.BuildName, request.BaseRepositoryUID, request.Manifests)
	return server, request
}

func repositoryRequest(t *testing.T, server http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(data))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	return response
}

func TestRepositoryMaterializationLifecycle(t *testing.T) {
	materializer := &testRepositoryMaterializer{started: make(chan struct{}, 1), release: make(chan struct{})}
	server, request := newRepositoryTestServer(t, materializer)

	response := repositoryRequest(t, server, http.MethodPost, "/internal/v1/repositories", request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("submit status = %d: %s", response.Code, response.Body.String())
	}
	select {
	case <-materializer.started:
	case <-time.After(time.Second):
		t.Fatal("materializer was not started")
	}
	if response := repositoryRequest(t, server, http.MethodPost, "/internal/v1/repositories", request); response.Code != http.StatusAccepted {
		t.Fatalf("duplicate submit status = %d", response.Code)
	}
	close(materializer.release)
	deadline := time.Now().Add(time.Second)
	for {
		response = repositoryRequest(t, server, http.MethodGet, "/internal/v1/repositories/"+request.RepositoryUID, nil)
		var state RepositoryResponse
		if err := json.Unmarshal(response.Body.Bytes(), &state); err == nil && state.State == RepositoryReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("repository did not become ready: %s", response.Body.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if response := repositoryRequest(t, server, http.MethodPost, "/internal/v1/repositories", request); response.Code != http.StatusOK {
		t.Fatalf("ready replay status = %d", response.Code)
	}
	conflict := request
	conflict.TargetOS = "different"
	if response := repositoryRequest(t, server, http.MethodPost, "/internal/v1/repositories", conflict); response.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d: %s", response.Code, response.Body.String())
	}
	if response := repositoryRequest(t, server, http.MethodDelete, "/internal/v1/repositories/"+request.RepositoryUID, nil); response.Code != http.StatusAccepted {
		t.Fatalf("delete status = %d", response.Code)
	}
}

func TestRepositoryContent(t *testing.T) {
	materializer := &testRepositoryMaterializer{started: make(chan struct{}, 1), release: make(chan struct{})}
	close(materializer.release)
	server, request := newRepositoryTestServer(t, materializer)
	directory := filepath.Join(repositoryVersionPath(server.cfg.DataDir, request.Project, request.TargetArch, request.BuildName, request.RepositoryUID), "repodata")
	if err := os.MkdirAll(directory, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "repomd.xml"), []byte("metadata"), 0640); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record := &RepositoryRecord{RepositoryUID: request.RepositoryUID, Project: request.Project, BuildName: request.BuildName, TargetArch: request.TargetArch, State: RepositoryReady, ContentURL: "/repositories/v1/" + request.RepositoryUID + "/", CreatedAt: now, UpdatedAt: now}
	server.repositories.mu.Lock()
	server.repositories.records[request.RepositoryUID] = record
	server.repositories.mu.Unlock()
	response := repositoryRequest(t, server, http.MethodGet, record.ContentURL+"repodata/repomd.xml", nil)
	if response.Code != http.StatusOK || response.Body.String() != "metadata" || response.Header().Get("ETag") == "" {
		t.Fatalf("content response = %d %q, etag=%q", response.Code, response.Body.String(), response.Header().Get("ETag"))
	}
}
