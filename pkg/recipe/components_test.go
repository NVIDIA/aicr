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
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestLoadComponentRegistry(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("failed to load component registry: %v", err)
	}

	if registry == nil {
		t.Fatal("registry is nil")
	}

	if registry.Count() == 0 {
		t.Error("registry has no components")
	}

	t.Logf("loaded %d components from registry", registry.Count())
}

func TestComponentRegistry_Validate(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("failed to load component registry: %v", err)
	}

	errs := registry.Validate()
	if len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("validation error: %v", e)
		}
	}
}

func TestComponentRegistry_RequiredFields(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("failed to load component registry: %v", err)
	}

	for _, comp := range registry.Components {
		t.Run(comp.Name, func(t *testing.T) {
			if comp.Name == "" {
				t.Error("name is required")
			}
			if comp.DisplayName == "" {
				t.Error("displayName is required")
			}
			// At least one valueOverrideKey should be defined
			if len(comp.ValueOverrideKeys) == 0 {
				t.Error("at least one valueOverrideKey is recommended")
			}
		})
	}
}

func TestComponentRegistry_UniqueNames(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("failed to load component registry: %v", err)
	}

	seen := make(map[string]bool)
	for _, comp := range registry.Components {
		if seen[comp.Name] {
			t.Errorf("duplicate component name: %s", comp.Name)
		}
		seen[comp.Name] = true
	}
}

func TestComponentRegistry_UniqueOverrideKeys(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("failed to load component registry: %v", err)
	}

	overrideKeys := make(map[string]string) // key -> component name
	for _, comp := range registry.Components {
		for _, key := range comp.ValueOverrideKeys {
			if existing, ok := overrideKeys[key]; ok {
				t.Errorf("duplicate valueOverrideKey %q: used by both %s and %s", key, existing, comp.Name)
			}
			overrideKeys[key] = comp.Name
		}
	}
}

func TestComponentRegistry_Get(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("failed to load component registry: %v", err)
	}

	tests := []struct {
		name    string
		wantNil bool
	}{
		{"gpu-operator", false},
		{"cert-manager", false},
		{"skyhook-operator", false},
		{"nvsentinel", false},
		{"network-operator", false},
		{"nvidia-dra-driver-gpu", false},
		{"nonexistent-component", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			comp := registry.Get(tt.name)
			if tt.wantNil && comp != nil {
				t.Errorf("expected nil for %s, got %+v", tt.name, comp)
			}
			if !tt.wantNil && comp == nil {
				t.Errorf("expected component for %s, got nil", tt.name)
			}
		})
	}
}

func TestComponentRegistry_GetByOverrideKey(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("failed to load component registry: %v", err)
	}

	tests := []struct {
		key      string
		wantName string
		wantNil  bool
	}{
		{"gpuoperator", "gpu-operator", false},
		{"gpu-operator", "gpu-operator", false},
		{"certmanager", "cert-manager", false},
		{"skyhook", "skyhook-operator", false},
		{"nv-sentinel", "nvsentinel", false},
		{"dradriver", "nvidia-dra-driver-gpu", false},
		{"networkoperator", "network-operator", false},
		{"nonexistent", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			comp := registry.GetByOverrideKey(tt.key)
			if tt.wantNil {
				if comp != nil {
					t.Errorf("expected nil for %s, got %s", tt.key, comp.Name)
				}
			} else {
				if comp == nil {
					t.Errorf("expected component for %s, got nil", tt.key)
				} else if comp.Name != tt.wantName {
					t.Errorf("expected %s for key %s, got %s", tt.wantName, tt.key, comp.Name)
				}
			}
		})
	}
}

