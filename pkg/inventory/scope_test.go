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

package inventory

import (
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// TestScopeCovers is the contract of the three tiers. Every case is named for
// the deployer or flag value that produces the shape, because the tier decides
// whether a record that cannot be read fails the run or is merely counted.
func TestScopeCovers(t *testing.T) {
	within := newScope("gpu-operator", "network-operator", "cert-manager", "nfd")

	tests := []struct {
		name   string
		record string
		want   confidence
	}{
		// Confident: names this project itself writes.
		{"helm and helmfile install under the component's own name", "gpu-operator", confident},
		{"a single-token component under its own name", "nfd", confident},
		{"the bundle writer's pre folder", "gpu-operator-pre", confident},
		{"the bundle writer's post folder", "gpu-operator-post", confident},
		{"the bundle writer's readiness folder", "gpu-operator-readiness", confident},

		// Possible: shapes a deployer's own naming produces.
		{"flux composes <targetNamespace>-<name>", "gpu-operator-gpu-operator", possible},
		{
			"flux with a namespace that is not the component name",
			"nvidia-network-operator-network-operator",
			possible,
		},
		{"argo namePrefix ending in a separator", "tenant-a-gpu-operator", possible},
		{
			// The case the token rule cannot see: the prefix fuses with the
			// component's first token. --set deployer:namePrefix=tenant is
			// accepted by ValidateNamePrefix today.
			"argo namePrefix not ending in a separator",
			"tenantgpu-operator",
			possible,
		},
		{"argo namePrefix and an injected phase together", "tenant-a-gpu-operator-pre", possible},
		{
			// The two rules above do not compose: the tail is the phase, not
			// the component, and the fused prefix hides the first token.
			"fused argo namePrefix with an injected pre folder",
			"tenantgpu-operator-pre",
			possible,
		},
		{"fused argo namePrefix with an injected post folder", "tenantgpu-operator-post", possible},
		{
			"fused argo namePrefix with an injected readiness folder",
			"tenantgpu-operator-readiness",
			possible,
		},
		{"fused argo namePrefix on a single-token component", "tenantnfd-post", possible},
		{
			// The deliberate cost of erring loose. Skipping this record's
			// failures is what makes it safe.
			"a foreign workload sharing a component token",
			"someone-elses-nfd-exporter",
			possible,
		},

		// Out of scope: no component's name appears at all.
		{"an unrelated workload", "someone-elses-app", outOfScope},
		{"a component's tokens out of order", "operator-gpu", outOfScope},
		{"a component's tokens split apart", "gpu-nvidia-operator", outOfScope},
		{"a partial trailing token", "gpu-operators", outOfScope},
		{"a component name inside a larger word", "mygpu-operatorx", outOfScope},
		{"a prefix of a component name", "gpu", outOfScope},
		{"the empty name", "", outOfScope},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := within.covers(tt.record); got != tt.want {
				t.Errorf("covers(%q) = %v, want %v", tt.record, got, tt.want)
			}
		})
	}
}

// TestScopeCoversPrefersTheStrongerTier pins that an exact name is confident
// even when another component in the same scope matches it only loosely, which
// is what keeps map iteration order out of the answer.
func TestScopeCoversPrefersTheStrongerTier(t *testing.T) {
	// "operator" matches "gpu-operator" as a raw suffix; "gpu-operator"
	// matches it exactly. Whichever the range visits first, the answer is the
	// stronger claim.
	within := newScope("gpu-operator", "operator")

	for range 20 {
		if got := within.covers("gpu-operator"); got != confident {
			t.Fatalf("covers() = %v, want %v", got, confident)
		}
	}
}

func TestNewScope(t *testing.T) {
	tests := []struct {
		name       string
		components []string
		wantLen    int
	}{
		{"names become entries", []string{"gpu-operator", "nfd"}, 2},
		{"duplicates collapse", []string{"nfd", "nfd"}, 1},
		{"empty names are dropped", []string{"gpu-operator", ""}, 1},
		{"only empty names leaves nothing", []string{"", ""}, 0},
		{"no names at all", nil, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := len(newScope(tt.components...)); got != tt.wantLen {
				t.Errorf("newScope(%q) has %d entries, want %d", tt.components, got, tt.wantLen)
			}
		})
	}
}

// TestScopeValidate pins the refusal that keeps a wiring mistake from reading
// as an empty cluster.
func TestScopeValidate(t *testing.T) {
	tests := []struct {
		name    string
		within  scope
		wantErr bool
	}{
		{"a scope naming a component is usable", newScope("gpu-operator"), false},
		{"an empty scope is refused", newScope(), true},
		{"a nil scope is refused", nil, true},
		{"a scope of only empty names is refused", newScope("", ""), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.within.validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				assertError(t, err, errors.ErrCodeInvalidRequest, nil)
			}
		})
	}
}
