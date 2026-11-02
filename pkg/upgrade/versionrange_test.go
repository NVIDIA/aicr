// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package upgrade

import (
	"strings"
	"testing"

	"github.com/Masterminds/semver/v3"
)

func TestParseRangeAccepted(t *testing.T) {
	tests := []struct {
		name       string
		constraint string
		wantLower  string // "" means unbounded
		lowerIncl  bool
		wantUpper  string
		upperIncl  bool
	}{
		{"upper exclusive only", "<0.18.0", "", false, "0.18.0", false},
		{"lower inclusive only", ">=0.16.0", "0.16.0", true, "", false},
		{"both, space separated", ">=0.18.0 <0.20.0", "0.18.0", true, "0.20.0", false},
		{"both, comma separated", ">=0.18.0, <0.20.0", "0.18.0", true, "0.20.0", false},
		{"both inclusive", ">=0.18.0 <=0.18.0", "0.18.0", true, "0.18.0", true},
		{"exact via =", "=0.18.0", "0.18.0", true, "0.18.0", true},
		{"exact bare", "0.18.0", "0.18.0", true, "0.18.0", true},
		{"v prefix normalizes", ">=v0.18.0 <v0.20.0", "0.18.0", true, "0.20.0", false},
		{"lower exclusive", ">0.18.0 <0.20.0", "0.18.0", false, "0.20.0", false},
		// "hotfix" contains an x/X; the wildcard check must look only at the
		// release segment (1.2.3), not the whole token, or a legal
		// prerelease tag gets misclassified as a wildcard.
		{"prerelease tag containing x is not a wildcard", "<=1.2.3-hotfix.1", "", false, "1.2.3-hotfix.1", true},
		// A prerelease lower bound is legal too, not only a prerelease
		// ceiling: a component whose upgrade history crosses more than one
		// prerelease boundary before a release needs both sides recordable.
		{"prerelease lower bound", ">=0.1.0-alpha.9 <=0.1.0-alpha.12", "0.1.0-alpha.9", true, "0.1.0-alpha.12", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := parseRange(tt.constraint)
			if err != nil {
				t.Fatalf("parseRange(%q) error = %v", tt.constraint, err)
			}
			checkBound(t, "lower", b.lower, tt.wantLower, tt.lowerIncl)
			checkBound(t, "upper", b.upper, tt.wantUpper, tt.upperIncl)
		})
	}
}

func checkBound(t *testing.T, side string, got rangeEnd, wantVer string, wantIncl bool) {
	t.Helper()
	if wantVer == "" {
		if !got.unbounded {
			t.Errorf("%s: got bounded %v, want unbounded", side, got.ver)
		}
		return
	}
	if got.unbounded {
		t.Fatalf("%s: got unbounded, want %s", side, wantVer)
	}
	if got.ver.String() != wantVer {
		t.Errorf("%s: version = %s, want %s", side, got.ver, wantVer)
	}
	if got.inclusive != wantIncl {
		t.Errorf("%s: inclusive = %v, want %v", side, got.inclusive, wantIncl)
	}
}

func TestParseRangeRejected(t *testing.T) {
	tests := []struct {
		name       string
		constraint string
	}{
		{"OR", ">=0.18.0 || >=0.20.0"},
		{"caret", "^0.18.0"},
		{"tilde", "~0.18.0"},
		{"star wildcard", "*"},
		{"x wildcard", "0.18.x"},
		{"hyphen range", "0.18.0 - 0.20.0"},
		{"not equal punches a hole bounds cannot express", ">=0.18.0 <0.20.0 !=0.19.0"},
		{"build metadata", "<=0.18.0+build.5"},
		{"partial version", ">=0.18"},
		{"partial version major only", ">=1"},
		{"empty", ""},
		{"whitespace only", "   "},
		{"two lower bounds", ">=0.18.0 >=0.19.0"},
		{"two upper bounds", "<0.20.0 <0.21.0"},
		{"garbage", "not-a-constraint"},
		{"exact version after a comparator", "<0.20.0 0.18.0"},
		{"exact version before a lower-bound comparator", "0.18.0 >=0.19.0"},
		{"space between operator and version", ">= 0.18.0"},
		{"trailing dot is not a valid version", "1.2."},
		{"uppercase V prefix does not normalize", "V1.2.3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseRange(tt.constraint); err == nil {
				t.Errorf("parseRange(%q) = nil error, want rejection", tt.constraint)
			}
		})
	}
}

