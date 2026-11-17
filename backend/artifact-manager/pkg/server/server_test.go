package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTemporaryCleanupDoesNotBlockReadiness(t *testing.T) {
	s, _ := newRepositoryTestServer(t, badManifestMaterializer{})
	started := make(chan struct{})
	s.startTemporaryCleanup(func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	t.Cleanup(s.Close)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("background cleanup did not start")
	}
	response := httptest.NewRecorder()
	s.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("readiness during cleanup: %d", response.Code)
	}
	s.Close()
	select {
	case <-s.cleanupDone:
	default:
		t.Fatal("Close returned before cleanup exited")
	}
}

func TestTemporaryCleanupFailureIsLogged(t *testing.T) {
	logs := captureFailureLogs(t)
	s, _ := newRepositoryTestServer(t, badManifestMaterializer{})
	s.startTemporaryCleanup(func(context.Context) error {
		return errors.New("sensitive-storage-details")
	})
	t.Cleanup(s.Close)
	<-s.cleanupDone
	if got := logs.String(); !strings.Contains(got, `operation="cleanup-temporary"`) ||
		strings.Contains(got, "sensitive-storage-details") {
		t.Fatalf("cleanup failure log: %s", got)
	}
	response := httptest.NewRecorder()
	s.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("readiness after cleanup failure: %d", response.Code)
	}
}

func TestNewHandlerCleansExpiredTemporaryUploads(t *testing.T) {
	c := DefaultConfig()
	c.DataDir = t.TempDir()
	tmp := filepath.Join(c.DataDir, "artifacts/project/jobs/job/.uploads/orphan.tmp")
	if err := os.MkdirAll(filepath.Dir(tmp), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("data"), 0640); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * c.Upload.TemporaryUploadTTL)
	if err := os.Chtimes(tmp, old, old); err != nil {
		t.Fatal(err)
	}
	s, err := NewHandler(c, testAuthorizer{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	select {
	case <-s.cleanupDone:
	case <-time.After(5 * time.Second):
		t.Fatal("background cleanup did not finish")
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("expired temporary upload remains: %v", err)
	}
}
