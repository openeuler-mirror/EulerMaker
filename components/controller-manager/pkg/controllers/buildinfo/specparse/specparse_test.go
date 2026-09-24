package specparse

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func parseFallback(t *testing.T, spec string) *SpecDepend {
	t.Helper()
	depend, err := Parse(spec, "demo.spec", "demo-repo", "x86_64", nil, EngineText)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return depend
}

func TestBasicSpec(t *testing.T) {
	depend := parseFallback(t, `
# a comment
Name: demo
Version: 1.2
Release: 3
Epoch: 2
Requires: glibc >= 2.17, make
BuildRequires: gcc
BuildRequires: -junk
Provides: demo-lib = 1.0
`)
	if depend.SpecName != "demo" {
		t.Fatalf("SpecName = %q", depend.SpecName)
	}
	if depend.SpecFileName != "demo.spec" || depend.RepoName != "demo-repo" {
		t.Fatalf("attachment fields: %+v", depend)
	}
	if depend.Version != "2:1.2-3" {
		t.Fatalf("Version = %q, want 2:1.2-3", depend.Version)
	}
	if depend.Release != "3" || depend.Epoch != "2" {
		t.Fatalf("Release/Epoch = %q/%q", depend.Release, depend.Epoch)
	}
	if got := depend.Requires["glibc"]; got.GE != "2.17" {
		t.Fatalf("Requires[glibc] = %+v", got)
	}
	if _, ok := depend.Requires["make"]; !ok {
		t.Fatal("Requires[make] missing")
	}
	if _, ok := depend.BuildRequires["gcc"]; !ok {
		t.Fatal("BuildRequires[gcc] missing")
	}
	if _, ok := depend.BuildRemoves["junk"]; !ok {
		t.Fatal("BuildRemoves[junk] missing")
	}
	if !reflect.DeepEqual(depend.Provides, []string{"demo-lib"}) {
		t.Fatalf("Provides = %v", depend.Provides)
	}
	// No ExclusiveArch declared: platform default set.
	if !reflect.DeepEqual(depend.ExclusiveArch, []string{"x86_64", "aarch64", "loongarch64", "riscv64", "ppc64le", "sw_64"}) {
		t.Fatalf("ExclusiveArch = %v", depend.ExclusiveArch)
	}
}

func TestVersionJoinZeroEpochAndNoRelease(t *testing.T) {
	depend := parseFallback(t, "Name: a\nVersion: 1.0\nEpoch: 0\n")
	if depend.Version != "1.0" {
		t.Fatalf("Version = %q, want 1.0", depend.Version)
	}
}

func TestTagCaseInsensitiveAndLastWins(t *testing.T) {
	depend := parseFallback(t, "NAME: first\nname: second\nVERSION : 9.9\n")
	if depend.SpecName != "second" {
		t.Fatalf("SpecName = %q, want second (last wins, case-insensitive)", depend.SpecName)
	}
	if depend.Version != "9.9" {
		t.Fatalf("Version = %q", depend.Version)
	}
}

func TestSingleValueTakesFirstToken(t *testing.T) {
	depend := parseFallback(t, "Name: demo extra words\nVersion: 1.0 # trailing\n")
	if depend.SpecName != "demo" || depend.Version != "1.0" {
		t.Fatalf("SpecName/Version = %q/%q", depend.SpecName, depend.Version)
	}
}

func TestMacroExpansion(t *testing.T) {
	depend := parseFallback(t, `
%define upstream 1.2
%define releaseBase %{upstream}
Name: demo
Version: %{releaseBase}
Release: %{?with_snapshot}1
Requires: foo >= %{upstream}
Requires: literal %{undefined} bar
`)
	if depend.Version != "1.2-1" {
		t.Fatalf("Version = %q, want 1.2-1 (recursive expansion, conditional without default)", depend.Version)
	}
	if got := depend.Requires["foo"]; got.GE != "1.2" {
		t.Fatalf("Requires[foo] = %+v", got)
	}
	// Undefined plain macro stays literal, then the tokenizer splits on
	// whitespace into separate names.
	for _, key := range []string{"literal", "%{undefined}", "bar"} {
		if _, ok := depend.Requires[key]; !ok {
			t.Fatalf("literal macro key %q missing: %v", key, depend.Requires)
		}
	}
}

