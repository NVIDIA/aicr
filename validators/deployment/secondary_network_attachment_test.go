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
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"aicr.run/pkg/defaults"
	"aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
	"aicr.run/pkg/recipe"
	"aicr.run/pkg/validator/catalog"
	"aicr.run/pkg/validator/ctrf"
	"aicr.run/pkg/validator/labels"
	v1 "aicr.run/pkg/validator/v1"
	"aicr.run/validators"
	"aicr.run/validators/helper"
)

const (
	testOVNNodeSubnets = `{"default":"192.168.1.0/24"}`
	testMlnxNICs       = "nvidia.com/mlnxnics"
)

// Recipe shapes, as the shipped leaves wire them.
var (
	gb200NVKERefs = []recipe.ComponentRef{
		{Name: networkOperatorComponent, Namespace: testNetworkOperatorNamespace,
			ValuesFile:    "components/network-operator/values-nvke-gb200.yaml",
			ManifestFiles: []string{"components/network-operator/manifests/nic-cluster-policy-nvke-gb200.yaml"}},
		{Name: sriovIBNetworksComponent, Namespace: testNetworkOperatorNamespace,
			ManifestFiles: []string{"components/sriov-ib-networks/manifests/nvke-gb200.yaml"}},
	}
	vr200NVKERefs = []recipe.ComponentRef{
		{Name: sriovIBNetworksComponent, Namespace: testNetworkOperatorNamespace,
			ManifestFiles: []string{"components/sriov-ib-networks/manifests/nvke-vr200.yaml"}},
	}
	gb300OKERefs = []recipe.ComponentRef{
		{Name: networkOperatorComponent, Namespace: testNetworkOperatorNamespace,
			ValuesFile: "components/network-operator/values-oke-gb300.yaml",
			ManifestFiles: []string{
				"components/network-operator/manifests/nic-cluster-policy-oke-gb300.yaml",
				"components/network-operator/manifests/rdma-net-oke-gb300.yaml",
			}},
	}
)

func railResource(i int) string { return fmt.Sprintf("nvidia.com/sriov_resource_rail_%d", i) }

// railAllocatable advertises count units of each of rails rails.
func railAllocatable(rails int, count int64) map[string]int64 {
	out := map[string]int64{}
	for i := range rails {
		out[railResource(i)] = count
	}
	return out
}

// attachmentNode is a Ready GPU node with OVN node-subnets, advertising
// alloc.
func attachmentNode(name string, alloc map[string]int64, mutate ...func(*corev1.Node)) *corev1.Node {
	allocatable := corev1.ResourceList{corev1.ResourceName(helper.GpuResourceName): *resource.NewQuantity(4, resource.DecimalSI)}
	for r, n := range alloc {
		allocatable[corev1.ResourceName(r)] = *resource.NewQuantity(n, resource.DecimalSI)
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{ovnNodeSubnetsAnnotation: testOVNNodeSubnets}},
		Status: corev1.NodeStatus{
			Allocatable: allocatable,
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	for _, m := range mutate {
		m(node)
	}
	return node
}

func cordoned(n *corev1.Node) { n.Spec.Unschedulable = true }

func notReady(n *corev1.Node) {
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
}

func simulated(n *corev1.Node) { n.Annotations[kwokNodeAnnotation] = "fake" }

// workloadPod is a running pod on node holding count units of resourceName.
func workloadPod(name, node, resourceName string, count int64) *corev1.Pod {
	q := *resource.NewQuantity(count, resource.DecimalSI)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "workloads"},
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{
			Name:      "main",
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceName(resourceName): q}},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// probeStamp decides what a created probe pod looks like: it mutates the pod
// and returns any objects (events) to add beside it.
type probeStamp func(pod *corev1.Pod) []runtime.Object

// attachedStatus stamps a Succeeded pod whose network-status shows the
// requested network attached as net1 with ip (no ip when ip is empty) and a
// VF PCI address.
func attachedStatus(ip string) probeStamp {
	return func(pod *corev1.Pod) []runtime.Object {
		entry := map[string]any{
			"name":        pod.Annotations[multusNetworksAnnotation],
			"interface":   "net1",
			"device-info": map[string]any{"type": "pci", "pci": map[string]any{"pci-address": "0000:03:00.2", "rdma-device": "mlx5_5"}},
		}
		if ip != "" {
			entry["ips"] = []string{ip}
		}
		raw, _ := json.Marshal([]any{
			map[string]any{"name": "ovn-kubernetes", "interface": "eth0", "ips": []string{"192.168.1.5"}, "default": true},
			entry,
		})
		pod.Annotations[multusNetworkStatusAnnotation] = string(raw)
		pod.Status.Phase = corev1.PodSucceeded
		return nil
	}
}

// perRailStatus stamps each rail's probe with an address from that rail's
// pool, 10.255.<4n>.0/22, reading the rail from the network annotation after
// prefix.
func perRailStatus(prefix string) probeStamp {
	return func(pod *corev1.Pod) []runtime.Object {
		rail, _ := strconv.Atoi(strings.TrimPrefix(pod.Annotations[multusNetworksAnnotation], prefix))
		return attachedStatus(fmt.Sprintf("10.255.%d.12", 4*rail))(pod)
	}
}

// probeLogs is the probe's output on a pod that got net1 carrying addrs.
func probeLogs(addrs []string) string {
	var b strings.Builder
	b.WriteString(probeSysNetBegin + "\neth0\nlo\nnet1\n" + probeSysNetEnd + "\n" + probeAddrBegin + "\n")
	b.WriteString("2: eth0    inet 192.168.1.5/24 scope global eth0\n")
	for _, addr := range addrs {
		fmt.Fprintf(&b, "3: net1    inet %s/22 scope global net1\n", addr)
	}
	b.WriteString(probeAddrEnd + "\n")
	return b.String()
}

// stampedAddrs returns the addresses pod's network-status gives the network
// it requested.
func stampedAddrs(pod *corev1.Pod) []string {
	entries, err := parseNetworkStatus(pod.Annotations[multusNetworkStatusAnnotation])
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if e.Name == pod.Annotations[multusNetworksAnnotation] {
			return e.IPs
		}
	}
	return nil
}

// attachmentCluster is a fake apiserver whose created probe pods are stamped
// at create, as a kubelet and Multus would have left them, each with its own
// probe output carrying the addresses its network-status reports. The fake
// log request names no pod, so the output is served through config.
type attachmentCluster struct {
	client *k8sfake.Clientset
	mu     sync.Mutex
	probes []*corev1.Pod
	logs   map[string]string
}

// config returns base reading each probe's own output.
func (c *attachmentCluster) config(base attachmentCheckConfig) attachmentCheckConfig {
	base.podLogs = func(_ context.Context, _ kubernetes.Interface, _, pod, _ string) (string, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		logs, ok := c.logs[pod]
		if !ok {
			return "", fmt.Errorf("no probe output for pod %s", pod)
		}
		return logs, nil
	}
	return base
}

func newAttachmentCluster(t *testing.T, stamp probeStamp, objects ...runtime.Object) *attachmentCluster {
	t.Helper()
	c := &attachmentCluster{client: k8sfake.NewClientset(objects...), logs: map[string]string{}}
	c.client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if !ok || pod.GenerateName != attachmentProbeGenerateName {
			return false, nil, nil
		}
		cp := pod.DeepCopy()
		c.mu.Lock()
		cp.Name = cp.GenerateName + strconv.Itoa(len(c.probes))
		c.probes = append(c.probes, cp)
		c.mu.Unlock()
		extra := stamp(cp)
		c.mu.Lock()
		c.logs[cp.Name] = probeLogs(stampedAddrs(cp))
		c.mu.Unlock()
		if err := c.client.Tracker().Add(cp); err != nil {
			return true, nil, err
		}
		for _, obj := range extra {
			if err := c.client.Tracker().Add(obj); err != nil {
				return true, nil, err
			}
		}
		return true, cp, nil
	})
	return c
}

func (c *attachmentCluster) createdProbes() []*corev1.Pod {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*corev1.Pod(nil), c.probes...)
}

// probeNamespaces lists the namespaces carrying the run ownership label.
func (c *attachmentCluster) probeNamespaces(t *testing.T) []corev1.Namespace {
	t.Helper()
	list, err := c.client.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{LabelSelector: labels.GPUTestRun})
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	return list.Items
}

