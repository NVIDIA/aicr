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
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDiscoverAKSRdmaCount(t *testing.T) {
	tests := []struct {
		name string
		node v1.Node
		want int
	}{
		{
			// Standard_ND96isr_H100_v5 with the network-operator
			// rdma-shared-device-plugin: allocatable is advertised as "1k"
			// (the shared pool size), which resource.Quantity parses as 1000.
			name: "ND96isr_H100_v5 with shared RDMA pool",
			node: v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"node.kubernetes.io/instance-type": "Standard_ND96isr_H100_v5",
					},
				},
				Status: v1.NodeStatus{
					Allocatable: v1.ResourceList{
						v1.ResourceName("nvidia.com/gpu"):      resource.MustParse("8"),
						v1.ResourceName(aksRdmaSharedResource): resource.MustParse("1k"),
					},
				},
			},
			want: 1000,
		},
		{
			name: "no RDMA shared device plugin (falls back to TCP)",
			node: v1.Node{
				Status: v1.NodeStatus{
					Allocatable: v1.ResourceList{
						v1.ResourceName("nvidia.com/gpu"): resource.MustParse("8"),
					},
				},
			},
			want: 0,
		},
		{
			name: "no allocatable at all",
			node: v1.Node{},
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := discoverAKSRdmaCount(tt.node); got != tt.want {
				t.Errorf("discoverAKSRdmaCount() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestApplyAKSTemplateData(t *testing.T) {
	const rdmaLine = `                      rdma/hca_shared_devices_a: "1"`
	tests := []struct {
		name           string
		allocatable    v1.ResourceList
		wantLine       string
		wantMaxMsgSize string
	}{
		{
			name: "shared RDMA pool present keeps IB message size",
			allocatable: v1.ResourceList{
				v1.ResourceName("nvidia.com/gpu"):      resource.MustParse("8"),
				v1.ResourceName(aksRdmaSharedResource): resource.MustParse("1k"),
			},
			wantLine:       rdmaLine,
			wantMaxMsgSize: maxMessageSize,
		},
		{
			name: "no RDMA pool falls back to TCP with reduced message size",
			allocatable: v1.ResourceList{
				v1.ResourceName("nvidia.com/gpu"): resource.MustParse("8"),
			},
			wantLine:       "",
			wantMaxMsgSize: maxMessageSizeTCP,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &gpuConfiguration{
				WorkerCount:     2,
				GPUCountPerNode: 8,
				TotalGPUCount:   16,
				Nodes: []v1.Node{
					{Status: v1.NodeStatus{Allocatable: tt.allocatable}},
					{Status: v1.NodeStatus{Allocatable: tt.allocatable}},
				},
			}
			templateData := map[string]string{"MAX_MESSAGE_SIZE": maxMessageSize}
			applyAKSTemplateData(config, templateData)
			if got := templateData["RDMA_RESOURCE_LIMITS"]; got != tt.wantLine {
				t.Errorf("RDMA_RESOURCE_LIMITS = %q, want %q", got, tt.wantLine)
			}
			if got := templateData["RDMA_RESOURCE_REQUESTS"]; got != tt.wantLine {
				t.Errorf("RDMA_RESOURCE_REQUESTS = %q, want %q", got, tt.wantLine)
			}
			if got := templateData["MAX_MESSAGE_SIZE"]; got != tt.wantMaxMsgSize {
				t.Errorf("MAX_MESSAGE_SIZE = %q, want %q", got, tt.wantMaxMsgSize)
			}
		})
	}
}

func TestBuildAKSRdmaResourceLine(t *testing.T) {
	tests := []struct {
		name      string
		rdmaCount int
		indent    string
		want      string
	}{
		{
			// A worker always requests exactly 1 unit, never the pool size:
			// the rdma-shared-device-plugin grants access to every shared IB
			// device per unit requested.
			name:      "shared pool of 1000 requests a single unit",
			rdmaCount: 1000,
			indent:    "                      ",
			want:      `                      rdma/hca_shared_devices_a: "1"`,
		},
		{
			name:      "single device still requests one unit",
			rdmaCount: 1,
			indent:    "                      ",
			want:      `                      rdma/hca_shared_devices_a: "1"`,
		},
		{
			name:      "no RDMA — empty string",
			rdmaCount: 0,
			indent:    "                      ",
			want:      "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildAKSRdmaResourceLine(tt.rdmaCount, tt.indent)
			if got != tt.want {
				t.Errorf("buildAKSRdmaResourceLine(%d) = %q, want %q", tt.rdmaCount, got, tt.want)
			}
		})
	}
}
