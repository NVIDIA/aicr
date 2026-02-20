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

package recipe

import (
	"fmt"
	"slices"
	"sync"

	"github.com/NVIDIA/eidos/pkg/errors"
	"gopkg.in/yaml.v3"
)

// ComponentRegistry holds the declarative configuration for all components.
// This is loaded from embedded recipe data (recipes/registry.yaml) at startup.
type ComponentRegistry struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Components []ComponentConfig `yaml:"components"`

	// Index for fast lookup by name (populated after loading)
	byName map[string]*ComponentConfig
}

// ComponentConfig defines the bundler configuration for a component.
// This replaces the per-component Go packages with declarative YAML.
type ComponentConfig struct {
	// Name is the component identifier used in recipes (e.g., "gpu-operator").
	Name string `yaml:"name"`

	// DisplayName is the human-readable name used in templates and output.
	DisplayName string `yaml:"displayName"`

	// ValueOverrideKeys are alternative keys for --set flag matching.
	// Example: ["gpuoperator"] allows --set gpuoperator:key=value
	ValueOverrideKeys []string `yaml:"valueOverrideKeys,omitempty"`

	// Helm contains default Helm chart settings.
	Helm HelmConfig `yaml:"helm,omitempty"`

	// Kustomize contains default Kustomize settings.
	Kustomize KustomizeConfig `yaml:"kustomize,omitempty"`

	// NodeScheduling defines paths for injecting node selectors and tolerations.
	NodeScheduling NodeSchedulingConfig `yaml:"nodeScheduling,omitempty"`
}

// HelmConfig contains default Helm chart settings for a component.
type HelmConfig struct {
	// DefaultRepository is the default Helm repository URL.
	DefaultRepository string `yaml:"defaultRepository,omitempty"`

	// DefaultChart is the chart name (e.g., "nvidia/gpu-operator").
	DefaultChart string `yaml:"defaultChart,omitempty"`

	// DefaultVersion is the default chart version if not specified in recipe.
	DefaultVersion string `yaml:"defaultVersion,omitempty"`

	// DefaultNamespace is the Kubernetes namespace for deploying this component.
	DefaultNamespace string `yaml:"defaultNamespace,omitempty"`
}

// KustomizeConfig contains default Kustomize settings for a component.
type KustomizeConfig struct {
	// DefaultSource is the default Git repository or OCI reference.
	DefaultSource string `yaml:"defaultSource,omitempty"`

	// DefaultPath is the path within the repository to the kustomization.
	DefaultPath string `yaml:"defaultPath,omitempty"`

	// DefaultTag is the default Git tag, branch, or commit.
	DefaultTag string `yaml:"defaultTag,omitempty"`
}

// NodeSchedulingConfig defines paths for node scheduling injection.
type NodeSchedulingConfig struct {
	// System defines paths for system component scheduling.
	System SchedulingPaths `yaml:"system,omitempty"`

	// Accelerated defines paths for GPU/accelerated node scheduling.
	Accelerated SchedulingPaths `yaml:"accelerated,omitempty"`
}

// SchedulingPaths holds the Helm value paths for node scheduling.
type SchedulingPaths struct {
	// NodeSelectorPaths are paths where node selectors are injected.
	NodeSelectorPaths []string `yaml:"nodeSelectorPaths,omitempty"`

	// TolerationPaths are paths where tolerations are injected.
	TolerationPaths []string `yaml:"tolerationPaths,omitempty"`

	// WorkloadSelectorPaths are paths where workload selectors are injected.
	// Used for components like skyhook-customizations to prevent eviction of running workloads.
	WorkloadSelectorPaths []string `yaml:"workloadSelectorPaths,omitempty"`

	// TaintPaths are paths where taints are injected as structured objects.
	TaintPaths []string `yaml:"taintPaths,omitempty"`

	// TaintStrPaths are paths where taints are injected as strings (format: key=value:effect or key:effect).
	TaintStrPaths []string `yaml:"taintStrPaths,omitempty"`
}

// Global component registry (loaded once, thread-safe access)
var (
	globalRegistry     *ComponentRegistry
	globalRegistryOnce sync.Once
	globalRegistryErr  error
)

// GetComponentRegistry returns the global component registry.
// The registry is loaded once from embedded data and cached.
// Returns an error if the registry file cannot be loaded or parsed.
func GetComponentRegistry() (*ComponentRegistry, error) {
	globalRegistryOnce.Do(func() {
		globalRegistry, globalRegistryErr = loadComponentRegistry()
	})
	return globalRegistry, globalRegistryErr
}

// MustGetComponentRegistry returns the global component registry or panics.
// Use this in init() functions where the registry must be available.
func MustGetComponentRegistry() *ComponentRegistry {
	reg, err := GetComponentRegistry()
	if err != nil {
		panic(fmt.Sprintf("failed to load component registry: %v", err))
	}
	return reg
}

// loadComponentRegistry loads the component registry from the data provider.
func loadComponentRegistry() (*ComponentRegistry, error) {
	provider := GetDataProvider()
	data, err := provider.ReadFile("registry.yaml")
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to read registry.yaml", err)
	}

	var registry ComponentRegistry
	if err := yaml.Unmarshal(data, &registry); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to parse registry.yaml", err)
	}

	// Build index for fast lookup
	registry.byName = make(map[string]*ComponentConfig, len(registry.Components))
	for i := range registry.Components {
		comp := &registry.Components[i]
		registry.byName[comp.Name] = comp
	}

	return &registry, nil
}

