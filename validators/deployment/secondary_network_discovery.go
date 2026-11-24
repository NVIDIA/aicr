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
	"net/netip"
	"regexp"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"aicr.run/pkg/errors"
	"aicr.run/pkg/recipe"
)

// The manifest kinds secondary-network discovery reads, and the API group each
// must carry. The SR-IOV Network Operator renders a SriovNetwork or
// SriovIBNetwork into a NetworkAttachmentDefinition of the same name; an
// nv-ipam IPPool is read only for its subnet.
const (
	kindSriovNetwork             = "SriovNetwork"
	kindSriovIBNetwork           = "SriovIBNetwork"
	kindNetworkAttachmentDef     = "NetworkAttachmentDefinition"
	kindNVIPAMIPPool             = "IPPool"
	sriovNetworkAPIGroup         = "sriovnetwork.openshift.io"
	networkAttachmentDefAPIGroup = "k8s.cni.cncf.io"
	nvIPAMAPIGroup               = "nv-ipam.nvidia.com"
)

const (
	// nadResourceNameAnnotation names the extended resource whose allocated
	// device Multus hands to the attachment's CNI.
	nadResourceNameAnnotation = "k8s.v1.cni.cncf.io/resourceName"

	// nvIPAMType is the CNI IPAM type of nv-ipam, whose poolName names the
	// IPPool (or a comma-separated pair, for dual stack) to allocate from.
	nvIPAMType = "nv-ipam"

	sriovIBNetworksComponent = "sriov-ib-networks"
)

// secondaryNetworkKindLine selects, from raw manifest content, the files that
// can declare a secondary network. Files of the components in
// alwaysRenderedNetworkComponents are rendered whether or not they match.
var secondaryNetworkKindLine = regexp.MustCompile(
	`(?m)^kind:[ \t]*(SriovIBNetwork|SriovNetwork|NetworkAttachmentDefinition|IPPool)[ \t]*$`)

// alwaysRenderedNetworkComponents are the components whose every manifest is
// rendered by discovery, not only the ones whose raw content names a
// secondary-network kind: they own the fabric, so a NAD added to any of their
// files must never be missed because of how its kind line is written.
var alwaysRenderedNetworkComponents = map[string]bool{
	networkOperatorComponent: true,
	sriovIBNetworksComponent: true,
}

// attachmentKind says how a secondary network reaches the cluster.
type attachmentKind string

const (
	// attachmentKindSRIOV is a NAD the SR-IOV Network Operator renders from a
	// SriovNetwork or SriovIBNetwork; its attachment carries a VF's PCI
	// device info.
	attachmentKindSRIOV attachmentKind = "sriov"
	// attachmentKindNAD is a NetworkAttachmentDefinition shipped as-is.
	attachmentKindNAD attachmentKind = "nad"
)

// secondaryNetwork is one NetworkAttachmentDefinition the recipe ships.
type secondaryNetwork struct {
	// Namespace and Name locate the NAD a pod annotation references.
	Namespace string
	Name      string
	// Resource is the extended resource a pod requests to get the network's
	// device, or "" when the NAD names none.
	Resource string
	Kind     attachmentKind
	// Source names the manifest object and component that declared it.
	Source string
	// IPAMDeclared reports whether the network's CNI config assigns an
	// address (an IPAM block with a type).
	IPAMDeclared bool
	// IPAMPools are the nv-ipam pool names the IPAM block allocates from.
	IPAMPools []string
}

// key is the "<namespace>/<name>" reference Multus reports in network-status.
func (n secondaryNetwork) key() string { return n.Namespace + "/" + n.Name }

// declaredSecondaryNetworks is what the recipe ships for secondary networks:
// every NAD, sorted by key, and the subnet of every nv-ipam IPPool by name.
type declaredSecondaryNetworks struct {
	networks    []secondaryNetwork
	poolSubnets map[string]netip.Prefix
}

// anyIPAM reports whether any declared network assigns an address.
func (d declaredSecondaryNetworks) anyIPAM() bool {
	for _, n := range d.networks {
		if n.IPAMDeclared {
			return true
		}
	}
	return false
}

