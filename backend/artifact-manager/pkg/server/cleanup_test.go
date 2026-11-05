package server

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"artifact-manager/pkg/fileutil"
	"artifact-manager/pkg/release"
	"artifact-manager/pkg/repository"
)

func TestBuildRepositoryCleanupAPI(t *testing.T) {
	materializer := &testRepositoryMaterializer{started: make(chan struct{}, 1), release: make(chan struct{})}
	s, request := newRepositoryTestServer(t, materializer)
	if _, _, err := s.repositories.Submit(request); err != nil {
		t.Fatal(err)
	}
	select {
	case <-materializer.started:
	case <-time.After(time.Second):
		t.Fatal("materialization did not start")
	}
	endpoint := "/internal/v1/projects/project-1/architectures/x86_64/builds/build-1/repositories"
	response := repositoryRequest(t, s, http.MethodDelete, endpoint, nil)
	if response.Code != http.StatusAccepted || response.Header().Get("Retry-After") != "5" {
		t.Fatalf("active work: %d %s", response.Code, response.Body.String())
	}
	close(materializer.release)
	deadline := time.Now().Add(time.Second)
	for {
		record, _, err := s.repositories.Get(request.RepositoryUID)
		if err != nil {
			t.Fatal(err)
		}
		if record.State == repository.RepositoryReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("materialization did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	s.Close()
	target := filepath.Join(s.cfg.DataDir, "repositories/project-1/x86_64")
	record := &release.ReleaseRecord{
		BuildName: "build-1", Project: "project-1", TargetArch: "x86_64", State: release.ReleasePrepared,
	}
	if err := fileutil.AtomicJSON(filepath.Join(target, "releases/.records/build-1.json"), record); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"releases/build-1/Packages/rpm", "releases/.work/build-1-123/rpm", "releases/.trash/build-1-d_123/rpm"} {
		if err := fileutil.AtomicJSON(filepath.Join(target, path), "rpm"); err != nil {
			t.Fatal(err)
		}
	}
	jobPath := filepath.Join(s.cfg.DataDir, "artifacts/project-1/jobs/job-1/rpm")
	if err := fileutil.AtomicJSON(jobPath, "job artifact"); err != nil {
		t.Fatal(err)
	}
	restarted, err := newServer(s.cfg, testAuthorizer{}, s.store, materializer, "")
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	current := filepath.Join(target, "current")
	if err := os.Symlink("releases/build-1", current); err != nil {
		t.Fatal(err)
	}
	response = repositoryRequest(t, restarted, http.MethodDelete, endpoint, nil)
	if response.Code != http.StatusConflict {
		t.Fatalf("current release: %d %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(target, "history/build-1")); err != nil {
		t.Fatalf("current conflict removed process repo: %v", err)
	}
	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		response = repositoryRequest(t, restarted, http.MethodDelete, endpoint, nil)
		if response.Code != http.StatusNoContent {
			t.Fatalf("cleanup: %d %s", response.Code, response.Body.String())
		}
	}
	for _, path := range []string{"history/build-1", "releases/build-1", "releases/.records/build-1.json", "releases/.work/build-1-123", "releases/.trash/build-1-d_123"} {
		if _, err := os.Stat(filepath.Join(target, path)); !os.IsNotExist(err) {
			t.Fatalf("remaining path %s: %v", path, err)
		}
	}
	if _, ok, err := restarted.repositories.Get(request.RepositoryUID); err != nil || ok {
		t.Fatalf("process repo index retained: %t, %v", ok, err)
	}
	if _, ok, err := restarted.releases.Get(request.BuildName); err != nil || ok {
		t.Fatalf("release index retained: %t, %v", ok, err)
	}
	if _, err := os.Stat(jobPath); err != nil {
		t.Fatalf("job artifact changed: %v", err)
	}
}
