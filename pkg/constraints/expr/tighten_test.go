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

import "testing"

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
		{"floor below an omitted-component ceiling", ">= 1.34.1 < 1.36.5", ">= 1.36", ">= 1.36 < 1.36.5", TightenNarrowed},

		{"exact match has no ordering", "ubuntu", ">= 1.35", "", TightenIncomparable},
		{"equality has no ordering", ">= 1.32", "== 1.35", "", TightenIncomparable},
		{"inequality has no ordering", ">= 1.32", "!= 1.35", "", TightenIncomparable},
		{"node-set label predicates have no ordering",
			"gke-no-default-nvidia-gpu-device-plugin=true", "!gke-no-default-nvidia-gpu-device-plugin",
			"", TightenIncomparable},
		{"alternatives are not intersected", ">= 1.34 < 1.35 || >= 1.35.1", ">= 1.35", "", TightenIncomparable},
		{"unparseable version", ">= 1.32", ">= not-a-version", "", TightenIncomparable},
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
		"1.34.3-gke.100", "1.34.3-gke.900", "2", "1",
	}
	ranges := []string{">= 1.34.1 < 1.36.0", ">= 1.32 < 1.35", "> 1.34.0 <= 1.35.2"}
	expressions := make([]string, 0, len(ranges)+len(operators)*len(versions))
	expressions = append(expressions, ranges...)
	for _, operator := range operators {
		for _, version := range versions {
			expressions = append(expressions, operator+" "+version)
		}
	}
	actuals := append([]string{"1.33", "1.33.9", "1.36", "1.36.0", "0.9"}, versions...)

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
