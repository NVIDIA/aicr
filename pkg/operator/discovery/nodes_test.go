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
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/randfill"

	v1alpha1 "aicr.run/api/operator/v1alpha1"
	operrors "aicr.run/pkg/operator/errors"
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
	n.UID, n.ResourceVersion = "u-gpu-a", "42"
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
	// The informer reads an update whose resource version is unchanged as a
	// resync, which reaches no handler.
	if stripped.UID != n.UID || stripped.ResourceVersion != n.ResourceVersion {
		t.Errorf("TransformNode() kept UID %q and resource version %q", stripped.UID, stripped.ResourceVersion)
	}
	if stripped.Annotations != nil || stripped.ManagedFields != nil || stripped.Status.Images != nil ||
		stripped.Status.Conditions != nil || stripped.Labels[GPUProductLabel] == "" {

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
	// Each decoded Node holds its own swap status pointer.
	swap := n.DeepCopy()
	swap.Status.NodeInfo.Swap = &corev1.NodeSwapStatus{Capacity: ptr.To[int64](8 << 30)}
	if Relevant(swap, swap.DeepCopy()) {
		t.Error("an equal swap status behind another pointer is relevant")
	}
	for name, f := range map[string]func(*corev1.Node){
		"label":   func(m *corev1.Node) { m.Labels["new"] = "x" },
		"product": func(m *corev1.Node) { m.Labels[GPUProductLabel] = "NVIDIA-B200" },
		"kubelet": func(m *corev1.Node) { m.Status.NodeInfo.KubeletVersion = "v1.35.0" },
	} {
		m := n.DeepCopy()
		f(m)
		if !Relevant(&n, m) {
			t.Errorf("%s change is not relevant", name)
		}
	}
}

// Discovery reads only what the Node cache keeps: a Node's name, labels and
// the node info fields of its tuple. Two Nodes that agree on those observe
// into the same groups, cache as the same object, and an update from one to
// the other is not relevant. The other fields are set at random, and to the
// values discovery would most plausibly read: readiness, taints, cordoning,
// provider ID, GPU capacity and deletion, each as an unhealthy node has it.
func TestDiscoveryReadsOnlyWhatTheCacheKeeps(t *testing.T) {
	t.Parallel()
	fill := randfill.NewWithSeed(3502).NilChance(0.2).NumElements(1, 3)
	healthy := func() corev1.Node {
		n := node("gpu-a", "gpu", "NVIDIA-H100-80GB-HBM3")
		n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
		return n
	}
	plausible := map[string]func(*corev1.Node){
		"not ready": func(n *corev1.Node) {
			n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
		},
		"tainted": func(n *corev1.Node) {
			n.Spec.Taints = []corev1.Taint{{Key: "nvidia.com/gpu", Effect: corev1.TaintEffectNoSchedule}}
		},
		"cordoned": func(n *corev1.Node) { n.Spec.Unschedulable = true },
		"no GPUs": func(n *corev1.Node) {
			n.Status.Allocatable = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("0")}
		},
		"no GPU capacity": func(n *corev1.Node) {
			n.Status.Capacity = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("0")}
		},
		"unknown readiness": func(n *corev1.Node) {
			n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionUnknown}}
		},
		"unreachable": func(n *corev1.Node) {
			n.Spec.Taints = []corev1.Taint{{Key: corev1.TaintNodeUnreachable, Effect: corev1.TaintEffectNoExecute}}
		},
		"deleting":  func(n *corev1.Node) { n.DeletionTimestamp = ptr.To(metav1.NewTime(time.Unix(1, 0))) },
		"providers": func(n *corev1.Node) { n.Spec.ProviderID = "aws:///us-east-1a/i-0" },
	}
	check := func(name string, a, b *corev1.Node) {
		t.Helper()
		nodes := func(n *corev1.Node) []corev1.Node { return []corev1.Node{*n.DeepCopy()} }
		cached := func(n *corev1.Node) *corev1.Node {
			out, err := TransformNode(n.DeepCopy())
			if err != nil {
				t.Fatal(err)
			}
			return out.(*corev1.Node)
		}
		if got, want := Observe(nodes(b), placement), Observe(nodes(a), placement); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: Observe() = %+v, want %+v", name, got, want)
		}
		if got, want := Observe(nodes(cached(a)), placement), Observe(nodes(a), placement); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: Observe() of the cached Node = %+v, want %+v", name, got, want)
		}
		if Relevant(a, b) || Relevant(b, a) {
			t.Errorf("%s: an update that discovery cannot see is relevant", name)
		}
		if !reflect.DeepEqual(cached(b), cached(a)) {
			t.Errorf("%s: the cache keeps a field discovery does not read", name)
		}
	}
	for name, change := range plausible {
		a := healthy()
		b := a.DeepCopy()
		change(b)
		check(name, &a, b)
	}
	for i := range 200 {
		a, b := &corev1.Node{}, &corev1.Node{}
		fill.Fill(a)
		fill.Fill(b)
		kept := healthy()
		for k, v := range a.Labels {
			kept.Labels[k] = v
		}
		if i%2 == 1 {
			kept.Labels["pool"] = "system"
		}
		for _, n := range []*corev1.Node{a, b} {
			n.Name, n.UID, n.ResourceVersion, n.Labels = kept.Name, a.UID, a.ResourceVersion, maps.Clone(kept.Labels)
			n.TypeMeta = a.TypeMeta
			info := &n.Status.NodeInfo
			info.Architecture, info.OSImage, info.KernelVersion = kept.Status.NodeInfo.Architecture, kept.Status.NodeInfo.OSImage, kept.Status.NodeInfo.KernelVersion
			info.ContainerRuntimeVersion, info.KubeletVersion = kept.Status.NodeInfo.ContainerRuntimeVersion, kept.Status.NodeInfo.KubeletVersion
		}
		check(fmt.Sprintf("random %d", i), a, b)
	}
}

