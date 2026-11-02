package repository

import (
	"os"
	"path/filepath"
	"testing"

	"artifact-manager/pkg/fileutil"
)

func TestRepositoryManagerCachesOnlyRecoverableStates(t *testing.T) {
	root := t.TempDir()
	m := &Manager{root: root, records: map[string]*RepositoryRecord{}}
	for _, state := range []RepositoryState{RepositoryCreating, RepositoryDeleting, RepositoryReady, RepositoryFailed} {
		record := &RepositoryRecord{
			RepositoryUID: string(state),
			State:         state,
			Project:       "project",
			BuildName:     "build",
			TargetArch:    "arch",
		}
		if state == RepositoryReady {
			record.RepositoryDigest = "repository-digest"
		}
		if err := m.persist(record); err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(root, "repositories/project/arch/history/build/.records", record.RepositoryUID+".json")
		if m.metaPath(record.RepositoryUID) != want {
			t.Fatalf("metadata path = %s, want %s", m.metaPath(record.RepositoryUID), want)
		}
		if state == RepositoryReady {
			path := m.RepositoryPath(record)
			if err := os.MkdirAll(path, 0750); err != nil {
				t.Fatal(err)
			}
			if err := fileutil.AtomicJSON(filepath.Join(path, ".repository.json"), Index{
				RepositoryUID: record.RepositoryUID,
				RPMs:          map[string]RepositoryRPMMeta{},
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	m.metadataPaths = nil
	if err := m.load(); err != nil {
		t.Fatal(err)
	}
	if len(m.records) != 2 || m.records[string(RepositoryCreating)] == nil || m.records[string(RepositoryDeleting)] == nil {
		t.Fatalf("cached states = %+v", m.records)
	}
	for _, state := range []RepositoryState{RepositoryReady, RepositoryFailed} {
		record, ok, err := m.Get(string(state))
		if err != nil || !ok || record.State != state {
			t.Fatalf("get %s = %+v, %v", state, record, err)
		}
		if state == RepositoryReady && record.RepositoryDigest != "repository-digest" {
			t.Fatalf("persisted repository digest = %q", record.RepositoryDigest)
		}
	}
	if len(m.records) != 2 {
		t.Fatalf("on-demand reads were cached: %+v", m.records)
	}
	ready := &RepositoryRecord{
		RepositoryUID: string(RepositoryReady),
		Project:       "project",
		BuildName:     "build",
		TargetArch:    "arch",
	}
	if err := os.Remove(filepath.Join(m.RepositoryPath(ready), ".repository.json")); err != nil {
		t.Fatal(err)
	}
	record, ok, err := m.Get(ready.RepositoryUID)
	if err != nil || !ok || record.State != RepositoryFailed || record.Failure == nil || record.Failure.Code != "RepositoryContentMissing" {
		t.Fatalf("missing Ready content = %+v, %v", record, err)
	}
}

func TestRepositoryDeletionRemovesLocationIndex(t *testing.T) {
	m := &Manager{root: t.TempDir(), records: map[string]*RepositoryRecord{}}
	record := &RepositoryRecord{RepositoryUID: "repo", Project: "project", TargetArch: "arch", BuildName: "build", State: RepositoryDeleting}
	if err := m.persist(record); err != nil {
		t.Fatal(err)
	}
	m.records[record.RepositoryUID] = record
	path := m.metaPath(record.RepositoryUID)
	source := m.RepositoryPath(record)
	if err := os.MkdirAll(source, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, ".repository.json"), []byte("{}"), 0640); err != nil {
		t.Fatal(err)
	}
	m.remove(record.RepositoryUID)
	if entries, err := os.ReadDir(filepath.Join(m.root, "repositories/project/arch/history/build/.trash")); err != nil || len(entries) != 0 {
		t.Fatalf("scoped repository trash: %v, %v", entries, err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("deleted repository content: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("deleted metadata: %v", err)
	}
	if len(m.metadataPaths) != 0 || len(m.records) != 0 {
		t.Fatalf("deletion retained index or record: %v, %v", m.metadataPaths, m.records)
	}
	if _, ok, err := m.Get(record.RepositoryUID); err != nil || ok {
		t.Fatalf("deleted lookup: %t, %v", ok, err)
	}
}

func TestRepositorySigningFingerprintIsFixedForRequest(t *testing.T) {
	root := t.TempDir()
	m := &Manager{
		root:               root,
		signingFingerprint: "new-key",
		records:            map[string]*RepositoryRecord{},
		queued:             map[string]bool{},
		queue:              make(chan string, 1),
	}
	in := CreateRepositoryRequest{
		RepositoryName:    "build",
		Project:           "project",
		BuildName:         "build",
		TargetArch:        "arch",
		BaseRepositoryUID: "base",
		Manifests:         []ManifestReference{{JobName: "job"}},
	}
	in.RepositoryUID = UID(in.Project, in.BuildName, in.BaseRepositoryUID, in.Manifests)
	m.records["base"] = &RepositoryRecord{
		RepositoryUID:      "base",
		Project:            in.Project,
		TargetArch:         in.TargetArch,
		State:              RepositoryReady,
		SigningFingerprint: "old-key",
	}
	_, _, err := m.Submit(in)
	if typed, ok := err.(*Error); !ok || typed.Code != "BaseRepositorySigningMismatch" {
		t.Fatalf("submit = %v", err)
	}
	in.BaseRepositoryUID = ""
	in.RepositoryUID = UID(in.Project, in.BuildName, in.BaseRepositoryUID, in.Manifests)
	record, _, err := m.Submit(in)
	if err != nil || record.SigningFingerprint != "new-key" {
		t.Fatalf("new record = %+v, %v", record, err)
	}
	m.signingFingerprint = "changed-key"
	_, _, err = m.Submit(in)
	if typed, ok := err.(*Error); !ok || typed.Code != "SigningConfigurationChanged" {
		t.Fatalf("retry = %v", err)
	}
}

func TestRepositoryRejectsUnsafeTargetArch(t *testing.T) {
	request := CreateRepositoryRequest{
		RepositoryName: "build",
		Project:        "project",
		BuildName:      "build",
		TargetArch:     "../other",
		Manifests:      []ManifestReference{{JobName: "job"}},
	}
	request.RepositoryUID = UID(request.Project, request.BuildName, "", request.Manifests)
	if _, _, err := normalizeRepositoryRequest(request); err == nil {
		t.Fatal("unsafe target architecture accepted")
	}
}
