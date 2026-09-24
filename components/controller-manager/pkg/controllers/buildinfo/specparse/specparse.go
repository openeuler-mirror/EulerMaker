// Package specparse parses *.spec file text into SpecDepend (design
// 16.3). The default engine (text) uses the in-package raw-text grammar
// with the macro expander only. The opt-in rpmspec engine first expands the
// spec through a local rpmspec subprocess: rpm macro expansion evaluates
// %(...) shell escapes and %{lua:...} blocks at parse time, so it executes
// untrusted repo content on the controller host — enable it only for
// trusted package sources (--spec-parse-engine, default text). When the
// subprocess yields empty stdout (including a missing executable), that
// engine falls back to the raw-text grammar. Both engines share the same
// line grammar and product model. A spec whose Name/Version cannot be
// resolved fails with an error — parseFailed, routed by E-23 — and never
// blocks the remaining specs of the same repository.
package specparse

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	ebsv1 "ebs-api/ebs/v1"
)

// SpecDepend is the in-memory parse product of one *.spec file (design
// 16.3). It is not part of the API schema: BuildInfoSpec no longer
// persists specDepends, so the type lives with its producer.
type SpecDepend struct {
	RepoName      string                       `json:"repoName"`
	SpecName      string                       `json:"specName"`
	SpecFileName  string                       `json:"specFileName,omitempty"`
	Version       string                       `json:"version"`
	Release       string                       `json:"release,omitempty"`
	Epoch         string                       `json:"epoch,omitempty"`
	ExclusiveArch []string                     `json:"exclusiveArch,omitempty"`
	Provides      []string                     `json:"provides,omitempty"`
	Requires      map[string]ebsv1.VersionConst `json:"requires,omitempty"`
	BuildRequires map[string]ebsv1.VersionConst `json:"buildRequires,omitempty"`
	BuildRemoves  map[string]ebsv1.VersionConst `json:"buildRemoves,omitempty"`
}

// defaultExclusiveArch is the platform default architecture set used when a
// spec declares no ExclusiveArch. Callers receive a copy; the package array
// is never mutated.
var defaultExclusiveArch = [...]string{"x86_64", "aarch64", "loongarch64", "riscv64", "ppc64le", "sw_64"}

// rpmspecCommand is the executable of the opt-in rpmspec engine. It is a
// package variable so tests can point it at a stub.
var rpmspecCommand = "rpmspec"

// rpmspecTimeout bounds one rpmspec subprocess run; a hung process is a
// primary-path failure and falls back to the raw-text path. It is a package
// variable so tests can shorten it.
var rpmspecTimeout = 30 * time.Second

// Engine selects the parse path (design 16.3, --spec-parse-engine).
type Engine string

const (
	// EngineText parses with the in-package raw-text grammar and macro
	// expander only — safe for untrusted repo content (default).
	EngineText Engine = "text"
	// EngineRpmspec first expands the spec through a local rpmspec
	// subprocess; rpm macro expansion evaluates %(...) shell escapes and
	// %{lua:...} at parse time, executing repo content on this host. Trusted
	// sources only.
	EngineRpmspec Engine = "rpmspec"
)

// Valid reports whether engine is a known value.
func (e Engine) Valid() bool {
	return e == EngineText || e == EngineRpmspec
}

// Parse parses one *.spec file with the selected engine. arch is the rpmspec
// --target value (unused by the text engine); macros are the raw
// macro-definition lines (buildPayload.macros) handed to rpmspec --load,
// written verbatim one per line. specFileName is the basename of the file
// inside the repository, repoName the owning package repository.
func Parse(specText, specFileName, repoName, arch string, macros []string, engine Engine) (*SpecDepend, error) {
	text := specText
	if engine == EngineRpmspec {
		if expanded, ok := tryRpmspec(specText, arch, macros); ok {
			text = expanded
		}
	}
	return parseSpec(text, specFileName, repoName)
}

