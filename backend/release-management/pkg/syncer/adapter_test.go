package syncer

import (
	"reflect"
	"strings"
	"testing"

	ebsv1 "ebs-api/ebs/v1"
)

func manifest(names ...string) Manifest {
	data := "packages:\n"
	for _, name := range names {
		data += "  - name: " + name + "\n"
	}
	return Manifest{Path: "baseos/packages.yaml", Data: []byte(data)}
}

func TestBuildList(t *testing.T) {
	b := testConfig(t).Bindings[0]
	files := []Manifest{manifest("gcc", "zlib"), manifest("zlib", "ant")}
	list, err := buildList(b, files, "refs/heads/default")
	if err != nil {
		t.Fatal(err)
	}
	expected := []ebsv1.PackageRepo{
		{Name: "ant", URL: "https://git.example.com/ant.git"},
		{Name: "gcc", URL: "https://git.example.com/gcc.git"},
		{Name: "zlib", URL: "https://git.example.com/zlib.git"},
	}
	if !reflect.DeepEqual(list, expected) {
		t.Fatal(list)
	}
	reversed, err := buildList(b, []Manifest{files[1], files[0]}, "refs/heads/default")
	if err != nil || digest(list) != digest(reversed) {
		t.Fatal("unstable digest", err)
	}
	files[1].PackageRef = "refs/heads/other"
	if _, err := buildList(b, files, "refs/heads/default"); err == nil {
		t.Fatal("conflicting ref accepted")
	}
}

func TestBuildListReferences(t *testing.T) {
	b := testConfig(t).Bindings[0]
	f := manifest("gcc")
	if _, err := buildList(b, []Manifest{f}, ""); err == nil {
		t.Fatal("missing default reference accepted")
	}
	f.PackageRef = "refs/tags/release"
	list, err := buildList(b, []Manifest{f}, "")
	if err != nil || list[0].Ref != f.PackageRef {
		t.Fatal(list, err)
	}
	old := append([]ebsv1.PackageRepo{}, list...)
	old = append(old, ebsv1.PackageRepo{Name: "old", URL: "https://git.example.com/old.git"})
	d := difference(old, list)
	if d.Removed != 1 || d.Changes[0].Before.Name != "old" {
		t.Fatal(d)
	}
}

func TestDifferenceIncludesManualOverrides(t *testing.T) {
	b := testConfig(t).Bindings[0]
	list, err := buildList(b, []Manifest{manifest("gcc")}, "refs/heads/default")
	if err != nil {
		t.Fatal(err)
	}
	old := []ebsv1.PackageRepo{
		{Name: "gcc", URL: "https://custom.example.com/gcc.git", Ref: "refs/heads/custom", CommitID: strings.Repeat("a", 40)},
		{Name: "manual", URL: "https://custom.example.com/manual.git"},
	}
	d := difference(old, list)
	if d.Modified != 1 || d.Removed != 1 || d.Added != 0 || len(d.Changes) != 2 {
		t.Fatal(d)
	}
	if *d.Changes[0].Before != old[0] || *d.Changes[0].After != list[0] || d.Changes[1].After != nil {
		t.Fatal("preview did not show replacement of manual configuration", d)
	}
}

func TestMalformedManifest(t *testing.T) {
	b := testConfig(t).Bindings[0]
	for _, data := range []string{"packages: [gcc]", "packages: [{name: 42}]", "packages: [{name: '../bad'}]", "packages: {}", "packages: [{name: gcc, name: other}]"} {
		if _, err := buildList(b, []Manifest{{Path: "test.yaml", Data: []byte(data)}}, "refs/heads/main"); err == nil {
			t.Fatal("accepted", data)
		}
	}
}
