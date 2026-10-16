package repository

import (
	"os"
	"path/filepath"
	"testing"

	"artifact-manager/pkg/upload"
)

func TestRepositoryManagerCachesOnlyRecoverableStates(t *testing.T) {
	root := t.TempDir()
	m := &Manager{root: root, records: map[string]*RepositoryRecord{}}
	if err := os.MkdirAll(filepath.Join(root, ".metadata", "repositories"), 0750); err != nil {
		t.Fatal(err)
	}
	for _, state := range []RepositoryState{RepositoryCreating, RepositoryDeleting, RepositoryReady, RepositoryFailed} {
		record := &RepositoryRecord{RepositoryUID: string(state), State: state, Project: "project", BuildName: "build", TargetOS: "os", TargetArch: "arch"}
		if state == RepositoryReady {
			record.RepositoryDigest = "repository-digest"
		}
		if err := m.persist(record); err != nil {
			t.Fatal(err)
		}
		if state == RepositoryReady {
			path := m.RepositoryPath(record)
			if err := os.MkdirAll(path, 0750); err != nil {
				t.Fatal(err)
			}
			if err := upload.AtomicJSON(filepath.Join(path, "repository.json"), Index{RepositoryUID: record.RepositoryUID, RPMs: map[string]RepositoryRPMMeta{}}); err != nil {
				t.Fatal(err)
			}
		}
	}
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
	ready := &RepositoryRecord{RepositoryUID: string(RepositoryReady), Project: "project", BuildName: "build", TargetOS: "os", TargetArch: "arch"}
	if err := os.Remove(filepath.Join(m.RepositoryPath(ready), "repository.json")); err != nil {
		t.Fatal(err)
	}
	record, ok, err := m.Get(ready.RepositoryUID)
	if err != nil || !ok || record.State != RepositoryFailed || record.Failure == nil || record.Failure.Code != "RepositoryContentMissing" {
		t.Fatalf("missing Ready content = %+v, %v", record, err)
	}
}

func TestRepositorySigningFingerprintIsFixedForRequest(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".metadata", "repositories"), 0750); err != nil {
		t.Fatal(err)
	}
	m := &Manager{root: root, signingFingerprint: "new-key", records: map[string]*RepositoryRecord{}, queued: map[string]bool{}, queue: make(chan string, 1)}
	in := CreateRepositoryRequest{RepositoryName: "build", Project: "project", BuildName: "build", TargetOS: "os", TargetArch: "arch", BaseRepositoryUID: "base", Manifests: []ManifestReference{{JobName: "job"}}}
	in.RepositoryUID = UID(in.Project, in.BuildName, in.BaseRepositoryUID, in.Manifests)
	m.records["base"] = &RepositoryRecord{RepositoryUID: "base", Project: in.Project, TargetOS: in.TargetOS, TargetArch: in.TargetArch, State: RepositoryReady, SigningFingerprint: "old-key"}
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

func TestRepositoryRejectsUnsafeTargetOS(t *testing.T) {
	request := CreateRepositoryRequest{RepositoryName: "build", Project: "project", BuildName: "build", TargetOS: "../other", TargetArch: "x86_64", Manifests: []ManifestReference{{JobName: "job"}}}
	request.RepositoryUID = UID(request.Project, request.BuildName, "", request.Manifests)
	if _, _, err := normalizeRepositoryRequest(request); err == nil {
		t.Fatal("unsafe target OS accepted")
	}
}