// tryRpmspec runs the primary path and reports whether stdout was non-empty.
// Any startup failure — including a missing executable — is a primary-path
// failure and falls through to the raw-text path silently (16.3).
func tryRpmspec(specText, arch string, macros []string) (string, bool) {
	if _, err := exec.LookPath(rpmspecCommand); err != nil {
		return "", false
	}
	dir, err := os.MkdirTemp("", "specparse-")
	if err != nil {
		return "", false
	}
	defer os.RemoveAll(dir)
	specPath := filepath.Join(dir, "input.spec")
	if err := os.WriteFile(specPath, []byte(specText), 0o644); err != nil {
		return "", false
	}
	// The macro file is always created (empty when macros is unset); entries
	// are written verbatim with a trailing newline, no %define prefix, no
	// escaping, sorting or validation.
	var macroContent strings.Builder
	for _, line := range macros {
		macroContent.WriteString(line)
		macroContent.WriteString("\n")
	}
	macroPath := filepath.Join(dir, "macros")
	if err := os.WriteFile(macroPath, []byte(macroContent.String()), 0o644); err != nil {
		return "", false
	}
	// The subprocess is bounded by a timeout so a hung rpmspec cannot block
	// the reconcile worker forever; a timeout is a primary-path failure and
	// falls through to the raw-text path silently, like any startup failure.
	ctx, cancel := context.WithTimeout(context.Background(), rpmspecTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, rpmspecCommand, "--target="+arch, "-P", specPath, "--load="+macroPath)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", false
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return "", false
		}
		// exitCode is not consulted: stdout decides.
	}
	if strings.TrimSpace(string(out)) == "" {
		return "", false
	}
	return string(out), true
}

// specModel accumulates the spec-level attributes during line parsing.
type specModel struct {
	macros        map[string]string
	props         map[string]string
	requires      []string
	buildRequires []string
	provides      []string
	exclusiveArch []string
	excludeArch   []string
}

// Tag tables. Single-value tags keep the last occurrence; the five
// constraint/list tags are the only ones this controller consumes; the
// recognized-but-unconsumed names still terminate multiline accumulation.
var singleValueTags = map[string]bool{
	"name": true, "version": true, "release": true, "epoch": true,
	"url": true, "summary": true, "group": true, "license": true,
	"buildarch": true, "buildroot": true, "prefix": true, "packager": true,
	"vendor": true, "distribution": true, "disttag": true, "bugurl": true,
	"icon": true,
}

var consumedListTags = map[string]bool{
	"requires": true, "buildrequires": true, "provides": true,
	"exclusivearch": true, "excludearch": true,
}

var otherRecognizedTags = map[string]bool{
	"conflicts": true, "obsoletes": true, "prereq": true, "suggests": true,
	"recommends": true, "supplements": true, "enhances": true,
}

