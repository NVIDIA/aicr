// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package bundler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestNew(t *testing.T) {
	t.Run("default bundler", func(t *testing.T) {
		bundler, err := New()
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		if bundler == nil {
			t.Fatal("New() returned nil bundler")
		}
		if bundler.Config == nil {
			t.Fatal("New() bundler has nil config")
		}
	})

	t.Run("with config", func(t *testing.T) {
		cfg := config.NewConfig(
			config.WithVersion("v1.0.0"),
		)
		bundler, err := New(WithConfig(cfg))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		if bundler.Config.Version() != "v1.0.0" {
			t.Errorf("expected version v1.0.0, got %s", bundler.Config.Version())
		}
	})

	t.Run("with nil config", func(t *testing.T) {
		bundler, err := New(WithConfig(nil))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		// Should use default config when nil is passed
		if bundler.Config == nil {
			t.Fatal("Config should not be nil after passing nil")
		}
	})
}

func TestNew_AttestWithoutBinaryAttestation(t *testing.T) {
	// The test binary won't have an attestation file next to it,
	// simulating a "go install" or manual download scenario.
	cfg := config.NewConfig(config.WithAttest(true))
	_, err := New(WithConfig(cfg))
	if err == nil {
		t.Fatal("New() with attest=true should fail when binary attestation file is missing")
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "NOT_FOUND") {
		t.Errorf("expected NOT_FOUND error code, got: %v", err)
	}
	if !strings.Contains(errMsg, "install script") {
		t.Errorf("error should mention install script, got: %v", err)
	}
	if !strings.Contains(errMsg, "--attest") {
		t.Errorf("error should mention --attest flag, got: %v", err)
	}
}

func TestNewWithConfig(t *testing.T) {
	t.Run("nil config uses default", func(t *testing.T) {
		bundler, err := NewWithConfig(nil)
		if err != nil {
			t.Fatalf("NewWithConfig(nil) error = %v", err)
		}
		if bundler.Config == nil {
			t.Fatal("Config should not be nil")
		}
	})

	t.Run("valid config", func(t *testing.T) {
		cfg := config.NewConfig(config.WithVersion("v2.0.0"))
		bundler, err := NewWithConfig(cfg)
		if err != nil {
			t.Fatalf("NewWithConfig() error = %v", err)
		}
		if bundler.Config.Version() != "v2.0.0" {
			t.Errorf("expected version v2.0.0, got %s", bundler.Config.Version())
		}
	})

	t.Run("equivalent to New(WithConfig())", func(t *testing.T) {
		cfg := config.NewConfig(config.WithVersion("v3.0.0"))
		b1, err := NewWithConfig(cfg)
		if err != nil {
			t.Fatalf("NewWithConfig() error = %v", err)
		}
		b2, err := New(WithConfig(cfg))
		if err != nil {
			t.Fatalf("New(WithConfig()) error = %v", err)
		}
		if b1.Config.Version() != b2.Config.Version() {
			t.Errorf("versions differ: NewWithConfig=%s, New(WithConfig)=%s",
				b1.Config.Version(), b2.Config.Version())
		}
	})
}

func TestWithAllowLists(t *testing.T) {
	t.Run("nil allowlists", func(t *testing.T) {
		bundler, err := New(WithAllowLists(nil))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		if bundler.AllowLists != nil {
			t.Error("AllowLists should be nil")
		}
	})

	t.Run("valid allowlists", func(t *testing.T) {
		al := &recipe.AllowLists{
			Services: []recipe.CriteriaServiceType{"eks", "gke"},
		}
		bundler, err := New(WithAllowLists(al))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		if bundler.AllowLists == nil {
			t.Fatal("AllowLists should not be nil")
		}
		if len(bundler.AllowLists.Services) != 2 {
			t.Errorf("expected 2 services, got %d", len(bundler.AllowLists.Services))
		}
	})
}

func TestMake_NilInput(t *testing.T) {
	bundler, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx := context.Background()
	tmpDir := t.TempDir()

	_, err = bundler.Make(ctx, nil, tmpDir)
	if err == nil {
		t.Fatal("expected error for nil input, got nil")
	}
	if !strings.Contains(err.Error(), "nil") {
		t.Errorf("expected error to mention nil, got: %v", err)
	}
}

func TestMake_EmptyComponentRefs(t *testing.T) {
	bundler, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx := context.Background()
	tmpDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{},
	}

	_, err = bundler.Make(ctx, recipeResult, tmpDir)
	if err == nil {
		t.Fatal("expected error for empty component refs, got nil")
	}
	if !strings.Contains(err.Error(), "component") {
		t.Errorf("expected error to mention component, got: %v", err)
	}
}

