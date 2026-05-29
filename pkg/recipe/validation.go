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

package recipe

import "maps"

// ValidationConfig defines validation phases and settings.
type ValidationConfig struct {
	// Readiness defines readiness validation phase settings.
	Readiness *ValidationPhase `json:"readiness,omitempty" yaml:"readiness,omitempty"`

	// Deployment defines deployment validation phase settings.
	Deployment *ValidationPhase `json:"deployment,omitempty" yaml:"deployment,omitempty"`

	// Performance defines performance validation phase settings.
	Performance *ValidationPhase `json:"performance,omitempty" yaml:"performance,omitempty"`

	// Conformance defines conformance validation phase settings.
	Conformance *ValidationPhase `json:"conformance,omitempty" yaml:"conformance,omitempty"`
}

// ValidationPhase represents a single validation phase configuration.
type ValidationPhase struct {
	// Timeout is the maximum duration for this phase (e.g., "10m").
	Timeout string `json:"timeout,omitempty" yaml:"timeout,omitempty"`

	// Constraints are phase-level constraints to evaluate.
	Constraints []Constraint `json:"constraints,omitempty" yaml:"constraints,omitempty"`

	// Checks are named validation checks to run in this phase.
	Checks []string `json:"checks,omitempty" yaml:"checks,omitempty"`

	// NodeSelection defines which nodes to include in validation.
	NodeSelection *NodeSelection `json:"nodeSelection,omitempty" yaml:"nodeSelection,omitempty"`

	// Infrastructure references a componentRef that provides validation infrastructure.
	// Example: "nccl-doctor" for performance testing.
	Infrastructure string `json:"infrastructure,omitempty" yaml:"infrastructure,omitempty"`
}

// NodeSelection defines node filtering for validation scope.
type NodeSelection struct {
	// Selector specifies label-based node selection.
	Selector map[string]string `json:"selector,omitempty" yaml:"selector,omitempty"`

	// MaxNodes limits the number of nodes to validate.
	MaxNodes int `json:"maxNodes,omitempty" yaml:"maxNodes,omitempty"`

	// ExcludeNodes lists node names to exclude from validation.
	ExcludeNodes []string `json:"excludeNodes,omitempty" yaml:"excludeNodes,omitempty"`
}

// cloneValidationConfig returns a deep copy of v. RecipeMetadataSpec.Merge
// uses this to avoid aliasing the source's nested phase pointers — without
// it, successive merges mutate whichever cached overlay's ValidationConfig
// the destination aliased.
func cloneValidationConfig(v *ValidationConfig) *ValidationConfig {
	if v == nil {
		return nil
	}
	return &ValidationConfig{
		Readiness:   cloneValidationPhase(v.Readiness),
		Deployment:  cloneValidationPhase(v.Deployment),
		Performance: cloneValidationPhase(v.Performance),
		Conformance: cloneValidationPhase(v.Conformance),
	}
}

