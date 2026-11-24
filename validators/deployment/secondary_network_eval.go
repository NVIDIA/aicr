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
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"aicr.run/pkg/errors"
)

const (
	// multusNetworkStatusAnnotation is where Multus records, per attached
	// network, the interface and addresses it configured in the pod.
	multusNetworkStatusAnnotation = "k8s.v1.cni.cncf.io/network-status"

	// ovnNodeSubnetsAnnotation is OVN-Kubernetes' per-node pod subnet
	// allocation: a JSON map of network name to one CIDR or a list of them.
	// OVN-Kubernetes leaves node.spec.podCIDRs empty.
	ovnNodeSubnetsAnnotation = "k8s.ovn.org/node-subnets"
)

// The probe prints /sys/class/net and `ip -o addr` between these lines.
const (
	probeSysNetBegin = "AICR-NETPROBE-SYSNET-BEGIN"
	probeSysNetEnd   = "AICR-NETPROBE-SYSNET-END"
	probeAddrBegin   = "AICR-NETPROBE-ADDR-BEGIN"
	probeAddrEnd     = "AICR-NETPROBE-ADDR-END"
)

// attachmentProbeScript is the probe container's shell. It always exits 0:
// the verdict comes from the markers and network-status, and a non-Succeeded
// phase means the probe never ran.
const attachmentProbeScript = "echo " + probeSysNetBegin + "; ls -1 /sys/class/net; echo " + probeSysNetEnd + "; " +
	"echo " + probeAddrBegin + "; ip -o addr; echo " + probeAddrEnd + "; exit 0"

// networkStatusEntry is one element of the network-status annotation.
type networkStatusEntry struct {
	Name       string             `json:"name"`
	Interface  string             `json:"interface"`
	IPs        []string           `json:"ips"`
	Default    bool               `json:"default"`
	DeviceInfo *networkDeviceInfo `json:"device-info"`
}

// networkDeviceInfo is the device-info block a device-plugin-backed CNI
// reports for the device it attached.
type networkDeviceInfo struct {
	Type string            `json:"type"`
	PCI  *networkDevicePCI `json:"pci"`
}

type networkDevicePCI struct {
	PCIAddress string `json:"pci-address"`
	RDMADevice string `json:"rdma-device"`
}

// parseNetworkStatus decodes the network-status annotation. An empty value is
// an error: Multus writes the annotation for every pod it handles, so its
// absence means no meta-plugin attached anything.
func parseNetworkStatus(annotation string) ([]networkStatusEntry, error) {
	if strings.TrimSpace(annotation) == "" {
		return nil, errors.New(errors.ErrCodeNotFound,
			"the pod has no "+multusNetworkStatusAnnotation+" annotation, so Multus attached no network")
	}
	var entries []networkStatusEntry
	if err := json.Unmarshal([]byte(annotation), &entries); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"the pod's "+multusNetworkStatusAnnotation+" annotation is not a JSON list", err)
	}
	return entries, nil
}

// parseProbeOutput reads the probe's log: the interface names under
// /sys/class/net and the `ip -o addr` lines. It fails closed when a marker is
// missing or out of order, or no interface was listed: truncated output must
// not read as "interface absent", nor as "interface present".
func parseProbeOutput(logs string) (interfaces, addrLines []string, err error) {
	sysNet, ok := sectionBetween(logs, probeSysNetBegin, probeSysNetEnd)
	if !ok {
		return nil, nil, errors.New(errors.ErrCodeInternal,
			"probe output lacks the /sys/class/net section markers; the probe did not run to completion")
	}
	addr, ok := sectionBetween(logs, probeAddrBegin, probeAddrEnd)
	if !ok {
		return nil, nil, errors.New(errors.ErrCodeInternal,
			"probe output lacks the ip addr section markers; the probe did not run to completion")
	}
	interfaces = strings.Fields(sysNet)
	if len(interfaces) == 0 {
		return nil, nil, errors.New(errors.ErrCodeInternal, "probe listed no interface under /sys/class/net")
	}
	for _, line := range strings.Split(addr, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			addrLines = append(addrLines, line)
		}
	}
	return interfaces, addrLines, nil
}

