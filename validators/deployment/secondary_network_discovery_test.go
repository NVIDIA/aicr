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
	"fmt"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"

	"aicr.run/pkg/recipe"
	"aicr.run/pkg/recipe/providertest"
)

const (
	testNetworkOperatorNamespace = "nvidia-network-operator"
	secondaryNetworkCheck        = "secondary-network-attachment"
)

// railNetworks builds the n per-rail networks the NVKE manifests declare.
func railNetworks(n int, namePrefix string, source string) []secondaryNetwork {
	out := make([]secondaryNetwork, 0, n)
	for i := range n {
		out = append(out, secondaryNetwork{
			Namespace:    "default",
			Name:         fmt.Sprintf("%s%d", namePrefix, i),
			Resource:     fmt.Sprintf("nvidia.com/sriov_resource_rail_%d", i),
			Kind:         attachmentKindSRIOV,
			Source:       fmt.Sprintf("%s %s%d (component %s)", source, namePrefix, i, sriovIBNetworksComponent),
			IPAMDeclared: true,
			IPAMPools:    []string{fmt.Sprintf("nv-ipam-pool-rail-%d", i)},
		})
	}
	return out
}

// railPools builds the n per-rail IPPool subnets, 10.255.<4i>.0/22.
func railPools(n int) map[string]netip.Prefix {
	out := map[string]netip.Prefix{}
	for i := range n {
		out[fmt.Sprintf("nv-ipam-pool-rail-%d", i)] = netip.MustParsePrefix(fmt.Sprintf("10.255.%d.0/22", 4*i))
	}
	return out
}

// TestDiscoverSecondaryNetworks_RealManifests derives the networks from the
// embedded manifests the shipped leaves reference, so discovery and the
// manifests cannot drift. It covers the three shapes that ship a NAD (NVKE
// GB200 SriovIBNetworks, NVKE VR200 SriovNetworks, the OKE GB300 raw NAD with
// empty IPAM, whose file name carries no marker) and the NicClusterPolicy
// shapes that ship none.
func TestDiscoverSecondaryNetworks_RealManifests(t *testing.T) {
	t.Parallel()

	networkOperator := func(valuesFile string, manifests ...string) recipe.ComponentRef {
		return recipe.ComponentRef{
			Name:          networkOperatorComponent,
			Namespace:     testNetworkOperatorNamespace,
			ValuesFile:    valuesFile,
			ManifestFiles: manifests,
		}
	}
	sriovIBNetworks := func(manifest string) recipe.ComponentRef {
		return recipe.ComponentRef{
			Name:          sriovIBNetworksComponent,
			Namespace:     testNetworkOperatorNamespace,
			ManifestFiles: []string{manifest},
		}
	}

	tests := []struct {
		name         string
		refs         []recipe.ComponentRef
		wantNetworks []secondaryNetwork
		wantPools    map[string]netip.Prefix
	}{
		{
			name: "NVKE GB200 SriovIBNetwork per rail",
			refs: []recipe.ComponentRef{
				networkOperator("components/network-operator/values-nvke-gb200.yaml",
					"components/network-operator/manifests/nic-cluster-policy-nvke-gb200.yaml"),
				sriovIBNetworks("components/sriov-ib-networks/manifests/nvke-gb200.yaml"),
			},
			wantNetworks: railNetworks(4, "sriov-network-rail-", kindSriovIBNetwork),
			wantPools:    railPools(4),
		},
		{
			name: "NVKE VR200 SriovNetwork per rail",
			refs: []recipe.ComponentRef{
				networkOperator("components/network-operator/values-nvke-gb200.yaml",
					"components/network-operator/manifests/nic-cluster-policy-nvke-vr200.yaml"),
				sriovIBNetworks("components/sriov-ib-networks/manifests/nvke-vr200.yaml"),
			},
			wantNetworks: railNetworks(8, "roce-network-rail-", kindSriovNetwork),
			wantPools:    railPools(8),
		},
		{
			name: "OKE GB300 raw NAD with empty IPAM",
			refs: []recipe.ComponentRef{
				networkOperator("components/network-operator/values-oke-gb300.yaml",
					"components/network-operator/manifests/nic-cluster-policy-oke-gb300.yaml",
					"components/network-operator/manifests/rdma-net-oke-gb300.yaml",
					"components/network-operator/manifests/rdma-cni-oke-gb300.yaml"),
			},
			wantNetworks: []secondaryNetwork{{
				Namespace: testNetworkOperatorNamespace,
				Name:      "rdma-net",
				Resource:  "nvidia.com/mlnxnics",
				Kind:      attachmentKindNAD,
				Source:    "NetworkAttachmentDefinition rdma-net (component network-operator)",
			}},
			wantPools: map[string]netip.Prefix{},
		},
		{
			name: "AKS NicClusterPolicy ships no NAD",
			refs: []recipe.ComponentRef{networkOperator("components/network-operator/values-aks.yaml",
				"components/network-operator/manifests/nic-cluster-policy-aks.yaml")},
			wantPools: map[string]netip.Prefix{},
		},
		{
			name: "OKE GB200 NicClusterPolicy ships no NAD",
			refs: []recipe.ComponentRef{networkOperator("components/network-operator/values-oke-gb200.yaml",
				"components/network-operator/manifests/nic-cluster-policy-oke-gb200.yaml")},
			wantPools: map[string]netip.Prefix{},
		},
		{
			name: "generic GB300 NicClusterPolicy ships no NAD",
			refs: []recipe.ComponentRef{networkOperator("components/network-operator/values-generic-gb300.yaml",
				"components/network-operator/manifests/nic-cluster-policy-generic-gb300.yaml")},
			wantPools: map[string]netip.Prefix{},
		},
		{
			name: "Nebius B300 NicClusterPolicy ships no NAD",
			refs: []recipe.ComponentRef{networkOperator("components/network-operator/values-mk8s-b300.yaml",
				"components/network-operator/manifests/nic-cluster-policy-nebius-b300.yaml")},
			wantPools: map[string]netip.Prefix{},
		},
		{
			name:      "no manifests at all",
			refs:      []recipe.ComponentRef{{Name: "gpu-operator", Namespace: "gpu-operator"}},
			wantPools: map[string]netip.Prefix{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := discoverSecondaryNetworks(context.Background(), nil, tt.refs)
			if err != nil {
				t.Fatalf("discoverSecondaryNetworks() error = %v", err)
			}
			if !reflect.DeepEqual(got.networks, tt.wantNetworks) {
				t.Errorf("networks =\n%+v\nwant\n%+v", got.networks, tt.wantNetworks)
			}
			if !reflect.DeepEqual(got.poolSubnets, tt.wantPools) {
				t.Errorf("pools = %v, want %v", got.poolSubnets, tt.wantPools)
			}
		})
	}
}

