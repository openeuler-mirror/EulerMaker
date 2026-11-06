package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"artifact-manager/pkg/fileutil"
	"artifact-manager/pkg/release"
	"artifact-manager/pkg/repository"
)

type transientRepositoryMaterializer struct{}

func (transientRepositoryMaterializer) Materialize(context.Context, repository.RepositoryRecord) (repository.Result, error) {
	return repository.Result{}, fmt.Errorf("temporary materialization failure")
}

func TestBuildDeletionFencesSubmissions(t *testing.T) {
	for _, resource := range []string{"repositories", "releases"} {
		for _, scenario := range []string{"unsubmitted", "retryable-failed", "running"} {
			t.Run(resource+"/"+scenario, func(t *testing.T) {
				blocked := &testRepositoryMaterializer{started: make(chan struct{}, 1), release: make(chan struct{})}
				var materializer repository.Materializer = blocked
				if scenario == "retryable-failed" {
					materializer = transientRepositoryMaterializer{}
				}
				s, request := newRepositoryTestServer(t, materializer)
				endpoint := "/internal/v1/projects/project-1/builds/build-1/repositories"
				cleanupEndpoint := buildManagementPath(request.Project, request.BuildName) + "/" + resource
				if scenario != "unsubmitted" {
					if _, _, err := s.repositories.Submit(request); err != nil {
						t.Fatal(err)
					}
					if scenario == "running" {
						select {
						case <-blocked.started:
						case <-time.After(time.Second):
							t.Fatal("materialization did not start")
						}
					} else {
						waitRepositoryState(t, s, request.RepositoryUID, repository.RepositoryFailed)
						record, _, _ := s.repositories.Get(request.RepositoryUID)
						if record.Failure == nil || !record.Failure.Retryable {
							t.Fatalf("expected retryable failure: %+v", record)
						}
					}
				}
				response := repositoryRequest(t, s, http.MethodDelete, cleanupEndpoint, nil)
				if scenario == "running" {
					if response.Code != http.StatusAccepted || response.Header().Get("Retry-After") != "5" {
						t.Fatalf("active stop: %d %s", response.Code, response.Body.String())
					}
					close(blocked.release)
					waitRepositoryState(t, s, request.RepositoryUID, repository.RepositoryReady)
					response = repositoryRequest(t, s, http.MethodDelete, cleanupEndpoint, nil)
				}
				if response.Code != http.StatusNoContent {
					t.Fatalf("drained stop: %d %s", response.Code, response.Body.String())
				}
				response = repositoryRequest(t, s, http.MethodPost, buildManagementPath(request.Project, request.BuildName)+"/repositories", request)
				if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "BuildStopped") {
					t.Fatalf("late submission: %d %s", response.Code, response.Body.String())
				}
				// The fence also covers architectures that have never created a repository.
				otherArch := request
				otherArch.TargetArch = "aarch64"
				response = repositoryRequest(t, s, http.MethodPost, buildManagementPath(request.Project, request.BuildName)+"/repositories", otherArch)
				if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "BuildStopped") {
					t.Fatalf("late submission on another architecture: %d %s", response.Code, response.Body.String())
				}
				s.Close()
				restarted, err := newServer(s.cfg, testAuthorizer{}, s.store, materializer, "")
				if err != nil {
					t.Fatal(err)
				}
				defer restarted.Close()
				for range 2 {
					response = repositoryRequest(t, restarted, http.MethodDelete, cleanupEndpoint, nil)
					if response.Code != http.StatusNoContent {
						t.Fatalf("recovered stop: %d %s", response.Code, response.Body.String())
					}
					response = repositoryRequest(t, restarted, http.MethodDelete, endpoint, nil)
					if response.Code != http.StatusNoContent {
						t.Fatalf("cleanup: %d %s", response.Code, response.Body.String())
					}
					response = repositoryRequest(t, restarted, http.MethodPost, buildManagementPath(request.Project, request.BuildName)+"/repositories", otherArch)
					if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "BuildStopped") {
						t.Fatalf("submission after cleanup: %d %s", response.Code, response.Body.String())
					}
				}
			})
		}
	}
}

