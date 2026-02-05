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
				if !containsString(err.Error(), tt.errMsg) {
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

// Helper function
func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && (s[:len(substr)] == substr || containsString(s[1:], substr)))
}

// TestOverlayAddsNewComponent verifies that overlay recipes can add components
// that don't exist in the base recipe.
func TestOverlayAddsNewComponent(t *testing.T) {
	ctx := context.Background()

	// Build recipe for H100 inference workload
	// h100-ubuntu-inference.yaml adds network-operator which is NOT in base.yaml
	// Note: The overlay file requires accelerator=h100, os=ubuntu, intent=inference
	// so we must specify all criteria to match it (asymmetric matching).
	builder := NewBuilder()
	criteria := NewCriteria()
	criteria.Accelerator = CriteriaAcceleratorH100
	criteria.OS = CriteriaOSUbuntu
	criteria.Intent = CriteriaIntentInference

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
	networkOp := result.GetComponentRef("network-operator")
	if networkOp == nil {
		t.Fatalf("network-operator not found (should be added by h100-inference overlay)")
	}

	// Verify network-operator properties
	if networkOp.Version == "" {
		t.Error("network-operator has empty version")
	}
	if networkOp.Type != "Helm" {
		t.Errorf("network-operator type = %q, want Helm", networkOp.Type)
	}
	if len(networkOp.DependencyRefs) == 0 {
		t.Error("network-operator has no dependencies (should depend on cert-manager)")
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
	t.Logf("   network-operator version: %s", networkOp.Version)
	t.Logf("   kubeflow-trainer version: %s", kubeflowTrainer.Version)
}

// TestOverlayMergeDoesNotLoseBaseComponents verifies that when overlays add
// components, base components are preserved.
func TestOverlayMergeDoesNotLoseBaseComponents(t *testing.T) {
	ctx := context.Background()
	builder := NewBuilder()

	// Build H100 inference recipe (matches overlay that adds network-operator)
	// Note: The h100-ubuntu-inference.yaml overlay requires all three criteria
	// (accelerator=h100, os=ubuntu, intent=inference) to match due to asymmetric matching.
	criteria := NewCriteria()
	criteria.Accelerator = CriteriaAcceleratorH100
	criteria.OS = CriteriaOSUbuntu
	criteria.Intent = CriteriaIntentInference

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

	// Verify network-operator was added
	networkOp := result.GetComponentRef("network-operator")
	if networkOp == nil {
		t.Error("network-operator not found (should be added by overlay)")
	}

	// Result should have at least 5 components (4 base + 1 added)
	if len(result.ComponentRefs) < 5 {
		t.Errorf("Expected at least 5 components, got %d", len(result.ComponentRefs))
	}

	t.Logf("Base components preserved when overlay adds new components")
	t.Logf("   Total components: %d (4 base + additions)", len(result.ComponentRefs))
	if networkOp != nil {
		t.Logf("   network-operator added: version %s", networkOp.Version)
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
