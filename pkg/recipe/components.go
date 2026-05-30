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

import (
	"fmt"
	"slices"
	"sync"

	"github.com/NVIDIA/aicr/pkg/errors"
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

	PodScheduling PodSchedulingConfig `yaml:"podScheduling,omitempty"`

	// StorageClassPaths are Helm value paths where the storage class name is injected.
	// When --storage-class is provided at bundle time, the value is written to each path.
	StorageClassPaths []string `yaml:"storageClassPaths,omitempty"`

	// Validations defines component-specific validation checks.
	Validations []ComponentValidationConfig `yaml:"validations,omitempty"`

	// HealthCheck defines custom health check configuration for this component.
	HealthCheck HealthCheckConfig `yaml:"healthCheck,omitempty"`

	// GKECriticalPriority signals that the component's default chart manifests
	// include pods with `priorityClassName: system-node-critical` or
	// `system-cluster-critical`. When true and the recipe's
	// `criteria.service` is "gke", the bundler synthesizes a permissive
	// ResourceQuota into the component's namespace (PreManifestFiles phase)
	// so GKE Standard's ResourceQuota admission plugin admits the pods.
	//
	// GKE Standard ships a kube-system ResourceQuota scoped to
	// `system-*-critical` PriorityClasses; per the Kubernetes spec, once
	// any quota in the cluster scopes by PriorityClass for those values,
	// pods that request a matching priority class can only be created in
	// namespaces that have a matching quota. Other services (EKS, AKS,
	// OKE, bare-metal) do not ship this default and are unaffected.
	//
	// See https://github.com/NVIDIA/aicr/issues/915.
	GKECriticalPriority bool `yaml:"gkeCriticalPriority,omitempty"`

	// HasSelfRefCRDs signals that the component's chart contains both
	// a CRD (shipped under `crds/` or via a CRDs subchart) AND a
	// template that creates a CR of that kind in the SAME release.
	// helm-diff's render pass fails on such charts on a fresh cluster:
	// it renders templates and validates against the live REST mapper,
	// but the mapper does not yet know the CRD because helm only
	// applies `crds/` resources during `helm install` (not during
	// render). This flag instructs the helmfile deployer to emit
	// `disableValidation: true` on the release, telling helm-diff to
	// skip the mapper check for that release only.
	//
	// Cross-chart CRD ordering — where one chart ships CRDs that
	// another chart's templates reference — is handled separately by
	// the helmfile bundler's DAG-stratified sub-helmfile layout (one
	// sub-helmfile per dependency level, processed sequentially). That
	// machinery reads ComponentRef.DependencyRefs and needs no
	// registry flag; correct dependencyRefs encode the ordering.
	// gpu-operator is the canonical example for self-reference: its
	// templates create a ClusterPolicy CR of the ClusterPolicy CRD it
	// ships in `crds/`. See https://github.com/NVIDIA/aicr/issues/914.
	HasSelfRefCRDs bool `yaml:"hasSelfRefCRDs,omitempty"`
}

// HealthCheckConfig defines custom health check settings for a component.
type HealthCheckConfig struct {
	// AssertFile is the path to a Chainsaw-style assert YAML file (relative to data directory).
	// When set, the expected-resources check uses Chainsaw CLI to evaluate assertions
	// instead of the default auto-discovery + typed replica checks.
	AssertFile string `yaml:"assertFile,omitempty"`
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

	// NodeCountPaths are Helm value paths where the bundle-time node count is injected (e.g. estimatedNodeCount for nodewright-operator).
	NodeCountPaths []string `yaml:"nodeCountPaths,omitempty"`
}

// SchedulingPaths holds the Helm value paths for node scheduling.
type SchedulingPaths struct {
	// NodeSelectorPaths are paths where node selectors are injected.
	NodeSelectorPaths []string `yaml:"nodeSelectorPaths,omitempty"`

	// TolerationPaths are paths where tolerations are injected.
	TolerationPaths []string `yaml:"tolerationPaths,omitempty"`

	// TaintPaths are paths where taints are injected as structured objects.
	// Intended to be used instea of TaintStrPaths for components that need to set specific parts of taints
	// and can't process the string format.
	TaintPaths []string `yaml:"taintPaths,omitempty"`

	// TaintStrPaths are paths where taints are injected as strings (format: key=value:effect or key:effect).
	TaintStrPaths []string `yaml:"taintStrPaths,omitempty"`
}

// PodSchedulingConfig defines paths for pod scheduling injection.
type PodSchedulingConfig struct {
	// Workload defines paths for workload pod scheduling.
	Workload WorkloadSchedulingPaths `yaml:"workload,omitempty"`
}

// WorkloadSchedulingPaths holds the Helm value paths for workload scheduling.
type WorkloadSchedulingPaths struct {
	// WorkloadSelectorPaths are paths where workload selectors are injected.
	WorkloadSelectorPaths []string `yaml:"workloadSelectorPaths,omitempty"`
}

// ComponentValidationConfig defines a component-specific validation check.
type ComponentValidationConfig struct {
	// Function is the name of the validation function to execute (e.g., "CheckWorkloadSelectorMissing").
	Function string `yaml:"function"`

	// Severity determines whether failures are warnings or errors ("warning" or "error").
	Severity string `yaml:"severity"`

	// Conditions are optional conditions that must be met for the validation to run.
	// Values are arrays of strings for OR matching (single element arrays are equivalent to single values).
	// Example: {"intent": ["training"]} or {"intent": ["training", "inference"]}
	Conditions map[string][]string `yaml:"conditions,omitempty"`

	// Message is an optional detail message to append to validation failures/warnings.
	Message string `yaml:"message,omitempty"`
}