// discoverSecondaryNetworks derives the NADs the recipe ships by rendering its
// manifests with each component's effective values, as rdmaFabricResources
// does for the fabric's device-plugin resources. Every ManifestFiles and
// PreManifestFiles entry of every ref is a candidate; it is rendered when its
// component is in alwaysRenderedNetworkComponents or its raw content has a
// secondaryNetworkKindLine, so file names play no part.
//
// dp is the data provider the manifests and values are read from; nil reads
// the embedded catalog.
//
// Fail-closed contract: a read, render, or parse failure is an error, and so
// is the same NAD or IPPool declared twice with different content. "Could not
// derive the networks" must never read as "the recipe ships none".
func discoverSecondaryNetworks(goCtx context.Context, dp recipe.DataProvider, refs []recipe.ComponentRef) (declaredSecondaryNetworks, error) {
	out := declaredSecondaryNetworks{poolSubnets: map[string]netip.Prefix{}}
	byKey := map[string]secondaryNetwork{}

	for _, ref := range refs {
		paths := append(append([]string(nil), ref.PreManifestFiles...), ref.ManifestFiles...)
		for _, path := range paths {
			render, err := manifestMayDeclareSecondaryNetwork(goCtx, dp, ref, path)
			if err != nil {
				return declaredSecondaryNetworks{}, err
			}
			if !render {
				continue
			}
			rendered, err := renderComponentManifest(goCtx, dp, ref, path, "secondary network")
			if err != nil {
				return declaredSecondaryNetworks{}, err
			}
			networks, pools, err := parseSecondaryNetworkManifest(rendered, ref.Namespace)
			if err != nil {
				return declaredSecondaryNetworks{}, errors.Wrap(errors.ErrCodeInternal,
					fmt.Sprintf("failed to parse secondary networks in manifest %s for component %s", path, ref.Name), err)
			}
			if err := mergeSecondaryNetworks(byKey, networks, ref.Name); err != nil {
				return declaredSecondaryNetworks{}, err
			}
			if err := mergePoolSubnets(out.poolSubnets, pools); err != nil {
				return declaredSecondaryNetworks{}, err
			}
		}
	}

	for _, n := range byKey {
		out.networks = append(out.networks, n)
	}
	sort.Slice(out.networks, func(i, j int) bool { return out.networks[i].key() < out.networks[j].key() })
	return out, nil
}

// manifestMayDeclareSecondaryNetwork reports whether discovery renders path.
func manifestMayDeclareSecondaryNetwork(goCtx context.Context, dp recipe.DataProvider, ref recipe.ComponentRef, path string) (bool, error) {
	if alwaysRenderedNetworkComponents[ref.Name] {
		return true, nil
	}
	content, err := recipe.GetManifestContentWithContext(goCtx, dp, path)
	if err != nil {
		return false, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to load manifest %s for component %s", path, ref.Name), err)
	}
	return secondaryNetworkKindLine.Match(content), nil
}

// mergeSecondaryNetworks adds networks to byKey, tagging each with component.
// A key already present with different content is an error.
func mergeSecondaryNetworks(byKey map[string]secondaryNetwork, networks []secondaryNetwork, component string) error {
	for _, n := range networks {
		n.Source += " (component " + component + ")"
		prev, dup := byKey[n.key()]
		if !dup {
			byKey[n.key()] = n
			continue
		}
		if prev.Resource != n.Resource || prev.Kind != n.Kind || prev.IPAMDeclared != n.IPAMDeclared ||
			strings.Join(prev.IPAMPools, ",") != strings.Join(n.IPAMPools, ",") {

			return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
				"NetworkAttachmentDefinition %s is declared twice with different content: %s and %s",
				n.key(), prev.Source, n.Source))
		}
	}
	return nil
}

// mergePoolSubnets adds pools to dst; a pool name already present with a
// different subnet is an error.
func mergePoolSubnets(dst, pools map[string]netip.Prefix) error {
	for name, subnet := range pools {
		if prev, dup := dst[name]; dup && prev != subnet {
			return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
				"nv-ipam IPPool %s is declared twice with different subnets: %s and %s", name, prev, subnet))
		}
		dst[name] = subnet
	}
	return nil
}

// manifestTypeMeta is decoded first, so a document of another kind is never
// decoded into networkManifestDoc, whose field types it need not share.
type manifestTypeMeta struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
}

