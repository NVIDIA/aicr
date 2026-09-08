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

func TestParseBoundsAccepted(t *testing.T) {
	tests := []struct {
		name       string
		constraint string
		pre        prereleasePolicy
		wantLower  string // "" means unbounded
		lowerIncl  bool
		wantUpper  string
		upperIncl  bool
	}{
		{"upper exclusive only", "<0.18.0", prereleaseForbidden, "", false, "0.18.0", false},
		{"lower inclusive only", ">=0.16.0", prereleaseForbidden, "0.16.0", true, "", false},
		{"both, space separated", ">=0.18.0 <0.20.0", prereleaseForbidden, "0.18.0", true, "0.20.0", false},
		{"both, comma separated", ">=0.18.0, <0.20.0", prereleaseForbidden, "0.18.0", true, "0.20.0", false},
		{"both inclusive", ">=0.18.0 <=0.18.0", prereleaseForbidden, "0.18.0", true, "0.18.0", true},
		{"exact via =", "=0.18.0", prereleaseForbidden, "0.18.0", true, "0.18.0", true},
		{"exact bare", "0.18.0", prereleaseForbidden, "0.18.0", true, "0.18.0", true},
		{"v prefix normalizes", ">=v0.18.0 <v0.20.0", prereleaseForbidden, "0.18.0", true, "0.20.0", false},
		{"lower exclusive", ">0.18.0 <0.20.0", prereleaseForbidden, "0.18.0", false, "0.20.0", false},
		// "hotfix" contains an x/X; the wildcard check must look only at the
		// release segment (1.2.3), not the whole token, or a legal
		// prerelease tag gets misclassified as a wildcard.
		{"prerelease tag containing x is not a wildcard", "<=1.2.3-hotfix.1", prereleaseAllowed, "", false, "1.2.3-hotfix.1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := parseBounds(tt.constraint, tt.pre)
			if err != nil {
				t.Fatalf("parseBounds(%q) error = %v", tt.constraint, err)
			}
			checkBound(t, "lower", b.lower, tt.wantLower, tt.lowerIncl)
			checkBound(t, "upper", b.upper, tt.wantUpper, tt.upperIncl)
		})
	}
}

func checkBound(t *testing.T, side string, got bound, wantVer string, wantIncl bool) {
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

func TestParseBoundsRejected(t *testing.T) {
	tests := []struct {
		name       string
		constraint string
		pre        prereleasePolicy
	}{
		{"OR", ">=0.18.0 || >=0.20.0", prereleaseForbidden},
		{"caret", "^0.18.0", prereleaseForbidden},
		{"tilde", "~0.18.0", prereleaseForbidden},
		{"star wildcard", "*", prereleaseForbidden},
		{"x wildcard", "0.18.x", prereleaseForbidden},
		{"hyphen range", "0.18.0 - 0.20.0", prereleaseForbidden},
		{"not equal punches a hole bounds cannot express", ">=0.18.0 <0.20.0 !=0.19.0", prereleaseForbidden},
		{"prerelease when forbidden", "<0.18.0-rc.1", prereleaseForbidden},
		{"build metadata", "<=0.18.0+build.5", prereleaseAllowed},
		{"partial version", ">=0.18", prereleaseForbidden},
		{"partial version major only", ">=1", prereleaseForbidden},
		{"empty", "", prereleaseForbidden},
		{"whitespace only", "   ", prereleaseForbidden},
		{"two lower bounds", ">=0.18.0 >=0.19.0", prereleaseForbidden},
		{"two upper bounds", "<0.20.0 <0.21.0", prereleaseForbidden},
		{"garbage", "not-a-constraint", prereleaseForbidden},
		{"exact version after a comparator", "<0.20.0 0.18.0", prereleaseForbidden},
		{"exact version before a lower-bound comparator", "0.18.0 >=0.19.0", prereleaseForbidden},
		{"space between operator and version", ">= 0.18.0", prereleaseForbidden},
		{"trailing dot is not a valid version", "1.2.", prereleaseForbidden},
		{"uppercase V prefix does not normalize", "V1.2.3", prereleaseForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseBounds(tt.constraint, tt.pre); err == nil {
				t.Errorf("parseBounds(%q) = nil error, want rejection", tt.constraint)
			}
		})
	}
}

