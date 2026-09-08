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
	"testing"

	"github.com/Masterminds/semver/v3"
)

func TestParseBoundsAccepted(t *testing.T) {
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := parseBounds(tt.constraint, prereleaseForbidden)
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseBounds(tt.constraint, tt.pre); err == nil {
				t.Errorf("parseBounds(%q) = nil error, want rejection", tt.constraint)
			}
		})
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