func attachmentContext(t *testing.T, client *k8sfake.Clientset, refs []recipe.ComponentRef) *validators.Context {
	t.Helper()
	return attachmentContextWithin(t, client, refs, 30*time.Second)
}

func attachmentContextWithin(t *testing.T, client *k8sfake.Clientset, refs []recipe.ComponentRef, budget time.Duration) *validators.Context {
	t.Helper()
	goCtx, cancel := context.WithTimeout(context.Background(), budget)
	t.Cleanup(cancel)
	return &validators.Context{
		Ctx:             goCtx,
		Clientset:       client,
		ValidationInput: &v1.ValidationInput{ComponentRefs: refs},
	}
}

// probeNode returns the node a probe's required node affinity pins it to.
func probeNode(pod *corev1.Pod) string {
	a := pod.Spec.Affinity
	if a == nil || a.NodeAffinity == nil || a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return ""
	}
	terms := a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || len(terms[0].MatchFields) != 1 || len(terms[0].MatchExpressions) != 0 {
		return ""
	}
	f := terms[0].MatchFields[0]
	if f.Key != nodeNameField || f.Operator != corev1.NodeSelectorOpIn || len(f.Values) != 1 {
		return ""
	}
	return f.Values[0]
}

// captureOutput returns what fn writes to stdout, the merged Extra in it, and
// fn's error. Callers must not be parallel.
func captureOutput(t *testing.T, fn func() error) (string, map[string]string, error) {
	t.Helper()
	out, err := captureStdout(t, fn)
	extra := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		payload, ok := strings.CutPrefix(line, ctrf.ExtraLinePrefix)
		if !ok {
			continue
		}
		var parsed map[string]string
		if jerr := json.Unmarshal([]byte(payload), &parsed); jerr != nil {
			t.Fatalf("sentinel payload %q: %v", payload, jerr)
		}
		for k, v := range parsed {
			extra[k] = v
		}
	}
	return out, extra, err
}

// testAttachmentConfig keeps a stuck probe's wait short.
var testAttachmentConfig = attachmentCheckConfig{
	probeTimeout:  300 * time.Millisecond,
	reportReserve: 0,
	concurrency:   defaults.SecondaryNetworkProbeConcurrency,
}

// TestCheckSecondaryNetworkAttachment_PassGB200 runs the check end to end on
// the NVKE GB200 shape: one probe per rail, each placed by the scheduler on
// the node with a free unit, requesting one VF and annotated with its network,
// and the probe namespace deleted afterwards.
func TestCheckSecondaryNetworkAttachment_PassGB200(t *testing.T) {
	cluster := newAttachmentCluster(t, perRailStatus("default/sriov-network-rail-"),
		attachmentNode("gpu-a", railAllocatable(4, 16)))
	ctx := attachmentContext(t, cluster.client, gb200NVKERefs)

	extra, err := captureExtras(t, func() error { return runSecondaryNetworkAttachment(ctx, cluster.config(testAttachmentConfig)) })
	if err != nil {
		t.Fatalf("runSecondaryNetworkAttachment() error = %v, want pass", err)
	}
	assertExtraSubset(t, extra, map[string]string{
		validators.ExtraKeyNodesValidated: "4", validators.ExtraKeyNodesTotal: "4",
		validators.ExtraKeyNodesFailed: "0", validators.ExtraKeyNodesSkipped: "0",
	})

	probes := cluster.createdProbes()
	if len(probes) != 4 {
		t.Fatalf("created %d probe pods, want 4", len(probes))
	}
	seen := map[string]bool{}
	for _, pod := range probes {
		network := pod.Annotations[multusNetworksAnnotation]
		seen[network] = true
		rail := strings.TrimPrefix(network, "default/sriov-network-rail-")
		res := corev1.ResourceName("nvidia.com/sriov_resource_rail_" + rail)
		if q := pod.Spec.Containers[0].Resources.Limits[res]; q.Value() != 1 {
			t.Errorf("probe for %s limits %s = %v, want 1", network, res, q.String())
		}
		if got := probeNode(pod); got != "gpu-a" || pod.Spec.NodeName != "" {
			t.Errorf("probe for %s placed by affinity on %q with nodeName %q, want the scheduler to place it on gpu-a",
				network, got, pod.Spec.NodeName)
		}
		if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
			t.Errorf("probe for %s mounts an API token", network)
		}
		if !strings.HasPrefix(pod.Namespace, defaults.GPUTestNamespacePrefix) || pod.Labels[labels.GPUTestRun] == "" {
			t.Errorf("probe for %s in namespace %q with labels %v, want the per-run test namespace and ownership label",
				network, pod.Namespace, pod.Labels)
		}
	}
	if len(seen) != 4 {
		t.Errorf("probed networks %v, want all four rails", seen)
	}
	if left := cluster.probeNamespaces(t); len(left) != 0 {
		t.Errorf("probe namespace(s) left behind: %v", left)
	}
}

