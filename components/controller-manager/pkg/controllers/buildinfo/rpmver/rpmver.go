// Package rpmver implements the RPM version comparison used by the BuildInfo
// controller (design 16.2). It is deliberately NOT a full rpmvercmp: mixed
// alphanumeric segments compare as whole strings, and leading-zero digit
// segments compare lexicographically ("02" != "2"). Both are declared,
// accepted deviations.
package rpmver

import (
	"fmt"
	"strings"

	ebsv1 "ebs-api/ebs/v1"
)

// Comparison operators, matching the VersionConst word keys.
const (
	OpGT = "GT"
	OpGE = "GE"
	OpEQ = "EQ"
	OpLE = "LE"
	OpLT = "LT"
)

// VRCompare compares version strings x and y with the six-step algorithm and
// evaluates op over the result. An unknown op is an error; the caller treats
// it as a programming defect.
func VRCompare(x, op, y string) (bool, error) {
	switch op {
	case OpGT, OpGE, OpEQ, OpLE, OpLT:
	default:
		return false, fmt.Errorf("unknown operator %q", op)
	}
	switch compare(x, y) {
	case -1:
		return op == OpLT || op == OpLE, nil
	case 1:
		return op == OpGT || op == OpGE, nil
	default:
		return op == OpGE || op == OpEQ || op == OpLE, nil
	}
}

// VersionSatisfies evaluates whether actual meets the constraint, with the
// three-state semantics of 16.2 (S3 dirty-data protection):
//   - an all-empty constraint passes unconditionally;
//   - a non-empty constraint against an empty actual (missing artifact
//     version, e.g. RpmRepo dirty data) fails as unavailable;
//   - otherwise every operator must hold (AND).
func VersionSatisfies(actual string, constraint ebsv1.VersionConst) (bool, error) {
	if constraint == (ebsv1.VersionConst{}) {
		return true, nil
	}
	if actual == "" {
		return false, nil
	}
	for _, clause := range []struct {
		op      string
		version string
	}{
		{OpGT, constraint.GT},
		{OpGE, constraint.GE},
		{OpEQ, constraint.EQ},
		{OpLE, constraint.LE},
		{OpLT, constraint.LT},
	} {
		if clause.version == "" {
			continue
		}
		ok, err := VRCompare(actual, clause.op, clause.version)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// compare returns -1, 0 or 1 for x < y, x == y, x > y.
func compare(x, y string) int {
	// Step 1: release preprocessing — the release segment participates only
	// when both sides carry one; otherwise truncate both at the first "-".
	if !strings.Contains(x, "-") || !strings.Contains(y, "-") {
		x = cutAtFirst(x, "-")
		y = cutAtFirst(y, "-")
	}
	// Step 2: epoch preprocessing — pad a missing epoch with "0:".
	if !strings.Contains(x, ":") {
		x = "0:" + x
	}
	if !strings.Contains(y, ":") {
		y = "0:" + y
	}
	// Step 3: split into segments.
	xs := splitSegments(x)
	ys := splitSegments(y)
	// Step 4: pairwise comparison within the common prefix length; the first
	// decisive segment wins.
	n := len(xs)
	if len(ys) < n {
		n = len(ys)
	}
	for i := 0; i < n; i++ {
		if c := compareSegment(xs[i], ys[i]); c != 0 {
			return c
		}
	}
	// Step 5: with a common prefix equal, the side with more segments wins.
	switch {
	case len(xs) > len(ys):
		return 1
	case len(xs) < len(ys):
		return -1
	default:
		// Step 6: equal segment counts and all segments equal.
		return 0
	}
}

// compareSegment compares one segment: numeric only when both segments are
// pure digits and neither has a leading zero; otherwise lexicographic.
func compareSegment(x, y string) int {
	if isPlainNumber(x) && isPlainNumber(y) {
		// No leading zeros on either side: longer digit string is larger,
		// equal lengths compare digit by digit.
		if len(x) != len(y) {
			if len(x) > len(y) {
				return 1
			}
			return -1
		}
		return strings.Compare(x, y)
	}
	return strings.Compare(x, y)
}

// isPlainNumber reports whether s is non-empty, all ASCII digits, and has no
// leading zero.
func isPlainNumber(s string) bool {
	if s == "" || s[0] == '0' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// splitSegments splits on ".", ":" and "-", keeping empty segments so that
// positional differences are preserved ("1..2" != "1.2" — malformed versions
// are never silently collapsed into well-formed ones).
func splitSegments(s string) []string {
	segs := make([]string, 0, len(s)/2+1)
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '.', ':', '-':
			segs = append(segs, s[start:i])
			start = i + 1
		}
	}
	return append(segs, s[start:])
}

// cutAtFirst removes the first occurrence of sep and everything after it.
func cutAtFirst(s, sep string) string {
	if i := strings.Index(s, sep); i >= 0 {
		return s[:i]
	}
	return s
}
