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
	"sort"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/validators/helper"
)

// TestRDMAFabricResources_RealManifests derives the fabric resources from the
// actual embedded catalog manifests, so the gate's parser and the shipped
// policies cannot drift. The AKS case doubles as the pin between this parser
// and helper.AKSRdmaSharedResource (the NCCL consumer's constant). The NVKE
// case covers resources that live in SriovNetworkNodePolicy manifests of a
// component other than network-operator, and the two-manifest case shows that
// distinct resources are all returned (the gate polls each).
func TestRDMAFabricResources_RealManifests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		ncpManifests []string
		extraRefs    []recipe.ComponentRef
		want         []string
	}{
		{
			// Pin: manifest-derived == the NCCL consumer's constant.
			name:         "AKS rdmaSharedDevicePlugin (hca_shared_devices_a)",
			ncpManifests: []string{"components/network-operator/manifests/nic-cluster-policy-aks.yaml"},
			want:         []string{helper.AKSRdmaSharedResource},
		},
		{
			name:         "OKE GB200 rdmaSharedDevicePlugin (IB shared HCAs)",
			ncpManifests: []string{"components/network-operator/manifests/nic-cluster-policy-oke-gb200.yaml"},
			want:         []string{"nvidia.com/mlnxnics"},
		},
		{
			name:         "OKE L40S sriovDevicePlugin (RoCE VFs)",
			ncpManifests: []string{"components/network-operator/manifests/nic-cluster-policy-oke-l40s.yaml"},
			want:         []string{"nvidia.com/mlnxnics"},
		},
		{
			name:         "NVKE GB200 SriovNetworkNodePolicy per rail (sriov-ib-networks component)",
			ncpManifests: []string{"components/network-operator/manifests/nic-cluster-policy-nvke-gb200.yaml"},
			extraRefs: []recipe.ComponentRef{{
				Name:          "sriov-ib-networks",
				Namespace:     "nvidia-network-operator",
				ManifestFiles: []string{"components/sriov-ib-networks/manifests/nvke-gb200.yaml"},
			}},
			want: []string{
				"nvidia.com/sriov_resource_rail_0",
				"nvidia.com/sriov_resource_rail_1",
				"nvidia.com/sriov_resource_rail_2",
				"nvidia.com/sriov_resource_rail_3",
			},
		},
		{
			name: "two NicClusterPolicy manifests with distinct resources are both derived",
			ncpManifests: []string{
				"components/network-operator/manifests/nic-cluster-policy-aks.yaml",
				"components/network-operator/manifests/nic-cluster-policy-oke-gb200.yaml",
			},
			want: []string{"nvidia.com/mlnxnics", helper.AKSRdmaSharedResource},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ref := recipe.ComponentRef{
				Name:          networkOperatorComponent,
				Namespace:     "network-operator",
				ManifestFiles: tt.ncpManifests,
			}
			refs := append([]recipe.ComponentRef{ref}, tt.extraRefs...)
			got, err := rdmaFabricResources(context.Background(), ref, refs)
			if err != nil {
				t.Fatalf("rdmaFabricResources(%v) error = %v, want nil", tt.ncpManifests, err)
			}
			want := append([]string(nil), tt.want...)
			sort.Strings(want)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("rdmaFabricResources(%v) = %v, want %v", tt.ncpManifests, got, want)
			}
		})
	}
}

// TestRDMAFabricResources_FailsClosed proves derivation failures are errors,
// never a silent skip: an unreadable manifest path fails, and so does a
// NicClusterPolicy without a device-plugin block when no SriovNetworkNodePolicy
// supplies the resource. (The zero-derivable-resources branch at the parser
// level is exercised in TestNICClusterPolicyResources_ParseShapes.)
func TestRDMAFabricResources_FailsClosed(t *testing.T) {
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
			name:          "NicClusterPolicy without device plugin and no SriovNetworkNodePolicy fails closed",
			manifestFiles: []string{"components/network-operator/manifests/nic-cluster-policy-nvke-gb200.yaml"},
			wantErrSub:    "no device-plugin resource could be derived",
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
			_, err := rdmaFabricResources(context.Background(), ref, []recipe.ComponentRef{ref})
			if err == nil {
				t.Fatalf("rdmaFabricResources() error = nil, want error containing %q", tt.wantErrSub)
			}
			if !strings.Contains(err.Error(), tt.wantErrSub) {
				t.Fatalf("rdmaFabricResources() error = %v, want substring %q", err, tt.wantErrSub)
			}
		})
	}
}

// TestSriovNodePolicyResources_ParseShapes exercises the SriovNetworkNodePolicy
// parser on synthetic documents: the operator prefix applied, accompanying
// IPPool/SriovIBNetwork documents skipped, and a policy without resourceName
// failing closed.
func TestSriovNodePolicyResources_ParseShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		rendered   string
		want       []string
		wantErrSub string
	}{
		{
			name: "operator prefix applied, companions skipped",
			rendered: `apiVersion: nv-ipam.nvidia.com/v1alpha1
kind: IPPool
metadata:
  name: pool
---
apiVersion: sriovnetwork.openshift.io/v1
kind: SriovNetworkNodePolicy
spec:
  resourceName: sriov_resource_rail_0
---
apiVersion: sriovnetwork.openshift.io/v1
kind: SriovIBNetwork
spec:
  resourceName: sriov_resource_rail_0
`,
			want: []string{"nvidia.com/sriov_resource_rail_0"},
		},
		{
			name: "policy without resourceName fails closed",
			rendered: `kind: SriovNetworkNodePolicy
spec:
  numVfs: 8
`,
			wantErrSub: "declares no resourceName",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := sriovNodePolicyResources([]byte(tt.rendered))
			if tt.wantErrSub != "" {
				if err == nil {
					t.Fatalf("sriovNodePolicyResources() error = nil, want error containing %q", tt.wantErrSub)
				}
				if !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("sriovNodePolicyResources() error = %v, want substring %q", err, tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("sriovNodePolicyResources() error = %v, want nil", err)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("sriovNodePolicyResources() = %v, want %v", got, tt.want)
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
