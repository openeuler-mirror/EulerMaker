// Package specparse parses *.spec files into SpecDepend (design 16.3).
// It first expands each spec with rpmspec. RPM macro expansion can execute
// %(...) shell escapes and %{lua:...} blocks on the controller host, so
// package sources must be trusted. rpmspec errors do not fall back to raw
// text parsing. The expanded text is then converted into SpecDepend.
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
	RepoName      string                        `json:"repoName"`
	SpecName      string                        `json:"specName"`
	SpecFileName  string                        `json:"specFileName,omitempty"`
	Version       string                        `json:"version"`
	Release       string                        `json:"release,omitempty"`
	Epoch         string                        `json:"epoch,omitempty"`
	ExclusiveArch []string                      `json:"exclusiveArch,omitempty"`
	Provides      []string                      `json:"provides,omitempty"`
	Requires      map[string]ebsv1.VersionConst `json:"requires,omitempty"`
	BuildRequires map[string]ebsv1.VersionConst `json:"buildRequires,omitempty"`
	BuildRemoves  map[string]ebsv1.VersionConst `json:"buildRemoves,omitempty"`
}

// defaultExclusiveArch is the platform default architecture set used when a
// spec declares no ExclusiveArch. Callers receive a copy; the package array
// is never mutated.
var defaultExclusiveArch = [...]string{"x86_64", "aarch64", "loongarch64", "riscv64", "ppc64le", "sw_64"}

// rpmspecCommand is a package variable so tests can point it at a stub.
var rpmspecCommand = "rpmspec"

// rpmspecTimeout bounds one rpmspec subprocess run.
var rpmspecTimeout = 30 * time.Second

const (
	maxSourceFiles                = 3
	maxSourceBytes                = 1 << 20
	maxConcurrentRpmspecProcesses = 20
)

var sourceFileNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// Bound rpmspec subprocesses across all concurrent BuildInfo reconciles.
var rpmspecProcessSlots = make(chan struct{}, maxConcurrentRpmspecProcesses)

// SourceFetchError preserves the Git Server error classification for callers.
type SourceFetchError struct {
	Name string
	Err  error
}

func (e *SourceFetchError) Error() string { return fmt.Sprintf("fetch source %s: %v", e.Name, e.Err) }
func (e *SourceFetchError) Unwrap() error { return e.Err }

// Parse expands one *.spec file using rpmspec and parses the result. arch is
// the --target value; macros are buildPayload.macros lines passed to --load.
func Parse(specText, specFileName, repoName, arch string, macros []string) (*SpecDepend, error) {
	return ParseWithSources(specText, specFileName, repoName, arch, macros, nil)
}

// ParseWithSources may fetch a bounded number of missing, root-level text
// files into an isolated SOURCES directory before retrying rpmspec.
func ParseWithSources(specText, specFileName, repoName, arch string, macros []string, fetch func(string) (string, error)) (*SpecDepend, error) {
	text, err := expandWithSources(specText, arch, macros, fetch)
	if err != nil {
		return nil, fmt.Errorf("rpmspec %s: %w", specFileName, err)
	}
	return parseSpec(text, specFileName, repoName, macros)
}

// expandWithRpmspec returns an error on startup, execution, timeout or empty
// output instead of silently parsing the unexpanded input.
func expandWithRpmspec(specText, arch string, macros []string) (string, error) {
	return expandWithSources(specText, arch, macros, nil)
}

func expandWithSources(specText, arch string, macros []string, fetch func(string) (string, error)) (string, error) {
	if _, err := exec.LookPath(rpmspecCommand); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "specparse-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	sourceDir := filepath.Join(dir, "SOURCES")
	if err := os.Mkdir(sourceDir, 0o700); err != nil {
		return "", err
	}
	specPath := filepath.Join(dir, "input.spec")
	if err := os.WriteFile(specPath, []byte(specText), 0o644); err != nil {
		return "", err
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
		return "", err
	}
	loaded := make(map[string]struct{})
	for attempt := 0; attempt <= maxSourceFiles; attempt++ {
		rpmspecProcessSlots <- struct{}{}
		ctx, cancel := context.WithTimeout(context.Background(), rpmspecTimeout)
		cmd := exec.CommandContext(ctx, rpmspecCommand, "--target="+arch, "-P", specPath, "--load="+macroPath, "--define", "_sourcedir "+sourceDir)
		out, runErr := cmd.Output()
		ctxErr := ctx.Err()
		cancel()
		<-rpmspecProcessSlots
		if ctxErr != nil {
			return "", ctxErr
		}
		if runErr == nil {
			if strings.TrimSpace(string(out)) == "" {
				return "", errors.New("empty output")
			}
			return string(out), nil
		}
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			return "", runErr
		}
		stderr := strings.TrimSpace(string(exitErr.Stderr))
		name := missingSourceName(stderr, sourceDir)
		if fetch == nil || name == "" || attempt == maxSourceFiles {
			return "", fmt.Errorf("%w: %s", runErr, stderr)
		}
		if _, exists := loaded[name]; exists {
			return "", fmt.Errorf("%w: %s", runErr, stderr)
		}
		content, fetchErr := fetch(name)
		if fetchErr != nil {
			if errors.Is(fetchErr, os.ErrNotExist) {
				return "", fmt.Errorf("%w: %s", runErr, stderr)
			}
			return "", &SourceFetchError{Name: name, Err: fetchErr}
		}
		if len(content) > maxSourceBytes || strings.ContainsRune(content, '\x00') {
			return "", fmt.Errorf("source %s exceeds the text-file limit", name)
		}
		if err := os.WriteFile(filepath.Join(sourceDir, name), []byte(content), 0o600); err != nil {
			return "", err
		}
		loaded[name] = struct{}{}
	}
	return "", errors.New("source retry limit exceeded")
}

