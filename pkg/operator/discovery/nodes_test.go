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

package discovery

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

var placement = &v1alpha1.Placement{
	System:      v1alpha1.NodeGroupPlacement{NodeSelector: map[string]v1alpha1.LabelValue{"pool": "system"}},
	Accelerated: v1alpha1.NodeGroupPlacement{NodeSelector: map[string]v1alpha1.LabelValue{"pool": "gpu", "tier": "h100"}},
}

func node(name, pool, product string) corev1.Node {
	labels := map[string]string{"pool": pool}
	if pool == "gpu" {
		labels["tier"] = "h100"
	}
	if product != "" {
		labels[GPUProductLabel] = product
	}
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{
			Architecture: "amd64", OSImage: "Ubuntu 24.04.3 LTS", KernelVersion: "6.8.0-1031-aws",
			ContainerRuntimeVersion: "containerd://2.1.4", KubeletVersion: "v1.34.2",
		}},
	}
}

func cluster() []corev1.Node {
	return []corev1.Node{
		node("sys-a", "system", ""), node("sys-b", "system", ""),
		node("gpu-a", "gpu", "NVIDIA-H100-80GB-HBM3"), node("gpu-b", "gpu", "NVIDIA-H100-80GB-HBM3"),
		node("other", "cpu", ""),
	}
}

// Node order and heartbeats change nothing; another node with a known
// tuple changes no tuple digest.
func TestObserveIsOrderIndependent(t *testing.T) {
	t.Parallel()
	nodes := cluster()
	want := Observe(nodes, placement)
	if err := want.Check(); err != nil {
		t.Fatalf("Check() = %v", err)
	}
	if !reflect.DeepEqual(want.System, []string{"sys-a", "sys-b"}) || !reflect.DeepEqual(want.Accelerated, []string{"gpu-a", "gpu-b"}) || len(want.Tuples) != 1 {
		t.Fatalf("Observe() = %+v", want)
	}
	for i := range 20 {
		shuffled := cluster()
		rand.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		for k := range shuffled {
			shuffled[k].Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue,
				LastHeartbeatTime: metav1.NewTime(time.Unix(int64(i*100+k), 0))}}
		}
		if got := Observe(shuffled, placement); !reflect.DeepEqual(got, want) || got.TupleDigest() != want.TupleDigest() {
			t.Fatalf("shuffle %d: Observe() = %+v", i, got)
		}
	}
	more := append(cluster(), node("gpu-c", "gpu", "NVIDIA-H100-80GB-HBM3"))
	if got := Observe(more, placement); got.TupleDigest() != want.TupleDigest() || len(got.Accelerated) != 3 {
		t.Fatal("a homogeneous node changed the tuple digest")
	}
	upgraded := append(cluster(), node("gpu-c", "gpu", "NVIDIA-H100-80GB-HBM3"))
	upgraded[5].Status.NodeInfo.KubeletVersion = "v1.35.0"
	if Observe(upgraded, placement).TupleDigest() == want.TupleDigest() {
		t.Fatal("a new kubelet version did not change the tuple digest")
	}
}

// While GPU Operator restarts containerd to install its toolkit, kubelet
// reports the runtime as containerd://Unknown. That observes nothing: the
// node stays in the group, but it neither makes the group heterogeneous nor
// changes the tuple digest, so it stalls nothing and takes no snapshot. A
// runtime version that really differs still blocks.
func TestUnknownRuntimeIsNotObserved(t *testing.T) {
	t.Parallel()
	want := Observe(cluster(), placement)
	restarting := cluster()
	restarting[3].Status.NodeInfo.ContainerRuntimeVersion = "containerd://Unknown"
	got := Observe(restarting, placement)
	if err := got.Check(); err != nil {
		t.Fatalf("Check() = %v", err)
	}
	if got.TupleDigest() != want.TupleDigest() || !reflect.DeepEqual(got.Accelerated, want.Accelerated) {
		t.Fatalf("restarting runtime: tuples %+v, accelerated %v; want %+v, %v", got.Tuples, got.Accelerated, want.Tuples, want.Accelerated)
	}
	restarting[3].Status.NodeInfo.ContainerRuntimeVersion = "containerd://2.2.7"
	if err := Observe(restarting, placement).Check(); operrors.ReasonOf(err) != ReasonHeterogeneous ||
		!strings.Contains(err.Error(), "containerRuntime (containerd://2.1.4, containerd://2.2.7)") {

		t.Fatalf("a different runtime version: Check() = %v", err)
	}
}