// Get returns the component configuration by name.
// Returns nil if the component is not found.
func (r *ComponentRegistry) Get(name string) *ComponentConfig {
	if r == nil || r.byName == nil {
		return nil
	}
	return r.byName[name]
}

// GetByOverrideKey returns the component configuration by value override key.
// This is used for matching --set flags like --set gpuoperator:key=value.
// Returns nil if no component matches the key.
func (r *ComponentRegistry) GetByOverrideKey(key string) *ComponentConfig {
	if r == nil {
		return nil
	}
	for i := range r.Components {
		comp := &r.Components[i]
		// Check the component name first
		if comp.Name == key {
			return comp
		}
		// Check alternative override keys
		if slices.Contains(comp.ValueOverrideKeys, key) {
			return comp
		}
	}
	return nil
}

// Names returns all component names in the registry.
func (r *ComponentRegistry) Names() []string {
	if r == nil {
		return nil
	}
	names := make([]string, len(r.Components))
	for i, comp := range r.Components {
		names[i] = comp.Name
	}
	return names
}

// Count returns the number of components in the registry.
func (r *ComponentRegistry) Count() int {
	if r == nil {
		return 0
	}
	return len(r.Components)
}

// Validate checks the component registry for errors.
// Returns a slice of validation errors (empty if valid).
func (r *ComponentRegistry) Validate() []error {
	if r == nil {
		return []error{errors.New(errors.ErrCodeInvalidRequest, "registry is nil")}
	}

	var errs []error

	// Check for required fields
	for i, comp := range r.Components {
		if comp.Name == "" {
			errs = append(errs, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("component[%d]: name is required", i)))
		}
		if comp.DisplayName == "" {
			errs = append(errs, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("component[%d] (%s): displayName is required", i, comp.Name)))
		}
	}

	// Check for duplicate names
	seen := make(map[string]bool)
	for _, comp := range r.Components {
		if comp.Name != "" {
			if seen[comp.Name] {
				errs = append(errs, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("duplicate component name: %s", comp.Name)))
			}
			seen[comp.Name] = true
		}
	}

	// Check for duplicate override keys
	overrideKeys := make(map[string]string) // key -> component name
	for _, comp := range r.Components {
		for _, key := range comp.ValueOverrideKeys {
			if existing, ok := overrideKeys[key]; ok {
				errs = append(errs, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("duplicate valueOverrideKey %q: used by both %s and %s", key, existing, comp.Name)))
			}
			overrideKeys[key] = comp.Name
		}
	}

	// Check for mutually exclusive helm/kustomize configuration
	for i, comp := range r.Components {
		hasHelm := comp.Helm.DefaultRepository != "" || comp.Helm.DefaultChart != ""
		hasKustomize := comp.Kustomize.DefaultSource != ""

		if hasHelm && hasKustomize {
			errs = append(errs, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("component[%d] (%s): cannot have both helm and kustomize configuration", i, comp.Name)))
		}
	}

	return errs
}

// GetSystemNodeSelectorPaths returns all system node selector paths for a component.
func (c *ComponentConfig) GetSystemNodeSelectorPaths() []string {
	if c == nil {
		return nil
	}
	return c.NodeScheduling.System.NodeSelectorPaths
}

// GetSystemTolerationPaths returns all system toleration paths for a component.
func (c *ComponentConfig) GetSystemTolerationPaths() []string {
	if c == nil {
		return nil
	}
	return c.NodeScheduling.System.TolerationPaths
}

// GetAcceleratedNodeSelectorPaths returns all accelerated node selector paths for a component.
func (c *ComponentConfig) GetAcceleratedNodeSelectorPaths() []string {
	if c == nil {
		return nil
	}
	return c.NodeScheduling.Accelerated.NodeSelectorPaths
}

// GetAcceleratedTolerationPaths returns all accelerated toleration paths for a component.
func (c *ComponentConfig) GetAcceleratedTolerationPaths() []string {
	if c == nil {
		return nil
	}
	return c.NodeScheduling.Accelerated.TolerationPaths
}

// GetAcceleratedWorkloadSelectorPaths returns all accelerated workload selector paths for a component.
func (c *ComponentConfig) GetAcceleratedWorkloadSelectorPaths() []string {
	if c == nil {
		return nil
	}
	return c.NodeScheduling.Accelerated.WorkloadSelectorPaths
}

// GetAcceleratedTaintStrPaths returns all accelerated taint string paths for a component.
func (c *ComponentConfig) GetAcceleratedTaintStrPaths() []string {
	if c == nil {
		return nil
	}
	return c.NodeScheduling.Accelerated.TaintStrPaths
}

// GetType returns the component deployment type based on which config is present.
// Returns ComponentTypeKustomize if Kustomize.DefaultSource is set,
// otherwise returns ComponentTypeHelm (the default).
func (c *ComponentConfig) GetType() ComponentType {
	if c == nil {
		return ComponentTypeHelm
	}
	if c.Kustomize.DefaultSource != "" {
		return ComponentTypeKustomize
	}
	return ComponentTypeHelm
}