// TestParseRangeErrorMessagesNameConstraint checks that every parseRange
// error names the full constraint.
func TestParseRangeErrorMessagesNameConstraint(t *testing.T) {
	tests := []struct {
		name       string
		constraint string
	}{
		{"OR", ">=0.18.0 || >=0.20.0"},
		{"hyphen range", "0.18.0 - 0.20.0"},
		{"unsupported operator", "^0.18.0"},
		{"empty range", ""},
		{"missing version after operator", ">= 0.18.0"},
		{"wildcard", "0.18.x"},
		{"partial version", ">=0.18"},
		{"build metadata", "<=0.18.0+build.5"},
		{"unparseable version", "V1.2.3"},
		{"two lower bounds", ">=0.18.0 >=0.19.0"},
		{"exact version after a comparator", "<0.20.0 0.18.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseRange(tt.constraint)
			if err == nil {
				t.Fatalf("parseRange(%q) = nil error, want rejection", tt.constraint)
			}
			if !strings.Contains(err.Error(), tt.constraint) {
				t.Errorf("error %q does not name the constraint %q", err.Error(), tt.constraint)
			}
		})
	}
}

// The most likely authoring slip - a space after the operator - must not be
// silently accepted (that would widen the grammar) but must tell the author
// exactly what to write instead.
func TestParseRangeMissingVersionSuggestsJoinedForm(t *testing.T) {
	_, err := parseRange(">= 0.18.0")
	if err == nil {
		t.Fatal("parseRange(\">= 0.18.0\") = nil error, want rejection")
	}
	msg := err.Error()
	for _, want := range []string{"space", ">=0.18.0"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
}

// A bare (exact) version followed by another comparator must be reported as
// an exact-version conflict regardless of which side appears first; the
// exact-version branch sets both bounds, so a naive twice-check on whichever
// bound the second comparator targets reports the wrong reason.
func TestParseRangeExactVersionFirstReportsConflict(t *testing.T) {
	_, err := parseRange("0.18.0 <0.20.0")
	if err == nil {
		t.Fatal("parseRange(\"0.18.0 <0.20.0\") = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "combines an exact version") {
		t.Errorf("error = %q, want a message about combining an exact version", err.Error())
	}
}

// newConstraint must surface a semver.NewConstraint failure rather than
// panic or silently succeed on ungrammatical input.
func TestNewConstraintError(t *testing.T) {
	if _, err := newConstraint("not-a-constraint"); err == nil {
		t.Error("newConstraint(\"not-a-constraint\") = nil error, want error")
	}
}

// TestVersionRangeZeroValueContainsIsFalse checks that the zero versionRange,
// which every parseRange error returns, contains no version and does not
// panic on its nil vers.
func TestVersionRangeZeroValueContainsIsFalse(t *testing.T) {
	v, err := semver.NewVersion("1.0.0")
	if err != nil {
		t.Fatalf("semver.NewVersion error = %v", err)
	}
	var zero versionRange
	if zero.contains(v) {
		t.Error("zero-value versionRange.contains = true, want false")
	}
	// The zero-value check above short-circuits on the lower side before
	// ever reaching the upper side's nil guard; exercise that guard
	// directly with an otherwise-unbounded lower side.
	upperOnly := versionRange{lower: rangeEnd{unbounded: true}, upper: rangeEnd{unbounded: false, ver: nil}}
	if upperOnly.contains(v) {
		t.Error("versionRange with nil upper.ver: contains = true, want false")
	}
}

// A prerelease is legal in `to`, which is what makes grove's
// v0.1.0-alpha.12 pin recordable at all.
func TestParseRangePrereleaseAllowedInTo(t *testing.T) {
	b, err := parseRange(">=0.1.0-alpha.1 <=0.1.0-alpha.12")
	if err != nil {
		t.Fatalf("parseRange error = %v", err)
	}
	if b.upper.unbounded || b.upper.ver.String() != "0.1.0-alpha.12" {
		t.Errorf("upper = %v, want 0.1.0-alpha.12", b.upper.ver)
	}
}

// TestVersionRangeAgreesWithMasterminds checks that parseRange agrees with
// Masterminds, whose oracle for "<X.Y.Z" on a release is "<X.Y.Z-0".
func TestVersionRangeAgreesWithMasterminds(t *testing.T) {
	constraints := []struct{ rng, oracle string }{
		{"<0.18.0", "<0.18.0-0"},
		{">=0.18.0", ">=0.18.0"},
		{">=0.18.0 <0.20.0", ">=0.18.0 <0.20.0-0"},
		{">=0.18.0 <=0.18.0", ">=0.18.0 <=0.18.0"},
		{">0.18.0 <0.20.0", ">0.18.0 <0.20.0-0"},
		{"=0.18.0", "=0.18.0"},
		{">=0.1.0-alpha.1 <=0.1.0-alpha.12", ">=0.1.0-alpha.1 <=0.1.0-alpha.12"},
		{"<0.1.0-alpha.12", "<0.1.0-alpha.12"},
	}
	// A sweep over releases alone would miss prerelease behavior entirely.
	sweep := []string{
		"0.0.1", "0.17.9", "0.17.9-rc.1",
		"0.18.0-alpha.1", "0.18.0-rc.1", "0.18.0", "0.18.1",
		"0.19.9", "0.20.0-rc.1", "0.20.0", "0.20.1", "1.0.0",
		"0.1.0-alpha.1", "0.1.0-alpha.11", "0.1.0-alpha.12", "0.1.0",
	}
	for _, tc := range constraints {
		cs := tc.rng
		t.Run(cs, func(t *testing.T) {
			b, err := parseRange(cs)
			if err != nil {
				t.Fatalf("parseRange(%q) error = %v", cs, err)
			}
			c, err := newConstraint(tc.oracle)
			if err != nil {
				t.Fatalf("newConstraint(%q) error = %v", tc.oracle, err)
			}
			for _, vs := range sweep {
				v, err := semver.NewVersion(vs)
				if err != nil {
					t.Fatalf("bad sweep version %q: %v", vs, err)
				}
				if got, want := b.contains(v), c.Check(v); got != want {
					t.Errorf("%q vs %q: versionRange.contains = %v, Constraints.Check(%q) = %v",
						cs, vs, got, tc.oracle, want)
				}
			}
		})
	}
}

// TestVersionRangePrereleaseOfARelease checks that "<1.25.0" leaves out
// 1.25.0-rc.1 and that every other operator orders it below 1.25.0, in plain
// semver.
func TestVersionRangePrereleaseOfARelease(t *testing.T) {
	rc := semver.MustParse("1.25.0-rc.1")
	tests := []struct {
		rng  string
		want bool
	}{
		{"<1.25.0", false},
		{"<=1.25.0", true},
		{">1.25.0", false},
		{">=1.25.0", false},
		{"=1.25.0", false},
		{"<1.25.0-rc.2", true},
		{">=1.25.0-rc.1 <1.25.0", false},
		{">=1.25.0-rc.1 <=1.25.0", true},
	}
	for _, tt := range tests {
		t.Run(tt.rng, func(t *testing.T) {
			b, err := parseRange(tt.rng)
			if err != nil {
				t.Fatalf("parseRange(%q) error = %v", tt.rng, err)
			}
			if got := b.contains(rc); got != tt.want {
				t.Errorf("parseRange(%q).contains(%s) = %v, want %v", tt.rng, rc, got, tt.want)
			}
		})
	}
}

func rangeOf(t *testing.T, s string) versionRange {
	t.Helper()
	r, err := parseRange(s)
	if err != nil {
		t.Fatalf("parseRange(%q) error = %v", s, err)
	}
	return r
}

func TestOverlaps(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"inclusive ends meeting share that version", ">=1.0.0 <=2.0.0", ">=2.0.0 <3.0.0", true},
		{"an exclusive end meeting an inclusive one shares nothing", ">=1.0.0 <2.0.0", ">=2.0.0 <3.0.0", false},
		{"two exclusive ends meeting share nothing", ">=1.0.0 <2.0.0", ">2.0.0 <3.0.0", false},
		{"< on a release leaves out its prereleases", "<2.0.0", ">=2.0.0-rc.1 <=2.0.0-rc.2", false},
		{"<= on a release holds its prereleases", "<=2.0.0", ">=2.0.0-rc.1 <=2.0.0-rc.2", true},
		{"disjoint ranges", ">=1.0.0 <1.5.0", ">=1.6.0 <2.0.0", false},
		{"open ends reach each other", "<2.0.0", ">=1.0.0", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := rangeOf(t, tt.a), rangeOf(t, tt.b)
			if got := overlaps(a, b); got != tt.want {
				t.Errorf("overlaps(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
			if got := overlaps(b, a); got != tt.want {
				t.Errorf("overlaps(%q, %q) = %v, want %v", tt.b, tt.a, got, tt.want)
			}
		})
	}
}

func TestPastCeiling(t *testing.T) {
	tests := []struct {
		rng, v string
		want   bool
	}{
		{"<=2.0.0", "2.0.0", false},
		{"<=2.0.0", "2.0.1", true},
		{"<2.0.0", "2.0.0", true},
		{"<2.0.0", "1.9.9", false},
		{"<2.0.0", "2.0.0-rc.1", true},
		{"<2.0.0-rc.2", "2.0.0-rc.1", false},
	}
	for _, tt := range tests {
		t.Run(tt.rng+" "+tt.v, func(t *testing.T) {
			ceiling := rangeOf(t, tt.rng).upper
			if got := pastCeiling(ceiling, semver.MustParse(tt.v)); got != tt.want {
				t.Errorf("pastCeiling(%q, %s) = %v, want %v", tt.rng, tt.v, got, tt.want)
			}
		})
	}
}

func TestEndsEqual(t *testing.T) {
	lower := func(s string) rangeEnd { return rangeOf(t, s).lower }
	upper := func(s string) rangeEnd { return rangeOf(t, s).upper }
	tests := []struct {
		name string
		a, b rangeEnd
		want bool
	}{
		{"the same inclusive floor", lower(">=1.0.0"), lower(">=1.0.0"), true},
		{"inclusive and exclusive at one version", lower(">=1.0.0"), lower(">1.0.0"), false},
		{"different versions", lower(">=1.0.0"), lower(">=1.1.0"), false},
		{"< on a release and < its first prerelease", upper("<1.0.0"), upper("<1.0.0-0"), true},
		{"unbounded and bounded", lower("<2.0.0"), lower(">=1.0.0"), false},
		{"two unbounded ends", lower("<2.0.0"), lower("<3.0.0"), true},
		{"two zero values", rangeEnd{}, rangeEnd{}, true},
		{"a zero value and a bounded end", rangeEnd{}, lower(">=1.0.0"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := endsEqual(tt.a, tt.b); got != tt.want {
				t.Errorf("endsEqual(%+v, %+v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
			if got := endsEqual(tt.b, tt.a); got != tt.want {
				t.Errorf("endsEqual(%+v, %+v) = %v, want %v", tt.b, tt.a, got, tt.want)
			}
		})
	}
}

func TestRangeUpTo(t *testing.T) {
	tests := []struct {
		floor, v string
		want     bool
	}{
		{">=2.0.0", "1.9.9", true},
		{">=2.0.0", "2.0.0-rc.1", true},
		{">=2.0.0", "2.0.0", false},
		{">2.0.0", "2.0.0", true},
		{">2.0.0", "2.0.1", false},
		{">=2.0.0", "0.9.0", false},
	}
	lower := rangeOf(t, ">=1.0.0").lower
	for _, tt := range tests {
		t.Run(tt.floor+" "+tt.v, func(t *testing.T) {
			r := rangeUpTo(lower, rangeOf(t, tt.floor).lower)
			if got := r.contains(semver.MustParse(tt.v)); got != tt.want {
				t.Errorf("rangeUpTo(>=1.0.0, %q).contains(%s) = %v, want %v", tt.floor, tt.v, got, tt.want)
			}
		})
	}
}

func TestAdmits(t *testing.T) {
	v := semver.MustParse
	tests := []struct {
		name string
		rng  string
		at   rangeEnd
		want bool
	}{
		{"on an inclusive floor", ">=2.0.0 <3.0.0", rangeEnd{ver: v("2.0.0"), inclusive: true}, true},
		{"on an exclusive floor", ">2.0.0 <3.0.0", rangeEnd{ver: v("2.0.0"), inclusive: true}, false},
		{"just above an exclusive floor", ">2.0.0 <3.0.0", rangeEnd{ver: v("2.0.0")}, true},
		{"just above an inclusive floor", ">=2.0.0 <3.0.0", rangeEnd{ver: v("2.0.0")}, true},
		{"just above a version below the floor", ">=2.0.0 <3.0.0", rangeEnd{ver: v("1.9.0")}, false},
		{"just above an inclusive ceiling", ">=1.0.0 <=2.0.0", rangeEnd{ver: v("2.0.0")}, false},
		{"just above a version below the ceiling", ">=1.0.0 <2.0.0", rangeEnd{ver: v("1.9.0")}, true},
		{"just above anything in an open range", "<3.0.0", rangeEnd{ver: v("2.0.0")}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := parseRange(tt.rng)
			if err != nil {
				t.Fatalf("parseRange(%q): %v", tt.rng, err)
			}
			if got := admits(b, tt.at); got != tt.want {
				t.Errorf("admits(%q, %+v) = %v, want %v", tt.rng, tt.at, got, tt.want)
			}
		})
	}
}
