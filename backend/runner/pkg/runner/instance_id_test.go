package runner

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestLoadOrCreateRunnerInstanceIDPersistsUUIDv4(t *testing.T) {
	root := t.TempDir()
	first, err := loadOrCreateRunnerInstanceID(root)
	if err != nil {
		t.Fatalf("create instance ID: %v", err)
	}
	if !runnerInstanceIDPattern.MatchString(first) {
		t.Fatalf("instance ID %q is not a canonical UUID v4", first)
	}
	second, err := loadOrCreateRunnerInstanceID(root)
	if err != nil {
		t.Fatalf("reload instance ID: %v", err)
	}
	if second != first {
		t.Fatalf("instance ID changed: %q != %q", second, first)
	}
	info, err := os.Stat(filepath.Join(root, runnerInstanceIDFile))
	if err != nil {
		t.Fatalf("stat instance ID: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("instance ID permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestLoadOrCreateRunnerInstanceIDIsAtomic(t *testing.T) {
	root := t.TempDir()
	const workers = 8
	ids := make(chan string, workers)
	errs := make(chan error, workers)
	var group sync.WaitGroup
	for i := 0; i < workers; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			id, err := loadOrCreateRunnerInstanceID(root)
			ids <- id
			errs <- err
		}()
	}
	group.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("initialize instance ID concurrently: %v", err)
		}
	}
	var want string
	for id := range ids {
		if want == "" {
			want = id
		}
		if id != want {
			t.Fatalf("concurrent instance IDs differ: %q != %q", id, want)
		}
	}
}

func TestLoadOrCreateRunnerInstanceIDRejectsInvalidFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, runnerInstanceIDFile)
	if err := os.WriteFile(path, []byte("not-a-uuid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateRunnerInstanceID(root); err == nil {
		t.Fatal("expected invalid instance ID to be rejected")
	}
}

func TestLoadOrCreateRunnerInstanceIDRejectsBroadPermissions(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, runnerInstanceIDFile)
	if err := os.WriteFile(path, []byte("01234567-89ab-4def-8123-456789abcdef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateRunnerInstanceID(root); err == nil {
		t.Fatal("expected broad instance ID file permissions to be rejected")
	}
}