// TestCheckSecondaryNetworkAttachment_Outcomes drives the verdicts on fake
// clusters: each failure names the network and why, skips carry the code the
// evidence reads, and a probe that could not run never passes.
func TestCheckSecondaryNetworkAttachment_Outcomes(t *testing.T) {
	sandboxFailure := func(pod *corev1.Pod) []runtime.Object {
		pod.Status.Phase = corev1.PodPending
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: attachmentProbeContainer,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}}
		return []runtime.Object{&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: pod.Name + ".sandbox", Namespace: pod.Namespace},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: pod.Name, Namespace: pod.Namespace},
			Reason:         eventReasonFailedCreatePodSandBox,
			Message: `Failed to create pod sandbox: plugin type="multus-shim" name="multus-cni-network" failed (add): ` +
				`CmdAdd (shim): error getting pod: Unauthorized`,
			LastTimestamp: metav1.Now(),
		}}
	}
	statusOnly := func(annotation string) probeStamp {
		return func(pod *corev1.Pod) []runtime.Object {
			if annotation != "" {
				pod.Annotations[multusNetworkStatusAnnotation] = annotation
			}
			pod.Status.Phase = corev1.PodSucceeded
			return nil
		}
	}
	failedWith := func(reason, message string) probeStamp {
		return func(pod *corev1.Pod) []runtime.Object {
			attachedStatus("")(pod)
			pod.Status.Phase, pod.Status.Reason, pod.Status.Message = corev1.PodFailed, reason, message
			return nil
		}
	}
	unschedulable := func(message string) probeStamp {
		return func(pod *corev1.Pod) []runtime.Object {
			pod.Status.Phase = corev1.PodPending
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
				Reason: corev1.PodReasonUnschedulable, Message: message}}
			return nil
		}
	}
	const schedulerShortage = "0/2 nodes are available: 1 Insufficient nvidia.com/mlnxnics, 1 node(s) didn't match Pod's node affinity/selector."
	const devicesUnavailableMessage = "Pod was rejected: Allocate failed due to requested number of devices unavailable " +
		"for nvidia.com/mlnxnics. Requested: 1, Available: 0, which is unexpected"
	mlnxNode := func(name string, mutate ...func(*corev1.Node)) *corev1.Node {
		return attachmentNode(name, map[string]int64{testMlnxNICs: 4}, mutate...)
	}
	noGPU := func(n *corev1.Node) { delete(n.Status.Allocatable, corev1.ResourceName(helper.GpuResourceName)) }

	tests := []struct {
		name          string
		refs          []recipe.ComponentRef
		objects       []runtime.Object
		stamp         probeStamp
		setup         func(*k8sfake.Clientset)
		realLogs      bool
		wantPass      bool
		wantSkip      string
		wantErrSubs   []string
		wantExtra     map[string]string
		wantNoProbeNS bool
	}{
		{
			name:      "OKE GB300 empty IPAM passes on the interface alone",
			refs:      gb300OKERefs,
			objects:   []runtime.Object{mlnxNode("gpu-a")},
			stamp:     attachedStatus(""),
			wantPass:  true,
			wantExtra: map[string]string{validators.ExtraKeyNodesValidated: "1", validators.ExtraKeyNodesTotal: "1"},
		},
		{
			name:      "a KWOK GPU node beside a real network node does not skip the check",
			refs:      gb300OKERefs,
			objects:   []runtime.Object{mlnxNode("gpu-a"), attachmentNode("kwok-a", nil, simulated)},
			stamp:     attachedStatus(""),
			wantPass:  true,
			wantExtra: map[string]string{validators.ExtraKeyNodesValidated: "1", validators.ExtraKeyNodesTotal: "1"},
		},
		{
			name:        "no Multus: no network-status",
			refs:        gb300OKERefs,
			objects:     []runtime.Object{mlnxNode("gpu-a")},
			stamp:       statusOnly(""),
			wantErrSubs: []string{"nvidia-network-operator/rdma-net on node gpu-a", "Multus attached no network"},
			wantExtra:   map[string]string{validators.ExtraKeyNodesFailed: "1", validators.ExtraKeyNodesValidated: "0"},
		},
		{
			name:        "missing attachment fails",
			refs:        gb300OKERefs,
			objects:     []runtime.Object{mlnxNode("gpu-a")},
			stamp:       statusOnly(`[{"name":"ovn-kubernetes","interface":"eth0","ips":["192.168.1.5"],"default":true}]`),
			wantErrSubs: []string{"has no entry for nvidia-network-operator/rdma-net"},
		},
		{
			name:        "sandbox failure fails with the kubelet event text",
			refs:        gb300OKERefs,
			objects:     []runtime.Object{mlnxNode("gpu-a")},
			stamp:       sandboxFailure,
			wantErrSubs: []string{eventReasonFailedCreatePodSandBox, "error getting pod: Unauthorized"},
		},
		{
			name: "only rail 0's address inside the pod network fails that rail alone",
			refs: gb200NVKERefs,
			objects: []runtime.Object{attachmentNode("gpu-a", railAllocatable(4, 16), func(n *corev1.Node) {
				n.Annotations[ovnNodeSubnetsAnnotation] = `{"default":["10.255.0.0/24"]}`
			})},
			stamp:       perRailStatus("default/sriov-network-rail-"),
			wantErrSubs: []string{"failed for 1 of 4", "default/sriov-network-rail-0 on node gpu-a", "inside the pod network 10.255.0.0/24"},
			wantExtra:   map[string]string{validators.ExtraKeyNodesFailed: "1", validators.ExtraKeyNodesValidated: "3"},
		},
		{
			name: "malformed OVN node-subnets fails instead of skipping the overlap",
			refs: gb200NVKERefs,
			objects: []runtime.Object{attachmentNode("gpu-a", railAllocatable(4, 16), func(n *corev1.Node) {
				n.Annotations[ovnNodeSubnetsAnnotation] = `{"default":42}`
			})},
			stamp:         perRailStatus("default/sriov-network-rail-"),
			wantErrSubs:   []string{"unparseable " + ovnNodeSubnetsAnnotation},
			wantNoProbeNS: true,
		},
		{
			name:        "one rail advertised nowhere fails while the others pass",
			refs:        gb200NVKERefs,
			objects:     []runtime.Object{attachmentNode("gpu-a", railAllocatable(3, 16))},
			stamp:       perRailStatus("default/sriov-network-rail-"),
			wantErrSubs: []string{"failed for 1 of 4", "default/sriov-network-rail-3: no node advertises allocatable " + railResource(3)},
			wantExtra:   map[string]string{validators.ExtraKeyNodesFailed: "1", validators.ExtraKeyNodesValidated: "3"},
		},
		{
			name: "one busy rail and three cordoned rails skip as busy",
			refs: gb200NVKERefs,
			objects: []runtime.Object{
				attachmentNode("gpu-a", map[string]int64{railResource(0): 1}),
				workloadPod("train", "gpu-a", railResource(0), 1),
				attachmentNode("gpu-b", map[string]int64{railResource(1): 16, railResource(2): 16, railResource(3): 16}, cordoned),
			},
			stamp:         perRailStatus("default/sriov-network-rail-"),
			wantSkip:      validators.SkipReasonNodesBusy,
			wantExtra:     map[string]string{validators.ExtraKeyNodesSkipped: "4", validators.ExtraKeyNodesTotal: "4"},
			wantNoProbeNS: true,
		},
		{
			name:    "the device manager losing the last unit to a workload is a busy skip",
			refs:    gb300OKERefs,
			objects: []runtime.Object{mlnxNode("gpu-a")},
			stamp: func(pod *corev1.Pod) []runtime.Object {
				failedWith(kubeletUnexpectedAdmissionError, devicesUnavailableMessage)(pod)
				// A workload bound outside the scheduler took every unit.
				return []runtime.Object{workloadPod("bound-by-name", "gpu-a", testMlnxNICs, 4)}
			},
			wantSkip: validators.SkipReasonNodesBusy,
		},
		{
			name:        "the device manager short of a unit no workload holds fails as unhealthy",
			refs:        gb300OKERefs,
			objects:     []runtime.Object{mlnxNode("gpu-a")},
			stamp:       failedWith(kubeletUnexpectedAdmissionError, devicesUnavailableMessage),
			wantErrSubs: []string{"other workloads request only 0 of 4", "a unit may be unhealthy"},
		},
		{
			name:        "the device manager rejecting another resource fails",
			refs:        gb300OKERefs,
			objects:     []runtime.Object{mlnxNode("gpu-a")},
			stamp:       failedWith(kubeletUnexpectedAdmissionError, "Allocate failed due to requested number of devices unavailable for nvidia.com/gpu"),
			wantErrSubs: []string{"the probe pod failed", kubeletUnexpectedAdmissionError},
		},
		{
			name:        "OutOf<resource> fails: the device plugin is not registered",
			refs:        gb300OKERefs,
			objects:     []runtime.Object{mlnxNode("gpu-a")},
			stamp:       failedWith("OutOf"+testMlnxNICs, "Pod was rejected: Node didn't have enough resource: nvidia.com/mlnxnics"),
			wantErrSubs: []string{"OutOf" + testMlnxNICs, "may not be registered on that node"},
		},
		{
			name:        "a probe that failed with an attached status still fails",
			refs:        gb300OKERefs,
			objects:     []runtime.Object{mlnxNode("gpu-a")},
			stamp:       failedWith("Error", ""),
			wantErrSubs: []string{"the probe pod failed: phase Failed, reason Error"},
		},
		{
			name:    "the scheduler finding no free unit while workloads hold them all is a busy skip",
			refs:    gb300OKERefs,
			objects: []runtime.Object{mlnxNode("gpu-a")},
			stamp: func(pod *corev1.Pod) []runtime.Object {
				unschedulable(schedulerShortage)(pod)
				return []runtime.Object{workloadPod("arrived-after-planning", "gpu-a", testMlnxNICs, 4)}
			},
			wantSkip: validators.SkipReasonNodesBusy,
		},
		{
			name:        "the scheduler finding no free unit that no workload holds fails",
			refs:        gb300OKERefs,
			objects:     []runtime.Object{mlnxNode("gpu-a")},
			stamp:       unschedulable(schedulerShortage),
			wantErrSubs: []string{"other workloads request only 0 of 4", "a unit may be unhealthy"},
		},
		{
			name: "units held only by this run's own probes are not workload contention",
			refs: gb300OKERefs,
			objects: []runtime.Object{mlnxNode("gpu-a", func(n *corev1.Node) {
				n.Status.Allocatable[corev1.ResourceName(testMlnxNICs)] = *resource.NewQuantity(1, resource.DecimalSI)
			})},
			stamp: func(pod *corev1.Pod) []runtime.Object {
				unschedulable(schedulerShortage)(pod)
				own := workloadPod("own-probe", "gpu-a", testMlnxNICs, 1)
				own.Namespace = pod.Namespace
				return []runtime.Object{own}
			},
			wantErrSubs: []string{"other workloads request only 0 of 1"},
		},
		{
			name:    "a watch that fails after the scheduler reported a shortage fails, never busy",
			refs:    gb300OKERefs,
			objects: []runtime.Object{mlnxNode("gpu-a")},
			stamp:   unschedulable("0/2 nodes are available: 1 Insufficient nvidia.com/mlnxnics."),
			setup: func(c *k8sfake.Clientset) {
				c.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
					return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "probe", fmt.Errorf("watch denied"))
				})
			},
			wantErrSubs: []string{"could not be observed to the end of the probe budget", "watch denied"},
		},
		{
			name:        "any other scheduler refusal fails with its message",
			refs:        gb300OKERefs,
			objects:     []runtime.Object{mlnxNode("gpu-a")},
			stamp:       unschedulable("0/1 nodes are available: 1 node(s) had untolerated taint {dedicated: infra}."),
			wantErrSubs: []string{"the scheduler could not place the probe on gpu-a", "untolerated taint {dedicated: infra}"},
		},
		{
			name:    "a probe pod that cannot be created fails its network",
			refs:    gb300OKERefs,
			objects: []runtime.Object{mlnxNode("gpu-a")},
			stamp:   attachedStatus(""),
			setup: func(c *k8sfake.Clientset) {
				c.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "probe", fmt.Errorf("admission denied"))
				})
			},
			wantErrSubs: []string{"the probe pod was not created", "admission denied"},
		},
		{
			name:    "unreadable probe logs fail",
			refs:    gb300OKERefs,
			objects: []runtime.Object{mlnxNode("gpu-a")},
			stamp:   attachedStatus(""),
			setup: func(c *k8sfake.Clientset) {
				c.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
					if action.GetSubresource() != "log" {
						return false, nil, nil
					}
					return true, nil, fmt.Errorf("kubelet unreachable")
				})
			},
			realLogs:    true,
			wantErrSubs: []string{"the probe's output could not be read"},
		},
		{
			name:          "uncordoned NotReady candidates fail",
			refs:          gb300OKERefs,
			objects:       []runtime.Object{mlnxNode("gpu-a", notReady), mlnxNode("gpu-b", cordoned)},
			stamp:         attachedStatus(""),
			wantErrSubs:   []string{"1 uncordoned node(s) are NotReady [gpu-a] and 1 cordoned"},
			wantNoProbeNS: true,
		},
		{
			name:          "every candidate cordoned skips unschedulable",
			refs:          gb300OKERefs,
			objects:       []runtime.Object{mlnxNode("gpu-a", cordoned)},
			stamp:         attachedStatus(""),
			wantSkip:      validators.SkipReasonNoSchedulableGPUNodes,
			wantNoProbeNS: true,
		},
		{
			name:          "resource advertised nowhere fails",
			refs:          gb300OKERefs,
			objects:       []runtime.Object{attachmentNode("gpu-a", nil)},
			stamp:         attachedStatus(""),
			wantErrSubs:   []string{"no node advertises allocatable nvidia.com/mlnxnics"},
			wantNoProbeNS: true,
		},
		{
			name:          "no node with a GPU or the resource fails",
			refs:          gb300OKERefs,
			objects:       []runtime.Object{attachmentNode("cpu-a", nil, noGPU)},
			stamp:         attachedStatus(""),
			wantErrSubs:   []string{"no node advertises allocatable nvidia.com/mlnxnics"},
			wantNoProbeNS: true,
		},
		{
			name:          "KWOK nodes skip simulated",
			refs:          gb300OKERefs,
			objects:       []runtime.Object{mlnxNode("kwok-a", simulated)},
			stamp:         attachedStatus(""),
			wantSkip:      validators.SkipReasonSimulatedNodes,
			wantNoProbeNS: true,
		},
		{
			name:          "recipe ships no secondary network skips",
			refs:          []recipe.ComponentRef{{Name: "gpu-operator", Namespace: "gpu-operator"}},
			objects:       []runtime.Object{attachmentNode("gpu-a", nil)},
			stamp:         attachedStatus(""),
			wantSkip:      validators.SkipReasonComponentNotInRecipe,
			wantNoProbeNS: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := newAttachmentCluster(t, tt.stamp, tt.objects...)
			if tt.setup != nil {
				tt.setup(cluster.client)
			}
			ctx := attachmentContext(t, cluster.client, tt.refs)
			cfg := cluster.config(testAttachmentConfig)
			if tt.realLogs {
				cfg.podLogs = nil
			}
			extra, err := captureExtras(t, func() error { return runSecondaryNetworkAttachment(ctx, cfg) })

			switch {
			case tt.wantPass:
				if err != nil {
					t.Fatalf("error = %v, want pass", err)
				}
			case tt.wantSkip != "":
				if !validators.IsSkip(err) {
					t.Fatalf("error = %v, want skip %s", err, tt.wantSkip)
				}
				assertExtraSubset(t, extra, map[string]string{validators.ExtraKeySkipReason: tt.wantSkip})
			default:
				if err == nil || validators.IsSkip(err) {
					t.Fatalf("error = %v, want failure", err)
				}
				for _, sub := range tt.wantErrSubs {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("error = %v, want substring %q", err, sub)
					}
				}
			}
			assertExtraSubset(t, extra, tt.wantExtra)
			if tt.wantNoProbeNS && len(cluster.createdProbes()) != 0 {
				t.Errorf("created %d probe pod(s), want none", len(cluster.createdProbes()))
			}
			if left := cluster.probeNamespaces(t); len(left) != 0 {
				t.Errorf("probe namespace(s) left behind: %v", left)
			}
		})
	}
}

