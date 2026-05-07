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

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/validators"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestRecipeHasComponent(t *testing.T) {
	tests := []struct {
		name      string
		recipe    *recipe.RecipeResult
		component string
		want      bool
	}{
		{
			name:      "nil recipe",
			recipe:    nil,
			component: "kubeflow-trainer",
			want:      false,
		},
		{
			name:      "empty componentRefs",
			recipe:    &recipe.RecipeResult{},
			component: "kubeflow-trainer",
			want:      false,
		},
		{
			name: "component present",
			recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "cert-manager"},
					{Name: "kubeflow-trainer"},
					{Name: "gpu-operator"},
				},
			},
			component: "kubeflow-trainer",
			want:      true,
		},
		{
			name: "component not present",
			recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "cert-manager"},
					{Name: "gpu-operator"},
				},
			},
			component: "kubeflow-trainer",
			want:      false,
		},
		{
			name: "dynamo-platform present",
			recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "dynamo-platform"},
					{Name: "gpu-operator"},
				},
			},
			component: "dynamo-platform",
			want:      true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := &validators.Context{Recipe: tt.recipe}
			got := recipeHasComponent(ctx, tt.component)
			if got != tt.want {
				t.Errorf("recipeHasComponent(%q) = %v, want %v", tt.component, got, tt.want)
			}
		})
	}
}

func TestCheckRobustControllerRouting(t *testing.T) {
	tests := []struct {
		name           string
		recipe         *recipe.RecipeResult
		expectSkip     bool
		expectContains string // substring in error or skip message
	}{
		{
			name:           "nil recipe skips",
			recipe:         nil,
			expectSkip:     true,
			expectContains: "no supported AI operator",
		},
		{
			name:           "empty recipe skips",
			recipe:         &recipe.RecipeResult{},
			expectSkip:     true,
			expectContains: "no supported AI operator",
		},
		{
			name: "recipe with only gpu-operator skips",
			recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "gpu-operator"},
					{Name: "cert-manager"},
				},
			},
			expectSkip:     true,
			expectContains: "no supported AI operator",
		},
		{
			name: "recipe with kubeflow-trainer routes to kubeflow check",
			recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "kubeflow-trainer"},
					{Name: "gpu-operator"},
				},
			},
			expectSkip: false,
			// Will fail because fake clientset has no deployments, but proves routing works
			expectContains: "Kubeflow Trainer controller not found",
		},
		{
			name: "recipe with dynamo-platform routes to dynamo check",
			recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "dynamo-platform"},
					{Name: "gpu-operator"},
				},
			},
			expectSkip: false,
			// Will fail because fake clientset has no deployments, but proves routing works
			expectContains: "Dynamo operator controller not found",
		},
		{
			name: "recipe with both prefers dynamo",
			recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "dynamo-platform"},
					{Name: "kubeflow-trainer"},
				},
			},
			expectSkip:     false,
			expectContains: "Dynamo operator controller not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := &validators.Context{
				Ctx:       context.Background(),
				Clientset: k8sfake.NewClientset(),
				Recipe:    tt.recipe,
			}

			err := CheckRobustController(ctx)

			if tt.expectSkip {
				if err == nil {
					t.Fatal("expected skip error, got nil")
				}
				if !strings.Contains(err.Error(), "skip") {
					t.Errorf("expected skip error, got: %v", err)
				}
				if tt.expectContains != "" && !strings.Contains(err.Error(), tt.expectContains) {
					t.Errorf("expected error to contain %q, got: %v", tt.expectContains, err)
				}
				return
			}

			// Non-skip: expect an error (because fake clientset has no resources)
			// but the error should indicate the correct operator was targeted
			if err == nil {
				t.Fatal("expected error from fake clientset, got nil")
			}
			if tt.expectContains != "" && !strings.Contains(err.Error(), tt.expectContains) {
				t.Errorf("expected error to contain %q, got: %v", tt.expectContains, err)
			}
		})
	}
}
