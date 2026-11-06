package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"artifact-manager/pkg/fileutil"
	"artifact-manager/pkg/repository"
	"artifact-manager/pkg/upload"
)

type testRepositoryMaterializer struct {
	started chan struct{}
	release chan struct{}
	root    string
}

type badManifestMaterializer struct{}

func (badManifestMaterializer) Materialize(context.Context, repository.RepositoryRecord) (repository.Result, error) {
	return repository.Result{}, &repository.Error{Code: "ManifestInvalid", Status: 422, JobName: "job-1"}
}

func TestRepositoryFailurePersistsOffendingJobName(t *testing.T) {
	server, request := newRepositoryTestServer(t, badManifestMaterializer{})
	if _, _, err := server.repositories.Submit(request); err != nil {
		t.Fatalf("submit: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		record, ok, err := server.repositories.Get(request.RepositoryUID)
		if err != nil {
			t.Fatal(err)
		}
		if ok && record.State == repository.RepositoryFailed {
			if record.Failure == nil || record.Failure.JobName != "job-1" {
				t.Fatalf("failure lost offending Job name: %+v", record.Failure)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("repository did not fail")
}

func (m *testRepositoryMaterializer) Materialize(ctx context.Context, record repository.RepositoryRecord) (repository.Result, error) {
	select {
	case m.started <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return repository.Result{}, ctx.Err()
	case <-m.release:
	}
	if m.root != "" {
		path := repository.VersionPath(m.root, record.Project, record.TargetArch, record.BuildName, record.RepositoryUID)
		if err := os.MkdirAll(path, 0750); err != nil {
			return repository.Result{}, err
		}
		index := repository.Index{RepositoryUID: record.RepositoryUID, RPMs: map[string]repository.RepositoryRPMMeta{}}
		if err := fileutil.AtomicJSON(filepath.Join(path, ".repository.json"), &index); err != nil {
			return repository.Result{}, err
		}
	}
	return repository.Result{Digest: "digest"}, nil
}

func newRepositoryTestServer(t *testing.T, materializer repository.Materializer) (*Server, repository.CreateRepositoryRequest) {
	t.Helper()
	c := DefaultConfig()
	c.DataDir = t.TempDir()
	if fake, ok := materializer.(*testRepositoryMaterializer); ok {
		fake.root = c.DataDir
	}
	c.Repository.Workers = 1
	c.Repository.QueueCapacity = 2
	store, err := upload.NewStore(c.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	server, err := newServer(c, testAuthorizer{}, store, materializer, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.repositories.Stop(); server.releases.Stop() })
	request := repository.CreateRepositoryRequest{
		RepositoryName: "build-1",
		Project:        "project-1",
		BuildName:      "build-1",
		TargetArch:     "x86_64",
		Manifests:      []repository.ManifestReference{{JobName: "job-1"}},
	}
	request.RepositoryUID = repository.UID(request.Project, request.BuildName, request.BaseRepositoryUID, request.Manifests)
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
		if method == http.MethodPost && strings.Contains(path, "/builds/") {
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(data, &payload); err != nil {
				t.Fatal(err)
			}
			delete(payload, "project")
			delete(payload, "buildName")
			data, err = json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
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

func buildManagementPath(project, build string) string {
	return "/internal/v1/projects/" + project + "/builds/" + build
}

func TestRepositoryMaterializationLifecycle(t *testing.T) {
	materializer := &testRepositoryMaterializer{started: make(chan struct{}, 1), release: make(chan struct{})}
	server, request := newRepositoryTestServer(t, materializer)

	response := repositoryRequest(t, server, http.MethodPost, buildManagementPath(request.Project, request.BuildName)+"/repositories", request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("submit status = %d: %s", response.Code, response.Body.String())
	}
	location := buildManagementPath(request.Project, request.BuildName) + "/repositories/" + request.RepositoryUID
	if response.Header().Get("Location") != location {
		t.Fatalf("repository location = %q", response.Header().Get("Location"))
	}
	select {
	case <-materializer.started:
	case <-time.After(time.Second):
		t.Fatal("materializer was not started")
	}
	if response := repositoryRequest(t, server, http.MethodPost, buildManagementPath(request.Project, request.BuildName)+"/repositories", request); response.Code != http.StatusAccepted {
		t.Fatalf("duplicate submit status = %d", response.Code)
	}
	close(materializer.release)
	deadline := time.Now().Add(time.Second)
	for {
		response = repositoryRequest(t, server, http.MethodGet, buildManagementPath(request.Project, request.BuildName)+"/repositories/"+request.RepositoryUID, nil)
		var state repository.RepositoryResponse
		if err := json.Unmarshal(response.Body.Bytes(), &state); err == nil && state.State == repository.RepositoryReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("repository did not become ready: %s", response.Body.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if response := repositoryRequest(t, server, http.MethodPost, buildManagementPath(request.Project, request.BuildName)+"/repositories", request); response.Code != http.StatusOK {
		t.Fatalf("ready replay status = %d", response.Code)
	}
	for _, scope := range [][2]string{{"other-project", request.BuildName}, {request.Project, "other-build"}} {
		path := buildManagementPath(scope[0], scope[1]) + "/repositories/" + request.RepositoryUID
		if response := repositoryRequest(t, server, http.MethodGet, path, nil); response.Code != http.StatusNotFound {
			t.Fatalf("repository scope %v: %d %s", scope, response.Code, response.Body.String())
		}
	}
	conflict := request
	conflict.TargetArch = "different"
	if response := repositoryRequest(t, server, http.MethodPost, buildManagementPath(request.Project, request.BuildName)+"/repositories", conflict); response.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d: %s", response.Code, response.Body.String())
	}
	if response := repositoryRequest(t, server, http.MethodDelete, buildManagementPath(request.Project, request.BuildName)+"/repositories", nil); response.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", response.Code)
	}
	deadline = time.Now().Add(time.Second)
	for {
		response = repositoryRequest(t, server, http.MethodGet, buildManagementPath(request.Project, request.BuildName)+"/repositories/"+request.RepositoryUID, nil)
		if response.Code == http.StatusNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("repository was not removed: %d %s", response.Code, response.Body.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRepositoryContent(t *testing.T) {
	materializer := &testRepositoryMaterializer{started: make(chan struct{}, 1), release: make(chan struct{})}
	close(materializer.release)
	server, request := newRepositoryTestServer(t, materializer)
	directory := filepath.Join(server.cfg.DataDir, "repositories", request.Project, request.TargetArch, "history", request.BuildName, "steps", request.RepositoryUID, "repodata")
	if err := os.MkdirAll(directory, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "repomd.xml"), []byte("metadata"), 0640); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record := &repository.RepositoryRecord{
		RepositoryUID: request.RepositoryUID,
		Project:       request.Project,
		BuildName:     request.BuildName,
		TargetArch:    request.TargetArch,
		State:         repository.RepositoryReady,
		ContentURL:    "/repositories/v1/" + request.RepositoryUID + "/",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	writeTestRepositoryIndex(t, server.repositories, record)
	writeTestRepositoryRecord(t, server, record)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		response := repositoryRequest(t, server, method, record.ContentURL+".repository.json", nil)
		if response.Code != http.StatusNotFound {
			t.Fatalf("internal repository index %s = %d", method, response.Code)
		}
	}
	response := repositoryRequest(t, server, http.MethodGet, record.ContentURL+"repodata/repomd.xml", nil)
	if response.Code != http.StatusOK || response.Body.String() != "metadata" || response.Header().Get("ETag") == "" {
		t.Fatalf("content response = %d %q, etag=%q", response.Code, response.Body.String(), response.Header().Get("ETag"))
	}
	rangeRequest := httptest.NewRequest(http.MethodGet, record.ContentURL+"repodata/repomd.xml", nil)
	rangeRequest.Header.Set("Range", "bytes=0-3")
	rangeResponse := httptest.NewRecorder()
	server.ServeHTTP(rangeResponse, rangeRequest)
	if rangeResponse.Code != http.StatusPartialContent || rangeResponse.Body.String() != "meta" {
		t.Fatalf("range response = %d %q", rangeResponse.Code, rangeResponse.Body.String())
	}
}