// sectionBetween returns the text between the line holding begin and the
// next line holding end.
func sectionBetween(logs, begin, end string) (string, bool) {
	_, rest, ok := strings.Cut(logs, begin+"\n")
	if !ok {
		return "", false
	}
	section, _, ok := strings.Cut(rest, end)
	return section, ok
}

// clusterPodCIDRs collects the pod network CIDRs the nodes declare:
// node.spec.podCIDRs (or the legacy podCIDR) plus every subnet in the
// OVN-Kubernetes node-subnets annotation. The result is deduplicated and
// sorted. A value that does not parse is an error: a pod network that cannot
// be read must not read as "no overlap".
func clusterPodCIDRs(nodes []corev1.Node) ([]netip.Prefix, error) {
	seen := map[netip.Prefix]bool{}
	for i := range nodes {
		node := &nodes[i]
		cidrs := append([]string(nil), node.Spec.PodCIDRs...)
		if len(cidrs) == 0 && node.Spec.PodCIDR != "" {
			cidrs = append(cidrs, node.Spec.PodCIDR)
		}
		if raw := node.Annotations[ovnNodeSubnetsAnnotation]; raw != "" {
			ovn, err := parseOVNNodeSubnets(raw)
			if err != nil {
				return nil, errors.Wrap(errors.ErrCodeInternal,
					fmt.Sprintf("node %s has an unparseable %s annotation", node.Name, ovnNodeSubnetsAnnotation), err)
			}
			cidrs = append(cidrs, ovn...)
		}
		for _, c := range cidrs {
			p, err := netip.ParsePrefix(strings.TrimSpace(c))
			if err != nil {
				return nil, errors.Wrap(errors.ErrCodeInternal,
					fmt.Sprintf("node %s declares an unparseable pod CIDR %q", node.Name, c), err)
			}
			seen[p.Masked()] = true
		}
	}
	out := make([]netip.Prefix, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

// parseOVNNodeSubnets reads the node-subnets annotation, whose values are a
// CIDR string or a list of them per network.
func parseOVNNodeSubnets(raw string) ([]string, error) {
	var byNetwork map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &byNetwork); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "annotation is not a JSON object", err)
	}
	var out []string
	for network, value := range byNetwork {
		var one string
		if err := json.Unmarshal(value, &one); err == nil {
			out = append(out, one)
			continue
		}
		var many []string
		if err := json.Unmarshal(value, &many); err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("network %q maps to neither a CIDR nor a list of CIDRs", network), err)
		}
		out = append(out, many...)
	}
	return out, nil
}

// attachmentEvaluation is the verdict on one probe's attachment. problems are
// the failed assertions; notes record what was deliberately not asserted.
type attachmentEvaluation struct {
	problems []string
	notes    []string
	summary  string
	iface    string
}

// podCIDRsNotEvaluatedNote is the note recorded when no node declares a pod
// network CIDR, so the overlap assertion could not run.
const podCIDRsNotEvaluatedNote = "pod-network overlap not evaluated: no node declares spec.podCIDRs or a " +
	ovnNodeSubnetsAnnotation + " annotation"

