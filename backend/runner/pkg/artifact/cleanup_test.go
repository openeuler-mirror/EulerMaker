package artifact

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"ebs-runner/pkg/gateway"
)

func TestArtifactCleanupSuccessIsImmediateAndFailureIsRetained(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	manager := &CleanupManager{RootDir: root, FailedRetention: 24 * time.Hour, Now: func() time.Time { return now }}
	job := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "job", Namespace: "project", UID: "uid"}}
	createLocalArtifactState(t, root, job)
	if err := manager.MarkSuccess(job); err != nil {
		t.Fatalf("mark success: %v", err)
	}
	assertLocalArtifactState(t, root, job, false)

	createLocalArtifactState(t, root, job)
	if err := manager.MarkFailure(job); err != nil {
		t.Fatalf("mark failure: %v", err)
	}
	assertLocalArtifactState(t, root, job, true)
	if err := manager.Sweep(); err != nil {
		t.Fatalf("early sweep: %v", err)
	}
	assertLocalArtifactState(t, root, job, true)
	now = now.Add(24 * time.Hour)
	if err := manager.Sweep(); err != nil {
		t.Fatalf("due sweep: %v", err)
	}
	assertLocalArtifactState(t, root, job, false)
}

func TestArtifactCleanupStoppedJobNeedsNoMarkerWrite(t *testing.T) {
	root := t.TempDir()
	job := gateway.JobResource{Metadata: gateway.ObjectMeta{Namespace: "project", Name: "job", UID: "uid"}}
	createLocalArtifactState(t, root, job)
	manager := &CleanupManager{
		RootDir:     root,
		writeMarker: func(string, []byte) error { t.Fatal("stopped cleanup must not write a marker"); return nil },
	}
	for i := 0; i < 2; i++ {
		if err := manager.Remove("project", "job"); err != nil {
			t.Fatal(err)
		}
		assertLocalArtifactState(t, root, job, false)
	}
}

func TestTerminalCleanupRecoveryKeepsOriginalDeadline(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	end := now.Add(-time.Hour)
	m := &CleanupManager{RootDir: root, FailedRetention: 24 * time.Hour, Now: func() time.Time { return now }}
	job := gateway.JobResource{
		Metadata: gateway.ObjectMeta{Namespace: "project", Name: "job"},
		Status:   gateway.JobStatus{Phase: "Failed", EndTime: &end},
	}
	createLocalArtifactState(t, root, job)
	if err := m.RecoverTerminal(job); err != nil {
		t.Fatal(err)
	}
	first, err := m.readMarker(m.markerPath("project", "job"))
	if err != nil || !first.CreatedAt.Equal(end) || !first.NotBefore.Equal(end.Add(24*time.Hour)) {
		t.Fatalf("recovered marker=%+v, err=%v", first, err)
	}
	now = now.Add(time.Hour)
	// A fresh manager simulates restart; even a new retention setting must not rewrite an existing marker.
	m = &CleanupManager{RootDir: root, FailedRetention: 48 * time.Hour, Now: func() time.Time { return now }}
	if err := m.RecoverTerminal(job); err != nil {
		t.Fatal(err)
	}
	second, err := m.readMarker(m.markerPath("project", "job"))
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("existing cleanup deadline changed: %+v, %v", second, err)
	}
	assertLocalArtifactState(t, root, job, true)
	now = first.NotBefore
	if err := m.RecoverTerminal(job); err != nil {
		t.Fatal(err)
	}
	assertLocalArtifactState(t, root, job, false)

	createLocalArtifactState(t, root, job)
	job.Status.EndTime = nil
	if err := m.RecoverTerminal(job); err == nil {
		t.Fatal("missing endTime must be reported without inventing a cleanup deadline")
	}
	assertLocalArtifactState(t, root, job, true)
	job.Status.Phase = "Succeeded"
	if err := m.RecoverTerminal(job); err != nil {
		t.Fatal(err)
	}
	assertLocalArtifactState(t, root, job, false)
}

