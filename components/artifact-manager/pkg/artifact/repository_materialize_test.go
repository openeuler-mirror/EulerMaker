package artifact

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRepositoryBatchReusesBaseRPMMetadata(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "base")
	basePackages := filepath.Join(base, "Packages")
	work := filepath.Join(root, "work")
	workPackages := filepath.Join(work, "Packages")
	for _, directory := range []string{basePackages, workPackages, filepath.Join(work, "repodata")} {
		if err := os.MkdirAll(directory, 0750); err != nil {
			t.Fatal(err)
		}
	}
	baseMetadata := make(map[string]RepositoryRPMMeta)
	for name, spec := range map[string]string{"old-a.rpm": "a", "old-b.rpm": "b"} {
		path := filepath.Join(basePackages, name)
		if err := os.WriteFile(path, []byte(name), 0640); err != nil {
			t.Fatal(err)
		}
		sum, err := fileSHA256(path)
		if err != nil {
			t.Fatal(err)
		}
		baseMetadata[name] = RepositoryRPMMeta{FileName: name, SpecName: spec, Size: int64(len(name)), SHA256: sum}
	}
	index, err := json.Marshal(repositoryIndex{RepositoryUID: "base-uid", RPMs: baseMetadata})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "repository.json"), index, 0640); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadRepositoryMetadata(base, "base-uid")
	if err != nil {
		t.Fatal(err)
	}
	retained, err := linkRPMDirectory(basePackages, workPackages, loaded, map[string]bool{"a": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(retained) != 1 || retained["old-b.rpm"].SpecName != "b" {
		t.Fatalf("retained RPMs = %+v", retained)
	}
	if _, err := os.Stat(filepath.Join(workPackages, "old-a.rpm")); !os.IsNotExist(err) {
		t.Fatalf("replaced RPM still linked: %v", err)
	}
	baseInfo, err := os.Stat(filepath.Join(basePackages, "old-b.rpm"))
	if err != nil {
		t.Fatal(err)
	}
	workInfo, err := os.Stat(filepath.Join(workPackages, "old-b.rpm"))
	if err != nil || !os.SameFile(baseInfo, workInfo) {
		t.Fatalf("retained RPM was not hard-linked: %v", err)
	}
	newPath := filepath.Join(workPackages, "new-a.rpm")
	if err := os.WriteFile(newPath, []byte("new-a"), 0640); err != nil {
		t.Fatal(err)
	}
	sum, err := fileSHA256(newPath)
	if err != nil {
		t.Fatal(err)
	}
	retained["new-a.rpm"] = RepositoryRPMMeta{FileName: "new-a.rpm", SpecName: "a", Size: 5, SHA256: sum}
	if err := os.WriteFile(filepath.Join(work, "repodata", "repomd.xml"), []byte("metadata"), 0640); err != nil {
		t.Fatal(err)
	}
	cachedDigest, err := digestDirectoryWithRPMMetadata(work, retained)
	if err != nil {
		t.Fatal(err)
	}
	fullDigest, err := digestDirectory(work)
	if err != nil {
		t.Fatal(err)
	}
	if cachedDigest != fullDigest {
		t.Fatalf("cached digest = %s, full digest = %s", cachedDigest, fullDigest)
	}
}

func TestInspectRPMSourceIdentityAndSubpackageOrigin(t *testing.T) {
	dir := t.TempDir()
	rpmCommand := filepath.Join(dir, "rpm")
	command := `#!/bin/sh
case "$2" in
  --qf)
    case "${4##*/}" in
      chrpath-0.16-14.aarch64.rpm) printf 'chrpath\t0\t0.16\t14\taarch64\tchrpath-0.16-14.src.rpm' ;;
      chrpath-0.16-14.src.rpm) printf 'chrpath\t0\t0.16\t14\taarch64\t(none)' ;;
      texlive-dhua-svn24035.0.11-2.noarch.rpm) printf 'texlive-dhua\t0\tsvn24035.0.11\t2\tnoarch\ttexlive-split-g-2021-2.src.rpm' ;;
      *) exit 1 ;;
    esac ;;
  --provides|--requires) ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(rpmCommand, []byte(command), 0755); err != nil {
		t.Fatal(err)
	}
	materializer := &filesystemMaterializer{rpmQueryCommand: rpmCommand}
	inspect := func(name string) RepositoryRPMMeta {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
		meta, err := materializer.inspectRPMFile(context.Background(), path)
		if err != nil {
			t.Fatalf("inspectRPMFile(%s): %v", name, err)
		}
		return meta
	}

	binary := inspect("chrpath-0.16-14.aarch64.rpm")
	source := inspect("chrpath-0.16-14.src.rpm")
	if binary.SpecName != "chrpath" || source.SpecName != "chrpath" || binary.Arch != "aarch64" || source.Arch != "src" {
		t.Fatalf("unexpected chrpath metadata: binary=%+v source=%+v", binary, source)
	}
	if rpmIdentity(binary) == rpmIdentity(source) {
		t.Fatalf("binary and source RPM share identity: %q", rpmIdentity(binary))
	}

	subpackage := inspect("texlive-dhua-svn24035.0.11-2.noarch.rpm")
	if subpackage.SpecName != "texlive-split-g" || subpackage.Version != "svn24035.0.11" {
		t.Fatalf("subpackage origin = %+v", subpackage)
	}
}
