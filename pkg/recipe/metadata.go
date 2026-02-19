// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

// Package recipe provides recipe building and matching functionality.
package recipe

import (
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/NVIDIA/eidos/pkg/errors"
)

// RecipeMetadataKind is the kind value for RecipeMetadata resources.
const RecipeMetadataKind = "RecipeMetadata"

// RecipeResultKind is the kind value for RecipeResult resources.
const RecipeResultKind = "RecipeResult"

// RecipeAPIVersion is the API version for recipe metadata and result resources.
const RecipeAPIVersion = "eidos.nvidia.com/v1alpha1"

// ComponentType represents the type of component deployment.
type ComponentType string

// ComponentType constants for supported deployment types.
const (
	ComponentTypeHelm      ComponentType = "Helm"
	ComponentTypeKustomize ComponentType = "Kustomize"
)

// Constraint represents a deployment constraint/assumption.
type Constraint struct {
	// Name is the constraint identifier (e.g., "k8s", "worker-os").
	Name string `json:"name" yaml:"name"`

	// Value is the constraint expression (e.g., ">= 1.30", "ubuntu").
	Value string `json:"value" yaml:"value"`

	// Severity indicates the constraint severity ("error" or "warning").
	Severity string `json:"severity,omitempty" yaml:"severity,omitempty"`

	// Remediation provides actionable guidance for fixing failed constraints.
	Remediation string `json:"remediation,omitempty" yaml:"remediation,omitempty"`

	// Unit specifies the unit for numeric constraints (e.g., "GB/s").
	Unit string `json:"unit,omitempty" yaml:"unit,omitempty"`
}

