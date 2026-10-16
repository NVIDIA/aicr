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
	"context"
	"encoding/json"
	"slices"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestNebiusB300Recipe(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if store.Overlays["b300-mk8s-ubuntu-inference-dynamo"] == nil {
		t.Fatal("missing native Nebius B300 leaf")
	}
	result, err := store.BuildRecipeResult(ctx, &Criteria{Service: "mk8s", Accelerator: "b300", OS: "ubuntu", Intent: "inference", Platform: "dynamo", Nodes: 2})
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"gpu-operator": "devicePlugin", "nvidia-dra-driver-gpu": "resources"} {
		values, valuesErr := result.GetValuesForComponent(name)
		if valuesErr != nil {
			t.Fatal(valuesErr)
		}
		if values[path] == nil {
			t.Errorf("%s missing %s", name, path)
		}
	}
	gpu, err := result.GetValuesForComponent("gpu-operator")
	if err != nil {
		t.Fatal(err)
	}
	if gpu["devicePlugin"].(map[string]any)["enabled"] != true {
		t.Error("device plugin must advertise whole GPUs")
	}
	dra, err := result.GetValuesForComponent("nvidia-dra-driver-gpu")
	if err != nil {
		t.Fatal(err)
	}
	resources := dra["resources"].(map[string]any)
	if resources["gpus"].(map[string]any)["enabled"] != false {
		t.Error("full-GPU DRA must be disabled")
	}
	if resources["computeDomains"].(map[string]any)["enabled"] != true {
		t.Error("ComputeDomain DRA must remain enabled")
	}
}

func TestNebiusSharedRDMA(t *testing.T) {
	data, err := GetManifestContent("components/network-operator/manifests/nic-cluster-policy-nebius-b300.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var policy struct {
		Spec struct {
			SecondaryNetwork any `json:"secondaryNetwork"`
			RDMA             struct {
				Config string `json:"config"`
			} `json:"rdmaSharedDevicePlugin"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(data, &policy); err != nil {
		t.Fatal(err)
	}
	if policy.Spec.SecondaryNetwork != nil {
		t.Fatal("RDMA device sharing must not require Multus or IPoIB")
	}
	var config struct {
		Pools []struct {
			Name      string `json:"resourceName"`
			Prefix    string `json:"resourcePrefix"`
			Max       int    `json:"rdmaHcaMax"`
			Selectors struct {
				IfNames []string `json:"ifNames"`
			} `json:"selectors"`
		} `json:"configList"`
	}
	if err := json.Unmarshal([]byte(policy.Spec.RDMA.Config), &config); err != nil {
		t.Fatal(err)
	}
	if len(config.Pools) != 1 {
		t.Fatalf("want one shared pool, got %d", len(config.Pools))
	}
	pool := config.Pools[0]
	if pool.Name != "ib" || pool.Prefix != "rdma" || pool.Max != 63 {
		t.Fatalf("unexpected RDMA resource: %+v", pool)
	}
	if !slices.Equal(pool.Selectors.IfNames, []string{"ib4", "ib5", "ib6", "ib7", "ib8", "ib9", "ib10", "ib11"}) {
		t.Fatalf("incorrect active HCA set: %v", pool.Selectors.IfNames)
	}
}