func TestMake_Success(t *testing.T) {
	bundler, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx := context.Background()
	tmpDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Kind:       "Recipe",
		Criteria: &recipe.Criteria{
			Service:     "eks",
			Accelerator: "gb200",
			Intent:      "training",
			OS:          "ubuntu",
		},
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:    "gpu-operator",
				Version: "v25.3.3",
				Type:    "helm",
				Source:  "https://helm.ngc.nvidia.com/nvidia",
			},
			{
				Name:    "network-operator",
				Version: "v25.4.0",
				Type:    "helm",
				Source:  "https://helm.ngc.nvidia.com/nvidia",
			},
		},
		DeploymentOrder: []string{"gpu-operator", "network-operator"},
	}

	output, err := bundler.Make(ctx, recipeResult, tmpDir)
	if err != nil {
		t.Fatalf("Make() error = %v", err)
	}

	if output == nil {
		t.Fatal("Make() returned nil output")
	}

	// Verify root files were created
	rootFiles := []string{"README.md", "deploy.sh", "recipe.yaml"}
	for _, filename := range rootFiles {
		path := filepath.Join(tmpDir, filename)
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			t.Errorf("expected file %s was not created", filename)
		}
	}

	// Verify per-component directories
	for _, comp := range []string{"gpu-operator", "network-operator"} {
		valuesPath := filepath.Join(tmpDir, comp, "values.yaml")
		if _, statErr := os.Stat(valuesPath); os.IsNotExist(statErr) {
			t.Errorf("expected %s/values.yaml was not created", comp)
		}
		readmePath := filepath.Join(tmpDir, comp, "README.md")
		if _, statErr := os.Stat(readmePath); os.IsNotExist(statErr) {
			t.Errorf("expected %s/README.md was not created", comp)
		}
	}

	// No Chart.yaml should exist
	chartPath := filepath.Join(tmpDir, "Chart.yaml")
	if _, statErr := os.Stat(chartPath); !os.IsNotExist(statErr) {
		t.Error("Chart.yaml should not exist in per-component bundle")
	}

	// Verify output summary (3 root + 2 components × 2 files = 7, +1 recipe.yaml = 8)
	if output.TotalFiles < 7 {
		t.Errorf("expected at least 7 files, got %d", output.TotalFiles)
	}
}

func TestMake_DisabledComponentsFiltered(t *testing.T) {
	bundler, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx := context.Background()
	tmpDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Kind:       "Recipe",
		Criteria: &recipe.Criteria{
			Service:     "eks",
			Accelerator: "h100",
			Intent:      "training",
		},
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:    "gpu-operator",
				Version: "v25.3.3",
				Type:    "helm",
				Source:  "https://helm.ngc.nvidia.com/nvidia",
			},
			{
				Name:      "aws-ebs-csi-driver",
				Version:   "2.55.0",
				Type:      "helm",
				Source:    "https://kubernetes-sigs.github.io/aws-ebs-csi-driver",
				Overrides: map[string]any{"enabled": false},
			},
		},
		DeploymentOrder: []string{"gpu-operator", "aws-ebs-csi-driver"},
	}

	output, err := bundler.Make(ctx, recipeResult, tmpDir)
	if err != nil {
		t.Fatalf("Make() error = %v", err)
	}

	if output == nil {
		t.Fatal("Make() returned nil output")
	}

	// Enabled component should have a directory
	if _, statErr := os.Stat(filepath.Join(tmpDir, "gpu-operator", "values.yaml")); os.IsNotExist(statErr) {
		t.Error("expected gpu-operator/values.yaml to be created")
	}

	// Disabled component should NOT have a directory
	if _, statErr := os.Stat(filepath.Join(tmpDir, "aws-ebs-csi-driver")); !os.IsNotExist(statErr) {
		t.Error("expected aws-ebs-csi-driver directory to NOT be created")
	}

	// deploy.sh should not reference the disabled component
	deployScript, readErr := os.ReadFile(filepath.Join(tmpDir, "deploy.sh"))
	if readErr != nil {
		t.Fatalf("failed to read deploy.sh: %v", readErr)
	}
	if strings.Contains(string(deployScript), "aws-ebs-csi-driver") {
		t.Error("deploy.sh should not contain aws-ebs-csi-driver")
	}
}