// ComponentRef represents a reference to a deployable component.
type ComponentRef struct {
	// Name is the unique identifier for this component.
	Name string `json:"name" yaml:"name"`

	// Namespace is the Kubernetes namespace for deploying this component.
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`

	// Chart is the Helm chart name (e.g., "gpu-operator").
	Chart string `json:"chart,omitempty" yaml:"chart,omitempty"`

	// Type is the deployment type (Helm, Kustomize).
	Type ComponentType `json:"type" yaml:"type"`

	// Source is the repository URL or OCI reference.
	Source string `json:"source" yaml:"source"`

	// Version is the chart/component version (for Helm).
	Version string `json:"version,omitempty" yaml:"version,omitempty"`

	// Tag is the image/resource tag (for Kustomize).
	Tag string `json:"tag,omitempty" yaml:"tag,omitempty"`

	// ValuesFile is the path to the values file (relative to data directory).
	ValuesFile string `json:"valuesFile,omitempty" yaml:"valuesFile,omitempty"`

	// Overrides contains inline values that override those from ValuesFile.
	// Merge order: base values → ValuesFile → Overrides (highest precedence).
	Overrides map[string]any `json:"overrides,omitempty" yaml:"overrides,omitempty"`

	// Patches is a list of patch files to apply (for Kustomize).
	Patches []string `json:"patches,omitempty" yaml:"patches,omitempty"`

	// DependencyRefs is a list of component names this component depends on.
	DependencyRefs []string `json:"dependencyRefs,omitempty" yaml:"dependencyRefs,omitempty"`

	// ManifestFiles lists manifest files to include in the component bundle.
	// Paths are relative to the data directory.
	// Example: ["components/gpu-operator/manifests/dcgm-exporter.yaml"]
	ManifestFiles []string `json:"manifestFiles,omitempty" yaml:"manifestFiles,omitempty"`

	// Path is the path within the repository to the kustomization (for Kustomize).
	Path string `json:"path,omitempty" yaml:"path,omitempty"`

	// Cleanup indicates whether to uninstall this component after validation.
	// Used for validation infrastructure components (e.g., nccl-doctor).
	Cleanup bool `json:"cleanup,omitempty" yaml:"cleanup,omitempty"`

	// ExpectedResources lists Kubernetes resources that should exist after deployment.
	// Used by deployment phase validation to verify component health.
	ExpectedResources []ExpectedResource `json:"expectedResources,omitempty" yaml:"expectedResources,omitempty"`
}

// ExpectedResource represents a Kubernetes resource that should exist after deployment.
type ExpectedResource struct {
	// Kind is the resource kind (e.g., "Deployment", "DaemonSet").
	Kind string `json:"kind" yaml:"kind"`

	// Name is the resource name.
	Name string `json:"name" yaml:"name"`

	// Namespace is the resource namespace (optional for cluster-scoped resources).
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
}

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

// ApplyRegistryDefaults fills in ComponentRef fields from ComponentConfig defaults.
// This applies registry defaults for fields that are not already set in the ComponentRef.
func (ref *ComponentRef) ApplyRegistryDefaults(config *ComponentConfig) {
	if config == nil {
		return
	}

	// Set type from config if not already set
	if ref.Type == "" {
		ref.Type = config.GetType()
	}

	switch ref.Type {
	case ComponentTypeHelm:
		// Apply Helm defaults
		if ref.Source == "" && config.Helm.DefaultRepository != "" {
			ref.Source = config.Helm.DefaultRepository
		}
		if ref.Version == "" && config.Helm.DefaultVersion != "" {
			ref.Version = config.Helm.DefaultVersion
		}
		if ref.Namespace == "" && config.Helm.DefaultNamespace != "" {
			ref.Namespace = config.Helm.DefaultNamespace
		}
		if ref.Chart == "" && config.Helm.DefaultChart != "" {
			chart := config.Helm.DefaultChart
			if idx := strings.LastIndex(chart, "/"); idx >= 0 {
				chart = chart[idx+1:]
			}
			ref.Chart = chart
		}
	case ComponentTypeKustomize:
		// Apply Kustomize defaults
		if ref.Source == "" && config.Kustomize.DefaultSource != "" {
			ref.Source = config.Kustomize.DefaultSource
		}
		if ref.Tag == "" && config.Kustomize.DefaultTag != "" {
			ref.Tag = config.Kustomize.DefaultTag
		}
		if ref.Path == "" && config.Kustomize.DefaultPath != "" {
			ref.Path = config.Kustomize.DefaultPath
		}
	}
}

// RecipeMetadataSpec contains the specification for a recipe.
type RecipeMetadataSpec struct {
	// Base is the name of the parent recipe to inherit from.
	// If empty, the recipe inherits from "base" (the root base.yaml).
	// This enables multi-level inheritance chains like:
	//   base → eks → eks-training → h100-eks-training
	Base string `json:"base,omitempty" yaml:"base,omitempty"`

	// Criteria defines when this recipe/overlay applies.
	// Only present in overlay files, not in base.
	Criteria *Criteria `json:"criteria,omitempty" yaml:"criteria,omitempty"`

	// Constraints are deployment assumptions/requirements.
	Constraints []Constraint `json:"constraints,omitempty" yaml:"constraints,omitempty"`

	// ComponentRefs is the list of components to deploy.
	ComponentRefs []ComponentRef `json:"componentRefs,omitempty" yaml:"componentRefs,omitempty"`

	// Validation defines multi-phase validation configuration.
	// Presence of a phase implies it is enabled.
	Validation *ValidationConfig `json:"validation,omitempty" yaml:"validation,omitempty"`
}

// RecipeMetadataHeader contains the Kubernetes-style header fields.
type RecipeMetadataHeader struct {
	// Kind is always "RecipeMetadata".
	Kind string `json:"kind" yaml:"kind"`

	// APIVersion is the API version (e.g., "eidos.nvidia.com/v1alpha1").
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`

	// Metadata contains the name and other metadata.
	Metadata struct {
		Name string `json:"name" yaml:"name"`
	} `json:"metadata" yaml:"metadata"`
}