func missingSourceName(stderr, sourceDir string) string {
	prefix := sourceDir + string(os.PathSeparator)
	for _, line := range strings.Split(stderr, "\n") {
		if !strings.Contains(line, "No such file or directory") {
			continue
		}
		index := strings.Index(line, prefix)
		if index < 0 {
			continue
		}
		rest := line[index+len(prefix):]
		parts := strings.FieldsFunc(rest, func(r rune) bool {
			return r == '\'' || r == '"' || r == ':' || r == ')' || r == '(' || r == ' ' || r == '\t'
		})
		if len(parts) == 0 {
			continue
		}
		name := parts[0]
		if sourceFileNamePattern.MatchString(name) && name != "." && name != ".." {
			return name
		}
	}
	return ""
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
	macroNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
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
func parseSpec(text, specFileName, repoName string, macros []string) (*SpecDepend, error) {
	m := &specModel{macros: payloadMacroDefinitions(macros), props: map[string]string{}}
	parseLines(text, m)
	return buildSpec(m, specFileName, repoName)
}

// payloadMacroDefinitions reads simple RPM macro-file forms for any macro
// references left in rpmspec output. Definitions inside the spec override these.
func payloadMacroDefinitions(lines []string) map[string]string {
	definitions := make(map[string]string)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var name, value string
		if body, ok := directiveLine(line, "%define"); ok {
			name, value = splitDefine(body)
		} else if body, ok := directiveLine(line, "%global"); ok {
			name, value = splitDefine(body)
		} else if strings.HasPrefix(line, "%") {
			name, value = splitDefine(line[1:])
		}
		if macroNamePattern.MatchString(name) && value != "" {
			definitions[name] = value
		}
	}
	return definitions
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

// expandMacros expands %{...} references, including nested conditional bodies,
// with lookup order: spec macro table (%define/%global) then same-named spec
// attributes. Undefined or empty references stay literal; conditional macros
// follow %{?x}/%{!x} semantics; expansion recurses until a round changes
// nothing. %{!x} without a default and x undefined is a parseFailed.
// maxExpandRounds and maxExpandLength bound macro expansion. Mutually
// referencing macros (%define x %{y} + %define y %{x}) never reach a fixed
// point, and self-referencing macros (%define x %{x}a) grow without bound;
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
		out, err := expandMacroRound(value, m)
		if err != nil {
			return "", err
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

// expandMacroRound walks complete outermost references. A regular expression
// would stop at the inner '}' in %{?name:%{other}} and leave a stray brace.
func expandMacroRound(value string, m *specModel) (string, error) {
	var out strings.Builder
	for offset := 0; offset < len(value); {
		start := strings.Index(value[offset:], "%{")
		if start < 0 {
			out.WriteString(value[offset:])
			break
		}
		start += offset
		out.WriteString(value[offset:start])
		end := macroEnd(value, start)
		if end < 0 {
			out.WriteString(value[start:])
			break
		}
		match := value[start:end]
		replacement, keep, err := expandOne(match[2:len(match)-1], m)
		if err != nil {
			return "", err
		}
		if keep {
			out.WriteString(match)
		} else {
			out.WriteString(replacement)
		}
		offset = end
	}
	return out.String(), nil
}

// macroEnd returns the offset after the matching closing brace.
func macroEnd(value string, start int) int {
	depth := 1
	for i := start + 2; i < len(value); i++ {
		if strings.HasPrefix(value[i:], "%{") {
			depth++
			i++
			continue
		}
		if value[i] == '}' {
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// expandOne resolves one %{...} body. keep reports that the literal must be
// preserved (undefined or empty plain reference).
func expandOne(inner string, m *specModel) (replacement string, keep bool, err error) {
	if inner == "" {
		return "", true, nil
	}
	conditional, negated := false, false
	switch {
	case strings.HasPrefix(inner, "!?"):
		conditional, negated = true, true
		inner = inner[2:]
	case inner[0] == '?':
		conditional = true
		inner = inner[1:]
	case inner[0] == '!':
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
			if hasDefault {
				return defaultValue, false, nil
			}
			return value, false, nil
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
