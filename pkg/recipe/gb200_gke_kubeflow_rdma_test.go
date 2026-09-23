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
	"slices"
	"testing"
)

// TestGB200GKEKubeflowShipsRDMARuntime asserts the resolved GB200 GKE COS
// Kubeflow leaf declares the GPUDirect-RDMA runtime among its kubeflow-trainer
// manifests.
func TestGB200GKEKubeflowShipsRDMARuntime(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}

	criteria := &Criteria{
		Service:     CriteriaServiceGKE,
		Accelerator: CriteriaAcceleratorGB200,
		OS:          CriteriaOSCOS,
		Intent:      CriteriaIntentTraining,
		Platform:    CriteriaPlatformKubeflow,
	}
	result, err := store.BuildRecipeResult(ctx, criteria)
	if err != nil {
		t.Fatalf("BuildRecipeResult: %v", err)
	}
	ref := result.GetComponentRef(kubeflowTrainerComponentName)
	if ref == nil {
		t.Fatal("kubeflow-trainer componentRef not present in resolved recipe")
	}
	const rdmaRuntime = "components/kubeflow-trainer/manifests/torch-distributed-rdma-cluster-training-runtime.yaml"
	if !slices.Contains(ref.ManifestFiles, rdmaRuntime) {
		t.Errorf("kubeflow-trainer manifestFiles = %v, want %s among them", ref.ManifestFiles, rdmaRuntime)
	}
}