// mergeValidationPhase merges overlay into base, returning a freshly allocated
// phase with no aliased state. Semantics mirror the top-level merge:
//   - Checks: an omitted overlay list (nil) inherits the base list; an
//     explicit empty list ([]string{}, from YAML `checks: []`) clears the
//     inherited list — needed by leaves like h100-eks-ubuntu-training-slurm
//     that must drop K8s-native checks (e.g., nccl-all-reduce-bw) which
//     don't apply to slurmd-managed clusters. A non-empty overlay list
//     unions with the base list, deduplicated, preserving order (base
//     entries first, then overlay-only entries appended).
//   - Constraints: same nil-vs-empty rule as Checks. A non-empty overlay
//     list unions with the base by Name; overlay value wins on same-name
//     (analogous to top-level RecipeMetadataSpec.Constraints).
//   - NodeSelection: overlay replaces if non-nil; otherwise base is preserved.
//   - Timeout, Infrastructure: overlay-wins-if-non-empty.
//
// If overlay is nil, base is returned untouched. If base is nil, overlay is
// deep-cloned to avoid aliasing into the source's cached metadata.
func mergeValidationPhase(base, overlay *ValidationPhase) *ValidationPhase {
	if overlay == nil {
		return base
	}
	if base == nil {
		return cloneValidationPhase(overlay)
	}

	out := &ValidationPhase{
		Timeout:        base.Timeout,
		Infrastructure: base.Infrastructure,
	}
	if overlay.Timeout != "" {
		out.Timeout = overlay.Timeout
	}
	if overlay.Infrastructure != "" {
		out.Infrastructure = overlay.Infrastructure
	}

	// Checks: explicit empty (non-nil, len 0) clears; nil inherits; non-empty
	// unions with base. yaml.v3 decodes `checks: []` as non-nil empty and an
	// omitted/null key as nil, so authors can intentionally drop inherited
	// checks (e.g., Slurm leaves dropping nccl-all-reduce-bw).
	switch {
	case overlay.Checks != nil && len(overlay.Checks) == 0:
		out.Checks = []string{}
	case len(base.Checks)+len(overlay.Checks) > 0:
		seen := make(map[string]bool, len(base.Checks)+len(overlay.Checks))
		out.Checks = make([]string, 0, len(base.Checks)+len(overlay.Checks))
		for _, c := range base.Checks {
			if !seen[c] {
				seen[c] = true
				out.Checks = append(out.Checks, c)
			}
		}
		for _, c := range overlay.Checks {
			if !seen[c] {
				seen[c] = true
				out.Checks = append(out.Checks, c)
			}
		}
	}

	// Constraints: same nil-vs-empty rule as Checks. Non-empty overlay
	// unions by Name; overlay value wins on same-name. Order preserves
	// base appearance, then overlay-only additions in overlay order.
	switch {
	case overlay.Constraints != nil && len(overlay.Constraints) == 0:
		out.Constraints = []Constraint{}
	case len(base.Constraints)+len(overlay.Constraints) > 0:
		overlayByName := make(map[string]Constraint, len(overlay.Constraints))
		for _, c := range overlay.Constraints {
			overlayByName[c.Name] = c
		}
		seen := make(map[string]bool, len(base.Constraints)+len(overlay.Constraints))
		out.Constraints = make([]Constraint, 0, len(base.Constraints)+len(overlay.Constraints))
		for _, c := range base.Constraints {
			if seen[c.Name] {
				continue
			}
			seen[c.Name] = true
			if ov, ok := overlayByName[c.Name]; ok {
				out.Constraints = append(out.Constraints, ov)
			} else {
				out.Constraints = append(out.Constraints, c)
			}
		}
		for _, c := range overlay.Constraints {
			if seen[c.Name] {
				continue
			}
			seen[c.Name] = true
			// Use the resolved overlay winner so intra-overlay duplicates
			// honor the same last-wins rule the base-override branch uses.
			out.Constraints = append(out.Constraints, overlayByName[c.Name])
		}
	}

	// NodeSelection: overlay-wins-if-non-nil (full struct replace).
	if overlay.NodeSelection != nil {
		out.NodeSelection = cloneNodeSelection(overlay.NodeSelection)
	} else if base.NodeSelection != nil {
		out.NodeSelection = cloneNodeSelection(base.NodeSelection)
	}

	return out
}

// cloneNodeSelection returns a deep copy of ns with independent backing
// map and slice, so callers writing through the clone cannot reach the
// source's cached metadata.
func cloneNodeSelection(ns *NodeSelection) *NodeSelection {
	if ns == nil {
		return nil
	}
	out := *ns
	if ns.Selector != nil {
		out.Selector = make(map[string]string, len(ns.Selector))
		maps.Copy(out.Selector, ns.Selector)
	}
	if ns.ExcludeNodes != nil {
		out.ExcludeNodes = make([]string, len(ns.ExcludeNodes))
		copy(out.ExcludeNodes, ns.ExcludeNodes)
	}
	return &out
}

// cloneValidationPhase returns a deep copy of p with independent backing
// slices and a freshly allocated NodeSelection, so callers writing through
// the clone cannot reach the source's cached metadata.
func cloneValidationPhase(p *ValidationPhase) *ValidationPhase {
	if p == nil {
		return nil
	}
	out := *p
	if p.Constraints != nil {
		out.Constraints = make([]Constraint, len(p.Constraints))
		copy(out.Constraints, p.Constraints)
	}
	if p.Checks != nil {
		out.Checks = make([]string, len(p.Checks))
		copy(out.Checks, p.Checks)
	}
	out.NodeSelection = cloneNodeSelection(p.NodeSelection)
	return &out
}
