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

package v1alpha1

import "testing"

func TestConditionTypesAreDistinct(t *testing.T) {
	all := []string{ConditionReady, ConditionAvailable, ConditionReconciling, ConditionStalled,
		ConditionDegraded, ConditionResolved, ConditionEngineAvailable}
	seen := map[string]bool{}
	for _, c := range all {
		if seen[c] {
			t.Fatalf("condition type %q is defined twice", c)
		}
		seen[c] = true
	}
	if ConditionReady == ConditionAvailable {
		t.Fatal("Ready (candidate) and Available (active plan) must be distinct")
	}
	if len(ResolverConditionTypes) != 1 || ResolverConditionTypes[0] != ConditionResolved {
		t.Fatalf("resolver must own only Resolved, got %v", ResolverConditionTypes)
	}
}

func TestRolloutPhaseTerminality(t *testing.T) {
	terminal := map[RolloutPhase]bool{
		PhaseSucceeded: true, PhaseFailed: true, PhaseExpired: true, PhaseRevoked: true,
		PhaseSuperseded: true, PhaseOrphaned: true,
		PhasePending: false, PhaseAwaitingApproval: false, PhaseProgressing: false,
		PhaseValidating: false, PhaseStalled: false,
	}
	for p, want := range terminal {
		if p.IsTerminal() != want {
			t.Errorf("%s.IsTerminal() = %v, want %v", p, p.IsTerminal(), want)
		}
	}
	if RolloutPhase("Bogus").IsTerminal() {
		t.Error("an unknown phase reported terminal")
	}
}
