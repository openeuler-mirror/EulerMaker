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

func TestStopBuildRepositoriesAPI(t *testing.T) {
	for _, scenario := range []string{"unsubmitted", "retryable-failed", "running"} {
		t.Run(scenario, func(t *testing.T) {
			blocked := &testRepositoryMaterializer{started: make(chan struct{}, 1), release: make(chan struct{})}
			var materializer repository.Materializer = blocked
			if scenario == "retryable-failed" {
				materializer = transientRepositoryMaterializer{}
			}
			s, request := newRepositoryTestServer(t, materializer)
			endpoint := "/internal/v1/projects/project-1/architectures/x86_64/builds/build-1/repositories"
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
			response := repositoryRequest(t, s, http.MethodPost, endpoint+"/stop", nil)
			if scenario == "running" {
				if response.Code != http.StatusAccepted || response.Header().Get("Retry-After") != "5" {
					t.Fatalf("active stop: %d %s", response.Code, response.Body.String())
				}
				close(blocked.release)
				waitRepositoryState(t, s, request.RepositoryUID, repository.RepositoryReady)
				response = repositoryRequest(t, s, http.MethodPost, endpoint+"/stop", nil)
			}
			if response.Code != http.StatusNoContent {
				t.Fatalf("drained stop: %d %s", response.Code, response.Body.String())
			}
			response = repositoryRequest(t, s, http.MethodPost, "/internal/v1/repositories", request)
			if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "BuildStopped") {
				t.Fatalf("late submission: %d %s", response.Code, response.Body.String())
			}
			s.Close()
			restarted, err := newServer(s.cfg, testAuthorizer{}, s.store, materializer, "")
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			for range 2 {
				response = repositoryRequest(t, restarted, http.MethodPost, endpoint+"/stop", nil)
				if response.Code != http.StatusNoContent {
					t.Fatalf("recovered stop: %d %s", response.Code, response.Body.String())
				}
				response = repositoryRequest(t, restarted, http.MethodDelete, endpoint, nil)
				if response.Code != http.StatusNoContent {
					t.Fatalf("cleanup: %d %s", response.Code, response.Body.String())
				}
				response = repositoryRequest(t, restarted, http.MethodPost, "/internal/v1/repositories", request)
				if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "BuildStopped") {
					t.Fatalf("submission after cleanup: %d %s", response.Code, response.Body.String())
				}
			}
		})
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

func TestStopBuildStorageFailure(t *testing.T) {
	s, _ := newRepositoryTestServer(t, transientRepositoryMaterializer{})
	marker := filepath.Join(s.cfg.DataDir, "repositories/project-1/x86_64/.stopped/build-1")
	if err := fileutil.AtomicJSON(marker, "not a stop directory"); err != nil {
		t.Fatal(err)
	}
	endpoint := "/internal/v1/projects/project-1/architectures/x86_64/builds/build-1/repositories/stop"
	response := repositoryRequest(t, s, http.MethodPost, endpoint, nil)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("invalid fence: %d %s", response.Code, response.Body.String())
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	response = repositoryRequest(t, s, http.MethodPost, endpoint, nil)
	if response.Code != http.StatusNoContent {
		t.Fatalf("repaired stop: %d %s", response.Code, response.Body.String())
	}
	if err := repository.CheckBuildActive(s.cfg.DataDir, "project-1", "x86_64", "other-build"); err != nil {
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
