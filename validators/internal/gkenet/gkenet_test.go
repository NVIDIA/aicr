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

package gkenet

import (
	"context"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func newFakeClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{NetworkGVR: "NetworkList"},
		objects...)
}

func testNetwork(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "networking.gke.io/v1",
		"kind":       "Network",
		"metadata":   map[string]interface{}{"name": name},
	}}
}

func networks(names ...string) []runtime.Object {
	objs := make([]runtime.Object, 0, len(names))
	for _, n := range names {
		objs = append(objs, testNetwork(n))
	}
	return objs
}

// prefixed builds the cluster-prefixed names GKE actually provisions
// (e.g. "aicr-demo2-gpu-nic-0") — the reason names cannot be matched exactly.
func prefixed(prefix string, count int) []string {
	names := make([]string, 0, count)
	for i := range count {
		names = append(names, fmt.Sprintf("%sgpu-nic-%d", prefix, i))
	}
	return names
}

// Note on sort coverage: DiscoverGPUNICNetworks sorts its result, and that
// order is load-bearing (the performance validator maps it positionally onto
// eth1..eth8). It is deliberately NOT asserted here — the fake dynamic client
// returns List items already in name order, so a case that inserts names out of
// order still passes with sort.Strings removed. Such a test would assert
// nothing; covering the sort needs a fake that preserves insertion order.
func TestDiscoverGPUNICNetworks(t *testing.T) {
	tests := []struct {
		name    string
		objects []runtime.Object
		want    []string
	}{
		{
			name:    "no networks at all",
			objects: nil,
			want:    nil,
		},
		{
			name:    "only non-GPU networks",
			objects: networks("default", "vpc-1"),
			want:    nil,
		},
		{
			name:    "partial provisioning",
			objects: networks(prefixed("", 7)...),
			want:    prefixed("", 7),
		},
		{
			name:    "exactly the required count",
			objects: networks(prefixed("", RequiredGPUNICNetworks)...),
			want:    prefixed("", RequiredGPUNICNetworks),
		},
		{
			name:    "cluster-specific prefixes are matched by substring",
			objects: networks(prefixed("aicr-demo2-", RequiredGPUNICNetworks)...),
			want:    prefixed("aicr-demo2-", RequiredGPUNICNetworks),
		},
		{
			name:    "unhyphenated gpu-nic0 form is matched",
			objects: networks("gpu-nic0", "gpu-nic1"),
			want:    []string{"gpu-nic0", "gpu-nic1"},
		},
		{
			name:    "non-GPU networks are excluded from the count",
			objects: networks(append(prefixed("", RequiredGPUNICNetworks), "default", "gvnic-0")...),
			want:    prefixed("", RequiredGPUNICNetworks),
		},
		{
			name:    "more than required is returned in full",
			objects: networks(prefixed("", 9)...),
			want:    prefixed("", 9),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DiscoverGPUNICNetworks(context.Background(), newFakeClient(tt.objects...))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d networks %v, want %d %v", len(got), got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("network[%d] = %q, want %q (result must be sorted)", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestDiscoverGPUNICNetworksReturnsRawError guards the contract the deployment
// check depends on: the API error reaches the caller unwrapped, so
// apierrors.IsForbidden still classifies it. Wrapping it in a pkg/errors code
// here would flatten an RBAC denial into a generic internal error.
func TestDiscoverGPUNICNetworksReturnsRawError(t *testing.T) {
	client := newFakeClient()
	client.PrependReactor("list", "networks", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "networking.gke.io", Resource: "networks"}, "", nil)
	})

	_, err := DiscoverGPUNICNetworks(context.Background(), client)
	if err == nil {
		t.Fatal("expected a list error, got nil")
	}
	if !apierrors.IsForbidden(err) {
		t.Errorf("error is no longer classifiable as Forbidden: %v", err)
	}
}
