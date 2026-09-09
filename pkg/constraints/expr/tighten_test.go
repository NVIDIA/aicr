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

package expr

import (
	"strings"
	"testing"
)

func TestTighten(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		existing    string
		candidate   string
		wantValue   string
		wantOutcome TightenOutcome
	}{
		{"raises the floor", ">= 1.30", ">= 1.35", ">= 1.35", TightenNarrowed},
		{"lower floor is dropped", ">= 1.35", ">= 1.30", ">= 1.35", TightenUnchanged},
		{"equal floor is dropped", ">= 1.35", ">= 1.35", ">= 1.35", TightenUnchanged},
		{"exclusive beats inclusive at the same version", ">= 1.35", "> 1.35", "> 1.35", TightenNarrowed},
		{"inclusive loses to exclusive at the same version", "> 1.35", ">= 1.35", "> 1.35", TightenUnchanged},
		{"differing precision is not orderable", ">= 1.32", ">= 1.32.4", "", TightenPrecisionMismatch},
		{"same precision at patch level", ">= 1.32.1", ">= 1.32.4", ">= 1.32.4", TightenNarrowed},
		{"ceiling survives a raised floor", ">= 1.34.1 < 1.36.0", ">= 1.35", ">= 1.35 < 1.36.0", TightenNarrowed},
		{"floor survives a lowered ceiling", ">= 1.32 < 1.36.0", "< 1.35.0", ">= 1.32 < 1.35.0", TightenNarrowed},
		{"a ceiling closes an open range", ">= 1.32", "< 1.35", ">= 1.32 < 1.35", TightenNarrowed},
		{"a floor closes an open range", "< 1.35", ">= 1.32", ">= 1.32 < 1.35", TightenNarrowed},
		{"single shared version is satisfiable", ">= 1.35", "<= 1.35", ">= 1.35 <= 1.35", TightenNarrowed},
		{"bounds equal only at the lower precision stay open", ">= 1.35", "< 1.35.2", ">= 1.35 < 1.35.2", TightenNarrowed},

		{"floor above ceiling", "<= 1.30", ">= 1.35", "", TightenUnsatisfiable},
		{"exclusive bounds meeting at one version", ">= 1.35", "< 1.35", "", TightenUnsatisfiable},
		{"disjoint closed ranges", ">= 1.30 < 1.32", ">= 1.34 < 1.36", "", TightenUnsatisfiable},
		{"floor meeting an omitted-component ceiling", ">= 1.34.1 < 1.36.0", ">= 1.36", "", TightenUnsatisfiable},
		{"exclusive coarse floor skips its own band", "> 1.35", "< 1.35.2", "", TightenUnsatisfiable},
		{"exclusive coarse floor clears the next band", "> 1.35", "< 1.36.2", "> 1.35 < 1.36.2", TightenNarrowed},
		{"inclusive coarse ceiling covers its own band", ">= 1.35.2", "<= 1.35", ">= 1.35.2 <= 1.35", TightenNarrowed},
		{"exclusive coarse ceiling excludes its own band", ">= 1.35.2", "< 1.35", "", TightenUnsatisfiable},
		{"floor below an omitted-component ceiling", ">= 1.34.1 < 1.36.5", ">= 1.36", ">= 1.36 < 1.36.5", TightenNarrowed},

		{"exact match has no ordering", "ubuntu", ">= 1.35", "", TightenIncomparable},
		{"equality has no ordering", ">= 1.32", "== 1.35", "", TightenIncomparable},
		{"inequality has no ordering", ">= 1.32", "!= 1.35", "", TightenIncomparable},
		{"node-set label predicates have no ordering",
			"gke-no-default-nvidia-gpu-device-plugin=true", "!gke-no-default-nvidia-gpu-device-plugin",
			"", TightenIncomparable},
		{"alternatives are not intersected", ">= 1.34 < 1.35 || >= 1.35.1", ">= 1.35", "", TightenIncomparable},
		{"unparseable version", ">= 1.32", ">= not-a-version", "", TightenIncomparable},
		{"a clause repeating a direction is not reduced", ">= 1.34.1 > 1.34.0", "< 1.36", "", TightenIncomparable},
		{"a repeated direction on the candidate side too", "< 1.36", ">= 1.34.1 > 1.34.0", "", TightenIncomparable},
		{"an exclusive loser is kept, not dropped", "> 1.34.0", ">= 1.34.1", ">= 1.34.1 > 1.34.0", TightenNarrowed},
		{"a losing exclusive candidate still restricts", ">= 24.04.1", "> 24.04.0", ">= 24.04.1 > 24.04.0", TightenNarrowed},
		{"empty candidate", ">= 1.32", "", "", TightenIncomparable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			value, outcome := Tighten(tt.existing, tt.candidate)
			if outcome != tt.wantOutcome {
				t.Fatalf("Tighten(%q, %q) outcome = %v, want %v",
					tt.existing, tt.candidate, outcome, tt.wantOutcome)
			}
			if value != tt.wantValue {
				t.Fatalf("Tighten(%q, %q) = %q, want %q",
					tt.existing, tt.candidate, value, tt.wantValue)
			}
		})
	}
}

