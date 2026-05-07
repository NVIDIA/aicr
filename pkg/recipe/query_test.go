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
	"testing"
)

const testQueryService = "eks"

func TestSelect(t *testing.T) {
	hydrated := map[string]any{
		"criteria": map[string]any{
			"service":     testQueryService,
			"accelerator": "h100",
		},
		"deploymentOrder": []string{"gpu-operator", "network-operator"},
		"components": map[string]any{
			"gpu-operator": map[string]any{
				"name":    "gpu-operator",
				"chart":   "gpu-operator",
				"version": "v24.9.0",
				"values": map[string]any{
					"driver": map[string]any{
						"version":    "570.86.16",
						"repository": "nvcr.io/nvidia",
					},
					"enabled": true,
				},
			},
		},
	}

	tests := []struct {
		name     string
		selector string
		wantErr  bool
		check    func(t *testing.T, val any)
	}{
		{
			name:     "empty selector returns entire map",
			selector: "",
			check: func(t *testing.T, val any) {
				m, ok := val.(map[string]any)
				if !ok {
					t.Fatal("expected map")
				}
				if _, exists := m["criteria"]; !exists {
					t.Error("expected criteria key")
				}
			},
		},
		{
			name:     "dot selector returns entire map",
			selector: ".",
			check: func(t *testing.T, val any) {
				m, ok := val.(map[string]any)
				if !ok {
					t.Fatal("expected map")
				}
				if _, exists := m["criteria"]; !exists {
					t.Error("expected criteria key")
				}
			},
		},
		{
			name:     "leading dot is stripped",
			selector: ".components.gpu-operator.chart",
			check: func(t *testing.T, val any) {
				if val != "gpu-operator" {
					t.Errorf("got %v, want gpu-operator", val)
				}
			},
		},
		{
			name:     "scalar value",
			selector: "components.gpu-operator.values.driver.version",
			check: func(t *testing.T, val any) {
				if val != "570.86.16" {
					t.Errorf("got %v, want 570.86.16", val)
				}
			},
		},
		{
			name:     "nested map",
			selector: "components.gpu-operator.values.driver",
			check: func(t *testing.T, val any) {
				m, ok := val.(map[string]any)
				if !ok {
					t.Fatal("expected map")
				}
				if m["version"] != "570.86.16" {
					t.Errorf("got %v, want 570.86.16", m["version"])
				}
			},
		},
		{
			name:     "top-level list",
			selector: "deploymentOrder",
			check: func(t *testing.T, val any) {
				list, ok := val.([]string)
				if !ok {
					t.Fatal("expected string slice")
				}
				if len(list) != 2 {
					t.Errorf("got %d items, want 2", len(list))
				}
			},
		},
		{
			name:     "criteria field",
			selector: "criteria.service",
			check: func(t *testing.T, val any) {
				if val != testQueryService {
					t.Errorf("got %v, want %s", val, testQueryService)
				}
			},
		},
		{
			name:     "boolean value",
			selector: "components.gpu-operator.values.enabled",
			check: func(t *testing.T, val any) {
				b, ok := val.(bool)
				if !ok {
					t.Fatal("expected bool")
				}
				if !b {
					t.Error("expected true")
				}
			},
		},
		{
			name:     "component-level map",
			selector: "components.gpu-operator",
			check: func(t *testing.T, val any) {
				m, ok := val.(map[string]any)
				if !ok {
					t.Fatal("expected map")
				}
				if m["chart"] != "gpu-operator" {
					t.Errorf("got %v, want gpu-operator", m["chart"])
				}
			},
		},
		{
			name:     "nonexistent key",
			selector: "components.gpu-operator.values.nonexistent",
			wantErr:  true,
		},
		{
			name:     "nonexistent component",
			selector: "components.missing-component",
			wantErr:  true,
		},
		{
			name:     "descend into scalar",
			selector: "components.gpu-operator.version.sub",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			val, err := Select(hydrated, tt.selector)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && tt.check != nil {
				tt.check(t, val)
			}
		})
	}
}

