package artifact

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type testReleaseMaterializer struct {
	root    string
	started chan struct{}
	release chan struct{}
}

func (m *testReleaseMaterializer) Create(ctx context.Context, record ReleaseRecord, _ RepositoryRecord) (releaseResult, error) {
	select {
	case m.started <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return releaseResult{}, ctx.Err()
	case <-m.release:
	}
	root := filepath.Join(m.root, "repositories", record.Project, record.TargetArch, "releases", record.BuildName)
	if err := os.MkdirAll(filepath.Join(root, "Packages"), 0750); err != nil {
		return releaseResult{}, err
	}
	if err := os.MkdirAll(filepath.Join(root, "repodata"), 0750); err != nil {
		return releaseResult{}, err
	}
	if err := os.WriteFile(filepath.Join(root, "repodata", "repomd.xml"), []byte("release-metadata"), 0640); err != nil {
		return releaseResult{}, err
	}
	return releaseResult{Digest: "release-digest", Count: 1}, nil
}

func newReleaseTestServer(t *testing.T) (*Server, CreateReleaseRequest, *testReleaseMaterializer) {
	t.Helper()
	c := DefaultConfig()
	c.DataDir = t.TempDir()
	c.RepositoryWorkers, c.ReleaseWorkers = 1, 1
	store, err := NewStore(c.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	repositoryMaterializer := &testRepositoryMaterializer{started: make(chan struct{}, 1), release: make(chan struct{})}
	server, err := newArtifactServer(c, testAuthorizer{}, store, repositoryMaterializer)
	if err != nil {
		t.Fatal(err)
	}
	server.releases.stop()
	releaseMaterializer := &testReleaseMaterializer{root: c.DataDir, started: make(chan struct{}, 1), release: make(chan struct{})}
	releases, err := newReleaseManager(c, server.repositories, releaseMaterializer)
	if err != nil {
		t.Fatal(err)
	}
	server.releases = releases
	t.Cleanup(func() { server.repositories.stop(); server.releases.stop() })
	now := time.Now().UTC()
	source := &RepositoryRecord{RepositoryUID: "source-repository", RepositoryName: "build-1", Project: "project-1", BuildName: "build-1", TargetOS: "openEuler", TargetArch: "x86_64", State: RepositoryReady, CreatedAt: now, UpdatedAt: now}
	server.repositories.mu.Lock()
	server.repositories.records[source.RepositoryUID] = source
	server.repositories.mu.Unlock()
	request := CreateReleaseRequest{BuildName: source.BuildName, Project: source.Project, TargetOS: source.TargetOS, TargetArch: source.TargetArch, SourceRepositoryUID: source.RepositoryUID, ExcludeSpecs: []string{"skip", "skip"}}
	return server, request, releaseMaterializer
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
	if response := repositoryRequest(t, server, http.MethodPost, "/internal/v1/releases", request); response.Code != http.StatusAccepted {
		t.Fatalf("replay status = %d", response.Code)
	}
	close(materializer.release)
	var state ReleaseResponse
	deadline := time.Now().Add(time.Second)
	for {
		response = repositoryRequest(t, server, http.MethodGet, "/internal/v1/releases/"+request.BuildName, nil)
		if err := json.Unmarshal(response.Body.Bytes(), &state); err == nil && state.State == ReleaseReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("release did not become ready: %s", response.Body.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if state.ContentURL != "/repositories/project-1/x86_64/" {
		t.Fatalf("content URL = %q", state.ContentURL)
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

func TestReleaseRequestNormalization(t *testing.T) {
	request := CreateReleaseRequest{BuildName: "build", Project: "project", TargetOS: "os", TargetArch: "arch", SourceRepositoryUID: "repository", ExcludeSpecs: []string{"z", "a", "a"}}
	normalized, first, err := normalizeReleaseRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized.ExcludeSpecs) != 2 || normalized.ExcludeSpecs[0] != "a" || normalized.ExcludeSpecs[1] != "z" {
		t.Fatalf("exclude specs = %#v", normalized.ExcludeSpecs)
	}
	_, second, err := normalizeReleaseRequest(CreateReleaseRequest{BuildName: "build", Project: "project", TargetOS: "os", TargetArch: "arch", SourceRepositoryUID: "repository", ExcludeSpecs: []string{"a", "z"}})
	if err != nil || first != second {
		t.Fatalf("digest mismatch: %q %q, %v", first, second, err)
	}
}

func TestFilesystemReleaseMaterializerExcludesSpecs(t *testing.T) {
	root := t.TempDir()
	command := filepath.Join(root, "createrepo-test")
	script := "#!/bin/sh\nfor last; do :; done\nmkdir -p \"$last/repodata\"\nprintf primary > \"$last/repodata/primary.xml\"\nsum=$(sha256sum \"$last/repodata/primary.xml\" | cut -d' ' -f1)\nprintf '<repomd><data type=\"primary\"><checksum type=\"sha256\">%s</checksum><location href=\"repodata/primary.xml\"/></data></repomd>' \"$sum\" > \"$last/repodata/repomd.xml\"\n"
	if err := os.WriteFile(command, []byte(script), 0750); err != nil {
		t.Fatal(err)
	}
	publicKey := filepath.Join(root, "public-key")
	if err := os.WriteFile(publicKey, []byte("key"), 0640); err != nil {
		t.Fatal(err)
	}
	c := DefaultConfig()
	c.DataDir, c.CreateRepoCommand, c.ReleasePublicKey = root, command, publicKey
	if err := os.Mkdir(filepath.Join(root, ".release-work"), 0750); err != nil {
		t.Fatal(err)
	}
	materializer := newFilesystemReleaseMaterializer(c)
	source := RepositoryRecord{RepositoryUID: "source", Project: "project", BuildName: "build", TargetArch: "x86_64", RPMs: map[string]RepositoryRPMMeta{
		"keep+1.rpm": {FileName: "keep+1.rpm", SpecName: "keep"},
		"skip.rpm":   {FileName: "skip.rpm", SpecName: "skip"},
	}}
	sourcePackages := filepath.Join(repositoryVersionPath(root, source.Project, source.TargetArch, source.BuildName, source.RepositoryUID), "Packages")
	if err := os.MkdirAll(sourcePackages, 0750); err != nil {
		t.Fatal(err)
	}
	for name := range source.RPMs {
		if err := os.WriteFile(filepath.Join(sourcePackages, name), []byte(name), 0640); err != nil {
			t.Fatal(err)
		}
	}
	record := ReleaseRecord{BuildName: "build", Project: "project", TargetArch: "x86_64", SourceRepositoryUID: "source", RequestDigest: "request", ExcludeSpecs: []string{"skip"}}
	result, err := materializer.Create(context.Background(), record, source)
	if err != nil {
		t.Fatal(err)
	}
	if result.Count != 1 || result.Digest == "" {
		t.Fatalf("result = %#v", result)
	}
	releasePath := filepath.Join(root, "repositories", "project", "x86_64", "releases", "build")
	if _, err := os.Stat(filepath.Join(releasePath, "Packages", "keep+1.rpm")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(releasePath, "Packages", "skip.rpm")); !os.IsNotExist(err) {
		t.Fatalf("excluded RPM exists: %v", err)
	}
	index, err := readReleaseIndex(releasePath)
	if err != nil || index.PublicKeySHA256 == "" || index.ReleaseDigest != result.Digest {
		t.Fatalf("release index = %#v, %v", index, err)
	}
}
