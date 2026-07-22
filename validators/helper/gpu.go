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

// Package helper provides shared utilities for v2 validator containers.
package helper

import (
	"context"
	"fmt"

	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// GpuResourceName is the Kubernetes resource name for NVIDIA GPUs.
const GpuResourceName = "nvidia.com/gpu"

// AKSRdmaSharedResource is the extended resource the network operator's
// rdma-shared-device-plugin publishes on Mellanox RDMA nodes. Its name is pinned
// by recipes/components/network-operator/manifests/nic-cluster-policy-aks.yaml
// (resourceName hca_shared_devices_a). The resource is transport-agnostic — the
// same pool backs InfiniBand and RoCE — so nothing keyed off it is IB-specific.
// Both the deployment-phase RDMA fabric readiness gate and the NCCL performance
// consumer key off this exact resource, so it lives here as the single source of
// truth rather than a duplicated literal in each validator binary.
const AKSRdmaSharedResource = "rdma/hca_shared_devices_a"

// PCIMellanoxPresentLabel is the NFD feature label marking nodes with a Mellanox
// (PCI vendor 15b3) NIC — the vendor NFD detects for any ConnectX card, whether
// it runs InfiniBand or RoCE, so this is an RDMA-capability marker, not an IB
// one. It is the nodeAffinity the AKS NicClusterPolicy uses to place the RDMA
// fabric (nic-cluster-policy-aks.yaml), so it identifies exactly the nodes that
// can ever advertise AKSRdmaSharedResource.
const PCIMellanoxPresentLabel = "feature.node.kubernetes.io/pci-15b3.present"

// FindSchedulableGpuNodes finds nodes that are schedulable and have allocatable GPU resources.
func FindSchedulableGpuNodes(ctx context.Context, clientset kubernetes.Interface) ([]v1.Node, error) {
	nodeList, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to list nodes", err)
	}

	var gpuNodes []v1.Node
	for _, node := range nodeList.Items {
		if node.Spec.Unschedulable {
			continue
		}
		if q, ok := node.Status.Allocatable[v1.ResourceName(GpuResourceName)]; ok && !q.IsZero() {
			gpuNodes = append(gpuNodes, node)
		}
	}
	return gpuNodes, nil
}

// IsNodeGpuBusy checks if any non-terminal pods on the specified node are currently using GPU resources.
func IsNodeGpuBusy(ctx context.Context, client kubernetes.Interface, nodeName string) (bool, error) {
	selector := fmt.Sprintf("spec.nodeName=%s", nodeName)
	podList, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: selector,
	})
	if err != nil {
		return true, aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
			fmt.Sprintf("failed to list pods on node %s", nodeName), err)
	}

	for _, pod := range podList.Items {
		if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
			continue
		}
		for _, container := range pod.Spec.Containers {
			if res := container.Resources.Limits; res != nil {
				if q, ok := res[v1.ResourceName(GpuResourceName)]; ok && !q.IsZero() {
					return true, nil
				}
			}
			if res := container.Resources.Requests; res != nil {
				if q, ok := res[v1.ResourceName(GpuResourceName)]; ok && !q.IsZero() {
					return true, nil
				}
			}
		}
	}
	return false, nil
}
