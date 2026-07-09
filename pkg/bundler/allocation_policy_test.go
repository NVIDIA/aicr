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
	"context"
	stderrors "errors"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// newPolicyTestBundler builds a DefaultBundler around the given config,
// failing the test on construction errors.
func newPolicyTestBundler(t *testing.T, cfg *config.Config) *DefaultBundler {
	t.Helper()
	b, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return b
}

// TestEnforceAllocationPolicyOverrides pins the #1327 bundle-time override
// policy: --dynamic declarations on allocation-policy keys are rejected
// (install-time value unknowable at validation), static overrides warn
// (deprecated but still the documented AKS workflow), and alias keys
// (dradriver/gpuoperator) resolve identically to canonical component names.
func TestEnforceAllocationPolicyOverrides(t *testing.T) {
	tests := []struct {
		name        string
		opts        []config.Option
		wantErr     bool
		wantErrMsg  string
		wantWarning string // substring expected in b.warnings; "" = no warning
	}{
		{
			name: "dynamic on the DRA switch via alias is rejected",
			opts: []config.Option{config.WithDynamicValues(map[string][]string{
				"dradriver": {"resources.gpus.enabled"},
			})},
			wantErr:    true,
			wantErrMsg: "resources.gpus.enabled",
		},
		{
			name: "dynamic on the chart-guard waiver is rejected",
			opts: []config.Option{config.WithDynamicValues(map[string][]string{
				"nvidia-dra-driver-gpu": {"gpuResourcesEnabledOverride"},
			})},
			wantErr:    true,
			wantErrMsg: "gpuResourcesEnabledOverride",
		},
		{
			name: "dynamic on a PARENT of devicePlugin.enabled is rejected",
			opts: []config.Option{config.WithDynamicValues(map[string][]string{
				"gpuoperator": {"devicePlugin"},
			})},
			wantErr:    true,
			wantErrMsg: "devicePlugin.enabled",
		},
		{
			name: "dynamic on unrelated paths passes",
			opts: []config.Option{config.WithDynamicValues(map[string][]string{
				"gpuoperator": {"driver.version"},
				"dradriver":   {"resources.computeDomains.enabled"},
			})},
		},
		{
			name: "static --set on devicePlugin.enabled warns (deprecated, not rejected)",
			opts: []config.Option{config.WithValueOverrides(map[string]map[string]string{
				"gpuoperator": {"devicePlugin.enabled": "false"},
			})},
			wantWarning: "devicePlugin.enabled",
		},
		{
			name: "static --set on the DRA switch via canonical name warns",
			opts: []config.Option{config.WithValueOverrides(map[string]map[string]string{
				"nvidia-dra-driver-gpu": {"resources.gpus.enabled": "true"},
			})},
			wantWarning: "resources.gpus.enabled",
		},
		{
			name: "typed --set-json parent-object override warns",
			opts: []config.Option{config.WithValueOverridesTypedPaths([]config.TypedComponentPath{
				{Component: "dradriver", Path: "resources", Value: map[string]any{
					"gpus": map[string]any{"enabled": true},
				}},
			})},
			wantWarning: "resources",
		},
		{
			name: "static overrides on unrelated paths stay silent",
			opts: []config.Option{config.WithValueOverrides(map[string]map[string]string{
				"gpuoperator": {"driver.version": "580.126.20"},
			})},
		},
		{
			// Component-level enable toggles change the allocation policy
			// exactly like the nested keys: disabling an advertiser removes
			// its mechanism.
			name: "static --set dradriver:enabled=false warns (component-level toggle, alias form)",
			opts: []config.Option{config.WithValueOverrides(map[string]map[string]string{
				"dradriver": {"enabled": "false"},
			})},
			wantWarning: "enabled",
		},
		{
			name: "static --set gpu-operator:enabled=false warns (canonical form)",
			opts: []config.Option{config.WithValueOverrides(map[string]map[string]string{
				"gpu-operator": {"enabled": "false"},
			})},
			wantWarning: "enabled",
		},
		{
			name: "static --set gpu-operator-ocp:enabled=false warns",
			opts: []config.Option{config.WithValueOverrides(map[string]map[string]string{
				"gpu-operator-ocp": {"enabled": "false"},
			})},
			wantWarning: "enabled",
		},
		{
			name: "static --set on gpu-operator-ocp devicePlugin.enabled via alias warns",
			opts: []config.Option{config.WithValueOverrides(map[string]map[string]string{
				"gpuoperatorocp": {"devicePlugin.enabled": "false"},
			})},
			wantWarning: "devicePlugin.enabled",
		},
		{
			name: "dynamic component-level enabled toggle is rejected (alias form)",
			opts: []config.Option{config.WithDynamicValues(map[string][]string{
				"gpuoperator": {"enabled"},
			})},
			wantErr:    true,
			wantErrMsg: "enabled",
		},
		{
			name: "dynamic gpu-operator-ocp enabled toggle is rejected",
			opts: []config.Option{config.WithDynamicValues(map[string][]string{
				"gpuoperatorocp": {"enabled"},
			})},
			wantErr:    true,
			wantErrMsg: "enabled",
		},
		{
			// enabled-suffixed sibling paths must not false-positive against
			// the component-level "enabled" key.
			name: "static --set gpuoperator:driver.enabled stays silent",
			opts: []config.Option{config.WithValueOverrides(map[string]map[string]string{
				"gpuoperator": {"driver.enabled": "true"},
			})},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newPolicyTestBundler(t, config.NewConfig(tt.opts...))
			err := b.enforceAllocationPolicyOverrides(nil)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
				}
				if !strings.Contains(err.Error(), tt.wantErrMsg) {
					t.Errorf("error = %v, want message containing %q", err, tt.wantErrMsg)
				}
				return
			}
			if tt.wantWarning == "" {
				if len(b.warnings) != 0 {
					t.Errorf("warnings = %v, want none", b.warnings)
				}
				return
			}
			joined := strings.Join(b.warnings, "\n")
			if !strings.Contains(joined, tt.wantWarning) || !strings.Contains(joined, "deprecated") {
				t.Errorf("warnings = %v, want a deprecation warning naming %q", b.warnings, tt.wantWarning)
			}
		})
	}
}

