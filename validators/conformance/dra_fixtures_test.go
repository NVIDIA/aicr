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

// DRA test fixtures shared by the conformance check tests. Duplicated from
// validators/internal/allocmode/allocmode_test.go (Go test helpers cannot be
// imported across packages) — keep the two copies in sync.

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	corev1 "k8s.io/api/core/v1"
)

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
			draGVRAt(version, "deviceclasses"):  "DeviceClassList",
			draGVRAt(version, "resourceslices"): "ResourceSliceList",
			draGVRAt(version, "resourceclaims"): "ResourceClaimList",
		}, objects...)
}

//nolint:unparam // signature kept in sync with the allocmode copy
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
//
//nolint:unparam // signature kept in sync with the allocmode copy
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

//nolint:unparam // signature kept in sync with the allocmode copy
func withGPUAllocatable(count string) nodeOpt {
	return func(n *corev1.Node) {
		if n.Status.Allocatable == nil {
			n.Status.Allocatable = corev1.ResourceList{}
		}
		n.Status.Allocatable[corev1.ResourceName(resourceNVIDIAGPU)] = resource.MustParse(count)
	}
}

func withUnschedulable() nodeOpt {
	return func(n *corev1.Node) { n.Spec.Unschedulable = true }
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
