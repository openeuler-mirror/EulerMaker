package storage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorePublishAndQuarantine(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	name, path, err := store.CreateTemporary("clone", strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(name, "example.com/team/repo.git"); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := store.RepositoryPath("example.com/team/repo.git"); err != nil || !exists {
		t.Fatalf("repository exists=%v err=%v", exists, err)
	}
	quarantine, found, err := store.Quarantine("example.com/team/repo.git", strings.Repeat("b", 32))
	if err != nil || !found {
		t.Fatalf("quarantine found=%v err=%v", found, err)
	}
	if err := store.Cleanup(quarantine); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRejectsSymlinkParent(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "example.com")); err != nil {
		t.Fatal(err)
	}
	_, _, err = store.RepositoryPath("example.com/team/repo.git")
	if err == nil || !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
}