// TestMake_DynamicAllocationPolicyKeyRejected verifies the enforcement is
// wired into Make: a --dynamic declaration on an allocation-policy key aborts
// bundle generation with ErrCodeInvalidRequest.
func TestMake_DynamicAllocationPolicyKeyRejected(t *testing.T) {
	cfg := config.NewConfig(
		config.WithDynamicValues(map[string][]string{
			"dradriver": {"resources.gpus.enabled"},
		}),
	)
	b := newPolicyTestBundler(t, cfg)
	recipeResult := &recipe.RecipeResult{
		APIVersion: "aicr.run/v1alpha2",
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

	_, err := b.Make(context.Background(), recipeResult, t.TempDir())
	if err == nil {
		t.Fatal("expected Make to reject the --dynamic allocation-policy declaration")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "recipe overlay") {
		t.Errorf("error = %v, want remediation pointing at a recipe overlay", err)
	}
}

func TestMatchingPolicyKey(t *testing.T) {
	keys := []string{"resources.gpus.enabled", "gpuResourcesEnabledOverride"}
	tests := []struct {
		path string
		want string
	}{
		{"resources.gpus.enabled", "resources.gpus.enabled"},
		{"resources.gpus", "resources.gpus.enabled"},
		{"resources", "resources.gpus.enabled"},
		{"resources.gpus.enabled.extra", "resources.gpus.enabled"},
		{"gpuResourcesEnabledOverride", "gpuResourcesEnabledOverride"},
		{"resources.computeDomains.enabled", ""},
		{"resourcesFoo", ""},
		{"gpuResourcesEnabledOverrideX", ""},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := matchingPolicyKey(tt.path, keys); got != tt.want {
				t.Errorf("matchingPolicyKey(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
