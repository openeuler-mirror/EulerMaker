package fileutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRepositoryMetadataPaths(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{
		"p1/x86_64/.metadata/repositories/repo-1.json",
		"p2/aarch64/.metadata/repositories/repo-2.json",
		"p1/x86_64/.metadata/releases/build-1.json",
		"p1/x86_64/history/build/steps/repo/repository.json",
		"p1/x86_64/releases/build/release.json",
	} {
		path = filepath.Join(root, "repositories", path)
		if err := AtomicJSON(path, struct{}{}); err != nil {
			t.Fatal(err)
		}
	}
	for kind, count := range map[string]int{"repositories": 2, "releases": 1} {
		paths, err := RepositoryMetadataPaths(root, kind)
		if err != nil || len(paths) != count {
			t.Fatalf("%s paths: %v, %v", kind, paths, err)
		}
	}
	paths, err := RepositoryMetadataPaths(root, "repositories")
	if err != nil || paths["repo-1"] != filepath.Join(root, "repositories/p1/x86_64/.metadata/repositories/repo-1.json") {
		t.Fatalf("repository location: %v, %v", paths, err)
	}
	// A second scope cannot silently replace an existing identity in the lookup index.
	duplicate := filepath.Join(root, "repositories/p2/aarch64/.metadata/repositories/repo-1.json")
	if err := os.WriteFile(duplicate, []byte("{}"), 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := RepositoryMetadataPaths(root, "repositories"); err == nil {
		t.Fatal("duplicate repository identity was accepted")
	}
}