// evaluateAttachment checks one probe pod's attachment of network against its
// network-status annotation and what the probe listed (the interfaces under
// /sys/class/net and the `ip -o addr` lines):
//   - network-status has an entry named "<namespace>/<name>" with an
//     interface, and that interface is in the pod's /sys/class/net;
//   - when the network declares IPAM, the entry has an address, each one
//     configured on that interface, inside the network's declared range
//     (evaluateAttachmentIPs), and outside the cluster's pod network;
//   - when the network attaches an SR-IOV VF, the entry carries its PCI
//     address.
func evaluateAttachment(network secondaryNetwork, statusAnnotation string, podInterfaces, addrLines []string,
	poolSubnets map[string]netip.Prefix, podCIDRs []netip.Prefix) attachmentEvaluation {

	var eval attachmentEvaluation
	entries, err := parseNetworkStatus(statusAnnotation)
	if err != nil {
		eval.problems = append(eval.problems, err.Error())
		return eval
	}
	idx := slices.IndexFunc(entries, func(e networkStatusEntry) bool { return e.Name == network.key() })
	if idx < 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name)
		}
		eval.problems = append(eval.problems, fmt.Sprintf(
			"%s has no entry for %s (Multus reported %s)", multusNetworkStatusAnnotation, network.key(), formatNames(names)))
		return eval
	}
	entry := entries[idx]
	eval.iface = entry.Interface

	switch {
	case entry.Interface == "":
		eval.problems = append(eval.problems, fmt.Sprintf("the %s entry for %s names no interface",
			multusNetworkStatusAnnotation, network.key()))
	case !slices.Contains(podInterfaces, entry.Interface):
		eval.problems = append(eval.problems, fmt.Sprintf(
			"interface %s that Multus reported is not in the pod's /sys/class/net %s",
			entry.Interface, formatNames(podInterfaces)))
	}

	ipProblems, ipNotes := evaluateAttachmentIPs(network, entry.IPs, poolSubnets, podCIDRs)
	eval.problems = append(eval.problems, ipProblems...)
	eval.notes = append(eval.notes, ipNotes...)
	if network.IPAMDeclared && entry.Interface != "" {
		eval.problems = append(eval.problems, unconfiguredAddresses(entry, addrLines)...)
	}

	pci := ""
	if entry.DeviceInfo != nil && entry.DeviceInfo.PCI != nil {
		pci = entry.DeviceInfo.PCI.PCIAddress
	}
	if network.RequiresPCI && pci == "" {
		eval.problems = append(eval.problems, fmt.Sprintf(
			"the %s entry for %s carries no device-info PCI address, so no VF was attached",
			multusNetworkStatusAnnotation, network.key()))
	}

	eval.summary = attachmentSummary(entry, pci)
	return eval
}

// evaluateAttachmentIPs applies the address assertions. A network that
// declares no IPAM gets none. An address must fall inside one of the
// network's declared ranges of its own family (inline ranges and the subnets
// of its nv-ipam pools); when no range of its family is known because a pool
// is not shipped or the IPAM type's range is not read, that bound is recorded
// as a note, while a network whose every range is known and none is of the
// address's family fails. Every family the known ranges cover must have an
// address, so a dual-stack declaration cannot pass on one family alone. An
// unknown pod network is a note too.
func evaluateAttachmentIPs(network secondaryNetwork, ips []string,
	poolSubnets map[string]netip.Prefix, podCIDRs []netip.Prefix) (problems, notes []string) {

	if !network.IPAMDeclared {
		return nil, []string{"no IPAM declared, so no address is asserted"}
	}
	if len(ips) == 0 {
		return []string{fmt.Sprintf("%s declares IPAM but the attachment has no address", network.key())}, nil
	}

	ranges := append([]netip.Prefix(nil), network.IPAMSubnets...)
	unknown := false
	for _, pool := range network.IPAMPools {
		if subnet, ok := poolSubnets[pool]; ok {
			ranges = append(ranges, subnet)
			continue
		}
		unknown = true
		notes = append(notes, fmt.Sprintf("IPPool %s is not shipped by the recipe, so its subnet is not asserted", pool))
	}
	for _, ipamType := range network.IPAMUnverified {
		unknown = true
		notes = append(notes, fmt.Sprintf("the address range of IPAM type %s is not read, so it is not asserted", ipamType))
	}
	if len(podCIDRs) == 0 {
		notes = append(notes, podCIDRsNotEvaluatedNote)
	}

	reported := map[bool]bool{}
	for _, raw := range ips {
		addr, err := parseStatusIP(raw)
		if err != nil {
			problems = append(problems, fmt.Sprintf("address %q is not an IP", raw))
			continue
		}
		reported[addr.Is4()] = true
		family := slices.DeleteFunc(slices.Clone(ranges), func(p netip.Prefix) bool { return p.Addr().Is4() != addr.Is4() })
		switch {
		case len(family) > 0:
			if !slices.ContainsFunc(family, func(p netip.Prefix) bool { return p.Contains(addr) }) {
				problems = append(problems, fmt.Sprintf("address %s is outside the declared range(s) %s",
					addr, formatPrefixes(family)))
			}
		case unknown:
			notes = append(notes, fmt.Sprintf("address %s is not bounded: no declared range of its family is known", addr))
		default:
			problems = append(problems, fmt.Sprintf("address %s has no declared range of its family; the network declares %s",
				addr, formatPrefixes(ranges)))
		}
		if i := slices.IndexFunc(podCIDRs, func(p netip.Prefix) bool { return p.Contains(addr) }); i >= 0 {
			problems = append(problems, fmt.Sprintf(
				"address %s is inside the pod network %s, so the attachment's route hijacks pod-to-pod traffic",
				addr, podCIDRs[i]))
		}
	}
	for _, is4 := range []bool{true, false} {
		declared := slices.DeleteFunc(slices.Clone(ranges), func(p netip.Prefix) bool { return p.Addr().Is4() != is4 })
		if len(declared) > 0 && !reported[is4] {
			problems = append(problems, fmt.Sprintf("the network declares the %s range(s) %s but the attachment has no %s address",
				familyName(is4), formatPrefixes(declared), familyName(is4)))
		}
	}
	return problems, notes
}

