package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"artifact-manager/pkg/fileutil"
	"artifact-manager/pkg/release"
	"artifact-manager/pkg/repository"
	"artifact-manager/pkg/upload"
)

type testReleaseMaterializer struct {
	root     string
	started  chan struct{}
	release  chan struct{}
	rpmCount chan int
}

func (m *testReleaseMaterializer) Create(ctx context.Context, record release.ReleaseRecord, source repository.RepositoryRecord) (release.Result, error) {
	if m.rpmCount != nil {
		m.rpmCount <- len(source.RPMs)
	}
	select {
	case m.started <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return release.Result{}, ctx.Err()
	case <-m.release:
	}
	root := filepath.Join(m.root, "repositories", record.Project, record.TargetOS, record.TargetArch, "releases", record.BuildName)
	if err := os.MkdirAll(filepath.Join(root, "Packages"), 0750); err != nil {
		return release.Result{}, err
	}
	if err := os.MkdirAll(filepath.Join(root, "repodata"), 0750); err != nil {
		return release.Result{}, err
	}
	if err := os.WriteFile(filepath.Join(root, "repodata", "repomd.xml"), []byte("release-metadata"), 0640); err != nil {
		return release.Result{}, err
	}
	return release.Result{Digest: "release-digest"}, nil
}

func newReleaseTestServer(t *testing.T) (*Server, release.CreateReleaseRequest, *testReleaseMaterializer) {
	t.Helper()
	c := DefaultConfig()
	c.DataDir = t.TempDir()
	c.Repository.Workers, c.Release.Workers = 1, 1
	store, err := upload.NewStore(c.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	materializer := &testRepositoryMaterializer{started: make(chan struct{}, 1), release: make(chan struct{})}
	server, err := newServer(c, testAuthorizer{}, store, materializer, "")
	if err != nil {
		t.Fatal(err)
	}
	server.releases.Stop()
	releaseMaterializer := &testReleaseMaterializer{root: c.DataDir, started: make(chan struct{}, 1), release: make(chan struct{}), rpmCount: make(chan int, 2)}
	releases, err := release.NewManager(c.DataDir, c.Release, server.repositories, releaseMaterializer)
	if err != nil {
		t.Fatal(err)
	}
	server.releases = releases
	t.Cleanup(func() { server.repositories.Stop(); server.releases.Stop() })
	now := time.Now().UTC()
	source := &repository.RepositoryRecord{RepositoryUID: "source-repository", RepositoryName: "build-1", Project: "project-1", BuildName: "build-1", TargetOS: "openEuler", TargetArch: "x86_64", State: repository.RepositoryReady, CreatedAt: now, UpdatedAt: now}
	writeTestRepositoryRecord(t, server, source)
	writeTestRepositoryIndex(t, server.repositories, source)
	request := release.CreateReleaseRequest{BuildName: source.BuildName, Project: source.Project, TargetOS: source.TargetOS, TargetArch: source.TargetArch, SourceRepositoryUID: source.RepositoryUID, ExcludeSpecs: []string{"skip", "skip"}}
	return server, request, releaseMaterializer
}

func writeTestRepositoryIndex(t *testing.T, manager *repository.Manager, record *repository.RepositoryRecord) {
	t.Helper()
	path := manager.RepositoryPath(record)
	if err := os.MkdirAll(path, 0750); err != nil {
		t.Fatal(err)
	}
	index := repository.Index{RepositoryUID: record.RepositoryUID, RPMs: map[string]repository.RepositoryRPMMeta{"test.rpm": {FileName: "test.rpm"}}}
	if err := fileutil.AtomicJSON(filepath.Join(path, "repository.json"), &index); err != nil {
		t.Fatal(err)
	}
}

func writeTestRepositoryRecord(t *testing.T, server *Server, record *repository.RepositoryRecord) {
	t.Helper()
	light := *record
	light.RPMs = nil
	path := filepath.Join(server.cfg.DataDir, ".metadata", "repositories", record.RepositoryUID+".json")
	if err := fileutil.AtomicJSON(path, &light); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseLifecycleAndContent(t *testing.T) {
	server, request, materializer := newReleaseTestServer(t)
	response := repositoryRequest(t, server, http.MethodPost, "/internal/v1/releases", request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("submit status = %d: %s", response.Code, response.Body.String())
	}
	select {
	case <-materializer.started:
	case <-time.After(time.Second):
		t.Fatal("release creation did not start")
	}
	if count := <-materializer.rpmCount; count != 1 {
		t.Fatalf("source RPM count = %d, want 1", count)
	}
	if response := repositoryRequest(t, server, http.MethodPost, "/internal/v1/releases", request); response.Code != http.StatusAccepted {
		t.Fatalf("replay status = %d", response.Code)
	}
	if response := repositoryRequest(t, server, http.MethodGet, "/repositories/project-1/", nil); response.Code != http.StatusNotFound {
		t.Fatalf("unpublished directory = %d", response.Code)
	}
	close(materializer.release)
	var state release.ReleaseResponse
	deadline := time.Now().Add(time.Second)
	for {
		response = repositoryRequest(t, server, http.MethodGet, "/internal/v1/releases/"+request.BuildName, nil)
		if err := json.Unmarshal(response.Body.Bytes(), &state); err == nil && state.State == release.ReleaseReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("release did not become ready: %s", response.Body.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if state.ContentURL != "/repositories/project-1/openEuler/x86_64/" {
		t.Fatalf("content URL = %q", state.ContentURL)
	}
	for path, want := range map[string]string{
		"/repositories/project-1/":           "openEuler/",
		"/repositories/project-1/openEuler/": "x86_64/",
		state.ContentURL:                     "Packages/",
		state.ContentURL + "repodata/":       "repomd.xml",
	} {
		response := repositoryRequest(t, server, http.MethodGet, path, nil)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), want) {
			t.Fatalf("browse %s = %d %q, want %q", path, response.Code, response.Body.String(), want)
		}
	}
	for _, path := range []string{
		"/repositories/project-1/openEuler/x86_64/history/",
		"/repositories/project-1/openEuler/x86_64/releases/",
		"/repositories/project-1/openEuler/no-release/",
	} {
		if response := repositoryRequest(t, server, http.MethodGet, path, nil); response.Code != http.StatusNotFound {
			t.Fatalf("hidden directory %s = %d", path, response.Code)
		}
	}
	for _, path := range []string{state.ContentURL + "repodata/repomd.xml", "/repositories/releases/v1/build-1/repodata/repomd.xml"} {
		response = repositoryRequest(t, server, http.MethodGet, path, nil)
		if response.Code != http.StatusOK || response.Body.String() != "release-metadata" || response.Header().Get("ETag") == "" {
			t.Fatalf("content %s = %d %q", path, response.Code, response.Body.String())
		}
	}
	if response := repositoryRequest(t, server, http.MethodDelete, "/internal/v1/releases/build-1", nil); response.Code != http.StatusConflict {
		t.Fatalf("delete current = %d", response.Code)
	}
	conflict := request
	conflict.ExcludeSpecs = []string{"other"}
	if response := repositoryRequest(t, server, http.MethodPost, "/internal/v1/releases", conflict); response.Code != http.StatusConflict {
		t.Fatalf("identity conflict = %d", response.Code)
	}
}

func TestReleaseSeparatesOSWithSameArchitecture(t *testing.T) {
	server, first, materializer := newReleaseTestServer(t)
	close(materializer.release)
	second := first
	second.BuildName = "build-2"
	second.TargetOS = "openEuler-mainline"
	second.SourceRepositoryUID = "source-repository-2"
	now := time.Now().UTC()
	secondSource := &repository.RepositoryRecord{
		RepositoryUID: second.SourceRepositoryUID, RepositoryName: second.BuildName,
		Project: second.Project, BuildName: second.BuildName, TargetOS: second.TargetOS,
		TargetArch: second.TargetArch, State: repository.RepositoryReady, CreatedAt: now, UpdatedAt: now,
	}
	writeTestRepositoryRecord(t, server, secondSource)
	writeTestRepositoryIndex(t, server.repositories, secondSource)

	for _, request := range []release.CreateReleaseRequest{first, second} {
		response := repositoryRequest(t, server, http.MethodPost, "/internal/v1/releases", request)
		if response.Code != http.StatusAccepted {
			t.Fatalf("submit %s = %d: %s", request.BuildName, response.Code, response.Body.String())
		}
		deadline := time.Now().Add(time.Second)
		for {
			response = repositoryRequest(t, server, http.MethodGet, "/internal/v1/releases/"+request.BuildName, nil)
			var state release.ReleaseResponse
			if err := json.Unmarshal(response.Body.Bytes(), &state); err == nil && state.State == release.ReleaseReady {
				want := "/repositories/" + request.Project + "/" + request.TargetOS + "/" + request.TargetArch + "/"
				if state.ContentURL != want {
					t.Fatalf("content URL = %q, want %q", state.ContentURL, want)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("release %s did not become ready: %s", request.BuildName, response.Body.String())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	for _, request := range []release.CreateReleaseRequest{first, second} {
		path := "/repositories/" + request.Project + "/" + request.TargetOS + "/" + request.TargetArch + "/repodata/repomd.xml"
		response := repositoryRequest(t, server, http.MethodGet, path, nil)
		if response.Code != http.StatusOK || response.Body.String() != "release-metadata" {
			t.Fatalf("content %s = %d %q", path, response.Code, response.Body.String())
		}
	}
	wrongOS := "/repositories/" + first.Project + "/another-os/" + first.TargetArch + "/repodata/repomd.xml"
	if response := repositoryRequest(t, server, http.MethodGet, wrongOS, nil); response.Code != http.StatusNotFound {
		t.Fatalf("wrong OS = %d", response.Code)
	}
}

func TestStableReleasePathWithReservedLookingNames(t *testing.T) {
	server, request, materializer := newReleaseTestServer(t)
	request.Project, request.TargetOS = "releases", "v1"
	source, ok, err := server.repositories.Get(request.SourceRepositoryUID)
	if err != nil || !ok {
		t.Fatalf("load source: %v", err)
	}
	source.Project = request.Project
	source.TargetOS = request.TargetOS
	writeTestRepositoryRecord(t, server, source)
	writeTestRepositoryIndex(t, server.repositories, source)
	close(materializer.release)
	if response := repositoryRequest(t, server, http.MethodPost, "/internal/v1/releases", request); response.Code != http.StatusAccepted {
		t.Fatalf("submit = %d", response.Code)
	}
	deadline := time.Now().Add(time.Second)
	for {
		response := repositoryRequest(t, server, http.MethodGet, "/repositories/releases/v1/x86_64/repodata/repomd.xml", nil)
		if response.Code == http.StatusOK && response.Body.String() == "release-metadata" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stable content = %d %q", response.Code, response.Body.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
