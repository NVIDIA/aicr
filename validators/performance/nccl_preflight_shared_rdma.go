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
	"fmt"

	corev1 "k8s.io/api/core/v1"

	aicrErrors "aicr.run/pkg/errors"
	"aicr.run/pkg/recipe"
	"aicr.run/validators/helper"
)

// sharedRDMANetPreflightApplies reports whether the NET workers reach their
// RoCE devices through the network-operator RDMA shared device plugin, which
// is the GB10 (DGX Spark) on NVKE template, testdata/gb10/nvke/runtime-net.yaml.
func sharedRDMANetPreflightApplies(variant ncclVariant, accelerator recipe.CriteriaAcceleratorType, service recipe.CriteriaServiceType) bool {
	return variant == variantNET &&
		accelerator == recipe.CriteriaAcceleratorGB10 &&
		service == recipe.CriteriaServiceNVKE
}

// preflightSharedRDMAResource fails before any TrainJob is created when the
// target GPU nodes do not all advertise the same positive count of the shared
// RDMA resource. Each worker requests one unit, so a missing or partial
// rollout would otherwise leave workers Pending until the launcher times out.
func preflightSharedRDMAResource(nodes []corev1.Node) error {
	count, err := uniformFabricResourceCount(nodes, corev1.ResourceName(helper.AKSRdmaSharedResource))
	if err != nil {
		return err
	}
	if count == 0 {
		return aicrErrors.New(aicrErrors.ErrCodeNotFound, fmt.Sprintf(
			"%s is not allocatable on the %d target GPU nodes: the NET check needs the network-operator "+
				"RDMA shared device plugin and a ConnectX-7 on every node",
			helper.AKSRdmaSharedResource, len(nodes)))
	}
	return nil
}
