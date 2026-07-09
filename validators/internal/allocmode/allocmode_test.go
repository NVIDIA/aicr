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

package allocmode

import (
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

// withDRAAPIDiscoveryAt registers the DRA API resources under an explicit
// group-version (e.g. resource.k8s.io/v1beta2 for beta-only cluster tests).
// Duplicated from validators/conformance (test helpers cannot be imported
// across packages) — keep the two copies in sync.
func withDRAAPIDiscoveryAt(t *testing.T, clientset *k8sfake.Clientset, groupVersion string) {
	t.Helper()
	fakeDisc, ok := clientset.Discovery().(*fakediscovery.FakeDiscovery)
	if !ok {
		t.Fatalf("discovery client = %T, want *fakediscovery.FakeDiscovery", clientset.Discovery())
	}
	fakeDisc.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: groupVersion,
			APIResources: []metav1.APIResource{
				{Name: "resourceclaims", Kind: "ResourceClaim", Namespaced: true},
				{Name: "resourceslices", Kind: "ResourceSlice", Namespaced: false},
				{Name: "deviceclasses", Kind: "DeviceClass", Namespaced: false},
			},
		},
	}
}

// withDRAAPIDiscovery registers the DRA API resources at the GA
// group-version (resource.k8s.io/v1).
func withDRAAPIDiscovery(t *testing.T, clientset *k8sfake.Clientset) {
	t.Helper()
	withDRAAPIDiscoveryAt(t, clientset, draAPIGroupVersion)
}

// newDRAFakeDynamicClient builds a fake dynamic client that knows the
// resource.k8s.io/v1 list kinds used by the DRA checks and probe.
func newDRAFakeDynamicClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return newDRAFakeDynamicClientAt("v1", objects...)
}

// newDRAFakeDynamicClientAt builds a fake dynamic client that knows the
// resource.k8s.io list kinds at an explicit served version (v1, v1beta2, or
// v1beta1) — for beta-only cluster tests.
func newDRAFakeDynamicClientAt(version string, objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			GVRAt(version, "deviceclasses"):  "DeviceClassList",
			GVRAt(version, "resourceslices"): "ResourceSliceList",
			GVRAt(version, "resourceclaims"): "ResourceClaimList",
		}, objects...)
}

func testDeviceClass(name string) *unstructured.Unstructured {
	return testDeviceClassAt(draAPIGroupVersion, name)
}

// testDeviceClassAt builds a DeviceClass at an explicit apiVersion
// (e.g. resource.k8s.io/v1beta2 for beta-only cluster tests).
func testDeviceClassAt(apiVersion, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": apiVersion,
		"kind":       "DeviceClass",
		"metadata":   map[string]interface{}{"name": name},
	}}
}

// testDeviceClassWithExtendedResource builds a DeviceClass carrying a
// KEP-5004 spec.extendedResourceName mapping.
func testDeviceClassWithExtendedResource(name, extendedResourceName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": draAPIGroupVersion,
		"kind":       "DeviceClass",
		"metadata":   map[string]interface{}{"name": name},
		"spec":       map[string]interface{}{"extendedResourceName": extendedResourceName},
	}}
}

// testResourceSlice builds a resource.k8s.io/v1 ResourceSlice. topo carries
// the slice-level topology fields (nodeName / allNodes / nodeSelector /
// perDeviceNodeSelection).
func testResourceSlice(name, driver, pool string, gen, count int64, topo map[string]interface{}, devices []interface{}) *unstructured.Unstructured {
	return testResourceSliceAt(draAPIGroupVersion, name, driver, pool, gen, count, topo, devices)
}

// testResourceSliceAt builds a ResourceSlice at an explicit apiVersion
// (e.g. resource.k8s.io/v1beta2 for beta-only cluster tests). The validated
// spec fields are structurally identical across v1beta1/v1beta2/v1.
func testResourceSliceAt(apiVersion, name, driver, pool string, gen, count int64, topo map[string]interface{}, devices []interface{}) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"driver": driver,
		"pool": map[string]interface{}{
			"name":               pool,
			"generation":         gen,
			"resourceSliceCount": count,
		},
		"devices": devices,
	}
	for k, v := range topo {
		spec[k] = v
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": apiVersion,
		"kind":       "ResourceSlice",
		"metadata":   map[string]interface{}{"name": name},
		"spec":       spec,
	}}
}

func plainDevice(name string) map[string]interface{} {
	return map[string]interface{}{"name": name}
}

//nolint:unparam // signature kept in sync with the conformance fixtures copy
func taintedDevice(name, effect string) map[string]interface{} {
	return map[string]interface{}{
		"name": name,
		"taints": []interface{}{
			map[string]interface{}{"key": "nvidia.com/gpu", "effect": effect},
		},
	}
}

// basicWrappedDevice builds a schema-accurate resource.k8s.io/v1beta1 device:
// the Device type there is a `{name, basic}` union, so every detail field
// (taints, per-device nodeName/nodeSelector/allNodes) nests under `basic`.
func basicWrappedDevice(name string, fields map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"name": name, "basic": fields}
}

type nodeOpt func(*corev1.Node)

func withGPUAllocatable(count string) nodeOpt {
	return func(n *corev1.Node) {
		if n.Status.Allocatable == nil {
			n.Status.Allocatable = corev1.ResourceList{}
		}
		n.Status.Allocatable[corev1.ResourceName(resourceNVIDIAGPU)] = resource.MustParse(count)
	}
}

func withNotReady() nodeOpt {
	return func(n *corev1.Node) {
		n.Status.Conditions = []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
		}
	}
}

func withUnschedulable() nodeOpt {
	return func(n *corev1.Node) { n.Spec.Unschedulable = true }
}

func withNodeLabels(labels map[string]string) nodeOpt {
	return func(n *corev1.Node) { n.Labels = labels }
}

func testNode(name string, opts ...nodeOpt) *corev1.Node {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
	for _, opt := range opts {
		opt(node)
	}
	return node
}

