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
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// discoverGKEGPUNICNetworks lists networks.networking.gke.io and returns
// GPU NIC network names (those containing "gpu-nic"), sorted alphabetically.
// GKE clusters provision these with cluster-specific prefixes (e.g.,
// "aicr-demo2-gpu-nic-0"); the names cannot be hardcoded.
func discoverGKEGPUNICNetworks(ctx context.Context, dynamicClient dynamic.Interface) ([]string, error) {
	networkGVR := schema.GroupVersionResource{
		Group: "networking.gke.io", Version: "v1", Resource: "networks",
	}

	listCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()

	networks, err := dynamicClient.Resource(networkGVR).List(listCtx, metav1.ListOptions{})
	if err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to list GKE networks", err)
	}

	var gpuNICs []string
	for _, n := range networks.Items {
		name := n.GetName()
		if strings.Contains(name, "gpu-nic") {
			gpuNICs = append(gpuNICs, name)
		}
	}

	sort.Strings(gpuNICs)
	return gpuNICs, nil
}

// buildGKENetworkInterfacesAnnotation builds the networking.gke.io/interfaces
// annotation value from discovered GPU NIC network names.
// Maps eth0 → default, eth1 → gpuNICs[0], eth2 → gpuNICs[1], etc.
func buildGKENetworkInterfacesAnnotation(gpuNICs []string) string {
	interfaces := make([]string, 0, len(gpuNICs)+1)
	interfaces = append(interfaces, `{"interfaceName":"eth0","network":"default"}`)
	for i, nic := range gpuNICs {
		interfaces = append(interfaces, fmt.Sprintf(`{"interfaceName":"eth%d","network":"%s"}`, i+1, nic))
	}
	return "[" + strings.Join(interfaces, ",") + "]"
}

// buildNRIDeviceAnnotation builds the devices.gke.io/container.tcpxo-daemon
// annotation value for NRI device injection. Lists /dev/nvidia0..N-1 plus
// the control and DMA devices the tcpxo-daemon needs without privileged mode.
// Each line after the first is indented with 20 spaces to match the YAML
// template indentation at the ${NRI_DEVICE_ANNOTATION} placeholder.
func buildNRIDeviceAnnotation(gpuCount int) string {
	const indent = "                    " // 20 spaces — matches template position
	lines := make([]string, 0, gpuCount+3)
	for i := range gpuCount {
		lines = append(lines, fmt.Sprintf("- path: /dev/nvidia%d", i))
	}
	lines = append(lines, "- path: /dev/nvidiactl")
	lines = append(lines, "- path: /dev/nvidia-uvm")
	lines = append(lines, "- path: /dev/dmabuf_import_helper")
	return strings.Join(lines, "\n"+indent)
}
