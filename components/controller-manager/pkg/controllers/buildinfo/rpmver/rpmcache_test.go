package rpmver

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	ebsv1 "ebs-api/ebs/v1"
)

type fetchMap map[string][]byte

func (f fetchMap) fetch(_ context.Context, url string) ([]byte, error) {
	if body, ok := f[url]; ok {
		return body, nil
	}
	return nil, fmt.Errorf("not found: %s", url)
}

func gz(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const repomdBody = `<?xml version="1.0" encoding="UTF-8"?>
<repomd xmlns="http://linux.duke.edu/metadata/repo">
  <data type="primary">
    <checksum type="sha256">abc</checksum>
    <location href="repodata/abc-primary.xml.gz"/>
  </data>
  <data type="filelists">
    <location href="repodata/def-filelists.xml.gz"/>
  </data>
</repomd>`

const primaryBody = `<?xml version="1.0" encoding="UTF-8"?>
<metadata xmlns="http://linux.duke.edu/metadata/common" xmlns:rpm="http://linux.duke.edu/metadata/rpm" packages="3">
  <package type="rpm">
    <name>glibc</name>
    <arch>x86_64</arch>
    <version epoch="0" ver="2.36" rel="4"/>
    <format>
      <rpm:sourcerpm>glibc-2.36-4.src.rpm</rpm:sourcerpm>
      <rpm:provides>
        <rpm:entry name="glibc" flags="EQ" epoch="0" ver="2.36" rel="4"/>
        <rpm:entry name="libc.so.6()(64bit)"/>
      </rpm:provides>
      <rpm:requires>
        <rpm:entry name="basesystem" flags="GE" epoch="0" ver="11" rel="1"/>
      </rpm:requires>
    </format>
  </package>
  <package type="rpm">
    <name>glibc-common</name>
    <arch>x86_64</arch>
    <version epoch="0" ver="2.36" rel="4"/>
    <format>
      <rpm:sourcerpm>glibc-2.36-4.src.rpm</rpm:sourcerpm>
      <rpm:provides>
        <rpm:entry name="glibc-common" flags="EQ" epoch="0" ver="2.36" rel="4"/>
      </rpm:provides>
    </format>
  </package>
  <package type="rpm">
    <name>setup</name>
    <arch>noarch</arch>
    <version epoch="0" ver="2.13" rel="7"/>
    <format>
      <rpm:sourcerpm>setup-2.13-7.src.rpm</rpm:sourcerpm>
      <rpm:provides>
        <rpm:entry name="setup" flags="EQ" epoch="0" ver="2.13" rel="7"/>
      </rpm:provides>
    </format>
  </package>
</metadata>`

func testRepo(t *testing.T) (*RpmMetaSource, fetchMap) {
	t.Helper()
	f := fetchMap{
		"http://repo/repodata/repomd.xml":         []byte(repomdBody),
		"http://repo/repodata/abc-primary.xml.gz": gz(t, primaryBody),
	}
	src, err := ParseRepoSource(context.Background(), f.fetch, "http://repo/", "x86_64")
	if err != nil {
		t.Fatalf("ParseRepoSource: %v", err)
	}
	return src, f
}

func TestParseRepoSourceBasic(t *testing.T) {
	src, _ := testRepo(t)
	if len(src.RpmByName) != 3 {
		t.Fatalf("RpmByName len = %d, want 3", len(src.RpmByName))
	}
	glibc := src.RpmByName["glibc"]
	if glibc.Version != "0:2.36-4" {
		t.Fatalf("glibc.Version = %q, want 0:2.36-4", glibc.Version)
	}
	if glibc.SpecName != "glibc" {
		t.Fatalf("glibc.SpecName = %q, want glibc", glibc.SpecName)
	}
	if got := glibc.Provides["glibc"]; got != "0:2.36-4" {
		t.Fatalf("provides glibc version = %q", got)
	}
	// Unversioned provide keeps an empty version string.
	if got, ok := glibc.Provides["libc.so.6()(64bit)"]; !ok || got != "" {
		t.Fatalf("unversioned provide = %q,%v", got, ok)
	}
	wantVC := ebsv1.VersionConst{GE: "0:11-1"}
	if got := glibc.Requires["basesystem"]; !reflect.DeepEqual(got, wantVC) {
		t.Fatalf("requires basesystem = %+v, want %+v", got, wantVC)
	}
	// ProvidesInfo secondary index.
	entry := src.ProvidesInfo["glibc"]["glibc"]
	if entry.SpecName != "glibc" || entry.Version != "0:2.36-4" {
		t.Fatalf("ProvidesInfo[glibc][glibc] = %+v", entry)
	}
	// Common XML namespace prefixes must not break local-name matching.
	if src.URL != "http://repo" {
		t.Fatalf("URL = %q, want trailing slash trimmed", src.URL)
	}
}

func TestParseRepoSourceErrors(t *testing.T) {
	// Download failure.
	_, err := ParseRepoSource(context.Background(), fetchMap{}.fetch, "http://repo", "x86_64")
	var se *SourceError
	if !errors.As(err, &se) || se.Kind != FailureDownload {
		t.Fatalf("err = %v, want SourceError Download", err)
	}
	// repomd without a primary record.
	f := fetchMap{"http://repo/repodata/repomd.xml": []byte(`<repomd><data type="filelists"/></repomd>`)}
	_, err = ParseRepoSource(context.Background(), f.fetch, "http://repo", "x86_64")
	if !errors.As(err, &se) || se.Kind != FailureParse {
		t.Fatalf("err = %v, want SourceError Parse", err)
	}
	// Broken gzip payload.
	f["http://repo/repodata/abc-primary.xml.gz"] = []byte("not gzip")
	f["http://repo/repodata/repomd.xml"] = []byte(repomdBody)
	_, err = ParseRepoSource(context.Background(), f.fetch, "http://repo", "x86_64")
	if !errors.As(err, &se) || se.Kind != FailureParse {
		t.Fatalf("err = %v, want SourceError Parse (gzip)", err)
	}
}

func TestParseRepoSourceArchSelection(t *testing.T) {
	body := `<?xml version="1.0"?>
<metadata xmlns="http://linux.duke.edu/metadata/common" xmlns:rpm="http://linux.duke.edu/metadata/rpm">
  <package type="rpm"><name>multi</name><arch>noarch</arch>
    <version epoch="0" ver="1.0" rel="1"/>
    <format><rpm:sourcerpm>multi-1.0-1.src.rpm</rpm:sourcerpm></format>
  </package>
  <package type="rpm"><name>multi</name><arch>x86_64</arch>
    <version epoch="0" ver="1.1" rel="1"/>
    <format><rpm:sourcerpm>multi-1.1-1.src.rpm</rpm:sourcerpm></format>
  </package>
  <package type="rpm"><name>foreign</name><arch>aarch64</arch>
    <version epoch="0" ver="9.9" rel="1"/>
    <format><rpm:sourcerpm>foreign-9.9-1.src.rpm</rpm:sourcerpm></format>
  </package>
  <package type="rpm"><name>duo</name><arch>x86_64</arch>
    <version epoch="0" ver="1.0" rel="1"/>
    <format><rpm:sourcerpm>duo-1.0-1.src.rpm</rpm:sourcerpm></format>
  </package>
  <package type="rpm"><name>duo</name><arch>x86_64</arch>
    <version epoch="0" ver="2.0" rel="1"/>
    <format><rpm:sourcerpm>duo-2.0-1.src.rpm</rpm:sourcerpm></format>
  </package>
</metadata>`
	f := fetchMap{
		"http://repo/repodata/repomd.xml":         []byte(repomdBody),
		"http://repo/repodata/abc-primary.xml.gz": gz(t, body),
	}
	src, err := ParseRepoSource(context.Background(), f.fetch, "http://repo", "x86_64")
	if err != nil {
		t.Fatalf("ParseRepoSource: %v", err)
	}
	// Target arch beats noarch on a name conflict.
	if got := src.RpmByName["multi"].Version; got != "0:1.1-1" {
		t.Fatalf("multi version = %q, want target arch 0:1.1-1", got)
	}
	// Foreign-arch-only packages are not indexed.
	if _, ok := src.RpmByName["foreign"]; ok {
		t.Fatal("aarch64-only package must not be indexed for x86_64")
	}
	// Same name+arch: highest version wins.
	if got := src.RpmByName["duo"].Version; got != "0:2.0-1" {
		t.Fatalf("duo version = %q, want highest 0:2.0-1", got)
	}
}

func TestEnsureRepoLayer(t *testing.T) {
	_, f := testRepo(t)
	calls := 0
	counting := func(ctx context.Context, url string) ([]byte, error) {
		calls++
		return f.fetch(ctx, url)
	}
	s := &RpmMetaSources{}
	// Empty contentURL: normal empty state, no fetch, no error.
	if err := s.EnsureRepoLayer(context.Background(), counting, "", "x86_64"); err != nil {
		t.Fatalf("empty contentURL err = %v", err)
	}
	if s.RepoLayer != nil || calls != 0 {
		t.Fatalf("empty contentURL must not fetch (calls=%d)", calls)
	}
	if err := s.EnsureRepoLayer(context.Background(), counting, "http://repo", "x86_64"); err != nil {
		t.Fatalf("first fill err = %v", err)
	}
	first := calls
	// Unchanged URL: reuse, no refetch.
	if err := s.EnsureRepoLayer(context.Background(), counting, "http://repo", "x86_64"); err != nil {
		t.Fatalf("reuse err = %v", err)
	}
	if calls != first {
		t.Fatalf("unchanged contentURL refetched (calls %d -> %d)", first, calls)
	}
	// Changed URL: refresh.
	f["http://repo2/repodata/repomd.xml"] = f["http://repo/repodata/repomd.xml"]
	f["http://repo2/repodata/abc-primary.xml.gz"] = f["http://repo/repodata/abc-primary.xml.gz"]
	if err := s.EnsureRepoLayer(context.Background(), counting, "http://repo2", "x86_64"); err != nil {
		t.Fatalf("refresh err = %v", err)
	}
	if s.RepoLayer.URL != "http://repo2" || calls == first {
		t.Fatal("changed contentURL must refresh the layer")
	}
}

func TestEnsureBootstrapLayersOnce(t *testing.T) {
	_, f := testRepo(t)
	calls := 0
	counting := func(ctx context.Context, url string) ([]byte, error) {
		calls++
		return f.fetch(ctx, url)
	}
	s := &RpmMetaSources{}
	urls := []string{"http://repo", "http://repo2"}
	f["http://repo2/repodata/repomd.xml"] = f["http://repo/repodata/repomd.xml"]
	f["http://repo2/repodata/abc-primary.xml.gz"] = f["http://repo/repodata/abc-primary.xml.gz"]
	if err := s.EnsureBootstrapLayers(context.Background(), counting, urls, "x86_64"); err != nil {
		t.Fatalf("fill err = %v", err)
	}
	if len(s.BootstrapLayer) != 2 || s.BootstrapLayer[0].URL != "http://repo" || s.BootstrapLayer[1].URL != "http://repo2" {
		t.Fatalf("bootstrap layers not in declaration order: %+v", s.BootstrapLayer)
	}
	first := calls
	if err := s.EnsureBootstrapLayers(context.Background(), counting, urls, "x86_64"); err != nil {
		t.Fatalf("second fill err = %v", err)
	}
	if calls != first {
		t.Fatal("bootstrap layers must be parsed once and reused")
	}
}

func TestSpecNameFromSourceRpm(t *testing.T) {
	cases := []struct{ in, rpmName, want string }{
		{"glibc-2.36-4.src.rpm", "glibc", "glibc"},
		{"python3-requests-2.28-1.src.rpm", "python3-requests", "python3-requests"},
		{"", "foo", "foo"},
		{"garbage", "foo", "foo"},
		{"noversion.src.rpm", "foo", "foo"},
	}
	for _, c := range cases {
		if got := specNameFromSourceRpm(c.in, c.rpmName); got != c.want {
			t.Fatalf("specNameFromSourceRpm(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func provideSource(entries map[string]map[string]ProvideEntry) *RpmMetaSource {
	return &RpmMetaSource{ProvidesInfo: entries, RpmByName: map[string]RpmMeta{}}
}

func TestGetProvideInfoChain(t *testing.T) {
	src := provideSource(map[string]map[string]ProvideEntry{
		"libfoo": {
			"foo":      {Version: "0:1.0-1", SpecName: "foo"},
			"foo-ng":   {Version: "0:2.0-1", SpecName: "foo-ng"},
			"foo@epel": {Version: "0:3.0-1", SpecName: "foo-epel"},
		},
		"single": {"only": {Version: "0:1.0-1", SpecName: "only"}},
		"dirty":  {"a": {Version: "", SpecName: "a"}, "b": {Version: "0:1.0-1", SpecName: "b"}},
	})
	// Miss.
	if got := GetProvideInfo("absent", src, nil, ebsv1.VersionConst{}); got.Provider.SpecName != "" {
		t.Fatalf("absent = %+v, want miss", got)
	}
	// Single candidate direct pick.
	if got := GetProvideInfo("single", src, nil, ebsv1.VersionConst{}); got.Provider.SpecName != "only" || got.Reason != SelectionSingle {
		t.Fatalf("single = %+v", got)
	}
	// Version-constraint filter leaves nobody.
	if got := GetProvideInfo("libfoo", src, nil, ebsv1.VersionConst{GT: "0:9.9-1"}); got.Provider.SpecName != "" {
		t.Fatalf("over-constrained = %+v, want miss", got)
	}
	// Version-constraint filter selects the only survivor.
	if got := GetProvideInfo("libfoo", src, nil, ebsv1.VersionConst{LT: "0:1.5-1"}); got.Provider.SpecName != "foo" || got.Reason != SelectionSingle {
		t.Fatalf("filtered single = %+v, want foo", got)
	}
	// prefer hits the @ base-name normalized candidate in order.
	got := GetProvideInfo("libfoo", src, []string{"foo-ng", "foo"}, ebsv1.VersionConst{})
	if got.Provider.SpecName != "foo-ng" || got.Reason != SelectionPrefer || got.RPMName != "foo-ng" {
		t.Fatalf("prefer first = %+v, want foo-ng", got)
	}
	got = GetProvideInfo("libfoo", src, []string{"foo"}, ebsv1.VersionConst{})
	if got.Provider.SpecName != "foo-epel" && got.Provider.SpecName != "foo" {
		t.Fatalf("prefer foo base = %+v", got)
	}
	// Without prefer: highest version wins.
	if got := GetProvideInfo("libfoo", src, nil, ebsv1.VersionConst{}); got.Provider.SpecName != "foo-epel" || got.Reason != SelectionHighestVersion {
		t.Fatalf("highest = %+v, want foo-epel (0:3.0-1)", got)
	}
	// Empty candidate version on the highest-version step: whole provide miss.
	if got := GetProvideInfo("dirty", src, nil, ebsv1.VersionConst{}); got.Provider.SpecName != "" {
		t.Fatalf("dirty = %+v, want miss", got)
	}
}

func TestFindProviderLayered(t *testing.T) {
	s := &RpmMetaSources{
		RepoLayer: provideSource(map[string]map[string]ProvideEntry{
			"cap": {"repo-rpm": {Version: "0:1.0-1", SpecName: "repo-spec"}},
		}),
		BootstrapLayer: []*RpmMetaSource{provideSource(map[string]map[string]ProvideEntry{
			"cap":      {"boot-rpm": {Version: "0:9.9-1", SpecName: "boot-spec"}},
			"bootonly": {"b": {Version: "0:1.0-1", SpecName: "b"}},
		})},
	}
	// Repo layer short-circuits the (higher-versioned) bootstrap candidate.
	selection, ok := s.FindProvider("cap", ebsv1.VersionConst{}, nil)
	if !ok || selection.Provider.SpecName != "repo-spec" {
		t.Fatalf("FindProvider cap = %+v,%v, want repo-spec", selection, ok)
	}
	// Miss in repo layer falls through to bootstrap.
	selection, ok = s.FindProvider("bootonly", ebsv1.VersionConst{}, nil)
	if !ok || selection.Provider.SpecName != "b" {
		t.Fatalf("FindProvider bootonly = %+v,%v", selection, ok)
	}
	if _, ok = s.FindProvider("nowhere", ebsv1.VersionConst{}, nil); ok {
		t.Fatal("nowhere must miss")
	}
}

func TestFindProviderSelectionReason(t *testing.T) {
	s := &RpmMetaSources{
		RepoLayer: provideSource(map[string]map[string]ProvideEntry{
			"cap": {
				"rpm-a@spec-a": {Version: "0:1.0-1", SpecName: "spec-a"},
				"rpm-b@spec-b": {Version: "0:2.0-1", SpecName: "spec-b"},
			},
		}),
		BootstrapLayer: []*RpmMetaSource{provideSource(map[string]map[string]ProvideEntry{
			"cap": {"rpm-boot": {Version: "0:9.0-1", SpecName: "boot"}},
		})},
	}
	selection, ok := s.FindProvider("cap", ebsv1.VersionConst{}, []string{"rpm-a", "rpm-b"})
	if !ok || selection.Provider.SpecName != "spec-a" || selection.Reason != SelectionPrefer || selection.RPMName != "rpm-a" {
		t.Fatalf("prefer choice = %+v, %v", selection, ok)
	}
	selection, ok = s.FindProvider("cap", ebsv1.VersionConst{GT: "0:1.0-1"}, []string{"rpm-a", "rpm-b"})
	if !ok || selection.Provider.SpecName != "spec-b" || selection.Reason != SelectionSingle {
		t.Fatalf("single filtered choice = %+v, %v", selection, ok)
	}
	selection, ok = s.FindProvider("cap", ebsv1.VersionConst{}, []string{"absent"})
	if !ok || selection.Provider.SpecName != "spec-b" || selection.Reason != SelectionHighestVersion {
		t.Fatalf("version fallback = %+v, %v", selection, ok)
	}
}

func TestAvailable(t *testing.T) {
	repo := provideSource(map[string]map[string]ProvideEntry{
		"cap": {"rpm-a": {Version: "0:1.0-1", SpecName: "spec-a"}},
	})
	repo.RpmByName["dirty"] = RpmMeta{Name: "dirty", Version: "", SpecName: "spec-dirty"}
	boot := provideSource(nil)
	boot.RpmByName["dirty"] = RpmMeta{Name: "dirty", Version: "0:2.0-1", SpecName: "spec-dirty"}
	s := &RpmMetaSources{RepoLayer: repo, BootstrapLayer: []*RpmMetaSource{boot}}

	if !s.Available("cap", ebsv1.VersionConst{LE: "0:2.0-1"}) {
		t.Fatal("provide hit with satisfied constraint must be available")
	}
	if s.Available("cap", ebsv1.VersionConst{GT: "0:2.0-1"}) {
		t.Fatal("unsatisfied constraint must be unavailable")
	}
	// Dirty repo-layer fallback entry (empty version) falls through to the
	// bootstrap layer's clean entry.
	if !s.Available("dirty", ebsv1.VersionConst{}) {
		t.Fatal("dirty repo entry must fall through to bootstrap layer")
	}
	if s.Available("nowhere", ebsv1.VersionConst{}) {
		t.Fatal("nowhere must be unavailable")
	}
}

func TestRepoRequires(t *testing.T) {
	s := &RpmMetaSources{}
	if got := s.RepoRequires("glibc"); got != nil {
		t.Fatalf("no repo layer: got %v, want nil", got)
	}
	src, _ := testRepo(t)
	s.RepoLayer = src
	got := s.RepoRequires("glibc")
	// glibc and glibc-common both derive specName glibc; requires merge.
	want := ebsv1.VersionConst{GE: "0:11-1"}
	if !reflect.DeepEqual(got["basesystem"], want) {
		t.Fatalf("RepoRequires glibc = %v", got)
	}
	if got := s.RepoRequires("absent"); got != nil {
		t.Fatalf("absent spec = %v, want nil", got)
	}
}