var (
	tagLinePattern   = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_]*)\s*:\s*(.*)$`)
	sourcePatchTag   = regexp.MustCompile(`^(source|patch)\d*$`)
	macroLinePattern = regexp.MustCompile(`^%[a-z_]`)
	macroRefPattern  = regexp.MustCompile(`%\{([^\s}]*)\}`)
	requirementSplit = regexp.MustCompile(`(.*?)\s+([<>]=?|=)\s+(\S+)`)
)

var operatorWords = map[string]string{
	">": "GT", "=": "EQ", "==": "EQ", "<": "LT",
	">=": "GE", "=>": "GE", "=<": "LE", "<=": "LE",
}

var mergeOperators = map[string]bool{
	">=": true, "!=": true, ">": true, "<": true,
	"<=": true, "==": true, "=": true,
}

func knownTag(name string) bool {
	return singleValueTags[name] || consumedListTags[name] || otherRecognizedTags[name] || sourcePatchTag.MatchString(name)
}

// parseSpec parses text (rpmspec-expanded or raw) into a SpecDepend.
func parseSpec(text, specFileName, repoName string) (*SpecDepend, error) {
	m := &specModel{macros: map[string]string{}, props: map[string]string{}}
	parseLines(text, m)
	return buildSpec(m, specFileName, repoName)
}

// lineKind classifies one input line against the fixed pattern table.
type lineKind int

const (
	lineNone lineKind = iota // hits nothing: discarded, or appended in multiline mode
	linePackage
	lineDefine
	lineGlobal
	lineDescription
	lineChangelog
	lineTagSingle
	lineTagList
	lineTagOther
	lineMacroFallback // any %-macro line: discarded, still terminates multiline
)

func classifyLine(line string) (kind lineKind, tag, value string) {
	for _, directive := range []struct {
		prefix string
		kind   lineKind
	}{
		{"%package", linePackage},
		{"%define", lineDefine},
		{"%global", lineGlobal},
		{"%description", lineDescription},
		{"%changelog", lineChangelog},
	} {
		if rest, ok := directiveLine(line, directive.prefix); ok {
			return directive.kind, "", rest
		}
	}
	if match := tagLinePattern.FindStringSubmatch(line); match != nil {
		name := strings.ToLower(match[1])
		switch {
		case singleValueTags[name]:
			return lineTagSingle, name, match[2]
		case consumedListTags[name]:
			return lineTagList, name, match[2]
		case otherRecognizedTags[name] || sourcePatchTag.MatchString(name):
			return lineTagOther, name, match[2]
		}
	}
	if macroLinePattern.MatchString(line) {
		return lineMacroFallback, "", ""
	}
	return lineNone, "", ""
}

// directiveLine reports whether line starts with the given %-directive as a
// whole word and returns the remainder of the line.
func directiveLine(line, directive string) (string, bool) {
	if !strings.HasPrefix(line, directive) {
		return "", false
	}
	rest := line[len(directive):]
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// parseLines walks the text applying the line grammar: first-hit pattern
// table, subpackage context, duplicate merging and multiline accumulation
// (description/changelog content is not consumed, but the mode must still be
// terminated correctly or following tags would be lost).
func parseLines(text string, m *specModel) {
	var multiline bool
	inSubpackage := false
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, "\r")
		kind, tag, value := classifyLine(line)
		if multiline {
			if kind == lineNone {
				continue
			}
			multiline = false
		}
		switch kind {
		case linePackage:
			inSubpackage = true
		case lineDefine, lineGlobal:
			name, macroValue := splitDefine(value)
			m.macros[name] = macroValue
			if !knownTag(name) {
				m.props[name] = macroValue
			}
		case lineDescription:
			multiline = true
		case lineChangelog:
			multiline = true
			inSubpackage = false
		case lineTagSingle:
			// Tags inside a %package section belong to the subpackage and are
			// never merged into the spec-level result.
			if inSubpackage {
				continue
			}
			m.props[tag] = firstToken(value)
		case lineTagList:
			// Tags inside a %package section belong to the subpackage and are
			// never merged into the spec-level result.
			if inSubpackage {
				continue
			}
			switch tag {
			case "requires":
				m.requires = append(m.requires, value)
			case "buildrequires":
				m.buildRequires = append(m.buildRequires, value)
			case "provides":
				m.provides = append(m.provides, value)
			case "exclusivearch":
				// Per-line accumulation: a later line's items go before the
				// existing list (order is not consumed).
				m.exclusiveArch = append(strings.Fields(value), m.exclusiveArch...)
			case "excludearch":
				m.excludeArch = append(strings.Fields(value), m.excludeArch...)
			}
		}
	}
}

// splitDefine splits a %define/%global body into macro name and raw value.
func splitDefine(body string) (name, value string) {
	body = strings.TrimSpace(body)
	for i := 0; i < len(body); i++ {
		if body[i] == ' ' || body[i] == '\t' {
			return body[:i], strings.TrimSpace(body[i:])
		}
	}
	return body, ""
}

// firstToken extracts the first non-whitespace run of a single-value tag.
func firstToken(value string) string {
	if fields := strings.Fields(value); len(fields) > 0 {
		return fields[0]
	}
	return ""
}

// buildSpec resolves macros and derives the SpecDepend from the accumulated
// model. Missing Name or Version is a parseFailed.
func buildSpec(m *specModel, specFileName, repoName string) (*SpecDepend, error) {
	specName, err := expandMacros(m.props["name"], m)
	if err != nil {
		return nil, err
	}
	if specName == "" {
		return nil, fmt.Errorf("spec Name is missing")
	}
	version, err := expandMacros(m.props["version"], m)
	if err != nil {
		return nil, err
	}
	if version == "" {
		return nil, fmt.Errorf("spec %s: Version is missing", specName)
	}
	release, err := expandMacros(m.props["release"], m)
	if err != nil {
		return nil, err
	}
	epoch, err := expandMacros(m.props["epoch"], m)
	if err != nil {
		return nil, err
	}
	requires, err := parseRequirements(m.requires, m)
	if err != nil {
		return nil, fmt.Errorf("spec %s: %w", specName, err)
	}
	buildParsed, err := parseRequirements(m.buildRequires, m)
	if err != nil {
		return nil, fmt.Errorf("spec %s: %w", specName, err)
	}
	providesParsed, err := parseRequirements(m.provides, m)
	if err != nil {
		return nil, fmt.Errorf("spec %s: %w", specName, err)
	}
	provides := make([]string, 0, len(providesParsed))
	for name := range providesParsed {
		provides = append(provides, name)
	}
	sort.Strings(provides)
	exclusive, err := expandArchList(m.exclusiveArch, m)
	if err != nil {
		return nil, fmt.Errorf("spec %s: %w", specName, err)
	}
	exclude, err := expandArchList(m.excludeArch, m)
	if err != nil {
		return nil, fmt.Errorf("spec %s: %w", specName, err)
	}
	exclusive = normalizeExclusiveArch(exclusive)
	exclude = normalizeX86(exclude)
	exclusive = removeAll(exclusive, exclude)
	if len(exclusive) == 0 {
		// 16.3 invariant "归一后列表恒非空": an empty whitelist would mean "no
		// buildable architecture", but archSupported treats empty as
		// allow-all — the inversion is a parseFailed (routed by E-23).
		return nil, fmt.Errorf("spec %s: exclusiveArch fully excluded by excludeArch", specName)
	}
	buildRequires, buildRemoves := splitBuildRequires(buildParsed)
	return &SpecDepend{
		RepoName:      repoName,
		SpecName:      specName,
		SpecFileName:  specFileName,
		Version:       joinVersion(epoch, version, release),
		Release:       release,
		Epoch:         epoch,
		ExclusiveArch: exclusive,
		Provides:      provides,
		Requires:      requires,
		BuildRequires: buildRequires,
		BuildRemoves:  buildRemoves,
	}, nil
}

// expandMacros expands %{...} references (no whitespace inside the braces)
// with lookup order: spec macro table (%define/%global) then same-named spec
// attributes. Undefined or empty references stay literal; conditional macros
// follow %{?x}/%{!x} semantics; expansion recurses until a round changes
// nothing. %{!x} without a default and x undefined is a parseFailed.
// maxExpandRounds and maxExpandLength bound macro expansion. Mutually
// referencing macros (%define x %{y} + %define y %{x}) never reach a fixed
// point, and self-referencing macros (%define x %{x}a) grow without bound;
// the default text engine must stay safe for untrusted repo content, so
// exceeding either bound fails the spec (parseFailed, routed by E-23).
const (
	maxExpandRounds = 32
	maxExpandLength = 64 * 1024
)

func expandMacros(value string, m *specModel) (string, error) {
	for round := 0; ; round++ {
		if round >= maxExpandRounds {
			return "", fmt.Errorf("macro expansion exceeded %d rounds (mutually referencing macros?)", maxExpandRounds)
		}
		var firstErr error
		out := macroRefPattern.ReplaceAllStringFunc(value, func(match string) string {
			if firstErr != nil {
				return match
			}
			replacement, keep, err := expandOne(match[2:len(match)-1], m)
			if err != nil {
				firstErr = err
				return match
			}
			if keep {
				return match
			}
			return replacement
		})
		if firstErr != nil {
			return "", firstErr
		}
		if out == value {
			return out, nil
		}
		if len(out) > maxExpandLength {
			return "", fmt.Errorf("macro expansion exceeded %d bytes (self-referencing macros?)", maxExpandLength)
		}
		value = out
	}
}

// expandOne resolves one %{...} body. keep reports that the literal must be
// preserved (undefined or empty plain reference).
func expandOne(inner string, m *specModel) (replacement string, keep bool, err error) {
	if inner == "" {
		return "", true, nil
	}
	conditional, negated := false, false
	switch inner[0] {
	case '?':
		conditional = true
		inner = inner[1:]
	case '!':
		conditional, negated = true, true
		inner = inner[1:]
	}
	name, defaultValue, hasDefault := inner, "", false
	if conditional {
		if i := strings.Index(inner, ":"); i >= 0 {
			name, defaultValue, hasDefault = inner[:i], inner[i+1:], true
		}
	}
	value, defined := lookupMacro(name, m)
	if !conditional {
		if !defined {
			return "", true, nil
		}
		return value, false, nil
	}
	if !negated {
		if defined {
			return value, false, nil
		}
		if hasDefault {
			return defaultValue, false, nil
		}
		return "", false, nil
	}
	if defined {
		return "", false, nil
	}
	if hasDefault {
		return defaultValue, false, nil
	}
	return "", false, fmt.Errorf("macro %%{!%s} is undefined and has no default", name)
}

// lookupMacro resolves a macro name: macro table first, then same-named spec
// attribute. defined means present with a non-empty value.
func lookupMacro(name string, m *specModel) (string, bool) {
	if value, ok := m.macros[name]; ok && value != "" {
		return value, true
	}
	if value, ok := m.props[name]; ok && value != "" {
		return value, true
	}
	return "", false
}

// rawRequirement is one phase-A token: a name, optionally with operator and
// version.
type rawRequirement struct{ name, op, version string }

// tokenizeRequirements performs phase A: split on blanks/commas, merge
// operator triples, then decompose with the拆解 regex. An operator token
// without a preceding token is a parseFailed.
func tokenizeRequirements(line string) ([]rawRequirement, error) {
	tokens := strings.FieldsFunc(line, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == ','
	})
	var merged []string
	expectVersion := false
	for _, token := range tokens {
		if mergeOperators[token] {
			if len(merged) == 0 {
				return nil, fmt.Errorf("requirement operator %q has no preceding token", token)
			}
			merged[len(merged)-1] += " " + token
			expectVersion = true
			continue
		}
		if expectVersion {
			merged[len(merged)-1] += " " + token
			expectVersion = false
			continue
		}
		merged = append(merged, token)
	}
	out := make([]rawRequirement, 0, len(merged))
	for _, token := range merged {
		if match := requirementSplit.FindStringSubmatch(token); match != nil {
			out = append(out, rawRequirement{name: match[1], op: match[2], version: match[3]})
		} else {
			out = append(out, rawRequirement{name: token})
		}
	}
	return out, nil
}

// parseRequirements performs the full two-phase requirement parsing over the
// given raw line values: macro-expand each line, tokenize, then merge into
// {name: {op: version}} with the documented rules (with-skip, bare-operator
// flag backfill, == inline whole-key replacement, paren stripping, same
// name+op last-wins).
func parseRequirements(lines []string, m *specModel) (map[string]ebsv1.VersionConst, error) {
	res := map[string]map[string]string{}
	var stack []string
	pendingOp, pendingName := "", ""
	backfill := func(rawVersion string) error {
		version := strings.TrimSuffix(rawVersion, ")")
		expanded, err := expandMacros(version, m)
		if err != nil {
			return err
		}
		res[pendingName][pendingOp] = expanded
		pendingOp, pendingName = "", ""
		return nil
	}
	for _, line := range lines {
		expanded, err := expandMacros(line, m)
		if err != nil {
			return nil, err
		}
		requirements, err := tokenizeRequirements(expanded)
		if err != nil {
			return nil, err
		}
		for _, requirement := range requirements {
			name := requirement.name
			if pendingOp != "" {
				// A pending bare-operator flag consumes this token's name as
				// the version of the last registered entry (LIFO).
				if err := backfill(name); err != nil {
					return nil, err
				}
				continue
			}
			if name == "with" {
				continue
			}
			if word, isOperator := operatorWords[name]; isOperator {
				if len(stack) == 0 {
					continue
				}
				pendingName = stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				pendingOp = word
				continue
			}
			if i := strings.Index(name, "=="); i >= 0 {
				// mergeOperators merges a spaced "foo == 1.2" into one token
				// ("foo " / " 1.2" around the ==); trim the stray blanks so
				// the registered key and version stay clean.
				key := strings.TrimSpace(strings.TrimPrefix(name[:i], "("))
				version := strings.TrimSuffix(strings.TrimSpace(name[i+2:]), ")")
				expandedVersion, err := expandMacros(version, m)
				if err != nil {
					return nil, err
				}
				res[key] = map[string]string{"EQ": expandedVersion}
				stack = append(stack, key)
				continue
			}
			name = strings.TrimPrefix(name, "(")
			if _, seen := res[name]; !seen {
				res[name] = map[string]string{}
				stack = append(stack, name)
			}
			if requirement.version != "" {
				version := strings.TrimSuffix(requirement.version, ")")
				expandedVersion, err := expandMacros(version, m)
				if err != nil {
					return nil, err
				}
				res[name][operatorWords[requirement.op]] = expandedVersion
			}
		}
		// A bare-operator flag dangling at end of line is discarded: the
		// registered dependency keeps its bare (versionless) constraint, and
		// the next line's first token is never consumed as its version.
		// Dangling operators are legitimate input — macro expansion can
		// empty a trailing version, e.g. "Requires: glibc => %{?ver}".
		pendingOp, pendingName = "", ""
	}
	out := make(map[string]ebsv1.VersionConst, len(res))
	for name, ops := range res {
		out[name] = ebsv1.VersionConst{GT: ops["GT"], GE: ops["GE"], EQ: ops["EQ"], LE: ops["LE"], LT: ops["LT"]}
	}
	return out, nil
}

// splitBuildRequires routes "-" prefixed keys to buildRemoves (prefix
// stripped) and the rest to buildRequires.
func splitBuildRequires(parsed map[string]ebsv1.VersionConst) (requires, removes map[string]ebsv1.VersionConst) {
	requires = map[string]ebsv1.VersionConst{}
	removes = map[string]ebsv1.VersionConst{}
	for name, constraint := range parsed {
		if strings.HasPrefix(name, "-") {
			removes[strings.TrimPrefix(name, "-")] = constraint
			continue
		}
		requires[name] = constraint
	}
	return requires, removes
}

// expandArchList macro-expands each accumulated line and splits it into
// items.
func expandArchList(lines []string, m *specModel) ([]string, error) {
	var out []string
	for _, line := range lines {
		expanded, err := expandMacros(line, m)
		if err != nil {
			return nil, err
		}
		out = append(out, strings.Fields(expanded)...)
	}
	return out, nil
}

// normalizeExclusiveArch applies the default architecture set to an empty
// declaration and the x86 → x86_64 normalization.
func normalizeExclusiveArch(list []string) []string {
	if len(list) == 0 {
		out := make([]string, len(defaultExclusiveArch))
		copy(out, defaultExclusiveArch[:])
		return out
	}
	return normalizeX86(list)
}

// normalizeX86 appends x86_64 when x86 is present.
func normalizeX86(list []string) []string {
	for _, arch := range list {
		if arch == "x86_64" {
			return list
		}
	}
	for _, arch := range list {
		if arch == "x86" {
			return append(list, "x86_64")
		}
	}
	return list
}

func removeAll(list, excludes []string) []string {
	if len(excludes) == 0 {
		return list
	}
	blocked := make(map[string]bool, len(excludes))
	for _, arch := range excludes {
		blocked[arch] = true
	}
	out := list[:0]
	for _, arch := range list {
		if !blocked[arch] {
			out = append(out, arch)
		}
	}
	return out
}

// joinVersion composes the final version: a non-zero epoch is prefixed as
// epoch:version, a non-empty release is appended as -release.
func joinVersion(epoch, version, release string) string {
	out := version
	if epoch != "" && epoch != "0" {
		out = epoch + ":" + out
	}
	if release != "" {
		out += "-" + release
	}
	return out
}