func TestMake_SetEnabledOverridesPrecedence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		recipeEnabled  *bool // nil = no override, true/false = overrides.enabled
		setEnabled     string
		expectIncluded bool
	}{
		{
			name:           "recipe disabled + --set enabled=true => included",
			recipeEnabled:  boolPtr(false),
			setEnabled:     "true",
			expectIncluded: true,
		},
		{
			name:           "recipe enabled + --set enabled=false => excluded",
			recipeEnabled:  nil,
			setEnabled:     "false",
			expectIncluded: false,
		},
		{
			name:           "recipe disabled + no --set => excluded",
			recipeEnabled:  boolPtr(false),
			setEnabled:     "",
			expectIncluded: false,
		},
		{
			name:           "recipe enabled (default) + no --set => included",
			recipeEnabled:  nil,
			setEnabled:     "",
			expectIncluded: true,
		},
		{
			name:           "invalid --set value ignored => falls back to recipe",
			recipeEnabled:  boolPtr(false),
			setEnabled:     "ture",
			expectIncluded: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var bundlerOpts []Option
			if tt.setEnabled != "" {
				cfg := config.NewConfig(
					config.WithValueOverrides(map[string]map[string]string{
						"awsebscsidriver": {"enabled": tt.setEnabled},
					}),
				)
				bundlerOpts = append(bundlerOpts, WithConfig(cfg))
			}

			bundler, err := New(bundlerOpts...)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			overrides := map[string]any{}
			if tt.recipeEnabled != nil {
				overrides["enabled"] = *tt.recipeEnabled
			}

			recipeResult := &recipe.RecipeResult{
				APIVersion: "aicr.nvidia.com/v1alpha1",
				Kind:       "Recipe",
				Criteria:   &recipe.Criteria{Service: "eks", Accelerator: "h100", Intent: "training"},
				ComponentRefs: []recipe.ComponentRef{
					{Name: "gpu-operator", Version: "v25.3.3", Type: "helm", Source: "https://helm.ngc.nvidia.com/nvidia"},
					{Name: "aws-ebs-csi-driver", Version: "2.55.0", Type: "helm", Source: "https://kubernetes-sigs.github.io/aws-ebs-csi-driver", Overrides: overrides},
				},
				DeploymentOrder: []string{"gpu-operator", "aws-ebs-csi-driver"},
			}

			ctx := context.Background()
			tmpDir := t.TempDir()
			_, makeErr := bundler.Make(ctx, recipeResult, tmpDir)
			if makeErr != nil {
				t.Fatalf("Make() error = %v", makeErr)
			}

			_, statErr := os.Stat(filepath.Join(tmpDir, "aws-ebs-csi-driver"))
			included := !os.IsNotExist(statErr)

			if included != tt.expectIncluded {
				t.Errorf("aws-ebs-csi-driver included=%v, want %v", included, tt.expectIncluded)
			}
		})
	}
}

func TestMake_SetEnabledNotLeakedToHelmValues(t *testing.T) {
	t.Parallel()

	cfg := config.NewConfig(
		config.WithValueOverrides(map[string]map[string]string{
			"awsebscsidriver": {"enabled": "true", "controller.replicaCount": "2"},
		}),
	)
	bundler, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	recipeResult := &recipe.RecipeResult{
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Kind:       "Recipe",
		Criteria:   &recipe.Criteria{Service: "eks", Accelerator: "h100", Intent: "training"},
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator", Version: "v25.3.3", Type: "helm", Source: "https://helm.ngc.nvidia.com/nvidia"},
			{Name: "aws-ebs-csi-driver", Version: "2.55.0", Type: "helm", Source: "https://kubernetes-sigs.github.io/aws-ebs-csi-driver"},
		},
		DeploymentOrder: []string{"gpu-operator", "aws-ebs-csi-driver"},
	}

	ctx := context.Background()
	tmpDir := t.TempDir()
	_, makeErr := bundler.Make(ctx, recipeResult, tmpDir)
	if makeErr != nil {
		t.Fatalf("Make() error = %v", makeErr)
	}

	valuesPath := filepath.Join(tmpDir, "aws-ebs-csi-driver", "values.yaml")
	valuesData, readErr := os.ReadFile(valuesPath)
	if readErr != nil {
		t.Fatalf("failed to read values.yaml: %v", readErr)
	}

	// "enabled" must not appear as a top-level key in the values file
	valuesStr := string(valuesData)
	if strings.Contains(valuesStr, "enabled: true") {
		t.Errorf("enabled key leaked into Helm values:\n%s", valuesStr)
	}

	// Other overrides should still be applied
	if !strings.Contains(valuesStr, "replicaCount") {
		t.Errorf("expected controller.replicaCount override in values, got:\n%s", valuesStr)
	}
}

func boolPtr(b bool) *bool { return &b }