// Every parseBounds rejection must name the full constraint text it came
// from: Task 5 aggregates these into a per-record list where an
// unattributed message is useless.
func TestParseBoundsErrorMessagesNameConstraint(t *testing.T) {
	tests := []struct {
		name       string
		constraint string
		pre        prereleasePolicy
	}{
		{"OR", ">=0.18.0 || >=0.20.0", prereleaseForbidden},
		{"hyphen range", "0.18.0 - 0.20.0", prereleaseForbidden},
		{"unsupported operator", "^0.18.0", prereleaseForbidden},
		{"empty range", "", prereleaseForbidden},
		{"missing version after operator", ">= 0.18.0", prereleaseForbidden},
		{"wildcard", "0.18.x", prereleaseForbidden},
		{"partial version", ">=0.18", prereleaseForbidden},
		{"build metadata", "<=0.18.0+build.5", prereleaseAllowed},
		{"unparseable version", "V1.2.3", prereleaseForbidden},
		{"prerelease forbidden", "<0.18.0-rc.1", prereleaseForbidden},
		{"two lower bounds", ">=0.18.0 >=0.19.0", prereleaseForbidden},
		{"exact version after a comparator", "<0.20.0 0.18.0", prereleaseForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseBounds(tt.constraint, tt.pre)
			if err == nil {
				t.Fatalf("parseBounds(%q) = nil error, want rejection", tt.constraint)
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
func TestParseBoundsMissingVersionSuggestsJoinedForm(t *testing.T) {
	_, err := parseBounds(">= 0.18.0", prereleaseForbidden)
	if err == nil {
		t.Fatal("parseBounds(\">= 0.18.0\") = nil error, want rejection")
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
func TestParseBoundsExactVersionFirstReportsConflict(t *testing.T) {
	_, err := parseBounds("0.18.0 <0.20.0", prereleaseForbidden)
	if err == nil {
		t.Fatal("parseBounds(\"0.18.0 <0.20.0\") = nil error, want rejection")
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

// Every parseBounds error returns the zero-value bounds{}, whose sides are
// bounded (unbounded: false) with a nil ver. A caller that logs the error
// and calls contains anyway must get false, not a nil-pointer panic.
func TestBoundsZeroValueContainsIsFalse(t *testing.T) {
	v, err := semver.NewVersion("1.0.0")
	if err != nil {
		t.Fatalf("semver.NewVersion error = %v", err)
	}
	var zero bounds
	if zero.contains(v) {
		t.Error("zero-value bounds.contains = true, want false")
	}
	// The zero-value check above short-circuits on the lower side before
	// ever reaching the upper side's nil guard; exercise that guard
	// directly with an otherwise-unbounded lower side.
	upperOnly := bounds{lower: bound{unbounded: true}, upper: bound{unbounded: false, ver: nil}}
	if upperOnly.contains(v) {
		t.Error("bounds with nil upper.ver: contains = true, want false")
	}
}

// A prerelease is legal in `to`, which is what makes grove's
// v0.1.0-alpha.12 pin recordable at all.
func TestParseBoundsPrereleaseAllowedInTo(t *testing.T) {
	b, err := parseBounds(">=0.1.0-alpha.1 <=0.1.0-alpha.12", prereleaseAllowed)
	if err != nil {
		t.Fatalf("parseBounds error = %v", err)
	}
	if b.upper.unbounded || b.upper.ver.String() != "0.1.0-alpha.12" {
		t.Errorf("upper = %v, want 0.1.0-alpha.12", b.upper.ver)
	}
}

// The whole point of parseBounds is that it agrees with the library that
// actually decides membership. A sweep over release versions only would pass
// while missing Masterminds' prerelease behavior entirely, so the table
// carries prerelease and boundary-adjacent versions on purpose.
func TestBoundsAgreeWithMasterminds(t *testing.T) {
	constraints := []string{
		"<0.18.0",
		">=0.18.0",
		">=0.18.0 <0.20.0",
		">=0.18.0 <=0.18.0",
		">0.18.0 <0.20.0",
		"=0.18.0",
		">=0.1.0-alpha.1 <=0.1.0-alpha.12",
	}
	sweep := []string{
		"0.0.1", "0.17.9", "0.17.9-rc.1",
		"0.18.0-alpha.1", "0.18.0-rc.1", "0.18.0", "0.18.1",
		"0.19.9", "0.20.0", "0.20.1", "1.0.0",
		"0.1.0-alpha.1", "0.1.0-alpha.12", "0.1.0",
	}
	for _, cs := range constraints {
		t.Run(cs, func(t *testing.T) {
			b, err := parseBounds(cs, prereleaseAllowed)
			if err != nil {
				t.Fatalf("parseBounds(%q) error = %v", cs, err)
			}
			c, err := newConstraint(cs)
			if err != nil {
				t.Fatalf("newConstraint(%q) error = %v", cs, err)
			}
			for _, vs := range sweep {
				v, err := semver.NewVersion(vs)
				if err != nil {
					t.Fatalf("bad sweep version %q: %v", vs, err)
				}
				if got, want := b.contains(v), c.Check(v); got != want {
					t.Errorf("%q vs %q: bounds.contains = %v, Constraints.Check = %v",
						cs, vs, got, want)
				}
			}
		})
	}
}