func TestReleaseDeletionDrainsBeforeCleanup(t *testing.T) {
	server, request, materializer := newReleaseTestServer(t)
	endpoint := buildManagementPath(request.Project, request.BuildName) + "/releases"
	response := repositoryRequest(t, server, http.MethodPost, endpoint, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("submit release: %d %s", response.Code, response.Body.String())
	}
	select {
	case <-materializer.started:
	case <-time.After(time.Second):
		t.Fatal("release did not start")
	}
	response = repositoryRequest(t, server, http.MethodDelete, endpoint, nil)
	if response.Code != http.StatusAccepted || response.Header().Get("Retry-After") != "5" {
		t.Fatalf("active release cleanup: %d %s", response.Code, response.Body.String())
	}
	close(materializer.release)
	deadline := time.Now().Add(time.Second)
	for {
		response = repositoryRequest(t, server, http.MethodDelete, endpoint, nil)
		if response.Code == http.StatusNoContent {
			break
		}
		if response.Code != http.StatusAccepted || time.Now().After(deadline) {
			t.Fatalf("release cleanup: %d %s", response.Code, response.Body.String())
		}
		time.Sleep(time.Millisecond)
	}
	if _, ok, err := server.repositories.Get(request.SourceRepositoryUID); err != nil || !ok {
		t.Fatalf("release cleanup removed source repository: %t %v", ok, err)
	}
	if _, ok, err := server.releases.Get(request.BuildName); err != nil || ok {
		t.Fatalf("release still exists: %t %v", ok, err)
	}
	current := filepath.Join(server.cfg.DataDir, "repositories", request.Project, request.TargetArch, "current")
	if _, err := os.Lstat(current); !os.IsNotExist(err) {
		t.Fatalf("release activated after cleanup fence: %v", err)
	}
	response = repositoryRequest(t, server, http.MethodPost, endpoint, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "BuildStopped") {
		t.Fatalf("late release submission: %d %s", response.Code, response.Body.String())
	}
}

func waitRepositoryState(t *testing.T, s *Server, uid string, expected repository.RepositoryState) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		record, ok, err := s.repositories.Get(uid)
		if err != nil {
			t.Fatal(err)
		}
		if ok && record.State == expected {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("repository did not reach %s", expected)
}

func TestBuildDeletionFenceStorageFailure(t *testing.T) {
	s, _ := newRepositoryTestServer(t, transientRepositoryMaterializer{})
	marker := filepath.Join(s.cfg.DataDir, "repositories/project-1/.stopped/build-1")
	if err := fileutil.AtomicJSON(marker, "not a stop directory"); err != nil {
		t.Fatal(err)
	}
	endpoint := "/internal/v1/projects/project-1/builds/build-1/releases"
	response := repositoryRequest(t, s, http.MethodDelete, endpoint, nil)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("invalid fence: %d %s", response.Code, response.Body.String())
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	response = repositoryRequest(t, s, http.MethodDelete, endpoint, nil)
	if response.Code != http.StatusNoContent {
		t.Fatalf("repaired stop: %d %s", response.Code, response.Body.String())
	}
	if err := repository.CheckBuildActive(s.cfg.DataDir, "project-1", "other-build"); err != nil {
		t.Fatalf("stop affected another build: %v", err)
	}
}

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
	endpoint := "/internal/v1/projects/project-1/builds/build-1/repositories"
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
	releaseEndpoint := buildManagementPath(request.Project, request.BuildName) + "/releases"
	response = repositoryRequest(t, restarted, http.MethodDelete, releaseEndpoint, nil)
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
		response = repositoryRequest(t, restarted, http.MethodDelete, releaseEndpoint, nil)
		if response.Code != http.StatusNoContent {
			t.Fatalf("release cleanup: %d %s", response.Code, response.Body.String())
		}
		if _, err := os.Stat(filepath.Join(target, "history/build-1")); err != nil {
			t.Fatalf("release cleanup removed process repo: %v", err)
		}
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

func TestProcessRepositoryCleanupPreservesReleaseFiles(t *testing.T) {
	server, request, _ := newReleaseTestServer(t)
	base := buildManagementPath(request.Project, request.BuildName)
	releaseFile := filepath.Join(server.cfg.DataDir, "repositories", request.Project, request.TargetArch,
		"releases", ".work", request.BuildName+"-123", "rpm")
	if err := fileutil.AtomicJSON(releaseFile, "release work"); err != nil {
		t.Fatal(err)
	}
	if response := repositoryRequest(t, server, http.MethodDelete, base+"/repositories", nil); response.Code != http.StatusNoContent {
		t.Fatalf("process cleanup: %d %s", response.Code, response.Body.String())
	}
	if _, ok, err := server.repositories.Get(request.SourceRepositoryUID); err != nil || ok {
		t.Fatalf("process repository retained: %t %v", ok, err)
	}
	if _, err := os.Stat(releaseFile); err != nil {
		t.Fatalf("process cleanup changed release files: %v", err)
	}
}
