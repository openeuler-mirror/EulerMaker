package upload

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeleteJobClearsTransientState(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ch, done := s.Subscribe("project", "job")
	defer done()
	other, otherDone := s.Subscribe("project", "other")
	defer otherDone()
	key, otherKey := logKey("project", "job", "combined"), logKey("project", "other", "combined")
	s.logRecovery[key], s.logRecovery[otherKey] = struct{}{}, struct{}{}
	s.sealing[logKey("project", "job", "")] = false
	s.sealing[logKey("project", "other", "")] = true
	if err := s.DeleteJob("project", "job"); err != nil {
		t.Fatal(err)
	}
	select {
	case _, open := <-ch:
		if open {
			t.Fatal("deleted Job subscription remains open")
		}
	default:
		t.Fatal("deleted Job subscription was not closed")
	}
	select {
	case <-other:
		t.Fatal("unrelated subscription changed")
	default:
	}
	if len(s.subscribers) != 1 || len(s.subscribers[otherKey]) != 1 || len(s.logRecovery) != 1 || len(s.sealing) != 1 {
		t.Fatalf("transient state after deletion: subscriptions=%v recovery=%v sealing=%v",
			s.subscribers, s.logRecovery, s.sealing)
	}
	if _, exists := s.logRecovery[otherKey]; !exists || !s.sealing[logKey("project", "other", "")] {
		t.Fatal("unrelated Job state removed")
	}
	done() // A deferred SSE unsubscribe remains safe after deletion closes the channel.
}

func TestDeleteJobSymlinkBoundaries(t *testing.T) {
	for _, entry := range []string{"artifacts", "artifacts/project", "artifacts/project/jobs",
		"artifacts/project/jobs/job", "artifacts/project/jobs/job/link"} {
		t.Run(entry, func(t *testing.T) {
			root := t.TempDir()
			s, err := NewStore(root)
			if err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			marker := filepath.Join(outside, "keep")
			if err := os.WriteFile(marker, []byte("retained"), 0600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, entry)
			if entry == "artifacts" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
			err = s.DeleteJob("project", "job")
			isParent := entry == "artifacts" || entry == "artifacts/project" || entry == "artifacts/project/jobs"
			if (err != nil) != isParent {
				t.Fatalf("delete through %s: %v", entry, err)
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "retained" {
				t.Fatalf("symlink target changed: %q, %v", data, err)
			}
			if !isParent {
				if _, err := os.Lstat(filepath.Join(root, "artifacts/project/jobs/job")); !os.IsNotExist(err) {
					t.Fatalf("Job entry remains: %v", err)
				}
			}
		})
	}
}

func TestDeleteJobAfterRestartWithPartialData(t *testing.T) {
	root := t.TempDir()
	jobDir := filepath.Join(root, "artifacts/project/jobs/job")
	for _, file := range []string{"packages/pkg.rpm", "logs/container.log", ".signed/pkg.rpm", ".uploads/pkg.tmp"} {
		path := filepath.Join(jobDir, file)
		if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("remaining data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate a deletion interrupted after metadata and one body were removed.
	if err := os.Remove(filepath.Join(jobDir, "packages/pkg.rpm")); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.DeleteJob("project", "job"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
		t.Fatalf("Job data remains: %v", err)
	}
}
