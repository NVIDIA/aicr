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
	"log/slog"

	v1 "k8s.io/api/core/v1"
)

// aksRdmaSharedResource is the extended resource published by the network
// operator's rdma-shared-device-plugin on AICR AKS clusters (selector
// vendors=15b3 drivers=mlx5_core). The resource name hca_shared_devices_a is
// pinned by recipes/components/network-operator/manifests/
// nic-cluster-policy-aks.yaml for workload compatibility — keep the two in
// sync. It is a *shared* device pool: a pod that requests one unit is granted
// access to every InfiniBand HCA on the node (/dev/infiniband/*), so NCCL
// workers request exactly 1 regardless of the HCA count.
const aksRdmaSharedResource = "rdma/hca_shared_devices_a"

// applyAKSTemplateData populates the AKS-specific runtime template variables:
// the ${RDMA_RESOURCE_LIMITS}/${RDMA_RESOURCE_REQUESTS} worker resource lines
// derived from the shared RDMA pool count validated as uniform across all
// target GPU nodes, with the same TCP fallback the EKS zero-EFA path uses
// (reduced max message size) when the RDMA shared device plugin is absent.
//
// A count of 0 is valid (network-operator RDMA shared device plugin not
// installed) when uniform — NCCL falls back to TCP over the pod network,
// mirroring the EKS zero-EFA behavior. On ND-series InfiniBand SKUs
// (e.g. Standard_ND96isr_H100_v5) the plugin advertises a large shared pool
// (allocatable "1k" = 1000). A mixed/partial rollout across the cohort fails
// closed rather than sizing every worker from the first node.
func applyAKSTemplateData(config *gpuConfiguration, templateData map[string]string) error {
	warnIfHeterogeneousNodes(config.Nodes)
	rdmaCount, err := uniformFabricResourceCount(config.Nodes, v1.ResourceName(aksRdmaSharedResource))
	if err != nil {
		return err
	}
	// Indentation matches the resource block position in runtime.yaml.
	const rdmaIndent = "                      "
	templateData["RDMA_RESOURCE_LIMITS"] = buildAKSRdmaResourceLine(rdmaCount, rdmaIndent)
	templateData["RDMA_RESOURCE_REQUESTS"] = buildAKSRdmaResourceLine(rdmaCount, rdmaIndent)
	if rdmaCount == 0 {
		templateData["MAX_MESSAGE_SIZE"] = maxMessageSizeTCP
		slog.Warn("No shared RDMA devices found — NCCL will use TCP (reduced bandwidth)",
			"resource", aksRdmaSharedResource, "maxMessageSize", maxMessageSizeTCP)
	} else {
		slog.Info("Discovered AKS shared RDMA device pool", "resource", aksRdmaSharedResource, "allocatable", rdmaCount)
	}
	return nil
}

// buildAKSRdmaResourceLine returns the YAML line requesting one unit of the
// shared RDMA resource at the correct indentation, or an empty string when
// the node advertises none (TCP fallback — the placeholder line is dropped
// from the rendered runtime). Requesting "1" (not the pool size) is the
// rdma-shared-device-plugin contract: any positive request mounts every
// shared IB device into the container.
func buildAKSRdmaResourceLine(rdmaCount int, indent string) string {
	if rdmaCount == 0 {
		return ""
	}
	return fmt.Sprintf("%s%s: \"1\"", indent, aksRdmaSharedResource)
}
