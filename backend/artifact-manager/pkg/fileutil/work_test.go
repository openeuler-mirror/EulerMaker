package fileutil

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanupRepositoryWork(t *testing.T) {
	for _, kind := range []string{"history", "releases"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			workPath := "releases/.work"
			if kind == "history" {
				workPath = "history/build/.work"
			}
			cases := []struct {
				path         string
				old, removed bool
			}{
				{"p1/arch1/" + workPath + "/old-task", true, true},
				{"p2/arch2/" + workPath + "/old-task", true, true},
				{"p1/arch1/" + workPath + "/recent-task", false, false},
				{"p1/arch1/history/other-build/.work/old-task", true, kind == "history"},
				{"p1/arch1/releases/.work/other-build-task", true, kind == "releases"},
				{"p1/arch1/history/build/steps/repository/Packages", true, false},
				{"p1/arch1/history/build", true, false},
				{"p1/arch1/releases/build", true, false},
				{"p1/arch1/releases/build/Packages", true, false},
				{"p1/arch1/history/build/.records", true, false},
				{"p1/arch1/history/build/.records/.work/old-task", true, false},
				{"p1/arch1/releases/.records", true, false},
				{"p1/arch1/history/build/.trash/old-task", true, false},
				{"p1/arch1/history/build/.trash/.work/old-task", true, false},
				{"p1/arch1/releases/.trash/old-task", true, false},
			}
			for _, item := range cases {
				path := filepath.Join(root, "repositories", item.path)
				if err := os.MkdirAll(path, 0750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "content"), []byte("data"), 0640); err != nil {
					t.Fatal(err)
				}
				if item.old {
					old := time.Now().Add(-48 * time.Hour)
					if err := os.Chtimes(path, old, old); err != nil {
						t.Fatal(err)
					}
				}
			}
			link := filepath.Join(root, "repositories/p1/arch1", workPath, "linked-task")
			if err := os.Symlink(filepath.Join(root, "repositories/p1/arch1/releases/build"), link); err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				if err := CleanupRepositoryWork(root, kind, time.Now().Add(-24*time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			for _, item := range cases {
				_, err := os.Stat(filepath.Join(root, "repositories", item.path))
				if item.removed && !os.IsNotExist(err) || !item.removed && err != nil {
					t.Errorf("cleanup %s: removed=%t, err=%v", item.path, item.removed, err)
				}
			}
			if _, err := os.Lstat(link); err != nil {
				t.Fatal(err)
			}
		})
	}
}
