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

package talos

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/NVIDIA/aicr/pkg/measurement"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// TestNewCollectors_SharedConfigFetchesNodeOnce verifies that when the
// service and OS collectors are constructed via NewCollectors, they share
// a single config and perform exactly one Node API call between them
// regardless of invocation order. This is what the factory relies on to
// keep one Talos snapshot to one round-trip.
func TestNewCollectors_SharedConfigFetchesNodeOnce(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "talos-node-0"},
		Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{
			ContainerRuntimeVersion: "containerd://1.7.20",
			KubeletVersion:          "v1.32.4",
			OSImage:                 "Talos (v1.7.6)",
		}},
	}
	cs := fake.NewSimpleClientset(node)

	var gets atomic.Int32
	cs.PrependReactor("get", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		gets.Add(1)
		// Returning false lets the default tracker handle the call.
		return false, nil, nil
	})

	svc, osCol := NewCollectors(WithClientSet(cs), WithNodeName("talos-node-0"))

	if _, err := svc.Collect(context.Background()); err != nil {
		t.Fatalf("ServiceCollector.Collect() error = %v", err)
	}
	if _, err := osCol.Collect(context.Background()); err != nil {
		t.Fatalf("OSCollector.Collect() error = %v", err)
	}

	if got := gets.Load(); got != 1 {
		t.Errorf("expected exactly 1 Node Get across both collectors, got %d", got)
	}
}

func TestServiceCollector_PopulatesFromNodeInfo(t *testing.T) {
	tests := []struct {
		name             string
		nodeInfo         corev1.NodeSystemInfo
		wantContainerd   map[string]string
		wantKubelet      map[string]string
		wantSubtypeCount int
	}{
		{
			name: "talos node with containerd and kubelet",
			nodeInfo: corev1.NodeSystemInfo{
				ContainerRuntimeVersion: "containerd://1.7.20",
				KubeletVersion:          "v1.32.4",
				OSImage:                 "Talos (v1.7.6)",
				KernelVersion:           "6.6.32-talos",
			},
			wantContainerd: map[string]string{
				keyActiveState: "active",
				keyRuntimeName: "containerd",
				keyVersion:     "1.7.20",
				keySource:      sourceTalosNodeInfo,
			},
			wantKubelet: map[string]string{
				keyActiveState: "active",
				keyVersion:     "v1.32.4",
				keySource:      sourceTalosNodeInfo,
			},
			wantSubtypeCount: 2,
		},
		{
			name: "node with empty container runtime version",
			nodeInfo: corev1.NodeSystemInfo{
				ContainerRuntimeVersion: "",
				KubeletVersion:          "v1.30.0",
			},
			wantContainerd: map[string]string{
				keyActiveState: "unknown",
				keySource:      sourceTalosNodeInfo,
			},
			wantKubelet: map[string]string{
				keyActiveState: "active",
				keyVersion:     "v1.30.0",
				keySource:      sourceTalosNodeInfo,
			},
			wantSubtypeCount: 2,
		},
		{
			name: "node with malformed runtime id (no scheme separator) is unknown",
			nodeInfo: corev1.NodeSystemInfo{
				ContainerRuntimeVersion: "containerd-1.7.20",
				KubeletVersion:          "v1.30.0",
			},
			wantContainerd: map[string]string{
				keyActiveState: "unknown",
				keyRuntimeName: "containerd-1.7.20",
				keySource:      sourceTalosNodeInfo,
			},
			wantKubelet: map[string]string{
				keyActiveState: "active",
				keyVersion:     "v1.30.0",
				keySource:      sourceTalosNodeInfo,
			},
			wantSubtypeCount: 2,
		},
		{
			name: "non-containerd runtime (cri-o) is unknown for the containerd subtype",
			nodeInfo: corev1.NodeSystemInfo{
				ContainerRuntimeVersion: "cri-o://1.30.0",
				KubeletVersion:          "v1.30.0",
			},
			wantContainerd: map[string]string{
				keyActiveState: "unknown",
				keyRuntimeName: "cri-o",
				keySource:      sourceTalosNodeInfo,
			},
			wantKubelet: map[string]string{
				keyActiveState: "active",
				keyVersion:     "v1.30.0",
				keySource:      sourceTalosNodeInfo,
			},
			wantSubtypeCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "talos-node-0"},
				Status:     corev1.NodeStatus{NodeInfo: tt.nodeInfo},
			}
			cs := fake.NewSimpleClientset(node)

			c := NewServiceCollector(WithClientSet(cs), WithNodeName("talos-node-0"))
			m, err := c.Collect(context.Background())
			if err != nil {
				t.Fatalf("Collect() error = %v", err)
			}
			if m.Type != measurement.TypeSystemD {
				t.Errorf("Type = %q, want %q", m.Type, measurement.TypeSystemD)
			}
			if len(m.Subtypes) != tt.wantSubtypeCount {
				t.Fatalf("len(Subtypes) = %d, want %d", len(m.Subtypes), tt.wantSubtypeCount)
			}

			containerd := m.GetSubtype(SubtypeContainerd)
			if containerd == nil {
				t.Fatalf("missing %q subtype", SubtypeContainerd)
			}
			assertSubtypeData(t, containerd, tt.wantContainerd)

			kubelet := m.GetSubtype(SubtypeKubelet)
			if kubelet == nil {
				t.Fatalf("missing %q subtype", SubtypeKubelet)
			}
			assertSubtypeData(t, kubelet, tt.wantKubelet)
		})
	}
}

