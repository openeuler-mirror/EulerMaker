package fileutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CleanupRepositoryWork removes expired history or releases work directories before workers start.
func CleanupRepositoryWork(root, kind string, cutoff time.Time) error {
	if kind != "history" && kind != "releases" {
		return fmt.Errorf("invalid repository work directory %q", kind)
	}
	workDepth := 4
	if kind == "history" {
		workDepth = 5
	}
	base := filepath.Join(root, "repositories")
	return filepath.WalkDir(base, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(base, path)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if (len(parts) == 2 && parts[1] == ".stopped") || (len(parts) == 3 && parts[2] != kind) ||
			(len(parts) == workDepth && parts[workDepth-1] != ".work") {
			return filepath.SkipDir
		}
		if len(parts) == workDepth+1 {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.ModTime().Before(cutoff) {
				if err := os.RemoveAll(path); err != nil {
					return err
				}
			}
			return filepath.SkipDir
		}
		return nil
	})
}