func familyName(is4 bool) string {
	if is4 {
		return "IPv4"
	}
	return "IPv6"
}

// unconfiguredAddresses reports each address network-status gives entry that
// the probe's `ip -o addr` did not show on entry's interface, and a probe that
// listed no address on it at all.
func unconfiguredAddresses(entry networkStatusEntry, addrLines []string) []string {
	configured := interfaceAddrs(addrLinesFor(addrLines, entry.Interface))
	if len(configured) == 0 {
		return []string{fmt.Sprintf("the probe's ip -o addr shows no address on %s", entry.Interface)}
	}
	var problems []string
	for _, raw := range entry.IPs {
		addr, err := parseStatusIP(raw)
		if err != nil {
			continue
		}
		if !slices.Contains(configured, addr.Unmap()) {
			problems = append(problems, fmt.Sprintf("address %s that Multus reported is not configured on %s", addr, entry.Interface))
		}
	}
	return problems
}

// interfaceAddrs reads the addresses from `ip -o addr` lines, the field
// after "inet" or "inet6".
func interfaceAddrs(lines []string) []netip.Addr {
	var out []netip.Addr
	for _, line := range lines {
		fields := strings.Fields(line)
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] != "inet" && fields[i] != "inet6" {
				continue
			}
			if p, err := netip.ParsePrefix(fields[i+1]); err == nil {
				out = append(out, p.Addr().Unmap())
			}
			break
		}
	}
	return out
}

// parseStatusIP reads an address as network-status reports it, with or
// without a prefix length.
func parseStatusIP(raw string) (netip.Addr, error) {
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, "/") {
		p, err := netip.ParsePrefix(raw)
		if err != nil {
			return netip.Addr{}, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid prefix", err)
		}
		return p.Addr().Unmap(), nil
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Addr{}, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid address", err)
	}
	return addr.Unmap(), nil
}

// addrLinesFor returns the `ip -o addr` lines of iface, whose second field
// is the interface name ("3: net1    inet 10.255.0.12/22 ...").
func addrLinesFor(addrLines []string, iface string) []string {
	var out []string
	for _, line := range addrLines {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimSuffix(fields[1], ":"), "@")
		if name == iface {
			out = append(out, line)
		}
	}
	return out
}

func attachmentSummary(entry networkStatusEntry, pci string) string {
	parts := []string{"interface " + valueOr(entry.Interface, "<none>")}
	if len(entry.IPs) > 0 {
		parts = append(parts, "ips "+strings.Join(entry.IPs, ","))
	}
	if pci != "" {
		parts = append(parts, "pci "+pci)
	}
	if entry.DeviceInfo != nil && entry.DeviceInfo.PCI != nil && entry.DeviceInfo.PCI.RDMADevice != "" {
		parts = append(parts, "rdma "+entry.DeviceInfo.PCI.RDMADevice)
	}
	return strings.Join(parts, ", ")
}

func formatPrefixes(prefixes []netip.Prefix) string {
	out := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		out = append(out, p.String())
	}
	return formatNames(out)
}

func valueOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