func TestConditionalMacros(t *testing.T) {
	depend := parseFallback(t, `
%define flag yes
Name: %{?flag:demo}%{!flag:other}
Version: 1.0
Release: %{!flag:neg}2
`)
	if depend.SpecName != "yes" {
		// %{?flag:demo} takes flag's value (yes), %{!flag:other} is empty.
		t.Fatalf("SpecName = %q, want yes", depend.SpecName)
	}
	if depend.Release != "2" {
		t.Fatalf("Release = %q, want 2", depend.Release)
	}
}

func TestNegatedConditionalWithoutDefaultFails(t *testing.T) {
	_, err := Parse("Name: %{!undefined}\nVersion: 1.0\n", "x.spec", "repo", "x86_64", nil, EngineText)
	if err == nil {
		t.Fatal("expected parseFailed for %{!undefined} without default")
	}
}

func TestRequirementTwoPhase(t *testing.T) {
	depend := parseFallback(t, `
Name: demo
Version: 1.0
Requires: binutils => 2.31
Requires: pinned==1.5
Requires: spaced == 2.0
Requires: (grouped >= 3.0)
Requires: pkg with other
Requires: multi >= 1.0
Requires: multi >= 2.0
`)
	if got := depend.Requires["binutils"]; got.GE != "2.31" {
		t.Fatalf("binutils bare-op backfill: %+v", got)
	}
	if got := depend.Requires["pinned"]; got.EQ != "1.5" || got.GE != "" || got.GT != "" {
		t.Fatalf("pinned == inline whole-key replace: %+v", got)
	}
	if got := depend.Requires["spaced"]; got.EQ != "2.0" {
		t.Fatalf("spaced == inline replace must trim blanks: %+v", got)
	}
	if _, ok := depend.Requires["spaced "]; ok {
		t.Fatal("spaced == must not register a blank-padded phantom key")
	}
	if got := depend.Requires["grouped"]; got.GE != "3.0" {
		t.Fatalf("paren strip: %+v", got)
	}
	if _, ok := depend.Requires["with"]; ok {
		t.Fatal("with must be skipped")
	}
	if got := depend.Requires["multi"]; got.GE != "2.0" {
		t.Fatalf("same name+op last wins: %+v", got)
	}
}

func TestDanglingOperatorDoesNotLeakToNextLine(t *testing.T) {
	// A line-trailing bare operator (macro expansion can legitimately empty
	// the version, e.g. "glibc => %{?ver}") must be discarded at end of line:
	// the dependency keeps its bare constraint and the next line's first
	// token is never consumed as its version.
	depend := parseFallback(t, `
Name: demo
Version: 1.0
Requires: glibc =>
Requires: openssl
`)
	if got := depend.Requires["glibc"]; got.GE != "" {
		t.Fatalf("dangling operator must be discarded, glibc: %+v", got)
	}
	if _, ok := depend.Requires["openssl"]; !ok {
		t.Fatal("next line's first token must not be consumed as a version")
	}
	if got := depend.Requires["openssl"]; got.GE != "" {
		t.Fatalf("openssl must stay bare: %+v", got)
	}
}

func TestOperatorWithoutPrecedingTokenFails(t *testing.T) {
	_, err := Parse("Name: demo\nVersion: 1.0\nRequires: >= 1.0\n", "x.spec", "repo", "x86_64", nil, EngineText)
	if err == nil {
		t.Fatal("expected parseFailed for leading operator token")
	}
}

