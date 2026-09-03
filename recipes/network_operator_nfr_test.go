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

// External test package: pkg/recipe imports this package for the embedded
// catalog FS, so resolving effective values through pkg/recipe from an
// in-package test would be an import cycle.
package recipes_test

import (
	"io/fs"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/recipes"
)

// TestOKENetworkOperatorKeepsChartNodeFeatureRule pins the label supply chain
// behind the deployment validator's RDMA readiness gate: the gate's node
// cohort is nodes labeled feature.node.kubernetes.io/pci-15b3.present=true
// (validators/helper.PCIMellanoxPresentLabel). On OKE that label comes from
// the network-operator chart's own nvidia-nics-rules NodeFeatureRule, which
// renders whenever `nfd.deployNodeFeatureRules` is left at the chart default
// (true) — the OKE values disable the chart's NFD deployment (nfd.enabled:
// false; the GPU Operator's NFD processes the rule) but must NOT disable the
// rule itself. AKS is the deliberate exception: it sets
// nfd.deployNodeFeatureRules: false and attaches its own targeted rule
// manifest (nfd-network-rule.yaml) instead.
//
// A contributor copying AKS's `deployNodeFeatureRules: false` into an OKE
// values file — or flipping it in the shared component base values.yaml —
// without also attaching a rule manifest would leave no producer for the
// cohort label, and the RDMA gate would fail closed on every OKE fabric
// deploy ("no schedulable Mellanox RDMA-capable GPU nodes observed"). This
// test turns that mistake into an immediate failure.
//
// It asserts on MERGED effective values (base values.yaml → overlay), exactly
// as bundle rendering resolves them, and derives the overlay set by glob so a
// future OKE fabric leaf is covered without editing this test.
func TestOKENetworkOperatorKeepsChartNodeFeatureRule(t *testing.T) {
	t.Parallel()

	overlays, err := fs.Glob(recipes.FS, "components/network-operator/values-oke-*.yaml")
	if err != nil {
		t.Fatalf("glob OKE network-operator values overlays: %v", err)
	}
	if len(overlays) == 0 {
		t.Fatal("no components/network-operator/values-oke-*.yaml overlays found; " +
			"the glob or the embedded FS layout changed")
	}

	for _, overlay := range overlays {
		t.Run(overlay, func(t *testing.T) {
			t.Parallel()

			ref := recipe.ComponentRef{Name: "network-operator", ValuesFile: overlay}
			values, err := recipe.GetComponentValuesWithContext(t.Context(), nil, &ref)
			if err != nil {
				t.Fatalf("resolve effective values for %s: %v", overlay, err)
			}

			nfd, ok := values["nfd"].(map[string]any)
			if !ok {
				// No nfd block anywhere in the merged values — both chart
				// defaults apply, including deployNodeFeatureRules: true.
				return
			}
			v, present := nfd["deployNodeFeatureRules"]
			if !present {
				return // chart default (true) applies — the rule renders
			}
			enabled, ok := v.(bool)
			if !ok || !enabled {
				t.Fatalf("effective values for %s set nfd.deployNodeFeatureRules=%v: the RDMA "+
					"readiness gate's cohort label (pci-15b3.present) has no other producer on "+
					"OKE — either leave the chart default or attach a targeted NodeFeatureRule "+
					"manifest like AKS does", overlay, v)
			}
		})
	}
}
