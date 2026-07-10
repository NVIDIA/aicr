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
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestApplyAKSTemplateData(t *testing.T) {
	const rdmaLine = `                      rdma/hca_shared_devices_a: "1"`
	// aksNode builds a GPU node whose shared RDMA pool allocatable is the given
	// Quantity string, or omits the resource entirely when rdma is empty.
	aksNode := func(rdma string) v1.Node {
		alloc := v1.ResourceList{v1.ResourceName("nvidia.com/gpu"): resource.MustParse("8")}
		if rdma != "" {
			alloc[v1.ResourceName(aksRdmaSharedResource)] = resource.MustParse(rdma)
		}
		return v1.Node{Status: v1.NodeStatus{Allocatable: alloc}}
	}
	tests := []struct {
		name           string
		nodes          []v1.Node
		wantErr        bool
		wantErrMsg     string
		wantLine       string
		wantMaxMsgSize string
	}{
		{
			// Standard_ND96isr_H100_v5 advertises the shared pool as "1k" (1000);
			// a worker still requests exactly one unit.
			name:           "uniform shared RDMA pool keeps IB message size",
			nodes:          []v1.Node{aksNode("1k"), aksNode("1k")},
			wantLine:       rdmaLine,
			wantMaxMsgSize: maxMessageSize,
		},
		{
			name:           "uniform absent RDMA falls back to TCP",
			nodes:          []v1.Node{aksNode(""), aksNode("")},
			wantLine:       "",
			wantMaxMsgSize: maxMessageSizeTCP,
		},
		{
			name:       "mixed RDMA rollout fails closed",
			nodes:      []v1.Node{aksNode("1k"), aksNode("")},
			wantErr:    true,
			wantErrMsg: "present on 1 of 2 target GPU nodes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &gpuConfiguration{
				WorkerCount:     len(tt.nodes),
				GPUCountPerNode: 8,
				TotalGPUCount:   8 * len(tt.nodes),
				Nodes:           tt.nodes,
			}
			templateData := map[string]string{"MAX_MESSAGE_SIZE": maxMessageSize}
			err := applyAKSTemplateData(config, templateData)
			if (err != nil) != tt.wantErr {
				t.Errorf("applyAKSTemplateData() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				if tt.wantErrMsg != "" && !strings.Contains(err.Error(), tt.wantErrMsg) {
					t.Errorf("error = %q, want substring %q", err.Error(), tt.wantErrMsg)
				}
				return
			}
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