// Cached Nodes share one copy of each label key: every node of a cluster
// carries the same keys.
func TestCachedNodesShareLabelKeys(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(labeledGPUNode())
	if err != nil {
		t.Fatal(err)
	}
	cached := func() map[string]string {
		n := &corev1.Node{}
		if err := json.Unmarshal(raw, n); err != nil {
			t.Fatal(err)
		}
		out, err := TransformNode(n)
		if err != nil {
			t.Fatal(err)
		}
		return out.(*corev1.Node).Labels
	}
	a, b := cached(), cached()
	keys := map[string]*byte{}
	for k, v := range a {
		keys[k] = unsafe.StringData(k)
		if v != b[k] {
			t.Fatalf("label %s = %q, then %q", k, v, b[k])
		}
	}
	for k := range b {
		if keys[k] != unsafe.StringData(k) {
			t.Fatalf("label key %s is stored once per cached Node", k)
		}
	}
	if got, err := TransformNode(&corev1.Node{}); err != nil || got.(*corev1.Node).Labels != nil {
		t.Errorf("an unlabeled Node caches labels %v", got)
	}
}

// A full key table still copies every label; the keys it cannot hold are
// stored with the Node.
func TestKeyTableBound(t *testing.T) {
	t.Parallel()
	table := &keyTable{keys: map[string]string{}, limit: 2}
	labels := map[string]string{"a": "1", "b": "2", "c": "3"}
	if got := table.share(labels); !reflect.DeepEqual(got, labels) || len(table.keys) != 2 {
		t.Fatalf("share() = %v with %d keys stored, want %v with 2", got, len(table.keys), labels)
	}
	if got := table.share(map[string]string{"d": "4"}); got["d"] != "4" || len(table.keys) != 2 {
		t.Errorf("share() past the bound = %v with %d keys stored", got, len(table.keys))
	}
}

// BenchmarkCachedNodeBytes reports the heap one cached Node holds, for a GPU
// node labeled as GPU feature discovery and Node Feature Discovery label one.
// Each Node is decoded from JSON, as the informer decodes it, then
// transformed.
func BenchmarkCachedNodeBytes(b *testing.B) {
	raw, err := json.Marshal(labeledGPUNode())
	if err != nil {
		b.Fatal(err)
	}
	const count = 1000
	for range b.N {
		kept := make([]any, count)
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		for i := range kept {
			n := &corev1.Node{}
			if err = json.Unmarshal(raw, n); err != nil {
				b.Fatal(err)
			}
			if kept[i], err = TransformNode(n); err != nil {
				b.Fatal(err)
			}
		}
		runtime.GC()
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(kept)
		b.ReportMetric(float64(after.HeapAlloc-before.HeapAlloc)/count, "bytes/node")
	}
}