// TestTightenResultParses guards the promise the recipe merge relies on: a
// tightened expression is written in the same grammar it came from, so it
// round-trips through the evaluator that reads the hydrated recipe.
func TestTightenResultParses(t *testing.T) {
	t.Parallel()

	value, outcome := Tighten(">= 1.34.1 < 1.36.0", ">= 1.35")
	if outcome != TightenNarrowed {
		t.Fatalf("Tighten() outcome = %v, want TightenNarrowed", outcome)
	}
	compound, err := ParseCompoundConstraint(value)
	if err != nil {
		t.Fatalf("ParseCompoundConstraint(%q) error = %v", value, err)
	}

	for _, tc := range []struct {
		actual string
		want   bool
	}{
		{"1.34.5", false},
		{"1.35.0", true},
		{"1.35.9", true},
		{"1.36.0", false},
	} {
		got, err := compound.Evaluate(tc.actual)
		if err != nil {
			t.Fatalf("Evaluate(%q) error = %v", tc.actual, err)
		}
		if got != tc.want {
			t.Errorf("%q against %q = %v, want %v", tc.actual, value, got, tc.want)
		}
	}
}

// TestTightenNeverWidens is the invariant the recipe merge depends on: the
// result is an intersection, so it must admit no version that either input
// rejected — neither the composed expression nor the profile's own.
// It is asserted by exhaustion rather than by argument because pkg/version
// compares at the lower of two precisions, which makes "stricter" subtle
// enough that reasoning about it has already been wrong once (an exclusive
// bound dropped in favor of a higher inclusive one admitted a shorter actual
// that the exclusive bound rejected).
func TestTightenNeverWidens(t *testing.T) {
	t.Parallel()

	operators := []string{">=", ">", "<=", "<"}
	versions := []string{
		"1.34", "1.34.0", "1.34.1", "1.35", "1.35.0", "1.35.2",
		"1.34.3-gke.100", "1.34.3-gke.900", "1.35.0-gke.100", "1.35.0-gke.101",
		"1.35.0-gke.0", "1.35.0-gke.1", "1.35-gke.100", "2", "1",
	}
	ranges := []string{
		">= 1.34.1 < 1.36.0", ">= 1.32 < 1.35", "> 1.34.0 <= 1.35.2",
		">= 1.34.3-gke.100 < 1.35.0", "> 1.35.0-gke.100 <= 1.35.0-gke.900",
		// The shape appendBound emits when it retains an exclusive loser.
		">= 1.34.1 > 1.34.0", ">= 24.04.1 > 24.04.0",
	}
	expressions := make([]string, 0, len(ranges)+len(operators)*len(versions))
	expressions = append(expressions, ranges...)
	for _, operator := range operators {
		for _, version := range versions {
			expressions = append(expressions, operator+" "+version)
		}
	}
	actuals := append([]string{
		"1.33", "1.33.9", "1.36", "1.36.0", "0.9",
		"1.35.0-gke.0", "1.35.0-gke.1", "1.35.0-gke.99", "1.35.0-gke.150",
		"1.36.0-gke.10", "1.34.3-gke.500",
	}, versions...)

	admits := func(t *testing.T, expression, actual string) bool {
		t.Helper()
		compound, err := ParseCompoundConstraint(expression)
		if err != nil {
			t.Fatalf("ParseCompoundConstraint(%q) error = %v", expression, err)
		}
		passed, err := compound.Evaluate(actual)
		if err != nil {
			t.Fatalf("Evaluate(%q against %q) error = %v", actual, expression, err)
		}
		return passed
	}

	for _, existing := range expressions {
		for _, candidate := range expressions {
			merged, outcome := Tighten(existing, candidate)
			if outcome != TightenNarrowed && outcome != TightenUnchanged {
				continue
			}
			for _, actual := range actuals {
				if !admits(t, merged, actual) {
					continue
				}
				// The result is an intersection, so it must imply BOTH
				// inputs. Checking only the composed side would miss a
				// result that silently discards the profile's own bound.
				if !admits(t, existing, actual) {
					t.Errorf("Tighten(%q, %q) = %q admits %q, which the composed expression rejects",
						existing, candidate, merged, actual)
				}
				if !admits(t, candidate, actual) {
					t.Errorf("Tighten(%q, %q) = %q admits %q, which the profile's own expression rejects",
						existing, candidate, merged, actual)
				}
			}
		}
	}
}

