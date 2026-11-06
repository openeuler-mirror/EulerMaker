package fileutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RepositoryMetadataPaths indexes scoped state files without traversing repository content.
func RepositoryMetadataPaths(root, kind string) (map[string]string, error) {
	if kind != "history" && kind != "releases" {
		return nil, fmt.Errorf("invalid repository record directory %q", kind)
	}
	recordDepth := 4
	if kind == "history" {
		recordDepth = 5
	}
	paths := make(map[string]string)
	base := filepath.Join(root, "repositories")
	err := filepath.WalkDir(base, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(base, path)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if entry.IsDir() {
			if (len(parts) == 2 && parts[1] == ".stopped") || (len(parts) == 3 && parts[2] != kind) ||
				(len(parts) == recordDepth && parts[recordDepth-1] != ".records") || len(parts) > recordDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if len(parts) != recordDepth+1 || filepath.Ext(path) != ".json" {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("invalid metadata file %s", path)
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if previous, exists := paths[id]; exists {
			return fmt.Errorf("duplicate metadata identity %s: %s and %s", id, previous, path)
		}
		paths[id] = path
		return nil
	})
	return paths, err
}