// TestCheckSecondaryNetworkAttachment_EntryPoints covers the paths that end
// before discovery or before any cluster call.
func TestCheckSecondaryNetworkAttachment_EntryPoints(t *testing.T) {
	t.Run("no validation input skips", func(t *testing.T) {
		ctx := &validators.Context{Ctx: context.Background(), Clientset: k8sfake.NewClientset()}
		extra, err := captureExtras(t, func() error { return runSecondaryNetworkAttachment(ctx, testAttachmentConfig) })
		if !validators.IsSkip(err) {
			t.Fatalf("error = %v, want skip", err)
		}
		assertExtraSubset(t, extra, map[string]string{validators.ExtraKeySkipReason: validators.SkipReasonNoValidationInput})
	})
	t.Run("no clientset fails", func(t *testing.T) {
		ctx := attachmentContext(t, nil, gb300OKERefs)
		ctx.Clientset = nil
		_, err := captureExtras(t, func() error { return runSecondaryNetworkAttachment(ctx, testAttachmentConfig) })
		if err == nil || validators.IsSkip(err) {
			t.Fatalf("error = %v, want failure", err)
		}
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	})
	t.Run("production config is the pkg/defaults budget", func(t *testing.T) {
		got := defaultAttachmentCheckConfig()
		if got.probeTimeout != defaults.SecondaryNetworkProbeTimeout ||
			got.reportReserve != defaults.SecondaryNetworkProbeReportReserve ||
			got.concurrency != defaults.SecondaryNetworkProbeConcurrency || got.podLogs == nil {

			t.Errorf("defaultAttachmentCheckConfig() = %+v, want the pkg/defaults budget and a pod-log reader", got)
		}
	})
}

// TestDeploymentChecksCoverCatalog proves every deployment-phase catalog
// entry's check name dispatches to a check in this binary.
func TestDeploymentChecksCoverCatalog(t *testing.T) {
	t.Parallel()
	cat, err := catalog.LoadWithDataProvider(context.Background(), nil, "", "")
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	checks := deploymentChecks()
	var found int
	for _, entry := range cat.Validators {
		if entry.Phase != string(v1.PhaseDeployment) {
			continue
		}
		found++
		if len(entry.Args) == 0 {
			t.Errorf("catalog entry %s has no args", entry.Name)
			continue
		}
		if _, ok := checks[entry.Args[0]]; !ok {
			t.Errorf("catalog entry %s dispatches %q, which the deployment binary does not register", entry.Name, entry.Args[0])
		}
	}
	if found == 0 {
		t.Fatal("catalog has no deployment-phase entries")
	}
}

// TestCheckSecondaryNetworkAttachment_ShortBudgetKeepsSandboxMessage proves
// the probe wait leaves the report reserve unused: with a check budget shorter
// than the probe timeout plus the reserve, the stuck probe's kubelet event is
// still read and reported.
func TestCheckSecondaryNetworkAttachment_ShortBudgetKeepsSandboxMessage(t *testing.T) {
	stamp := func(pod *corev1.Pod) []runtime.Object {
		pod.Status.Phase = corev1.PodPending
		return []runtime.Object{&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: pod.Name + ".sandbox", Namespace: pod.Namespace},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: pod.Name, Namespace: pod.Namespace},
			Reason:         eventReasonFailedCreatePodSandBox,
			Message:        "multus: error getting pod: Unauthorized",
			LastTimestamp:  metav1.Now(),
		}}
	}
	cluster := newAttachmentCluster(t, stamp, attachmentNode("gpu-a", map[string]int64{testMlnxNICs: 4}))
	ctx := attachmentContextWithin(t, cluster.client, gb300OKERefs, 2*time.Second)
	cfg := attachmentCheckConfig{probeTimeout: 10 * time.Second, reportReserve: time.Second, concurrency: 1}

	_, err := captureExtras(t, func() error { return runSecondaryNetworkAttachment(ctx, cfg) })
	if err == nil || !strings.Contains(err.Error(), "FailedCreatePodSandBox: multus: error getting pod: Unauthorized") {
		t.Fatalf("error = %v, want the sandbox failure message", err)
	}
	// A real apiserver refuses the events List on a dead context, which the
	// fake does not; the check finishing inside its own budget is what
	// proves the wait left the reserve.
	if ctx.Ctx.Err() != nil {
		t.Errorf("the check ran to its own deadline (%v): the probe wait did not leave the report reserve", ctx.Ctx.Err())
	}
}

