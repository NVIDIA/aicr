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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// nodeWithGPUs builds a node advertising `gpu` allocatable nvidia.com/gpu.
func nodeWithGPUs(name string, gpu int64) v1.Node {
	return v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: v1.NodeStatus{Allocatable: v1.ResourceList{
			v1.ResourceName("nvidia.com/gpu"): *resource.NewQuantity(gpu, resource.DecimalSI),
		}},
	}
}

func TestUniformGPUCountPerNode(t *testing.T) {
	tests := []struct {
		name        string
		nodes       []v1.Node
		wantCount   int
		wantErr     bool
		wantErrSubs []string // ALL must appear in the error (every degraded node named)
	}{
		{
			name:      "uniform full nodes",
			nodes:     []v1.Node{nodeWithGPUs("gpu-0", 8), nodeWithGPUs("gpu-1", 8)},
			wantCount: 8,
		},
		{
			name:      "single node",
			nodes:     []v1.Node{nodeWithGPUs("gpu-0", 8)},
			wantCount: 8,
		},
		{
			name:        "one node short of its peers fails fast, naming it",
			nodes:       []v1.Node{nodeWithGPUs("gpu-0", 8), nodeWithGPUs("gpu-1", 7)},
			wantErr:     true,
			wantErrSubs: []string{"gpu-1=7/8"},
		},
		{
			// The degraded node appears BEFORE the full node — the case that
			// distinguishes "expected = max of peers" from a reverted
			// "expected = first node's count" (which would size to 7 and pass).
			name:        "degraded node first, full node second — still fails, naming the short node",
			nodes:       []v1.Node{nodeWithGPUs("gpu-0", 7), nodeWithGPUs("gpu-1", 8)},
			wantErr:     true,
			wantErrSubs: []string{"gpu-0=7/8"},
		},
		{
			name:        "multiple degraded nodes are all named",
			nodes:       []v1.Node{nodeWithGPUs("gpu-0", 8), nodeWithGPUs("gpu-1", 7), nodeWithGPUs("gpu-2", 6)},
			wantErr:     true,
			wantErrSubs: []string{"gpu-1=7/8", "gpu-2=6/8"},
		},
		{
			name:      "uniformly low count is coherent — not flagged",
			nodes:     []v1.Node{nodeWithGPUs("gpu-0", 7), nodeWithGPUs("gpu-1", 7)},
			wantCount: 7,
		},
		{
			name:        "no nodes",
			nodes:       nil,
			wantErr:     true,
			wantErrSubs: []string{"no target GPU nodes"},
		},
		{
			name:        "zero GPUs on all nodes",
			nodes:       []v1.Node{nodeWithGPUs("gpu-0", 0)},
			wantErr:     true,
			wantErrSubs: []string{"no GPUs found"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := uniformGPUCountPerNode(tt.nodes)
			if (err != nil) != tt.wantErr {
				t.Fatalf("uniformGPUCountPerNode() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				for _, sub := range tt.wantErrSubs {
					if !strings.Contains(err.Error(), sub) {
						t.Fatalf("error = %q, want substring %q", err.Error(), sub)
					}
				}
				return
			}
			if got != tt.wantCount {
				t.Fatalf("uniformGPUCountPerNode() = %d, want %d", got, tt.wantCount)
			}
		})
	}
}
