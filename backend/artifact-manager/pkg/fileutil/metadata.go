package fileutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RepositoryMetadataPaths indexes scoped state files without traversing repository content.
func RepositoryMetadataPaths(root, kind string) (map[string]string, error) {
	paths := make(map[string]string)
	base := filepath.Join(root, "repositories")
	err := filepath.WalkDir(base, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(base, path)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if entry.IsDir() {
			if (len(parts) == 3 && parts[2] != ".metadata") ||
				(len(parts) == 4 && parts[3] != kind) || len(parts) > 4 {
				return filepath.SkipDir
			}
			return nil
		}
		if len(parts) != 5 || filepath.Ext(path) != ".json" {
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