func TestSubpackageTagsNotMerged(t *testing.T) {
	depend := parseFallback(t, `
Name: demo
Version: 1.0
Requires: maindep
%package devel
Requires: subdep
BuildRequires: subbuild
Version: 9.9
Release: 9%{?dist}
Epoch: 5
Name: subname
%package -n custom-name
Provides: subprov
%changelog
Requires: afterlog
`)
	if _, ok := depend.Requires["maindep"]; !ok {
		t.Fatal("maindep missing")
	}
	for _, key := range []string{"subdep", "subprov"} {
		if _, ok := depend.Requires[key]; ok {
			t.Fatalf("subpackage tag %s must not merge into requires", key)
		}
	}
	if _, ok := depend.BuildRequires["subbuild"]; ok {
		t.Fatal("subpackage BuildRequires must not merge")
	}
	if depend.SpecName != "demo" {
		t.Fatalf("subpackage Name must not override: SpecName = %q", depend.SpecName)
	}
	if depend.Version != "1.0" {
		t.Fatalf("subpackage Version must not override: Version = %q", depend.Version)
	}
	if depend.Release != "" {
		t.Fatalf("subpackage Release must not merge: Release = %q", depend.Release)
	}
	if depend.Epoch != "" {
		t.Fatalf("subpackage Epoch must not merge: Epoch = %q", depend.Epoch)
	}
	if _, ok := depend.Requires["afterlog"]; !ok {
		t.Fatal("changelog must reset subpackage context")
	}
}

func TestDescriptionMultilineTermination(t *testing.T) {
	depend := parseFallback(t, `
Name: demo
%description
Some free text with Name: not-a-tag-inside
still body
Version: 1.0
`)
	// The free-text lines hit nothing and accumulate; Version must still parse.
	if depend.Version != "1.0" {
		t.Fatalf("Version = %q (multiline termination broken)", depend.Version)
	}
}

func TestExclusiveArchNormalize(t *testing.T) {
	depend := parseFallback(t, "Name: a\nVersion: 1.0\nExclusiveArch: x86 aarch64\nExcludeArch: aarch64\n")
	want := []string{"x86", "x86_64"}
	if !reflect.DeepEqual(depend.ExclusiveArch, want) {
		t.Fatalf("ExclusiveArch = %v, want %v (x86 normalized, aarch64 excluded)", depend.ExclusiveArch, want)
	}
}

func TestExclusiveArchFullyExcludedFails(t *testing.T) {
	// 16.3 invariant "归一后列表恒非空": a whitelist fully excluded by
	// excludeArch means no buildable architecture, and an empty list would
	// invert archSupported's empty=allow-all semantics — parseFailed instead.
	_, err := Parse("Name: demo\nVersion: 1.0\nExclusiveArch: x86_64\nExcludeArch: x86\n", "x.spec", "repo", "x86_64", nil, EngineText)
	if err == nil || !strings.Contains(err.Error(), "exclusiveArch") {
		t.Fatalf("error = %v, want a parseFailed mentioning exclusiveArch", err)
	}
}

func TestMacroLineFallbackDiscarded(t *testing.T) {
	depend := parseFallback(t, `
Name: demo
%prep
%setup -q
Version: 1.0
%Package devel
plain text line
Release: 2
`)
	if depend.Version != "1.0-2" || depend.Release != "2" {
		t.Fatalf("Version/Release = %q/%q", depend.Version, depend.Release)
	}
}

func TestMissingNameOrVersionFails(t *testing.T) {
	if _, err := Parse("Version: 1.0\n", "x.spec", "repo", "x86_64", nil, EngineText); err == nil {
		t.Fatal("missing Name must fail")
	}
	if _, err := Parse("Name: demo\n", "x.spec", "repo", "x86_64", nil, EngineText); err == nil {
		t.Fatal("missing Version must fail")
	}
}