// registryCacheEntry holds the lazily-built ComponentRegistry for a single
// DataProvider identity. sync.Once gates concurrent first-load callers onto
// the same registry and the same error.
type registryCacheEntry struct {
	once     sync.Once
	registry *ComponentRegistry
	err      error
}

// registryCache holds registryCacheEntry pointers keyed by DataProvider
// identity. Two callers bound to different DataProvider values populate
// distinct entries; a single provider value yields a single shared registry
// regardless of caller goroutine count. EvictCachedRegistry drops a single
// entry so callers can force a refetch after rotating provider content
// without disturbing other providers.
var registryCache sync.Map // map[DataProvider]*registryCacheEntry

// GetComponentRegistryFor returns the component registry for the supplied
// DataProvider. Concurrent callers with the same provider observe the same
// singleton; distinct providers populate distinct cache entries and never
// share state. A nil provider falls back to GetDataProvider() so the legacy
// GetComponentRegistry entry point continues to work transparently.
//
// Note: a first-load error is preserved by sync.Once and returned to every
// subsequent caller for the same provider until EvictCachedRegistry drops
// the entry.
func GetComponentRegistryFor(dp DataProvider) (*ComponentRegistry, error) {
	if dp == nil {
		dp = defaultEmbeddedProvider
	}
	e, _ := registryCache.LoadOrStore(dp, &registryCacheEntry{})
	entry := e.(*registryCacheEntry)
	entry.once.Do(func() {
		entry.registry, entry.err = loadComponentRegistryFor(dp)
	})
	return entry.registry, entry.err
}

// GetComponentRegistry returns the component registry for the package-global
// DataProvider. New callers — especially those that need per-tenant
// isolation — should use GetComponentRegistryFor directly with a
// caller-supplied provider.
func GetComponentRegistry() (*ComponentRegistry, error) {
	return GetComponentRegistryFor(defaultEmbeddedProvider)
}

// EvictCachedRegistry drops the cached registry for the supplied provider
// so the next GetComponentRegistryFor call rebuilds from source. Passing a
// nil provider is a no-op (callers handle that case explicitly to avoid
// silently evicting the package-global registry).
func EvictCachedRegistry(dp DataProvider) {
	if dp == nil {
		return
	}
	registryCache.Delete(dp)
}

// CachedRegistryCountForTesting returns the number of distinct
// DataProvider entries currently held in the registry cache. Exposed
// for tests in the aicr facade that assert Client.Close evicts the
// cached registry — without this, the only way to observe eviction
// from outside the recipe package would be to reach into unexported
// state via reflection.
//
// Test-only by convention (the _ForTesting suffix); never call from
// production code.
//
// NOTE: global count across every DataProvider. Tests that need a
// stable signal scoped to a specific DataProvider should prefer
// CachedRegistryContainsForTesting.
func CachedRegistryCountForTesting() int {
	n := 0
	registryCache.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// CachedRegistryContainsForTesting reports whether the registry cache
// has an entry for the supplied DataProvider. Pair with
// CachedStoreContainsForTesting in pkg/recipe/metadata_store.go to
// verify a single Client's caches are released. Scoped per-provider so
// it is robust under parallel test execution.
//
// Test-only by convention (the _ForTesting suffix); never call from
// production code.
func CachedRegistryContainsForTesting(dp DataProvider) bool {
	_, ok := registryCache.Load(dp)
	return ok
}

// ResetComponentRegistryForTesting drops every cached registry so the next
// GetComponentRegistryFor call rebuilds from source. This must only be
// called from tests.
func ResetComponentRegistryForTesting() {
	registryCache.Range(func(k, _ any) bool {
		registryCache.Delete(k)
		return true
	})
}

// loadComponentRegistryFor loads the component registry from the supplied
// provider. It is pure with respect to the package-global DataProvider —
// callers that need package-global semantics route through
// GetComponentRegistry, which resolves the provider once at the entry point.
func loadComponentRegistryFor(provider DataProvider) (*ComponentRegistry, error) {
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

// GetWorkloadSelectorPaths returns all workload selector paths for a component.
func (c *ComponentConfig) GetWorkloadSelectorPaths() []string {
	if c == nil {
		return nil
	}
	return c.PodScheduling.Workload.WorkloadSelectorPaths
}

// GetAcceleratedTaintStrPaths returns all accelerated taint string paths for a component.
func (c *ComponentConfig) GetAcceleratedTaintStrPaths() []string {
	if c == nil {
		return nil
	}
	return c.NodeScheduling.Accelerated.TaintStrPaths
}

// GetNodeCountPaths returns Helm value paths where the node count is injected.
func (c *ComponentConfig) GetNodeCountPaths() []string {
	if c == nil {
		return nil
	}
	return c.NodeScheduling.NodeCountPaths
}

// GetStorageClassPaths returns Helm value paths where the storage class name is injected.
func (c *ComponentConfig) GetStorageClassPaths() []string {
	if c == nil {
		return nil
	}
	return c.StorageClassPaths
}

// GetValidations returns all validation configurations for a component.
func (c *ComponentConfig) GetValidations() []ComponentValidationConfig {
	if c == nil {
		return nil
	}
	return c.Validations
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
