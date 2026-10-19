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
	"reflect"
	"testing"

	"github.com/NVIDIA/aicr/pkg/inventory"
)

// inventoryFixture is declared out of sorted order, and every component's
// namespace differs from its name, so neither the sort nor the namespace can
// be produced by an adapter that reached for the wrong field.
func inventoryFixture() *ComponentRegistry {
	return &ComponentRegistry{
		Components: []ComponentConfig{
			{
				Name:     "nodewright-operator",
				Helm:     HelmConfig{DefaultVersion: "v0.18.0", DefaultNamespace: "nodewright-system"},
				Upgrades: UpgradesConfig{File: "components/nodewright-operator/upgrades.yaml"},
			},
			{
				// A pinned Kustomize tag is not an upstream chart: the
				// version of whatever wrapper carries it in a cluster is the
				// wrapper's, never the payload's.
				Name:      "dra-node-labeler",
				Helm:      HelmConfig{DefaultNamespace: "kube-system"},
				Kustomize: KustomizeConfig{DefaultSource: "https://example.test/x", DefaultTag: "v1.2.3"},
			},
			{
				Name: "kueue-queues",
				Helm: HelmConfig{DefaultNamespace: "kueue-system"},
			},
			{
				// No upgrades.file: dropped by UpgradeComponents, kept here.
				Name: "cert-manager",
				Helm: HelmConfig{DefaultVersion: "1.20.2", DefaultNamespace: "cert-manager-system"},
			},
		},
	}
}

func TestInventoryComponents(t *testing.T) {
	want := []inventory.Component{
		{Name: "cert-manager", Namespace: "cert-manager-system", HasUpstreamChart: true},
		{Name: "dra-node-labeler", Namespace: "kube-system", HasUpstreamChart: false},
		{Name: "kueue-queues", Namespace: "kueue-system", HasUpstreamChart: false},
		{Name: "nodewright-operator", Namespace: "nodewright-system", HasUpstreamChart: true},
	}

	got := InventoryComponents(inventoryFixture())
	if !reflect.DeepEqual(got, want) {
		t.Errorf("InventoryComponents() = %+v, want %+v", got, want)
	}
}

// TestInventoryComponentsKeepsRecordlessEntries states the deliberate
// difference from UpgradeComponents next door: mapping a cluster record needs
// every component, including the ones that reference no transition record,
// because a record that maps to none of them is the signal that the mapping
// is broken.
func TestInventoryComponentsKeepsRecordlessEntries(t *testing.T) {
	registry := inventoryFixture()

	upgrades := UpgradeComponents(registry)
	if len(upgrades) != 1 || upgrades[0].Name != "nodewright-operator" {
		t.Fatalf("UpgradeComponents() = %+v, want only nodewright-operator", upgrades)
	}

	if got := len(InventoryComponents(registry)); got != len(registry.Components) {
		t.Errorf("InventoryComponents() returned %d of %d registry entries",
			got, len(registry.Components))
	}
}

func TestInventoryComponentsNilRegistry(t *testing.T) {
	if got := InventoryComponents(nil); got != nil {
		t.Errorf("InventoryComponents(nil) = %+v, want nil", got)
	}
}