func TestComponentRegistry_NodeSchedulingPaths(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("failed to load component registry: %v", err)
	}

	// Test gpu-operator has all scheduling paths
	gpuOp := registry.Get("gpu-operator")
	if gpuOp == nil {
		t.Fatal("gpu-operator not found in registry")
	}

	if len(gpuOp.GetSystemNodeSelectorPaths()) == 0 {
		t.Error("gpu-operator should have system node selector paths")
	}
	if len(gpuOp.GetSystemTolerationPaths()) == 0 {
		t.Error("gpu-operator should have system toleration paths")
	}
	if len(gpuOp.GetAcceleratedNodeSelectorPaths()) == 0 {
		t.Error("gpu-operator should have accelerated node selector paths")
	}
	if len(gpuOp.GetAcceleratedTolerationPaths()) == 0 {
		t.Error("gpu-operator should have accelerated toleration paths")
	}

	// Verify specific paths exist
	sysSelectors := gpuOp.GetSystemNodeSelectorPaths()
	if !slices.Contains(sysSelectors, "operator.nodeSelector") {
		t.Error("gpu-operator should have 'operator.nodeSelector' in system node selector paths")
	}
}

func TestComponentRegistry_TaintStrPaths(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("failed to load component registry: %v", err)
	}

	// Test skyhook-operator has taint string paths
	skyhookOp := registry.Get("skyhook-operator")
	if skyhookOp == nil {
		t.Fatal("skyhook-operator not found in registry")
	}

	taintStrPaths := skyhookOp.GetAcceleratedTaintStrPaths()
	if len(taintStrPaths) == 0 {
		t.Error("skyhook-operator should have accelerated taint string paths")
	}

	// Verify specific path exists
	if !slices.Contains(taintStrPaths, "controllerManager.manager.env.runtimeRequiredTaint") {
		t.Error("skyhook-operator should have 'controllerManager.manager.env.runtimeRequiredTaint' in accelerated taint string paths")
	}

	// Test skyhook-operator has node count path (for --nodes bundle flag)
	nodeCountPaths := skyhookOp.GetNodeCountPaths()
	if len(nodeCountPaths) == 0 {
		t.Error("skyhook-operator should have nodeCountPaths")
	}
	if !slices.Contains(nodeCountPaths, "estimatedNodeCount") {
		t.Error("skyhook-operator should have 'estimatedNodeCount' in nodeCountPaths")
	}
}

func TestComponentRegistry_WorkloadSelectorPaths(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("failed to load component registry: %v", err)
	}

	// Test skyhook-customizations has workload selector paths
	skyhookCust := registry.Get("skyhook-customizations")
	if skyhookCust == nil {
		t.Fatal("skyhook-customizations not found in registry")
	}

	workloadSelectorPaths := skyhookCust.GetWorkloadSelectorPaths()
	if len(workloadSelectorPaths) == 0 {
		t.Error("skyhook-customizations should have workload selector paths")
	}

	// Verify specific path exists
	if !slices.Contains(workloadSelectorPaths, "workloadSelector") {
		t.Error("skyhook-customizations should have 'workloadSelector' in workload selector paths")
	}
}