func TestDetectGPUAllocationMode(t *testing.T) {
	tests := []struct {
		name string
		// draVersion is the served resource.k8s.io version registered in
		// discovery and used for the dynamic fixtures; defaults to "v1".
		draVersion string
		// noDRAAPI leaves discovery empty: no resource.k8s.io version served.
		noDRAAPI    bool
		nodes       []runtime.Object
		dynObjects  []runtime.Object
		wantDRA     bool
		wantPlugin  bool
		wantDRANode string // when non-empty, must appear in DRANodes
	}{
		{
			name:  "dra usable via nodeName topology",
			nodes: []runtime.Object{testNode("node1")},
			dynObjects: []runtime.Object{
				testDeviceClass(draDriverGPU),
				testResourceSlice("s1", draDriverGPU, "node1", 1, 1,
					map[string]interface{}{"nodeName": "node1"},
					[]interface{}{plainDevice("gpu-0")}),
			},
			wantDRA:     true,
			wantPlugin:  false,
			wantDRANode: "node1",
		},
		{
			name:  "dra not usable without DeviceClass, device plugin usable",
			nodes: []runtime.Object{testNode("node1", withGPUAllocatable("8"))},
			dynObjects: []runtime.Object{
				testResourceSlice("s1", draDriverGPU, "node1", 1, 1,
					map[string]interface{}{"nodeName": "node1"},
					[]interface{}{plainDevice("gpu-0")}),
			},
			wantDRA:    false,
			wantPlugin: true,
		},
		{
			name:  "compute-domain-only slices do not enable full-GPU DRA",
			nodes: []runtime.Object{testNode("node1", withGPUAllocatable("4"))},
			dynObjects: []runtime.Object{
				testDeviceClass(draDriverComputeDomain),
				testResourceSlice("s1", draDriverComputeDomain, "node1", 1, 1,
					map[string]interface{}{"nodeName": "node1"},
					[]interface{}{plainDevice("channel-0")}),
			},
			wantDRA:    false,
			wantPlugin: true,
		},
		{
			name: "stale pool generation ignored",
			nodes: []runtime.Object{
				testNode("node1"),
				testNode("node2", withUnschedulable()),
			},
			dynObjects: []runtime.Object{
				testDeviceClass(draDriverGPU),
				// Stale gen-1 slice points at a healthy node; current gen-2
				// slice points at a cordoned node. Only gen 2 counts.
				testResourceSlice("s1-old", draDriverGPU, "pool-a", 1, 1,
					map[string]interface{}{"nodeName": "node1"},
					[]interface{}{plainDevice("gpu-0")}),
				testResourceSlice("s1-new", draDriverGPU, "pool-a", 2, 1,
					map[string]interface{}{"nodeName": "node2"},
					[]interface{}{plainDevice("gpu-0")}),
			},
			wantDRA:    false,
			wantPlugin: false,
		},
		{
			name:  "incomplete pool ignored",
			nodes: []runtime.Object{testNode("node1")},
			dynObjects: []runtime.Object{
				testDeviceClass(draDriverGPU),
				// resourceSliceCount says 2 but only 1 slice observed.
				testResourceSlice("s1", draDriverGPU, "pool-a", 1, 2,
					map[string]interface{}{"nodeName": "node1"},
					[]interface{}{plainDevice("gpu-0")}),
			},
			wantDRA:    false,
			wantPlugin: false,
		},
		{
			name:  "inconsistent resourceSliceCount ignored",
			nodes: []runtime.Object{testNode("node1")},
			dynObjects: []runtime.Object{
				testDeviceClass(draDriverGPU),
				testResourceSlice("s1", draDriverGPU, "pool-a", 1, 2,
					map[string]interface{}{"nodeName": "node1"},
					[]interface{}{plainDevice("gpu-0")}),
				testResourceSlice("s2", draDriverGPU, "pool-a", 1, 3,
					map[string]interface{}{"nodeName": "node1"},
					[]interface{}{plainDevice("gpu-1")}),
			},
			wantDRA:    false,
			wantPlugin: false,
		},
		{
			// The Kubernetes allocator rejects pools that advertise duplicate
			// device names — an invalid pool is not usable capacity.
			name:  "duplicate device names within a slice invalidate the pool",
			nodes: []runtime.Object{testNode("node1")},
			dynObjects: []runtime.Object{
				testDeviceClass(draDriverGPU),
				testResourceSlice("s1", draDriverGPU, "node1", 1, 1,
					map[string]interface{}{"nodeName": "node1"},
					[]interface{}{plainDevice("gpu-0"), plainDevice("gpu-0")}),
			},
			wantDRA:    false,
			wantPlugin: false,
		},
		{
			name:  "duplicate device names across a pool's slices invalidate the pool",
			nodes: []runtime.Object{testNode("node1")},
			dynObjects: []runtime.Object{
				testDeviceClass(draDriverGPU),
				testResourceSlice("s1", draDriverGPU, "pool-a", 1, 2,
					map[string]interface{}{"nodeName": "node1"},
					[]interface{}{plainDevice("gpu-0")}),
				testResourceSlice("s2", draDriverGPU, "pool-a", 1, 2,
					map[string]interface{}{"nodeName": "node1"},
					[]interface{}{plainDevice("gpu-0")}),
			},
			wantDRA:    false,
			wantPlugin: false,
		},
		{
			name:  "NoSchedule-tainted devices excluded",
			nodes: []runtime.Object{testNode("node1")},
			dynObjects: []runtime.Object{
				testDeviceClass(draDriverGPU),
				testResourceSlice("s1", draDriverGPU, "node1", 1, 1,
					map[string]interface{}{"nodeName": "node1"},
					[]interface{}{taintedDevice("gpu-0", "NoSchedule")}),
			},
			wantDRA:    false,
			wantPlugin: false,
		},
		{
			name:  "NoExecute-tainted devices excluded",
			nodes: []runtime.Object{testNode("node1")},
			dynObjects: []runtime.Object{
				testDeviceClass(draDriverGPU),
				testResourceSlice("s1", draDriverGPU, "node1", 1, 1,
					map[string]interface{}{"nodeName": "node1"},
					[]interface{}{taintedDevice("gpu-0", "NoExecute")}),
			},
			wantDRA:    false,
			wantPlugin: false,
		},
		{
			name:  "tainted device does not mask an untainted sibling",
			nodes: []runtime.Object{testNode("node1")},
			dynObjects: []runtime.Object{
				testDeviceClass(draDriverGPU),
				testResourceSlice("s1", draDriverGPU, "node1", 1, 1,
					map[string]interface{}{"nodeName": "node1"},
					[]interface{}{taintedDevice("gpu-0", "NoSchedule"), plainDevice("gpu-1")}),
			},
			wantDRA:     true,
			wantPlugin:  false,
			wantDRANode: "node1",
		},
		{
			name:  "allNodes topology",
			nodes: []runtime.Object{testNode("node1")},
			dynObjects: []runtime.Object{
				testDeviceClass(draDriverGPU),
				testResourceSlice("s1", draDriverGPU, "pool-a", 1, 1,
					map[string]interface{}{"allNodes": true},
					[]interface{}{plainDevice("gpu-0")}),
			},
			wantDRA:     true,
			wantPlugin:  false,
			wantDRANode: "node1",
		},
		{
			name: "nodeSelector topology matches labeled node",
			nodes: []runtime.Object{
				testNode("node1", withNodeLabels(map[string]string{"accel": "gpu"})),
				testNode("node2"),
			},
			dynObjects: []runtime.Object{
				testDeviceClass(draDriverGPU),
				testResourceSlice("s1", draDriverGPU, "pool-a", 1, 1,
					map[string]interface{}{"nodeSelector": map[string]interface{}{
						"nodeSelectorTerms": []interface{}{
							map[string]interface{}{
								"matchExpressions": []interface{}{
									map[string]interface{}{
										"key":      "accel",
										"operator": "In",
										"values":   []interface{}{"gpu"},
									},
								},
							},
						},
					}},
					[]interface{}{plainDevice("gpu-0")}),
			},
			wantDRA:     true,
			wantPlugin:  false,
			wantDRANode: "node1",
		},
		{
			name:  "nodeSelector topology with no matching node",
			nodes: []runtime.Object{testNode("node1")},
			dynObjects: []runtime.Object{
				testDeviceClass(draDriverGPU),
				testResourceSlice("s1", draDriverGPU, "pool-a", 1, 1,
					map[string]interface{}{"nodeSelector": map[string]interface{}{
						"nodeSelectorTerms": []interface{}{
							map[string]interface{}{
								"matchExpressions": []interface{}{
									map[string]interface{}{
										"key":      "accel",
										"operator": "In",
										"values":   []interface{}{"gpu"},
									},
								},
							},
						},
					}},
					[]interface{}{plainDevice("gpu-0")}),
			},
			wantDRA:    false,
			wantPlugin: false,
		},
		{
			name:  "perDeviceNodeSelection topology",
			nodes: []runtime.Object{testNode("node1")},
			dynObjects: []runtime.Object{
				testDeviceClass(draDriverGPU),
				testResourceSlice("s1", draDriverGPU, "pool-a", 1, 1,
					map[string]interface{}{"perDeviceNodeSelection": true},
					[]interface{}{map[string]interface{}{
						"name":     "gpu-0",
						"nodeName": "node1",
					}}),
			},
			wantDRA:     true,
			wantPlugin:  false,
			wantDRANode: "node1",
		},
		{
			name:       "dra usable on v1beta2-only cluster",
			draVersion: "v1beta2",
			nodes:      []runtime.Object{testNode("node1")},
			dynObjects: []runtime.Object{
				testDeviceClassAt(apiGroupResourceK8sIO+"/v1beta2", draDriverGPU),
				testResourceSliceAt(apiGroupResourceK8sIO+"/v1beta2",
					"s1", draDriverGPU, "node1", 1, 1,
					map[string]interface{}{"nodeName": "node1"},
					[]interface{}{plainDevice("gpu-0")}),
			},
			wantDRA:     true,
			wantPlugin:  false,
			wantDRANode: "node1",
		},
		{
			name:       "dra usable on v1beta1-only cluster (basic-wrapped devices)",
			draVersion: versionV1beta1,
			nodes:      []runtime.Object{testNode("node1")},
			dynObjects: []runtime.Object{
				testDeviceClassAt(apiGroupResourceK8sIO+"/"+versionV1beta1, draDriverGPU),
				testResourceSliceAt(apiGroupResourceK8sIO+"/"+versionV1beta1,
					"s1", draDriverGPU, "node1", 1, 1,
					map[string]interface{}{"nodeName": "node1"},
					[]interface{}{basicWrappedDevice("gpu-0", map[string]interface{}{})}),
			},
			wantDRA:     true,
			wantPlugin:  false,
			wantDRANode: "node1",
		},
		{
			// v1beta1 nests device taints under the `basic` wrapper — the
			// probe must still see and honor them.
			name:       "v1beta1 NoSchedule taint under basic wrapper excluded",
			draVersion: versionV1beta1,
			nodes:      []runtime.Object{testNode("node1")},
			dynObjects: []runtime.Object{
				testDeviceClassAt(apiGroupResourceK8sIO+"/"+versionV1beta1, draDriverGPU),
				testResourceSliceAt(apiGroupResourceK8sIO+"/"+versionV1beta1,
					"s1", draDriverGPU, "node1", 1, 1,
					map[string]interface{}{"nodeName": "node1"},
					[]interface{}{basicWrappedDevice("gpu-0", map[string]interface{}{
						"taints": []interface{}{
							map[string]interface{}{"key": "nvidia.com/gpu", "effect": "NoSchedule"},
						},
					})}),
			},
			wantDRA:    false,
			wantPlugin: false,
		},
		{
			// v1beta1 perDeviceNodeSelection: the per-device nodeName also
			// nests under the `basic` wrapper.
			name:       "v1beta1 perDeviceNodeSelection under basic wrapper",
			draVersion: versionV1beta1,
			nodes:      []runtime.Object{testNode("node1")},
			dynObjects: []runtime.Object{
				testDeviceClassAt(apiGroupResourceK8sIO+"/"+versionV1beta1, draDriverGPU),
				testResourceSliceAt(apiGroupResourceK8sIO+"/"+versionV1beta1,
					"s1", draDriverGPU, "pool-a", 1, 1,
					map[string]interface{}{"perDeviceNodeSelection": true},
					[]interface{}{basicWrappedDevice("gpu-1", map[string]interface{}{
						"nodeName": "node1",
					})}),
			},
			wantDRA:     true,
			wantPlugin:  false,
			wantDRANode: "node1",
		},
		{
			name:       "no served DRA API version — device plugin unaffected",
			noDRAAPI:   true,
			nodes:      []runtime.Object{testNode("node1", withGPUAllocatable("8"))},
			wantDRA:    false,
			wantPlugin: true,
		},
		{
			name:       "device plugin not usable on NotReady node",
			nodes:      []runtime.Object{testNode("node1", withNotReady(), withGPUAllocatable("8"))},
			wantDRA:    false,
			wantPlugin: false,
		},
		{
			name:       "device plugin not usable on cordoned node",
			nodes:      []runtime.Object{testNode("node1", withUnschedulable(), withGPUAllocatable("8"))},
			wantDRA:    false,
			wantPlugin: false,
		},
		{
			name:       "neither mechanism usable",
			nodes:      []runtime.Object{testNode("node1")},
			wantDRA:    false,
			wantPlugin: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			version := tt.draVersion
			if version == "" {
				version = "v1"
			}
			clientset := k8sfake.NewClientset(tt.nodes...)
			if !tt.noDRAAPI {
				withDRAAPIDiscoveryAt(t, clientset, apiGroupResourceK8sIO+"/"+version)
			}
			dynClient := newDRAFakeDynamicClientAt(version, tt.dynObjects...)

			mode, err := Detect(context.Background(), clientset, dynClient)
			if err != nil {
				t.Fatalf("Detect() error = %v", err)
			}
			wantVersion := version
			if tt.noDRAAPI {
				wantVersion = ""
			}
			if mode.APIVersion != wantVersion {
				t.Errorf("APIVersion = %q, want %q", mode.APIVersion, wantVersion)
			}
			if mode.DRAUsable != tt.wantDRA {
				t.Errorf("DRAUsable = %t, want %t (detail: %s)", mode.DRAUsable, tt.wantDRA, mode.DRADetail)
			}
			if mode.DevicePluginUsable != tt.wantPlugin {
				t.Errorf("DevicePluginUsable = %t, want %t (detail: %s)",
					mode.DevicePluginUsable, tt.wantPlugin, mode.DevicePluginDetail)
			}
			if tt.wantDRANode != "" {
				found := false
				for _, n := range mode.DRANodes {
					if n == tt.wantDRANode {
						found = true
					}
				}
				if !found {
					t.Errorf("DRANodes = %v, want to contain %q", mode.DRANodes, tt.wantDRANode)
				}
			}
			if mode.DRADetail == "" || mode.DevicePluginDetail == "" {
				t.Error("mode details must always be populated")
			}
		})
	}
}