func TestMake_WithValueOverrides(t *testing.T) {
	cfg := config.NewConfig(
		config.WithValueOverrides(map[string]map[string]string{
			"gpu-operator": {
				"gds.enabled": "true",
			},
		}),
	)
	bundler, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx := context.Background()
	tmpDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Kind:       "Recipe",
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:    "gpu-operator",
				Version: "v25.3.3",
				Type:    "helm",
				Source:  "https://helm.ngc.nvidia.com/nvidia",
			},
		},
	}

	output, err := bundler.Make(ctx, recipeResult, tmpDir)
	if err != nil {
		t.Fatalf("Make() error = %v", err)
	}

	if output == nil {
		t.Fatal("Make() returned nil output")
	}

	// Verify gpu-operator/values.yaml was created
	valuesPath := filepath.Join(tmpDir, "gpu-operator", "values.yaml")
	if _, err := os.Stat(valuesPath); os.IsNotExist(err) {
		t.Fatal("gpu-operator/values.yaml was not created")
	}
}

func TestMake_WithNodeSelectors(t *testing.T) {
	cfg := config.NewConfig(
		config.WithSystemNodeSelector(map[string]string{
			"nodeGroup": "system-pool",
		}),
		config.WithAcceleratedNodeSelector(map[string]string{
			"nvidia.com/gpu.present": "true",
		}),
	)
	bundler, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx := context.Background()
	tmpDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Kind:       "Recipe",
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:    "gpu-operator",
				Version: "v25.3.3",
				Type:    "helm",
				Source:  "https://helm.ngc.nvidia.com/nvidia",
			},
		},
	}

	output, err := bundler.Make(ctx, recipeResult, tmpDir)
	if err != nil {
		t.Fatalf("Make() error = %v", err)
	}

	if output == nil {
		t.Fatal("Make() returned nil output")
	}
}

func TestMake_WithTolerations(t *testing.T) {
	cfg := config.NewConfig(
		config.WithSystemNodeTolerations([]corev1.Toleration{
			{
				Key:      "dedicated",
				Operator: corev1.TolerationOpEqual,
				Value:    "system",
				Effect:   corev1.TaintEffectNoSchedule,
			},
		}),
	)
	bundler, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx := context.Background()
	tmpDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Kind:       "Recipe",
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:    "gpu-operator",
				Version: "v25.3.3",
				Type:    "helm",
				Source:  "https://helm.ngc.nvidia.com/nvidia",
			},
		},
	}

	output, err := bundler.Make(ctx, recipeResult, tmpDir)
	if err != nil {
		t.Fatalf("Make() error = %v", err)
	}

	if output == nil {
		t.Fatal("Make() returned nil output")
	}
}

func TestMake_ContextCancellation(t *testing.T) {
	bundler, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	tmpDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Kind:       "Recipe",
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:    "gpu-operator",
				Version: "v25.3.3",
				Type:    "helm",
				Source:  "https://helm.ngc.nvidia.com/nvidia",
			},
		},
	}

	_, err = bundler.Make(ctx, recipeResult, tmpDir)
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}

func TestMake_DefaultOutputDir(t *testing.T) {
	bundler, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx := context.Background()

	recipeResult := &recipe.RecipeResult{
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Kind:       "Recipe",
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:    "gpu-operator",
				Version: "v25.3.3",
				Type:    "helm",
				Source:  "https://helm.ngc.nvidia.com/nvidia",
			},
		},
	}

	// Use current working directory
	originalDir, _ := os.Getwd()
	tmpDir := t.TempDir()
	defer os.Chdir(originalDir)
	os.Chdir(tmpDir)

	output, err := bundler.Make(ctx, recipeResult, "")
	if err != nil {
		t.Fatalf("Make() error = %v", err)
	}

	if output == nil {
		t.Fatal("Make() returned nil output")
	}
}

func TestMake_ArgoCD(t *testing.T) {
	cfg := config.NewConfig(
		config.WithDeployer(config.DeployerArgoCD),
		config.WithRepoURL("https://github.com/org/repo.git"),
		config.WithVersion("v1.0.0"),
	)
	bundler, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx := context.Background()
	tmpDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Kind:       "Recipe",
		Criteria: &recipe.Criteria{
			Service:     "eks",
			Accelerator: "h100",
			Intent:      "training",
		},
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:    "gpu-operator",
				Version: "v25.3.3",
				Type:    "helm",
				Source:  "https://helm.ngc.nvidia.com/nvidia",
			},
			{
				Name:    "network-operator",
				Version: "v25.4.0",
				Type:    "helm",
				Source:  "https://helm.ngc.nvidia.com/nvidia",
			},
		},
		DeploymentOrder: []string{"gpu-operator", "network-operator"},
	}

	output, err := bundler.Make(ctx, recipeResult, tmpDir)
	if err != nil {
		t.Fatalf("Make() error = %v", err)
	}

	if output == nil {
		t.Fatal("Make() returned nil output")
	}

	// Argo CD output should have results
	if len(output.Results) == 0 {
		t.Error("expected at least 1 result")
	}

	// Check the result type
	for _, r := range output.Results {
		if r.Type != "argocd-applications" {
			t.Errorf("result type = %q, want %q", r.Type, "argocd-applications")
		}
		if !r.Success {
			t.Error("expected successful result")
		}
	}

	// Verify deployment info
	if output.Deployment == nil {
		t.Fatal("expected deployment info")
	}
	if output.Deployment.Type != "Argo CD applications" {
		t.Errorf("deployment type = %q, want %q", output.Deployment.Type, "Argo CD applications")
	}

	// Verify output directory has files
	if output.TotalFiles == 0 {
		t.Error("expected generated files")
	}
}

