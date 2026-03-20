// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
// TODO: Add GPU product compatibility layer (nvidia.com/gpu.product family grouping).
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

// discoverEKSNodeConfig reads the instance type label and EFA adapter count
// from a GPU node. Returns an error if the label is missing. EFA count of 0
// is valid (device plugin not installed — NCCL falls back to TCP).
func discoverEKSNodeConfig(node v1.Node) (string, int, error) {
	instanceType := node.Labels["node.kubernetes.io/instance-type"]
	if instanceType == "" {
		return "", 0, aicrErrors.New(aicrErrors.ErrCodeInternal,
			"GPU node missing node.kubernetes.io/instance-type label")
	}

	efaResource := v1.ResourceName("vpc.amazonaws.com/efa")
	efaQuantity := node.Status.Allocatable[efaResource]
	efaCount := int(efaQuantity.Value())

	return instanceType, efaCount, nil
}

// buildEFAResourceLine returns the YAML line for EFA resource requests/limits
// at the correct indentation, or an empty string if efaCount is 0.
func buildEFAResourceLine(efaCount int, indent string) string {
	if efaCount == 0 {
		return ""
	}
	return fmt.Sprintf("%svpc.amazonaws.com/efa: \"%d\"", indent, efaCount)
}