func TestCleanupContinuesAfterIndividualErrors(t *testing.T) {
	for _, mode := range []string{"expired", "pressure", "marker-reclaim"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			now := time.Now().UTC()
			m := &CleanupManager{
				RootDir: root, FailedRetention: 24 * time.Hour, Now: func() time.Time { return now },
				DiskUsage: func(string) (float64, error) { return 0.9, nil },
			}
			blocked := gateway.JobResource{Metadata: gateway.ObjectMeta{Namespace: "blocked", Name: "job"}}
			good := gateway.JobResource{Metadata: gateway.ObjectMeta{Namespace: "good", Name: "job"}}
			if err := m.MarkFailure(blocked); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Minute)
			createLocalArtifactState(t, root, good)
			if err := m.MarkFailure(good); err != nil {
				t.Fatal(err)
			}
			// This metadata sorts before the valid records, and the first valid candidate cannot be removed.
			if err := os.WriteFile(filepath.Join(root, "cleanup", "aaa.json"), []byte("broken"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "results", "blocked"), []byte("not a directory"), 0o600); err != nil {
				t.Fatal(err)
			}
			var cleanupErr error
			switch mode {
			case "expired":
				now = now.Add(24 * time.Hour)
				cleanupErr = m.Sweep()
			case "pressure":
				cleanupErr = m.Sweep()
			case "marker-reclaim":
				current := gateway.JobResource{Metadata: gateway.ObjectMeta{Namespace: "current", Name: "job"}}
				createLocalArtifactState(t, root, current)
				m.writeMarker = func(path string, data []byte) error {
					if _, err := os.Stat(filepath.Join(root, "results", "good", "job")); err == nil {
						return syscall.ENOSPC
					}
					return writeAtomicFile(path, data)
				}
				cleanupErr = m.MarkFailure(current)
				if _, err := m.readMarker(m.markerPath("current", "job")); err != nil {
					t.Fatalf("current marker was not persisted after reclaim: %v", err)
				}
				assertLocalArtifactState(t, root, current, true)
			}
			if cleanupErr == nil || !strings.Contains(cleanupErr.Error(), "aaa.json") || !strings.Contains(cleanupErr.Error(), "blocked") {
				t.Fatalf("individual errors were not aggregated: %v", cleanupErr)
			}
			assertLocalArtifactState(t, root, good, false)
			if _, err := os.Stat(m.markerPath("blocked", "job")); err != nil {
				t.Fatalf("failed cleanup lost its retry marker: %v", err)
			}
		})
	}
}

func TestArtifactCleanupDiskPressureRemovesOldestFailedJobsFirst(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	checks := 0
	manager := &CleanupManager{
		RootDir: root, FailedRetention: 24 * time.Hour,
		Now: func() time.Time { return now },
		DiskUsage: func(string) (float64, error) {
			checks++
			if checks == 1 {
				return 0.86, nil
			}
			return 0.85, nil
		},
	}
	oldest := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "oldest", Namespace: "project"}}
	newer := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "newer", Namespace: "project"}}
	active := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "active", Namespace: "project"}}
	for _, job := range []gateway.JobResource{oldest, newer, active} {
		createLocalArtifactState(t, root, job)
	}
	if err := manager.MarkFailure(oldest); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := manager.MarkFailure(newer); err != nil {
		t.Fatal(err)
	}
	if err := manager.Sweep(); err != nil {
		t.Fatal(err)
	}
	assertLocalArtifactState(t, root, oldest, false)
	assertLocalArtifactState(t, root, newer, true)
	assertLocalArtifactState(t, root, active, true)
	if checks != 2 {
		t.Fatalf("disk usage checks = %d, want 2", checks)
	}
}

func TestArtifactCleanupDoesNotShortenRetentionBelowThreshold(t *testing.T) {
	root := t.TempDir()
	manager := &CleanupManager{
		RootDir: root, FailedRetention: 24 * time.Hour,
		DiskUsage: func(string) (float64, error) { return 0.85, nil },
	}
	job := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "job", Namespace: "project"}}
	createLocalArtifactState(t, root, job)
	if err := manager.MarkFailure(job); err != nil {
		t.Fatal(err)
	}
	if err := manager.Sweep(); err != nil {
		t.Fatal(err)
	}
	assertLocalArtifactState(t, root, job, true)
}

