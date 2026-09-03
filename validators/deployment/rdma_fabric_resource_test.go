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
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/validators/helper"
)

// TestRDMAFabricResource_RealManifests derives the fabric resource from the
// actual embedded catalog manifests, so the gate's parser and the shipped
// NicClusterPolicies cannot drift. The AKS case doubles as the pin between
// this parser and helper.AKSRdmaSharedResource (the NCCL consumer's constant).
func TestRDMAFabricResource_RealManifests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		manifest string
		want     string
	}{
		{
			// Pin: manifest-derived == the NCCL consumer's constant.
			name:     "AKS rdmaSharedDevicePlugin (hca_shared_devices_a)",
			manifest: "components/network-operator/manifests/nic-cluster-policy-aks.yaml",
			want:     helper.AKSRdmaSharedResource,
		},
		{
			name:     "OKE GB200 rdmaSharedDevicePlugin (IB shared HCAs)",
			manifest: "components/network-operator/manifests/nic-cluster-policy-oke-gb200.yaml",
			want:     "nvidia.com/mlnxnics",
		},
		{
			name:     "OKE L40S sriovDevicePlugin (RoCE VFs)",
			manifest: "components/network-operator/manifests/nic-cluster-policy-oke-l40s.yaml",
			want:     "nvidia.com/mlnxnics",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ref := recipe.ComponentRef{
				Name:          networkOperatorComponent,
				Namespace:     "network-operator",
				ManifestFiles: []string{tt.manifest},
			}
			got, err := rdmaFabricResource(context.Background(), ref)
			if err != nil {
				t.Fatalf("rdmaFabricResource(%s) error = %v, want nil", tt.manifest, err)
			}
			if got != tt.want {
				t.Errorf("rdmaFabricResource(%s) = %q, want %q", tt.manifest, got, tt.want)
			}
		})
	}
}

// TestRDMAFabricResource_FailsClosed proves derivation failures are errors,
// never a silent skip: an unreadable manifest path and distinct resources
// across manifests both fail. (The zero-derivable-resources branch is
// exercised at the parser level in TestNICClusterPolicyResources_ParseShapes
// — no embedded marker-named manifest without a device-plugin block exists
// to drive it end to end.)
func TestRDMAFabricResource_FailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		manifestFiles []string
		wantErrSub    string
	}{
		{
			name:          "missing manifest path fails closed",
			manifestFiles: []string{"components/network-operator/manifests/nic-cluster-policy-nonexistent.yaml"},
			wantErrSub:    "failed to load NicClusterPolicy manifest",
		},
		{
			name: "two manifests with distinct resources fail closed",
			manifestFiles: []string{
				"components/network-operator/manifests/nic-cluster-policy-aks.yaml",
				"components/network-operator/manifests/nic-cluster-policy-oke-gb200.yaml",
			},
			wantErrSub: "multiple distinct RDMA fabric resources",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ref := recipe.ComponentRef{
				Name:          networkOperatorComponent,
				Namespace:     "network-operator",
				ManifestFiles: tt.manifestFiles,
			}
			_, err := rdmaFabricResource(context.Background(), ref)
			if err == nil {
				t.Fatalf("rdmaFabricResource() error = nil, want error containing %q", tt.wantErrSub)
			}
			if !strings.Contains(err.Error(), tt.wantErrSub) {
				t.Fatalf("rdmaFabricResource() error = %v, want substring %q", err, tt.wantErrSub)
			}
		})
	}
}

// TestNICClusterPolicyResources_ParseShapes exercises the parser on synthetic
// documents: default-prefix fallback for both plugin kinds, non-NCP documents
// skipped, malformed config JSON failing closed, and a policy with neither
// device-plugin block yielding no resources.
func TestNICClusterPolicyResources_ParseShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		rendered   string
		want       []string
		wantErrSub string
	}{
		{
			name: "rdmaShared default prefix applied",
			rendered: `apiVersion: mellanox.com/v1alpha1
kind: NicClusterPolicy
spec:
  rdmaSharedDevicePlugin:
    config: '{"configList":[{"resourceName":"hca_shared_devices_a"}]}'
`,
			want: []string{"rdma/hca_shared_devices_a"},
		},
		{
			name: "sriov default prefix applied",
			rendered: `kind: NicClusterPolicy
spec:
  sriovDevicePlugin:
    config: '{"resourceList":[{"resourceName":"vf_pool"}]}'
`,
			want: []string{"intel.com/vf_pool"},
		},
		{
			name: "non-NCP document skipped, no resources",
			rendered: `kind: ConfigMap
metadata:
  name: unrelated
`,
			want: nil,
		},
		{
			name: "config entry without resourceName fails closed",
			rendered: `kind: NicClusterPolicy
spec:
  rdmaSharedDevicePlugin:
    config: '{"configList":[{"resourcePrefix":"rdma"}]}'
`,
			wantErrSub: "declares no resourceName",
		},
		{
			name: "malformed config JSON fails closed",
			rendered: `kind: NicClusterPolicy
spec:
  rdmaSharedDevicePlugin:
    config: 'not-json'
`,
			wantErrSub: "failed to decode rdmaSharedDevicePlugin config JSON",
		},
		{
			name: "policy without device-plugin blocks yields none",
			rendered: `kind: NicClusterPolicy
spec:
  ofedDriver:
    image: doca-driver
`,
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := nicClusterPolicyResources([]byte(tt.rendered))
			if tt.wantErrSub != "" {
				if err == nil {
					t.Fatalf("nicClusterPolicyResources() error = nil, want error containing %q", tt.wantErrSub)
				}
				if !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("nicClusterPolicyResources() error = %v, want substring %q", err, tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("nicClusterPolicyResources() error = %v, want nil", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("nicClusterPolicyResources() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("nicClusterPolicyResources()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
