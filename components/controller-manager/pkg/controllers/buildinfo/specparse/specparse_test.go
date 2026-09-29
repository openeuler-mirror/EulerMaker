package specparse

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func parseExpandedText(t *testing.T, spec string) *SpecDepend {
	t.Helper()
	depend, err := parseSpec(spec, "demo.spec", "demo-repo", nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return depend
}

func TestBasicSpec(t *testing.T) {
	depend := parseExpandedText(t, `
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
	depend := parseExpandedText(t, "Name: a\nVersion: 1.0\nEpoch: 0\n")
	if depend.Version != "1.0" {
		t.Fatalf("Version = %q, want 1.0", depend.Version)
	}
}

func TestTagCaseInsensitiveAndLastWins(t *testing.T) {
	depend := parseExpandedText(t, "NAME: first\nname: second\nVERSION : 9.9\n")
	if depend.SpecName != "second" {
		t.Fatalf("SpecName = %q, want second (last wins, case-insensitive)", depend.SpecName)
	}
	if depend.Version != "9.9" {
		t.Fatalf("Version = %q", depend.Version)
	}
}

func TestSingleValueTakesFirstToken(t *testing.T) {
	depend := parseExpandedText(t, "Name: demo extra words\nVersion: 1.0 # trailing\n")
	if depend.SpecName != "demo" || depend.Version != "1.0" {
		t.Fatalf("SpecName/Version = %q/%q", depend.SpecName, depend.Version)
	}
}

func TestMacroExpansion(t *testing.T) {
	depend := parseExpandedText(t, `
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

func TestExpandedTextUsesBuildPayloadMacros(t *testing.T) {
	depend, err := parseSpec("Name: %{_vendor}openEuler-indexhtml\nVersion: %{version}\n", "indexhtml.spec", "indexhtml", []string{
		"%_vendor test-",
		"%define version 1.0",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if depend.SpecName != "test-openEuler-indexhtml" || depend.Version != "1.0" {
		t.Fatalf("payload macros not expanded: %+v", depend)
	}
}

func TestSpecMacroOverridesBuildPayloadMacro(t *testing.T) {
	depend, err := parseSpec("%global _vendor spec-\nName: %{_vendor}openEuler-indexhtml\nVersion: 1.0\n", "indexhtml.spec", "indexhtml", []string{"%_vendor payload-"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if depend.SpecName != "spec-openEuler-indexhtml" {
		t.Fatalf("spec macro did not override payload: %q", depend.SpecName)
	}
}

func TestConditionalMacros(t *testing.T) {
	depend := parseExpandedText(t, `
%define flag yes
Name: %{?flag:demo}%{!flag:other}
Version: 1.0
Release: %{!flag:neg}2
`)
	if depend.SpecName != "demo" {
		// %{?flag:demo} expands its body, %{!flag:other} is empty.
		t.Fatalf("SpecName = %q, want demo", depend.SpecName)
	}
	if depend.Release != "2" {
		t.Fatalf("Release = %q, want 2", depend.Release)
	}
}

func TestNestedVendorConditional(t *testing.T) {
	const spec = `%global vendor %{?_vendor:%{_vendor}}%{!?_vendor:openEuler}
Name: %{vendor}-indexhtml
Version: 7
`
	for _, tc := range []struct {
		name   string
		macros []string
		want   string
	}{
		{name: "vendor defined", macros: []string{"%_vendor openEuler"}, want: "openEuler-indexhtml"},
		{name: "vendor absent", want: "openEuler-indexhtml"},
		{name: "different vendor", macros: []string{"%_vendor Example"}, want: "Example-indexhtml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			depend, err := parseSpec(spec, "generic-indexhtml.spec", "openEuler-indexhtml", tc.macros)
			if err != nil {
				t.Fatal(err)
			}
			if depend.SpecName != tc.want {
				t.Fatalf("SpecName = %q, want %q", depend.SpecName, tc.want)
			}
		})
	}
}

func TestNegatedConditionalWithoutDefaultFails(t *testing.T) {
	_, err := parseSpec("Name: %{!undefined}\nVersion: 1.0\n", "x.spec", "repo", nil)
	if err == nil {
		t.Fatal("expected parseFailed for %{!undefined} without default")
	}
}

func TestRequirementTwoPhase(t *testing.T) {
	depend := parseExpandedText(t, `
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
	depend := parseExpandedText(t, `
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
	_, err := parseSpec("Name: demo\nVersion: 1.0\nRequires: >= 1.0\n", "x.spec", "repo", nil)
	if err == nil {
		t.Fatal("expected parseFailed for leading operator token")
	}
}

func TestSubpackageTagsNotMerged(t *testing.T) {
	depend := parseExpandedText(t, `
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
	depend := parseExpandedText(t, `
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
	depend := parseExpandedText(t, "Name: a\nVersion: 1.0\nExclusiveArch: x86 aarch64\nExcludeArch: aarch64\n")
	want := []string{"x86", "x86_64"}
	if !reflect.DeepEqual(depend.ExclusiveArch, want) {
		t.Fatalf("ExclusiveArch = %v, want %v (x86 normalized, aarch64 excluded)", depend.ExclusiveArch, want)
	}
}

func TestExclusiveArchFullyExcludedFails(t *testing.T) {
	// 16.3 invariant "归一后列表恒非空": a whitelist fully excluded by
	// excludeArch means no buildable architecture, and an empty list would
	// invert archSupported's empty=allow-all semantics — parseFailed instead.
	_, err := parseSpec("Name: demo\nVersion: 1.0\nExclusiveArch: x86_64\nExcludeArch: x86\n", "x.spec", "repo", nil)
	if err == nil || !strings.Contains(err.Error(), "exclusiveArch") {
		t.Fatalf("error = %v, want a parseFailed mentioning exclusiveArch", err)
	}
}

func TestMacroLineFallbackDiscarded(t *testing.T) {
	depend := parseExpandedText(t, `
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
	if _, err := parseSpec("Version: 1.0\n", "x.spec", "repo", nil); err == nil {
		t.Fatal("missing Name must fail")
	}
	if _, err := parseSpec("Name: demo\n", "x.spec", "repo", nil); err == nil {
		t.Fatal("missing Version must fail")
	}
}

// stubRpmspec installs a fake rpmspec executable that records its argv.
func stubRpmspec(t *testing.T, stdout string) (argvFile *string) {
	t.Helper()
	dir := t.TempDir()
	argvPath := filepath.Join(dir, "argv")
	script := filepath.Join(dir, "rpmspec-stub")
	content := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvPath + "\nprintf '%s' " + shellQuote(stdout) + "\n"
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

func TestRpmspecOutputUsed(t *testing.T) {
	// The stub echoes back an expanded spec with macros already resolved.
	argvFile := stubRpmspec(t, "Name: expanded\nVersion: 9.9\n")
	depend, err := Parse("Name: %{would_not_expand}\nVersion: 0.1\n", "x.spec", "repo", "aarch64", []string{"%define foo bar"})
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

func TestRpmspecEmptyOutputFails(t *testing.T) {
	stubRpmspec(t, "")
	if _, err := Parse("Name: raw\nVersion: 1.0\n", "x.spec", "repo", "x86_64", nil); err == nil || !strings.Contains(err.Error(), "empty output") {
		t.Fatalf("expected empty output error, got %v", err)
	}
}

func TestRpmspecMissingFails(t *testing.T) {
	old := rpmspecCommand
	rpmspecCommand = filepath.Join(t.TempDir(), "missing-rpmspec")
	t.Cleanup(func() { rpmspecCommand = old })
	if _, err := Parse("Name: raw\nVersion: 1.0\n", "x.spec", "repo", "x86_64", nil); err == nil {
		t.Fatal("missing rpmspec must not fall back to raw spec")
	}
}

func TestRpmspecExitFailureDoesNotUseStdout(t *testing.T) {
	old := rpmspecCommand
	script := filepath.Join(t.TempDir(), "rpmspec-stub")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'Name: expanded\\nVersion: 9.9\\n'\necho failed >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rpmspecCommand = script
	t.Cleanup(func() { rpmspecCommand = old })
	if _, err := Parse("Name: raw\nVersion: 1.0\n", "x.spec", "repo", "x86_64", nil); err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("expected rpmspec exit error, got %v", err)
	}
}

func TestParseFetchesMissingSourceAndRetries(t *testing.T) {
	old := rpmspecCommand
	script := filepath.Join(t.TempDir(), "rpmspec-stub")
	stub := `#!/bin/sh
for arg in "$@"; do
  case "$arg" in "_sourcedir "*) source_dir=${arg#_sourcedir };; esac
done
if [ ! -f "$source_dir/LanguageList" ]; then
  echo "cannot open file '$source_dir/LanguageList' (No such file or directory)" >&2
  exit 1
fi
printf 'Name: demo\nVersion: 1\n'
`
	if err := os.WriteFile(script, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	rpmspecCommand = script
	t.Cleanup(func() { rpmspecCommand = old })
	calls := 0
	result, err := ParseWithSources("Name: demo\nVersion: 1\n", "demo.spec", "demo", "x86_64", nil, func(name string) (string, error) {
		calls++
		if name != "LanguageList" {
			t.Fatalf("unexpected source %q", name)
		}
		return "en_US", nil
	})
	if err != nil || result.SpecName != "demo" || calls != 1 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, calls)
	}
}

func TestParseDoesNotFetchOutsideIsolatedSources(t *testing.T) {
	old := rpmspecCommand
	script := filepath.Join(t.TempDir(), "rpmspec-stub")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'cannot open file /etc/passwd (No such file or directory)' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rpmspecCommand = script
	t.Cleanup(func() { rpmspecCommand = old })
	_, err := ParseWithSources("Name: demo\nVersion: 1\n", "demo.spec", "demo", "x86_64", nil, func(name string) (string, error) {
		t.Fatalf("unexpected fetch %q", name)
		return "", nil
	})
	if err == nil {
		t.Fatal("expected rpmspec failure")
	}
}

func TestParseSourceFetchErrorIsTyped(t *testing.T) {
	old := rpmspecCommand
	script := filepath.Join(t.TempDir(), "rpmspec-stub")
	stub := `#!/bin/sh
for arg in "$@"; do
  case "$arg" in "_sourcedir "*) source_dir=${arg#_sourcedir };; esac
done
echo "Unable to open $source_dir/grub.macros: No such file or directory" >&2
exit 1
`
	if err := os.WriteFile(script, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	rpmspecCommand = script
	t.Cleanup(func() { rpmspecCommand = old })
	want := fmt.Errorf("git unavailable")
	_, err := ParseWithSources("Name: demo\nVersion: 1\n", "demo.spec", "demo", "x86_64", nil, func(name string) (string, error) {
		if name != "grub.macros" {
			t.Fatalf("unexpected source %q", name)
		}
		return "", want
	})
	var fetchErr *SourceFetchError
	if !errors.As(err, &fetchErr) || !errors.Is(err, want) {
		t.Fatalf("expected wrapped source fetch error, got %v", err)
	}
}

func TestParseFailedDoesNotBlockSiblings(t *testing.T) {
	specs := map[string]string{
		"good.spec": "Name: good\nVersion: 1.0\n",
		"bad.spec":  "Version: 1.0\n",
	}
	var succeeded []string
	for name, text := range specs {
		if depend, err := parseSpec(text, name, "repo", nil); err == nil {
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
		if _, err := parseSpec(tc.spec, "x.spec", "repo", nil); err == nil {
			t.Fatalf("%s: expected bounded-expansion failure, got success", tc.name)
		}
	}
}