func TestRemoveHyphens(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"gpu-operator", "gpuoperator"},
		{"network-operator", "networkoperator"},
		{"cert-manager", "certmanager"},
		{"skyhook-operator", "skyhookoperator"},
		{"", ""},
		{"a-b-c-d", "abcd"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := removeHyphens(tt.input)
			if result != tt.expected {
				t.Errorf("removeHyphens(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

// Note: Tests for convertMapValue, setMapValueByPath, and applyMapOverrides
// are in pkg/component/overrides_test.go since those functions now live there.

func TestGetValueOverridesForComponent(t *testing.T) {
	tests := []struct {
		name          string
		overrides     map[string]map[string]string
		componentName string
		wantOverrides bool
		wantKey       string
		wantValue     string
	}{
		{
			name:          "nil config overrides",
			overrides:     nil,
			componentName: "gpu-operator",
			wantOverrides: false,
		},
		{
			name: "exact name match",
			overrides: map[string]map[string]string{
				"gpu-operator": {"driver.enabled": "true"},
			},
			componentName: "gpu-operator",
			wantOverrides: true,
			wantKey:       "driver.enabled",
			wantValue:     "true",
		},
		{
			name: "no match returns nil",
			overrides: map[string]map[string]string{
				"network-operator": {"enabled": "true"},
			},
			componentName: "gpu-operator",
			wantOverrides: false,
		},
		{
			name: "override key match via registry",
			overrides: map[string]map[string]string{
				"gpuoperator": {"driver.enabled": "true"},
			},
			componentName: "gpu-operator",
			wantOverrides: true,
			wantKey:       "driver.enabled",
			wantValue:     "true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.NewConfig(
				config.WithValueOverrides(tt.overrides),
			)
			b, err := New(WithConfig(cfg))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			result := b.getValueOverridesForComponent(tt.componentName)
			if tt.wantOverrides && result == nil {
				t.Error("expected overrides, got nil")
			}
			if !tt.wantOverrides && result != nil {
				t.Errorf("expected nil overrides, got %v", result)
			}
			if tt.wantOverrides && result != nil {
				if v, ok := result[tt.wantKey]; !ok || v != tt.wantValue {
					t.Errorf("override[%q] = %q, want %q", tt.wantKey, v, tt.wantValue)
				}
			}
		})
	}
}

// TestApplyNodeSchedulingOverrides_EstimatedNodeCount verifies that when Config has
// EstimatedNodeCount() > 0 and the component has nodeCountPaths, the value is written
// to the values map via ApplyMapOverrides (and thus appears as an int for Helm).
func TestApplyNodeSchedulingOverrides_EstimatedNodeCount(t *testing.T) {
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry() error = %v", err)
	}
	comp := registry.Get("skyhook-operator")
	if comp == nil || len(comp.GetNodeCountPaths()) == 0 {
		t.Skip("skyhook-operator with nodeCountPaths not in registry; skipping estimated node count path test")
	}

	cfg := config.NewConfig(config.WithEstimatedNodeCount(8))
	b, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	values := make(map[string]any)
	b.applyNodeSchedulingOverrides("skyhook-operator", values)

	// Path "estimatedNodeCount" is in skyhook-operator's nodeCountPaths; convertMapValue produces int64.
	got, ok := values["estimatedNodeCount"]
	if !ok {
		t.Fatal("estimatedNodeCount not set in values map")
	}
	var want int64 = 8
	switch v := got.(type) {
	case int64:
		if v != want {
			t.Errorf("estimatedNodeCount = %d, want %d", v, want)
		}
	case int:
		if int64(v) != want {
			t.Errorf("estimatedNodeCount = %d, want %d", v, want)
		}
	default:
		t.Errorf("estimatedNodeCount type = %T, value = %v (want int/int64)", got, got)
	}
}