// TestTightenRejectsEmptyRanges is the complement of TestTightenNeverWidens,
// which cannot see this class: implication holds vacuously for a result no
// version satisfies, so an empty range accepted as narrowed passes it. Here
// every pair reported satisfiable must be satisfied by some version, and
// every pair reported unsatisfiable by none.
//
// It matters because the criteria-only generation path evaluates constraints
// with a nil evaluator: an empty range accepted here is not caught later, it
// ships in the recipe.
func TestTightenRejectsEmptyRanges(t *testing.T) {
	t.Parallel()

	operators := []string{">=", ">", "<=", "<"}
	versions := []string{
		"1", "1.35", "1.35.0", "1.35.2", "1.36", "1.36.0", "2",
		"1.35-gke.100", "1.35.0-gke.0", "1.35.0-gke.100", "1.35.0-gke.101",
		"1.36.0-gke.50",
	}
	// Readings carry at least major.minor (Kubernetes "1.34.1", Ubuntu
	// "24.04"), and emptiness is decided over that domain. A bare-major
	// actual is degenerate under pkg/version — "1" compares equal to every
	// 1.x bound and so satisfies mutually exclusive ranges — which is a
	// property of the comparison, not of this solver.
	actuals := []string{
		"0.9", "0.9.9",
		"1.34", "1.34.9", "1.35", "1.35.0", "1.35.1", "1.35.2", "1.35.9",
		"1.36", "1.36.0", "1.36.1", "1.36.9", "1.37", "1.37.0",
		"2.0", "2.0.0", "2.1", "3.0.0",
		// A GKE build sorts after the bare core it builds on, and the
		// build number is the finest dimension pkg/version orders.
		"1.35.0-gke.0", "1.35.0-gke.1", "1.35.0-gke.99", "1.35.0-gke.100",
		"1.35.0-gke.101", "1.35.0-gke.150",
		"1.36.0-gke.10", "1.36.0-gke.50", "1.36.0-gke.51",
	}

	expressions := make([]string, 0, len(operators)*len(versions))
	for _, operator := range operators {
		for _, version := range versions {
			expressions = append(expressions, operator+" "+version)
		}
	}

	for _, existing := range expressions {
		for _, candidate := range expressions {
			merged, outcome := Tighten(existing, candidate)
			if outcome != TightenNarrowed && outcome != TightenUnchanged && outcome != TightenUnsatisfiable {
				continue
			}

			satisfiable := false
			if outcome != TightenUnsatisfiable {
				compound, err := ParseCompoundConstraint(merged)
				if err != nil {
					t.Fatalf("ParseCompoundConstraint(%q) error = %v", merged, err)
				}
				for _, actual := range actuals {
					passed, err := compound.Evaluate(actual)
					if err != nil {
						t.Fatalf("Evaluate(%q against %q) error = %v", actual, merged, err)
					}
					if passed {
						satisfiable = true
						break
					}
				}
				if !satisfiable {
					t.Errorf("Tighten(%q, %q) = %q reported outcome %v, but no version satisfies it",
						existing, candidate, merged, outcome)
				}
				continue
			}

			// Reported unsatisfiable: no full-precision version may satisfy
			// both inputs. The check stops there deliberately. A coarser
			// actual can straddle two disjoint fine-grained bounds — "1.35"
			// compares equal to both ">= 1.35.2" and "<= 1.35.0" — and
			// refusing that range is the fail-closed reading of an
			// incoherent pair, not a false rejection to fix.
			for _, actual := range actuals {
				if !isFullPrecision(actual) {
					continue
				}
				if admitsBoth(t, existing, candidate, actual) {
					t.Errorf("Tighten(%q, %q) reported TightenUnsatisfiable, but %q satisfies both",
						existing, candidate, actual)
				}
			}
		}
	}
}

