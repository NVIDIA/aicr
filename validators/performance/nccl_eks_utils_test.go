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
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDiscoverEKSNodeConfig(t *testing.T) {
	tests := []struct {
		name         string
		node         v1.Node
		wantInstance string
		wantEFA      int
		wantErr      bool
		wantErrMsg   string
	}{
		{
			name: "p5.48xlarge with 32 EFA",
			node: v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"node.kubernetes.io/instance-type": "p5.48xlarge",
					},
				},
				Status: v1.NodeStatus{
					Allocatable: v1.ResourceList{
						v1.ResourceName("nvidia.com/gpu"):        resource.MustParse("8"),
						v1.ResourceName("vpc.amazonaws.com/efa"): resource.MustParse("32"),
					},
				},
			},
			wantInstance: "p5.48xlarge",
			wantEFA:      32,
		},
		{
			name: "p4d.24xlarge with 4 EFA",
			node: v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"node.kubernetes.io/instance-type": "p4d.24xlarge",
					},
				},
				Status: v1.NodeStatus{
					Allocatable: v1.ResourceList{
						v1.ResourceName("nvidia.com/gpu"):        resource.MustParse("8"),
						v1.ResourceName("vpc.amazonaws.com/efa"): resource.MustParse("4"),
					},
				},
			},
			wantInstance: "p4d.24xlarge",
			wantEFA:      4,
		},
		{
			name: "missing instance type label",
			node: v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{},
				},
				Status: v1.NodeStatus{
					Allocatable: v1.ResourceList{
						v1.ResourceName("vpc.amazonaws.com/efa"): resource.MustParse("32"),
					},
				},
			},
			wantErr:    true,
			wantErrMsg: "missing node.kubernetes.io/instance-type label",
		},
		{
			name: "no EFA adapters (falls back to TCP)",
			node: v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"node.kubernetes.io/instance-type": "p5.48xlarge",
					},
				},
				Status: v1.NodeStatus{
					Allocatable: v1.ResourceList{
						v1.ResourceName("nvidia.com/gpu"): resource.MustParse("8"),
					},
				},
			},
			wantInstance: "p5.48xlarge",
			wantEFA:      0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instanceType, efaCount, err := discoverEKSNodeConfig(tt.node)
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
