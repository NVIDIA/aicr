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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
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
	// nadResourceNameAnnotation names the extended resource a pod requests to
	// get the network's device.
	nadResourceNameAnnotation = "k8s.v1.cni.cncf.io/resourceName"
)

// The CNI and IPAM plugin types discovery reads. nv-ipam's poolName names the
// IPPool (or a comma-separated pair, for dual stack) to allocate from;
// host-local and whereabouts carry their ranges inline; the sriov and
// ib-sriov CNIs attach an SR-IOV VF.
const (
	nvIPAMType          = "nv-ipam"
	hostLocalIPAMType   = "host-local"
	whereaboutsIPAMType = "whereabouts"
	sriovCNIType        = "sriov"
	ibSRIOVCNIType      = "ib-sriov"
)

// secondaryNetworkKindNames are matched anywhere in a manifest's text, so
// quoting, flow or JSON style, and comments cannot hide a declaration.
var secondaryNetworkKindNames = [][]byte{
	[]byte(kindSriovNetwork), []byte(kindSriovIBNetwork), []byte(kindNetworkAttachmentDef), []byte(kindNVIPAMIPPool),
}

// templateActionOpen starts every action of the manifest template language,
// so a file without it renders to its own text.
var templateActionOpen = []byte("{{")

// attachmentKind says how a secondary network reaches the cluster: as a NAD
// the SR-IOV Network Operator renders from a SriovNetwork or SriovIBNetwork
// (attachmentKindSRIOV), or as a NetworkAttachmentDefinition the recipe ships
// as-is (attachmentKindNAD). Whether the attachment must carry a VF's PCI
// address is secondaryNetwork.RequiresPCI, not the kind, because a shipped
// NAD can name the sriov CNI too.
type attachmentKind string