// Unsafe observations block with specific reasons.
func TestCheckReasons(t *testing.T) {
	t.Parallel()
	mutate := func(f func([]corev1.Node) []corev1.Node) []corev1.Node { return f(cluster()) }
	tests := []struct {
		name   string
		nodes  []corev1.Node
		reason string
		text   string
	}{
		{"no accelerated nodes", mutate(func(n []corev1.Node) []corev1.Node { return n[:2] }), ReasonGroupEmpty, "accelerated"},
		{"no system nodes", mutate(func(n []corev1.Node) []corev1.Node { return n[2:4] }), ReasonGroupEmpty, "system placement"},
		{"mixed GPU products", mutate(func(n []corev1.Node) []corev1.Node {
			n[3].Labels[GPUProductLabel] = "NVIDIA-A100-SXM4-80GB"
			return n
		}), ReasonHeterogeneous, "product (NVIDIA-A100-SXM4-80GB, NVIDIA-H100-80GB-HBM3)"},
		{"mixed kernels", mutate(func(n []corev1.Node) []corev1.Node {
			n[3].Status.NodeInfo.KernelVersion = "6.8.0-1032-aws"
			return n
		}), ReasonHeterogeneous, "kernelVersion"},
		{"unlabeled node next to a labeled one", mutate(func(n []corev1.Node) []corev1.Node {
			delete(n[3].Labels, GPUProductLabel)
			return n
		}), "", ""},
	}
	for _, tt := range tests {
		err := Observe(tt.nodes, placement).Check()
		if tt.reason == "" {
			if err != nil {
				t.Errorf("%s: Check() = %v", tt.name, err)
			}
			continue
		}
		if operrors.ReasonOf(err) != tt.reason || !strings.Contains(err.Error(), tt.text) {
			t.Errorf("%s: Check() = %v, want %s with %q", tt.name, err, tt.reason, tt.text)
		}
	}

	overlap := map[string]v1alpha1.LabelValue{"pool": "gpu"}
	p := &v1alpha1.Placement{System: v1alpha1.NodeGroupPlacement{NodeSelector: overlap}, Accelerated: placement.Accelerated}
	if err := Observe(cluster(), p).Check(); operrors.ReasonOf(err) != ReasonPlacementOverlap || !strings.Contains(err.Error(), "gpu-a, gpu-b") {
		t.Errorf("overlapping selectors: Check() = %v", err)
	}
	many := make([]corev1.Node, 0, 8)
	for i := range 8 {
		many = append(many, node(fmt.Sprintf("n%d", i), "gpu", ""))
	}
	p = &v1alpha1.Placement{System: v1alpha1.NodeGroupPlacement{NodeSelector: overlap}, Accelerated: placement.Accelerated}
	if err := Observe(many, p).Check(); !strings.Contains(err.Error(), "and 3 more") {
		t.Errorf("long overlap list: %v", err)
	}
}

func TestTransformAndRelevance(t *testing.T) {
	t.Parallel()
	n := node("gpu-a", "gpu", "NVIDIA-H100-80GB-HBM3")
	n.Annotations = map[string]string{"big": strings.Repeat("x", 1000)}
	n.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubelet"}}
	n.Status.Images = []corev1.ContainerImage{{Names: []string{"a"}}}
	n.Status.Conditions = []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionTrue, LastHeartbeatTime: metav1.NewTime(time.Unix(100, 0))},
		{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse},
	}
	out, err := TransformNode(n.DeepCopy())
	if err != nil {
		t.Fatal(err)
	}
	stripped := out.(*corev1.Node)
	if stripped.Annotations != nil || stripped.ManagedFields != nil || stripped.Status.Images != nil ||
		len(stripped.Status.Conditions) != 1 || stripped.Labels[GPUProductLabel] == "" {

		t.Fatalf("TransformNode() = %+v", stripped)
	}
	if other, _ := TransformNode("not a node"); other != "not a node" {
		t.Error("TransformNode changed a non-Node")
	}

	beat := n.DeepCopy()
	beat.Status.Conditions[0].LastHeartbeatTime = metav1.NewTime(time.Unix(200, 0))
	if Relevant(&n, beat) {
		t.Error("a heartbeat is relevant")
	}
	for name, f := range map[string]func(*corev1.Node){
		"label":     func(m *corev1.Node) { m.Labels["new"] = "x" },
		"taint":     func(m *corev1.Node) { m.Spec.Taints = []corev1.Taint{{Key: "k", Effect: corev1.TaintEffectNoSchedule}} },
		"kubelet":   func(m *corev1.Node) { m.Status.NodeInfo.KubeletVersion = "v1.35.0" },
		"not ready": func(m *corev1.Node) { m.Status.Conditions[0].Status = corev1.ConditionFalse },
	} {
		m := n.DeepCopy()
		f(m)
		if !Relevant(&n, m) {
			t.Errorf("%s change is not relevant", name)
		}
	}
}
