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
	"testing"

	"github.com/NVIDIA/eidos/pkg/validator/checks"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func TestFindSchedulableGpuNodes(t *testing.T) {
	tests := []struct {
		name      string
		nodes     []v1.Node
		wantCount int
		wantErr   bool
	}{
		{
			name: "finds schedulable GPU nodes",
			nodes: []v1.Node{
				createNode("gpu-node-1", false, true),
				createNode("gpu-node-2", false, true),
			},
			wantCount: 2,
			wantErr:   false,
		},
		{
			name: "skips unschedulable nodes",
			nodes: []v1.Node{
				createNode("gpu-node-1", false, true),
				createNode("gpu-node-2", true, true), // unschedulable
			},
			wantCount: 1,
			wantErr:   false,
		},
		{
			name: "skips nodes without GPU",
			nodes: []v1.Node{
				createNode("gpu-node-1", false, true),
				createNode("cpu-node-1", false, false), // no GPU
			},
			wantCount: 1,
			wantErr:   false,
		},
		{
			name:      "returns empty list when no GPU nodes",
			nodes:     []v1.Node{createNode("cpu-node-1", false, false)},
			wantCount: 0,
			wantErr:   false,
		},
		{
			name:      "handles empty cluster",
			nodes:     []v1.Node{},
			wantCount: 0,
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := make([]runtime.Object, 0, len(tt.nodes))
			for i := range tt.nodes {
				objects = append(objects, &tt.nodes[i])
			}

			//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
			clientset := fake.NewSimpleClientset(objects...)

			ctx := &checks.ValidationContext{
				Context:   context.Background(),
				Clientset: clientset,
			}

			nodes, err := FindSchedulableGpuNodes(ctx)
			if (err != nil) != tt.wantErr {
				t.Errorf("FindSchedulableGpuNodes() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if len(nodes) != tt.wantCount {
				t.Errorf("FindSchedulableGpuNodes() returned %d nodes, want %d", len(nodes), tt.wantCount)
			}
		})
	}
}

func TestIsNodeGpuBusy(t *testing.T) {
	tests := []struct {
		name     string
		nodeName string
		pods     []v1.Pod
		wantBusy bool
		wantErr  bool
	}{
		{
			name:     "node is busy with running GPU pod",
			nodeName: "gpu-node-1",
			pods: []v1.Pod{
				createGpuPod(v1.PodRunning, true),
			},
			wantBusy: true,
			wantErr:  false,
		},
		{
			name:     "node is busy with pending GPU pod",
			nodeName: "gpu-node-1",
			pods: []v1.Pod{
				createGpuPod(v1.PodPending, true),
			},
			wantBusy: true,
			wantErr:  false,
		},
		{
			name:     "node is free - succeeded pod",
			nodeName: "gpu-node-1",
			pods: []v1.Pod{
				createGpuPod(v1.PodSucceeded, true),
			},
			wantBusy: false,
			wantErr:  false,
		},
		{
			name:     "node is free - failed pod",
			nodeName: "gpu-node-1",
			pods: []v1.Pod{
				createGpuPod(v1.PodFailed, true),
			},
			wantBusy: false,
			wantErr:  false,
		},
		{
			name:     "node is free - no GPU requests",
			nodeName: "gpu-node-1",
			pods: []v1.Pod{
				createGpuPod(v1.PodRunning, false),
			},
			wantBusy: false,
			wantErr:  false,
		},
		{
			name:     "node is free - no pods",
			nodeName: "gpu-node-1",
			pods:     []v1.Pod{},
			wantBusy: false,
			wantErr:  false,
		},
		// Note: fake clientset doesn't support field selectors properly, so we can't test
		// filtering by nodeName. That's tested in integration tests.
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := make([]runtime.Object, 0, len(tt.pods))
			for i := range tt.pods {
				objects = append(objects, &tt.pods[i])
			}

			//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
			clientset := fake.NewSimpleClientset(objects...)

			busy, err := IsNodeGpuBusy(context.Background(), clientset, tt.nodeName)
			if (err != nil) != tt.wantErr {
				t.Errorf("IsNodeGpuBusy() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if busy != tt.wantBusy {
				t.Errorf("IsNodeGpuBusy() = %v, want %v", busy, tt.wantBusy)
			}
		})
	}
}

// Helper functions for creating test objects

func createNode(name string, unschedulable bool, hasGPU bool) v1.Node {
	node := v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1.NodeSpec{
			Unschedulable: unschedulable,
		},
		Status: v1.NodeStatus{
			Allocatable: v1.ResourceList{},
		},
	}

	if hasGPU {
		node.Status.Allocatable[v1.ResourceName(GpuResourceName)] = resource.MustParse("1")
	}

	return node
}

func createGpuPod(phase v1.PodPhase, requestGPU bool) v1.Pod {
	nodeName := "gpu-node-1"
	pod := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
		},
		Spec: v1.PodSpec{
			NodeName: nodeName,
			Containers: []v1.Container{
				{
					Name:  "test-container",
					Image: "test:latest",
				},
			},
		},
		Status: v1.PodStatus{
			Phase: phase,
		},
	}

	if requestGPU {
		pod.Spec.Containers[0].Resources = v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceName(GpuResourceName): resource.MustParse("1"),
			},
			Limits: v1.ResourceList{
				v1.ResourceName(GpuResourceName): resource.MustParse("1"),
			},
		}
	}

	return pod
}