// TestCheckSecondaryNetworkAttachment_CleanupFailureFailsPass proves a probe
// namespace that could not be deleted turns a pass into a failure: a leaked
// probe may hold a VF.
func TestCheckSecondaryNetworkAttachment_CleanupFailureFailsPass(t *testing.T) {
	cluster := newAttachmentCluster(t, attachedStatus(""), attachmentNode("gpu-a", map[string]int64{testMlnxNICs: 4}))
	cluster.client.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "probe", fmt.Errorf("denied"))
	})
	ctx := attachmentContext(t, cluster.client, gb300OKERefs)

	_, err := captureExtras(t, func() error { return runSecondaryNetworkAttachment(ctx, cluster.config(testAttachmentConfig)) })
	if err == nil || validators.IsSkip(err) || !strings.Contains(err.Error(), "CLEANUP FAILED") {
		t.Fatalf("error = %v, want a CLEANUP FAILED failure", err)
	}
}

// TestCheckSecondaryNetworkAttachment_PartialBusyPasses proves busy rails are
// disclosed as skipped while the free ones are still probed and pass.
func TestCheckSecondaryNetworkAttachment_PartialBusyPasses(t *testing.T) {
	objects := []runtime.Object{
		attachmentNode("vr-a", railAllocatable(8, 1)),
		workloadPod("train-6", "vr-a", railResource(6), 1),
		workloadPod("train-7", "vr-a", railResource(7), 1),
	}
	cluster := newAttachmentCluster(t, perRailStatus("default/roce-network-rail-"), objects...)
	ctx := attachmentContext(t, cluster.client, vr200NVKERefs)

	out, extra, err := captureOutput(t, func() error { return runSecondaryNetworkAttachment(ctx, cluster.config(testAttachmentConfig)) })
	if err != nil {
		t.Fatalf("error = %v, want pass", err)
	}
	assertExtraSubset(t, extra, map[string]string{
		validators.ExtraKeyNodesValidated: "6", validators.ExtraKeyNodesSkipped: "2",
		validators.ExtraKeyNodesFailed: "0", validators.ExtraKeyNodesTotal: "8",
	})
	if got := len(cluster.createdProbes()); got != 6 {
		t.Errorf("created %d probe pods, want 6", got)
	}
	for _, want := range []string{
		"RESULT: default/roce-network-rail-6: SKIPPED (busy: every unit of " + railResource(6),
		"RESULT: default/roce-network-rail-7: SKIPPED (busy: every unit of " + railResource(7),
		"RESULT: default/roce-network-rail-0: PASSED on node vr-a (interface net1, ips 10.255.0.12",
		"RESULT: attachmentsValidated: 6/8 (passed 6, failed 0, skipped 2: busy 2, unschedulable 0)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
}

// TestPlanAttachmentProbes covers node selection: the first Ready, uncordoned
// node by name with a free unit, cordoned, NotReady and KWOK nodes excluded,
// NotReady uncordoned candidates failing where cordoned ones skip, units
// reserved across networks that share a resource, and a network with no
// resource placed on a GPU node.
func TestPlanAttachmentProbes(t *testing.T) {
	t.Parallel()

	shared := func(name string) secondaryNetwork {
		return secondaryNetwork{Namespace: "ns", Name: name, Resource: testMlnxNICs, Kind: attachmentKindNAD}
	}

	gpuTaint := func(n *corev1.Node) {
		n.Spec.Taints = []corev1.Taint{{Key: "dedicated", Value: "training", Effect: corev1.TaintEffectNoSchedule}}
	}
	coveredAndPreferTaints := func(n *corev1.Node) {
		n.Spec.Taints = []corev1.Taint{
			{Key: "dedicated", Value: "training", Effect: corev1.TaintEffectNoSchedule},
			{Key: "soft", Effect: corev1.TaintEffectPreferNoSchedule},
		}
	}
	onlyGPUOperator := []corev1.Toleration{{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists}}

	tests := []struct {
		name         string
		nodes        []*corev1.Node
		pods         []runtime.Object
		networks     []secondaryNetwork
		tolerations  []corev1.Toleration
		ownPods      []runtime.Object
		wantProbes   map[string]string
		wantResolved map[string]attachmentStatus
		wantDeferred []string
	}{
		{
			name:       "the default tolerate-all probe may use a tainted node",
			nodes:      []*corev1.Node{attachmentNode("a", map[string]int64{testMlnxNICs: 1}, gpuTaint), attachmentNode("b", map[string]int64{testMlnxNICs: 1})},
			networks:   []secondaryNetwork{shared("n1")},
			wantProbes: map[string]string{"ns/n1": "a"},
		},
		{
			name:        "a toleration override passes over a node whose taint it does not cover",
			nodes:       []*corev1.Node{attachmentNode("a", map[string]int64{testMlnxNICs: 1}, gpuTaint), attachmentNode("b", map[string]int64{testMlnxNICs: 1})},
			networks:    []secondaryNetwork{shared("n1")},
			tolerations: onlyGPUOperator,
			wantProbes:  map[string]string{"ns/n1": "b"},
		},
		{
			name:        "a covered or PreferNoSchedule taint does not exclude a node",
			nodes:       []*corev1.Node{attachmentNode("a", map[string]int64{testMlnxNICs: 1}, coveredAndPreferTaints), attachmentNode("b", map[string]int64{testMlnxNICs: 1})},
			networks:    []secondaryNetwork{shared("n1")},
			tolerations: []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "training"}},
			wantProbes:  map[string]string{"ns/n1": "a"},
		},
		{
			name:         "every candidate excluded by the run's tolerations skips",
			nodes:        []*corev1.Node{attachmentNode("a", map[string]int64{testMlnxNICs: 1}, gpuTaint)},
			networks:     []secondaryNetwork{shared("n1")},
			tolerations:  onlyGPUOperator,
			wantResolved: map[string]attachmentStatus{"ns/n1": attachmentSkippedUnschedulable},
		},
		{
			name: "a NotReady candidate still fails when the others are excluded by tolerations",
			nodes: []*corev1.Node{
				attachmentNode("a", map[string]int64{testMlnxNICs: 1}, gpuTaint),
				attachmentNode("b", map[string]int64{testMlnxNICs: 1}, notReady),
			},
			networks:     []secondaryNetwork{shared("n1")},
			tolerations:  onlyGPUOperator,
			wantResolved: map[string]attachmentStatus{"ns/n1": attachmentFailed},
		},
		{
			name:       "first node by name with a free unit",
			nodes:      []*corev1.Node{attachmentNode("b", map[string]int64{testMlnxNICs: 1}), attachmentNode("a", map[string]int64{testMlnxNICs: 1})},
			networks:   []secondaryNetwork{shared("n1")},
			wantProbes: map[string]string{"ns/n1": "a"},
		},
		{
			name:       "a fully requested node is passed over",
			nodes:      []*corev1.Node{attachmentNode("a", map[string]int64{testMlnxNICs: 2}), attachmentNode("b", map[string]int64{testMlnxNICs: 2})},
			pods:       []runtime.Object{workloadPod("w", "a", testMlnxNICs, 2)},
			networks:   []secondaryNetwork{shared("n1")},
			wantProbes: map[string]string{"ns/n1": "b"},
		},
		{
			name:         "all busy",
			nodes:        []*corev1.Node{attachmentNode("a", map[string]int64{testMlnxNICs: 1})},
			pods:         []runtime.Object{workloadPod("w", "a", testMlnxNICs, 1)},
			networks:     []secondaryNetwork{shared("n1")},
			wantResolved: map[string]attachmentStatus{"ns/n1": attachmentSkippedBusy},
		},
		{
			name: "cordoned and NotReady nodes are excluded",
			nodes: []*corev1.Node{
				attachmentNode("a", map[string]int64{testMlnxNICs: 1}, cordoned),
				attachmentNode("b", map[string]int64{testMlnxNICs: 1}, notReady),
				attachmentNode("c", map[string]int64{testMlnxNICs: 1}),
			},
			networks:   []secondaryNetwork{shared("n1")},
			wantProbes: map[string]string{"ns/n1": "c"},
		},
		{
			name: "a Ready node is preferred over an earlier NotReady one",
			nodes: []*corev1.Node{
				attachmentNode("a", map[string]int64{testMlnxNICs: 1}, notReady),
				attachmentNode("b", map[string]int64{testMlnxNICs: 1}),
			},
			networks:   []secondaryNetwork{shared("n1")},
			wantProbes: map[string]string{"ns/n1": "b"},
		},
		{
			name: "cordoned and NotReady candidates fail on the NotReady one",
			nodes: []*corev1.Node{
				attachmentNode("a", map[string]int64{testMlnxNICs: 1}, cordoned),
				attachmentNode("b", map[string]int64{testMlnxNICs: 1}, notReady),
			},
			networks:     []secondaryNetwork{shared("n1")},
			wantResolved: map[string]attachmentStatus{"ns/n1": attachmentFailed},
		},
		{
			name:         "a cordoned NotReady candidate is cordoned, not a fault",
			nodes:        []*corev1.Node{attachmentNode("a", map[string]int64{testMlnxNICs: 1}, cordoned, notReady)},
			networks:     []secondaryNetwork{shared("n1")},
			wantResolved: map[string]attachmentStatus{"ns/n1": attachmentSkippedUnschedulable},
		},
		{
			name:         "only cordoned candidates skip",
			nodes:        []*corev1.Node{attachmentNode("a", map[string]int64{testMlnxNICs: 1}, cordoned)},
			networks:     []secondaryNetwork{shared("n1")},
			wantResolved: map[string]attachmentStatus{"ns/n1": attachmentSkippedUnschedulable},
		},
		{
			name:         "KWOK node is never a candidate",
			nodes:        []*corev1.Node{attachmentNode("a", map[string]int64{testMlnxNICs: 1}, simulated)},
			networks:     []secondaryNetwork{shared("n1")},
			wantResolved: map[string]attachmentStatus{"ns/n1": attachmentFailed},
		},
		{
			name:         "a network waiting only on this run's reservation is deferred, not busy",
			nodes:        []*corev1.Node{attachmentNode("a", map[string]int64{testMlnxNICs: 1}), attachmentNode("b", map[string]int64{testMlnxNICs: 1})},
			networks:     []secondaryNetwork{shared("n1"), shared("n2"), shared("n3")},
			wantProbes:   map[string]string{"ns/n1": "a", "ns/n2": "b"},
			wantDeferred: []string{"ns/n3"},
		},
		{
			name:         "a unit held by this run's unfinished probe pod defers the network",
			nodes:        []*corev1.Node{attachmentNode("a", map[string]int64{testMlnxNICs: 1})},
			ownPods:      []runtime.Object{ownProbePod("probe-0", "a", testMlnxNICs)},
			networks:     []secondaryNetwork{shared("n1")},
			wantDeferred: []string{"ns/n1"},
		},
		{
			name: "units held by workloads and by this run: busy only where workloads hold them all",
			nodes: []*corev1.Node{
				attachmentNode("a", map[string]int64{testMlnxNICs: 1}),
				attachmentNode("b", map[string]int64{testMlnxNICs: 1}),
			},
			pods:         []runtime.Object{workloadPod("w", "a", testMlnxNICs, 1)},
			ownPods:      []runtime.Object{ownProbePod("probe-0", "b", testMlnxNICs)},
			networks:     []secondaryNetwork{shared("n1")},
			wantDeferred: []string{"ns/n1"},
		},
		{
			name: "network without a resource goes to a GPU node",
			nodes: []*corev1.Node{
				attachmentNode("a", nil, func(n *corev1.Node) { delete(n.Status.Allocatable, corev1.ResourceName(helper.GpuResourceName)) }),
				attachmentNode("b", nil),
			},
			networks:   []secondaryNetwork{{Namespace: "ns", Name: "plain", Kind: attachmentKindNAD}},
			wantProbes: map[string]string{"ns/plain": "b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			objects := append([]runtime.Object(nil), tt.pods...)
			nodes := make([]corev1.Node, 0, len(tt.nodes))
			for _, n := range tt.nodes {
				nodes = append(nodes, *n)
			}
			objects = append(objects, tt.ownPods...)
			ctx := attachmentContext(t, k8sfake.NewClientset(objects...), nil)
			ctx.Tolerations = tt.tolerations
			plan, err := planAttachmentProbes(ctx, nodes, tt.networks, testOwnNamespace)
			if err != nil {
				t.Fatalf("planAttachmentProbes() error = %v", err)
			}
			gotProbes := map[string]string{}
			for _, p := range plan.probes {
				gotProbes[p.network.key()] = p.node
			}
			gotResolved := map[string]attachmentStatus{}
			for _, o := range plan.resolved {
				gotResolved[o.network.key()] = o.status
			}
			if fmt.Sprint(gotProbes) != fmt.Sprint(nilIfEmpty(tt.wantProbes)) {
				t.Errorf("probes = %v, want %v", gotProbes, tt.wantProbes)
			}
			if fmt.Sprint(gotResolved) != fmt.Sprint(nilIfEmptyStatus(tt.wantResolved)) {
				t.Errorf("resolved = %v, want %v", gotResolved, tt.wantResolved)
			}
			if got := networkKeys(plan.deferred); strings.Join(got, ",") != strings.Join(tt.wantDeferred, ",") {
				t.Errorf("deferred = %v, want %v", got, tt.wantDeferred)
			}
		})
	}
}