// labeledGPUNode is an 8-GPU node with the labels, annotations, images and
// status a GPU node carries once GPU feature discovery and Node Feature
// Discovery have labeled it.
func labeledGPUNode() *corev1.Node {
	n := node("ip-10-0-12-34.ec2.internal", "gpu", "NVIDIA-H100-80GB-HBM3")
	for k, v := range map[string]string{
		"kubernetes.io/arch": "amd64", "kubernetes.io/os": "linux", "kubernetes.io/hostname": n.Name,
		"beta.kubernetes.io/arch": "amd64", "beta.kubernetes.io/os": "linux",
		"beta.kubernetes.io/instance-type": "p5.48xlarge", "node.kubernetes.io/instance-type": "p5.48xlarge",
		"topology.kubernetes.io/region": "us-east-1", "topology.kubernetes.io/zone": "us-east-1a",
		"failure-domain.beta.kubernetes.io/region": "us-east-1", "failure-domain.beta.kubernetes.io/zone": "us-east-1a",
		"eks.amazonaws.com/nodegroup": "gpu-h100", "eks.amazonaws.com/capacityType": "ON_DEMAND",
		"eks.amazonaws.com/nodegroup-image": "ami-0123456789abcdef0", "eks.amazonaws.com/sourceLaunchTemplateId": "lt-0123456789abcdef0",
		"eks.amazonaws.com/sourceLaunchTemplateVersion": "3", "k8s.io/cloud-provider-aws": "0123456789abcdef0123456789abcdef",
		"nvidia.com/cuda.driver-version.full": "580.95.05", "nvidia.com/cuda.driver-version.major": "580",
		"nvidia.com/cuda.driver-version.minor": "95", "nvidia.com/cuda.driver-version.revision": "05",
		"nvidia.com/cuda.driver.major": "580", "nvidia.com/cuda.driver.minor": "95", "nvidia.com/cuda.driver.rev": "05",
		"nvidia.com/cuda.runtime-version.full": "13.0", "nvidia.com/cuda.runtime-version.major": "13",
		"nvidia.com/cuda.runtime-version.minor": "0", "nvidia.com/cuda.runtime.major": "13", "nvidia.com/cuda.runtime.minor": "0",
		"nvidia.com/gfd.timestamp": "1763700000", "nvidia.com/gpu.compute.major": "9", "nvidia.com/gpu.compute.minor": "0",
		"nvidia.com/gpu.count": "8", "nvidia.com/gpu.family": "hopper", "nvidia.com/gpu.machine": "p5.48xlarge",
		"nvidia.com/gpu.memory": "81559", "nvidia.com/gpu.mode": "compute", "nvidia.com/gpu.replicas": "1",
		"nvidia.com/gpu.sharing-strategy": "none", "nvidia.com/gpu.present": "true", "nvidia.com/mig.capable": "true",
		"nvidia.com/mig.strategy": "single", "nvidia.com/mps.capable": "false", "nvidia.com/vgpu.present": "false",
		"nvidia.com/gpu.deploy.container-toolkit": "true", "nvidia.com/gpu.deploy.dcgm": "true",
		"nvidia.com/gpu.deploy.dcgm-exporter": "true", "nvidia.com/gpu.deploy.device-plugin": "true",
		"nvidia.com/gpu.deploy.driver": "pre-installed", "nvidia.com/gpu.deploy.gpu-feature-discovery": "true",
		"nvidia.com/gpu.deploy.node-status-exporter": "true", "nvidia.com/gpu.deploy.operator-validator": "true",
		"nvidia.com/gpu-driver-upgrade-state": "upgrade-done", "nvidia.com/gpu.clique": "0123abcd-4567-89ef.1",
		"feature.node.kubernetes.io/cpu-hardware_multithreading": "true", "feature.node.kubernetes.io/cpu-model.family": "6",
		"feature.node.kubernetes.io/cpu-model.id": "143", "feature.node.kubernetes.io/cpu-model.vendor_id": "Intel",
		"feature.node.kubernetes.io/kernel-version.full": "6.8.0-1031-aws", "feature.node.kubernetes.io/kernel-version.major": "6",
		"feature.node.kubernetes.io/kernel-version.minor": "8", "feature.node.kubernetes.io/kernel-version.revision": "0",
		"feature.node.kubernetes.io/memory-numa": "true", "feature.node.kubernetes.io/network-sriov.capable": "true",
		"feature.node.kubernetes.io/pci-10de.present": "true", "feature.node.kubernetes.io/pci-1d0f.present": "true",
		"feature.node.kubernetes.io/storage-nonrotationaldisk": "true", "feature.node.kubernetes.io/system-os_release.ID": "ubuntu",
		"feature.node.kubernetes.io/system-os_release.VERSION_ID": "24.04", "feature.node.kubernetes.io/system-os_release.VERSION_ID.major": "24",
		"feature.node.kubernetes.io/system-os_release.VERSION_ID.minor": "04", "feature.node.kubernetes.io/cpu-pstate.status": "active",
	} {
		n.Labels[k] = v
	}
	for _, flag := range []string{"ADX", "AESNI", "AMXBF16", "AMXINT8", "AMXTILE", "AVX", "AVX2", "AVX512BF16", "AVX512BITALG",
		"AVX512BW", "AVX512CD", "AVX512DQ", "AVX512F", "AVX512FP16", "AVX512IFMA", "AVX512VBMI", "AVX512VBMI2", "AVX512VL",
		"AVX512VNNI", "AVX512VPOPCNTDQ", "AVXVNNI", "CETIBT", "CETSS", "CMPXCHG8", "FMA3", "FSRM", "FXSR", "FXSROPT", "GFNI",
		"HYPERVISOR", "IBPB", "LAHF", "MOVBE", "MOVDIR64B", "MOVDIRI", "OSXSAVE", "SERIALIZE", "SHA", "SPEC_CTRL_SSBD",
		"STIBP", "SYSCALL", "SYSEE", "TSXLDTRK", "VAES", "VMX", "VPCLMULQDQ", "WAITPKG", "WBNOINVD", "X87", "XGETBV1",
		"XSAVE", "XSAVEC", "XSAVEOPT", "XSAVES"} {
		n.Labels["feature.node.kubernetes.io/cpu-cpuid."+flag] = "true"
	}
	keys := make([]string, 0, len(n.Labels))
	for k := range n.Labels {
		if strings.HasPrefix(k, "feature.node.kubernetes.io/") {
			keys = append(keys, strings.TrimPrefix(k, "feature.node.kubernetes.io/"))
		}
	}
	n.Annotations = map[string]string{
		"nfd.node.kubernetes.io/feature-labels":                  strings.Join(keys, ","),
		"node.alpha.kubernetes.io/ttl":                           "0",
		"volumes.kubernetes.io/controller-managed-attach-detach": "true",
		"csi.volume.kubernetes.io/nodeid":                        `{"ebs.csi.aws.com":"i-0123456789abcdef0","efs.csi.aws.com":"i-0123456789abcdef0"}`,
	}
	n.Spec = corev1.NodeSpec{
		ProviderID: "aws:///us-east-1a/i-0123456789abcdef0",
		Taints:     []corev1.Taint{{Key: "nvidia.com/gpu", Value: "present", Effect: corev1.TaintEffectNoSchedule}},
	}
	resources := corev1.ResourceList{
		"cpu": resource.MustParse("192"), "memory": resource.MustParse("2097152Mi"), "pods": resource.MustParse("110"),
		"ephemeral-storage": resource.MustParse("3758096384Ki"), "hugepages-1Gi": resource.MustParse("0"),
		"hugepages-2Mi": resource.MustParse("0"), "nvidia.com/gpu": resource.MustParse("8"),
		"vpc.amazonaws.com/efa": resource.MustParse("32"), "rdma/ib": resource.MustParse("0"),
	}
	n.Status.Capacity, n.Status.Allocatable = resources, resources.DeepCopy()
	beat := metav1.NewTime(time.Unix(1763700000, 0))
	for _, c := range []corev1.NodeConditionType{corev1.NodeMemoryPressure, corev1.NodeDiskPressure, corev1.NodePIDPressure, corev1.NodeReady} {
		status := corev1.ConditionFalse
		if c == corev1.NodeReady {
			status = corev1.ConditionTrue
		}
		n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{
			Type: c, Status: status, LastHeartbeatTime: beat, LastTransitionTime: beat, Reason: "Kubelet" + string(c), Message: "kubelet reports " + string(c),
		})
	}
	n.Status.Addresses = []corev1.NodeAddress{
		{Type: corev1.NodeInternalIP, Address: "10.0.12.34"}, {Type: corev1.NodeHostName, Address: n.Name},
		{Type: corev1.NodeInternalDNS, Address: n.Name},
	}
	n.Status.DaemonEndpoints.KubeletEndpoint.Port = 10250
	n.Status.NodeInfo.MachineID, n.Status.NodeInfo.SystemUUID = "ec2a0123456789abcdef0123456789ab", "ec2a0123-4567-89ab-cdef-0123456789ab"
	n.Status.NodeInfo.BootID, n.Status.NodeInfo.OperatingSystem = "01234567-89ab-cdef-0123-456789abcdef", "linux"
	n.Status.RuntimeHandlers = []corev1.NodeRuntimeHandler{{Name: "nvidia"}, {Name: "runc"}, {Name: ""}}
	for i := range 40 {
		name := fmt.Sprintf("nvcr.io/nvidia/k8s/component-%02d", i)
		n.Status.Images = append(n.Status.Images, corev1.ContainerImage{
			Names:     []string{name + "@sha256:" + strings.Repeat("0123456789abcdef", 4), name + ":v26.7.3"},
			SizeBytes: 512 << 20,
		})
	}
	return &n
}