// TestDetectGPUAllocationMode_DualAdvertisementWarns verifies that when both
// mechanisms advertise GPUs on the same node the probe records the dual
// advertisement and emits a warning — without failing (a later PR promotes
// this to an error). Exercised at both the GA and a beta served version:
// dual-mode beta clusters must get the same detection.
func TestDetectGPUAllocationMode_DualAdvertisementWarns(t *testing.T) {
	for _, version := range []string{"v1", "v1beta2"} {
		t.Run(version, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			apiVersion := apiGroupResourceK8sIO + "/" + version
			clientset := k8sfake.NewClientset(testNode("node1", withGPUAllocatable("8")))
			withDRAAPIDiscoveryAt(t, clientset, apiVersion)
			dynClient := newDRAFakeDynamicClientAt(version,
				testDeviceClassAt(apiVersion, draDriverGPU),
				testResourceSliceAt(apiVersion, "s1", draDriverGPU, "node1", 1, 1,
					map[string]interface{}{"nodeName": "node1"},
					[]interface{}{plainDevice("gpu-0")}),
			)

			mode, err := Detect(context.Background(), clientset, dynClient)
			if err != nil {
				t.Fatalf("Detect() error = %v", err)
			}
			if !mode.DRAUsable || !mode.DevicePluginUsable {
				t.Fatalf("both mechanisms should be usable: DRA=%t plugin=%t", mode.DRAUsable, mode.DevicePluginUsable)
			}
			if len(mode.DualAdvertisedNodes) != 1 || mode.DualAdvertisedNodes[0] != "node1" {
				t.Errorf("DualAdvertisedNodes = %v, want [node1]", mode.DualAdvertisedNodes)
			}
			if !strings.Contains(buf.String(), "over-admission") {
				t.Errorf("expected a GPU over-admission warning in logs, got: %s", buf.String())
			}
			if !strings.Contains(mode.Summary(), "over-admission") {
				t.Errorf("Summary() should mention the over-admission risk: %s", mode.Summary())
			}
		})
	}
}

