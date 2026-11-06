package fileutil

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestScopedDirectoryCleanup(t *testing.T) {
	dataDir, outside := t.TempDir(), t.TempDir()
	keep := filepath.Join(outside, "keep")
	if err := os.WriteFile(keep, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dataDir, "linked-parent")); err != nil {
		t.Fatal(err)
	}
	if root, err := OpenDirectory(dataDir, "linked-parent"); err == nil || root != nil {
		t.Fatalf("symlink parent accepted: %v", err)
	}
	if root, err := OpenDirectory(filepath.Join(dataDir, "missing-root"), "child"); err == nil || root != nil {
		t.Fatalf("missing data root accepted: %v", err)
	}
	if root, err := OpenDirectory(dataDir, "missing-child"); err != nil || root != nil {
		t.Fatalf("missing child: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "history/build"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dataDir, "history/build/link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(keep, filepath.Join(dataDir, "history/build/shared")); err != nil {
		t.Fatal(err)
	}
	parent, err := OpenDirectory(dataDir, "history")
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	for range 2 {
		if err := RemoveEntry(parent, "build", true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, "history/build")); !os.IsNotExist(err) {
		t.Fatalf("build not removed: %v", err)
	}
	if data, err := os.ReadFile(keep); err != nil || string(data) != "keep" {
		t.Fatalf("linked content modified: %q, %v", data, err)
	}
}

func TestDirectoryNames(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"x86_64", "aarch64", ".stopped"} {
		if err := os.MkdirAll(filepath.Join(root, "repositories/project", name), 0750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "repositories/project/note"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	names, err := DirectoryNames(root, "repositories", "project")
	if err != nil || !slices.Equal(names, []string{".stopped", "aarch64", "x86_64"}) {
		t.Fatalf("directories=%v err=%v", names, err)
	}
	if names, err := DirectoryNames(root, "repositories", "missing"); err != nil || len(names) != 0 {
		t.Fatalf("missing project: %v, %v", names, err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "repositories/project/linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := DirectoryNames(root, "repositories", "project"); err == nil {
		t.Fatal("symlink architecture accepted")
	}
}