func TestCollectComponentManifests(t *testing.T) {
	bundler, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	t.Run("empty manifest files", func(t *testing.T) {
		recipeResult := &recipe.RecipeResult{
			ComponentRefs: []recipe.ComponentRef{
				{
					Name:          "gpu-operator",
					ManifestFiles: []string{},
				},
			},
		}

		contents, err := bundler.collectComponentManifests(context.Background(), recipeResult)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(contents) != 0 {
			t.Errorf("expected 0 contents, got %d", len(contents))
		}
	})

	t.Run("no components", func(t *testing.T) {
		recipeResult := &recipe.RecipeResult{
			ComponentRefs: []recipe.ComponentRef{},
		}

		contents, err := bundler.collectComponentManifests(context.Background(), recipeResult)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(contents) != 0 {
			t.Errorf("expected 0 contents, got %d", len(contents))
		}
	})

	t.Run("invalid manifest path", func(t *testing.T) {
		recipeResult := &recipe.RecipeResult{
			ComponentRefs: []recipe.ComponentRef{
				{
					Name:          "gpu-operator",
					ManifestFiles: []string{"nonexistent/file.yaml"},
				},
			},
		}

		_, err := bundler.collectComponentManifests(context.Background(), recipeResult)
		if err == nil {
			t.Fatal("expected error for invalid manifest path")
		}
		if !strings.Contains(err.Error(), "nonexistent/file.yaml") {
			t.Errorf("error should mention the invalid file: %v", err)
		}
	})

	t.Run("empty manifests for multiple components", func(t *testing.T) {
		recipeResult := &recipe.RecipeResult{
			ComponentRefs: []recipe.ComponentRef{
				{
					Name:          "component-a",
					ManifestFiles: []string{},
				},
				{
					Name:          "component-b",
					ManifestFiles: []string{},
				},
			},
		}

		contents, err := bundler.collectComponentManifests(context.Background(), recipeResult)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(contents) != 0 {
			t.Errorf("expected 0 contents, got %d", len(contents))
		}
	})
}

// TestMake_Reproducible verifies that bundle generation is deterministic.
// Running Make() twice with the same input should produce identical output.
func TestMake_Reproducible(t *testing.T) {
	recipeResult := &recipe.RecipeResult{
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Kind:       "Recipe",
		Criteria: &recipe.Criteria{
			Service:     "eks",
			Accelerator: "gb200",
			Intent:      "training",
			OS:          "ubuntu",
		},
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:    "gpu-operator",
				Version: "v25.3.3",
				Type:    "helm",
				Source:  "https://helm.ngc.nvidia.com/nvidia",
			},
			{
				Name:    "network-operator",
				Version: "v25.4.0",
				Type:    "helm",
				Source:  "https://helm.ngc.nvidia.com/nvidia",
			},
		},
		DeploymentOrder: []string{"gpu-operator", "network-operator"},
	}

	// Generate bundles twice in different directories
	var fileHashes [2]map[string]string

	for i := 0; i < 2; i++ {
		bundler, err := New()
		if err != nil {
			t.Fatalf("iteration %d: New() error = %v", i, err)
		}

		ctx := context.Background()
		tmpDir := t.TempDir()

		_, err = bundler.Make(ctx, recipeResult, tmpDir)
		if err != nil {
			t.Fatalf("iteration %d: Make() error = %v", i, err)
		}

		// Compute file hashes
		fileHashes[i] = make(map[string]string)
		err = filepath.Walk(tmpDir, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if info.IsDir() {
				return nil
			}

			content, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}

			// Use relative path as key for comparison
			relPath, _ := filepath.Rel(tmpDir, path)
			hash := computeTestChecksum(content)
			fileHashes[i][relPath] = hash
			return nil
		})
		if err != nil {
			t.Fatalf("iteration %d: failed to walk directory: %v", i, err)
		}
	}

	// Compare file sets
	if len(fileHashes[0]) != len(fileHashes[1]) {
		t.Errorf("different number of files: iteration 1 has %d, iteration 2 has %d",
			len(fileHashes[0]), len(fileHashes[1]))
	}

	// Compare individual file hashes
	for filename, hash1 := range fileHashes[0] {
		hash2, exists := fileHashes[1][filename]
		if !exists {
			t.Errorf("file %s exists in iteration 1 but not iteration 2", filename)
			continue
		}
		if hash1 != hash2 {
			t.Errorf("file %s has different content between iterations:\n  iteration 1: %s\n  iteration 2: %s",
				filename, hash1, hash2)
		}
	}

	// Check for files only in iteration 2
	for filename := range fileHashes[1] {
		if _, exists := fileHashes[0][filename]; !exists {
			t.Errorf("file %s exists in iteration 2 but not iteration 1", filename)
		}
	}

	t.Logf("Reproducibility verified: both iterations produced %d identical files", len(fileHashes[0]))
}