// TestDiscoverSecondaryNetworks_FailsClosed proves a manifest discovery cannot
// read is an error, never "the recipe ships no network".
func TestDiscoverSecondaryNetworks_FailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		ref        recipe.ComponentRef
		wantErrSub string
	}{
		{
			name: "missing network-operator manifest",
			ref: recipe.ComponentRef{Name: networkOperatorComponent, Namespace: testNetworkOperatorNamespace,
				ManifestFiles: []string{"components/network-operator/manifests/does-not-exist.yaml"}},
			wantErrSub: "failed to load secondary network manifest",
		},
		{
			name: "missing manifest of another component",
			ref: recipe.ComponentRef{Name: "gpu-operator", Namespace: "gpu-operator",
				PreManifestFiles: []string{"components/gpu-operator/manifests/does-not-exist.yaml"}},
			wantErrSub: "failed to load manifest",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := discoverSecondaryNetworks(context.Background(), nil, []recipe.ComponentRef{tt.ref})
			if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
				t.Fatalf("discoverSecondaryNetworks() error = %v, want substring %q", err, tt.wantErrSub)
			}
		})
	}
}

// TestParseSecondaryNetworkManifest covers the parse rules on synthetic
// documents: namespace fallbacks, resource prefixing, IPAM detection in a
// single config and a conflist, foreign API groups, and the fail-closed
// shapes.
func TestParseSecondaryNetworkManifest(t *testing.T) {
	t.Parallel()

	// releaseNS "-" renders with an empty release namespace; "" means
	// "release-ns".
	tests := []struct {
		name         string
		rendered     string
		releaseNS    string
		wantNetworks []secondaryNetwork
		wantPools    map[string]netip.Prefix
		wantErrSub   string
	}{
		{
			name: "SriovNetwork without networkNamespace uses its own namespace; prefixed resource kept",
			rendered: `apiVersion: sriovnetwork.openshift.io/v1
kind: SriovNetwork
metadata:
  name: net-a
  namespace: sriov
spec:
  resourceName: example.com/vf
  ipam: '{"type":"whereabouts","range":"10.0.0.0/24"}'
`,
			wantNetworks: []secondaryNetwork{{Namespace: "sriov", Name: "net-a", Resource: "example.com/vf",
				Kind: attachmentKindSRIOV, Source: "SriovNetwork net-a", IPAMDeclared: true}},
			wantPools: map[string]netip.Prefix{},
		},
		{
			name: "SriovIBNetwork with no namespace falls back to the release namespace; empty ipam",
			rendered: `apiVersion: sriovnetwork.openshift.io/v1
kind: SriovIBNetwork
metadata:
  name: ib
spec:
  resourceName: rail
`,
			wantNetworks: []secondaryNetwork{{Namespace: "release-ns", Name: "ib", Resource: "nvidia.com/rail",
				Kind: attachmentKindSRIOV, Source: "SriovIBNetwork ib"}},
			wantPools: map[string]netip.Prefix{},
		},
		{
			name: "NAD conflist with nv-ipam in a plugin, dual-stack pools; IPPool subnet masked",
			rendered: `apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: nad
  annotations:
    k8s.v1.cni.cncf.io/resourceName: nvidia.com/hostdev
spec:
  config: '{"cniVersion":"0.4.0","plugins":[{"type":"host-device","ipam":{"type":"nv-ipam","poolName":"p4, p6"}},{"type":"rdma"}]}'
---
apiVersion: nv-ipam.nvidia.com/v1alpha1
kind: IPPool
metadata:
  name: p4
spec:
  subnet: 10.9.1.7/16
`,
			wantNetworks: []secondaryNetwork{{Namespace: "release-ns", Name: "nad", Resource: "nvidia.com/hostdev",
				Kind: attachmentKindNAD, Source: "NetworkAttachmentDefinition nad", IPAMDeclared: true,
				IPAMPools: []string{"p4", "p6"}}},
			wantPools: map[string]netip.Prefix{"p4": netip.MustParsePrefix("10.9.0.0/16")},
		},
		{
			name: "NAD single config with top-level ipam; no resource annotation",
			rendered: `apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: macvlan
  namespace: apps
spec:
  config: '{"type":"macvlan","ipam":{"type":"host-local","subnet":"10.1.0.0/24"}}'
`,
			wantNetworks: []secondaryNetwork{{Namespace: "apps", Name: "macvlan", Kind: attachmentKindNAD,
				Source: "NetworkAttachmentDefinition macvlan", IPAMDeclared: true}},
			wantPools: map[string]netip.Prefix{},
		},
		{
			name: "NAD with empty config reads the node's config: no IPAM asserted",
			rendered: `apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: from-node
  namespace: apps
`,
			wantNetworks: []secondaryNetwork{{Namespace: "apps", Name: "from-node", Kind: attachmentKindNAD,
				Source: "NetworkAttachmentDefinition from-node"}},
			wantPools: map[string]netip.Prefix{},
		},
		{
			name: "recognized kinds in a foreign API group and unrelated kinds are skipped",
			rendered: `apiVersion: example.com/v1
kind: IPPool
metadata:
  name: other
spec:
  subnet: {not: a string}
---
apiVersion: mellanox.com/v1alpha1
kind: NicClusterPolicy
spec:
  ipam: {type: object}
`,
			wantPools: map[string]netip.Prefix{},
		},
		{
			name: "SriovNetwork without resourceName fails closed",
			rendered: `apiVersion: sriovnetwork.openshift.io/v1
kind: SriovNetwork
metadata:
  name: bad
`,
			wantErrSub: "declares no resourceName",
		},
		{
			name: "malformed spec.ipam fails closed",
			rendered: `apiVersion: sriovnetwork.openshift.io/v1
kind: SriovNetwork
metadata:
  name: bad
spec:
  resourceName: r
  ipam: '{not json'
`,
			wantErrSub: "unparseable spec.ipam",
		},
		{
			name: "malformed NAD config fails closed",
			rendered: `apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: bad
spec:
  config: '{"plugins": "nope"}'
`,
			wantErrSub: "unparseable spec.config",
		},
		{
			name: "nameless IPPool fails closed",
			rendered: `apiVersion: nv-ipam.nvidia.com/v1alpha1
kind: IPPool
spec:
  subnet: 10.0.0.0/24
`,
			wantErrSub: "IPPool declares no metadata.name",
		},
		{
			name: "nameless SriovNetwork fails closed",
			rendered: `apiVersion: sriovnetwork.openshift.io/v1
kind: SriovNetwork
spec:
  resourceName: r
`,
			wantErrSub: "SriovNetwork declares no metadata.name",
		},
		{
			name: "nameless NAD fails closed",
			rendered: `apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
spec:
  config: '{"type":"macvlan"}'
`,
			wantErrSub: "NetworkAttachmentDefinition declares no metadata.name",
		},
		{
			name:       "SriovNetwork with no namespace anywhere fails closed",
			releaseNS:  "-",
			rendered:   "apiVersion: sriovnetwork.openshift.io/v1\nkind: SriovNetwork\nmetadata:\n  name: net\nspec:\n  resourceName: r\n",
			wantErrSub: "has no networkNamespace and no namespace",
		},
		{
			name:       "NAD with no namespace anywhere fails closed",
			releaseNS:  "-",
			rendered:   "apiVersion: k8s.cni.cncf.io/v1\nkind: NetworkAttachmentDefinition\nmetadata:\n  name: net\n",
			wantErrSub: "NetworkAttachmentDefinition net has no namespace",
		},
		{
			name: "IPPool with an unparseable subnet fails closed",
			rendered: `apiVersion: nv-ipam.nvidia.com/v1alpha1
kind: IPPool
metadata:
  name: pool
spec:
  subnet: ten-dot-something
`,
			wantErrSub: "unparseable subnet",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			releaseNS := "release-ns"
			if tt.releaseNS == "-" {
				releaseNS = ""
			}
			networks, pools, err := parseSecondaryNetworkManifest([]byte(tt.rendered), releaseNS)
			if tt.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("parseSecondaryNetworkManifest() error = %v, want substring %q", err, tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSecondaryNetworkManifest() error = %v", err)
			}
			if !reflect.DeepEqual(networks, tt.wantNetworks) {
				t.Errorf("networks =\n%+v\nwant\n%+v", networks, tt.wantNetworks)
			}
			if !reflect.DeepEqual(pools, tt.wantPools) {
				t.Errorf("pools = %v, want %v", pools, tt.wantPools)
			}
		})
	}
}

