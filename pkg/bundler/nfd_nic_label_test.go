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
	"slices"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/recipe"
	corev1 "k8s.io/api/core/v1"
)

const (
	wantNFDNICWarningA = "Warning: network-operator is in this bundle, but nfd strips pci.device " +
		"(worker.config.core.noPublishFeatures) and has no nfd-worker rule labelling " +
		"feature.node.kubernetes.io/pci-15b3.present (worker.config.sources.custom). " +
		"network-operator's NodeFeatureRule reads pci.device, so after nfd-worker restarts nothing " +
		"labels the NVIDIA NIC nodes, and the pods that select on that label (the NicClusterPolicy " +
		"operands) leave them. Select components/nfd/values-nvidia-nics.yaml " +
		"(values-nvidia-nics-aks.yaml on AKS) as the nfd valuesFile in the overlay that adds " +
		"network-operator, or include its rule in a worker.config.sources.custom override."
	wantNFDNICWarningB = "Warning: nfd labels NVIDIA NIC nodes feature.node.kubernetes.io/pci-15b3.present " +
		"(worker.config.sources.custom), but network-operator is not in this bundle. The GPU Operator " +
		"validator waits on those nodes for the network-operator MOFED driver when the GPU Operator " +
		"manages the driver: driver.enabled=true, driver.rdma.enabled=true and " +
		"driver.rdma.useHostMofed=false. Unless network-operator is " +
		"installed outside this bundle, drop the rule with --set-json " +
		"'nfd:worker.config.sources.custom=[]', which removes every nfd-worker custom rule."
)

var (
	nfdNICTestEKS = recipe.Criteria{
		Service: recipe.CriteriaServiceEKS, Accelerator: recipe.CriteriaAcceleratorH100,
		OS: recipe.CriteriaOSUbuntu, Intent: recipe.CriteriaIntentTraining,
	}
	nfdNICTestAKS = recipe.Criteria{
		Service: recipe.CriteriaServiceAKS, Accelerator: recipe.CriteriaAcceleratorH100,
		OS: recipe.CriteriaOSUbuntu, Intent: recipe.CriteriaIntentTraining,
	}
	nfdNICTestGeneric = recipe.Criteria{
		Service: recipe.CriteriaServiceGeneric, Accelerator: recipe.CriteriaAcceleratorGB300,
		OS: recipe.CriteriaOSUbuntu, Intent: recipe.CriteriaIntentTraining,
	}
)