func TestMake_DynamicValuesUnknownComponent(t *testing.T) {
	cfg := config.NewConfig(
		config.WithDynamicValues(map[string][]string{
			"nonexistent-component": {"some.path"},
		}),
	)
	bundler, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	recipeResult := &recipe.RecipeResult{
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Kind:       "RecipeResult",
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:    "gpu-operator",
				Version: "v25.3.3",
				Type:    "helm",
				Source:  "https://helm.ngc.nvidia.com/nvidia",
			},
		},
	}

	_, err = bundler.Make(context.Background(), recipeResult, t.TempDir())
	if err == nil {
		t.Fatal("expected error for unknown component in --dynamic, got nil")
	}
	if !strings.Contains(err.Error(), "nonexistent-component") {
		t.Errorf("error should mention the unknown component, got: %v", err)
	}
}

func TestMake_DynamicValuesValidComponent(t *testing.T) {
	cfg := config.NewConfig(
		config.WithDynamicValues(map[string][]string{
			"gpu-operator": {"driver.version"},
		}),
	)
	bundler, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	recipeResult := &recipe.RecipeResult{
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Kind:       "RecipeResult",
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:      "gpu-operator",
				Namespace: "gpu-operator",
				Version:   "v25.3.3",
				Type:      "helm",
				Source:    "https://helm.ngc.nvidia.com/nvidia",
				Chart:     "gpu-operator",
			},
		},
	}

	out, err := bundler.Make(context.Background(), recipeResult, t.TempDir())
	if err != nil {
		t.Fatalf("expected success for valid --dynamic component, got: %v", err)
	}
	if out == nil {
		t.Fatal("expected non-nil output")
	}
}

func TestMake_DisabledComponentWithDynamic(t *testing.T) {
	t.Parallel()

	cfg := config.NewConfig(
		config.WithValueOverrides(map[string]map[string]string{
			"awsebscsidriver": {"enabled": "false"},
		}),
		config.WithDynamicValues(map[string][]string{
			"awsebscsidriver": {"controller.replicaCount"},
		}),
	)
	bundler, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	recipeResult := &recipe.RecipeResult{
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Kind:       "RecipeResult",
		Criteria:   &recipe.Criteria{Service: "eks", Accelerator: "h100", Intent: "training"},
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:      "gpu-operator",
				Namespace: "gpu-operator",
				Version:   "v25.3.3",
				Type:      "helm",
				Source:    "https://helm.ngc.nvidia.com/nvidia",
				Chart:     "gpu-operator",
			},
			{
				Name:      "aws-ebs-csi-driver",
				Namespace: "kube-system",
				Version:   "2.55.0",
				Type:      "helm",
				Source:    "https://kubernetes-sigs.github.io/aws-ebs-csi-driver",
				Chart:     "aws-ebs-csi-driver",
			},
		},
		DeploymentOrder: []string{"gpu-operator", "aws-ebs-csi-driver"},
	}

	ctx := context.Background()
	tmpDir := t.TempDir()
	_, makeErr := bundler.Make(ctx, recipeResult, tmpDir)
	if makeErr != nil {
		t.Fatalf("Make() error = %v", makeErr)
	}

	// Disabled component should NOT have a directory at all
	if _, statErr := os.Stat(filepath.Join(tmpDir, "aws-ebs-csi-driver")); !os.IsNotExist(statErr) {
		t.Error("expected aws-ebs-csi-driver directory to NOT be created (component is disabled)")
	}

	// Disabled component should NOT have cluster-values.yaml
	if _, statErr := os.Stat(filepath.Join(tmpDir, "aws-ebs-csi-driver", "cluster-values.yaml")); !os.IsNotExist(statErr) {
		t.Error("expected aws-ebs-csi-driver/cluster-values.yaml to NOT exist (component is disabled)")
	}

	// Enabled component should still exist
	if _, statErr := os.Stat(filepath.Join(tmpDir, "gpu-operator", "values.yaml")); os.IsNotExist(statErr) {
		t.Error("expected gpu-operator/values.yaml to be created")
	}

	// deploy.sh should not reference the disabled component
	deployScript, readErr := os.ReadFile(filepath.Join(tmpDir, "deploy.sh"))
	if readErr != nil {
		t.Fatalf("failed to read deploy.sh: %v", readErr)
	}
	if strings.Contains(string(deployScript), "aws-ebs-csi-driver") {
		t.Error("deploy.sh should not contain aws-ebs-csi-driver (disabled component)")
	}
}

