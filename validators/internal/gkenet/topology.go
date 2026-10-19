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
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// NICInfoAnnotation is the node annotation GKE multi-networking populates with
// the node's NIC→PCI topology. The deployment-phase topology check reads it to
// detect a gVNIC additional network displacing a GPU NIC (#2265 case 1).
const NICInfoAnnotation = "networking.gke.io/nic-info"

// NorthInterfacesAnnotation is the node annotation GKE multi-networking populates
// with each interface's Network and underlay IP. Joined with nic-info on IP, it
// reveals WHICH Network an ethN sits on — the signal that catches a gVNIC
// displaced onto a GPU NIC's eth1..eth8 even when every node is identically
// misprovisioned (a per-node identity check, not a pool comparison).
const NorthInterfacesAnnotation = "networking.gke.io/north-interfaces"

// RequiredGPUNICInterfaces is the number of GPUDirect-TCPXO data NICs an
// a3-megagpu-8g node binds (eth1..eth8 — the NCCL_FASTRAK_IFNAME contract).
// It is a3-megagpu-8g-specific, like the node selector the caller uses.
const RequiredGPUNICInterfaces = 8

// nicInfoEntry is one interface record in the nic-info annotation. GKE's
// gke-networking-api emits an array of these: the interface's kernel name
// (birthName), its PCI address, and its IPs.
type nicInfoEntry struct {
	BirthName  string `json:"birthName"`
	BirthIP    string `json:"birthIP"`
	BirthIPv6  string `json:"birthIPv6"`
	PCIAddress string `json:"pciAddress"`
}

// northInterfaceEntry is one record in the north-interfaces annotation
// (gke-networking-api NorthInterface): the Network the interface connects to and
// its underlay/parent IP (which joins to a nic-info birthIP).
type northInterfaceEntry struct {
	Network     string `json:"network"`
	IPAddress   string `json:"ipAddress,omitempty"`
	IPv6Address string `json:"ipv6Address,omitempty"`
}

// NodeNICInfo is the parsed per-node NIC topology from the nic-info annotation.
type NodeNICInfo struct {
	// Interfaces maps kernel interface name -> PCI address.
	Interfaces map[string]string
	// IPByInterface maps kernel interface name -> birth IP (for the north-interfaces join).
	IPByInterface map[string]string
	// GPUNICInterfaces is the count of TCPXO interfaces (eth1..eth8) present.
	GPUNICInterfaces int
	// MissingInterfaces names TCPXO interfaces (eth1..eth8) that did not map.
	MissingInterfaces []string
	// ExtraInterfaces names any interface that is not a canonical eth0..eth8 —
	// an extra gVNIC (gve1), a non-canonical form (eth08/eth-1), or eth9+. Sorted for
	// stable output.
	ExtraInterfaces []string
}