func TestComponentRegistry_Validations(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("failed to load component registry: %v", err)
	}

	// Test skyhook-customizations has validations
	skyhookCust := registry.Get("skyhook-customizations")
	if skyhookCust == nil {
		t.Fatal("skyhook-customizations not found in registry")
	}

	validations := skyhookCust.GetValidations()
	if len(validations) == 0 {
		t.Error("skyhook-customizations should have validations configured")
	}

	// Verify specific validations exist
	foundWorkloadSelector := false
	foundAcceleratedSelector := false
	for _, v := range validations {
		if v.Function == "CheckWorkloadSelectorMissing" {
			foundWorkloadSelector = true
			if v.Severity != "warning" {
				t.Errorf("CheckWorkloadSelectorMissing should have severity 'warning', got %q", v.Severity)
			}
			if v.Conditions == nil {
				t.Error("CheckWorkloadSelectorMissing should have conditions")
			} else {
				intentValues, ok := v.Conditions["intent"]
				if !ok || !slices.Contains(intentValues, "training") {
					t.Error("CheckWorkloadSelectorMissing should have condition intent containing 'training'")
				}
			}
			if v.Message == "" {
				t.Error("CheckWorkloadSelectorMissing should have a message")
			}
		}
		if v.Function == "CheckAcceleratedSelectorMissing" {
			foundAcceleratedSelector = true
			if v.Severity != "warning" {
				t.Errorf("CheckAcceleratedSelectorMissing should have severity 'warning', got %q", v.Severity)
			}
			if v.Conditions == nil {
				t.Error("CheckAcceleratedSelectorMissing should have conditions")
			} else {
				intentValues, ok := v.Conditions["intent"]
				if !ok {
					t.Error("CheckAcceleratedSelectorMissing should have condition intent")
				} else {
					if !slices.Contains(intentValues, "training") {
						t.Error("CheckAcceleratedSelectorMissing should have condition intent containing 'training'")
					}
					if !slices.Contains(intentValues, "inference") {
						t.Error("CheckAcceleratedSelectorMissing should have condition intent containing 'inference'")
					}
				}
			}
			if v.Message == "" {
				t.Error("CheckAcceleratedSelectorMissing should have a message")
			}
		}
	}

	if !foundWorkloadSelector {
		t.Error("skyhook-customizations should have CheckWorkloadSelectorMissing validation")
	}
	if !foundAcceleratedSelector {
		t.Error("skyhook-customizations should have CheckAcceleratedSelectorMissing validation")
	}
}

func TestComponentRegistry_PathSyntax(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("failed to load component registry: %v", err)
	}

	// Validate path syntax (should be dot-notation)
	for _, comp := range registry.Components {
		allPaths := make([]string, 0,
			len(comp.GetSystemNodeSelectorPaths())+
				len(comp.GetSystemTolerationPaths())+
				len(comp.GetAcceleratedNodeSelectorPaths())+
				len(comp.GetAcceleratedTolerationPaths())+
				len(comp.GetNodeCountPaths()))
		allPaths = append(allPaths, comp.GetSystemNodeSelectorPaths()...)
		allPaths = append(allPaths, comp.GetSystemTolerationPaths()...)
		allPaths = append(allPaths, comp.GetAcceleratedNodeSelectorPaths()...)
		allPaths = append(allPaths, comp.GetAcceleratedTolerationPaths()...)
		allPaths = append(allPaths, comp.GetNodeCountPaths()...)

		for _, path := range allPaths {
			// Paths should not be empty
			if path == "" {
				t.Errorf("component %s has empty path", comp.Name)
				continue
			}
			// Paths should not start or end with a dot
			if strings.HasPrefix(path, ".") || strings.HasSuffix(path, ".") {
				t.Errorf("component %s has invalid path %q (should not start/end with dot)", comp.Name, path)
			}
			// Paths should not have consecutive dots
			if strings.Contains(path, "..") {
				t.Errorf("component %s has invalid path %q (consecutive dots)", comp.Name, path)
			}
		}
	}
}

func TestComponentRegistry_MatchesBaseRecipe(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("failed to load component registry: %v", err)
	}

	// Load base recipe via metadata store
	ctx := t.Context()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("failed to load metadata store: %v", err)
	}

	if store.Base == nil {
		t.Fatal("base recipe not loaded")
	}

	for _, ref := range store.Base.Spec.ComponentRefs {
		comp := registry.Get(ref.Name)
		if comp == nil {
			t.Errorf("component %s in base.yaml not found in registry", ref.Name)
		}
	}
}

func TestComponentRegistry_Names(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("failed to load component registry: %v", err)
	}

	names := registry.Names()
	if len(names) == 0 {
		t.Error("expected at least one component name")
	}

	// Verify expected components
	expected := []string{
		"gpu-operator",
		"cert-manager",
		"skyhook-operator",
		"nvsentinel",
		"network-operator",
		"nvidia-dra-driver-gpu",
	}

	for _, exp := range expected {
		if !slices.Contains(names, exp) {
			t.Errorf("expected component %s not found in registry.Names()", exp)
		}
	}
}

