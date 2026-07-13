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

	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	v1 "k8s.io/api/core/v1"
)

// warnIfHeterogeneousNodes logs a warning if GPU nodes have different instance
// types. NCCL all-reduce requires homogeneous nodes for optimal performance.
// Under normal NCCL flow this is redundant on EKS after resolveTargetGPUNodes,
// which narrows both by accelerator (nvidia.com/gpu.product) and by
// instance-type. Kept as insurance for code paths that bypass the resolver and
// for discoverEKSNodeConfig call sites that might use the snapshot's GPU node
// list directly.
func warnIfHeterogeneousNodes(nodes []v1.Node) {
	if len(nodes) < 2 {
		return
	}
	firstInstance := nodes[0].Labels["node.kubernetes.io/instance-type"]
	for _, node := range nodes[1:] {
		nodeInstance := node.Labels["node.kubernetes.io/instance-type"]
		if nodeInstance != firstInstance {
			slog.Warn("Heterogeneous GPU node instance types detected — NCCL requires homogeneous nodes",
				"expected", firstInstance, "found", nodeInstance, "node", node.Name)
		}
	}
}

// efaResourceName is the EFA extended resource published by the AWS EFA
// Kubernetes device plugin on EKS GPU nodes.
const efaResourceName = v1.ResourceName("vpc.amazonaws.com/efa")

// discoverEKSNodeConfig reads the instance type label (from the first target
// node) and the EFA adapter count validated as uniform across all target GPU
// nodes. Returns an error if the instance-type label is missing or if the EFA
// count is not uniform across the cohort (partial device-plugin rollout). An
// EFA count of 0 is valid when uniform (device plugin not installed — NCCL
// falls back to TCP).
func discoverEKSNodeConfig(nodes []v1.Node) (string, int, error) {
	if len(nodes) == 0 {
		return "", 0, aicrErrors.New(aicrErrors.ErrCodeInternal,
			"no target GPU nodes for EKS discovery")
	}

	instanceType := nodes[0].Labels["node.kubernetes.io/instance-type"]
	if instanceType == "" {
		return "", 0, aicrErrors.New(aicrErrors.ErrCodeInternal,
			"GPU node missing node.kubernetes.io/instance-type label")
	}

	efaCount, err := uniformFabricResourceCount(nodes, efaResourceName)
	if err != nil {
		return "", 0, err
	}

	return instanceType, efaCount, nil
}

// uniformFabricResourceCount returns the allocatable count of an extended
// fabric resource (EFA on EKS, the shared RDMA pool on AKS) across the full set
// of resolved target GPU nodes, requiring every node to advertise the SAME
// count. NCCL sizes every worker identically, so deriving the count from only
// the first node silently mis-sizes the fabric request during a partial
// device-plugin rollout or a mixed-instance cohort.
//
// A uniform count is returned as-is; a uniform zero is valid and selects the
// existing TCP fallback. A disagreement across nodes (mixed/partial rollout)
// returns an error so the validator fails closed and the operator re-runs once
// the cluster has converged.
func uniformFabricResourceCount(nodes []v1.Node, resource v1.ResourceName) (int, error) {
	if len(nodes) == 0 {
		return 0, aicrErrors.New(aicrErrors.ErrCodeInternal,
			fmt.Sprintf("no target GPU nodes to inspect for %s", resource))
	}

	firstQuantity := nodes[0].Status.Allocatable[resource]
	first := int(firstQuantity.Value())

	present := 0
	uniform := true
	for i := range nodes {
		quantity := nodes[i].Status.Allocatable[resource]
		count := int(quantity.Value())
		if count > 0 {
			present++
		}
		if count != first {
			uniform = false
		}
	}

	if !uniform {
		return 0, aicrErrors.New(aicrErrors.ErrCodeInternal,
			fmt.Sprintf("%s present on %d of %d target GPU nodes — cluster still converging "+
				"or device plugin not fully rolled out; re-run once uniform",
				resource, present, len(nodes)))
	}

	return first, nil
}

// buildEFAResourceLine returns the YAML line for EFA resource requests/limits
// at the correct indentation, or an empty string if efaCount is 0.
func buildEFAResourceLine(efaCount int, indent string) string {
	if efaCount == 0 {
		return ""
	}
	return fmt.Sprintf("%svpc.amazonaws.com/efa: \"%d\"", indent, efaCount)
}
