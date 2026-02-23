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

// metadata_test.go tests the RecipeMetadata types and MetadataStore.
//
// Area of Concern: Recipe metadata behavior and inheritance
// - RecipeMetadataSpec.ValidateDependencies() - component dependency validation
// - RecipeMetadataSpec.TopologicalSort() - deployment ordering
// - RecipeMetadataSpec.Merge() - overlay merging with base recipes
// - ComponentRef merging - how overlays override/inherit base values
// - MetadataStore inheritance chains - multi-level spec.base resolution
//   (e.g., base → eks → eks-training → gb200-eks-training)
//
// These tests use synthesized Go structs and the actual MetadataStore
// to verify runtime behavior of the metadata layer.
//
// Related test files:
// - recipe_test.go: Tests Recipe struct validation methods after recipes
//   are built (Validate, ValidateStructure, ValidateMeasurementExists)
// - yaml_test.go: Tests embedded YAML data files for schema conformance,
//   valid references, enum values, and constraint syntax

package recipe

import (
	"context"
	"strings"
	"testing"
)

func TestRecipeMetadataSpecValidateDependencies(t *testing.T) {
	tests := []struct {
		name    string
		spec    RecipeMetadataSpec
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid no dependencies",
			spec: RecipeMetadataSpec{
				ComponentRefs: []ComponentRef{
					{Name: "cert-manager", Type: ComponentTypeHelm},
					{Name: "gpu-operator", Type: ComponentTypeHelm},
				},
			},
			wantErr: false,
		},
		{
			name: "valid with dependencies",
			spec: RecipeMetadataSpec{
				ComponentRefs: []ComponentRef{
					{Name: "cert-manager", Type: ComponentTypeHelm},
					{Name: "gpu-operator", Type: ComponentTypeHelm, DependencyRefs: []string{"cert-manager"}},
					{Name: "nvidia-dra-driver-gpu", Type: ComponentTypeHelm, DependencyRefs: []string{"gpu-operator"}},
				},
			},
			wantErr: false,
		},
		{
			name: "missing dependency",
			spec: RecipeMetadataSpec{
				ComponentRefs: []ComponentRef{
					{Name: "gpu-operator", Type: ComponentTypeHelm, DependencyRefs: []string{"cert-manager"}},
				},
			},
			wantErr: true,
			errMsg:  "references unknown dependency",
		},
		{
			name: "self-dependency (cycle)",
			spec: RecipeMetadataSpec{
				ComponentRefs: []ComponentRef{
					{Name: "cert-manager", Type: ComponentTypeHelm, DependencyRefs: []string{"cert-manager"}},
				},
			},
			wantErr: true,
			errMsg:  "circular dependency",
		},
		{
			name: "two-node cycle",
			spec: RecipeMetadataSpec{
				ComponentRefs: []ComponentRef{
					{Name: "A", Type: ComponentTypeHelm, DependencyRefs: []string{"B"}},
					{Name: "B", Type: ComponentTypeHelm, DependencyRefs: []string{"A"}},
				},
			},
			wantErr: true,
			errMsg:  "circular dependency",
		},
		{
			name: "three-node cycle",
			spec: RecipeMetadataSpec{
				ComponentRefs: []ComponentRef{
					{Name: "A", Type: ComponentTypeHelm, DependencyRefs: []string{"B"}},
					{Name: "B", Type: ComponentTypeHelm, DependencyRefs: []string{"C"}},
					{Name: "C", Type: ComponentTypeHelm, DependencyRefs: []string{"A"}},
				},
			},
			wantErr: true,
			errMsg:  "circular dependency",
		},
		{
			name: "complex valid graph",
			spec: RecipeMetadataSpec{
				ComponentRefs: []ComponentRef{
					{Name: "cert-manager", Type: ComponentTypeHelm},
					{Name: "gpu-operator", Type: ComponentTypeHelm, DependencyRefs: []string{"cert-manager"}},
					{Name: "network-operator", Type: ComponentTypeHelm, DependencyRefs: []string{"cert-manager"}},
					{Name: "nvsentinel", Type: ComponentTypeHelm, DependencyRefs: []string{"cert-manager", "gpu-operator"}},
					{Name: "nvidia-dra-driver-gpu", Type: ComponentTypeHelm, DependencyRefs: []string{"gpu-operator"}},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.spec.ValidateDependencies()
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateDependencies() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.errMsg != "" && err != nil {
				if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("ValidateDependencies() error = %v, want error containing %q", err, tt.errMsg)
				}
			}
		})
	}
}

