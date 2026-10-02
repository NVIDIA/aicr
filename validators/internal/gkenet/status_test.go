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
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

// testNetworkStatus builds a GPU NIC Network with the given Ready/ParamsReady
// condition statuses and an optional GKENetworkParamSet parametersRef name.
func testNetworkStatus(name, ready, paramsReady, paramSetName string) *unstructured.Unstructured {
	cond := func(typ, status string) map[string]any {
		return map[string]any{"type": typ, "status": status, "reason": "", "message": ""}
	}
	obj := map[string]any{
		"apiVersion": "networking.gke.io/v1",
		"kind":       "Network",
		"metadata":   map[string]any{"name": name},
		"status": map[string]any{"conditions": []any{
			cond("Ready", ready), cond("ParamsReady", paramsReady),
		}},
	}
	if paramSetName != "" {
		obj["spec"] = map[string]any{"parametersRef": map[string]any{
			"group": "networking.gke.io", "kind": "GKENetworkParamSet", "name": paramSetName,
		}}
	}
	return &unstructured.Unstructured{Object: obj}
}

func TestDiscoverGPUNICNetworkStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		objects      []runtime.Object
		wantName     string
		wantReady    bool
		wantParamsOk bool
		wantParamSet string
	}{
		{
			name:         "ready and bound",
			objects:      []runtime.Object{testNetworkStatus("c-gpu-nic-0", "True", "True", "ps-0")},
			wantName:     "c-gpu-nic-0",
			wantReady:    true,
			wantParamsOk: true,
			wantParamSet: "ps-0",
		},
		{
			name:         "not ready",
			objects:      []runtime.Object{testNetworkStatus("c-gpu-nic-1", "False", "True", "ps-1")},
			wantName:     "c-gpu-nic-1",
			wantReady:    false,
			wantParamsOk: true,
			wantParamSet: "ps-1",
		},
		{
			name:         "binding not ready",
			objects:      []runtime.Object{testNetworkStatus("c-gpu-nic-2", "True", "False", "ps-2")},
			wantName:     "c-gpu-nic-2",
			wantReady:    true,
			wantParamsOk: false,
			wantParamSet: "ps-2",
		},
		{
			name:         "no status at all reads not-ready",
			objects:      []runtime.Object{testNetwork("c-gpu-nic-3")},
			wantName:     "c-gpu-nic-3",
			wantReady:    false,
			wantParamsOk: false,
			wantParamSet: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newFakeClient(tt.objects...)
			got, err := DiscoverGPUNICNetworkStatus(context.Background(), c)
			if err != nil {
				t.Fatalf("DiscoverGPUNICNetworkStatus: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d statuses, want 1: %+v", len(got), got)
			}
			s := got[0]
			if s.Name != tt.wantName || s.Ready != tt.wantReady || s.ParamsReady != tt.wantParamsOk || s.ParamSetName != tt.wantParamSet {
				t.Errorf("status = %+v, want name=%s ready=%v paramsReady=%v paramSet=%s",
					s, tt.wantName, tt.wantReady, tt.wantParamsOk, tt.wantParamSet)
			}
		})
	}
}

// The census filter must be shared: only gpu-nic-substring networks are
// considered, so a non-GPU Network never enters the capability view.
func TestDiscoverGPUNICNetworkStatusFiltersSubstring(t *testing.T) {
	t.Parallel()
	c := newFakeClient(
		testNetworkStatus("c-gpu-nic-0", "True", "True", "ps-0"),
		testNetworkStatus("vpc1", "False", "False", ""), // not a GPU NIC network
	)
	got, err := DiscoverGPUNICNetworkStatus(context.Background(), c)
	if err != nil {
		t.Fatalf("DiscoverGPUNICNetworkStatus: %v", err)
	}
	if len(got) != 1 || got[0].Name != "c-gpu-nic-0" {
		t.Fatalf("got %+v, want only c-gpu-nic-0", got)
	}
}

// The raw-error contract must hold for the status path too — callers classify
// list errors by shape (apierrors.IsNotFound vs everything else).
func TestDiscoverGPUNICNetworkStatusRawError(t *testing.T) {
	t.Parallel()
	c := newFakeClient()
	c.PrependReactor("list", "networks", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "networking.gke.io", Resource: "networks"}, "", nil)
	})
	_, err := DiscoverGPUNICNetworkStatus(context.Background(), c)
	if err == nil {
		t.Fatal("want the raw list error, got nil")
	}
	if !apierrors.IsForbidden(err) {
		t.Fatalf("error is no longer classifiable as Forbidden: %v", err)
	}
}

// Bound is the single definition of "usable by a workload" — Ready AND
// ParamsReady AND a real parametersRef. Cover every combination.
func TestBoundCombinations(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		ready       bool
		paramsReady bool
		paramSet    string
		want        bool
	}{
		{"T,T,set = bound", true, true, "a3-mega-pool", true},
		{"T,T,empty = not bound", true, true, "", false},
		{"T,F,set = not bound", true, false, "a3-mega-pool", false},
		{"T,F,empty = not bound", true, false, "", false},
		{"F,T,set = not bound", false, true, "a3-mega-pool", false},
		{"F,T,empty = not bound", false, true, "", false},
		{"F,F,set = not bound", false, false, "a3-mega-pool", false},
		{"F,F,empty = not bound", false, false, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := GPUNICNetworkStatus{Ready: tc.ready, ParamsReady: tc.paramsReady, ParamSetName: tc.paramSet}
			if got := s.Bound(); got != tc.want {
				t.Errorf("Bound()=%v, want %v (ready=%v paramsReady=%v paramSet=%q)", got, tc.want, tc.ready, tc.paramsReady, tc.paramSet)
			}
		})
	}
}