// networkManifestDoc is the union of the fields discovery reads from the
// secondary-network kinds; each kind fills only its own.
type networkManifestDoc struct {
	Metadata struct {
		Name        string            `json:"name"`
		Namespace   string            `json:"namespace"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		NetworkNamespace string `json:"networkNamespace"`
		ResourceName     string `json:"resourceName"`
		IPAM             string `json:"ipam"`
		Config           string `json:"config"`
		Subnet           string `json:"subnet"`
	} `json:"spec"`
}

// parseSecondaryNetworkManifest extracts the NADs and nv-ipam IPPool subnets
// declared in rendered manifest content. releaseNamespace is the namespace an
// object without metadata.namespace is applied to. Documents of other kinds,
// or of a recognized kind in another API group, are skipped.
func parseSecondaryNetworkManifest(rendered []byte, releaseNamespace string) ([]secondaryNetwork, map[string]netip.Prefix, error) {
	var networks []secondaryNetwork
	pools := map[string]netip.Prefix{}
	for _, raw := range strings.Split(string(rendered), "\n---") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var tm manifestTypeMeta
		if err := yaml.Unmarshal([]byte(raw), &tm); err != nil {
			return nil, nil, errors.Wrap(errors.ErrCodeInternal, "failed to decode manifest document", err)
		}
		group, _, _ := strings.Cut(tm.APIVersion, "/")
		isSriov := (tm.Kind == kindSriovNetwork || tm.Kind == kindSriovIBNetwork) && group == sriovNetworkAPIGroup
		isNAD := tm.Kind == kindNetworkAttachmentDef && group == networkAttachmentDefAPIGroup
		isPool := tm.Kind == kindNVIPAMIPPool && group == nvIPAMAPIGroup
		if !isSriov && !isNAD && !isPool {
			continue
		}
		var doc networkManifestDoc
		if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
			return nil, nil, errors.Wrap(errors.ErrCodeInternal, "failed to decode "+tm.Kind+" document", err)
		}
		switch {
		case isPool:
			subnet, err := poolSubnet(doc)
			if err != nil {
				return nil, nil, err
			}
			pools[doc.Metadata.Name] = subnet
		case isSriov:
			n, err := sriovAttachment(tm.Kind, doc, releaseNamespace)
			if err != nil {
				return nil, nil, err
			}
			networks = append(networks, n)
		default:
			n, err := nadAttachment(doc, releaseNamespace)
			if err != nil {
				return nil, nil, err
			}
			networks = append(networks, n)
		}
	}
	return networks, pools, nil
}

// poolSubnet returns an IPPool's spec.subnet.
func poolSubnet(doc networkManifestDoc) (netip.Prefix, error) {
	if doc.Metadata.Name == "" {
		return netip.Prefix{}, errors.New(errors.ErrCodeInternal, "IPPool declares no metadata.name")
	}
	subnet, err := netip.ParsePrefix(strings.TrimSpace(doc.Spec.Subnet))
	if err != nil {
		return netip.Prefix{}, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("IPPool %s declares an unparseable subnet %q", doc.Metadata.Name, doc.Spec.Subnet), err)
	}
	return subnet.Masked(), nil
}

// sriovAttachment describes the NAD the SR-IOV Network Operator renders for a
// SriovNetwork or SriovIBNetwork: same name, in spec.networkNamespace (else
// the object's own namespace), with the operator-prefixed resource.
func sriovAttachment(kind string, doc networkManifestDoc, releaseNamespace string) (secondaryNetwork, error) {
	name := doc.Metadata.Name
	if name == "" {
		return secondaryNetwork{}, errors.New(errors.ErrCodeInternal, kind+" declares no metadata.name")
	}
	namespace := firstNonEmpty(doc.Spec.NetworkNamespace, doc.Metadata.Namespace, releaseNamespace)
	if namespace == "" {
		return secondaryNetwork{}, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("%s %s has no networkNamespace and no namespace", kind, name))
	}
	if doc.Spec.ResourceName == "" {
		return secondaryNetwork{}, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("%s %s declares no resourceName", kind, name))
	}
	resource := doc.Spec.ResourceName
	if !strings.Contains(resource, "/") {
		resource = sriovOperatorResourcePrefix + "/" + resource
	}
	ipam, err := parseIPAMJSON(doc.Spec.IPAM)
	if err != nil {
		return secondaryNetwork{}, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("%s %s declares an unparseable spec.ipam", kind, name), err)
	}
	return secondaryNetwork{
		Namespace:    namespace,
		Name:         name,
		Resource:     resource,
		Kind:         attachmentKindSRIOV,
		Source:       kind + " " + name,
		IPAMDeclared: ipam.declared,
		IPAMPools:    ipam.pools,
	}, nil
}

// nadAttachment describes a NetworkAttachmentDefinition shipped as-is. Its
// resource comes from the resourceName annotation; an empty spec.config means
// Multus reads the CNI config from the node, so no IPAM is asserted.
func nadAttachment(doc networkManifestDoc, releaseNamespace string) (secondaryNetwork, error) {
	name := doc.Metadata.Name
	if name == "" {
		return secondaryNetwork{}, errors.New(errors.ErrCodeInternal, "NetworkAttachmentDefinition declares no metadata.name")
	}
	namespace := firstNonEmpty(doc.Metadata.Namespace, releaseNamespace)
	if namespace == "" {
		return secondaryNetwork{}, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("NetworkAttachmentDefinition %s has no namespace", name))
	}
	ipam, err := parseNADConfigIPAM(doc.Spec.Config)
	if err != nil {
		return secondaryNetwork{}, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("NetworkAttachmentDefinition %s declares an unparseable spec.config", name), err)
	}
	return secondaryNetwork{
		Namespace:    namespace,
		Name:         name,
		Resource:     strings.TrimSpace(doc.Metadata.Annotations[nadResourceNameAnnotation]),
		Kind:         attachmentKindNAD,
		Source:       kindNetworkAttachmentDef + " " + name,
		IPAMDeclared: ipam.declared,
		IPAMPools:    ipam.pools,
	}, nil
}

// cniIPAM is the part of a CNI IPAM block discovery reads.
type cniIPAM struct {
	Type     string `json:"type"`
	PoolName string `json:"poolName"`
}

// cniPluginConf is one CNI plugin config, or a single-plugin network config.
type cniPluginConf struct {
	Type string   `json:"type"`
	IPAM *cniIPAM `json:"ipam"`
}

// cniNetConf is a network config: a single plugin, or a conflist's plugins.
type cniNetConf struct {
	cniPluginConf
	Plugins []cniPluginConf `json:"plugins"`
}

// ipamDeclaration is what a network's CNI config says about addressing.
type ipamDeclaration struct {
	declared bool
	pools    []string
}

// add folds one IPAM block in: a block with a type assigns an address, and an
// nv-ipam block names its pools.
func (d *ipamDeclaration) add(ipam *cniIPAM) {
	if ipam == nil || strings.TrimSpace(ipam.Type) == "" {
		return
	}
	d.declared = true
	if ipam.Type != nvIPAMType {
		return
	}
	for _, pool := range strings.Split(ipam.PoolName, ",") {
		if pool = strings.TrimSpace(pool); pool != "" {
			d.pools = append(d.pools, pool)
		}
	}
}

// parseIPAMJSON reads a standalone IPAM block (SriovNetwork spec.ipam). Empty
// means no IPAM.
func parseIPAMJSON(raw string) (ipamDeclaration, error) {
	var d ipamDeclaration
	if strings.TrimSpace(raw) == "" {
		return d, nil
	}
	var ipam cniIPAM
	if err := json.Unmarshal([]byte(raw), &ipam); err != nil {
		return d, errors.Wrap(errors.ErrCodeInternal, "failed to decode IPAM JSON", err)
	}
	d.add(&ipam)
	return d, nil
}

// parseNADConfigIPAM reads the IPAM blocks of a NAD's spec.config, top-level
// or in any plugin of a conflist. Empty config means no IPAM.
func parseNADConfigIPAM(config string) (ipamDeclaration, error) {
	var d ipamDeclaration
	if strings.TrimSpace(config) == "" {
		return d, nil
	}
	var conf cniNetConf
	if err := json.Unmarshal([]byte(config), &conf); err != nil {
		return d, errors.Wrap(errors.ErrCodeInternal, "failed to decode CNI config JSON", err)
	}
	d.add(conf.IPAM)
	for i := range conf.Plugins {
		d.add(conf.Plugins[i].IPAM)
	}
	return d, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