// testOwnNamespace is the probe namespace the planning tests treat as this
// run's.
const testOwnNamespace = "aicr-gpu-test-own"

// ownProbePod is an unfinished probe pod of this run on node holding one unit
// of resourceName.
func ownProbePod(name, node, resourceName string) *corev1.Pod {
	pod := workloadPod(name, node, resourceName, 1)
	pod.Namespace = testOwnNamespace
	return pod
}

func nilIfEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func nilIfEmptyStatus(m map[string]attachmentStatus) map[string]attachmentStatus {
	if m == nil {
		return map[string]attachmentStatus{}
	}
	return m
}

// TestPlanAttachmentProbes_OccupancyErrorFailsClosed proves a node whose pod
// requests cannot be read is an error, never "busy".
func TestPlanAttachmentProbes_OccupancyErrorFailsClosed(t *testing.T) {
	t.Parallel()
	client := k8sfake.NewClientset()
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewInternalError(fmt.Errorf("etcd unavailable"))
	})
	ctx := attachmentContext(t, client, nil)
	_, err := planAttachmentProbes(ctx,
		[]corev1.Node{*attachmentNode("a", map[string]int64{testMlnxNICs: 1})},
		[]secondaryNetwork{{Namespace: "ns", Name: "n", Resource: testMlnxNICs}}, "")
	if err == nil || validators.IsSkip(err) || !strings.Contains(err.Error(), "failed to list pods on node a") {
		t.Fatalf("planAttachmentProbes() error = %v, want the occupancy failure", err)
	}
}

