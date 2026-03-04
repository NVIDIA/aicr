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

package topology

import (
	"context"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/measurement"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

const effectNoSchedule = "NoSchedule"

func makeNode(name string, taints []corev1.Taint, labels map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Spec: corev1.NodeSpec{
			Taints: taints,
		},
	}
}

func newFakeCollector(nodes []*corev1.Node, maxNodes int) *Collector {
	objects := make([]runtime.Object, len(nodes))
	for i, n := range nodes {
		objects[i] = n
	}
	return &Collector{
		ClientSet:        fake.NewClientset(objects...),
		MaxNodesPerEntry: maxNodes,
	}
}

func TestCollect(t *testing.T) {
	tests := []struct {
		name             string
		nodes            []*corev1.Node
		maxNodesPerEntry int
		wantNodeCount    int
		wantTaintCount   int
		wantLabelCount   int
		checkFn          func(t *testing.T, m *measurement.Measurement)
	}{
		{
			name:           "empty cluster",
			nodes:          nil,
			wantNodeCount:  0,
			wantTaintCount: 0,
			wantLabelCount: 0,
		},
		{
			name: "single node with taint and labels",
			nodes: []*corev1.Node{
				makeNode("node-1",
					[]corev1.Taint{
						{Key: "nvidia.com/gpu", Effect: corev1.TaintEffectNoSchedule},
					},
					map[string]string{
						"kubernetes.io/arch": "amd64",
						"node-role":          "worker",
					},
				),
			},
			wantNodeCount:  1,
			wantTaintCount: 1,
			wantLabelCount: 2,
			checkFn: func(t *testing.T, m *measurement.Measurement) {
				t.Helper()
				taintSt := m.GetSubtype("taint")
				if taintSt == nil {
					t.Fatal("missing taint subtype")
				}
				if !taintSt.Has("nvidia.com/gpu") {
					t.Error("expected taint key nvidia.com/gpu")
				}

				labelSt := m.GetSubtype("label")
				if labelSt == nil {
					t.Fatal("missing label subtype")
				}
				if !labelSt.Has("kubernetes.io/arch") {
					t.Error("expected label key kubernetes.io/arch")
				}
				if !labelSt.Has("node-role") {
					t.Error("expected label key node-role")
				}
			},
		},
		{
			name: "multi-node shared taints aggregated",
			nodes: []*corev1.Node{
				makeNode("node-a",
					[]corev1.Taint{{Key: "nvidia.com/gpu", Effect: corev1.TaintEffectNoSchedule}},
					nil,
				),
				makeNode("node-b",
					[]corev1.Taint{{Key: "nvidia.com/gpu", Effect: corev1.TaintEffectNoSchedule}},
					nil,
				),
			},
			wantNodeCount:  2,
			wantTaintCount: 1,
			wantLabelCount: 0,
			checkFn: func(t *testing.T, m *measurement.Measurement) {
				t.Helper()
				taintSt := m.GetSubtype("taint")
				if taintSt == nil {
					t.Fatal("missing taint subtype")
				}
				val, err := taintSt.GetString("nvidia.com/gpu")
				if err != nil {
					t.Fatalf("expected taint key nvidia.com/gpu: %v", err)
				}
				parts := strings.SplitN(val, "|", 3)
				if len(parts) != 3 {
					t.Fatalf("expected 3 pipe-separated parts, got %d: %q", len(parts), val)
				}
				nodes := strings.Split(parts[2], ",")
				if len(nodes) != 2 {
					t.Errorf("expected 2 nodes, got %d: %q", len(nodes), parts[2])
				}
				if parts[0] != effectNoSchedule {
					t.Errorf("expected effect NoSchedule, got %q", parts[0])
				}
			},
		},
		{
			name: "same taint key different effects are separate",
			nodes: []*corev1.Node{
				makeNode("node-1",
					[]corev1.Taint{
						{Key: "nvidia.com/gpu", Effect: corev1.TaintEffectNoSchedule},
						{Key: "nvidia.com/gpu", Effect: corev1.TaintEffectNoExecute},
					},
					nil,
				),
			},
			wantNodeCount:  1,
			wantTaintCount: 2,
			checkFn: func(t *testing.T, m *measurement.Measurement) {
				t.Helper()
				taintSt := m.GetSubtype("taint")
				if taintSt == nil {
					t.Fatal("missing taint subtype")
				}
				if !taintSt.Has("nvidia.com/gpu.NoSchedule") {
					t.Error("expected disambiguated taint key nvidia.com/gpu.NoSchedule")
				}
				if !taintSt.Has("nvidia.com/gpu.NoExecute") {
					t.Error("expected disambiguated taint key nvidia.com/gpu.NoExecute")
				}
			},
		},
		{
			name: "taint with value",
			nodes: []*corev1.Node{
				makeNode("node-1",
					[]corev1.Taint{
						{Key: "dedicated", Value: "gpu-workload", Effect: corev1.TaintEffectNoSchedule},
					},
					nil,
				),
			},
			wantNodeCount:  1,
			wantTaintCount: 1,
			checkFn: func(t *testing.T, m *measurement.Measurement) {
				t.Helper()
				taintSt := m.GetSubtype("taint")
				if taintSt == nil {
					t.Fatal("missing taint subtype")
				}
				val, err := taintSt.GetString("dedicated")
				if err != nil {
					t.Fatalf("expected taint key dedicated: %v", err)
				}
				parts := strings.SplitN(val, "|", 3)
				if len(parts) != 3 {
					t.Fatalf("expected 3 parts, got %d: %q", len(parts), val)
				}
				if parts[0] != effectNoSchedule {
					t.Errorf("expected effect NoSchedule, got %q", parts[0])
				}
				if parts[1] != "gpu-workload" {
					t.Errorf("expected value gpu-workload, got %q", parts[1])
				}
			},
		},
		{
			name: "empty taint value",
			nodes: []*corev1.Node{
				makeNode("node-1",
					[]corev1.Taint{
						{Key: "node.kubernetes.io/not-ready", Effect: corev1.TaintEffectNoSchedule},
					},
					nil,
				),
			},
			wantNodeCount:  1,
			wantTaintCount: 1,
			checkFn: func(t *testing.T, m *measurement.Measurement) {
				t.Helper()
				taintSt := m.GetSubtype("taint")
				if taintSt == nil {
					t.Fatal("missing taint subtype")
				}
				val, err := taintSt.GetString("node.kubernetes.io/not-ready")
				if err != nil {
					t.Fatalf("expected taint key: %v", err)
				}
				parts := strings.SplitN(val, "|", 3)
				if len(parts) != 3 {
					t.Fatalf("expected 3 parts, got %d: %q", len(parts), val)
				}
				if parts[1] != "" {
					t.Errorf("expected empty value between pipes, got %q", parts[1])
				}
			},
		},
		{
			name: "max nodes per entry truncation",
			nodes: []*corev1.Node{
				makeNode("node-a",
					[]corev1.Taint{{Key: "gpu", Effect: corev1.TaintEffectNoSchedule}},
					nil,
				),
				makeNode("node-b",
					[]corev1.Taint{{Key: "gpu", Effect: corev1.TaintEffectNoSchedule}},
					nil,
				),
				makeNode("node-c",
					[]corev1.Taint{{Key: "gpu", Effect: corev1.TaintEffectNoSchedule}},
					nil,
				),
			},
			maxNodesPerEntry: 2,
			wantNodeCount:    3,
			wantTaintCount:   1,
			checkFn: func(t *testing.T, m *measurement.Measurement) {
				t.Helper()
				taintSt := m.GetSubtype("taint")
				if taintSt == nil {
					t.Fatal("missing taint subtype")
				}
				val, err := taintSt.GetString("gpu")
				if err != nil {
					t.Fatalf("expected taint key gpu: %v", err)
				}
				if !strings.Contains(val, "(+1 more)") {
					t.Errorf("expected truncation marker (+1 more), got %q", val)
				}
			},
		},
		{
			name: "labels with empty value",
			nodes: []*corev1.Node{
				makeNode("node-1", nil,
					map[string]string{
						"node-role.kubernetes.io/control-plane": "",
					},
				),
			},
			wantNodeCount:  1,
			wantTaintCount: 0,
			wantLabelCount: 1,
			checkFn: func(t *testing.T, m *measurement.Measurement) {
				t.Helper()
				labelSt := m.GetSubtype("label")
				if labelSt == nil {
					t.Fatal("missing label subtype")
				}
				val, err := labelSt.GetString("node-role.kubernetes.io/control-plane")
				if err != nil {
					t.Fatalf("expected label key: %v", err)
				}
				parts := strings.SplitN(val, "|", 2)
				if len(parts) != 2 {
					t.Fatalf("expected 2 parts, got %d: %q", len(parts), val)
				}
				if parts[0] != "" {
					t.Errorf("expected empty value between pipes, got %q", parts[0])
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newFakeCollector(tt.nodes, tt.maxNodesPerEntry)

			m, err := c.Collect(context.Background())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if m.Type != measurement.TypeNodeTopology {
				t.Errorf("expected type %s, got %s", measurement.TypeNodeTopology, m.Type)
			}

			summarySt := m.GetSubtype("summary")
			if summarySt == nil {
				t.Fatal("missing summary subtype")
			}

			nodeCount, err := summarySt.GetInt64("node-count")
			if err != nil {
				t.Fatalf("missing node-count: %v", err)
			}
			if int(nodeCount) != tt.wantNodeCount {
				t.Errorf("node-count = %d, want %d", nodeCount, tt.wantNodeCount)
			}

			taintCount, err := summarySt.GetInt64("taint-count")
			if err != nil {
				t.Fatalf("missing taint-count: %v", err)
			}
			if int(taintCount) != tt.wantTaintCount {
				t.Errorf("taint-count = %d, want %d", taintCount, tt.wantTaintCount)
			}

			if tt.wantLabelCount > 0 {
				labelCount, err := summarySt.GetInt64("label-count")
				if err != nil {
					t.Fatalf("missing label-count: %v", err)
				}
				if int(labelCount) != tt.wantLabelCount {
					t.Errorf("label-count = %d, want %d", labelCount, tt.wantLabelCount)
				}
			}

			if tt.checkFn != nil {
				tt.checkFn(t, m)
			}
		})
	}
}

func TestCollectContextCanceled(t *testing.T) {
	c := newFakeCollector(nil, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Collect(ctx)
	if err == nil {
		t.Fatal("expected error for canceled context")
	}
}

func TestTaintEncoding(t *testing.T) {
	node := makeNode("worker-1",
		[]corev1.Taint{
			{Key: "nvidia.com/gpu", Value: "present", Effect: corev1.TaintEffectNoSchedule},
		},
		nil,
	)
	c := newFakeCollector([]*corev1.Node{node}, 0)

	m, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	taintSt := m.GetSubtype("taint")
	if taintSt == nil {
		t.Fatal("missing taint subtype")
	}

	val, err := taintSt.GetString("nvidia.com/gpu")
	if err != nil {
		t.Fatalf("missing taint key: %v", err)
	}

	// Format: effect|value|node1,node2,...
	parts := strings.SplitN(val, "|", 3)
	if len(parts) != 3 {
		t.Fatalf("expected 3 pipe-separated parts, got %d: %q", len(parts), val)
	}
	if parts[0] != effectNoSchedule {
		t.Errorf("effect = %q, want NoSchedule", parts[0])
	}
	if parts[1] != "present" {
		t.Errorf("value = %q, want present", parts[1])
	}
	if parts[2] != "worker-1" {
		t.Errorf("nodes = %q, want worker-1", parts[2])
	}
}

func TestLabelEncoding(t *testing.T) {
	node := makeNode("worker-1", nil,
		map[string]string{
			"kubernetes.io/arch": "amd64",
		},
	)
	c := newFakeCollector([]*corev1.Node{node}, 0)

	m, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	labelSt := m.GetSubtype("label")
	if labelSt == nil {
		t.Fatal("missing label subtype")
	}

	val, err := labelSt.GetString("kubernetes.io/arch")
	if err != nil {
		t.Fatalf("missing label key: %v", err)
	}

	// Format: value|node1,node2,...
	parts := strings.SplitN(val, "|", 2)
	if len(parts) != 2 {
		t.Fatalf("expected 2 pipe-separated parts, got %d: %q", len(parts), val)
	}
	if parts[0] != "amd64" {
		t.Errorf("value = %q, want amd64", parts[0])
	}
	if parts[1] != "worker-1" {
		t.Errorf("nodes = %q, want worker-1", parts[1])
	}
}
