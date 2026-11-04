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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestMake_DriverOwnershipLockUsesEmittedComponents(t *testing.T) {
	tests := []struct {
		name      string
		bundlers  []string
		overrides map[string]map[string]string
		wantGPU   bool
		wantDRA   bool
	}{
		{name: "both families", wantGPU: true, wantDRA: true},
		{name: "subset without guarded components", bundlers: []string{"cert-manager"}},
		{name: "subset without GPU Operator", bundlers: []string{"nvidia-dra-driver-gpu"}, wantDRA: true},
		{name: "subset without DRA", bundlers: []string{"gpu-operator"}, wantGPU: true},
		{
			name: "disabled by bundle-time override",
			overrides: map[string]map[string]string{
				"gpuoperator": {"enabled": "false"},
				"dradriver":   {"enabled": "false"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := New(WithConfig(config.NewConfig(
				config.WithDeployer(config.DeployerArgoCDHelm),
				config.WithBundlers(tt.bundlers),
				config.WithValueOverrides(tt.overrides),
				config.WithDeterministic(true),
			)))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			rr := &recipe.RecipeResult{
				APIVersion: recipe.RecipeResultAPIVersion,
				Kind:       recipe.RecipeResultKind,
				ComponentRefs: []recipe.ComponentRef{
					{
						Name: "gpu-operator", Namespace: "gpu-operator", Chart: "gpu-operator",
						Version: "v26.4.0", Type: recipe.ComponentTypeHelm,
						Source: "https://helm.ngc.nvidia.com/nvidia",
					},
					{
						Name: "nvidia-dra-driver-gpu", Namespace: "nvidia-dra-driver-gpu", Chart: "nvidia-dra-driver-gpu",
						Version: "25.12.0", Type: recipe.ComponentTypeHelm,
						Source:    "https://helm.ngc.nvidia.com/nvidia",
						Overrides: map[string]any{"nvidiaDriverRoot": "/run/nvidia/driver"},
					},
					{
						Name: "cert-manager", Namespace: "cert-manager", Chart: "cert-manager",
						Version: "v1.20.2", Type: recipe.ComponentTypeHelm,
						Source: "https://charts.jetstack.io",
					},
				},
				DeploymentOrder: []string{"cert-manager", "gpu-operator", "nvidia-dra-driver-gpu"},
			}
			outputDir := t.TempDir()
			if _, makeErr := b.Make(t.Context(), rr, outputDir); makeErr != nil {
				t.Fatalf("Make() error = %v", makeErr)
			}
			guard, err := os.ReadFile(filepath.Join(outputDir, "templates", "aicr-driver-ownership-lock.yaml"))
			if !tt.wantGPU && !tt.wantDRA {
				if !os.IsNotExist(err) {
					t.Fatalf("guard stat error = %v, want not-exist", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("read guard: %v", err)
			}
			for _, family := range []struct {
				path string
				want bool
			}{
				{"gpu-operator.driver.enabled", tt.wantGPU},
				{"nvidia-dra-driver-gpu.nvidiaDriverRoot", tt.wantDRA},
			} {
				if got := strings.Contains(string(guard), family.path); got != family.want {
					t.Errorf("guard names %s = %v, want %v\n%s", family.path, got, family.want, guard)
				}
			}
		})
	}
}
