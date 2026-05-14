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
