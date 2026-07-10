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

package v1

import "time"

const (
	// CatalogAPIVersion is the supported catalog API version.
	CatalogAPIVersion = "validator.nvidia.com/v1alpha1"

	// CatalogKind is the supported catalog kind.
	CatalogKind = "ValidatorCatalog"
)

// Phase represents a validation phase.
type Phase string

const (
	// PhaseDeployment is the deployment validation phase.
	PhaseDeployment Phase = "deployment"

	// PhasePerformance is the performance validation phase.
	PhasePerformance Phase = "performance"

	// PhaseConformance is the conformance validation phase.
	PhaseConformance Phase = "conformance"
)

// ValidatorCatalog is the top-level catalog document.
// Supports both standalone file usage (with full metadata) and embedded usage in CRs (metadata omitted).
//
// Standalone usage (catalog.yaml):
//
//	apiVersion: validator.nvidia.com/v1alpha1
//	kind: ValidatorCatalog
//	metadata:
//	  name: default
//	  version: 1.0.0
//	validators: [...]
//
// Embedded usage (in a CR):
//
//	spec:
//	  catalog:
//	    validators: [...]
type ValidatorCatalog struct {
	// APIVersion is the API version (optional, for standalone resource usage).
	APIVersion string `json:"apiVersion,omitempty" yaml:"apiVersion,omitempty"`

	// Kind is always "ValidatorCatalog" (optional, for standalone resource usage).
	Kind string `json:"kind,omitempty" yaml:"kind,omitempty"`

	// Metadata contains catalog metadata (optional, for standalone resource usage).
	Metadata *CatalogMetadata `json:"metadata,omitempty" yaml:"metadata,omitempty"`

	// Validators is the list of validator entries (required).
	Validators []ValidatorEntry `json:"validators" yaml:"validators"`
}

// CatalogMetadata contains catalog-level metadata.
type CatalogMetadata struct {
	Name    string `json:"name" yaml:"name"`
	Version string `json:"version" yaml:"version"` // SemVer
}

// ValidatorEntry defines a single validator container.
type ValidatorEntry struct {
	// Name is the unique identifier for this validator, used in Job names.
	Name string `json:"name" yaml:"name"`

	// Phase is the validation phase: "deployment", "performance", or "conformance".
	Phase string `json:"phase" yaml:"phase"`

	// Description is a human-readable description of what this validator checks.
	Description string `json:"description" yaml:"description"`

	// Image is the OCI image reference for the validator container.
	Image string `json:"image" yaml:"image"`

	// Timeout is the maximum execution time for this validator.
	// Maps to Job activeDeadlineSeconds.
	Timeout time.Duration `json:"timeout" yaml:"timeout"`

	// Args are the container arguments.
	Args []string `json:"args,omitempty" yaml:"args,omitempty"`

	// Env are environment variables to set in the container.
	Env []EnvVar `json:"env,omitempty" yaml:"env,omitempty"`

	// Resources specifies container resource requests/limits.
	// If nil, defaults from pkg/defaults are used.
	Resources *ResourceRequirements `json:"resources,omitempty" yaml:"resources,omitempty"`

	// DependencyAffinity declares co-location preferences for the orchestrator
	// pod of this validator. Each entry references a recipe component by name
	// (componentRef) and a label selector matching that component's pods.
	// The deployer resolves the componentRef to a namespace from the resolved
	// recipe at spawn time and emits a podAffinity term on the orchestrator
	// Pod spec. "required" entries hard-fail the run when the referenced
	// component is absent from the recipe; "preferred" entries (default) emit
	// a structured warning and proceed with no affinity term for that
	// dependency.
	//
	// Motivation: ai-service-metrics queries Prometheus over a Service. On
	// clusters with asymmetric pod-to-pod network reachability (e.g.,
	// multi-Security-Group DGXC EKS), the orchestrator must run on a node
	// that can reach the Prometheus pod. Co-locating with the Prometheus pod
	// makes the dial loopback / same-network and removes the dependency on
	// cluster network topology. See https://github.com/NVIDIA/aicr/issues/933.
	DependencyAffinity []DependencyAffinity `json:"dependencyAffinity,omitempty" yaml:"dependencyAffinity,omitempty"`
}

// ResourceRequirements defines CPU and memory for a validator container.
type ResourceRequirements struct {
	CPU    string `json:"cpu,omitempty" yaml:"cpu,omitempty"`
	Memory string `json:"memory,omitempty" yaml:"memory,omitempty"`
}