func TestServiceCollector_GracefulDegradation(t *testing.T) {
	tests := []struct {
		name      string
		clientSet *fake.Clientset
		nodeName  string
	}{
		{
			name:      "node not found returns empty measurement",
			clientSet: fake.NewSimpleClientset(),
			nodeName:  "missing-node",
		},
		{
			name:      "no node name configured returns empty measurement",
			clientSet: fake.NewSimpleClientset(),
			nodeName:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("NODE_NAME", "")
			t.Setenv("KUBERNETES_NODE_NAME", "")
			t.Setenv("HOSTNAME", "")

			c := NewServiceCollector(WithClientSet(tt.clientSet), WithNodeName(tt.nodeName))
			m, err := c.Collect(context.Background())
			if err != nil {
				t.Fatalf("Collect() error = %v, want nil (graceful)", err)
			}
			if m.Type != measurement.TypeSystemD {
				t.Errorf("Type = %q, want %q", m.Type, measurement.TypeSystemD)
			}
			if len(m.Subtypes) != 0 {
				t.Errorf("len(Subtypes) = %d, want 0 (empty for graceful degradation)", len(m.Subtypes))
			}
		})
	}
}

func TestSplitRuntimeID(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantName    string
		wantVersion string
	}{
		{"containerd", "containerd://1.7.20", "containerd", "1.7.20"},
		{"crio", "cri-o://1.30.0", "cri-o", "1.30.0"},
		{"no separator", "containerd-1.7.20", "containerd-1.7.20", ""},
		{"empty", "", "", ""},
		{"separator only", "://", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, gotVersion := splitRuntimeID(tt.input)
			if gotName != tt.wantName {
				t.Errorf("name = %q, want %q", gotName, tt.wantName)
			}
			if gotVersion != tt.wantVersion {
				t.Errorf("version = %q, want %q", gotVersion, tt.wantVersion)
			}
		})
	}
}

func assertSubtypeData(t *testing.T, st *measurement.Subtype, want map[string]string) {
	t.Helper()
	for k, v := range want {
		got, err := st.GetString(k)
		if err != nil {
			t.Errorf("GetString(%q) error = %v", k, err)
			continue
		}
		if got != v {
			t.Errorf("subtype %q key %q = %q, want %q", st.Name, k, got, v)
		}
	}
}
