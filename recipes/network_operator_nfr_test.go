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

package recipes

import (
	"io/fs"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestOKENetworkOperatorKeepsChartNodeFeatureRule pins the label supply chain
// behind the deployment validator's RDMA readiness gate: the gate's node
// cohort is nodes labeled feature.node.kubernetes.io/pci-15b3.present=true
// (validators/helper.PCIMellanoxPresentLabel). On OKE that label comes from
// the network-operator chart's own nvidia-nics-rules NodeFeatureRule, which
// renders whenever `deployNodeFeatureRules` is left at the chart default
// (true) — the OKE values disable the chart's NFD deployment (nfd.enabled:
// false, GPU Operator's NFD processes the rule) but must NOT disable the
// rule itself. AKS is the deliberate exception: it sets
// deployNodeFeatureRules: false and attaches its own targeted rule manifest
// (nfd-network-rule.yaml) instead.
//
// A contributor copying AKS's `deployNodeFeatureRules: false` into an OKE
// values file — without also attaching a rule manifest — would leave no
// producer for the cohort label, and the RDMA gate would fail closed on
// every OKE fabric deploy ("no schedulable Mellanox RDMA-capable GPU nodes
// observed"). This test turns that mistake into an immediate failure.
func TestOKENetworkOperatorKeepsChartNodeFeatureRule(t *testing.T) {
	t.Parallel()

	for _, p := range []string{
		"components/network-operator/values-oke-gb200.yaml",
		"components/network-operator/values-oke-l40s.yaml",
	} {
		t.Run(p, func(t *testing.T) {
			t.Parallel()
			data, err := fs.ReadFile(FS, p)
			if err != nil {
				t.Fatalf("read %s: %v", p, err)
			}
			var values map[string]any
			if err := yaml.Unmarshal(data, &values); err != nil {
				t.Fatalf("parse %s: %v", p, err)
			}
			v, present := values["deployNodeFeatureRules"]
			if !present {
				return // chart default (true) applies — the rule renders
			}
			enabled, ok := v.(bool)
			if !ok || !enabled {
				t.Fatalf("%s sets deployNodeFeatureRules=%v: the RDMA readiness gate's "+
					"cohort label (pci-15b3.present) has no other producer on OKE — either "+
					"leave the chart default or attach a targeted NodeFeatureRule manifest "+
					"like AKS does", p, v)
			}
		})
	}
}