func TestArtifactCleanupReclaimsOldestFailedJobWhenMarkerWriteRunsOutOfSpace(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	manager := &CleanupManager{RootDir: root, FailedRetention: 24 * time.Hour, Now: func() time.Time { return now }}
	oldest := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "oldest", Namespace: "project"}}
	newer := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "newer", Namespace: "project"}}
	current := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "current", Namespace: "project"}}
	for _, job := range []gateway.JobResource{oldest, newer, current} {
		createLocalArtifactState(t, root, job)
	}
	if err := manager.MarkFailure(oldest); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := manager.MarkFailure(newer); err != nil {
		t.Fatal(err)
	}
	manager.writeMarker = func(path string, data []byte) error {
		if _, err := os.Stat(filepath.Join(root, "results", "project", "oldest")); err == nil {
			return syscall.ENOSPC
		}
		return writeAtomicFile(path, data)
	}
	now = now.Add(time.Minute)
	if err := manager.MarkFailure(current); err != nil {
		t.Fatalf("mark failure after reclaim: %v", err)
	}
	assertLocalArtifactState(t, root, oldest, false)
	assertLocalArtifactState(t, root, newer, true)
	assertLocalArtifactState(t, root, current, true)
	if _, err := os.Stat(manager.markerPath("project", "current")); err != nil {
		t.Fatalf("current cleanup marker: %v", err)
	}
}

func TestArtifactCleanupMarkerWriteNoSpaceReclaimsCurrentTerminalJob(t *testing.T) {
	root := t.TempDir()
	manager := &CleanupManager{
		RootDir:     root,
		writeMarker: func(string, []byte) error { return syscall.ENOSPC },
	}
	job := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "job", Namespace: "project"}}
	createLocalArtifactState(t, root, job)
	if err := manager.MarkFailure(job); err != nil {
		t.Fatalf("mark failure after emergency reclaim: %v", err)
	}
	assertLocalArtifactState(t, root, job, false)
}

func TestArtifactCleanupMarkerWriteOtherErrorDoesNotReclaim(t *testing.T) {
	root := t.TempDir()
	manager := &CleanupManager{
		RootDir:     root,
		writeMarker: func(string, []byte) error { return syscall.EACCES },
	}
	job := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "job", Namespace: "project"}}
	createLocalArtifactState(t, root, job)
	if err := manager.MarkFailure(job); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("mark failure error = %v, want EACCES", err)
	}
	assertLocalArtifactState(t, root, job, true)
}

func TestArtifactCleanupUsesJobNameAcrossUIDChanges(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	manager := &CleanupManager{RootDir: root, FailedRetention: time.Hour, Now: func() time.Time { return now }}
	first := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "job", Namespace: "project", UID: "uid-1"}}
	if err := manager.MarkFailure(first); err != nil {
		t.Fatal(err)
	}
	second := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "job", Namespace: "project", UID: "uid-2"}}
	if err := manager.MarkFailure(second); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(root, "logs", "project", "job")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if err := manager.Sweep(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logDir); !os.IsNotExist(err) {
		t.Fatalf("log spool was not removed: %v", err)
	}
}

func createLocalArtifactState(t *testing.T, root string, job gateway.JobResource) {
	t.Helper()
	for _, category := range []string{"results", "logs", "uploads"} {
		dir := filepath.Join(root, category, job.Metadata.Namespace, job.Metadata.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "state"), []byte("state"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func assertLocalArtifactState(t *testing.T, root string, job gateway.JobResource, want bool) {
	t.Helper()
	for _, category := range []string{"results", "logs", "uploads"} {
		_, err := os.Stat(filepath.Join(root, category, job.Metadata.Namespace, job.Metadata.Name))
		if got := err == nil; got != want {
			t.Fatalf("%s existence = %v, want %v (err=%v)", category, got, want, err)
		}
	}
}
