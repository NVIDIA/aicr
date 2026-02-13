// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package helper

import (
	"context"
	"fmt"

	"github.com/NVIDIA/eidos/pkg/validator/checks"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const GpuResourceName = "nvidia.com/gpu"

// FindSchedulableGpuNodes finds nodes that are schedulable and have allocatable GPU resources.
func FindSchedulableGpuNodes(ctx *checks.ValidationContext) ([]v1.Node, error) {
	// List all nodes in the cluster
	nodeList, err := ctx.Clientset.CoreV1().Nodes().List(ctx.Context, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	var gpuNodes []v1.Node
	for _, node := range nodeList.Items {
		// Condition 1: Check if node is schedulable
		if node.Spec.Unschedulable {
			continue // Skip node if it's unschedulable
		}

		// Condition 2: Check Allocatable GPU resources
		if q, ok := node.Status.Allocatable[v1.ResourceName(GpuResourceName)]; ok && !q.IsZero() {
			// All conditions met, add the node
			gpuNodes = append(gpuNodes, node)
		}
	}
	return gpuNodes, nil
}

// IsNodeGpuBusy checks if any non-terminal pods on the specified node are currently using GPU resources.
func IsNodeGpuBusy(ctx context.Context, client kubernetes.Interface, nodeName string) (bool, error) {
	// Use a field selector to only list pods scheduled on the target node
	selector := fmt.Sprintf("spec.nodeName=%s", nodeName)
	podList, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: selector,
	})
	if err != nil {
		// If listing pods fails for the node, treat it cautiously as potentially busy or problematic
		return true, fmt.Errorf("failed to list pods on node %s: %w", nodeName, err)
	}

	for _, pod := range podList.Items {
		// Skip pods that are already finished
		if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
			continue
		}
		for _, container := range pod.Spec.Containers {
			// Check Limits first
			if res := container.Resources.Limits; res != nil {
				if q, ok := res[v1.ResourceName(GpuResourceName)]; ok && !q.IsZero() {
					return true, nil // Found a running/pending pod using GPU limits
				}
			}
			// Then check Requests
			if res := container.Resources.Requests; res != nil {
				if q, ok := res[v1.ResourceName(GpuResourceName)]; ok && !q.IsZero() {
					return true, nil // Found a running/pending pod using GPU requests
				}
			}
		}
	}
	return false, nil // No running/pending pods found using GPUs on this node
}