// TestMergeSecondaryNetworks_Conflicts proves a NAD or pool declared twice is
// accepted only when both declarations agree on every field the check uses.
func TestMergeSecondaryNetworks_Conflicts(t *testing.T) {
	t.Parallel()

	base := secondaryNetwork{Namespace: "ns", Name: "n", Resource: "r/a", Kind: attachmentKindSRIOV, Source: "net n",
		IPAMDeclared: true, IPAMPools: []string{"p1"}}

	tests := []struct {
		name    string
		mutate  func(*secondaryNetwork)
		wantErr bool
	}{
		{name: "identical redeclaration", mutate: func(*secondaryNetwork) {}},
		{name: "different source only", mutate: func(n *secondaryNetwork) { n.Source = "elsewhere" }},
		{name: "resource", mutate: func(n *secondaryNetwork) { n.Resource = "r/b" }, wantErr: true},
		{name: "kind", mutate: func(n *secondaryNetwork) { n.Kind = attachmentKindNAD }, wantErr: true},
		{name: "IPAM declared", mutate: func(n *secondaryNetwork) { n.IPAMDeclared = false }, wantErr: true},
		{name: "IPAM pools", mutate: func(n *secondaryNetwork) { n.IPAMPools = []string{"p1", "p2"} }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			byKey := map[string]secondaryNetwork{}
			if err := mergeSecondaryNetworks(byKey, []secondaryNetwork{base}, "c1"); err != nil {
				t.Fatalf("first merge error = %v", err)
			}
			second := base
			second.IPAMPools = append([]string(nil), base.IPAMPools...)
			tt.mutate(&second)
			err := mergeSecondaryNetworks(byKey, []secondaryNetwork{second}, "c2")
			if (err != nil) != tt.wantErr {
				t.Errorf("second merge error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}

	pools := map[string]netip.Prefix{"p": netip.MustParsePrefix("10.0.0.0/24")}
	if err := mergePoolSubnets(pools, map[string]netip.Prefix{"p": netip.MustParsePrefix("10.0.0.0/24")}); err != nil {
		t.Errorf("identical pool error = %v, want nil", err)
	}
	if err := mergePoolSubnets(pools, map[string]netip.Prefix{"p": netip.MustParsePrefix("10.0.1.0/24")}); err == nil {
		t.Error("conflicting pool error = nil, want error")
	}
}

// TestDiscoverSecondaryNetworks_SyntheticData covers discovery over data no
// shipped recipe carries: a component outside the fabric owners whose
// manifest names a NAD kind at column 0 is rendered and found, and two
// components declaring the same NAD differently fail.
func TestDiscoverSecondaryNetworks_SyntheticData(t *testing.T) {
	t.Parallel()

	nad := func(resource string) []byte {
		return []byte(`apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: shared
  namespace: apps
  annotations:
    k8s.v1.cni.cncf.io/resourceName: ` + resource + `
spec:
  config: '{"cniVersion":"0.3.1","type":"host-device","ipam":{}}'
`)
	}
	dp := providertest.NewMap(map[string][]byte{
		"components/custom-nets/manifests/nad.yaml":   nad("example.com/vf_a"),
		"components/other-nets/manifests/nad.yaml":    nad("example.com/vf_b"),
		"components/custom-nets/manifests/plain.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n"),
	})
	custom := recipe.ComponentRef{Name: "custom-nets", Namespace: "custom", ManifestFiles: []string{
		"components/custom-nets/manifests/plain.yaml", "components/custom-nets/manifests/nad.yaml",
	}}
	other := recipe.ComponentRef{Name: "other-nets", Namespace: "other", ManifestFiles: []string{"components/other-nets/manifests/nad.yaml"}}

	got, err := discoverSecondaryNetworks(context.Background(), dp, []recipe.ComponentRef{custom})
	if err != nil {
		t.Fatalf("discoverSecondaryNetworks() error = %v", err)
	}
	want := []secondaryNetwork{{Namespace: "apps", Name: "shared", Resource: "example.com/vf_a", Kind: attachmentKindNAD,
		Source: "NetworkAttachmentDefinition shared (component custom-nets)"}}
	if !reflect.DeepEqual(got.networks, want) {
		t.Errorf("networks = %+v, want %+v", got.networks, want)
	}

	_, err = discoverSecondaryNetworks(context.Background(), dp, []recipe.ComponentRef{custom, other})
	if err == nil || !strings.Contains(err.Error(), "apps/shared is declared twice with different content") {
		t.Errorf("discoverSecondaryNetworks() error = %v, want the duplicate-declaration failure", err)
	}
}

// TestShippedLeavesDeclareAttachmentCheck ties the check to the catalog: a
// leaf that ships a secondary network must run secondary-network-attachment
// in its deployment phase, and a leaf that runs it must ship one, so a new
// SR-IOV leaf cannot reintroduce the #3474 blind spot and the check is never
// declared where it can only skip.
func TestShippedLeavesDeclareAttachmentCheck(t *testing.T) {
	t.Parallel()

	leaves, err := recipe.ResolveLeaves(context.Background(), recipe.ResolveLeavesOptions{})
	if err != nil {
		t.Fatalf("ResolveLeaves() error = %v", err)
	}
	var shipping []string
	for _, leaf := range leaves {
		if leaf.Err != nil {
			t.Errorf("leaf %s failed to resolve: %v", leaf.Entry.Name, leaf.Err)
			continue
		}
		declared, derr := discoverSecondaryNetworks(context.Background(), nil, enabledComponentRefs(leaf.Result.ComponentRefs))
		if derr != nil {
			t.Errorf("leaf %s: discoverSecondaryNetworks() error = %v", leaf.Entry.Name, derr)
			continue
		}
		var checks []string
		if v := leaf.Result.Validation; v != nil && v.Deployment != nil {
			checks = v.Deployment.Checks
		}
		ships, runs := len(declared.networks) > 0, slices.Contains(checks, secondaryNetworkCheck)
		if ships {
			shipping = append(shipping, leaf.Entry.Name)
		}
		switch {
		case ships && !runs:
			t.Errorf("leaf %s ships secondary network(s) %v but its deployment checks %v omit %s",
				leaf.Entry.Name, networkKeys(declared.networks), checks, secondaryNetworkCheck)
		case runs && !ships:
			t.Errorf("leaf %s declares %s but ships no secondary network, so the check can only skip",
				leaf.Entry.Name, secondaryNetworkCheck)
		}
	}
	if len(shipping) == 0 {
		t.Fatal("no leaf ships a secondary network; discovery or the catalog changed and this test proves nothing")
	}
	t.Logf("leaves shipping a secondary network: %v", shipping)
}

func networkKeys(networks []secondaryNetwork) []string {
	out := make([]string, 0, len(networks))
	for _, n := range networks {
		out = append(out, n.key())
	}
	return out
}