// TestDetectGPUAllocationMode_ExtendedResourceDRABacked verifies KEP-5004
// DRAExtendedResource attribution evidence for a mapped class WITHOUT
// gpu.nvidia.com ResourceSlices: the mapping is recorded, but a Ready node
// advertising scalar allocatable nvidia.com/gpu remains device-plugin usable
// (scalar allocatable is device-plugin-backed by definition; DRA satisfies
// the request only where scalar allocatable is absent/zero) and no
// dual-advertisement warning is emitted.
func TestDetectGPUAllocationMode_ExtendedResourceDRABacked(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	clientset := k8sfake.NewClientset(testNode("node1", withGPUAllocatable("8")))
	withDRAAPIDiscovery(t, clientset)
	dynClient := newDRAFakeDynamicClient(
		testDeviceClassWithExtendedResource("gpu-er.nvidia.com", resourceNVIDIAGPU),
	)

	mode, err := Detect(context.Background(), clientset, dynClient)
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	if !mode.ExtendedResourceDRABacked {
		t.Errorf("ExtendedResourceDRABacked = false, want true (detail: %s)", mode.ExtendedResourceDetail)
	}
	if !strings.Contains(mode.ExtendedResourceDetail, "gpu-er.nvidia.com") {
		t.Errorf("ExtendedResourceDetail = %q, want DeviceClass name", mode.ExtendedResourceDetail)
	}
	if !strings.Contains(mode.Summary(), "extended-resource attribution") {
		t.Errorf("Summary() should include the extended-resource attribution detail: %s", mode.Summary())
	}
	// The mapping is recorded evidence only — it must not clear usability:
	// scalar allocatable nvidia.com/gpu on a Ready node is device-plugin-backed.
	if !mode.DevicePluginUsable {
		t.Errorf("DevicePluginUsable = false, want true — KEP-5004 mapping must not clear usability (detail: %s)",
			mode.DevicePluginDetail)
	}
	if len(mode.DualAdvertisedNodes) != 0 {
		t.Errorf("DualAdvertisedNodes = %v, want none without gpu.nvidia.com slices", mode.DualAdvertisedNodes)
	}
	if strings.Contains(buf.String(), "over-admission") {
		t.Errorf("no over-admission warning expected without gpu.nvidia.com slices, got: %s", buf.String())
	}
}

