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

// eksNode builds an EKS GPU node with the given instance type and EFA count.
// A negative efa omits the EFA resource entirely (device plugin absent).
func eksNode(name, instanceType string, efa int) v1.Node {
	labels := map[string]string{}
	if instanceType != "" {
		labels["node.kubernetes.io/instance-type"] = instanceType
	}
	alloc := v1.ResourceList{
		v1.ResourceName("nvidia.com/gpu"): resource.MustParse("8"),
	}
	if efa >= 0 {
		alloc[efaResourceName] = *resource.NewQuantity(int64(efa), resource.DecimalSI)
	}
	return v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status:     v1.NodeStatus{Allocatable: alloc},
	}
}

func TestDiscoverEKSNodeConfig(t *testing.T) {
	tests := []struct {
		name         string
		nodes        []v1.Node
		wantInstance string
		wantEFA      int
		wantErr      bool
		wantErrMsg   string
	}{
		{
			name: "uniform p5.48xlarge with 32 EFA",
			nodes: []v1.Node{
				eksNode("n1", "p5.48xlarge", 32),
				eksNode("n2", "p5.48xlarge", 32),
			},
			wantInstance: "p5.48xlarge",
			wantEFA:      32,
		},
		{
			name:         "single node p4d.24xlarge with 4 EFA",
			nodes:        []v1.Node{eksNode("n1", "p4d.24xlarge", 4)},
			wantInstance: "p4d.24xlarge",
			wantEFA:      4,
		},
		{
			name:       "missing instance type label",
			nodes:      []v1.Node{eksNode("n1", "", 32)},
			wantErr:    true,
			wantErrMsg: "missing node.kubernetes.io/instance-type label",
		},
		{
			name: "uniform zero EFA (falls back to TCP)",
			nodes: []v1.Node{
				eksNode("n1", "p5.48xlarge", 0),
				eksNode("n2", "p5.48xlarge", -1),
			},
			wantInstance: "p5.48xlarge",
			wantEFA:      0,
		},
		{
			name: "mixed EFA counts fail closed",
			nodes: []v1.Node{
				eksNode("n1", "p5.48xlarge", 32),
				eksNode("n2", "p5.48xlarge", 0),
			},
			wantErr:    true,
			wantErrMsg: "present on 1 of 2 target GPU nodes",
		},
		{
			name:       "no target nodes",
			nodes:      []v1.Node{},
			wantErr:    true,
			wantErrMsg: "no target GPU nodes for EKS discovery",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instanceType, efaCount, err := discoverEKSNodeConfig(tt.nodes)
			if (err != nil) != tt.wantErr {
				t.Errorf("discoverEKSNodeConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				if tt.wantErrMsg != "" && !strings.Contains(err.Error(), tt.wantErrMsg) {
					t.Errorf("error = %q, want substring %q", err.Error(), tt.wantErrMsg)
				}
				return
			}
			if instanceType != tt.wantInstance {
				t.Errorf("instanceType = %q, want %q", instanceType, tt.wantInstance)
			}
			if efaCount != tt.wantEFA {
				t.Errorf("efaCount = %d, want %d", efaCount, tt.wantEFA)
			}
		})
	}
}

func TestUniformFabricResourceCount(t *testing.T) {
	const res = v1.ResourceName("example.com/fabric")
	node := func(count int) v1.Node {
		alloc := v1.ResourceList{}
		if count >= 0 {
			alloc[res] = *resource.NewQuantity(int64(count), resource.DecimalSI)
		}
		return v1.Node{Status: v1.NodeStatus{Allocatable: alloc}}
	}
	tests := []struct {
		name       string
		nodes      []v1.Node
		want       int
		wantErr    bool
		wantErrMsg string
	}{
		{
			name:  "uniform nonzero",
			nodes: []v1.Node{node(32), node(32)},
			want:  32,
		},
		{
			name:  "uniform zero (resource present, value 0)",
			nodes: []v1.Node{node(0), node(0)},
			want:  0,
		},
		{
			name:  "uniform absent (resource missing)",
			nodes: []v1.Node{node(-1), node(-1)},
			want:  0,
		},
		{
			name:       "mixed present and absent",
			nodes:      []v1.Node{node(32), node(-1)},
			wantErr:    true,
			wantErrMsg: "present on 1 of 2 target GPU nodes",
		},
		{
			name:       "mixed nonzero counts",
			nodes:      []v1.Node{node(32), node(16), node(32)},
			wantErr:    true,
			wantErrMsg: "present on 3 of 3 target GPU nodes",
		},
		{
			name:       "no nodes",
			nodes:      []v1.Node{},
			wantErr:    true,
			wantErrMsg: "no target GPU nodes to inspect",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := uniformFabricResourceCount(tt.nodes, res)
			if (err != nil) != tt.wantErr {
				t.Errorf("uniformFabricResourceCount() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				if tt.wantErrMsg != "" && !strings.Contains(err.Error(), tt.wantErrMsg) {
					t.Errorf("error = %q, want substring %q", err.Error(), tt.wantErrMsg)
				}
				return
			}
			if got != tt.want {
				t.Errorf("uniformFabricResourceCount() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestWarnIfHeterogeneousNodes(t *testing.T) {
	tests := []struct {
		name  string
		nodes []v1.Node
	}{
		{
			name: "homogeneous nodes",
			nodes: []v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{"node.kubernetes.io/instance-type": "p5.48xlarge"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "n2", Labels: map[string]string{"node.kubernetes.io/instance-type": "p5.48xlarge"}}},
			},
		},
		{
			name: "heterogeneous nodes",
			nodes: []v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{"node.kubernetes.io/instance-type": "p5.48xlarge"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "n2", Labels: map[string]string{"node.kubernetes.io/instance-type": "p4d.24xlarge"}}},
			},
		},
		{
			name: "single node",
			nodes: []v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{"node.kubernetes.io/instance-type": "p5.48xlarge"}}},
			},
		},
		{
			name:  "empty",
			nodes: []v1.Node{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// warnIfHeterogeneousNodes should never panic.
			warnIfHeterogeneousNodes(tt.nodes)
		})
	}
}

func TestBuildEFAResourceLine(t *testing.T) {
	tests := []struct {
		name     string
		efaCount int
		indent   string
		want     string
	}{
		{
			name:     "32 EFA adapters",
			efaCount: 32,
			indent:   "                      ",
			want:     `                      vpc.amazonaws.com/efa: "32"`,
		},
		{
			name:     "4 EFA adapters",
			efaCount: 4,
			indent:   "                      ",
			want:     `                      vpc.amazonaws.com/efa: "4"`,
		},
		{
			name:     "no EFA — empty string",
			efaCount: 0,
			indent:   "                      ",
			want:     "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildEFAResourceLine(tt.efaCount, tt.indent)
			if got != tt.want {
				t.Errorf("buildEFAResourceLine(%d) = %q, want %q", tt.efaCount, got, tt.want)
			}
		})
	}
}
