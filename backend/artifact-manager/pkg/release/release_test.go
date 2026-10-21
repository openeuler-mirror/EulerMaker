package release

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"artifact-manager/pkg/fileutil"
	"artifact-manager/pkg/repository"
)

func TestReleaseRequestNormalization(t *testing.T) {
	request := CreateReleaseRequest{
		BuildName:           "build",
		Project:             "project",
		TargetOS:            "os",
		TargetArch:          "arch",
		SourceRepositoryUID: "repository",
		ExcludeSpecs:        []string{"z", "a", "a"},
	}
	normalized, first, err := normalizeReleaseRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized.ExcludeSpecs) != 2 || normalized.ExcludeSpecs[0] != "a" || normalized.ExcludeSpecs[1] != "z" {
		t.Fatalf("exclude specs = %#v", normalized.ExcludeSpecs)
	}
	_, second, err := normalizeReleaseRequest(
		CreateReleaseRequest{
			BuildName:           "build",
			Project:             "project",
			TargetOS:            "os",
			TargetArch:          "arch",
			SourceRepositoryUID: "repository",
			ExcludeSpecs:        []string{"a", "z"},
		},
	)
	if err != nil || first != second {
		t.Fatalf("digest mismatch: %q %q, %v", first, second, err)
	}
	request.TargetOS = "../other"
	if _, _, err := normalizeReleaseRequest(request); err == nil {
		t.Fatal("unsafe target OS accepted")
	}
}

func TestFilesystemReleaseMaterializerExcludesSpecs(t *testing.T) {
	root := t.TempDir()
	command := filepath.Join(root, "createrepo-test")
	script := "#!/bin/sh\n" +
		"for last; do :; done\n" +
		"mkdir -p \"$last/repodata\"\n" +
		"printf primary > \"$last/repodata/primary.xml\"\n" +
		"sum=$(sha256sum \"$last/repodata/primary.xml\" | cut -d' ' -f1)\n" +
		"printf '<repomd><data type=\"primary\"><checksum type=\"sha256\">%s</checksum>" +
		"<location href=\"repodata/primary.xml\"/></data></repomd>' \"$sum\" > \"$last/repodata/repomd.xml\"\n"
	if err := os.WriteFile(command, []byte(script), 0750); err != nil {
		t.Fatal(err)
	}
	publicKey := filepath.Join(root, "public-key")
	if err := os.WriteFile(publicKey, []byte("key"), 0640); err != nil {
		t.Fatal(err)
	}
	c := Config{PublicKey: publicKey}
	repoConfig := repository.Config{CreateRepoCommand: command, CreateRepoWorkers: 1}
	if err := os.Mkdir(filepath.Join(root, ".release-work"), 0750); err != nil {
		t.Fatal(err)
	}
	materializer := NewFilesystemMaterializer(root, c, repoConfig)
	source := repository.RepositoryRecord{
		RepositoryUID: "source",
		Project:       "project",
		BuildName:     "build",
		TargetOS:      "openEuler",
		TargetArch:    "x86_64",
		RPMs: map[string]repository.RepositoryRPMMeta{
			"keep+1.rpm": {
				FileName: "keep+1.rpm",
				SpecName: "keep",
				Size:     int64(len("keep+1.rpm")),
				SHA256:   fmt.Sprintf("%x", sha256.Sum256([]byte("keep+1.rpm"))),
			},
			"skip.rpm": {
				FileName: "skip.rpm",
				SpecName: "skip",
				Size:     int64(len("skip.rpm")),
				SHA256:   fmt.Sprintf("%x", sha256.Sum256([]byte("skip.rpm"))),
			},
		},
	}
	sourcePackages := filepath.Join(
		root,
		"repositories",
		source.Project,
		source.TargetOS,
		source.TargetArch,
		"history",
		source.BuildName,
		"steps",
		source.RepositoryUID,
		"Packages",
	)
	if err := os.MkdirAll(sourcePackages, 0750); err != nil {
		t.Fatal(err)
	}
	for name := range source.RPMs {
		if err := os.WriteFile(filepath.Join(sourcePackages, name), []byte(name), 0640); err != nil {
			t.Fatal(err)
		}
	}
	record := ReleaseRecord{
		BuildName:           "build",
		Project:             "project",
		TargetOS:            "openEuler",
		TargetArch:          "x86_64",
		SourceRepositoryUID: "source",
		RequestDigest:       "request",
		ExcludeSpecs:        []string{"skip"},
	}
	result, err := materializer.Create(context.Background(), record, source)
	if err != nil {
		t.Fatal(err)
	}
	if result.Digest == "" {
		t.Fatalf("result = %#v", result)
	}
	releasePath := filepath.Join(root, "repositories", "project", "openEuler", "x86_64", "releases", "build")
	if _, err := os.Stat(filepath.Join(releasePath, "Packages", "keep+1.rpm")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(releasePath, "Packages", "skip.rpm")); !os.IsNotExist(err) {
		t.Fatalf("excluded RPM exists: %v", err)
	}
	index, err := readReleaseIndex(releasePath)
	if err != nil || index.PublicKeySHA256 == "" || index.ReleaseDigest != result.Digest {
		t.Fatalf("release index = %#v, %v", index, err)
	}
	if fullDigest, err := digestReleaseDirectory(releasePath); err != nil || fullDigest != result.Digest {
		t.Fatalf("reused release digest = %q, full digest = %q, error = %v", result.Digest, fullDigest, err)
	}
}

