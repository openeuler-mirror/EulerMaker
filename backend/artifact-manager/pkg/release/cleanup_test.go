package release

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"artifact-manager/pkg/fileutil"
)

func TestDeleteBuildRelease(t *testing.T) {
	m := &Manager{root: t.TempDir(), records: map[string]*ReleaseRecord{}, running: map[string]bool{}}
	record := &ReleaseRecord{Project: "project", TargetArch: "arch", BuildName: "build", State: ReleasePrepared}
	if err := m.persist(record); err != nil {
		t.Fatal(err)
	}
	target := m.releaseTargetDir(record)
	for _, path := range []string{
		"releases/build/Packages/rpm", "releases/.work/build-123/partial", "releases/.trash/build-d_123/rpm",
		"releases/.records/build.json.tmp-m_123",
		"releases/other/Packages/rpm", "releases/.work/build-other-123/partial", "releases/.trash/build-other-d_123/rpm",
	} {
		if err := fileutil.AtomicJSON(filepath.Join(target, path), map[string]string{"content": "rpm"}); err != nil {
			t.Fatal(err)
		}
	}
	current := filepath.Join(target, "current")
	if err := os.Symlink("releases/build", current); err != nil {
		t.Fatal(err)
	}
	done, err := m.DeleteBuild("project", "arch", "build")
	var failure *Error
	if done || !errors.As(err, &failure) || failure.Status != http.StatusConflict || failure.Code != "ReleaseInUse" {
		t.Fatalf("current protection: %t, %v", done, err)
	}
	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	m.records[record.BuildName] = record
	m.running[record.BuildName] = true
	if done, err := m.DeleteBuild("project", "arch", "build"); err != nil || done {
		t.Fatalf("running release: %t, %v", done, err)
	}
	delete(m.running, record.BuildName)
	// A failed filesystem operation retains metadata and can be retried.
	if err := os.Remove(m.metaPath(record.BuildName)); err != nil {
		t.Fatal(err)
	}
	if err := fileutil.AtomicJSON(filepath.Join(m.metaPath(record.BuildName), "obstruction"), "x"); err != nil {
		t.Fatal(err)
	}
	if done, err := m.DeleteBuild("project", "arch", "build"); err == nil || done {
		t.Fatalf("partial cleanup: %t, %v", done, err)
	}
	if err := os.RemoveAll(m.metaPath(record.BuildName)); err != nil {
		t.Fatal(err)
	}
	// No cached record or existing directory is required to finish after restart.
	m = &Manager{root: m.root, records: map[string]*ReleaseRecord{}}
	for range 2 {
		if done, err := m.DeleteBuild("project", "arch", "build"); err != nil || !done {
			t.Fatalf("cleanup retry: %t, %v", done, err)
		}
	}
	for _, path := range []string{
		"releases/build", "releases/.records/build.json", "releases/.records/build.json.tmp-m_123",
		"releases/.work/build-123", "releases/.trash/build-d_123",
	} {
		if _, err := os.Stat(filepath.Join(target, path)); !os.IsNotExist(err) {
			t.Fatalf("cleanup left %s: %v", path, err)
		}
	}
	for _, path := range []string{"releases/other/Packages/rpm", "releases/.work/build-other-123/partial", "releases/.trash/build-other-d_123/rpm"} {
		if _, err := os.Stat(filepath.Join(target, path)); err != nil {
			t.Fatalf("unrelated path %s: %v", path, err)
		}
	}
}