// TestBuildAttachmentProbePod pins the probe's shape: the network annotation,
// one unit of the resource, placement by the scheduler through a node
// affinity rather than a nodeName bind, no API token, tolerate-all unless
// overridden, and a restricted security context.
func TestBuildAttachmentProbePod(t *testing.T) {
	t.Parallel()
	ns := &probeNamespace{token: "tok", name: "aicr-gpu-test-tok", imagePullSecrets: []string{"mirror"}}
	p := probeAssignment{network: rail0, node: "gpu-a"}

	pod := buildAttachmentProbePod(ns, p, nil)
	if got := pod.Annotations[multusNetworksAnnotation]; got != "default/sriov-network-rail-0" {
		t.Errorf("networks annotation = %q", got)
	}
	res := corev1.ResourceName(rail0.Resource)
	c := pod.Spec.Containers[0]
	if q := c.Resources.Limits[res]; q.Value() != 1 {
		t.Errorf("limit = %s, want 1", q.String())
	}
	if q := c.Resources.Requests[res]; q.Value() != 1 {
		t.Errorf("request = %s, want 1", q.String())
	}
	if probeNode(pod) != "gpu-a" || pod.Spec.NodeName != "" || pod.Namespace != ns.name || pod.GenerateName != attachmentProbeGenerateName {
		t.Errorf("placement = affinity %q nodeName %q namespace %q generateName %q",
			probeNode(pod), pod.Spec.NodeName, pod.Namespace, pod.GenerateName)
	}
	if len(pod.Spec.Tolerations) != 1 || pod.Spec.Tolerations[0].Operator != corev1.TolerationOpExists {
		t.Errorf("tolerations = %v, want tolerate-all", pod.Spec.Tolerations)
	}
	if len(pod.Spec.ImagePullSecrets) != 1 || pod.Spec.ImagePullSecrets[0].Name != "mirror" {
		t.Errorf("imagePullSecrets = %v", pod.Spec.ImagePullSecrets)
	}
	if c.Image != defaults.ProbeImage || c.Command[len(c.Command)-1] != attachmentProbeScript {
		t.Errorf("container image/command = %s %v", c.Image, c.Command)
	}
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.RunAsNonRoot == nil || !*pod.Spec.SecurityContext.RunAsNonRoot ||
		c.SecurityContext == nil || c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {

		t.Errorf("security context is not restricted: pod %+v container %+v", pod.Spec.SecurityContext, c.SecurityContext)
	}

	override := []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "gpu"}}
	if got := buildAttachmentProbePod(ns, p, override).Spec.Tolerations; len(got) != 1 || got[0].Key != "dedicated" {
		t.Errorf("override tolerations = %v", got)
	}
	noResource := buildAttachmentProbePod(ns, probeAssignment{network: secondaryNetwork{Namespace: "a", Name: "b"}, node: "n"}, nil)
	if len(noResource.Spec.Containers[0].Resources.Limits) != 0 {
		t.Errorf("a network without a resource requests %v", noResource.Spec.Containers[0].Resources.Limits)
	}
}

// TestWithProbeCleanup pins how a cleanup failure combines with the result.
func TestWithProbeCleanup(t *testing.T) {
	cleanupErr := fmt.Errorf("cleanup")
	failure := fmt.Errorf("probe failed")
	skip := validators.Skip("busy")

	tests := []struct {
		name    string
		result  error
		cleanup error
		want    error
	}{
		{"pass, clean", nil, nil, nil},
		{"pass, cleanup failed", nil, cleanupErr, cleanupErr},
		{"skip, cleanup failed", skip, cleanupErr, cleanupErr},
		{"failure keeps its error", failure, cleanupErr, failure},
		{"skip, clean", skip, nil, skip},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := withProbeCleanup(tt.result, tt.cleanup); got != tt.want { //nolint:errorlint // identity is the assertion
				t.Errorf("withProbeCleanup() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestStuckProbeDetail covers the stuck-probe explanation: the latest
// FailedCreatePodSandBox event of this pod while it waits for its sandbox,
// and otherwise the pod's state and the wait error.
func TestStuckProbeDetail(t *testing.T) {
	t.Parallel()

	sandboxEvent := func(name, pod, msg string, at time.Time) *corev1.Event {
		return &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: "probe-ns"},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: pod},
			Reason:         eventReasonFailedCreatePodSandBox,
			Message:        msg,
			LastTimestamp:  metav1.NewTime(at),
		}
	}
	now := time.Now()
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: "probe-ns"},
		Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
			Name: attachmentProbeContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
		}}},
	}
	running := pending.DeepCopy()
	running.Status.Phase = corev1.PodRunning
	waitErr := fmt.Errorf("deadline exceeded")

	tests := []struct {
		name    string
		events  []runtime.Object
		pod     *corev1.Pod
		want    string
		notWant string
	}{
		{
			name: "latest sandbox failure of this pod wins",
			events: []runtime.Object{
				sandboxEvent("old", "probe", "network not ready", now.Add(-time.Minute)),
				sandboxEvent("new", "probe", "multus: Unauthorized", now),
				sandboxEvent("other", "another-pod", "someone else's failure", now.Add(time.Minute)),
			},
			pod:  pending,
			want: "never started: FailedCreatePodSandBox: multus: Unauthorized",
		},
		{
			name: "a newer non-sandbox event is not quoted",
			events: []runtime.Object{
				sandboxEvent("sandbox", "probe", "multus: Unauthorized", now.Add(-time.Minute)),
				func() *corev1.Event {
					ev := sandboxEvent("scheduled", "probe", "Successfully assigned probe-ns/probe to gpu-a", now)
					ev.Reason = "Scheduled"
					return ev
				}(),
			},
			pod:     pending,
			want:    "FailedCreatePodSandBox: multus: Unauthorized",
			notWant: "Successfully assigned",
		},
		{
			name: "no event reports the pod state and the wait error",
			pod:  pending,
			want: "did not finish (phase Pending, container probe waiting: ContainerCreating): deadline exceeded",
		},
		{
			name:    "a started pod ignores stale sandbox events",
			events:  []runtime.Object{sandboxEvent("old", "probe", "multus: Unauthorized", now)},
			pod:     running,
			want:    "did not finish (phase Running",
			notWant: "Unauthorized",
		},
		{
			name: "never observed",
			want: "never observed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := attachmentContext(t, k8sfake.NewClientset(tt.events...), nil)
			got := stuckProbeDetail(ctx, "probe-ns", "probe", tt.pod, waitErr)
			if !strings.Contains(got, tt.want) {
				t.Errorf("stuckProbeDetail() = %q, want substring %q", got, tt.want)
			}
			if tt.notWant != "" && strings.Contains(got, tt.notWant) {
				t.Errorf("stuckProbeDetail() = %q, must not contain %q", got, tt.notWant)
			}
		})
	}
}

func TestAttachmentOutcomeResultLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		outcome attachmentOutcome
		want    string
	}{
		{"passed", attachmentOutcome{network: rail0, node: "gpu-a", status: attachmentPassed, detail: "interface net1"},
			"RESULT: default/sriov-network-rail-0: PASSED on node gpu-a (interface net1)"},
		{"failed on a node", attachmentOutcome{network: rail0, node: "gpu-a", status: attachmentFailed, detail: "no entry"},
			"RESULT: default/sriov-network-rail-0: FAILED on node gpu-a (no entry)"},
		{"failed before a probe", attachmentOutcome{network: rail0, status: attachmentFailed, detail: "not advertised"},
			"RESULT: default/sriov-network-rail-0: FAILED (not advertised)"},
		{"busy", attachmentOutcome{network: rail0, node: "gpu-a", status: attachmentSkippedBusy, detail: "all requested"},
			"RESULT: default/sriov-network-rail-0: SKIPPED (busy: all requested)"},
		{"unschedulable", attachmentOutcome{network: rail0, status: attachmentSkippedUnschedulable, detail: "all cordoned"},
			"RESULT: default/sriov-network-rail-0: SKIPPED (unschedulable: all cordoned)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.outcome.resultLine(); got != tt.want {
				t.Errorf("resultLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAttachmentCoverageLine(t *testing.T) {
	t.Parallel()
	c := attachmentCoverage{total: 8, passed: 4, failed: 1, busy: 2, unschedulable: 1}
	if got, want := c.line(), "RESULT: attachmentsValidated: 4/8 (passed 4, failed 1, skipped 3: busy 2, unschedulable 1)"; got != want {
		t.Errorf("line() = %q, want %q", got, want)
	}
	if got, want := noNetworkProbedReason(c), "no secondary network could be probed (2 busy, 1 unschedulable)"; got != want {
		t.Errorf("noNetworkProbedReason() = %q, want %q", got, want)
	}
}

// TestWaitForProbePod drives the probe wait through a fake watch: a terminal
// phase delivered by the watch, a watch that closes after the pod finished
// (the re-Get finds it), a Get error while the context is alive, and a pod
// the scheduler leaves Unschedulable for lack of a unit, which the wait
// returns at its deadline for probeUnschedulable to read as busy.
func TestWaitForProbePod(t *testing.T) {
	t.Parallel()
	const ns, name = "probe-ns", "probe"
	pending := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Status: corev1.PodStatus{Phase: corev1.PodPending}}
	withPhase := func(phase corev1.PodPhase) *corev1.Pod {
		p := pending.DeepCopy()
		p.Status.Phase = phase
		return p
	}

	t.Run("terminal phase delivered by the watch", func(t *testing.T) {
		t.Parallel()
		client := k8sfake.NewClientset(pending.DeepCopy())
		fw := watch.NewFake()
		client.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) { return true, fw, nil })
		go func() {
			fw.Modify(withPhase(corev1.PodRunning))
			fw.Modify(withPhase(corev1.PodSucceeded))
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pod, err := waitForProbePod(ctx, client, ns, name)
		if err != nil || pod.Status.Phase != corev1.PodSucceeded {
			t.Fatalf("waitForProbePod() = %v, %v; want Succeeded", pod, err)
		}
	})

	t.Run("watch closed after the pod finished is re-read", func(t *testing.T) {
		t.Parallel()
		client := k8sfake.NewClientset(pending.DeepCopy())
		// The pod finishes while the watch is being opened, and the watch
		// closes without delivering it: only the re-Get can see it.
		client.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
			if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), withPhase(corev1.PodSucceeded), ns); err != nil {
				return true, nil, err
			}
			fw := watch.NewFake()
			fw.Stop()
			return true, fw, nil
		})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pod, err := waitForProbePod(ctx, client, ns, name)
		if err != nil || pod.Status.Phase != corev1.PodSucceeded {
			t.Fatalf("waitForProbePod() = %v, %v; want Succeeded from the re-Get", pod, err)
		}
	})

	t.Run("get error with a live context is internal", func(t *testing.T) {
		t.Parallel()
		client := k8sfake.NewClientset()
		client.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, k8serrors.NewInternalError(fmt.Errorf("etcd unavailable"))
		})
		_, err := waitForProbePod(context.Background(), client, ns, name)
		errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
	})

	t.Run("unschedulable for lack of a unit is busy at the deadline", func(t *testing.T) {
		t.Parallel()
		stuck := pending.DeepCopy()
		stuck.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
			Reason: corev1.PodReasonUnschedulable, Message: "0/3 nodes are available: 1 Insufficient " + testMlnxNICs + "."}}
		client := k8sfake.NewClientset(pending.DeepCopy())
		fw := watch.NewFake()
		client.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) { return true, fw, nil })
		go fw.Modify(stuck)
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		pod, err := waitForProbePod(ctx, client, ns, name)
		errorstest.WantReportedCode(t, err, errors.ErrCodeTimeout)
		insufficient, message, unscheduled := probeUnschedulable(pod, testMlnxNICs)
		if !unscheduled || !insufficient || !strings.Contains(message, "Insufficient "+testMlnxNICs) {
			t.Errorf("probeUnschedulable() = %v, %q, %v; want an insufficient-unit verdict", insufficient, message, unscheduled)
		}
		if insufficient, _, _ := probeUnschedulable(pod, railResource(0)); insufficient {
			t.Error("probeUnschedulable() read another resource's shortage as this network's")
		}
	})
}