// stubRpmspec installs a fake rpmspec executable that records its argv and
// replays the spec file content as stdout.
func stubRpmspec(t *testing.T, stdout string) (argvFile *string) {
	t.Helper()
	dir := t.TempDir()
	argvPath := filepath.Join(dir, "argv")
	script := filepath.Join(dir, "rpmspec-stub")
	content := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvPath + "\ncat \"$3\"\n"
	if stdout != "" {
		content = "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvPath + "\nprintf '%s' " + shellQuote(stdout) + "\n"
	}
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	old := rpmspecCommand
	rpmspecCommand = script
	t.Cleanup(func() { rpmspecCommand = old })
	return &argvPath
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func TestPrimaryPathUsedWhenStdoutNonEmpty(t *testing.T) {
	// The stub echoes back an expanded spec with macros already resolved.
	argvFile := stubRpmspec(t, "Name: expanded\nVersion: 9.9\n")
	depend, err := Parse("Name: %{would_not_expand}\nVersion: 0.1\n", "x.spec", "repo", "aarch64", []string{"%define foo bar"}, EngineRpmspec)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if depend.SpecName != "expanded" || depend.Version != "9.9" {
		t.Fatalf("primary path not used: %+v", depend)
	}
	argv, err := os.ReadFile(*argvFile)
	if err != nil {
		t.Fatal(err)
	}
	joined := string(argv)
	if !strings.Contains(joined, "--target=aarch64") || !strings.Contains(joined, "-P") || !strings.Contains(joined, "--load=") {
		t.Fatalf("rpmspec argv = %q", joined)
	}
}

func TestPrimaryPathEmptyStdoutFallsBack(t *testing.T) {
	stubRpmspec(t, "")
	depend, err := Parse("Name: raw\nVersion: 1.0\n", "x.spec", "repo", "x86_64", nil, EngineRpmspec)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if depend.SpecName != "raw" {
		t.Fatalf("fallback not used: %+v", depend)
	}
}

// TestTextEngineIgnoresRpmspec pins the security default: even with a working
// rpmspec stub on PATH, the text engine never spawns a subprocess.
func TestTextEngineIgnoresRpmspec(t *testing.T) {
	argvFile := stubRpmspec(t, "Name: expanded\nVersion: 9.9\n")
	depend, err := Parse("Name: raw\nVersion: 1.0\n", "x.spec", "repo", "x86_64", nil, EngineText)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if depend.SpecName != "raw" || depend.Version != "1.0" {
		t.Fatalf("text engine result polluted: %+v", depend)
	}
	if _, err := os.Stat(*argvFile); !os.IsNotExist(err) {
		t.Fatal("text engine must not run rpmspec (argv file written)")
	}
}

func TestParseFailedDoesNotBlockSiblings(t *testing.T) {
	specs := map[string]string{
		"good.spec": "Name: good\nVersion: 1.0\n",
		"bad.spec":  "Version: 1.0\n",
	}
	var succeeded []string
	for name, text := range specs {
		if depend, err := Parse(text, name, "repo", "x86_64", nil, EngineText); err == nil {
			succeeded = append(succeeded, depend.SpecName)
		}
	}
	if !reflect.DeepEqual(succeeded, []string{"good"}) {
		t.Fatalf("succeeded = %v", succeeded)
	}
}

// Mutually referencing macros never reach a fixed point and self-referencing
// macros grow without bound; both must fail the spec quickly (bounded by
// maxExpandRounds / maxExpandLength) instead of hanging or exhausting memory.
func TestMacroExpansionBounded(t *testing.T) {
	cases := []struct {
		name string
		spec string
	}{
		{"mutual reference", "%define x %{y}\n%define y %{x}\nName: %{x}\nVersion: 1.0\n"},
		{"self reference linear", "%define x %{x}a\nName: %{x}\nVersion: 1.0\n"},
		{"self reference exponential", "%define x %{x}%{x}\nName: %{x}\nVersion: 1.0\n"},
	}
	for _, tc := range cases {
		if _, err := Parse(tc.spec, "x.spec", "repo", "x86_64", nil, EngineText); err == nil {
			t.Fatalf("%s: expected bounded-expansion failure, got success", tc.name)
		}
	}
}