func TestComponentConfig_NilSafety(t *testing.T) {
	var nilComp *ComponentConfig

	// These should not panic
	if nilComp.GetSystemNodeSelectorPaths() != nil {
		t.Error("expected nil for nil component")
	}
	if nilComp.GetSystemTolerationPaths() != nil {
		t.Error("expected nil for nil component")
	}
	if nilComp.GetAcceleratedNodeSelectorPaths() != nil {
		t.Error("expected nil for nil component")
	}
	if nilComp.GetAcceleratedTolerationPaths() != nil {
		t.Error("expected nil for nil component")
	}
}

func TestComponentRegistry_NilSafety(t *testing.T) {
	var nilRegistry *ComponentRegistry

	// These should not panic
	if nilRegistry.Get("test") != nil {
		t.Error("expected nil for nil registry")
	}
	if nilRegistry.GetByOverrideKey("test") != nil {
		t.Error("expected nil for nil registry")
	}
	if nilRegistry.Names() != nil {
		t.Error("expected nil for nil registry")
	}
	if nilRegistry.Count() != 0 {
		t.Error("expected 0 for nil registry")
	}
}

func TestComponentRegistry_Validate_EdgeCases(t *testing.T) {
	t.Run("nil registry returns error", func(t *testing.T) {
		var nilRegistry *ComponentRegistry
		errs := nilRegistry.Validate()
		if len(errs) == 0 {
			t.Error("expected validation error for nil registry")
		}
	})

	t.Run("empty name validation", func(t *testing.T) {
		registry := &ComponentRegistry{
			Components: []ComponentConfig{
				{
					Name:        "",
					DisplayName: "Test",
				},
			},
		}
		errs := registry.Validate()
		if len(errs) == 0 {
			t.Error("expected validation error for empty name")
		}
		found := false
		for _, e := range errs {
			if strings.Contains(e.Error(), "name is required") {
				found = true
				break
			}
		}
		if !found {
			t.Error("expected error about name being required")
		}
	})

	t.Run("empty displayName validation", func(t *testing.T) {
		registry := &ComponentRegistry{
			Components: []ComponentConfig{
				{
					Name:        "test",
					DisplayName: "",
				},
			},
		}
		errs := registry.Validate()
		if len(errs) == 0 {
			t.Error("expected validation error for empty displayName")
		}
		found := false
		for _, e := range errs {
			if strings.Contains(e.Error(), "displayName is required") {
				found = true
				break
			}
		}
		if !found {
			t.Error("expected error about displayName being required")
		}
	})

	t.Run("duplicate component names", func(t *testing.T) {
		registry := &ComponentRegistry{
			Components: []ComponentConfig{
				{Name: "test", DisplayName: "Test 1"},
				{Name: "test", DisplayName: "Test 2"},
			},
		}
		errs := registry.Validate()
		found := false
		for _, e := range errs {
			if strings.Contains(e.Error(), "duplicate component name") {
				found = true
				break
			}
		}
		if !found {
			t.Error("expected error about duplicate component name")
		}
	})

	t.Run("duplicate override keys", func(t *testing.T) {
		registry := &ComponentRegistry{
			Components: []ComponentConfig{
				{Name: "comp1", DisplayName: "Comp 1", ValueOverrideKeys: []string{"shared-key"}},
				{Name: "comp2", DisplayName: "Comp 2", ValueOverrideKeys: []string{"shared-key"}},
			},
		}
		errs := registry.Validate()
		found := false
		for _, e := range errs {
			if strings.Contains(e.Error(), "duplicate valueOverrideKey") {
				found = true
				break
			}
		}
		if !found {
			t.Error("expected error about duplicate valueOverrideKey")
		}
	})

	t.Run("valid registry passes", func(t *testing.T) {
		registry := &ComponentRegistry{
			Components: []ComponentConfig{
				{Name: "comp1", DisplayName: "Comp 1", ValueOverrideKeys: []string{"c1"}},
				{Name: "comp2", DisplayName: "Comp 2", ValueOverrideKeys: []string{"c2"}},
			},
		}
		errs := registry.Validate()
		if len(errs) != 0 {
			t.Errorf("expected no validation errors, got: %v", errs)
		}
	})
}