// ParseNICInfo parses the networking.gke.io/nic-info node annotation (a JSON
// array of {birthName, birthIP, birthIPv6, pciAddress}) into per-node topology.
//
// Detection is PCI-address-free (the slot assignment is not a documented stable
// contract): it flags an interface beyond eth0..eth8 and fewer than 8 of
// eth1..eth8. A node whose annotation is present but unparseable is an error
// (the caller fails closed); an absent annotation is the caller's Skip path.
//
// A pool provisioned uniformly with one gVNIC plus seven GPU NICs presents exactly
// eth0..eth8 on every node (no extra interface, all eight names mapped) — that
// shape is invisible to name/PCI here and is caught instead by the caller's
// north-interfaces join (DisplacedGPUNICInterfaces), which identifies which
// Network each ethN sits on regardless of name or slot.
func ParseNICInfo(annotation string) (*NodeNICInfo, error) {
	annotation = strings.TrimSpace(annotation)
	if annotation == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"node has no "+NICInfoAnnotation+" annotation")
	}
	var entries []nicInfoEntry
	if err := json.Unmarshal([]byte(annotation), &entries); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			"cannot parse "+NICInfoAnnotation+" annotation", err)
	}
	if len(entries) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			NICInfoAnnotation+" annotation is an empty list")
	}

	info := &NodeNICInfo{Interfaces: map[string]string{}, IPByInterface: map[string]string{}}
	for _, e := range entries {
		if e.BirthName == "" || e.PCIAddress == "" {
			continue
		}
		// A duplicate interface name means the annotation is malformed — fail
		// rather than silently keeping one entry.
		if _, dup := info.Interfaces[e.BirthName]; dup {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				NICInfoAnnotation+" annotation has a duplicate interface name "+strconv.Quote(e.BirthName))
		}
		info.Interfaces[e.BirthName] = e.PCIAddress
		info.IPByInterface[e.BirthName] = e.BirthIP
	}
	if len(info.Interfaces) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			NICInfoAnnotation+" annotation has no interface/PCI entries")
	}

	for _, ifName := range tcpXOInterfaces {
		if _, ok := info.Interfaces[ifName]; !ok {
			info.MissingInterfaces = append(info.MissingInterfaces, ifName)
			continue
		}
		info.GPUNICInterfaces++
	}

	// Extra interfaces: anything that is not a canonical eth0..eth8 name — a
	// non-eth name (an extra gVNIC like gve1), a non-numeric suffix (ethX), or a
	// non-canonical numeric form (eth08, eth-1) or eth9+. Canonical means the
	// name round-trips through Atoi without padding/sign.
	for ifName := range info.Interfaces {
		n, err := strconv.Atoi(strings.TrimPrefix(ifName, "eth"))
		canonical := err == nil && n >= 0 && ifName == "eth"+strconv.Itoa(n)
		if !canonical || n > RequiredGPUNICInterfaces {
			info.ExtraInterfaces = append(info.ExtraInterfaces, ifName)
		}
	}
	sort.Strings(info.ExtraInterfaces)

	return info, nil
}

// SortedMissing returns the missing TCPXO interfaces sorted for stable output.
func (n *NodeNICInfo) SortedMissing() []string {
	out := append([]string(nil), n.MissingInterfaces...)
	sort.Strings(out)
	return out
}

// ParseNorthInterfaces parses the networking.gke.io/north-interfaces annotation
// (a JSON array of {network, ipAddress, ipv6Address}) into an underlay-IP →
// Network-name map. Joined with nic-info on IP, it identifies which Network each
// interface sits on. A present-but-unparseable value is an error (fail closed);
// an absent annotation is the caller's unverified path.
func ParseNorthInterfaces(annotation string) (map[string]string, error) {
	annotation = strings.TrimSpace(annotation)
	if annotation == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"node has no "+NorthInterfacesAnnotation+" annotation")
	}
	var entries []northInterfaceEntry
	if err := json.Unmarshal([]byte(annotation), &entries); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			"cannot parse "+NorthInterfacesAnnotation+" annotation", err)
	}
	byIP := map[string]string{}
	for _, e := range entries {
		if e.IPAddress != "" && e.Network != "" {
			byIP[e.IPAddress] = e.Network
		}
	}
	return byIP, nil
}

// DisplacedGPUNICInterfaces returns the TCPXO interface names (eth1..eth8) whose
// Network — resolved by joining the nic-info birth IP to north-interfaces — is
// NOT one of gpuNetworks. A non-empty result is the gVNIC displacement signature
// that name/PCI alone cannot see (a gVNIC occupies a GPU NIC's ethN and PCI slot
// but maps to a different Network).
func DisplacedGPUNICInterfaces(info *NodeNICInfo, northByIP map[string]string, gpuNetworks map[string]bool) []string {
	var displaced []string
	for _, ifName := range tcpXOInterfaces {
		ip, ok := info.IPByInterface[ifName]
		if !ok || ip == "" {
			continue // interface absent — reported via MissingInterfaces, not here
		}
		network, known := northByIP[ip]
		if !known || !gpuNetworks[network] {
			displaced = append(displaced, fmt.Sprintf("%s(ip %s -> network %q)", ifName, ip, network))
		}
	}
	return displaced
}

// tcpXOInterfaces are the kernel names the 8 GPUDirect-TCPXO data NICs bind on
// an a3-megagpu-8g node (eth1..eth8 — the NCCL_FASTRAK_IFNAME contract).
var tcpXOInterfaces = []string{"eth1", "eth2", "eth3", "eth4", "eth5", "eth6", "eth7", "eth8"}