func TestHydrateResult(t *testing.T) {
	t.Run("nil result", func(t *testing.T) {
		_, err := HydrateResult(nil)
		if err == nil {
			t.Fatal("expected error for nil result")
		}
	})

	t.Run("basic result", func(t *testing.T) {
		result := &RecipeResult{
			Kind:       "RecipeResult",
			APIVersion: "aicr.nvidia.com/v1alpha1",
			Criteria: &Criteria{
				Service:     "eks",
				Accelerator: "h100",
				Intent:      "training",
				OS:          "ubuntu",
				Platform:    "any",
			},
			ComponentRefs: []ComponentRef{
				{
					Name:    "test-component",
					Type:    ComponentTypeHelm,
					Source:  "https://example.com/charts",
					Chart:   "test-chart",
					Version: "v1.0.0",
				},
			},
			DeploymentOrder: []string{"test-component"},
		}

		hydrated, err := HydrateResult(result)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Verify top-level keys exist
		for _, key := range []string{"kind", "apiVersion", "metadata", "criteria", "deploymentOrder", "components"} {
			if _, exists := hydrated[key]; !exists {
				t.Errorf("missing key %q", key)
			}
		}

		// Verify criteria
		criteria, ok := hydrated["criteria"].(map[string]any)
		if !ok {
			t.Fatal("criteria is not a map")
		}
		if criteria["service"] != testQueryService {
			t.Errorf("got service %v, want %s", criteria["service"], testQueryService)
		}

		// Verify component
		components, ok := hydrated["components"].(map[string]any)
		if !ok {
			t.Fatal("components is not a map")
		}
		comp, ok := components["test-component"].(map[string]any)
		if !ok {
			t.Fatal("test-component is not a map")
		}
		if comp["chart"] != "test-chart" {
			t.Errorf("got chart %v, want test-chart", comp["chart"])
		}
	})

	t.Run("excluded overlays include reasons", func(t *testing.T) {
		result := &RecipeResult{
			Kind:            "RecipeResult",
			APIVersion:      "aicr.nvidia.com/v1alpha1",
			DeploymentOrder: []string{},
		}
		result.Metadata.ExcludedOverlays = []ExcludedOverlay{
			{Name: "eks-training", Reason: ExcludedOverlayReasonConstraintFailed},
			{Name: "h100-eks-ubuntu-training", Reason: ExcludedOverlayReasonMixinConstraintFailed},
		}

		hydrated, err := HydrateResult(result)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		metadata, ok := hydrated["metadata"].(map[string]any)
		if !ok {
			t.Fatal("metadata is not a map")
		}
		excluded, ok := metadata["excludedOverlays"].([]ExcludedOverlay)
		if !ok {
			t.Fatalf("excludedOverlays has type %T, want []ExcludedOverlay", metadata["excludedOverlays"])
		}
		if len(excluded) != 2 {
			t.Fatalf("got %d excluded overlays, want 2", len(excluded))
		}
		if excluded[0].Reason != ExcludedOverlayReasonConstraintFailed {
			t.Fatalf("first reason = %q", excluded[0].Reason)
		}
		if excluded[1].Reason != ExcludedOverlayReasonMixinConstraintFailed {
			t.Fatalf("second reason = %q", excluded[1].Reason)
		}
	})

	t.Run("nil criteria", func(t *testing.T) {
		result := &RecipeResult{
			Kind:       "RecipeResult",
			APIVersion: "aicr.nvidia.com/v1alpha1",
		}

		hydrated, err := HydrateResult(result)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, exists := hydrated["criteria"]; exists {
			t.Error("expected no criteria key for nil criteria")
		}
	})

	t.Run("constraints with optional fields", func(t *testing.T) {
		result := &RecipeResult{
			Kind:       "RecipeResult",
			APIVersion: "aicr.nvidia.com/v1alpha1",
			Constraints: []Constraint{
				{Name: "k8s", Value: ">= 1.30"},
				{Name: "gpu-mem", Value: ">= 80", Severity: "error", Remediation: "upgrade GPU", Unit: "GB"},
			},
			DeploymentOrder: []string{},
		}

		hydrated, err := HydrateResult(result)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		constraintList, ok := hydrated["constraints"].([]map[string]any)
		if !ok {
			t.Fatal("constraints is not a slice of maps")
		}
		if len(constraintList) != 2 {
			t.Fatalf("got %d constraints, want 2", len(constraintList))
		}

		// First constraint: no optional fields
		if _, exists := constraintList[0]["severity"]; exists {
			t.Error("first constraint should not have severity")
		}

		// Second constraint: all optional fields
		if constraintList[1]["severity"] != "error" {
			t.Errorf("got severity %v, want error", constraintList[1]["severity"])
		}
		if constraintList[1]["unit"] != "GB" {
			t.Errorf("got unit %v, want GB", constraintList[1]["unit"])
		}
		if constraintList[1]["remediation"] != "upgrade GPU" {
			t.Errorf("got remediation %v, want 'upgrade GPU'", constraintList[1]["remediation"])
		}
	})

	t.Run("component optional fields", func(t *testing.T) {
		result := &RecipeResult{
			Kind:       "RecipeResult",
			APIVersion: "aicr.nvidia.com/v1alpha1",
			ComponentRefs: []ComponentRef{
				{
					Name:           "kustomize-app",
					Type:           ComponentTypeKustomize,
					Source:         "https://github.com/example/app",
					Namespace:      "custom-ns",
					Tag:            "v2.0.0",
					Path:           "deploy/prod",
					DependencyRefs: []string{"cert-manager"},
				},
			},
			DeploymentOrder: []string{"kustomize-app"},
		}

		hydrated, err := HydrateResult(result)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		components, ok := hydrated["components"].(map[string]any)
		if !ok {
			t.Fatal("components is not a map")
		}
		comp, ok := components["kustomize-app"].(map[string]any)
		if !ok {
			t.Fatal("kustomize-app is not a map")
		}
		if comp["namespace"] != "custom-ns" {
			t.Errorf("got namespace %v, want custom-ns", comp["namespace"])
		}
		if comp["tag"] != "v2.0.0" {
			t.Errorf("got tag %v, want v2.0.0", comp["tag"])
		}
		if comp["path"] != "deploy/prod" {
			t.Errorf("got path %v, want deploy/prod", comp["path"])
		}
		deps, ok := comp["dependencyRefs"].([]string)
		if !ok {
			t.Fatal("dependencyRefs is not a string slice")
		}
		if len(deps) != 1 || deps[0] != "cert-manager" {
			t.Errorf("got dependencyRefs %v, want [cert-manager]", deps)
		}
		// chart should not be present (empty)
		if _, exists := comp["chart"]; exists {
			t.Error("chart should not be set for kustomize component")
		}
	})
}