const (
	attachmentKindSRIOV attachmentKind = "sriov"
	attachmentKindNAD   attachmentKind = "nad"
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
	// RequiresPCI reports whether the network attaches an SR-IOV VF (an
	// operator-rendered kind, or a sriov or ib-sriov plugin in a shipped
	// NAD), so its network-status entry must carry the VF's PCI address.
	RequiresPCI bool
	// IPAMDeclared reports whether the network's CNI config assigns an
	// address (an IPAM block with a type).
	IPAMDeclared bool
	// IPAMPools are the nv-ipam pool names the IPAM block allocates from.
	IPAMPools []string
	// IPAMSubnets are the address ranges the IPAM block declares inline
	// (host-local subnet or ranges, whereabouts range or ipRanges).
	IPAMSubnets []netip.Prefix
	// IPAMUnverified names the IPAM types whose address range discovery does
	// not read, so the addresses they assign are not bounded.
	IPAMUnverified []string
	// declaration is the declaring object's kind, resource and spec in
	// canonical JSON, so two declarations of one NAD compare in full.
	declaration string
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
// PreManifestFiles entry of every ref is a candidate (networkManifest), so
// file names play no part, and a kind that only a template action produces,
// from values or otherwise, is found in the rendered output.
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
			rendered, candidate, err := networkManifest(goCtx, dp, ref, path)
			if err != nil {
				return declaredSecondaryNetworks{}, err
			}
			if !candidate {
				continue
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

// networkManifest returns path rendered, and whether the rendered text names a
// secondary-network kind. A file is skipped without rendering only when it has
// no template action, so its text is its rendered output, and that text names
// no kind. Every templated file is rendered with its component's values, and
// a render failure is an error.
func networkManifest(goCtx context.Context, dp recipe.DataProvider, ref recipe.ComponentRef, path string) ([]byte, bool, error) {
	content, err := recipe.GetManifestContentWithContext(goCtx, dp, path)
	if err != nil {
		return nil, false, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to load manifest %s for component %s", path, ref.Name), err)
	}
	if !bytes.Contains(content, templateActionOpen) && !namesSecondaryNetworkKind(content) {
		return nil, false, nil
	}
	rendered, err := renderComponentManifest(goCtx, dp, ref, path, "secondary network")
	if err != nil {
		return nil, false, err
	}
	return rendered, namesSecondaryNetworkKind(rendered), nil
}

func namesSecondaryNetworkKind(content []byte) bool {
	for _, kind := range secondaryNetworkKindNames {
		if bytes.Contains(content, kind) {
			return true
		}
	}
	return false
}

// mergeSecondaryNetworks adds networks to byKey, tagging each with component.
// A key already present with a different declaration (any difference in the
// declaring object's spec, such as a VLAN in the CNI config) is an error;
// identical declarations from different sources are accepted.
func mergeSecondaryNetworks(byKey map[string]secondaryNetwork, networks []secondaryNetwork, component string) error {
	for _, n := range networks {
		n.Source += " (component " + component + ")"
		prev, dup := byKey[n.key()]
		if !dup {
			byKey[n.key()] = n
			continue
		}
		if prev.declaration != n.declaration || prev.Resource != n.Resource || prev.Kind != n.Kind ||
			prev.IPAMDeclared != n.IPAMDeclared || strings.Join(prev.IPAMPools, ",") != strings.Join(n.IPAMPools, ",") {

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
		if isPool {
			subnet, err := poolSubnet(doc)
			if err != nil {
				return nil, nil, err
			}
			if prev, dup := pools[doc.Metadata.Name]; dup && prev != subnet {
				return nil, nil, errors.New(errors.ErrCodeInternal, fmt.Sprintf(
					"nv-ipam IPPool %s is declared twice with different subnets: %s and %s", doc.Metadata.Name, prev, subnet))
			}
			pools[doc.Metadata.Name] = subnet
			continue
		}
		n, err := networkFromDoc(tm.Kind, isSriov, doc, releaseNamespace)
		if err != nil {
			return nil, nil, err
		}
		if n.declaration, err = declarationOf(tm.Kind, n.Resource, []byte(raw)); err != nil {
			return nil, nil, err
		}
		networks = append(networks, n)
	}
	return networks, pools, nil
}

func networkFromDoc(kind string, isSriov bool, doc networkManifestDoc, releaseNamespace string) (secondaryNetwork, error) {
	if isSriov {
		return sriovAttachment(kind, doc, releaseNamespace)
	}
	return nadAttachment(doc, releaseNamespace)
}

// declarationOf renders a network-declaring document's kind, resource and
// spec as canonical JSON. The JSON strings a spec embeds (a NAD's config, a
// SriovNetwork's ipam) are decoded first, so formatting differences do not
// make two equal declarations differ.
func declarationOf(kind, resource string, raw []byte) (string, error) {
	var doc struct {
		Spec map[string]any `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to decode "+kind+" spec", err)
	}
	for _, field := range []string{"config", "ipam"} {
		text, ok := doc.Spec[field].(string)
		if !ok || strings.TrimSpace(text) == "" {
			continue
		}
		var decoded any
		if err := json.Unmarshal([]byte(text), &decoded); err != nil {
			return "", errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to decode %s spec.%s", kind, field), err)
		}
		doc.Spec[field] = decoded
	}
	out, err := json.Marshal(struct {
		Kind     string         `json:"kind"`
		Resource string         `json:"resource"`
		Spec     map[string]any `json:"spec"`
	}{kind, resource, doc.Spec})
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to encode "+kind+" declaration", err)
	}
	return string(out), nil
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
		Namespace:      namespace,
		Name:           name,
		Resource:       resource,
		Kind:           attachmentKindSRIOV,
		Source:         kind + " " + name,
		RequiresPCI:    true,
		IPAMDeclared:   ipam.declared,
		IPAMPools:      ipam.pools,
		IPAMSubnets:    ipam.subnets,
		IPAMUnverified: ipam.unverified,
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
	conf, err := parseNADConfig(doc.Spec.Config)
	if err != nil {
		return secondaryNetwork{}, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("NetworkAttachmentDefinition %s declares an unparseable spec.config", name), err)
	}
	return secondaryNetwork{
		Namespace:      namespace,
		Name:           name,
		Resource:       strings.TrimSpace(doc.Metadata.Annotations[nadResourceNameAnnotation]),
		Kind:           attachmentKindNAD,
		Source:         kindNetworkAttachmentDef + " " + name,
		RequiresPCI:    conf.requiresPCI,
		IPAMDeclared:   conf.ipam.declared,
		IPAMPools:      conf.ipam.pools,
		IPAMSubnets:    conf.ipam.subnets,
		IPAMUnverified: conf.ipam.unverified,
	}, nil
}

// cniIPAM is the part of a CNI IPAM block discovery reads: nv-ipam's pool
// names, host-local's subnet and ranges, and whereabouts' range and ipRanges.
type cniIPAM struct {
	Type     string `json:"type"`
	PoolName string `json:"poolName"`
	Subnet   string `json:"subnet"`
	Ranges   [][]struct {
		Subnet string `json:"subnet"`
	} `json:"ranges"`
	Range    string `json:"range"`
	IPRanges []struct {
		Range string `json:"range"`
	} `json:"ipRanges"`
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
	declared   bool
	pools      []string
	subnets    []netip.Prefix
	unverified []string
}

// add folds one IPAM block in. A block with a type assigns an address; its
// range is read from nv-ipam pool names, host-local subnets and whereabouts
// ranges, and any other type, or one of those with no range, is recorded as
// unverified. A range that does not parse is an error.
func (d *ipamDeclaration) add(ipam *cniIPAM) error {
	if ipam == nil || strings.TrimSpace(ipam.Type) == "" {
		return nil
	}
	d.declared = true
	var ranges []string
	switch ipam.Type {
	case nvIPAMType:
		before := len(d.pools)
		for _, pool := range strings.Split(ipam.PoolName, ",") {
			if pool = strings.TrimSpace(pool); pool != "" {
				d.pools = append(d.pools, pool)
			}
		}
		if len(d.pools) == before {
			d.unverified = append(d.unverified, ipam.Type)
		}
		return nil
	case hostLocalIPAMType:
		ranges = append(ranges, ipam.Subnet)
		for _, set := range ipam.Ranges {
			for _, r := range set {
				ranges = append(ranges, r.Subnet)
			}
		}
	case whereaboutsIPAMType:
		ranges = append(ranges, ipam.Range)
		for _, r := range ipam.IPRanges {
			ranges = append(ranges, r.Range)
		}
	default:
		d.unverified = append(d.unverified, ipam.Type)
		return nil
	}
	before := len(d.subnets)
	for _, r := range ranges {
		if strings.TrimSpace(r) == "" {
			continue
		}
		prefix, err := parseIPAMRange(r)
		if err != nil {
			return err
		}
		d.subnets = append(d.subnets, prefix)
	}
	if len(d.subnets) == before {
		d.unverified = append(d.unverified, ipam.Type)
	}
	return nil
}

// parseIPAMRange reads a range as a CIDR. A whereabouts start-end range
// ("10.0.0.5-10.0.0.9/24") is bounded by the CIDR its end address carries.
func parseIPAMRange(r string) (netip.Prefix, error) {
	text := strings.TrimSpace(r)
	if i := strings.LastIndex(text, "-"); i >= 0 {
		text = text[i+1:]
	}
	prefix, err := netip.ParsePrefix(text)
	if err != nil {
		return netip.Prefix{}, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("unparseable IPAM range %q", r), err)
	}
	return prefix.Masked(), nil
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
	err := d.add(&ipam)
	return d, err
}

// nadConfig is what discovery reads from a NAD's spec.config.
type nadConfig struct {
	ipam        ipamDeclaration
	requiresPCI bool
}

// parseNADConfig reads a NAD's spec.config, a single plugin or every plugin
// of a conflist: its IPAM blocks, and whether a plugin is the sriov or
// ib-sriov CNI. Empty config means no IPAM.
func parseNADConfig(config string) (nadConfig, error) {
	var c nadConfig
	if strings.TrimSpace(config) == "" {
		return c, nil
	}
	var conf cniNetConf
	if err := json.Unmarshal([]byte(config), &conf); err != nil {
		return c, errors.Wrap(errors.ErrCodeInternal, "failed to decode CNI config JSON", err)
	}
	// A conflist runs only its plugins; its top-level object is not one.
	plugins := conf.Plugins
	if len(plugins) == 0 {
		plugins = []cniPluginConf{conf.cniPluginConf}
	}
	for i := range plugins {
		if plugins[i].Type == sriovCNIType || plugins[i].Type == ibSRIOVCNIType {
			c.requiresPCI = true
		}
		if err := c.ipam.add(plugins[i].IPAM); err != nil {
			return c, err
		}
	}
	return c, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