// TestMake_ArgoCDRejectsDynamic verifies that --deployer argocd with --dynamic
// returns a clear error directing users to --deployer argocd-helm.
func TestMake_ArgoCDRejectsDynamic(t *testing.T) {
	cfg := config.NewConfig(
		config.WithDeployer(config.DeployerArgoCD),
		config.WithDynamicValues(map[string][]string{
			"gpu-operator": {"driver.version"},
		}),
	)
	bundler, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	recipeResult := &recipe.RecipeResult{
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Kind:       "RecipeResult",
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator", Namespace: "gpu-operator", Version: "v25.3.3", Type: "helm", Source: "https://helm.ngc.nvidia.com/nvidia", Chart: "gpu-operator"},
		},
	}

	_, err = bundler.Make(context.Background(), recipeResult, t.TempDir())
	if err == nil {
		t.Fatal("expected error for --deployer argocd with --dynamic")
	}
	if !strings.Contains(err.Error(), "argocd-helm") {
		t.Errorf("error should suggest argocd-helm, got: %v", err)
	}
}

// TestMake_ArgoCDHelmRejectsAttest verifies that --attest is rejected with
// --deployer argocd-helm since attestation is not yet supported.
func TestMake_ArgoCDHelmRejectsAttest(t *testing.T) {
	cfg := config.NewConfig(
		config.WithDeployer(config.DeployerArgoCDHelm),
		config.WithAttest(true),
	)
	bundler, err := New(WithConfig(cfg))
	if err != nil {
		// New() may fail for attest pre-flight (no binary attestation file)
		// which is fine — the important thing is it doesn't silently succeed
		return
	}

	recipeResult := &recipe.RecipeResult{
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Kind:       "RecipeResult",
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator", Namespace: "gpu-operator", Version: "v25.3.3", Type: "helm", Source: "https://helm.ngc.nvidia.com/nvidia", Chart: "gpu-operator"},
		},
	}

	_, err = bundler.Make(context.Background(), recipeResult, t.TempDir())
	if err == nil {
		t.Fatal("expected error for --attest with --deployer argocd-helm")
	}
	if !strings.Contains(err.Error(), "not yet supported") {
		t.Errorf("error should mention not yet supported, got: %v", err)
	}
}

// TestMake_ArgoCDHelmRejectsData verifies that --data is rejected with
// --deployer argocd-helm since external data is not yet supported.
func TestMake_ArgoCDHelmRejectsData(t *testing.T) {
	// Create a temp external data dir with a minimal registry.yaml (required by LayeredDataProvider)
	dataDir := t.TempDir()
	registryContent := "components:\n  - name: custom\n    displayName: Custom\n"
	if err := os.WriteFile(filepath.Join(dataDir, "registry.yaml"), []byte(registryContent), 0600); err != nil {
		t.Fatalf("failed to write registry.yaml: %v", err)
	}

	// Set up layered data provider (simulates --data flag)
	originalProvider := recipe.GetDataProvider()
	embedded := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), ".")
	layered, providerErr := recipe.NewLayeredDataProvider(embedded, recipe.LayeredProviderConfig{
		ExternalDir: dataDir,
	})
	if providerErr != nil {
		t.Fatalf("NewLayeredDataProvider error: %v", providerErr)
	}
	recipe.SetDataProvider(layered)
	recipe.ResetComponentRegistryForTesting()
	defer func() {
		recipe.SetDataProvider(originalProvider)
		recipe.ResetComponentRegistryForTesting()
	}()

	cfg := config.NewConfig(
		config.WithDeployer(config.DeployerArgoCDHelm),
	)
	bundler, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	recipeResult := &recipe.RecipeResult{
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Kind:       "RecipeResult",
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator", Namespace: "gpu-operator", Version: "v25.3.3", Type: "helm", Source: "https://helm.ngc.nvidia.com/nvidia", Chart: "gpu-operator"},
		},
	}

	_, err = bundler.Make(context.Background(), recipeResult, t.TempDir())
	if err == nil {
		t.Fatal("expected error for --data with --deployer argocd-helm")
	}
	if !strings.Contains(err.Error(), "not yet supported") {
		t.Errorf("error should mention not yet supported, got: %v", err)
	}
}

// computeTestChecksum computes SHA256 hash for test comparison.
func computeTestChecksum(content []byte) string {
	hash := make([]byte, 32)
	for i, b := range content {
		hash[i%32] ^= b
	}
	return string(hash)
}