// RecipeMetadata represents a recipe definition (base or overlay).
type RecipeMetadata struct {
	RecipeMetadataHeader `json:",inline" yaml:",inline"`

	// Spec contains the recipe specification.
	Spec RecipeMetadataSpec `json:"spec" yaml:"spec"`
}

// ConstraintWarning represents a warning about an overlay that matched criteria
// but was excluded due to failing constraint validation against the snapshot.
type ConstraintWarning struct {
	// Overlay is the name of the overlay that was excluded.
	Overlay string `json:"overlay" yaml:"overlay"`

	// Constraint is the name of the constraint that failed.
	Constraint string `json:"constraint" yaml:"constraint"`

	// Expected is the expected constraint value.
	Expected string `json:"expected" yaml:"expected"`

	// Actual is the actual value from the snapshot (if found).
	Actual string `json:"actual,omitempty" yaml:"actual,omitempty"`

	// Reason explains why the constraint evaluation resulted in exclusion.
	Reason string `json:"reason" yaml:"reason"`
}

// RecipeResult represents the final merged recipe output.
type RecipeResult struct {
	// Kind is always "RecipeResult".
	Kind string `json:"kind" yaml:"kind"`

	// APIVersion is the API version.
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`

	// Metadata contains result metadata.
	Metadata struct {
		// Version is the recipe version (CLI version that generated this recipe).
		Version string `json:"version,omitempty" yaml:"version,omitempty"`

		// AppliedOverlays lists the overlay names in order of application.
		AppliedOverlays []string `json:"appliedOverlays,omitempty" yaml:"appliedOverlays,omitempty"`

		// ExcludedOverlays lists overlays that matched criteria but were excluded
		// due to failing constraint validation against the snapshot.
		// Only populated when a snapshot is provided during recipe generation.
		ExcludedOverlays []string `json:"excludedOverlays,omitempty" yaml:"excludedOverlays,omitempty"`

		// ConstraintWarnings contains details about why specific overlays were excluded.
		// Helps users understand why certain environment-specific configurations
		// were not applied and what would need to change to include them.
		ConstraintWarnings []ConstraintWarning `json:"constraintWarnings,omitempty" yaml:"constraintWarnings,omitempty"`
	} `json:"metadata" yaml:"metadata"`

	// Criteria is the input criteria used to generate this result.
	Criteria *Criteria `json:"criteria" yaml:"criteria"`

	// Constraints is the merged list of constraints.
	Constraints []Constraint `json:"constraints,omitempty" yaml:"constraints,omitempty"`

	// ComponentRefs is the merged list of components.
	ComponentRefs []ComponentRef `json:"componentRefs" yaml:"componentRefs"`

	// DeploymentOrder is the topologically sorted component names for deployment.
	// Components should be deployed in this order to satisfy dependencies.
	DeploymentOrder []string `json:"deploymentOrder" yaml:"deploymentOrder"`

	// Validation defines multi-phase validation configuration.
	// Inherited from recipe metadata during merging.
	Validation *ValidationConfig `json:"validation,omitempty" yaml:"validation,omitempty"`
}

// Merge merges another RecipeMetadataSpec into this one.
// The other spec takes precedence for conflicts.
func (s *RecipeMetadataSpec) Merge(other *RecipeMetadataSpec) {
	if other == nil {
		return
	}

	// Merge constraints - other takes precedence for same name
	constraintMap := make(map[string]Constraint)
	for _, c := range s.Constraints {
		constraintMap[c.Name] = c
	}
	for _, c := range other.Constraints {
		constraintMap[c.Name] = c
	}
	s.Constraints = make([]Constraint, 0, len(constraintMap))
	for _, c := range constraintMap {
		s.Constraints = append(s.Constraints, c)
	}
	// Sort constraints by name for deterministic output
	sort.Slice(s.Constraints, func(i, j int) bool {
		return s.Constraints[i].Name < s.Constraints[j].Name
	})

	// Merge componentRefs - overlay fields take precedence, but inherit missing from base
	componentMap := make(map[string]ComponentRef)
	for _, c := range s.ComponentRefs {
		componentMap[c.Name] = c
	}
	for _, overlay := range other.ComponentRefs {
		if base, exists := componentMap[overlay.Name]; exists {
			// Merge overlay into base - overlay takes precedence for non-empty fields
			componentMap[overlay.Name] = mergeComponentRef(base, overlay)
		} else {
			// New component from overlay
			componentMap[overlay.Name] = overlay
		}
	}
	s.ComponentRefs = make([]ComponentRef, 0, len(componentMap))
	for _, c := range componentMap {
		s.ComponentRefs = append(s.ComponentRefs, c)
	}
	// Sort components by name for deterministic output
	sort.Slice(s.ComponentRefs, func(i, j int) bool {
		return s.ComponentRefs[i].Name < s.ComponentRefs[j].Name
	})

	// Merge validation config - overlay phases take precedence
	if other.Validation != nil {
		if s.Validation == nil {
			s.Validation = other.Validation
		} else {
			if other.Validation.Readiness != nil {
				s.Validation.Readiness = other.Validation.Readiness
			}
			if other.Validation.Deployment != nil {
				s.Validation.Deployment = other.Validation.Deployment
			}
			if other.Validation.Performance != nil {
				s.Validation.Performance = other.Validation.Performance
			}
			if other.Validation.Conformance != nil {
				s.Validation.Conformance = other.Validation.Conformance
			}
		}
	}
}

// mergeComponentRef merges overlay into base, with overlay taking precedence
// for non-empty fields. Empty/zero fields in overlay inherit from base.
func mergeComponentRef(base, overlay ComponentRef) ComponentRef {
	result := base // Start with base values

	// Namespace: overlay takes precedence if set
	if overlay.Namespace != "" {
		result.Namespace = overlay.Namespace
	}

	// Chart: overlay takes precedence if set
	if overlay.Chart != "" {
		result.Chart = overlay.Chart
	}

	// Type: overlay takes precedence if set
	if overlay.Type != "" {
		result.Type = overlay.Type
	}

	// Source: overlay takes precedence if set
	if overlay.Source != "" {
		result.Source = overlay.Source
	}

	// Version: overlay takes precedence if set
	if overlay.Version != "" {
		result.Version = overlay.Version
	}

	// Tag: overlay takes precedence if set
	if overlay.Tag != "" {
		result.Tag = overlay.Tag
	}

	// ValuesFile: overlay takes precedence if set
	if overlay.ValuesFile != "" {
		result.ValuesFile = overlay.ValuesFile
	}

	// Overrides: merge maps, overlay takes precedence
	if len(overlay.Overrides) > 0 {
		if result.Overrides == nil {
			result.Overrides = make(map[string]any)
		}
		maps.Copy(result.Overrides, overlay.Overrides)
	}

	// Patches: overlay replaces if set
	if len(overlay.Patches) > 0 {
		result.Patches = overlay.Patches
	}

	// DependencyRefs: overlay replaces if set
	if len(overlay.DependencyRefs) > 0 {
		result.DependencyRefs = overlay.DependencyRefs
	}

	// ManifestFiles: additive merge (base + overlay, deduplicated)
	if len(overlay.ManifestFiles) > 0 {
		seen := make(map[string]bool)
		for _, f := range result.ManifestFiles {
			seen[f] = true
		}
		for _, f := range overlay.ManifestFiles {
			if !seen[f] {
				result.ManifestFiles = append(result.ManifestFiles, f)
			}
		}
	}

	// Path: overlay takes precedence if set (for Kustomize)
	if overlay.Path != "" {
		result.Path = overlay.Path
	}

	// Cleanup: overlay takes precedence if true
	if overlay.Cleanup {
		result.Cleanup = overlay.Cleanup
	}

	// ExpectedResources: overlay replaces if set
	if len(overlay.ExpectedResources) > 0 {
		result.ExpectedResources = overlay.ExpectedResources
	}

	return result
}

// ValidateDependencies validates that all dependencyRefs reference existing components.
// Returns an error if any dependency is missing or if there are circular dependencies.
func (s *RecipeMetadataSpec) ValidateDependencies() error {
	// Build a set of known component names
	known := make(map[string]bool)
	for _, c := range s.ComponentRefs {
		known[c.Name] = true
	}

	// Check all dependencyRefs point to known components
	for _, c := range s.ComponentRefs {
		for _, dep := range c.DependencyRefs {
			if !known[dep] {
				return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("component %q references unknown dependency %q", c.Name, dep))
			}
		}
	}

	// Check for circular dependencies
	if err := s.detectCycles(); err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "dependency validation failed", err)
	}

	return nil
}

// detectCycles uses DFS to detect circular dependencies.
func (s *RecipeMetadataSpec) detectCycles() error {
	// Build adjacency list
	deps := make(map[string][]string)
	for _, c := range s.ComponentRefs {
		deps[c.Name] = c.DependencyRefs
	}

	// Track visited nodes and recursion stack
	visited := make(map[string]bool)
	recStack := make(map[string]bool)
	var path []string

	var dfs func(node string) error
	dfs = func(node string) error {
		visited[node] = true
		recStack[node] = true
		path = append(path, node)

		for _, neighbor := range deps[node] {
			if !visited[neighbor] {
				if err := dfs(neighbor); err != nil {
					return err
				}
			} else if recStack[neighbor] {
				// Found a cycle - build the cycle path
				cycleStart := -1
				for i, n := range path {
					if n == neighbor {
						cycleStart = i
						break
					}
				}
				// Build cycle path: copy to avoid modifying original path slice
				cyclePath := make([]string, len(path)-cycleStart+1)
				copy(cyclePath, path[cycleStart:])
				cyclePath[len(cyclePath)-1] = neighbor
				return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("circular dependency detected: %v", cyclePath))
			}
		}

		path = path[:len(path)-1]
		recStack[node] = false
		return nil
	}

	// Run DFS from each unvisited node
	for _, c := range s.ComponentRefs {
		if !visited[c.Name] {
			if err := dfs(c.Name); err != nil {
				return err
			}
		}
	}

	return nil
}

// TopologicalSort returns components in dependency order (dependencies first).
// Components with no dependencies come first, then components that depend only
// on already-listed components, etc.
func (s *RecipeMetadataSpec) TopologicalSort() ([]string, error) {
	inDegree := make(map[string]int, len(s.ComponentRefs))
	for _, c := range s.ComponentRefs {
		inDegree[c.Name] = len(c.DependencyRefs)
	}

	// Kahn's algorithm
	// https://www.geeksforgeeks.org/dsa/topological-sorting-indegree-based-solution/
	queue := make([]string, 0, len(inDegree))
	for name, degree := range inDegree {
		if degree == 0 {
			queue = append(queue, name)
		}
	}
	// Sort queue for deterministic output
	sort.Strings(queue)

	result := make([]string, 0, len(s.ComponentRefs))
	dependents := make(map[string][]string) // dep -> list of components that depend on it
	for _, c := range s.ComponentRefs {
		for _, dep := range c.DependencyRefs {
			dependents[dep] = append(dependents[dep], c.Name)
		}
	}

	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		result = append(result, node)

		for _, dependent := range dependents[node] {
			inDegree[dependent]--
			if inDegree[dependent] == 0 {
				queue = append(queue, dependent)
			}
		}
		sort.Strings(queue)
	}

	// Check if all nodes were processed (no cycles)
	if len(result) != len(s.ComponentRefs) {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "cannot determine deployment order: circular dependencies exist")
	}

	return result, nil
}