func TestWarnNFDNICLabelMismatch(t *testing.T) {
	addNetOp := func(values map[string]map[string]any) {
		values[networkOperatorComponentName] = map[string]any{}
	}
	setNFD := func(path string, value any) func(map[string]map[string]any) {
		return func(values map[string]map[string]any) {
			component.SetValueByPath(values["nfd"], path, value)
		}
	}
	dropNetOp := func(values map[string]map[string]any) {
		delete(values, networkOperatorComponentName)
	}

	tests := []struct {
		name     string
		criteria recipe.Criteria
		mutate   []func(map[string]map[string]any)
		bundlers []string
		want     string
	}{
		{
			name:     "eks without network-operator",
			criteria: nfdNICTestEKS,
		},
		{
			name:     "aks network-operator with the aks rule",
			criteria: nfdNICTestAKS,
		},
		{
			name:     "generic network-operator with the generic rule",
			criteria: nfdNICTestGeneric,
		},
		{
			name:     "A: network-operator added on base nfd values",
			criteria: nfdNICTestEKS,
			mutate:   []func(map[string]map[string]any){addNetOp},
			want:     wantNFDNICWarningA,
		},
		{
			name:     "A: exact pci.device strip",
			criteria: nfdNICTestEKS,
			mutate: []func(map[string]map[string]any){addNetOp,
				setNFD("worker.config.core.noPublishFeatures", []any{"pci.device"})},
			want: wantNFDNICWarningA,
		},
		{
			name:     "A: wildcard strip",
			criteria: nfdNICTestEKS,
			mutate: []func(map[string]map[string]any){addNetOp,
				setNFD("worker.config.core.noPublishFeatures", []any{"*"})},
			want: wantNFDNICWarningA,
		},
		{
			name:     "A silent: pci republished",
			criteria: nfdNICTestEKS,
			mutate: []func(map[string]map[string]any){addNetOp,
				setNFD("worker.config.core.noPublishFeatures", []any{"kernel.config", "kernel.enabledmodule"})},
		},
		{
			name:     "A silent: pattern without a star is exact",
			criteria: nfdNICTestEKS,
			mutate: []func(map[string]map[string]any){addNetOp,
				setNFD("worker.config.core.noPublishFeatures", []any{"pci"})},
		},
		{
			name:     "A silent: rule with the bare label",
			criteria: nfdNICTestEKS,
			mutate: []func(map[string]map[string]any){addNetOp,
				setNFD("worker.config.sources.custom", []any{
					map[string]any{
						"name":   "my-nics",
						"labels": map[string]any{"pci-15b3.present": "true"},
					},
				})},
		},
		{
			name:     "B: network-operator disabled",
			criteria: nfdNICTestAKS,
			mutate:   []func(map[string]map[string]any){dropNetOp},
			want:     wantNFDNICWarningB,
		},
		{
			name:     "B silent: subset bundle",
			criteria: nfdNICTestAKS,
			mutate:   []func(map[string]map[string]any){dropNetOp},
			bundlers: []string{"nfd"},
		},
		{
			name:     "B silent: rule dropped",
			criteria: nfdNICTestAKS,
			mutate: []func(map[string]map[string]any){dropNetOp,
				setNFD("worker.config.sources.custom", []any{})},
		},
		{
			name:     "nfd absent",
			criteria: nfdNICTestAKS,
			mutate: []func(map[string]map[string]any){func(values map[string]map[string]any) {
				delete(values, "nfd")
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			criteria := tt.criteria
			result, err := recipe.NewBuilder().BuildFromCriteria(t.Context(), &criteria)
			if err != nil {
				t.Fatalf("BuildFromCriteria() error = %v", err)
			}
			var opts []config.Option
			if tt.bundlers != nil {
				opts = append(opts, config.WithBundlers(tt.bundlers))
			}
			b, err := New(WithConfig(config.NewConfig(opts...)))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			values, err := b.extractComponentValues(t.Context(), result)
			if err != nil {
				t.Fatalf("extractComponentValues() error = %v", err)
			}
			if _, ok := values["nfd"]; !ok {
				t.Fatalf("recipe %+v resolved without nfd", criteria)
			}
			for _, m := range tt.mutate {
				m(values)
			}

			b.warnNFDNICLabelMismatch(values)

			var want []string
			if tt.want != "" {
				want = []string{tt.want}
			}
			if !slices.Equal(b.warnings, want) {
				t.Errorf("warnings = %q\nwant       %q", b.warnings, want)
			}
		})
	}
}

// TestMakeNFDNICLabelMismatchSurfacesWarning drives both directions through
// Make: the user-facing paths are an overlay that adds network-operator to a
// recipe on base nfd values, and the AKS --set networkoperator:enabled=false
// opt-out without the matching nfd override.
func TestMakeNFDNICLabelMismatchSurfacesWarning(t *testing.T) {
	addNetOp := func(t *testing.T, result *recipe.RecipeResult) {
		t.Helper()
		donorCriteria := nfdNICTestGeneric
		donor, err := recipe.NewBuilder().BuildFromCriteria(t.Context(), &donorCriteria)
		if err != nil {
			t.Fatalf("BuildFromCriteria(generic) error = %v", err)
		}
		ref := donor.GetComponentRef(networkOperatorComponentName)
		if ref == nil {
			t.Fatal("generic recipe resolved without network-operator")
		}
		// An overlay ref names the component only; the registry fills the rest.
		added := recipe.ComponentRef{
			Name:           ref.Name,
			Namespace:      ref.Namespace,
			Chart:          ref.Chart,
			Type:           ref.Type,
			Source:         ref.Source,
			Version:        ref.Version,
			DependencyRefs: ref.DependencyRefs,
		}
		result.ComponentRefs = append(result.ComponentRefs, added)
		result.DeploymentOrder = append(result.DeploymentOrder, added.Name)
	}

	tests := []struct {
		name     string
		criteria recipe.Criteria
		edit     func(*testing.T, *recipe.RecipeResult)
		options  []config.Option
		want     string
	}{
		{
			name:     "A: overlay adds network-operator",
			criteria: nfdNICTestEKS,
			edit:     addNetOp,
			want:     wantNFDNICWarningA,
		},
		{
			name:     "B: --set networkoperator:enabled=false",
			criteria: nfdNICTestAKS,
			options: []config.Option{
				config.WithValueOverrides(map[string]map[string]string{
					"networkoperator": {"enabled": "false"},
				}),
				// AKS bundles reject the keyless toleration nodewright would render.
				config.WithAcceleratedNodeTolerations([]corev1.Toleration{{
					Key:      "nvidia.com/gpu",
					Operator: corev1.TolerationOpExists,
					Effect:   corev1.TaintEffectNoSchedule,
				}}),
			},
			want: wantNFDNICWarningB,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			criteria := tt.criteria
			result, err := recipe.NewBuilder().BuildFromCriteria(t.Context(), &criteria)
			if err != nil {
				t.Fatalf("BuildFromCriteria() error = %v", err)
			}
			if tt.edit != nil {
				tt.edit(t, result)
			}
			opts := append([]config.Option{config.WithVersion("v-test")}, tt.options...)
			b, err := New(WithConfig(config.NewConfig(opts...)))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			output, err := b.Make(t.Context(), result, t.TempDir())
			if err != nil {
				t.Fatalf("Make() error = %v", err)
			}
			if output.Deployment == nil {
				t.Fatal("Make() returned nil deployment info")
			}
			if !slices.Contains(output.Deployment.Notes, tt.want) {
				t.Errorf("deployment notes = %q, want an entry %q", output.Deployment.Notes, tt.want)
			}
		})
	}
}