func TestComponentRegistry_GetEmptyByName(t *testing.T) {
	registry := &ComponentRegistry{
		byName: nil, // Not initialized
	}

	// Should not panic and return nil
	result := registry.Get("test")
	if result != nil {
		t.Error("expected nil for registry with nil byName map")
	}
}

func TestComponentConfig_GetType(t *testing.T) {
	tests := []struct {
		name     string
		config   *ComponentConfig
		expected ComponentType
	}{
		{
			name:     "nil config returns Helm",
			config:   nil,
			expected: ComponentTypeHelm,
		},
		{
			name: "empty config returns Helm",
			config: &ComponentConfig{
				Name: "test",
			},
			expected: ComponentTypeHelm,
		},
		{
			name: "helm config returns Helm",
			config: &ComponentConfig{
				Name: "test",
				Helm: HelmConfig{
					DefaultRepository: "https://charts.example.com",
					DefaultChart:      "example/chart",
				},
			},
			expected: ComponentTypeHelm,
		},
		{
			name: "kustomize config returns Kustomize",
			config: &ComponentConfig{
				Name: "test",
				Kustomize: KustomizeConfig{
					DefaultSource: "https://github.com/example/repo",
				},
			},
			expected: ComponentTypeKustomize,
		},
		{
			name: "kustomize with path and tag returns Kustomize",
			config: &ComponentConfig{
				Name: "test",
				Kustomize: KustomizeConfig{
					DefaultSource: "https://github.com/example/repo",
					DefaultPath:   "deploy/production",
					DefaultTag:    "v1.0.0",
				},
			},
			expected: ComponentTypeKustomize,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.config.GetType()
			if result != tt.expected {
				t.Errorf("GetType() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestComponentRegistry_Validate_MutuallyExclusiveHelmKustomize(t *testing.T) {
	t.Run("helm only is valid", func(t *testing.T) {
		registry := &ComponentRegistry{
			Components: []ComponentConfig{
				{
					Name:        "test-helm",
					DisplayName: "Test Helm",
					Helm: HelmConfig{
						DefaultRepository: "https://charts.example.com",
						DefaultChart:      "example/chart",
					},
				},
			},
		}
		errs := registry.Validate()
		for _, e := range errs {
			if strings.Contains(e.Error(), "both helm and kustomize") {
				t.Errorf("unexpected error for helm-only component: %v", e)
			}
		}
	})

	t.Run("kustomize only is valid", func(t *testing.T) {
		registry := &ComponentRegistry{
			Components: []ComponentConfig{
				{
					Name:        "test-kustomize",
					DisplayName: "Test Kustomize",
					Kustomize: KustomizeConfig{
						DefaultSource: "https://github.com/example/repo",
					},
				},
			},
		}
		errs := registry.Validate()
		for _, e := range errs {
			if strings.Contains(e.Error(), "both helm and kustomize") {
				t.Errorf("unexpected error for kustomize-only component: %v", e)
			}
		}
	})

	t.Run("both helm and kustomize is invalid", func(t *testing.T) {
		registry := &ComponentRegistry{
			Components: []ComponentConfig{
				{
					Name:        "test-both",
					DisplayName: "Test Both",
					Helm: HelmConfig{
						DefaultRepository: "https://charts.example.com",
						DefaultChart:      "example/chart",
					},
					Kustomize: KustomizeConfig{
						DefaultSource: "https://github.com/example/repo",
					},
				},
			},
		}
		errs := registry.Validate()
		found := false
		for _, e := range errs {
			if strings.Contains(e.Error(), "both helm and kustomize") {
				found = true
				break
			}
		}
		if !found {
			t.Error("expected error about both helm and kustomize configuration")
		}
	})
}

func TestHelmConfig_DefaultNamespace(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("failed to load component registry: %v", err)
	}

	tests := []struct {
		name              string
		expectedNamespace string
	}{
		{"gpu-operator", "gpu-operator"},
		{"network-operator", "nvidia-network-operator"},
		{"cert-manager", "cert-manager"},
		{"nvsentinel", "nvsentinel"},
		{"skyhook-operator", "skyhook"},
		{"kube-prometheus-stack", "monitoring"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			comp := registry.Get(tt.name)
			if comp == nil {
				t.Fatalf("component %s not found in registry", tt.name)
			}
			if comp.Helm.DefaultNamespace != tt.expectedNamespace {
				t.Errorf("DefaultNamespace = %q, want %q", comp.Helm.DefaultNamespace, tt.expectedNamespace)
			}
		})
	}
}

func TestHelmConfig_DefaultNamespaceParsing(t *testing.T) {
	yamlData := `
apiVersion: aicr.nvidia.com/v1alpha1
kind: ComponentRegistry
components:
  - name: test-component
    displayName: Test Component
    valueOverrideKeys:
      - testcomp
    helm:
      defaultRepository: https://charts.example.com
      defaultChart: example/test-component
      defaultNamespace: custom-namespace
`

	var registry ComponentRegistry
	err := yaml.Unmarshal([]byte(yamlData), &registry)
	if err != nil {
		t.Fatalf("failed to unmarshal YAML: %v", err)
	}

	if len(registry.Components) != 1 {
		t.Fatalf("expected 1 component, got %d", len(registry.Components))
	}

	comp := registry.Components[0]
	if comp.Helm.DefaultNamespace != "custom-namespace" {
		t.Errorf("Helm.DefaultNamespace = %q, want %q", comp.Helm.DefaultNamespace, "custom-namespace")
	}
}

func TestKustomizeConfig_Parsing(t *testing.T) {
	// Test that KustomizeConfig can be parsed correctly from YAML
	const (
		testKustomizeSource = "https://github.com/example/my-app"
		testKustomizePath   = "deploy/production"
		testKustomizeTag    = "v1.0.0"
	)

	yamlData := `
apiVersion: aicr.nvidia.com/v1alpha1
kind: ComponentRegistry
components:
  - name: my-kustomize-app
    displayName: My Kustomize App
    valueOverrideKeys:
      - mykustomize
    kustomize:
      defaultSource: https://github.com/example/my-app
      defaultPath: deploy/production
      defaultTag: v1.0.0
`

	var registry ComponentRegistry
	err := yaml.Unmarshal([]byte(yamlData), &registry)
	if err != nil {
		t.Fatalf("failed to unmarshal YAML: %v", err)
	}

	if len(registry.Components) != 1 {
		t.Fatalf("expected 1 component, got %d", len(registry.Components))
	}

	comp := registry.Components[0]
	if comp.Name != "my-kustomize-app" {
		t.Errorf("Name = %q, want %q", comp.Name, "my-kustomize-app")
	}
	if comp.Kustomize.DefaultSource != testKustomizeSource {
		t.Errorf("Kustomize.DefaultSource = %q, want %q", comp.Kustomize.DefaultSource, testKustomizeSource)
	}
	if comp.Kustomize.DefaultPath != testKustomizePath {
		t.Errorf("Kustomize.DefaultPath = %q, want %q", comp.Kustomize.DefaultPath, testKustomizePath)
	}
	if comp.Kustomize.DefaultTag != testKustomizeTag {
		t.Errorf("Kustomize.DefaultTag = %q, want %q", comp.Kustomize.DefaultTag, testKustomizeTag)
	}

	// Verify GetType returns Kustomize
	if comp.GetType() != ComponentTypeKustomize {
		t.Errorf("GetType() = %v, want %v", comp.GetType(), ComponentTypeKustomize)
	}
}
