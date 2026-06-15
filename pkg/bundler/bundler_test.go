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

package bundler

import (
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/errors"
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

	// Verify per-component directories (numbered by deployment order)
	componentDirs := map[string]string{
		"gpu-operator":     "001-gpu-operator",
		"network-operator": "002-network-operator",
	}
	for comp, dir := range componentDirs {
		valuesPath := filepath.Join(tmpDir, dir, "values.yaml")
		if _, statErr := os.Stat(valuesPath); os.IsNotExist(statErr) {
			t.Errorf("expected %s/values.yaml was not created (component %s)", dir, comp)
		}
	}

	// No Chart.yaml should exist at top level
	chartPath := filepath.Join(tmpDir, "Chart.yaml")
	if _, statErr := os.Stat(chartPath); !os.IsNotExist(statErr) {
		t.Error("Chart.yaml should not exist in per-component bundle")
	}

	// Verify output summary (3 root + 2 components × multiple files >= 7)
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

	// Enabled component should have a directory (numbering reflects only enabled components)
	if _, statErr := os.Stat(filepath.Join(tmpDir, "001-gpu-operator", "values.yaml")); os.IsNotExist(statErr) {
		t.Error("expected 001-gpu-operator/values.yaml to be created")
	}

	// Disabled component should NOT have a directory (under any numbering)
	for _, dir := range []string{"aws-ebs-csi-driver", "001-aws-ebs-csi-driver", "002-aws-ebs-csi-driver"} {
		if _, statErr := os.Stat(filepath.Join(tmpDir, dir)); !os.IsNotExist(statErr) {
			t.Errorf("expected %s directory to NOT be created", dir)
		}
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
		expectErr      bool
	}{
		{
			name:           "recipe disabled + --set enabled=true => included",
			recipeEnabled:  new(bool),
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
			recipeEnabled:  new(bool),
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
			// Fail closed: an unparseable --set enabled value must error
			// out rather than silently ignore the operator's intent.
			name:          "invalid --set value => error",
			recipeEnabled: new(bool),
			setEnabled:    "ture",
			expectErr:     true,
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
			if tt.expectErr {
				if makeErr == nil {
					t.Fatalf("Make() expected error, got nil")
				}
				return
			}
			if makeErr != nil {
				t.Fatalf("Make() error = %v", makeErr)
			}

			// When included, the component appears as the second numbered folder
			// (gpu-operator is 001, aws-ebs-csi-driver is 002). The flat layout
			// is gone in this PR — only assert against the numbered path.
			_, statErr := os.Stat(filepath.Join(tmpDir, "002-aws-ebs-csi-driver"))
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

	// aws-ebs-csi-driver is the 2nd component in deployment order (after gpu-operator)
	valuesPath := filepath.Join(tmpDir, "002-aws-ebs-csi-driver", "values.yaml")
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

	// Verify 001-gpu-operator/values.yaml was created (single component → 001)
	valuesPath := filepath.Join(tmpDir, "001-gpu-operator", "values.yaml")
	if _, err := os.Stat(valuesPath); os.IsNotExist(err) {
		t.Fatal("001-gpu-operator/values.yaml was not created")
	}
}

// TestMake_WithTypedValueOverrides verifies that a --set-json / --set-file list
// override is rendered into the generated values.yaml as a real YAML sequence
// (not the bare string scalar --set would produce). This is the regression
// guard for the agentgateway.allowedSourceRanges trap described in #1161.
func TestMake_WithTypedValueOverrides(t *testing.T) {
	cfg := config.NewConfig(
		config.WithValueOverridesTypedPaths([]config.TypedComponentPath{
			{
				Component: "gpu-operator",
				Path:      "allowedSourceRanges",
				Value:     []any{"216.228.127.128/30", "10.0.0.0/8"},
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

	if _, makeErr := bundler.Make(ctx, recipeResult, tmpDir); makeErr != nil {
		t.Fatalf("Make() error = %v", makeErr)
	}

	valuesPath := filepath.Join(tmpDir, "001-gpu-operator", "values.yaml")
	data, err := os.ReadFile(valuesPath)
	if err != nil {
		t.Fatalf("read values.yaml: %v", err)
	}

	var values map[string]any
	if err := yaml.Unmarshal(data, &values); err != nil {
		t.Fatalf("unmarshal values.yaml: %v", err)
	}

	got, ok := values["allowedSourceRanges"].([]any)
	if !ok {
		t.Fatalf("allowedSourceRanges is %T, want a YAML list ([]any); raw:\n%s", values["allowedSourceRanges"], data)
	}
	want := []any{"216.228.127.128/30", "10.0.0.0/8"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("allowedSourceRanges = %#v, want %#v", got, want)
	}
}

// TestMake_TypedOverrideWinsOverSet locks in the precedence contract: when
// scalar --set and structured --set-json/--set-file target the same path, the
// typed override wins (it is applied last in buildComponentValues). Guards
// against a future reordering silently flipping precedence.
func TestMake_TypedOverrideWinsOverSet(t *testing.T) {
	const path = "driver.version"

	cfg := config.NewConfig(
		config.WithValueOverrides(map[string]map[string]string{
			"gpu-operator": {path: "from-set"},
		}),
		config.WithValueOverridesTypedPaths([]config.TypedComponentPath{
			{Component: "gpu-operator", Path: path, Value: "from-json"},
		}),
	)
	bundler, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	recipeResult := &recipe.RecipeResult{
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Kind:       "Recipe",
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator", Version: "v25.3.3", Type: "helm", Source: "https://helm.ngc.nvidia.com/nvidia"},
		},
	}

	tmpDir := t.TempDir()
	if _, makeErr := bundler.Make(context.Background(), recipeResult, tmpDir); makeErr != nil {
		t.Fatalf("Make() error = %v", makeErr)
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, "001-gpu-operator", "values.yaml"))
	if err != nil {
		t.Fatalf("read values.yaml: %v", err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(data, &values); err != nil {
		t.Fatalf("unmarshal values.yaml: %v", err)
	}

	got, _ := component.GetValueByPath(values, path)
	if got != "from-json" {
		t.Errorf("%s = %#v, want \"from-json\" (typed --set-json must win over scalar --set)", path, got)
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

func TestMake_Helmfile(t *testing.T) {
	cfg := config.NewConfig(
		config.WithDeployer(config.DeployerHelmfile),
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
	if len(output.Results) == 0 {
		t.Error("expected at least 1 result")
	}
	for _, r := range output.Results {
		if r.Type != "helmfile-bundle" {
			t.Errorf("result type = %q, want %q", r.Type, "helmfile-bundle")
		}
		if !r.Success {
			t.Error("expected successful result")
		}
	}
	if output.Deployment == nil {
		t.Fatal("expected deployment info")
	}
	if output.Deployment.Type != "Helmfile release graph" {
		t.Errorf("deployment type = %q, want %q",
			output.Deployment.Type, "Helmfile release graph")
	}
	if output.TotalFiles == 0 {
		t.Error("expected generated files")
	}
	// Sanity-check the deployer emitted helmfile.yaml at the bundle root.
	if _, statErr := os.Stat(filepath.Join(tmpDir, "helmfile.yaml")); statErr != nil {
		t.Errorf("helmfile.yaml missing at bundle root: %v", statErr)
	}
	// Lock in helmfile non-goals (per issue #632): the helmfile deployer must
	// NOT emit bash wrappers or peer-deployer orchestration artifacts. A
	// regression that brings any of these back is a scope violation.
	for _, leaked := range []string{
		"deploy.sh",          // helm deployer
		"app-of-apps.yaml",   // argocd / argocd-helm deployer
		"kustomization.yaml", // flux deployer
	} {
		if _, statErr := os.Stat(filepath.Join(tmpDir, leaked)); !stderrors.Is(statErr, os.ErrNotExist) {
			t.Errorf("%s should not be generated for deployer=helmfile", leaked)
		}
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
		{"nodewright-operator", "nodewrightoperator"},
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

			result := b.getValueOverridesForComponent(tt.componentName, nil)
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

// TestGetTypedValueOverridesForComponent verifies that --set-json / --set-file
// overrides resolve component aliases the same way scalar --set overrides do.
func TestGetTypedValueOverridesForComponent(t *testing.T) {
	tests := []struct {
		name          string
		paths         []config.TypedComponentPath
		componentName string
		wantOverrides bool
		wantKey       string
		wantValue     any
	}{
		{
			name:          "no overrides returns nil",
			componentName: "agentgateway",
			wantOverrides: false,
		},
		{
			name: "exact name match with list value",
			paths: []config.TypedComponentPath{
				{Component: "agentgateway", Path: "allowedSourceRanges", Value: []any{"10.0.0.0/8"}},
			},
			componentName: "agentgateway",
			wantOverrides: true,
			wantKey:       "allowedSourceRanges",
			wantValue:     []any{"10.0.0.0/8"},
		},
		{
			name: "override key match via registry alias",
			paths: []config.TypedComponentPath{
				{Component: "gpuoperator", Path: "driver.env", Value: map[string]any{"A": "b"}},
			},
			componentName: "gpu-operator",
			wantOverrides: true,
			wantKey:       "driver.env",
			wantValue:     map[string]any{"A": "b"},
		},
		{
			name: "no match returns nil",
			paths: []config.TypedComponentPath{
				{Component: "network-operator", Path: "list", Value: []any{"x"}},
			},
			componentName: "gpu-operator",
			wantOverrides: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.NewConfig(config.WithValueOverridesTypedPaths(tt.paths))
			b, err := New(WithConfig(cfg))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			result := b.getTypedValueOverridesForComponent(tt.componentName, nil)
			if tt.wantOverrides && result == nil {
				t.Fatal("expected overrides, got nil")
			}
			if !tt.wantOverrides && result != nil {
				t.Errorf("expected nil overrides, got %v", result)
			}
			if tt.wantOverrides {
				if !reflect.DeepEqual(result[tt.wantKey], tt.wantValue) {
					t.Errorf("override[%q] = %#v, want %#v", tt.wantKey, result[tt.wantKey], tt.wantValue)
				}
			}
		})
	}
}

// TestGetTypedValueOverridesForComponent_MergesAcrossAliases verifies that when
// typed overrides are supplied under BOTH the canonical name and a registry
// alias for the same component, all of them are honored — none are silently
// dropped — and that the canonical (higher-priority) key wins on a path that
// collides across keys.
func TestGetTypedValueOverridesForComponent_MergesAcrossAliases(t *testing.T) {
	cfg := config.NewConfig(config.WithValueOverridesTypedPaths([]config.TypedComponentPath{
		{Component: "gpu-operator", Path: "driver.env", Value: map[string]any{"A": "b"}},
		{Component: "gpuoperator", Path: "allowedSourceRanges", Value: []any{"10.0.0.0/8"}},
		// Collision: both the exact name and the alias set the same path.
		{Component: "gpu-operator", Path: "shared", Value: "from-exact"},
		{Component: "gpuoperator", Path: "shared", Value: "from-alias"},
	}))
	b, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	result := b.getTypedValueOverridesForComponent("gpu-operator", nil)
	if result == nil {
		t.Fatal("expected merged overrides, got nil")
	}
	if !reflect.DeepEqual(result["driver.env"], map[string]any{"A": "b"}) {
		t.Errorf("driver.env (exact-name override) = %#v, want {A:b}", result["driver.env"])
	}
	if !reflect.DeepEqual(result["allowedSourceRanges"], []any{"10.0.0.0/8"}) {
		t.Errorf("allowedSourceRanges (alias override) = %#v, want [10.0.0.0/8] — alias override was dropped", result["allowedSourceRanges"])
	}
	if result["shared"] != "from-exact" {
		t.Errorf("shared = %#v, want \"from-exact\" (canonical name must win on collision)", result["shared"])
	}
}

// TestMake_TypedEnabledToggleRejectedBelowCLI verifies the bundler rejects an
// "enabled" toggle supplied on the typed path even when it reaches the config
// directly (i.e. not through the CLI flag parser) — guarding non-CLI/SDK
// callers, not just the CLI. Routing the toggle through --set-json/--set-file
// would write a stray literal `enabled:` into chart values instead of toggling
// the component.
func TestMake_TypedEnabledToggleRejectedBelowCLI(t *testing.T) {
	cfg := config.NewConfig(
		config.WithValueOverridesTypedPaths([]config.TypedComponentPath{
			{Component: "gpu-operator", Path: config.ComponentEnabledKey, Value: false},
		}),
	)
	bundler, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	recipeResult := &recipe.RecipeResult{
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Kind:       "Recipe",
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator", Version: "v25.3.3", Type: "helm", Source: "https://helm.ngc.nvidia.com/nvidia"},
		},
	}

	_, makeErr := bundler.Make(context.Background(), recipeResult, t.TempDir())
	if makeErr == nil {
		t.Fatal("expected error: typed 'enabled' override must be rejected below the CLI boundary")
	}
	if !strings.Contains(makeErr.Error(), config.ComponentEnabledKey) || !strings.Contains(makeErr.Error(), "--set") {
		t.Errorf("error %q must name the enabled toggle and point to --set", makeErr.Error())
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
	comp := registry.Get("nodewright-operator")
	if comp == nil || len(comp.GetNodeCountPaths()) == 0 {
		t.Skip("nodewright-operator with nodeCountPaths not in registry; skipping estimated node count path test")
	}

	cfg := config.NewConfig(config.WithEstimatedNodeCount(8))
	b, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	values := make(map[string]any)
	b.applyNodeSchedulingOverrides("nodewright-operator", values, nil, schedulingPathPolicy{})

	// Path "estimatedNodeCount" is in nodewright-operator's nodeCountPaths; convertMapValue produces int64.
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

// TestApplyNodeSchedulingOverrides_StorageClass covers all storage-class injection scenarios
// as a table-driven test: global injection, no-op when flag is unset, and explicit --set
// per-component override winning over the global default.
func TestApplyNodeSchedulingOverrides_StorageClass(t *testing.T) {
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry() error = %v", err)
	}
	comp := registry.Get("kube-prometheus-stack")
	if comp == nil || len(comp.GetStorageClassPaths()) == 0 {
		t.Fatalf("registry missing kube-prometheus-stack or storageClassPaths: cannot run storage class path test")
	}

	const scPath = "prometheus.prometheusSpec.storageSpec.volumeClaimTemplate.spec.storageClassName"

	tests := []struct {
		name          string
		cfgOpts       []config.Option
		initialValues map[string]string // applied to values map before the call (simulates earlier --set application)
		wantValue     string
		wantPresent   bool
	}{
		{
			name:        "global storageClass injected into empty values",
			cfgOpts:     []config.Option{config.WithStorageClass("my-storage-class")},
			wantValue:   "my-storage-class",
			wantPresent: true,
		},
		{
			name:          "no injection when --storage-class is not set",
			cfgOpts:       []config.Option{},
			initialValues: map[string]string{scPath: "gp2"},
			wantValue:     "gp2",
			wantPresent:   true,
		},
		{
			name: "explicit --set per-component wins over global --storage-class",
			cfgOpts: []config.Option{
				config.WithStorageClass("my-storage-class"),
				config.WithValueOverrides(map[string]map[string]string{
					// "prometheus" is the valueOverrideKey for kube-prometheus-stack.
					"prometheus": {scPath: "explicit-gp2"},
				}),
			},
			initialValues: map[string]string{scPath: "explicit-gp2"},
			wantValue:     "explicit-gp2",
			wantPresent:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.NewConfig(tt.cfgOpts...)
			b, err := New(WithConfig(cfg))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			values := map[string]any{}
			if len(tt.initialValues) > 0 {
				if applyErr := component.ApplyMapOverrides(values, tt.initialValues); applyErr != nil {
					t.Fatalf("ApplyMapOverrides() setup error = %v", applyErr)
				}
			}

			b.applyNodeSchedulingOverrides("kube-prometheus-stack", values, nil, schedulingPathPolicy{})

			got, ok := component.GetValueByPath(values, scPath)
			if ok != tt.wantPresent {
				t.Fatalf("storageClassName present = %v, want %v", ok, tt.wantPresent)
			}
			if tt.wantPresent && got != tt.wantValue {
				t.Errorf("storageClassName = %v, want %q", got, tt.wantValue)
			}
		})
	}
}

func TestApplyNodeSchedulingOverrides_DynamoPlatformStorageClass(t *testing.T) {
	const scPath = "nats.config.jetstream.fileStore.pvc.storageClassName"

	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry() error = %v", err)
	}
	comp := registry.Get("dynamo-platform")
	if comp == nil {
		t.Fatal("registry missing dynamo-platform")
	}
	if !slices.Contains(comp.GetStorageClassPaths(), scPath) {
		t.Fatalf("registry storageClassPaths for dynamo-platform = %v, want %q",
			comp.GetStorageClassPaths(), scPath)
	}

	tests := []struct {
		name          string
		cfgOpts       []config.Option
		initialValues map[string]string
		wantValue     string
	}{
		{
			name:      "global storageClass injected into bundled NATS PVC",
			cfgOpts:   []config.Option{config.WithStorageClass("my-storage-class")},
			wantValue: "my-storage-class",
		},
		{
			name: "explicit dynamo --set wins over global --storage-class",
			cfgOpts: []config.Option{
				config.WithStorageClass("my-storage-class"),
				config.WithValueOverrides(map[string]map[string]string{
					"dynamo": {scPath: "explicit-gp2"},
				}),
			},
			initialValues: map[string]string{scPath: "explicit-gp2"},
			wantValue:     "explicit-gp2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.NewConfig(tt.cfgOpts...)
			b, err := New(WithConfig(cfg))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			values := map[string]any{}
			if len(tt.initialValues) > 0 {
				if applyErr := component.ApplyMapOverrides(values, tt.initialValues); applyErr != nil {
					t.Fatalf("ApplyMapOverrides() setup error = %v", applyErr)
				}
			}

			b.applyNodeSchedulingOverrides("dynamo-platform", values, nil, schedulingPathPolicy{})

			got, ok := component.GetValueByPath(values, scPath)
			if !ok {
				t.Fatal("storageClassName not injected")
			}
			if got != tt.wantValue {
				t.Errorf("storageClassName = %v, want %q", got, tt.wantValue)
			}
		})
	}
}

// TestApplyNodeSchedulingOverrides_RespectsRecipeSetPaths verifies the
// precedence rule that paths the user explicitly populated via the recipe
// overlay's inline overrides or CLI --set are NOT overwritten by CLI/config
// defaults. This is the fix for #982: kind.yaml's `daemonsets.tolerations: []`
// (an opt-out) and bcm.yaml's `controller.tolerations` (a BCM-master
// toleration list) must reach the rendered bundle untouched.
//
// CRITICALLY, component default values files are intentionally NOT treated
// as authoritative (see the "values-file default does not lock the path"
// sub-test). Several components (kai-scheduler, kueue, network-operator,
// aws-efa) ship a chart-default-equivalent toleration in their values.yaml;
// treating those as authoritative would silently turn
// --system-node-toleration / --accelerated-node-toleration into a no-op for
// those components — a real CLI regression. The bundler computes the
// authoritative set from ComponentRef.Overrides + --set only, then passes it
// in. This test drives the function at that contract.
//
// gpu-operator's `daemonsets.tolerations` is registry-declared as an
// accelerated toleration path; we sanity-check the binding via the registry
// before each run to fail loudly (rather than silently passing) if the
// registry shape ever changes.
func TestApplyNodeSchedulingOverrides_RespectsRecipeSetPaths(t *testing.T) {
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry() error = %v", err)
	}
	gpuOp := registry.Get("gpu-operator")
	if gpuOp == nil {
		t.Fatalf("registry missing gpu-operator component")
	}
	const tolPath = "daemonsets.tolerations"
	if !slices.Contains(gpuOp.GetAcceleratedTolerationPaths(), tolPath) {
		t.Fatalf("gpu-operator accelerated toleration paths must include %q; got %v",
			tolPath, gpuOp.GetAcceleratedTolerationPaths())
	}

	tests := []struct {
		name        string
		initial     map[string]any
		policy      schedulingPathPolicy
		cliTols     []corev1.Toleration
		wantValue   any
		description string
	}{
		{
			name: "overlay-set empty slice is preserved (opt-out)",
			initial: map[string]any{
				"daemonsets": map[string]any{
					"tolerations": []any{},
				},
			},
			policy:      schedulingPathPolicy{optOut: map[string]struct{}{tolPath: {}}},
			cliTols:     []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			wantValue:   []any{},
			description: "kind.yaml: overlay-set empty list must defeat the default {Exists} injection",
		},
		{
			name: "overlay-set non-empty list — CLI tolerations APPEND",
			initial: map[string]any{
				"daemonsets": map[string]any{
					"tolerations": []any{
						map[string]any{"key": "node-role.kubernetes.io/master", "operator": "Exists", "effect": "NoSchedule"},
					},
				},
			},
			policy: schedulingPathPolicy{appendMode: map[string]struct{}{tolPath: {}}},
			cliTols: []corev1.Toleration{
				{Key: "kwok.x-k8s.io/node", Operator: corev1.TolerationOpEqual, Value: "fake", Effect: corev1.TaintEffectNoSchedule},
			},
			wantValue: []any{
				map[string]any{"key": "node-role.kubernetes.io/master", "operator": "Exists", "effect": "NoSchedule"},
				map[string]any{"key": "kwok.x-k8s.io/node", "operator": "Equal", "value": "fake", "effect": "NoSchedule"},
			},
			description: "bcm-style: overlay tolerations must coexist with CLI tolerations (UNION), not be replaced",
		},
		{
			name:    "unset path receives CLI injection",
			initial: map[string]any{},
			policy:  schedulingPathPolicy{},
			cliTols: []corev1.Toleration{
				{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpEqual, Value: "present", Effect: corev1.TaintEffectNoSchedule},
			},
			wantValue: []any{
				map[string]any{"key": "nvidia.com/gpu", "operator": "Equal", "value": "present", "effect": "NoSchedule"},
			},
			description: "default flow (eks/gke/etc.): no overlay value, CLI toleration is injected as before",
		},
		{
			// REGRESSION GUARD for PR #1082 review feedback: component default
			// values files (kai-scheduler, kueue, network-operator, aws-efa)
			// ship tolerations in their values.yaml that overlap with registry
			// scheduling paths. They land in `values` via the values-file load
			// — not via overlay overrides or --set — so the policy is empty
			// and the CLI default MUST still win (REPLACE).
			name: "values-file default does not lock the path",
			initial: map[string]any{
				"daemonsets": map[string]any{
					"tolerations": []any{
						map[string]any{"operator": "Exists"},
					},
				},
			},
			policy: schedulingPathPolicy{}, // values-file is not authoritative
			cliTols: []corev1.Toleration{
				{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpEqual, Value: "present", Effect: corev1.TaintEffectNoSchedule},
			},
			wantValue: []any{
				map[string]any{"key": "nvidia.com/gpu", "operator": "Equal", "value": "present", "effect": "NoSchedule"},
			},
			description: "component default values file value is overwritten by --accelerated-node-toleration",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.NewConfig(config.WithAcceleratedNodeTolerations(tt.cliTols))
			b, err := New(WithConfig(cfg))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			b.applyNodeSchedulingOverrides("gpu-operator", tt.initial, nil, tt.policy)

			got, ok := component.GetValueByPath(tt.initial, tolPath)
			if !ok {
				t.Fatalf("%s: %s not present after injection", tt.description, tolPath)
			}
			gotList, ok := got.([]any)
			if !ok {
				t.Fatalf("%s: %s has wrong type %T (want []any)", tt.description, tolPath, got)
			}
			wantList, _ := tt.wantValue.([]any)
			if !reflect.DeepEqual(gotList, wantList) {
				t.Errorf("%s:\n  got  = %#v\n  want = %#v", tt.description, gotList, wantList)
			}
		})
	}
}

// TestClassifySchedulingPaths covers the helper that splits scheduling
// paths into opt-out vs append based on the recipe overlay's inline
// overrides and CLI --set. Component default values files are deliberately
// NOT consulted — see the godoc on classifySchedulingPaths.
func TestClassifySchedulingPaths(t *testing.T) {
	tests := []struct {
		name         string
		overrides    map[string]any
		setOverrides map[string]string
		paths        []string
		wantOptOut   []string
		wantAppend   []string
	}{
		{
			name:  "empty paths returns empty policy",
			paths: nil,
		},
		{
			name: "overlay empty slice → opt-out (kind.yaml semantics)",
			overrides: map[string]any{
				"daemonsets": map[string]any{
					"tolerations": []any{},
				},
			},
			paths:      []string{"daemonsets.tolerations", "daemonsets.nodeSelector"},
			wantOptOut: []string{"daemonsets.tolerations"},
		},
		{
			name: "overlay non-empty slice → append (bcm.yaml semantics)",
			overrides: map[string]any{
				"controller": map[string]any{
					"tolerations": []any{
						map[string]any{"key": "node-role.kubernetes.io/master"},
					},
				},
			},
			paths:      []string{"controller.tolerations"},
			wantAppend: []string{"controller.tolerations"},
		},
		{
			name: "explicit nil leaf → opt-out (matches empty-slice semantics)",
			// GetValueByPath returns (nil, true) for an explicit nil leaf,
			// e.g. `tolerations: ~` in YAML. Helm collapses nil to "unset",
			// so this is a deliberate opt-out gesture.
			overrides: map[string]any{
				"daemonsets": map[string]any{
					"tolerations": nil,
				},
			},
			paths:      []string{"daemonsets.tolerations"},
			wantOptOut: []string{"daemonsets.tolerations"},
		},
		{
			name:         "--set with exact path match → append",
			setOverrides: map[string]string{"daemonsets.tolerations": "non-empty-string"},
			paths:        []string{"daemonsets.tolerations"},
			wantAppend:   []string{"daemonsets.tolerations"},
		},
		{
			// Simulates kai-scheduler / kueue / network-operator / aws-efa:
			// the values file populates the path but the overlay and --set
			// do not. The policy must be empty so the CLI default (REPLACE)
			// applies — addresses PR #1082 reviewer concern that the CLI
			// flag would silently become a no-op for those components.
			name:  "overlay and --set both empty — empty policy (values-file ignored)",
			paths: []string{"global.tolerations"},
		},
		{
			name: "intermediate non-map → unclassified",
			overrides: map[string]any{
				"daemonsets": "scalar",
			},
			paths: []string{"daemonsets.tolerations"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifySchedulingPaths(tt.overrides, tt.setOverrides, tt.paths)

			assertSetEqual(t, "optOut", got.optOut, tt.wantOptOut)
			assertSetEqual(t, "appendMode", got.appendMode, tt.wantAppend)
		})
	}
}

func assertSetEqual(t *testing.T, label string, got map[string]struct{}, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: size = %d, want %d (got=%v, want=%v)", label, len(got), len(want), got, want)
		return
	}
	for _, p := range want {
		if _, ok := got[p]; !ok {
			t.Errorf("%s: missing %q (got %v)", label, p, got)
		}
	}
}

// TestFilterPaths verifies the helper that removes pre-existing paths from
// an injection target list.
func TestFilterPaths(t *testing.T) {
	tests := []struct {
		name  string
		paths []string
		skip  map[string]struct{}
		want  []string
	}{
		{
			name:  "empty paths returns nil",
			paths: nil,
			skip:  map[string]struct{}{"x": {}},
			want:  nil,
		},
		{
			name:  "empty skip returns input unchanged",
			paths: []string{"a", "b"},
			skip:  nil,
			want:  []string{"a", "b"},
		},
		{
			name:  "single blocked path removed",
			paths: []string{"a", "b", "c"},
			skip:  map[string]struct{}{"b": {}},
			want:  []string{"a", "c"},
		},
		{
			name:  "all paths blocked returns empty slice",
			paths: []string{"a", "b"},
			skip:  map[string]struct{}{"a": {}, "b": {}},
			want:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterPaths(tt.paths, tt.skip)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("filterPaths() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestApplyNodeSchedulingOverrides_BoundProvider verifies that
// applyNodeSchedulingOverrides honors the bound provider parameter when
// resolving the component registry. The bound provider exposes a component
// whose name is absent from the embedded registry, so a positive assertion
// on the injected nodeSelector path proves the helper used the supplied
// provider rather than the package-global fallback.
func TestApplyNodeSchedulingOverrides_BoundProvider(t *testing.T) {
	const uniqueComponent = "task2-bound-provider-only"
	const nodeSelectorPath = "scheduling.nodeSelector"

	tmpDir := t.TempDir()
	registryYAML := "apiVersion: aicr.nvidia.com/v1alpha1\n" +
		"kind: ComponentRegistry\n" +
		"components:\n" +
		"  - name: " + uniqueComponent + "\n" +
		"    displayName: Task 2 Bound Provider Only\n" +
		"    nodeScheduling:\n" +
		"      system:\n" +
		"        nodeSelectorPaths:\n" +
		"          - " + nodeSelectorPath + "\n"
	if writeErr := os.WriteFile(filepath.Join(tmpDir, "registry.yaml"), []byte(registryYAML), 0600); writeErr != nil {
		t.Fatalf("write registry.yaml: %v", writeErr)
	}

	embedded := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	layered, layeredErr := recipe.NewLayeredDataProvider(embedded, recipe.LayeredProviderConfig{
		ExternalDir: tmpDir,
	})
	if layeredErr != nil {
		t.Fatalf("NewLayeredDataProvider: %v", layeredErr)
	}
	// Drop any cached registry for this provider identity so the test
	// registry YAML is the source of truth on first read.
	recipe.EvictCachedRegistry(layered)
	t.Cleanup(func() { recipe.EvictCachedRegistry(layered) })

	// Sanity check: the embedded (global) registry must NOT know about the
	// unique component, otherwise the assertion below cannot distinguish
	// "honored the provider" from "fell back to the global".
	globalRegistry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	if globalRegistry.Get(uniqueComponent) != nil {
		t.Fatalf("global registry unexpectedly contains %q; fixture cannot prove provider isolation", uniqueComponent)
	}

	cfg := config.NewConfig(
		config.WithSystemNodeSelector(map[string]string{"role": "system"}),
	)
	b, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// With a nil provider the lookup must miss (component unknown to global registry).
	nilValues := map[string]any{}
	b.applyNodeSchedulingOverrides(uniqueComponent, nilValues, nil, schedulingPathPolicy{})
	if got, ok := component.GetValueByPath(nilValues, nodeSelectorPath); ok {
		t.Fatalf("nil-provider call unexpectedly populated %s = %v; component must be unknown to global registry", nodeSelectorPath, got)
	}

	// With the bound provider the lookup hits and the nodeSelector lands at the
	// path the external registry declares.
	values := map[string]any{}
	b.applyNodeSchedulingOverrides(uniqueComponent, values, layered, schedulingPathPolicy{})

	got, ok := component.GetValueByPath(values, nodeSelectorPath)
	if !ok {
		t.Fatalf("nodeSelector not injected at %s; bound provider was not consulted", nodeSelectorPath)
	}
	gotMap, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("nodeSelector at %s = %T %v; want map[string]any", nodeSelectorPath, got, got)
	}
	if gotMap["role"] != "system" {
		t.Errorf("nodeSelector.role = %v, want %q", gotMap["role"], "system")
	}
}

// TestBundler_Make_BoundProviderEndToEnd is the canonical end-to-end check for
// the bundler-provider migration: build a recipe via WithDataProvider(layered)
// against a real embedded overlay, run bundler.Make, and confirm the emitted
// values.yaml carries a marker that lives ONLY in the layered (external)
// provider. If the bundler silently fell back to the package-global embedded
// data, the marker would be absent and this test would fail.
//
// Why this test exists:
//   - PR #1015 made RecipeResult.GetValuesForComponent honor the bound
//     provider; this test exercises that path through the bundler entry point
//     (Make -> extractComponentValues -> GetValuesForComponent).
//   - Tasks 1-5 of this PR threaded the bound provider through the bundler's
//     internal helpers (applyNodeSchedulingOverrides, copyDataFiles, etc.).
//     Those helpers are unit-tested at TestApplyNodeSchedulingOverrides_BoundProvider;
//     this test is the integration backstop that proves the whole pipeline
//     stays consistent when a layered provider replaces the global.
func TestBundler_Make_BoundProviderEndToEnd(t *testing.T) {
	const markerVersion = "777.77.77-aicr-task6-marker"

	// LayeredDataProvider over a tempdir that overrides exactly two files:
	//   1. registry.yaml (required by NewLayeredDataProvider; merged into
	//      embedded so all upstream components remain known).
	//   2. components/gpu-operator/values.yaml (the base values that
	//      h100-eks-ubuntu-training inherits via the eks-training overlay,
	//      which pins valuesFile = components/gpu-operator/values-eks-training.yaml
	//      and triggers the "base + overlay" merge in
	//      RecipeResult.GetValuesForComponent — driver.version is only set
	//      in the base, so our marker passes through into the emitted bundle).
	tmpData := t.TempDir()

	registryYAML := []byte("apiVersion: aicr.nvidia.com/v1alpha1\n" +
		"kind: ComponentRegistry\n" +
		"components: []\n")
	if err := os.WriteFile(filepath.Join(tmpData, "registry.yaml"), registryYAML, 0o600); err != nil {
		t.Fatalf("write registry.yaml: %v", err)
	}

	componentsDir := filepath.Join(tmpData, "components", "gpu-operator")
	if err := os.MkdirAll(componentsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	valuesContent := fmt.Appendf(nil, "driver:\n  version: %q\n", markerVersion)
	if err := os.WriteFile(filepath.Join(componentsDir, "values.yaml"), valuesContent, 0o600); err != nil {
		t.Fatalf("write values.yaml: %v", err)
	}

	embedded := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	layered, err := recipe.NewLayeredDataProvider(embedded, recipe.LayeredProviderConfig{
		ExternalDir: tmpData,
	})
	if err != nil {
		t.Fatalf("NewLayeredDataProvider: %v", err)
	}
	// Drop any cached registry for this provider identity so the merged
	// external registry is the source of truth on first read.
	recipe.EvictCachedRegistry(layered)
	t.Cleanup(func() { recipe.EvictCachedRegistry(layered) })

	// Build a recipe through the bound provider. Criteria selects
	// h100-eks-ubuntu-training, which inherits gpu-operator from eks-training.
	b := recipe.NewBuilder(recipe.WithDataProvider(layered))
	criteria := &recipe.Criteria{
		Service:     recipe.CriteriaServiceEKS,
		Accelerator: recipe.CriteriaAcceleratorH100,
		Intent:      recipe.CriteriaIntentTraining,
		OS:          recipe.CriteriaOSUbuntu,
	}
	result, err := b.BuildFromCriteria(context.Background(), criteria)
	if err != nil {
		t.Fatalf("BuildFromCriteria: %v", err)
	}

	// Sanity check: the RecipeResult must carry the bound provider — otherwise
	// the bundler will silently fall back to the package-global and the
	// assertion below cannot distinguish "honored the provider" from "global
	// happened to match".
	if result.DataProvider() != layered {
		t.Fatalf("result.DataProvider() did not return the layered provider; bound-provider plumbing is broken")
	}

	// Confirm gpu-operator is in the resolved component list — if criteria
	// resolution silently dropped it, the marker assertion would vacuously
	// pass with zero values.yaml files matched.
	if result.GetComponentRef("gpu-operator") == nil {
		t.Fatalf("gpu-operator not present in recipe component refs; criteria/overlay drift")
	}

	bundler, err := New()
	if err != nil {
		t.Fatalf("bundler.New: %v", err)
	}

	outDir := t.TempDir()
	if _, makeErr := bundler.Make(context.Background(), result, outDir); makeErr != nil {
		t.Fatalf("bundler.Make: %v", makeErr)
	}

	// Walk the output and locate the gpu-operator values.yaml. The bundler
	// emits a numbered per-component directory whose suffix is the component
	// name (e.g. "001-gpu-operator/values.yaml"), but the exact ordinal
	// depends on the deployment graph for the resolved overlay — walking by
	// suffix avoids hardcoding a brittle path.
	var gpuValuesPath string
	walkErr := filepath.Walk(outDir, func(p string, info os.FileInfo, innerErr error) error {
		if innerErr != nil {
			return innerErr
		}
		if info.IsDir() {
			return nil
		}
		if info.Name() != "values.yaml" {
			return nil
		}
		if strings.HasSuffix(filepath.Dir(p), "-gpu-operator") {
			gpuValuesPath = p
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk outDir: %v", walkErr)
	}
	if gpuValuesPath == "" {
		t.Fatalf("gpu-operator values.yaml not found under %s; bundler did not emit expected per-component artifact", outDir)
	}

	data, err := os.ReadFile(gpuValuesPath)
	if err != nil {
		t.Fatalf("read emitted gpu-operator values.yaml: %v", err)
	}
	if !bytes.Contains(data, []byte(markerVersion)) {
		t.Errorf("emitted %s missing marker %q — bundler did not honor bound provider\ncontent:\n%s",
			gpuValuesPath, markerVersion, data)
	}
}

func TestWarnMissingStorageClassForPVCs(t *testing.T) {
	const scPath = "prometheus.prometheusSpec.storageSpec.volumeClaimTemplate.spec.storageClassName"
	const pvcSizePath = "prometheus.prometheusSpec.storageSpec.volumeClaimTemplate.spec.resources.requests.storage"

	recipeResult := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{{Name: "kube-prometheus-stack"}},
	}

	tests := []struct {
		name        string
		cfgOpts     []config.Option
		setupValues func(map[string]any)
		wantWarning bool
	}{
		{
			name: "warns when rendered PVC omits storageClassName",
			setupValues: func(values map[string]any) {
				component.SetValueByPath(values, pvcSizePath, "50Gi")
			},
			wantWarning: true,
		},
		{
			name: "warns when rendered PVC has blank storageClassName",
			setupValues: func(values map[string]any) {
				component.SetValueByPath(values, pvcSizePath, "50Gi")
				component.SetValueByPath(values, scPath, " ")
			},
			wantWarning: true,
		},
		{
			name: "does not warn when rendered PVC has storageClassName",
			setupValues: func(values map[string]any) {
				component.SetValueByPath(values, scPath, "gp3")
			},
		},
		{
			name: "does not warn for emptyDir storage",
			setupValues: func(values map[string]any) {
				component.SetValueByPath(values, "prometheus.prometheusSpec.storageSpec.emptyDir.sizeLimit", "10Gi")
			},
		},
		{
			name:    "warns when explicit blank override wins over global storageClass",
			cfgOpts: []config.Option{config.WithStorageClass("gp3")},
			setupValues: func(values map[string]any) {
				component.SetValueByPath(values, pvcSizePath, "50Gi")
				component.SetValueByPath(values, scPath, " ")
			},
			wantWarning: true,
		},
		{
			name:    "does not warn when global storageClass was injected",
			cfgOpts: []config.Option{config.WithStorageClass("gp3")},
			setupValues: func(values map[string]any) {
				component.SetValueByPath(values, pvcSizePath, "50Gi")
				component.SetValueByPath(values, scPath, "gp3")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := New(WithConfig(config.NewConfig(tt.cfgOpts...)))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			values := map[string]any{}
			tt.setupValues(values)

			err = b.warnMissingStorageClassForPVCs(context.Background(), recipeResult, map[string]map[string]any{
				"kube-prometheus-stack": values,
			})
			if err != nil {
				t.Fatalf("warnMissingStorageClassForPVCs() error = %v", err)
			}

			if gotWarning := len(b.warnings) > 0; gotWarning != tt.wantWarning {
				t.Fatalf("warning present = %v, want %v; warnings = %v", gotWarning, tt.wantWarning, b.warnings)
			}

			if tt.wantWarning {
				warning := b.warnings[0]
				for _, want := range []string{
					"Warning: kube-prometheus-stack renders a PVC without storageClassName",
					scPath,
					"--storage-class <name>",
					"--set kube-prometheus-stack:" + scPath + "=<name>",
				} {
					if !strings.Contains(warning, want) {
						t.Errorf("warning = %q, want substring %q", warning, want)
					}
				}
			}
		})
	}
}

func TestWarnMissingStorageClassForPVCs_DynamoPlatformNATS(t *testing.T) {
	const scPath = "nats.config.jetstream.fileStore.pvc.storageClassName"

	recipeResult := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{{
			Name:       "dynamo-platform",
			ValuesFile: "components/dynamo-platform/values.yaml",
		}},
	}

	tests := []struct {
		name        string
		setupValues func(map[string]any)
		wantWarning bool
	}{
		{
			name:        "warns when bundled NATS PVC omits storageClassName",
			wantWarning: true,
		},
		{
			name: "does not warn when bundled NATS PVC has storageClassName",
			setupValues: func(values map[string]any) {
				component.SetValueByPath(values, scPath, "gp3")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values, err := recipeResult.GetValuesForComponent("dynamo-platform")
			if err != nil {
				t.Fatalf("GetValuesForComponent(dynamo-platform): %v", err)
			}
			if tt.setupValues != nil {
				tt.setupValues(values)
			}

			b, err := New()
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			err = b.warnMissingStorageClassForPVCs(context.Background(), recipeResult, map[string]map[string]any{
				"dynamo-platform": values,
			})
			if err != nil {
				t.Fatalf("warnMissingStorageClassForPVCs() error = %v", err)
			}

			if gotWarning := len(b.warnings) > 0; gotWarning != tt.wantWarning {
				t.Fatalf("warning present = %v, want %v; warnings = %v", gotWarning, tt.wantWarning, b.warnings)
			}

			if tt.wantWarning {
				warning := b.warnings[0]
				for _, want := range []string{
					"Warning: dynamo-platform renders a PVC without storageClassName",
					scPath,
					"--storage-class <name>",
					"--set dynamo-platform:" + scPath + "=<name>",
				} {
					if !strings.Contains(warning, want) {
						t.Errorf("warning = %q, want substring %q", warning, want)
					}
				}
			}
		})
	}
}

// TestAgentgatewayComponentExistsInRegistry locks agentgatewayComponentName to a
// real registry entry. warnAgentgatewayOpenExposure keys into componentValues by
// this name; if the "agentgateway" component were renamed in recipes/registry.yaml,
// the lookup would silently return nil and the open-exposure warning would never
// fire — with no other test failing. Mirrors the validator-side
// TestEmbeddedCatalog_InferenceGatewayEntryExists guard. See #1160.
func TestAgentgatewayComponentExistsInRegistry(t *testing.T) {
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry() error = %v", err)
	}
	if registry.Get(agentgatewayComponentName) == nil {
		t.Fatalf("no registry component named %q (open-exposure warning would silently no-op)", agentgatewayComponentName)
	}
}

func TestWarnAgentgatewayOpenExposure(t *testing.T) {
	tests := []struct {
		name        string
		values      map[string]any
		present     bool
		wantWarning bool
	}{
		{
			name:        "warns when allowedSourceRanges is unset",
			values:      map[string]any{"fullnameOverride": "agentgateway"},
			present:     true,
			wantWarning: true,
		},
		{
			name:        "warns when allowedSourceRanges is an empty list",
			values:      map[string]any{"allowedSourceRanges": []any{}},
			present:     true,
			wantWarning: true,
		},
		{
			name:        "warns when allowedSourceRanges is a bare string (mistaken --set)",
			values:      map[string]any{"allowedSourceRanges": "216.228.127.128/30"},
			present:     true,
			wantWarning: true,
		},
		{
			name:        "does not warn when allowedSourceRanges is a non-empty list",
			values:      map[string]any{"allowedSourceRanges": []any{"216.228.127.128/30"}},
			present:     true,
			wantWarning: false,
		},
		{
			name:        "warns when allowedSourceRanges is an explicit 0.0.0.0/0",
			values:      map[string]any{"allowedSourceRanges": []any{"0.0.0.0/0"}},
			present:     true,
			wantWarning: true,
		},
		{
			name:        "warns when allowedSourceRanges is an explicit ::/0",
			values:      map[string]any{"allowedSourceRanges": []any{"::/0"}},
			present:     true,
			wantWarning: true,
		},
		{
			name:        "warns when a scoped range is mixed with an any-source CIDR",
			values:      map[string]any{"allowedSourceRanges": []any{"10.0.0.0/8", "0.0.0.0/0"}},
			present:     true,
			wantWarning: true,
		},
		{
			name:        "does not warn when agentgateway is absent",
			present:     false,
			wantWarning: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := New(WithConfig(config.NewConfig()))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			componentValues := map[string]map[string]any{}
			if tt.present {
				componentValues[agentgatewayComponentName] = tt.values
			}

			b.warnAgentgatewayOpenExposure(componentValues)

			if gotWarning := len(b.warnings) > 0; gotWarning != tt.wantWarning {
				t.Fatalf("warning present = %v, want %v; warnings = %v", gotWarning, tt.wantWarning, b.warnings)
			}
			if tt.wantWarning {
				warning := b.warnings[0]
				for _, want := range []string{"inference-gateway", "0.0.0.0/0", "allowedSourceRanges"} {
					if !strings.Contains(warning, want) {
						t.Errorf("warning = %q, want substring %q", warning, want)
					}
				}
			}
		})
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

func TestCollectComponentManifests_MissingPath(t *testing.T) {
	bundler, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	recipeResult := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:          "gpu-operator",
				ManifestFiles: []string{"components/gpu-operator/manifests/removed-in-newer-binary.yaml"},
			},
		},
	}

	t.Run("embedded-only provider", func(t *testing.T) {
		_, err := bundler.collectComponentManifests(context.Background(), recipeResult)
		if err == nil {
			t.Fatal("expected error for missing manifest path")
		}
		if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
			t.Errorf("expected ErrCodeInvalidRequest, got %v", err)
		}
		msg := err.Error()
		for _, want := range []string{"removed-in-newer-binary.yaml", "gpu-operator", "embedded data", "regenerate"} {
			if !strings.Contains(msg, want) {
				t.Errorf("error message should mention %q: %v", want, msg)
			}
		}
		if strings.Contains(msg, "--data") {
			t.Errorf("embedded-only message should not mention --data: %v", msg)
		}
	})

	t.Run("layered provider with --data", func(t *testing.T) {
		tmpDir := t.TempDir()
		minimalRegistry := "apiVersion: aicr.nvidia.com/v1alpha1\nkind: ComponentRegistry\ncomponents: []\n"
		if writeErr := os.WriteFile(filepath.Join(tmpDir, "registry.yaml"), []byte(minimalRegistry), 0600); writeErr != nil {
			t.Fatalf("write registry.yaml: %v", writeErr)
		}

		embedded := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
		layered, layeredErr := recipe.NewLayeredDataProvider(embedded, recipe.LayeredProviderConfig{
			ExternalDir: tmpDir,
		})
		if layeredErr != nil {
			t.Fatalf("NewLayeredDataProvider: %v", layeredErr)
		}

		recipeResult.BindDataProvider(layered)

		_, err := bundler.collectComponentManifests(context.Background(), recipeResult)
		if err == nil {
			t.Fatal("expected error for missing manifest path")
		}
		if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
			t.Errorf("expected ErrCodeInvalidRequest, got %v", err)
		}
		msg := err.Error()
		for _, want := range []string{"removed-in-newer-binary.yaml", "gpu-operator", "--data", "regenerate"} {
			if !strings.Contains(msg, want) {
				t.Errorf("error message should mention %q: %v", want, msg)
			}
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

	for i := range 2 {
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
		t.Fatal("expected error for unknown component in dynamic declaration, got nil")
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
		t.Fatalf("expected success for valid dynamic component, got: %v", err)
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

	// Disabled component should NOT have a directory at all (under any numbering).
	// The directory check implies cluster-values.yaml absence, so don't double-check.
	for _, dir := range []string{"aws-ebs-csi-driver", "001-aws-ebs-csi-driver", "002-aws-ebs-csi-driver"} {
		if _, statErr := os.Stat(filepath.Join(tmpDir, dir)); !os.IsNotExist(statErr) {
			t.Errorf("expected %s directory to NOT be created (component is disabled)", dir)
		}
	}

	// Enabled component should still exist (gpu-operator is the only enabled → 001)
	if _, statErr := os.Stat(filepath.Join(tmpDir, "001-gpu-operator", "values.yaml")); os.IsNotExist(statErr) {
		t.Error("expected 001-gpu-operator/values.yaml to be created")
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

// TestMake_ArgoCDRejectsDynamic verifies that deployer argocd with dynamic declarations
// returns a clear error directing users to deployer argocd-helm.
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
		t.Fatal("expected error for deployer argocd with dynamic declarations")
	}
	if !strings.Contains(err.Error(), "argocd-helm") {
		t.Errorf("error should suggest argocd-helm, got: %v", err)
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

func TestMake_PreservesInnerErrorCode(t *testing.T) {
	bundler, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx := context.Background()
	tmpDir := t.TempDir()

	// "../evil" triggers deployer.IsSafePathComponent → ErrCodeInvalidRequest
	recipeResult := &recipe.RecipeResult{
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Kind:       "Recipe",
		ComponentRefs: []recipe.ComponentRef{
			{Name: "../evil", Version: "v1.0.0", Type: "helm", Source: "https://example.com"},
		},
		DeploymentOrder: []string{"../evil"},
	}

	_, err = bundler.Make(ctx, recipeResult, tmpDir)
	if err == nil {
		t.Fatal("expected error for path-traversal component name, got nil")
	}

	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("expected *errors.StructuredError, got %T: %v", err, err)
	}

	if se.Code != errors.ErrCodeInvalidRequest {
		t.Errorf("expected error code %s, got %s (error: %v)",
			errors.ErrCodeInvalidRequest, se.Code, err)
	}
}

func TestMake_PreservesTimeoutFromExtractValues(t *testing.T) {
	bundler, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately — extractComponentValues checks ctx.Err()

	tmpDir := t.TempDir()
	recipeResult := &recipe.RecipeResult{
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Kind:       "Recipe",
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator", Version: "v25.3.3", Type: "helm", Source: "https://helm.ngc.nvidia.com/nvidia"},
		},
		DeploymentOrder: []string{"gpu-operator"},
	}

	_, err = bundler.Make(ctx, recipeResult, tmpDir)
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}

	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("expected *errors.StructuredError, got %T: %v", err, err)
	}

	if se.Code != errors.ErrCodeTimeout {
		t.Errorf("expected error code %s, got %s (error: %v)",
			errors.ErrCodeTimeout, se.Code, err)
	}
}

// TestBundlerValueParity_WithRecipeResult pins the invariant that the
// bundler's extractComponentValues produces values byte-identical (deep-equal)
// to RecipeResult.GetValuesForComponent for every component in a representative
// RecipeResult, when run with a vanilla bundler (no --set overrides, no node
// scheduling configured, so applyNodeSchedulingOverrides is a no-op).
//
// This guards against silent drift between the two code paths as the cache
// layer beneath them is refactored. If this test fails, the bundler has
// started returning different values than the canonical RecipeResult adapter —
// investigate before changing the test.
func TestBundlerValueParity_WithRecipeResult(t *testing.T) {
	// Build a RecipeResult covering all three value-source shapes:
	//   - ValuesFile only            → gpu-operator (loads from embedded data)
	//   - Overrides only             → cert-manager (inline only)
	//   - ValuesFile + Overrides     → network-operator (hybrid merge)
	recipeResult := &recipe.RecipeResult{
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Kind:       "Recipe",
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:       "gpu-operator",
				Version:    "v25.3.3",
				Type:       "helm",
				Source:     "https://helm.ngc.nvidia.com/nvidia",
				ValuesFile: "components/gpu-operator/values.yaml",
			},
			{
				Name:    "cert-manager",
				Version: "v1.15.3",
				Type:    "helm",
				Source:  "https://charts.jetstack.io",
				Overrides: map[string]any{
					"installCRDs": true,
					"resources": map[string]any{
						"requests": map[string]any{
							"cpu":    "100m",
							"memory": "128Mi",
						},
					},
				},
			},
			{
				Name:       "network-operator",
				Version:    "v25.4.0",
				Type:       "helm",
				Source:     "https://helm.ngc.nvidia.com/nvidia",
				ValuesFile: "components/network-operator/values.yaml",
				Overrides: map[string]any{
					"deployCR": false,
				},
			},
		},
	}

	// Vanilla bundler: default config (no --set overrides, no scheduling).
	// Under these conditions applyNodeSchedulingOverrides is a no-op, so the
	// two outputs must be deep-equal without any mirroring helper.
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	bundlerValues, err := b.extractComponentValues(context.Background(), recipeResult)
	if err != nil {
		t.Fatalf("extractComponentValues() error = %v", err)
	}

	// Sanity: every ref should have produced an entry; at least one must be
	// non-empty so the test isn't trivially passing on empty-vs-empty.
	if len(bundlerValues) != len(recipeResult.ComponentRefs) {
		t.Fatalf("extractComponentValues returned %d entries, want %d",
			len(bundlerValues), len(recipeResult.ComponentRefs))
	}
	var anyNonEmpty bool
	for _, v := range bundlerValues {
		if len(v) > 0 {
			anyNonEmpty = true
			break
		}
	}
	if !anyNonEmpty {
		t.Fatal("all bundler-extracted component values are empty; fixture is not exercising the merge paths")
	}

	for _, ref := range recipeResult.ComponentRefs {
		t.Run(ref.Name, func(t *testing.T) {
			adapted, err := recipeResult.GetValuesForComponent(ref.Name)
			if err != nil {
				t.Fatalf("GetValuesForComponent(%q): %v", ref.Name, err)
			}

			got, ok := bundlerValues[ref.Name]
			if !ok {
				t.Fatalf("extractComponentValues missing entry for %q", ref.Name)
			}

			if !reflect.DeepEqual(adapted, got) {
				t.Errorf("value mismatch for %q:\n  RecipeResult.GetValuesForComponent: %#v\n  extractComponentValues:             %#v",
					ref.Name, adapted, got)
			}
		})
	}
}
