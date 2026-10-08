package artifact

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

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