func TestRecipeMetadataSpecTopologicalSort(t *testing.T) {
	tests := []struct {
		name    string
		spec    RecipeMetadataSpec
		want    []string
		wantErr bool
	}{
		{
			name: "no dependencies",
			spec: RecipeMetadataSpec{
				ComponentRefs: []ComponentRef{
					{Name: "cert-manager", Type: ComponentTypeHelm},
					{Name: "gpu-operator", Type: ComponentTypeHelm},
				},
			},
			want: []string{"cert-manager", "gpu-operator"},
		},
		{
			name: "linear dependencies",
			spec: RecipeMetadataSpec{
				ComponentRefs: []ComponentRef{
					{Name: "cert-manager", Type: ComponentTypeHelm},
					{Name: "gpu-operator", Type: ComponentTypeHelm, DependencyRefs: []string{"cert-manager"}},
					{Name: "nvidia-dra-driver-gpu", Type: ComponentTypeHelm, DependencyRefs: []string{"gpu-operator"}},
				},
			},
			want: []string{"cert-manager", "gpu-operator", "nvidia-dra-driver-gpu"},
		},
		{
			name: "diamond dependencies",
			spec: RecipeMetadataSpec{
				ComponentRefs: []ComponentRef{
					{Name: "cert-manager", Type: ComponentTypeHelm},
					{Name: "gpu-operator", Type: ComponentTypeHelm, DependencyRefs: []string{"cert-manager"}},
					{Name: "network-operator", Type: ComponentTypeHelm, DependencyRefs: []string{"cert-manager"}},
					{Name: "nvsentinel", Type: ComponentTypeHelm, DependencyRefs: []string{"gpu-operator", "network-operator"}},
				},
			},
			// cert-manager first, then gpu-operator and network-operator (alphabetically), then nvsentinel
			want: []string{"cert-manager", "gpu-operator", "network-operator", "nvsentinel"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.spec.TopologicalSort()
			if (err != nil) != tt.wantErr {
				t.Errorf("TopologicalSort() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			if len(got) != len(tt.want) {
				t.Errorf("TopologicalSort() len = %d, want %d", len(got), len(tt.want))
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("TopologicalSort()[%d] = %v, want %v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestRecipeMetadataSpecMerge(t *testing.T) {
	tests := []struct {
		name        string
		base        RecipeMetadataSpec
		overlay     RecipeMetadataSpec
		wantCompCnt int
		wantConCnt  int
	}{
		{
			name: "merge disjoint components",
			base: RecipeMetadataSpec{
				ComponentRefs: []ComponentRef{
					{Name: "cert-manager", Type: ComponentTypeHelm, Version: "v1.0.0"},
				},
			},
			overlay: RecipeMetadataSpec{
				ComponentRefs: []ComponentRef{
					{Name: "gpu-operator", Type: ComponentTypeHelm, Version: "v2.0.0"},
				},
			},
			wantCompCnt: 2,
		},
		{
			name: "overlay overrides component",
			base: RecipeMetadataSpec{
				ComponentRefs: []ComponentRef{
					{Name: "gpu-operator", Type: ComponentTypeHelm, Version: "v1.0.0"},
				},
			},
			overlay: RecipeMetadataSpec{
				ComponentRefs: []ComponentRef{
					{Name: "gpu-operator", Type: ComponentTypeHelm, Version: "v2.0.0"},
				},
			},
			wantCompCnt: 1,
		},
		{
			name: "merge constraints",
			base: RecipeMetadataSpec{
				Constraints: []Constraint{
					{Name: "k8s", Value: ">= 1.30"},
				},
			},
			overlay: RecipeMetadataSpec{
				Constraints: []Constraint{
					{Name: "kernel", Value: ">= 6.8"},
				},
			},
			wantConCnt: 2,
		},
		{
			name: "overlay overrides constraint",
			base: RecipeMetadataSpec{
				Constraints: []Constraint{
					{Name: "k8s", Value: ">= 1.30"},
				},
			},
			overlay: RecipeMetadataSpec{
				Constraints: []Constraint{
					{Name: "k8s", Value: ">= 1.32"},
				},
			},
			wantConCnt: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.base.Merge(&tt.overlay)
			if tt.wantCompCnt > 0 && len(tt.base.ComponentRefs) != tt.wantCompCnt {
				t.Errorf("Merge() componentRefs count = %d, want %d", len(tt.base.ComponentRefs), tt.wantCompCnt)
			}
			if tt.wantConCnt > 0 && len(tt.base.Constraints) != tt.wantConCnt {
				t.Errorf("Merge() constraints count = %d, want %d", len(tt.base.Constraints), tt.wantConCnt)
			}
		})
	}
}

// TestComponentRefMergeInheritsFromBase verifies that when an overlay specifies
// only partial fields for a component, the missing fields are inherited from base.
func TestComponentRefMergeInheritsFromBase(t *testing.T) {
	base := RecipeMetadataSpec{
		ComponentRefs: []ComponentRef{
			{
				Name:       "cert-manager",
				Type:       ComponentTypeHelm,
				Source:     "https://charts.jetstack.io",
				Version:    "v1.17.2",
				ValuesFile: "components/cert-manager/values.yaml",
			},
		},
	}

	// Overlay only specifies name, type, and new valuesFile
	overlay := RecipeMetadataSpec{
		ComponentRefs: []ComponentRef{
			{
				Name:       "cert-manager",
				Type:       ComponentTypeHelm,
				ValuesFile: "components/cert-manager/tainted-values.yaml",
			},
		},
	}

	base.Merge(&overlay)

	if len(base.ComponentRefs) != 1 {
		t.Fatalf("expected 1 component, got %d", len(base.ComponentRefs))
	}

	comp := base.ComponentRefs[0]

	// Verify inherited fields from base
	if comp.Source != "https://charts.jetstack.io" {
		t.Errorf("Source should be inherited from base, got %q", comp.Source)
	}
	if comp.Version != "v1.17.2" {
		t.Errorf("Version should be inherited from base, got %q", comp.Version)
	}

	// Verify overridden field from overlay
	if comp.ValuesFile != "components/cert-manager/tainted-values.yaml" {
		t.Errorf("ValuesFile should be from overlay, got %q", comp.ValuesFile)
	}

	t.Logf("ComponentRef correctly merged: source=%s, version=%s, valuesFile=%s",
		comp.Source, comp.Version, comp.ValuesFile)
}

func TestMergeComponentRef_AdvancedFields(t *testing.T) {
	t.Run("overrides merged from overlay", func(t *testing.T) {
		base := ComponentRef{
			Name:      "gpu-operator",
			Overrides: map[string]any{"driver.enabled": true},
		}
		overlay := ComponentRef{
			Name:      "gpu-operator",
			Overrides: map[string]any{"gds.enabled": true},
		}
		result := mergeComponentRef(base, overlay)
		if result.Overrides["driver.enabled"] != true {
			t.Error("expected base override to be preserved")
		}
		if result.Overrides["gds.enabled"] != true {
			t.Error("expected overlay override to be merged")
		}
	})

	t.Run("overrides overlay into nil base", func(t *testing.T) {
		base := ComponentRef{Name: "test"}
		overlay := ComponentRef{
			Name:      "test",
			Overrides: map[string]any{"key": "val"},
		}
		result := mergeComponentRef(base, overlay)
		if result.Overrides["key"] != "val" {
			t.Error("expected override to be set on nil base")
		}
	})

	t.Run("patches replaced by overlay", func(t *testing.T) {
		base := ComponentRef{
			Name:    "test",
			Patches: []string{"base-patch.yaml"},
		}
		overlay := ComponentRef{
			Name:    "test",
			Patches: []string{"overlay-patch.yaml"},
		}
		result := mergeComponentRef(base, overlay)
		if len(result.Patches) != 1 || result.Patches[0] != "overlay-patch.yaml" {
			t.Errorf("patches = %v, want [overlay-patch.yaml]", result.Patches)
		}
	})

	t.Run("dependencyRefs replaced by overlay", func(t *testing.T) {
		base := ComponentRef{
			Name:           "test",
			DependencyRefs: []string{"dep-a"},
		}
		overlay := ComponentRef{
			Name:           "test",
			DependencyRefs: []string{"dep-b", "dep-c"},
		}
		result := mergeComponentRef(base, overlay)
		if len(result.DependencyRefs) != 2 {
			t.Errorf("dependencyRefs len = %d, want 2", len(result.DependencyRefs))
		}
	})

	t.Run("manifestFiles additive dedup merge", func(t *testing.T) {
		base := ComponentRef{
			Name:          "test",
			ManifestFiles: []string{"a.yaml", "b.yaml"},
		}
		overlay := ComponentRef{
			Name:          "test",
			ManifestFiles: []string{"b.yaml", "c.yaml"},
		}
		result := mergeComponentRef(base, overlay)
		if len(result.ManifestFiles) != 3 {
			t.Errorf("manifestFiles = %v, want 3 items (a, b, c)", result.ManifestFiles)
		}
	})

	t.Run("tag from overlay", func(t *testing.T) {
		base := ComponentRef{Name: "test", Tag: "v1.0"}
		overlay := ComponentRef{Name: "test", Tag: "v2.0"}
		result := mergeComponentRef(base, overlay)
		if result.Tag != "v2.0" {
			t.Errorf("tag = %q, want v2.0", result.Tag)
		}
	})

	t.Run("expectedResources replaced by overlay", func(t *testing.T) {
		base := ComponentRef{
			Name: "gpu-operator",
			ExpectedResources: []ExpectedResource{
				{Kind: "Deployment", Name: "gpu-operator", Namespace: "gpu-operator"},
			},
		}
		overlay := ComponentRef{
			Name: "gpu-operator",
			ExpectedResources: []ExpectedResource{
				{Kind: "DaemonSet", Name: "nvidia-driver", Namespace: "gpu-operator"},
				{Kind: "DaemonSet", Name: "dcgm-exporter", Namespace: "gpu-operator"},
			},
		}
		result := mergeComponentRef(base, overlay)
		if len(result.ExpectedResources) != 2 {
			t.Errorf("expectedResources len = %d, want 2", len(result.ExpectedResources))
		}
		if result.ExpectedResources[0].Kind != "DaemonSet" {
			t.Errorf("expectedResources[0].Kind = %q, want DaemonSet", result.ExpectedResources[0].Kind)
		}
	})

	t.Run("expectedResources inherited from base", func(t *testing.T) {
		base := ComponentRef{
			Name: "gpu-operator",
			ExpectedResources: []ExpectedResource{
				{Kind: "Deployment", Name: "gpu-operator", Namespace: "gpu-operator"},
			},
		}
		overlay := ComponentRef{
			Name:      "gpu-operator",
			Overrides: map[string]any{"cdi.enabled": true},
		}
		result := mergeComponentRef(base, overlay)
		if len(result.ExpectedResources) != 1 {
			t.Errorf("expectedResources len = %d, want 1 (inherited from base)", len(result.ExpectedResources))
		}
		if result.ExpectedResources[0].Name != "gpu-operator" {
			t.Errorf("expectedResources[0].Name = %q, want gpu-operator", result.ExpectedResources[0].Name)
		}
	})

	t.Run("cleanup inherited from base", func(t *testing.T) {
		base := ComponentRef{Name: "nccl-doctor", Cleanup: true}
		overlay := ComponentRef{Name: "nccl-doctor", Version: "v2.0"}
		result := mergeComponentRef(base, overlay)
		if !result.Cleanup {
			t.Error("cleanup should be inherited from base when overlay doesn't set it")
		}
	})

	t.Run("cleanup set by overlay", func(t *testing.T) {
		base := ComponentRef{Name: "nccl-doctor"}
		overlay := ComponentRef{Name: "nccl-doctor", Cleanup: true}
		result := mergeComponentRef(base, overlay)
		if !result.Cleanup {
			t.Error("cleanup should be true when overlay sets it")
		}
	})
}

func TestMergeValidationConfig(t *testing.T) {
	t.Run("overlay phases merge with base", func(t *testing.T) {
		base := RecipeMetadataSpec{
			Validation: &ValidationConfig{
				Readiness: &ValidationPhase{
					Constraints: []Constraint{{Name: "K8s.server.version", Value: ">= 1.30"}},
				},
				Deployment: &ValidationPhase{
					Timeout: "5m",
					Checks:  []string{"operator-health"},
				},
			},
		}
		overlay := RecipeMetadataSpec{
			Validation: &ValidationConfig{
				Deployment: &ValidationPhase{
					Timeout: "10m",
					Checks:  []string{"operator-health", "expected-resources"},
				},
				Performance: &ValidationPhase{
					Timeout:        "15m",
					Infrastructure: "nccl-doctor",
				},
			},
		}
		base.Merge(&overlay)

		if base.Validation == nil {
			t.Fatal("validation should not be nil after merge")
		}
		if base.Validation.Readiness == nil {
			t.Fatal("readiness should be preserved from base")
		}
		if base.Validation.Readiness.Constraints[0].Name != "K8s.server.version" {
			t.Error("readiness constraints should be preserved from base")
		}
		if base.Validation.Deployment.Timeout != "10m" {
			t.Errorf("deployment timeout = %q, want 10m (from overlay)", base.Validation.Deployment.Timeout)
		}
		if len(base.Validation.Deployment.Checks) != 2 {
			t.Errorf("deployment checks len = %d, want 2 (from overlay)", len(base.Validation.Deployment.Checks))
		}
		if base.Validation.Performance == nil {
			t.Fatal("performance should be added from overlay")
		}
		if base.Validation.Performance.Infrastructure != "nccl-doctor" {
			t.Errorf("performance infrastructure = %q, want nccl-doctor", base.Validation.Performance.Infrastructure)
		}
	})

	t.Run("overlay validation into nil base", func(t *testing.T) {
		base := RecipeMetadataSpec{}
		overlay := RecipeMetadataSpec{
			Validation: &ValidationConfig{
				Deployment: &ValidationPhase{
					Checks: []string{"operator-health"},
				},
			},
		}
		base.Merge(&overlay)

		if base.Validation == nil {
			t.Fatal("validation should be set from overlay")
		}
		if base.Validation.Deployment == nil || base.Validation.Deployment.Checks[0] != "operator-health" {
			t.Error("deployment check should be set from overlay")
		}
	})

	t.Run("nil overlay validation preserves base", func(t *testing.T) {
		base := RecipeMetadataSpec{
			Validation: &ValidationConfig{
				Deployment: &ValidationPhase{
					Checks: []string{"operator-health"},
				},
			},
		}
		overlay := RecipeMetadataSpec{}
		base.Merge(&overlay)

		if base.Validation == nil || base.Validation.Deployment == nil {
			t.Fatal("validation should be preserved from base when overlay has nil")
		}
	})
}

func TestFinalizeRecipeResultIncludesValidation(t *testing.T) {
	spec := RecipeMetadataSpec{
		ComponentRefs: []ComponentRef{
			{Name: "gpu-operator", Type: "Helm", Source: "https://example.com"},
		},
		Validation: &ValidationConfig{
			Deployment: &ValidationPhase{
				Checks: []string{"operator-health"},
			},
		},
	}
	criteria := NewCriteria()
	result, err := finalizeRecipeResult(criteria, &spec, []string{"base"})
	if err != nil {
		t.Fatalf("finalizeRecipeResult() error: %v", err)
	}
	if result.Validation == nil {
		t.Fatal("result.Validation should not be nil")
	}
	if result.Validation.Deployment == nil {
		t.Fatal("result.Validation.Deployment should not be nil")
	}
	if result.Validation.Deployment.Checks[0] != "operator-health" {
		t.Errorf("check = %q, want operator-health", result.Validation.Deployment.Checks[0])
	}
}

// TestOverlayAddsNewComponent verifies that overlay recipes can add components
// that don't exist in the base recipe.
func TestOverlayAddsNewComponent(t *testing.T) {
	ctx := context.Background()

	// Build recipe for H100 EKS inference workload with dynamo platform
	// h100-eks-ubuntu-inference-dynamo.yaml adds kai-scheduler, dynamo-crds, dynamo-platform
	// which are NOT in base.yaml
	builder := NewBuilder()
	criteria := NewCriteria()
	criteria.Service = CriteriaServiceEKS
	criteria.Accelerator = CriteriaAcceleratorH100
	criteria.OS = CriteriaOSUbuntu
	criteria.Intent = CriteriaIntentInference
	criteria.Platform = CriteriaPlatformDynamo

	result, err := builder.BuildFromCriteria(ctx, criteria)
	if err != nil {
		t.Fatalf("BuildFromCriteria failed: %v", err)
	}

	if result == nil {
		t.Fatal("Recipe result is nil")
	}

	// Verify base components exist
	baseComponents := []string{"cert-manager", "gpu-operator", "nvsentinel", "skyhook-operator"}
	for _, name := range baseComponents {
		if comp := result.GetComponentRef(name); comp == nil {
			t.Errorf("Base component %q not found in result", name)
		}
	}

	// Verify overlay-added component exists
	dynamoPlatform := result.GetComponentRef("dynamo-platform")
	if dynamoPlatform == nil {
		t.Fatalf("dynamo-platform not found (should be added by h100-eks-ubuntu-inference-dynamo overlay)")
	}

	// Verify dynamo-platform properties
	if dynamoPlatform.Version == "" {
		t.Error("dynamo-platform has empty version")
	}
	if dynamoPlatform.Type != "Helm" {
		t.Errorf("dynamo-platform type = %q, want Helm", dynamoPlatform.Type)
	}
	if len(dynamoPlatform.DependencyRefs) == 0 {
		t.Error("dynamo-platform has no dependencies (should depend on dynamo-crds, cert-manager, kube-prometheus-stack)")
	}

	// Build recipe for EKS H100 training workload with kubeflow platform
	// h100-eks-ubuntu-training-kubeflow.yaml adds kubeflow-trainer which is NOT in base.yaml
	builder = NewBuilder()
	criteria = NewCriteria()
	criteria.Accelerator = CriteriaAcceleratorH100
	criteria.Intent = CriteriaIntentTraining
	criteria.Service = CriteriaServiceEKS
	criteria.OS = CriteriaOSUbuntu
	criteria.Platform = CriteriaPlatformKubeflow

	result, err = builder.BuildFromCriteria(ctx, criteria)
	if err != nil {
		t.Fatalf("BuildFromCriteria failed: %v", err)
	}

	if result == nil {
		t.Fatal("Recipe result is nil")
	}

	// Verify overlay-added component exists
	kubeflowTrainer := result.GetComponentRef("kubeflow-trainer")
	if kubeflowTrainer == nil {
		t.Fatalf("kubeflow-trainer not found (should be added by h100 kubeflow overlay)")
	}

	t.Logf("Successfully verified overlay can add new components")
	t.Logf("   Base components: %d", len(baseComponents))
	t.Logf("   Total components: %d", len(result.ComponentRefs))
	t.Logf("   dynamo-platform version: %s", dynamoPlatform.Version)
	t.Logf("   kubeflow-trainer version: %s", kubeflowTrainer.Version)
}

// TestOverlayMergeDoesNotLoseBaseComponents verifies that when overlays add
// components, base components are preserved.
func TestOverlayMergeDoesNotLoseBaseComponents(t *testing.T) {
	ctx := context.Background()
	builder := NewBuilder()

	// Build H100 EKS inference recipe with dynamo platform
	// Matches overlay chain that adds kgateway, dynamo-platform, kai-scheduler, etc.
	criteria := NewCriteria()
	criteria.Service = CriteriaServiceEKS
	criteria.Accelerator = CriteriaAcceleratorH100
	criteria.OS = CriteriaOSUbuntu
	criteria.Intent = CriteriaIntentInference
	criteria.Platform = CriteriaPlatformDynamo

	result, err := builder.BuildFromCriteria(ctx, criteria)
	if err != nil {
		t.Fatalf("BuildFromCriteria failed: %v", err)
	}

	// Verify all 4 base components exist
	expectedBaseComponents := []string{"cert-manager", "gpu-operator", "nvsentinel", "skyhook-operator"}
	for _, name := range expectedBaseComponents {
		if comp := result.GetComponentRef(name); comp == nil {
			t.Errorf("Base component %q missing from overlay result", name)
		}
	}

	// Verify dynamo-platform was added by overlay
	dynamoPlatform := result.GetComponentRef("dynamo-platform")
	if dynamoPlatform == nil {
		t.Error("dynamo-platform not found (should be added by overlay)")
	}

	// Result should have at least 5 components (4 base + 1 added)
	if len(result.ComponentRefs) < 5 {
		t.Errorf("Expected at least 5 components, got %d", len(result.ComponentRefs))
	}

	t.Logf("Base components preserved when overlay adds new components")
	t.Logf("   Total components: %d (4 base + additions)", len(result.ComponentRefs))
	if dynamoPlatform != nil {
		t.Logf("   dynamo-platform added: version %s", dynamoPlatform.Version)
	}
}

// TestInheritanceChain verifies that multi-level inheritance chains work correctly.
// Tests the chain: base → eks → eks-training → h100-eks-training → h100-eks-ubuntu-training → h100-eks-ubuntu-training-kubeflow
func TestInheritanceChain(t *testing.T) {
	ctx := context.Background()
	builder := NewBuilder()

	// Build H100 EKS training recipe with kubeflow platform
	criteria := NewCriteria()
	criteria.Service = CriteriaServiceEKS
	criteria.Accelerator = CriteriaAcceleratorH100
	criteria.OS = CriteriaOSUbuntu
	criteria.Intent = CriteriaIntentTraining
	criteria.Platform = CriteriaPlatformKubeflow

	result, err := builder.BuildFromCriteria(ctx, criteria)
	if err != nil {
		t.Fatalf("BuildFromCriteria failed: %v", err)
	}

	// Verify applied overlays includes the full chain
	appliedOverlays := result.Metadata.AppliedOverlays
	t.Logf("Applied overlays: %v", appliedOverlays)

	if len(appliedOverlays) < 2 {
		t.Errorf("Expected at least 2 applied overlays (base + matching), got %d: %v",
			len(appliedOverlays), appliedOverlays)
	}

	// Verify base components are present
	expectedComponents := []string{"cert-manager", "gpu-operator", "nvsentinel", "skyhook-operator"}
	for _, name := range expectedComponents {
		if comp := result.GetComponentRef(name); comp == nil {
			t.Errorf("Expected component %q not found in result", name)
		}
	}

	// Verify kubeflow-trainer was added by the kubeflow overlay
	kubeflowTrainer := result.GetComponentRef("kubeflow-trainer")
	if kubeflowTrainer == nil {
		t.Error("kubeflow-trainer should be added by h100-eks-ubuntu-training-kubeflow overlay")
	}

	// Verify gpu-operator has training values file (from eks-training)
	gpuOp := result.GetComponentRef("gpu-operator")
	if gpuOp == nil {
		t.Fatal("gpu-operator not found")
	}
	if gpuOp.ValuesFile != "components/gpu-operator/values-eks-training.yaml" {
		t.Errorf("Expected gpu-operator valuesFile from eks-training, got %q", gpuOp.ValuesFile)
	}

	t.Logf("Inheritance chain test passed")
	t.Logf("   Applied overlays: %v", appliedOverlays)
	t.Logf("   GPU operator version: %s", gpuOp.Version)
	t.Logf("   GPU operator valuesFile: %s", gpuOp.ValuesFile)
}

// TestInheritanceChainKubeflow verifies that kubeflow platform inherits correctly from eks-training.
func TestInheritanceChainKubeflow(t *testing.T) {
	ctx := context.Background()
	builder := NewBuilder()

	// Build H100 EKS training recipe with kubeflow platform
	criteria := NewCriteria()
	criteria.Service = CriteriaServiceEKS
	criteria.Accelerator = CriteriaAcceleratorH100
	criteria.OS = CriteriaOSUbuntu
	criteria.Intent = CriteriaIntentTraining
	criteria.Platform = CriteriaPlatformKubeflow

	result, err := builder.BuildFromCriteria(ctx, criteria)
	if err != nil {
		t.Fatalf("BuildFromCriteria failed: %v", err)
	}

	// Verify applied overlays
	t.Logf("Applied overlays: %v", result.Metadata.AppliedOverlays)

	// Verify kubeflow-trainer was added
	kubeflowTrainer := result.GetComponentRef("kubeflow-trainer")
	if kubeflowTrainer == nil {
		t.Fatal("kubeflow-trainer not found")
	}

	// Verify gpu-operator exists and has training values file
	gpuOp := result.GetComponentRef("gpu-operator")
	if gpuOp == nil {
		t.Fatal("gpu-operator not found")
	}

	// Verify training values file is inherited
	if gpuOp.ValuesFile != "components/gpu-operator/values-eks-training.yaml" {
		t.Errorf("Expected gpu-operator valuesFile from eks-training, got %q", gpuOp.ValuesFile)
	}

	t.Logf("Kubeflow inheritance chain test passed")
}

// TestInheritanceChainDoesNotDuplicateRecipes verifies that recipes in the inheritance
// chain are only applied once, even if they appear in multiple matching overlays' chains.
func TestInheritanceChainDoesNotDuplicateRecipes(t *testing.T) {
	ctx := context.Background()
	builder := NewBuilder()

	criteria := NewCriteria()
	criteria.Service = CriteriaServiceEKS
	criteria.Accelerator = CriteriaAcceleratorH100
	criteria.Intent = CriteriaIntentTraining
	criteria.OS = CriteriaOSUbuntu
	criteria.Platform = CriteriaPlatformKubeflow

	result, err := builder.BuildFromCriteria(ctx, criteria)
	if err != nil {
		t.Fatalf("BuildFromCriteria failed: %v", err)
	}

	// Count occurrences of each overlay in the applied list
	counts := make(map[string]int)
	for _, name := range result.Metadata.AppliedOverlays {
		counts[name]++
	}

	// Verify no duplicates
	for name, count := range counts {
		if count > 1 {
			t.Errorf("Recipe %q applied %d times (should be 1)", name, count)
		}
	}

	t.Logf("No duplicate recipes in chain: %v", result.Metadata.AppliedOverlays)
}

// TestComponentRefApplyRegistryDefaults verifies that ComponentRef.ApplyRegistryDefaults
// correctly applies defaults from ComponentConfig for both Helm and Kustomize components.
func TestComponentRefApplyRegistryDefaults(t *testing.T) {
	const (
		testHelmRepo       = "https://charts.example.com"
		testHelmRepoCustom = "https://custom.charts.com"
		testVersion1       = "v1.0.0"
		testVersion2       = "v2.0.0"
	)

	t.Run("helm defaults applied", func(t *testing.T) {
		config := &ComponentConfig{
			Name:        "test-helm",
			DisplayName: "Test Helm",
			Helm: HelmConfig{
				DefaultRepository: testHelmRepo,
				DefaultChart:      "example/chart",
				DefaultVersion:    testVersion1,
			},
		}

		ref := &ComponentRef{
			Name: "test-helm",
			// Type, Source, Version are empty - should be filled from defaults
		}

		ref.ApplyRegistryDefaults(config)

		if ref.Type != ComponentTypeHelm {
			t.Errorf("Type = %v, want %v", ref.Type, ComponentTypeHelm)
		}
		if ref.Source != testHelmRepo {
			t.Errorf("Source = %q, want %q", ref.Source, testHelmRepo)
		}
		if ref.Version != testVersion1 {
			t.Errorf("Version = %q, want %q", ref.Version, testVersion1)
		}
	})

	t.Run("helm defaults not overwritten", func(t *testing.T) {
		config := &ComponentConfig{
			Name:        "test-helm",
			DisplayName: "Test Helm",
			Helm: HelmConfig{
				DefaultRepository: testHelmRepo,
				DefaultChart:      "example/chart",
				DefaultVersion:    testVersion1,
			},
		}

		ref := &ComponentRef{
			Name:    "test-helm",
			Type:    ComponentTypeHelm,
			Source:  testHelmRepoCustom,
			Version: testVersion2,
		}

		ref.ApplyRegistryDefaults(config)

		// Should keep existing values
		if ref.Source != testHelmRepoCustom {
			t.Errorf("Source = %q, want %q (should not be overwritten)", ref.Source, testHelmRepoCustom)
		}
		if ref.Version != testVersion2 {
			t.Errorf("Version = %q, want %q (should not be overwritten)", ref.Version, testVersion2)
		}
	})

	t.Run("kustomize defaults applied", func(t *testing.T) {
		const (
			kustomizeSource = "https://github.com/example/repo"
			kustomizePath   = "deploy/production"
			kustomizeTag    = "v1.0.0"
		)

		config := &ComponentConfig{
			Name:        "test-kustomize",
			DisplayName: "Test Kustomize",
			Kustomize: KustomizeConfig{
				DefaultSource: kustomizeSource,
				DefaultPath:   kustomizePath,
				DefaultTag:    kustomizeTag,
			},
		}

		ref := &ComponentRef{
			Name: "test-kustomize",
			// Type, Source, Tag, Path are empty - should be filled from defaults
		}

		ref.ApplyRegistryDefaults(config)

		if ref.Type != ComponentTypeKustomize {
			t.Errorf("Type = %v, want %v", ref.Type, ComponentTypeKustomize)
		}
		if ref.Source != kustomizeSource {
			t.Errorf("Source = %q, want %q", ref.Source, kustomizeSource)
		}
		if ref.Tag != kustomizeTag {
			t.Errorf("Tag = %q, want %q", ref.Tag, kustomizeTag)
		}
		if ref.Path != kustomizePath {
			t.Errorf("Path = %q, want %q", ref.Path, kustomizePath)
		}
	})

	t.Run("kustomize defaults not overwritten", func(t *testing.T) {
		const (
			kustomizeSource       = "https://github.com/example/repo"
			kustomizePath         = "deploy/production"
			kustomizeTag          = "v1.0.0"
			kustomizeSourceCustom = "https://github.com/custom/repo"
			kustomizePathCustom   = "deploy/staging"
			kustomizeTagCustom    = "v2.0.0"
		)

		config := &ComponentConfig{
			Name:        "test-kustomize",
			DisplayName: "Test Kustomize",
			Kustomize: KustomizeConfig{
				DefaultSource: kustomizeSource,
				DefaultPath:   kustomizePath,
				DefaultTag:    kustomizeTag,
			},
		}

		ref := &ComponentRef{
			Name:   "test-kustomize",
			Type:   ComponentTypeKustomize,
			Source: kustomizeSourceCustom,
			Tag:    kustomizeTagCustom,
			Path:   kustomizePathCustom,
		}

		ref.ApplyRegistryDefaults(config)

		// Should keep existing values
		if ref.Source != kustomizeSourceCustom {
			t.Errorf("Source = %q, want %q (should not be overwritten)", ref.Source, kustomizeSourceCustom)
		}
		if ref.Tag != kustomizeTagCustom {
			t.Errorf("Tag = %q, want %q (should not be overwritten)", ref.Tag, kustomizeTagCustom)
		}
		if ref.Path != kustomizePathCustom {
			t.Errorf("Path = %q, want %q (should not be overwritten)", ref.Path, kustomizePathCustom)
		}
	})

	t.Run("nil config is safe", func(t *testing.T) {
		ref := &ComponentRef{
			Name: "test",
		}

		// Should not panic
		ref.ApplyRegistryDefaults(nil)

		// Values should be unchanged
		if ref.Type != "" {
			t.Errorf("Type = %q, want empty", ref.Type)
		}
	})

	t.Run("explicit type preserved", func(t *testing.T) {
		// Test that if a ComponentRef already has a type set, it's not changed
		config := &ComponentConfig{
			Name:        "test-helm",
			DisplayName: "Test Helm",
			Helm: HelmConfig{
				DefaultRepository: "https://charts.example.com",
			},
		}

		ref := &ComponentRef{
			Name: "test-helm",
			Type: ComponentTypeKustomize, // Explicit type set
		}

		ref.ApplyRegistryDefaults(config)

		// Type should not be changed
		if ref.Type != ComponentTypeKustomize {
			t.Errorf("Type = %v, want %v (should preserve explicit type)", ref.Type, ComponentTypeKustomize)
		}
		// Since type is Kustomize, Helm defaults should NOT be applied
		if ref.Source != "" {
			t.Errorf("Source = %q, want empty (helm defaults should not apply to kustomize type)", ref.Source)
		}
	})
}

// TestComponentRefMergeNamespaceAndChart verifies that Namespace and Chart fields
// are correctly merged when merging ComponentRefs (overlay into base).
func TestComponentRefMergeNamespaceAndChart(t *testing.T) {
	const gpuOp = "gpu-operator"

	t.Run("namespace and chart inherited from base", func(t *testing.T) {
		base := RecipeMetadataSpec{
			ComponentRefs: []ComponentRef{
				{
					Name:      gpuOp,
					Namespace: gpuOp,
					Chart:     gpuOp,
					Type:      ComponentTypeHelm,
					Version:   "v1.0.0",
				},
			},
		}

		overlay := RecipeMetadataSpec{
			ComponentRefs: []ComponentRef{
				{
					Name:    gpuOp,
					Version: "v2.0.0",
				},
			},
		}

		base.Merge(&overlay)

		comp := base.ComponentRefs[0]
		if comp.Namespace != gpuOp {
			t.Errorf("Namespace = %q, want %q (should be inherited from base)", comp.Namespace, gpuOp)
		}
		if comp.Chart != gpuOp {
			t.Errorf("Chart = %q, want %q (should be inherited from base)", comp.Chart, gpuOp)
		}
		if comp.Version != "v2.0.0" {
			t.Errorf("Version = %q, want %q (should be from overlay)", comp.Version, "v2.0.0")
		}
	})

	t.Run("namespace and chart overridden by overlay", func(t *testing.T) {
		base := RecipeMetadataSpec{
			ComponentRefs: []ComponentRef{
				{
					Name:      gpuOp,
					Namespace: gpuOp,
					Chart:     gpuOp,
					Type:      ComponentTypeHelm,
					Version:   "v1.0.0",
				},
			},
		}

		overlay := RecipeMetadataSpec{
			ComponentRefs: []ComponentRef{
				{
					Name:      gpuOp,
					Namespace: "custom-ns",
					Chart:     "custom-chart",
				},
			},
		}

		base.Merge(&overlay)

		comp := base.ComponentRefs[0]
		if comp.Namespace != "custom-ns" {
			t.Errorf("Namespace = %q, want %q (should be from overlay)", comp.Namespace, "custom-ns")
		}
		if comp.Chart != "custom-chart" {
			t.Errorf("Chart = %q, want %q (should be from overlay)", comp.Chart, "custom-chart")
		}
	})
}

// TestComponentRefApplyRegistryDefaults_NamespaceAndChart verifies that
// ApplyRegistryDefaults populates Namespace and Chart from HelmConfig.
func TestComponentRefApplyRegistryDefaults_NamespaceAndChart(t *testing.T) {
	const gpuOp = "gpu-operator"

	t.Run("namespace and chart applied from registry", func(t *testing.T) {
		config := &ComponentConfig{
			Name:        gpuOp,
			DisplayName: gpuOp,
			Helm: HelmConfig{
				DefaultRepository: "https://helm.ngc.nvidia.com/nvidia",
				DefaultChart:      "nvidia/gpu-operator",
				DefaultNamespace:  gpuOp,
			},
		}

		ref := &ComponentRef{Name: gpuOp}
		ref.ApplyRegistryDefaults(config)

		if ref.Namespace != gpuOp {
			t.Errorf("Namespace = %q, want %q", ref.Namespace, gpuOp)
		}
		if ref.Chart != gpuOp {
			t.Errorf("Chart = %q, want %q", ref.Chart, gpuOp)
		}
	})

	t.Run("existing namespace and chart not overwritten", func(t *testing.T) {
		config := &ComponentConfig{
			Name:        gpuOp,
			DisplayName: gpuOp,
			Helm: HelmConfig{
				DefaultRepository: "https://helm.ngc.nvidia.com/nvidia",
				DefaultChart:      "nvidia/gpu-operator",
				DefaultNamespace:  gpuOp,
			},
		}

		ref := &ComponentRef{
			Name:      gpuOp,
			Namespace: "custom-ns",
			Chart:     "custom-chart",
		}
		ref.ApplyRegistryDefaults(config)

		if ref.Namespace != "custom-ns" {
			t.Errorf("Namespace = %q, want %q (should not be overwritten)", ref.Namespace, "custom-ns")
		}
		if ref.Chart != "custom-chart" {
			t.Errorf("Chart = %q, want %q (should not be overwritten)", ref.Chart, "custom-chart")
		}
	})

	t.Run("chart extracted from slash-separated DefaultChart", func(t *testing.T) {
		config := &ComponentConfig{
			Name:        "kube-prometheus-stack",
			DisplayName: "kube-prometheus-stack",
			Helm: HelmConfig{
				DefaultChart:     "prometheus-community/kube-prometheus-stack",
				DefaultNamespace: "nvidia-system",
			},
		}

		ref := &ComponentRef{Name: "kube-prometheus-stack"}
		ref.ApplyRegistryDefaults(config)

		if ref.Chart != "kube-prometheus-stack" {
			t.Errorf("Chart = %q, want %q (should extract after /)", ref.Chart, "kube-prometheus-stack")
		}
	})
}

// TestComponentRefMergeWithPath verifies that the Path field is correctly merged
// when merging ComponentRefs (overlay into base).
func TestComponentRefMergeWithPath(t *testing.T) {
	t.Run("path inherited from base", func(t *testing.T) {
		base := RecipeMetadataSpec{
			ComponentRefs: []ComponentRef{
				{
					Name:   "my-kustomize-app",
					Type:   ComponentTypeKustomize,
					Source: "https://github.com/example/repo",
					Path:   "deploy/production",
					Tag:    "v1.0.0",
				},
			},
		}

		// Overlay only specifies name and new tag
		overlay := RecipeMetadataSpec{
			ComponentRefs: []ComponentRef{
				{
					Name: "my-kustomize-app",
					Tag:  "v2.0.0",
				},
			},
		}

		base.Merge(&overlay)

		if len(base.ComponentRefs) != 1 {
			t.Fatalf("expected 1 component, got %d", len(base.ComponentRefs))
		}

		comp := base.ComponentRefs[0]

		// Path should be inherited from base
		if comp.Path != "deploy/production" {
			t.Errorf("Path = %q, want %q (should be inherited from base)", comp.Path, "deploy/production")
		}
		// Tag should be overridden by overlay
		if comp.Tag != "v2.0.0" {
			t.Errorf("Tag = %q, want %q (should be from overlay)", comp.Tag, "v2.0.0")
		}
	})

	t.Run("path overridden by overlay", func(t *testing.T) {
		base := RecipeMetadataSpec{
			ComponentRefs: []ComponentRef{
				{
					Name:   "my-kustomize-app",
					Type:   ComponentTypeKustomize,
					Source: "https://github.com/example/repo",
					Path:   "deploy/production",
					Tag:    "v1.0.0",
				},
			},
		}

		// Overlay specifies a new path
		overlay := RecipeMetadataSpec{
			ComponentRefs: []ComponentRef{
				{
					Name: "my-kustomize-app",
					Path: "deploy/staging",
				},
			},
		}

		base.Merge(&overlay)

		comp := base.ComponentRefs[0]

		// Path should be overridden by overlay
		if comp.Path != "deploy/staging" {
			t.Errorf("Path = %q, want %q (should be from overlay)", comp.Path, "deploy/staging")
		}
	})
}
