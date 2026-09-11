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
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// tcpxoBundlerTestResult is a fingerprint recipe (h100 GKE kubeflow with the
// tcpxo fabric component) that records the interface mapping when
// withMapping is set, mimicking a recipe generated with
// --gke-tcpxo-interfaces.
func tcpxoBundlerTestResult(withMapping bool) *recipe.RecipeResult {
	result := &recipe.RecipeResult{
		Kind:       recipe.RecipeResultKind,
		APIVersion: recipe.ConfiguredRecipeResultAPIVersion,
		Criteria: &recipe.Criteria{
			Service:     recipe.CriteriaServiceGKE,
			Accelerator: recipe.CriteriaAcceleratorH100,
			OS:          recipe.CriteriaOSCOS,
			Intent:      recipe.CriteriaIntentTraining,
			Platform:    recipe.CriteriaPlatformKubeflow,
		},
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gke-nccl-tcpxo"},
			{Name: "kubeflow-trainer", ManifestFiles: []string{
				"components/kubeflow-trainer/manifests/torch-distributed-tcpxo-cluster-training-runtime.yaml",
			}},
		},
	}
	if withMapping {
		mapping := recipe.GKETCPXOIntrospectionInterfaces()
		result.Configuration = &recipe.RecipeConfiguration{
			GKE: &recipe.GKEConfiguration{TCPXOInterfaces: mapping},
		}
		override := make([]any, 0, len(mapping))
		for _, entry := range mapping {
			override = append(override, map[string]any{
				"interfaceName": entry.InterfaceName,
				"network":       entry.Network,
			})
		}
		result.ComponentRefs[1].Overrides = map[string]any{"tcpxoInterfaces": override}
	}
	return result
}

func TestEnforceGKETCPXOOwnership(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mapping    bool
		option     config.Option
		wantErr    string // substring; empty means no error expected
		wantErrNot string // substring that must NOT appear (message hygiene)
	}{
		{
			name:    "reject --set on the canonical component name",
			mapping: true,
			option: config.WithValueOverrides(map[string]map[string]string{
				"kubeflow-trainer": {"tcpxoInterfaces": "anything"},
			}),
			wantErr: "owned by configuration.gke.tcpxoInterfaces",
		},
		{
			name:    "reject --set via the trainer alias",
			mapping: true,
			option: config.WithValueOverrides(map[string]map[string]string{
				"trainer": {"tcpxoInterfaces": "anything"},
			}),
			wantErr: "owned by configuration.gke.tcpxoInterfaces",
		},
		{
			name:    "reject --set via the kubeflowtrainer alias",
			mapping: true,
			option: config.WithValueOverrides(map[string]map[string]string{
				"kubeflowtrainer": {"tcpxoInterfaces": "anything"},
			}),
			wantErr: "owned by configuration.gke.tcpxoInterfaces",
		},
		{
			name:    "reject --set-json path",
			mapping: true,
			option: config.WithValueOverridesTypedPaths([]config.TypedComponentPath{{
				Component: "kubeflow-trainer",
				Path:      "tcpxoInterfaces",
				Value:     []any{},
			}}),
			wantErr: "--set-json/--set-file",
		},
		{
			name:    "reject --dynamic path",
			mapping: true,
			option: config.WithDynamicValues(map[string][]string{
				"kubeflow-trainer": {"tcpxoInterfaces"},
			}),
			wantErr: "--dynamic",
		},
		{
			name:    "reject a descendant path",
			mapping: true,
			option: config.WithValueOverrides(map[string]map[string]string{
				"trainer": {"tcpxoInterfaces.eth1": "sneaky"},
			}),
			wantErr: "owned by configuration.gke.tcpxoInterfaces",
		},
		{
			name:    "allow unrelated kubeflow-trainer overrides",
			mapping: true,
			option: config.WithValueOverrides(map[string]map[string]string{
				"trainer": {"manager.nodeSelector.foo": "bar"},
			}),
		},
		{
			name:    "allow overrides on other components",
			mapping: true,
			option: config.WithValueOverrides(map[string]map[string]string{
				"gpu-operator": {"tcpxoInterfaces": "unrelated-path-on-another-component"},
			}),
		},
		{
			name:    "no overrides is fine",
			mapping: true,
		},
		{
			name:    "fail closed when the mapping is absent",
			mapping: false,
			wantErr: "records no configuration.gke.tcpxoInterfaces",
			// The divergence from the Slurm accounting warning is deliberate;
			// the error must name the remedy.
			wantErrNot: "deprecated",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var opts []config.Option
			if tt.option != nil {
				opts = append(opts, tt.option)
			}
			b, err := New(WithConfig(config.NewConfig(opts...)))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			err = b.enforceGKETCPXOOwnership(tcpxoBundlerTestResult(tt.mapping))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("enforceGKETCPXOOwnership() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("enforceGKETCPXOOwnership() error = %v, want substring %q", err, tt.wantErr)
			}
			if tt.wantErrNot != "" && strings.Contains(err.Error(), tt.wantErrNot) {
				t.Errorf("enforceGKETCPXOOwnership() error contains %q: %v", tt.wantErrNot, err)
			}
		})
	}
}

func TestEnforceGKETCPXOOwnershipNonFingerprintRecipe(t *testing.T) {
	t.Parallel()

	// A recipe outside the fingerprint family carries no mapping and must be
	// left alone even with overrides that would be rejected on one.
	result := tcpxoBundlerTestResult(false)
	result.ComponentRefs[1].ManifestFiles = nil

	b, err := New(WithConfig(config.NewConfig(config.WithValueOverrides(
		map[string]map[string]string{
			"trainer": {"tcpxoInterfaces": "anything"},
		}))))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := b.enforceGKETCPXOOwnership(result); err != nil {
		t.Fatalf("enforceGKETCPXOOwnership() error = %v, want nil on a non-fingerprint recipe", err)
	}
}

// TestEnforceGKETCPXOOwnershipNilConfig covers the defensive nil-config
// branch: Make rejects a nil bundler config before reaching this gate, so
// production never enters it — the test clears Config after New to pin the
// branch's behavior for future callers.
func TestEnforceGKETCPXOOwnershipNilConfig(t *testing.T) {
	t.Parallel()

	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// New supplies defaults; explicitly clear them to exercise the nil branch.
	b.Config = nil

	if err := b.enforceGKETCPXOOwnership(tcpxoBundlerTestResult(false)); err == nil {
		t.Fatal("enforceGKETCPXOOwnership() = nil on a fingerprint recipe without the mapping, want fail-closed")
	}
	if err := b.enforceGKETCPXOOwnership(tcpxoBundlerTestResult(true)); err != nil {
		t.Fatalf("enforceGKETCPXOOwnership() error = %v, want nil with the mapping recorded", err)
	}
}
