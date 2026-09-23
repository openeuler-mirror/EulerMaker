package rpmver

import (
	"testing"

	ebsv1 "ebs-api/ebs/v1"
)

func TestVRCompare(t *testing.T) {
	cases := []struct {
		name   string
		x, y   string
		result int // -1 x<y, 0 equal, 1 x>y
	}{
		// Release preprocessing: release participates only when both sides
		// have one, so 1.0-2 equals 1.0 (declared deviation).
		{"release one-sided", "1.0-2", "1.0", 0},
		{"release one-sided reversed", "1.0", "1.0-2", 0},
		{"release both-sided", "1.0-2", "1.0-3", -1},
		{"release both-sided equal", "1.0-2", "1.0-2", 0},
		// Leading-zero digit segments compare lexicographically: "02" != "2".
		{"leading zero not equal", "02", "2", -1},
		{"leading zero with epoch", "0:02", "0:2", -1},
		{"1.05 lt 1.5", "1.05", "1.5", -1},
		// Numeric comparison without leading zeros.
		{"numeric 10 gt 9", "10", "9", 1},
		{"numeric 2 lt 10", "2", "10", -1},
		// Segment count: more segments wins.
		{"more segments", "1.0.1", "1.0", 1},
		{"more segments reversed", "1.0", "1.0.1", -1},
		// Epoch: padded 0: for the side without one.
		{"epoch padded equal", "1.0", "0:1.0", 0},
		{"epoch higher wins", "1:1.0", "2:1.0", -1},
		{"epoch beats version", "2:1.0", "1:9.9", 1},
		// Mixed segments compare as whole strings (declared deviation).
		{"mixed segment", "10a", "9b", -1},
		// Lexicographic fallback for non-numeric segments.
		{"alpha segments", "1.0a", "1.0b", -1},
		{"equal", "2:1.0-3", "2:1.0-3", 0},
		// Empty values after padding still compare positionally.
		{"empty vs version", "", "1.0", -1},
		// Empty segments are preserved: malformed versions never collapse
		// into well-formed ones ("" sorts below any digit segment).
		{"double dot not equal", "1..2", "1.2", -1},
		{"double dot reversed", "1.2", "1..2", 1},
		{"double dot equal", "1..2", "1..2", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := compare(tc.x, tc.y)
			if got != tc.result {
				t.Fatalf("compare(%q, %q) = %d, want %d", tc.x, tc.y, got, tc.result)
			}
			// Consistency: GT/GE/EQ/LE/LT must agree with the ordering.
			expect := map[string]bool{
				OpGT: got == 1, OpGE: got >= 0, OpEQ: got == 0,
				OpLE: got <= 0, OpLT: got == -1,
			}
			for op, want := range expect {
				ok, err := VRCompare(tc.x, op, tc.y)
				if err != nil {
					t.Fatalf("VRCompare(%q, %s, %q): %v", tc.x, op, tc.y, err)
				}
				if ok != want {
					t.Fatalf("VRCompare(%q, %s, %q) = %t, want %t", tc.x, op, tc.y, ok, want)
				}
			}
		})
	}
}

func TestVRCompareInvalidOperator(t *testing.T) {
	if _, err := VRCompare("1.0", "==", "1.0"); err == nil {
		t.Fatal("expected error for unknown operator")
	}
}

func TestVersionSatisfies(t *testing.T) {
	cases := []struct {
		name       string
		actual     string
		constraint ebsv1.VersionConst
		want       bool
	}{
		{"empty constraint passes", "anything", ebsv1.VersionConst{}, true},
		{"empty constraint empty actual passes", "", ebsv1.VersionConst{}, true},
		{"non-empty constraint empty actual fails", "", ebsv1.VersionConst{GE: "1.0"}, false},
		{"ge satisfied", "1.0", ebsv1.VersionConst{GE: "1.0"}, true},
		{"ge violated", "0.9", ebsv1.VersionConst{GE: "1.0"}, false},
		{"range satisfied", "1.5", ebsv1.VersionConst{GT: "1.0", LT: "2.0"}, true},
		{"range violated", "2.5", ebsv1.VersionConst{GT: "1.0", LT: "2.0"}, false},
		{"and semantics one clause empty", "1.5", ebsv1.VersionConst{GT: "1.0", EQ: ""}, true},
		{"eq with release", "1.0-2", ebsv1.VersionConst{EQ: "1.0"}, true},
		{"only eq clause", "1.1", ebsv1.VersionConst{EQ: "1.0"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := VersionSatisfies(tc.actual, tc.constraint)
			if err != nil {
				t.Fatalf("VersionSatisfies(%q, %+v): %v", tc.actual, tc.constraint, err)
			}
			if got != tc.want {
				t.Fatalf("VersionSatisfies(%q, %+v) = %t, want %t", tc.actual, tc.constraint, got, tc.want)
			}
		})
	}
}