// TestDetectGPUAllocationMode_ExtendedResourceDualAdvertisementWarns verifies
// the combined KEP-5004 case: a DeviceClass maps nvidia.com/gpu to DRA,
// gpu.nvidia.com ResourceSlices are live, and the same node advertises scalar
// allocatable nvidia.com/gpu. Both mechanisms are usable, and because DRA
// slices coexist with scalar allocatable on the same node — a real
// over-admission risk — the dual-advertisement warning must fire.
func TestDetectGPUAllocationMode_ExtendedResourceDualAdvertisementWarns(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	clientset := k8sfake.NewClientset(testNode("node1", withGPUAllocatable("8")))
	withDRAAPIDiscovery(t, clientset)
	dynClient := newDRAFakeDynamicClient(
		testDeviceClass(draDriverGPU),
		testDeviceClassWithExtendedResource("gpu-er.nvidia.com", resourceNVIDIAGPU),
		testResourceSlice("s1", draDriverGPU, "node1", 1, 1,
			map[string]interface{}{"nodeName": "node1"},
			[]interface{}{plainDevice("gpu-0")}),
	)

	mode, err := Detect(context.Background(), clientset, dynClient)
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	if !mode.ExtendedResourceDRABacked {
		t.Errorf("ExtendedResourceDRABacked = false, want true (detail: %s)", mode.ExtendedResourceDetail)
	}
	if !mode.DRAUsable {
		t.Errorf("DRAUsable = false, want true (detail: %s)", mode.DRADetail)
	}
	if !mode.DevicePluginUsable {
		t.Errorf("DevicePluginUsable = false, want true — scalar allocatable is device-plugin-backed (detail: %s)",
			mode.DevicePluginDetail)
	}
	if len(mode.DualAdvertisedNodes) != 1 || mode.DualAdvertisedNodes[0] != "node1" {
		t.Errorf("DualAdvertisedNodes = %v, want [node1] — DRA slices plus scalar allocatable is an over-admission risk",
			mode.DualAdvertisedNodes)
	}
	if !strings.Contains(buf.String(), "over-admission") {
		t.Errorf("expected a GPU over-admission warning in logs, got: %s", buf.String())
	}
	if !strings.Contains(mode.Summary(), "over-admission") {
		t.Errorf("Summary() should report the over-admission risk: %s", mode.Summary())
	}
}

// TestDetectGPUAllocationMode_ExtendedResourceNotBacked verifies that a
// DeviceClass without the mapping (or mapping another resource) does not
// trigger the guard.
func TestDetectGPUAllocationMode_ExtendedResourceNotBacked(t *testing.T) {
	clientset := k8sfake.NewClientset(testNode("node1", withGPUAllocatable("8")))
	withDRAAPIDiscovery(t, clientset)
	dynClient := newDRAFakeDynamicClient(
		testDeviceClass(draDriverComputeDomain),
		testDeviceClassWithExtendedResource("other.example.com", "example.com/other"),
	)

	mode, err := Detect(context.Background(), clientset, dynClient)
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	if mode.ExtendedResourceDRABacked {
		t.Errorf("ExtendedResourceDRABacked = true, want false (detail: %s)", mode.ExtendedResourceDetail)
	}
}

// TestDetectGPUAllocationMode_DeviceClassListErrors verifies the fail-closed
// contract of the extended-resource guard: only "DRA API not served"
// (NotFound) bypasses it; any other DeviceClass list error propagates.
func TestDetectGPUAllocationMode_DeviceClassListErrors(t *testing.T) {
	tests := []struct {
		name    string
		listErr error
		wantErr bool
	}{
		{
			name: "NotFound (DRA API not served) bypasses the guard",
			listErr: k8serrors.NewNotFound(
				schema.GroupResource{Group: apiGroupResourceK8sIO, Resource: "deviceclasses"}, ""),
			wantErr: false,
		},
		{
			name:    "other list errors propagate (fail closed)",
			listErr: stderrors.New("apiserver hiccup"),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := k8sfake.NewClientset(testNode("node1", withGPUAllocatable("8")))
			withDRAAPIDiscovery(t, clientset)
			dynClient := newDRAFakeDynamicClient()
			dynClient.PrependReactor("list", "deviceclasses",
				func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.listErr
				})

			mode, err := Detect(context.Background(), clientset, dynClient)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected DeviceClass list error to propagate")
				}
				return
			}
			if err != nil {
				t.Fatalf("Detect() error = %v, want nil", err)
			}
			if mode.ExtendedResourceDRABacked {
				t.Error("ExtendedResourceDRABacked = true, want false when DRA API not served")
			}
		})
	}
}

