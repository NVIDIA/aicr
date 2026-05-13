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

const (
	// KindValidation is the Kubernetes kind for Validation resources.
	KindValidation = "Validation"
)

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

// Validation is the complete validation specification.
// Supports both standalone file usage (with full metadata) and embedded usage in CRs (metadata omitted).
//
// Standalone usage (validation.yaml):
//
//	apiVersion: aicr.nvidia.com/v1
//	kind: Validation
//	metadata:
//	  name: my-validation
//	  version: 1.0.0
//	componentRefs: [...]
//	criteria: {...}
//
// Embedded usage (in a CR):
//
//	spec:
//	  validation:
//	    componentRefs: [...]
//	    criteria: {...}
type Validation struct {
	// APIVersion is the API version (optional, for standalone resource usage).
	APIVersion string `json:"apiVersion,omitempty" yaml:"apiVersion,omitempty"`

	// Kind is always "Validation" (optional, for standalone resource usage).
	Kind string `json:"kind,omitempty" yaml:"kind,omitempty"`

	// Metadata contains validation metadata (optional, for standalone resource usage).
	Metadata *ValidationMetadata `json:"metadata,omitempty" yaml:"metadata,omitempty"`

	ValidationConfig `json:",inline" yaml:",inline"`

	// ComponentRefs lists the components to validate (optional).
	ComponentRefs []ComponentRef `json:"componentRefs,omitempty" yaml:"componentRefs,omitempty"`

	// Criteria specifies the cluster characteristics (optional).
	Criteria Criteria `json:"criteria,omitempty" yaml:"criteria,omitempty"`

	// Constraints are top-level readiness constraints evaluated before validation phases (optional).
	Constraints []Constraint `json:"constraints,omitempty" yaml:"constraints,omitempty"`
}

// ValidationMetadata contains validation-level metadata.
type ValidationMetadata struct {
	// Name is a human-readable name for this validation.
	Name string `json:"name,omitempty" yaml:"name,omitempty"`

	// Version is the version of this validation specification.
	Version string `json:"version,omitempty" yaml:"version,omitempty"`
}

// NewValidation creates a new empty Validation instance.
func NewValidation() *Validation {
	return &Validation{
		ValidationConfig: ValidationConfig{},
		ComponentRefs:    []ComponentRef{},
		Criteria:         Criteria{},
		Constraints:      []Constraint{},
	}
}

// ToValidation converts RecipeResult to Validation for use with validators.
// This extracts the validation-relevant fields (ValidationConfig, ComponentRefs, Criteria)
// and discards recipe-specific metadata (AppliedOverlays, DeploymentOrder, etc.).
// Returns nil if the input RecipeResult is nil.
//
// Populates optional APIVersion/Kind/Metadata fields to support standalone usage.
// When embedding in CRs, these fields can be omitted via omitempty tags.
func ToValidation(r *RecipeResult) *Validation {
	if r == nil {
		return nil
	}

	validation := NewValidation()

	// Populate optional resource fields for standalone usage
	validation.APIVersion = r.APIVersion
	validation.Kind = KindValidation // Change from "RecipeResult" to "Validation"
	if r.Metadata.Version != "" {
		validation.Metadata = &ValidationMetadata{
			Version: r.Metadata.Version,
		}
	}

	// Copy ValidationConfig if present
	if r.Validation != nil {
		validation.ValidationConfig = *r.Validation
	}

	// Copy top-level Constraints
	validation.Constraints = r.Constraints

	// Copy ComponentRefs
	validation.ComponentRefs = r.ComponentRefs

	// Copy Criteria
	if r.Criteria != nil {
		validation.Criteria = *r.Criteria
	}

	return validation
}