func admitsBoth(t *testing.T, first, second, actual string) bool {
	t.Helper()

	for _, expression := range []string{first, second} {
		compound, err := ParseCompoundConstraint(expression)
		if err != nil {
			t.Fatalf("ParseCompoundConstraint(%q) error = %v", expression, err)
		}
		passed, err := compound.Evaluate(actual)
		if err != nil {
			t.Fatalf("Evaluate(%q against %q) error = %v", actual, expression, err)
		}
		if !passed {
			return false
		}
	}
	return true
}

// isFullPrecision reports whether an actual names every numeric component,
// with or without a build suffix ("1.35.0", "1.35.0-gke.100").
func isFullPrecision(actual string) bool {
	core, _, _ := strings.Cut(actual, "-")
	return strings.Count(core, ".") == fullPrecision-1
}

// TestTightenGKEBuildDimension pins the two ways the build suffix breaks the
// numeric-only model: a coarse bound never has its suffix consulted, and
// adjacent builds leave no room between them.
func TestTightenGKEBuildDimension(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		existing    string
		candidate   string
		wantValue   string
		wantOutcome TightenOutcome
	}{
		{
			name:     "adjacent builds leave no room",
			existing: "> 1.35.0-gke.100", candidate: "< 1.35.0-gke.101",
			wantOutcome: TightenUnsatisfiable,
		},
		{
			name:     "separated builds do",
			existing: "> 1.35.0-gke.100", candidate: "< 1.35.0-gke.150",
			wantValue: "> 1.35.0-gke.100 < 1.35.0-gke.150", wantOutcome: TightenNarrowed,
		},
		{
			name:     "a coarse bound ignores its own suffix",
			existing: "> 1.35-gke.100", candidate: "< 1.36.0-gke.50",
			wantValue: "> 1.35-gke.100 < 1.36.0-gke.50", wantOutcome: TightenNarrowed,
		},
		{
			name:     "a build sorts after its bare core",
			existing: "> 1.35.0", candidate: "< 1.35.1",
			wantValue: "> 1.35.0 < 1.35.1", wantOutcome: TightenNarrowed,
		},
		{
			name:        "nothing sits between a bare core and its first build",
			existing:    "> 1.35.0",
			candidate:   "< 1.35.0-gke.0",
			wantOutcome: TightenUnsatisfiable,
		},
		{
			name:        "the first build itself is reachable",
			existing:    "> 1.35.0",
			candidate:   "<= 1.35.0-gke.0",
			wantValue:   "> 1.35.0 <= 1.35.0-gke.0",
			wantOutcome: TightenNarrowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			value, outcome := Tighten(tt.existing, tt.candidate)
			if outcome != tt.wantOutcome {
				t.Fatalf("Tighten(%q, %q) outcome = %v, want %v",
					tt.existing, tt.candidate, outcome, tt.wantOutcome)
			}
			if value != tt.wantValue {
				t.Fatalf("Tighten(%q, %q) = %q, want %q",
					tt.existing, tt.candidate, value, tt.wantValue)
			}
		})
	}
}