// EnvVar is a name/value pair for container environment variables.
type EnvVar struct {
	Name  string `json:"name" yaml:"name"`
	Value string `json:"value" yaml:"value"`
}

// ForPhase returns validators filtered by phase.
func (c *ValidatorCatalog) ForPhase(phase Phase) []ValidatorEntry {
	var result []ValidatorEntry
	for _, v := range c.Validators {
		if v.Phase == string(phase) {
			result = append(result, v)
		}
	}
	return result
}

// checksForPhase returns the check names declared for phase in the validation
// input, or nil when the validation has no configuration for that phase.
func checksForPhase(phase Phase, validationInput *ValidationInput) []string {
	if validationInput == nil {
		return nil
	}
	switch phase {
	case PhaseDeployment:
		if validationInput.Config.Deployment != nil {
			return validationInput.Config.Deployment.Checks
		}
	case PhasePerformance:
		if validationInput.Config.Performance != nil {
			return validationInput.Config.Performance.Checks
		}
	case PhaseConformance:
		if validationInput.Config.Conformance != nil {
			return validationInput.Config.Conformance.Checks
		}
	}
	return nil
}

// FilterEntriesByValidation filters catalog entries based on the validation's declared checks for the given phase.
// Returns nil if the validation has no phase configuration or no checks declared.
//
// Declared check names that match no entry are dropped here; use
// UnmatchedChecks against the full catalog to surface those so a typo'd or
// misplaced check name does not silently produce an empty (spuriously passing)
// phase.
func FilterEntriesByValidation(entries []ValidatorEntry, phase Phase, validationInput *ValidationInput) []ValidatorEntry {
	phaseChecks := checksForPhase(phase, validationInput)

	// No checks declared for this phase → skip it.
	if len(phaseChecks) == 0 {
		return nil
	}

	// Build set for O(1) lookup.
	allowed := make(map[string]bool, len(phaseChecks))
	for _, name := range phaseChecks {
		allowed[name] = true
	}

	filtered := make([]ValidatorEntry, 0, len(phaseChecks))
	for _, entry := range entries {
		if allowed[entry.Name] {
			filtered = append(filtered, entry)
		}
	}

	return filtered
}

// UnmatchedCheck describes a declared check name that matched no catalog entry
// in the phase it was declared under.
type UnmatchedCheck struct {
	// Name is the declared check name that matched nothing in its phase.
	Name string

	// Phase is the phase the name was declared under.
	Phase Phase

	// OtherPhase is non-empty when the name exists in the catalog under a
	// different phase — a likely misplacement rather than a typo.
	OtherPhase Phase
}

// UnmatchedChecks returns the check names declared for phase that match no
// catalog entry in that phase. It is the complement of
// FilterEntriesByValidation: the names dropped on the floor. For each unmatched
// name it records whether the same name exists under a different phase, which
// distinguishes a misplaced check from an outright typo.
//
// The receiver is the full catalog (all phases) so cross-phase matches can be
// detected. Returns nil when every declared check for the phase resolves.
func (c *ValidatorCatalog) UnmatchedChecks(phase Phase, validationInput *ValidationInput) []UnmatchedCheck {
	if c == nil {
		return nil
	}

	phaseChecks := checksForPhase(phase, validationInput)
	if len(phaseChecks) == 0 {
		return nil
	}

	// Index catalog entry names by phase for O(1) lookup.
	namePhases := make(map[string]Phase, len(c.Validators))
	inPhase := make(map[string]bool)
	for _, v := range c.Validators {
		if Phase(v.Phase) == phase {
			inPhase[v.Name] = true
			continue
		}
		// Record one other phase per name; a name matching the target phase
		// is never treated as "elsewhere".
		if _, ok := namePhases[v.Name]; !ok {
			namePhases[v.Name] = Phase(v.Phase)
		}
	}

	var unmatched []UnmatchedCheck
	seen := make(map[string]bool, len(phaseChecks))
	for _, name := range phaseChecks {
		if inPhase[name] || seen[name] {
			continue
		}
		seen[name] = true
		unmatched = append(unmatched, UnmatchedCheck{
			Name:       name,
			Phase:      phase,
			OtherPhase: namePhases[name],
		})
	}

	return unmatched
}