// TestDiscoverServedDRAAPIVersion_HonorsContext verifies discovery goes
// through a context-aware REST request: client-go's
// ServerResourcesForGroupVersion issues its GET with context.TODO()
// internally, so without the REST-path probe a hung apiserver would outlive
// the validator timeout. A canceled context must abort the probe.
func TestDiscoverServedDRAAPIVersion_HonorsContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"resource.k8s.io/v1","resources":[]}`))
	}))
	defer srv.Close()
	clientset, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("failed to build clientset: %v", err)
	}

	// Sanity: with a live context, discovery resolves via the REST path.
	version, _, err := DiscoverServedVersion(context.Background(), clientset)
	if err != nil {
		t.Fatalf("DiscoverServedVersion() error = %v, want nil", err)
	}
	if version != "v1" {
		t.Fatalf("version = %q, want v1", version)
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = DiscoverServedVersion(canceledCtx, clientset)
	if err == nil {
		t.Fatal("expected a canceled context to abort DRA API discovery")
	}
	if !stderrors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled in the unwrap chain", err)
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Errorf("error code = %v, want ErrCodeTimeout for a canceled discovery probe", err)
	}
}

// TestDiscoverServedDRAAPIVersion_FakeClientHonorsContext verifies the
// nil-RESTClient fallback branch (fake discovery clients) still honors the
// context: a canceled probe aborts with ErrCodeTimeout instead of silently
// consulting the in-memory resource list — both when the cancellation
// happens before the discovery call and when it races the call itself.
func TestDiscoverServedDRAAPIVersion_FakeClientHonorsContext(t *testing.T) {
	t.Run("live context resolves", func(t *testing.T) {
		clientset := k8sfake.NewClientset()
		withDRAAPIDiscovery(t, clientset)
		version, _, err := DiscoverServedVersion(context.Background(), clientset)
		if err != nil || version != "v1" {
			t.Fatalf("DiscoverServedVersion() = %q, %v; want v1, nil", version, err)
		}
	})

	t.Run("canceled before the call aborts", func(t *testing.T) {
		clientset := k8sfake.NewClientset()
		withDRAAPIDiscovery(t, clientset)
		canceledCtx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _, err := DiscoverServedVersion(canceledCtx, clientset)
		if err == nil {
			t.Fatal("expected a canceled context to abort discovery on the fallback branch")
		}
		if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
			t.Errorf("error code = %v, want ErrCodeTimeout", err)
		}
	})

	t.Run("canceled during the call aborts", func(t *testing.T) {
		clientset := k8sfake.NewClientset()
		withDRAAPIDiscovery(t, clientset)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		// The fake DiscoveryInterface records a `get resource` action for
		// ServerResourcesForGroupVersion; cancel the probe's context from
		// inside that call and let it return successfully — the fallback
		// must still observe the cancellation (recheck AFTER the call).
		clientset.PrependReactor("get", "resource", func(k8stesting.Action) (bool, runtime.Object, error) {
			cancel()
			return false, nil, nil
		})
		_, _, err := DiscoverServedVersion(ctx, clientset)
		if err == nil {
			t.Fatal("expected a cancellation during the discovery call to abort the probe")
		}
		if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
			t.Errorf("error code = %v, want ErrCodeTimeout", err)
		}
		if !stderrors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled in the unwrap chain", err)
		}
	})
}

func TestNodeMatchesSelector(t *testing.T) {
	node := testNode("node1", withNodeLabels(map[string]string{"accel": "gpu", "zone": "a"}))

	tests := []struct {
		name string
		sel  corev1.NodeSelector
		want bool
	}{
		{
			name: "In matches",
			sel: corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{
					{Key: "accel", Operator: corev1.NodeSelectorOpIn, Values: []string{"gpu"}},
				},
			}}},
			want: true,
		},
		{
			name: "NotIn rejects matching value",
			sel: corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{
					{Key: "accel", Operator: corev1.NodeSelectorOpNotIn, Values: []string{"gpu"}},
				},
			}}},
			want: false,
		},
		{
			name: "NotIn accepts non-matching value",
			sel: corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{
					{Key: "accel", Operator: corev1.NodeSelectorOpNotIn, Values: []string{"tpu"}},
				},
			}}},
			want: true,
		},
		{
			// Kubernetes NotIn semantics: an absent label key also matches.
			name: "NotIn matches when label key absent",
			sel: corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{
					{Key: "missing-key", Operator: corev1.NodeSelectorOpNotIn, Values: []string{"gpu"}},
				},
			}}},
			want: true,
		},
		{
			name: "Exists matches present label",
			sel: corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{
					{Key: "zone", Operator: corev1.NodeSelectorOpExists},
				},
			}}},
			want: true,
		},
		{
			name: "DoesNotExist rejects present label",
			sel: corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{
					{Key: "zone", Operator: corev1.NodeSelectorOpDoesNotExist},
				},
			}}},
			want: false,
		},
		{
			name: "AND within a term",
			sel: corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{
					{Key: "accel", Operator: corev1.NodeSelectorOpIn, Values: []string{"gpu"}},
					{Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"b"}},
				},
			}}},
			want: false,
		},
		{
			name: "OR across terms",
			sel: corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{MatchExpressions: []corev1.NodeSelectorRequirement{
					{Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"b"}},
				}},
				{MatchExpressions: []corev1.NodeSelectorRequirement{
					{Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"a"}},
				}},
			}},
			want: true,
		},
		{
			name: "matchFields metadata.name",
			sel: corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchFields: []corev1.NodeSelectorRequirement{
					{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"node1"}},
				},
			}}},
			want: true,
		},
		{
			name: "unsupported field key fails closed",
			sel: corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchFields: []corev1.NodeSelectorRequirement{
					{Key: "metadata.uid", Operator: corev1.NodeSelectorOpIn, Values: []string{"x"}},
				},
			}}},
			want: false,
		},
		{
			name: "unsupported operator Gt fails closed",
			sel: corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{
					{Key: "accel", Operator: corev1.NodeSelectorOpGt, Values: []string{"1"}},
				},
			}}},
			want: false,
		},
		{
			name: "empty term matches nothing",
			sel:  corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{}}},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nodeMatchesSelector(node, &tt.sel); got != tt.want {
				t.Errorf("nodeMatchesSelector() = %t, want %t", got, tt.want)
			}
		})
	}
}

// TestScanGPUSliceTopology pins the inference-support detectors: node-local
// gpu.nvidia.com attribution by RAW spec.nodeName with unvalidated device
// counting (kai's model), foreign "gpu"-named drivers surfaced for the
// mixed-driver fail-fast (#1652), non-node-local gpu.nvidia.com topologies
// surfaced for the topology fail-fast (#1652), and ComputeDomain drivers
// (no "gpu" substring) ignored entirely.
func TestScanGPUSliceTopology(t *testing.T) {
	items := []unstructured.Unstructured{
		// Node-local, tainted device, incomplete pool: counted raw anyway.
		*testResourceSlice("bad-pool", draDriverGPU, "pool-x", 1, 2,
			map[string]interface{}{"nodeName": "node-a"},
			[]interface{}{taintedDevice("gpu-0", "NoSchedule"), plainDevice("gpu-1")}),
		// Node-local, healthy: counted.
		*testResourceSlice("good", draDriverGPU, "node-b", 1, 1,
			map[string]interface{}{"nodeName": "node-b"},
			[]interface{}{plainDevice("gpu-0")}),
		// nodeSelector topology: non-node-local — surfaced for fail-fast.
		*testResourceSlice("selector-topo", draDriverGPU, "pool-s", 1, 1,
			map[string]interface{}{"nodeSelector": map[string]interface{}{}},
			[]interface{}{plainDevice("gpu-0")}),
		// allNodes topology: non-node-local — surfaced for fail-fast.
		*testResourceSlice("allnodes-topo", draDriverGPU, "pool-a", 1, 1,
			map[string]interface{}{"allNodes": true},
			[]interface{}{plainDevice("gpu-0")}),
		// ComputeDomain driver: no "gpu" substring — ignored entirely.
		*testResourceSlice("cd", draDriverComputeDomain, "node-a", 1, 1,
			map[string]interface{}{"nodeName": "node-a"},
			[]interface{}{plainDevice("ch-0")}),
		// Non-NVIDIA "gpu"-named driver: surfaced for the mixed-driver
		// fail-fast, never counted into the node-local map.
		*testResourceSlice("amd", "gpu.amd.com", "node-c", 1, 1,
			map[string]interface{}{"nodeName": "node-c"},
			[]interface{}{plainDevice("gpu-0"), plainDevice("gpu-1"), plainDevice("gpu-2")}),
		// Two node-local slices publishing the SAME pool from different
		// nodes: attribution ambiguous — excluded from the pool map and
		// surfaced for fail-fast. Devices still count raw per node.
		*testResourceSlice("dup-d", draDriverGPU, "pool-dup", 1, 2,
			map[string]interface{}{"nodeName": "node-d"},
			[]interface{}{plainDevice("gpu-0")}),
		*testResourceSlice("dup-e", draDriverGPU, "pool-dup", 1, 2,
			map[string]interface{}{"nodeName": "node-e"},
			[]interface{}{plainDevice("gpu-0")}),
		// All-zero-device node-local slice (upstream dra-driver-nvidia-gpu
		// #1008, seen on GKE/COS): kai sets HasDRAGPUs only for a POSITIVE
		// aggregate device count, so the node must NOT enter the raw
		// kai-attributable map — a zero entry would emit false
		// kai-rejection evidence in Mode.Summary. Pool attribution is kept
		// (the slice still names the pool's home node).
		*testResourceSlice("empty", draDriverGPU, "pool-z", 1, 1,
			map[string]interface{}{"nodeName": "node-z"},
			[]interface{}{}),
	}
	nodeLocal, poolNodes, foreign, nonLocal, ambiguous := scanGPUSliceTopology(items)

	wantLocal := map[string]int{"node-a": 2, "node-b": 1, "node-d": 1, "node-e": 1}
	if len(nodeLocal) != len(wantLocal) {
		t.Fatalf("nodeLocal = %v, want %v", nodeLocal, wantLocal)
	}
	if _, present := nodeLocal["node-z"]; present {
		t.Errorf("nodeLocal[node-z] present (= %d) — an all-zero-device node must not enter the raw kai-attributable map", nodeLocal["node-z"])
	}
	for n, c := range wantLocal {
		if nodeLocal[n] != c {
			t.Errorf("nodeLocal[%s] = %d, want %d (raw counting, no validation)", n, nodeLocal[n], c)
		}
	}
	// Pool attribution comes from the slices' spec.nodeName, NOT pool-name /
	// node-name equality: pool-x belongs to node-a even though no node is
	// named pool-x. Foreign-driver and non-node-local pools never map; the
	// duplicated pool is excluded as ambiguous.
	wantPools := map[string]string{"pool-x": "node-a", "node-b": "node-b", "pool-z": "node-z"}
	if len(poolNodes) != len(wantPools) {
		t.Fatalf("poolNodes = %v, want %v", poolNodes, wantPools)
	}
	for pool, node := range wantPools {
		if poolNodes[pool] != node {
			t.Errorf("poolNodes[%s] = %q, want %q (slice-derived attribution)", pool, poolNodes[pool], node)
		}
	}
	if fmt.Sprintf("%v", foreign) != "[gpu.amd.com]" {
		t.Errorf("foreignDrivers = %v, want [gpu.amd.com]", foreign)
	}
	if fmt.Sprintf("%v", nonLocal) != "[allnodes-topo selector-topo]" {
		t.Errorf("nonNodeLocal = %v, want [allnodes-topo selector-topo] (sorted)", nonLocal)
	}
	if fmt.Sprintf("%v", ambiguous) != "[pool-dup]" {
		t.Errorf("ambiguousPools = %v, want [pool-dup]", ambiguous)
	}
}

// TestDetect_KaiRawSetPopulatedWithoutDeviceClass verifies the raw kai set is
// populated even when the gpu.nvidia.com DeviceClass is absent — kai's node
// classification does not consult DeviceClasses, so neither may the mirror.
func TestDetect_KaiRawSetPopulatedWithoutDeviceClass(t *testing.T) {
	clientset := k8sfake.NewClientset(testNode("node-a"))
	withDRAAPIDiscovery(t, clientset)
	// Slices exist, but NO DeviceClass object.
	dynClient := newDRAFakeDynamicClient(
		testResourceSlice("s1", draDriverGPU, "node-a", 1, 2,
			map[string]interface{}{"nodeName": "node-a"},
			[]interface{}{taintedDevice("gpu-0", "NoSchedule"), plainDevice("gpu-1")}),
	)

	mode, err := Detect(context.Background(), clientset, dynClient)
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	if mode.DRAUsable {
		t.Error("DRAUsable = true, want false (no DeviceClass)")
	}
	if got := mode.NodeLocalGPUSliceDevices["node-a"]; got != 2 {
		t.Errorf("NodeLocalGPUSliceDevices[node-a] = %d, want 2 — raw set must not depend on DeviceClass existence", got)
	}
	if !strings.Contains(mode.Summary(), "node-local raw") {
		t.Errorf("Summary() missing the kai raw-slice evidence line:\n%s", mode.Summary())
	}
}

// TestDetect_ProbeReadErrorClassification pins the shared read-error
// classification at EVERY probe read: timeout forms map to ErrCodeTimeout
// and non-timeout API failures stay ErrCodeInternal, per call site.
func TestDetect_ProbeReadErrorClassification(t *testing.T) {
	gr := schema.GroupResource{Group: apiGroupResourceK8sIO, Resource: "any"}
	timeoutErr := k8serrors.NewServerTimeout(gr, "get", 1)
	forbiddenErr := k8serrors.NewForbidden(gr, "x", stderrors.New("rbac denied"))

	type site struct {
		name    string
		typed   bool // reactor on the typed clientset vs the dynamic client
		verb    string
		resname string
	}
	sites := []site{
		{name: "node list", typed: true, verb: "list", resname: "nodes"},
		{name: "API discovery", typed: true, verb: "get", resname: "resource"},
		{name: "ResourceSlice list", typed: false, verb: "list", resname: "resourceslices"},
		// The single DeviceClass List serves both the extended-resource
		// attribution probe and full-GPU DRA detection (the per-Detect Get
		// was removed as redundant — one round-trip per Detect).
		{name: "DeviceClass list (attribution + full-GPU detection)", typed: false, verb: "list", resname: "deviceclasses"},
	}
	cases := []struct {
		kind       string
		inject     error
		wantTarget error
	}{
		{kind: "ServerTimeout → ErrCodeTimeout", inject: timeoutErr, wantTarget: errors.New(errors.ErrCodeTimeout, "")},
		{kind: "Forbidden → ErrCodeInternal", inject: forbiddenErr, wantTarget: errors.New(errors.ErrCodeInternal, "")},
	}
	for _, st := range sites {
		for _, tc := range cases {
			t.Run(st.name+" / "+tc.kind, func(t *testing.T) {
				clientset := k8sfake.NewClientset(testNode("node1"))
				withDRAAPIDiscovery(t, clientset)
				dynClient := newDRAFakeDynamicClient(
					testDeviceClass(draDriverGPU),
					testResourceSlice("s1", draDriverGPU, "node1", 1, 1,
						map[string]interface{}{"nodeName": "node1"},
						[]interface{}{plainDevice("gpu-0")}),
				)
				reactor := func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tc.inject
				}
				if st.typed {
					clientset.PrependReactor(st.verb, st.resname, reactor)
				} else {
					dynClient.PrependReactor(st.verb, st.resname, reactor)
				}
				_, err := Detect(context.Background(), clientset, dynClient)
				if err == nil {
					t.Fatalf("Detect() = nil error, want failure injected at %s", st.name)
				}
				if !stderrors.Is(err, tc.wantTarget) {
					t.Errorf("error = %v, want code %v to match (site %s)", err, tc.wantTarget, st.name)
				}
			})
		}
	}
}

// TestUsableDriverSliceNodes_Direct pins the exported wrapper directly —
// previously it was exercised only transitively through the conformance
// bridge alias, which trips the repo's new-exported-func coverage gate if
// the alias ever moves (#1620 review).
func TestUsableDriverSliceNodes_Direct(t *testing.T) {
	eligible := EligibleReadySchedulableNodes(&corev1.NodeList{
		Items: []corev1.Node{*testNode("node-a")},
	})
	items := []unstructured.Unstructured{
		// Valid: complete current-generation pool, untainted device,
		// Ready node → node-a is usable.
		*testResourceSlice("good", draDriverGPU, "node-a", 1, 1,
			map[string]interface{}{"nodeName": "node-a"},
			[]interface{}{plainDevice("gpu-0")}),
		// Foreign driver: not counted for gpu.nvidia.com.
		*testResourceSlice("foreign", "other.example.com", "node-a", 1, 1,
			map[string]interface{}{"nodeName": "node-a"},
			[]interface{}{plainDevice("x-0")}),
		// Incomplete pool (resourceSliceCount 2, one slice present): the
		// driver's slice is SEEN but not usable.
		*testResourceSlice("incomplete", draDriverGPU, "pool-i", 1, 2,
			map[string]interface{}{"nodeName": "node-a"},
			[]interface{}{plainDevice("gpu-9")}),
	}
	nodes, seen, err := UsableDriverSliceNodes(context.Background(), items, draDriverGPU, eligible)
	if err != nil {
		t.Fatalf("UsableDriverSliceNodes() error = %v", err)
	}
	if seen != 2 {
		t.Errorf("seen = %d, want 2 (both gpu.nvidia.com slices, foreign driver excluded)", seen)
	}
	if _, ok := nodes["node-a"]; !ok || len(nodes) != 1 {
		t.Errorf("nodes = %v, want exactly {node-a} (complete pool only)", nodes)
	}
}

// TestDriverIdentifierPins is the drift-detection safeguard for the
// identifiers deliberately duplicated from validators/conformance/consts.go
// (an import in either direction risks a cycle): each package pins its copy
// to the canonical literal, so a typo or rename in one copy fails that
// package's pin instead of silently desyncing detection behavior between
// the shared probe and the conformance checks.
func TestDriverIdentifierPins(t *testing.T) {
	pins := map[string]string{
		draDriverGPU:           "gpu.nvidia.com",
		draDriverComputeDomain: "compute-domain.nvidia.com",
		apiGroupResourceK8sIO:  "resource.k8s.io",
		resourceNVIDIAGPU:      "nvidia.com/gpu",
	}
	for got, want := range pins {
		if got != want {
			t.Errorf("driver identifier drifted: %q, want %q (keep in sync with validators/conformance/consts.go)", got, want)
		}
	}
}

// TestDetect_HalfInstalledDRASurfacedInDetail pins the half-installation
// diagnostic (#1620 review): validated gpu.nvidia.com ResourceSlices with
// the DeviceClass missing is a broken install, and DRADetail must say so
// rather than reading like a benign ComputeDomain-only configuration.
func TestDetect_HalfInstalledDRASurfacedInDetail(t *testing.T) {
	clientset := k8sfake.NewClientset(testNode("node-a"))
	withDRAAPIDiscovery(t, clientset)
	// Validated slice, NO gpu.nvidia.com DeviceClass.
	dynClient := newDRAFakeDynamicClient(
		testResourceSlice("s1", draDriverGPU, "node-a", 1, 1,
			map[string]interface{}{"nodeName": "node-a"},
			[]interface{}{plainDevice("gpu-0")}),
	)

	mode, err := Detect(context.Background(), clientset, dynClient)
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	if mode.DRAUsable {
		t.Error("DRAUsable = true, want false (DeviceClass missing)")
	}
	if !strings.Contains(mode.DRADetail, "HALF-INSTALLED") {
		t.Errorf("DRADetail = %q, want the HALF-INSTALLED half-installation surface", mode.DRADetail)
	}
}