func TestFilesystemReleaseMaterializerReusesSourceMetadata(t *testing.T) {
	root := t.TempDir()
	c := Config{}
	repoConfig := repository.Config{
		CreateRepoCommand: filepath.Join(root, "createrepo-must-not-run"),
		CreateRepoWorkers: 1,
	}
	if err := os.Mkdir(filepath.Join(root, ".release-work"), 0750); err != nil {
		t.Fatal(err)
	}
	source := repository.RepositoryRecord{
		RepositoryUID: "source", Project: "project", BuildName: "build", TargetOS: "openEuler", TargetArch: "x86_64",
		RPMs: map[string]repository.RepositoryRPMMeta{
			"keep.rpm": {
				FileName: "keep.rpm",
				SpecName: "keep",
				Size:     3,
				SHA256:   fmt.Sprintf("%x", sha256.Sum256([]byte("rpm"))),
			},
		},
	}
	sourcePath := repository.VersionPath(
		root,
		source.Project,
		source.TargetOS,
		source.TargetArch,
		source.BuildName,
		source.RepositoryUID,
	)
	if err := os.MkdirAll(filepath.Join(sourcePath, "Packages"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sourcePath, "repodata"), 0750); err != nil {
		t.Fatal(err)
	}
	rpmPath := filepath.Join(sourcePath, "Packages", "keep.rpm")
	if err := os.WriteFile(rpmPath, []byte("rpm"), 0640); err != nil {
		t.Fatal(err)
	}
	primary := []byte("source primary metadata")
	if err := os.WriteFile(filepath.Join(sourcePath, "repodata", "primary.xml"), primary, 0640); err != nil {
		t.Fatal(err)
	}
	repomd := fmt.Sprintf(
		`<repomd><data type="primary"><checksum type="sha256">%x</checksum><location href="repodata/primary.xml"/></data></repomd>`,
		sha256.Sum256(primary),
	)
	if err := os.WriteFile(filepath.Join(sourcePath, "repodata", "repomd.xml"), []byte(repomd), 0640); err != nil {
		t.Fatal(err)
	}
	record := ReleaseRecord{
		BuildName:           "build",
		Project:             source.Project,
		TargetOS:            source.TargetOS,
		TargetArch:          source.TargetArch,
		SourceRepositoryUID: source.RepositoryUID,
		RequestDigest:       "request",
	}
	extraRPM := filepath.Join(sourcePath, "Packages", "untracked.rpm")
	if err := os.WriteFile(extraRPM, []byte("untracked"), 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFilesystemMaterializer(root, c, repoConfig).Create(context.Background(), record, source); err == nil {
		t.Fatal("untracked source RPM was accepted with reused metadata")
	}
	if err := os.Remove(extraRPM); err != nil {
		t.Fatal(err)
	}
	result, err := NewFilesystemMaterializer(root, c, repoConfig).Create(context.Background(), record, source)
	if err != nil {
		t.Fatal(err)
	}
	releasePath := filepath.Join(
		root,
		"repositories",
		source.Project,
		source.TargetOS,
		source.TargetArch,
		"releases",
		record.BuildName,
	)
	if fullDigest, err := digestReleaseDirectory(releasePath); err != nil || fullDigest != result.Digest {
		t.Fatalf("reused release digest = %q, full digest = %q, error = %v", result.Digest, fullDigest, err)
	}
	got, err := os.ReadFile(filepath.Join(releasePath, "repodata", "repomd.xml"))
	if err != nil || string(got) != repomd {
		t.Fatalf("release repomd.xml = %q, %v", got, err)
	}
	sourceRPM, err := os.Stat(rpmPath)
	if err != nil {
		t.Fatal(err)
	}
	releaseRPM, err := os.Stat(filepath.Join(releasePath, "Packages", "keep.rpm"))
	if err != nil || !os.SameFile(sourceRPM, releaseRPM) {
		t.Fatalf("release RPM does not link source: %v", err)
	}
}

func TestReadyReleaseRecoveryUsesPersistedDigest(t *testing.T) {
	root := t.TempDir()
	record := &ReleaseRecord{
		BuildName: "build", Project: "project", TargetOS: "openEuler", TargetArch: "x86_64",
		RequestDigest: "request", State: ReleaseReady,
		ReleaseDigest: fmt.Sprintf("%x", sha256.Sum256([]byte("release"))),
	}
	m := &Manager{root: root, records: map[string]*ReleaseRecord{}}
	for _, dir := range []string{
		m.Path(record),
		filepath.Join(root, ".release-work"),
		filepath.Join(root, ".metadata/releases"),
	} {
		if err := os.MkdirAll(dir, 0750); err != nil {
			t.Fatal(err)
		}
	}
	index := releaseIndex{
		BuildName:     record.BuildName,
		RequestDigest: record.RequestDigest,
		ReleaseDigest: record.ReleaseDigest,
	}
	if err := fileutil.AtomicJSON(filepath.Join(m.Path(record), "release.json"), index); err != nil {
		t.Fatal(err)
	}
	if err := m.persist(record); err != nil {
		t.Fatal(err)
	}
	// No RPM files are present: startup must not recompute the full directory digest.
	if err := m.recover(time.Hour); err != nil {
		t.Fatal(err)
	}
	record, ok, err := m.Get(record.BuildName)
	if err != nil || !ok || record.ReleaseDigest != index.ReleaseDigest || len(m.records) != 0 {
		t.Fatalf("persisted release = %#v, cached=%d, error=%v", record, len(m.records), err)
	}
	if record.State != ReleaseReady {
		t.Fatalf("matching metadata changed Ready release to %s", record.State)
	}
	response := ResponseFor(record)
	if response.ContentURL != "/repositories/project/openEuler/x86_64/" {
		t.Fatalf("recovered release content URL = %q", response.ContentURL)
	}

	index.ReleaseDigest = fmt.Sprintf("%x", sha256.Sum256([]byte("different")))
	if err := fileutil.AtomicJSON(filepath.Join(m.Path(record), "release.json"), index); err != nil {
		t.Fatal(err)
	}
	if err := m.recover(time.Hour); err != nil {
		t.Fatal(err)
	}
	record, ok, err = m.Get(record.BuildName)
	if err != nil || !ok || len(m.records) != 0 {
		t.Fatalf("recovered release = %#v, cached=%d, error=%v", record, len(m.records), err)
	}
	if record.State != ReleaseFailed || record.Failure == nil || record.Failure.Code != "ReleaseContentInvalid" {
		t.Fatalf("mismatched metadata did not fail release: %#v", record)
	}
}

func TestReleaseCacheTracksActiveStates(t *testing.T) {
	m := &Manager{root: t.TempDir(), records: map[string]*ReleaseRecord{}}
	record := &ReleaseRecord{BuildName: "build", Project: "project", TargetOS: "os", TargetArch: "arch"}
	for _, state := range []ReleaseState{ReleaseCreating, ReleasePrepared, ReleaseReady, ReleaseDeleting, ReleaseFailed} {
		record.State = state
		if err := m.persist(record); err != nil {
			t.Fatal(err)
		}
		active := state == ReleaseCreating || state == ReleasePrepared || state == ReleaseDeleting
		if _, cached := m.records[record.BuildName]; cached != active {
			t.Fatalf("state %s: cached=%t, want %t", state, cached, active)
		}
		got, ok, err := m.Get(record.BuildName)
		if err != nil || !ok || got.State != state {
			t.Fatalf("Get(%s) = %#v, %t, %v", state, got, ok, err)
		}
		got.State = ReleaseFailed
		if cached := m.records[record.BuildName]; active && cached.State != state {
			t.Fatal("mutating returned record changed cached state")
		}
	}
	// Terminal reads observe the persisted record without retaining a copy.
	record.UpdatedAt = time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	if err := fileutil.AtomicJSON(m.metaPath(record.BuildName), record); err != nil {
		t.Fatal(err)
	}
	got, ok, err := m.Get(record.BuildName)
	if err != nil || !ok || !got.UpdatedAt.Equal(record.UpdatedAt) || len(m.records) != 0 {
		t.Fatalf("disk record = %#v, cached=%d, error=%v", got, len(m.records), err)
	}
}

func TestReleaseRecoveryCachesActiveRecords(t *testing.T) {
	m := &Manager{root: t.TempDir(), records: map[string]*ReleaseRecord{}}
	if err := os.MkdirAll(filepath.Join(m.root, ".release-work"), 0750); err != nil {
		t.Fatal(err)
	}
	states := []ReleaseState{ReleaseCreating, ReleasePrepared, ReleaseDeleting, ReleaseFailed}
	for _, state := range states {
		record := &ReleaseRecord{
			BuildName:  string(state),
			Project:    "project",
			TargetOS:   "os",
			TargetArch: "arch",
			State:      state,
		}
		if err := fileutil.AtomicJSON(m.metaPath(record.BuildName), record); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.recover(time.Hour); err != nil {
		t.Fatal(err)
	}
	if len(m.records) != 3 {
		t.Fatalf("cached records = %d, want 3", len(m.records))
	}
	for _, state := range states {
		got, ok, err := m.Get(string(state))
		if err != nil || !ok || got.State != state {
			t.Fatalf("recovered %s = %#v, %t, %v", state, got, ok, err)
		}
	}
}

func TestReleaseHistoryCleanupReadsDiskRecords(t *testing.T) {
	m := &Manager{root: t.TempDir(), records: map[string]*ReleaseRecord{}, historyCount: 1, historyTTL: time.Hour}
	now := time.Now().UTC()
	for i, name := range []string{"latest", "old", "current"} {
		record := &ReleaseRecord{
			BuildName: name, Project: "project", TargetOS: "os", TargetArch: "arch", State: ReleaseReady,
			UpdatedAt: now.Add(-time.Duration(i+2) * time.Hour),
		}
		if err := os.MkdirAll(m.Path(record), 0750); err != nil {
			t.Fatal(err)
		}
		if err := m.persist(record); err != nil {
			t.Fatal(err)
		}
		if name == "current" {
			if err := os.Symlink("releases/current", filepath.Join(m.releaseTargetDir(record), "current")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(m.records) != 0 {
		t.Fatalf("historical records cached: %d", len(m.records))
	}
	m.cleanupHistory(now)
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, ok, err := m.Get("old")
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("old release was not removed")
		}
		time.Sleep(time.Millisecond)
	}
	for _, name := range []string{"latest", "current"} {
		got, ok, err := m.Get(name)
		if err != nil || !ok || got.State != ReleaseReady {
			t.Fatalf("retained %s = %#v, %t, %v", name, got, ok, err)
		}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.records) != 0 {
		t.Fatalf("completed cleanup retained %d records", len(m.records))
	}
}