// TestProbeAttachmentStartingAfterDeadline proves a probe queued past the
// shared deadline fails without creating a pod, so it cannot spend the report
// reserve.
func TestProbeAttachmentStartingAfterDeadline(t *testing.T) {
	t.Parallel()
	client := k8sfake.NewClientset()
	ctx := attachmentContext(t, client, nil)
	ns := &probeNamespace{token: "tok", name: "aicr-gpu-test-tok"}
	out := probeAttachment(ctx, nil, ns, probeAssignment{network: rail0, node: "gpu-a"}, time.Now().Add(-time.Second), rail0Pools, nil)
	if out.status != attachmentFailed || !strings.Contains(out.detail, "the probe budget ran out before this probe could start") {
		t.Errorf("outcome = %+v, want a failure naming the spent budget", out)
	}
	pods, err := client.CoreV1().Pods(ns.name).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 0 {
		t.Errorf("created %d probe pod(s) after the deadline, want none", len(pods.Items))
	}
}

// TestProbeNetworksProbesDeferredNetworkAfterOwnRelease drives two networks
// sharing a resource with one unit: the first is probed, the second waits on
// this run's own reservation, and is probed in the next wave once the first
// probe has finished, instead of being reported as held by other pods.
func TestProbeNetworksProbesDeferredNetworkAfterOwnRelease(t *testing.T) {
	shared := func(name string) secondaryNetwork {
		return secondaryNetwork{Namespace: "ns", Name: name, Resource: testMlnxNICs, Kind: attachmentKindNAD}
	}
	node := attachmentNode("gpu-a", map[string]int64{testMlnxNICs: 1})
	cluster := newAttachmentCluster(t, attachedStatus(""), node)
	ctx := attachmentContext(t, cluster.client, nil)
	networks := []secondaryNetwork{shared("n1"), shared("n2")}

	plan, err := planAttachmentProbes(ctx, []corev1.Node{*node}, networks, "")
	if err != nil {
		t.Fatalf("planAttachmentProbes() error = %v", err)
	}
	if len(plan.probes) != 1 || len(plan.deferred) != 1 {
		t.Fatalf("plan = %d probe(s), %d deferred; want 1 and 1", len(plan.probes), len(plan.deferred))
	}
	ns := testProbeNamespace(t)
	t.Cleanup(func() { _ = ns.delete(cluster.client) })

	extra, err := captureExtras(t, func() error {
		return probeNetworks(ctx, cluster.config(testAttachmentConfig), ns, []corev1.Node{*node}, plan, nil, nil)
	})
	if err != nil {
		t.Fatalf("probeNetworks() error = %v, want both networks attached", err)
	}
	assertExtraSubset(t, extra, map[string]string{validators.ExtraKeyNodesValidated: "2", validators.ExtraKeyNodesSkipped: "0"})
	if got := len(cluster.createdProbes()); got != 2 {
		t.Errorf("created %d probe pods, want one per network across two waves", got)
	}
}

// TestHeldByRunOutcomesNameTheirCause pins that a network left waiting on
// this run's own probes is never described as held by other pods.
func TestHeldByRunOutcomesNameTheirCause(t *testing.T) {
	t.Parallel()
	out := heldByRunOutcomes([]secondaryNetwork{rail0}, "the probe budget ran out before it was released")
	if len(out) != 1 || out[0].status != attachmentSkippedBusy ||
		!strings.Contains(out[0].detail, "held by this run's probe of another network") ||
		strings.Contains(out[0].detail, "requested by other pods") {

		t.Errorf("heldByRunOutcomes() = %+v", out)
	}
}

// TestAwaitProbeRereadsAtDeadline proves the verdict at the probe deadline
// rests on a fresh read, not on a state a silent watch left stale: a pod last
// seen Unschedulable that has since finished is judged finished, and a fresh
// read that fails is a failure, never busy.
func TestAwaitProbeRereadsAtDeadline(t *testing.T) {
	t.Parallel()
	const ns, name = "probe-ns", "probe"
	stale := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
			Message: "0/1 nodes are available: 1 Insufficient " + testMlnxNICs + ".",
		}}},
	}
	finished := stale.DeepCopy()
	finished.Status = corev1.PodStatus{Phase: corev1.PodSucceeded}
	out := attachmentOutcome{network: okeRdmaNet, node: "gpu-a"}

	newClient := func(laterGet func() (runtime.Object, error)) *k8sfake.Clientset {
		client := k8sfake.NewClientset(stale.DeepCopy())
		client.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
			return true, watch.NewFake(), nil
		})
		var gets int
		client.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			gets++
			if gets == 1 {
				return true, stale.DeepCopy(), nil
			}
			obj, err := laterGet()
			return true, obj, err
		})
		return client
	}

	t.Run("finished since the last event", func(t *testing.T) {
		t.Parallel()
		ctx := attachmentContext(t, newClient(func() (runtime.Object, error) { return finished.DeepCopy(), nil }), nil)
		pod, verdict, decided := awaitProbe(ctx, out, ns, name, time.Now().Add(300*time.Millisecond))
		if decided || pod == nil || pod.Status.Phase != corev1.PodSucceeded {
			t.Errorf("awaitProbe() = %v, %+v, %v; want the finished pod to be judged", pod, verdict, decided)
		}
	})
	t.Run("fresh read fails", func(t *testing.T) {
		t.Parallel()
		ctx := attachmentContext(t, newClient(func() (runtime.Object, error) {
			return nil, k8serrors.NewServiceUnavailable("apiserver restarting")
		}), nil)
		_, verdict, decided := awaitProbe(ctx, out, ns, name, time.Now().Add(300*time.Millisecond))
		if !decided || verdict.status != attachmentFailed || !strings.Contains(verdict.detail, "could not be read again") {
			t.Errorf("awaitProbe() verdict = %+v, decided %v; want a failure, never busy", verdict, decided)
		}
	})
}
