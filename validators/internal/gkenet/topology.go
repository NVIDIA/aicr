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
	"sort"
	"strconv"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// NICInfoAnnotation is the node annotation GKE multi-networking populates with
// the node's NIC→PCI topology. The deployment-phase topology check reads it to
// detect a gVNIC additional network displacing a GPU NIC (#2265 case 1).
const NICInfoAnnotation = "networking.gke.io/nic-info"

// RequiredGPUNICInterfaces is the number of GPUDirect-TCPXO data NICs an
// a3-megagpu-8g node binds (eth1..eth8 — the NCCL_FASTRAK_IFNAME contract).
const RequiredGPUNICInterfaces = 8

// observedA3GPUNICSlots is the GPU NIC PCI layout captured from live a3-megagpu-8g
// nodes (06,07,0d,0e,86,87,8d,8e). It is used ONLY for an informational warning
// when a node's layout deviates from this observed norm — never for pass/fail,
// because PCI slot assignment is not a documented stable contract.
var observedA3GPUNICSlots = []string{
	"0000:06:00.0", "0000:07:00.0", "0000:0d:00.0", "0000:0e:00.0",
	"0000:86:00.0", "0000:87:00.0", "0000:8d:00.0", "0000:8e:00.0",
}

// nicInfoEntry is one interface record in the nic-info annotation. GKE's
// gke-networking-api emits an array of these: the interface's kernel name
// (birthName), its PCI address, and its IPs.
type nicInfoEntry struct {
	BirthName  string `json:"birthName"`
	BirthIP    string `json:"birthIP"`
	BirthIPv6  string `json:"birthIPv6"`
	PCIAddress string `json:"pciAddress"`
}

// NodeNICInfo is the parsed per-node NIC topology from the nic-info annotation.
type NodeNICInfo struct {
	// Interfaces maps kernel interface name -> PCI address.
	Interfaces map[string]string
	// GPUNICInterfaces is the count of TCPXO interfaces (eth1..eth8) present.
	GPUNICInterfaces int
	// MissingInterfaces names TCPXO interfaces (eth1..eth8) that did not map.
	MissingInterfaces []string
	// ExtraInterfaces names ethN interfaces beyond eth8 (e.g. eth9) — an extra
	// gVNIC additional network alongside the 8 GPU NICs. Sorted for stable output.
	ExtraInterfaces []string
	// GPUNICPCIs is the sorted set of PCI addresses the node's eth1..eth8 occupy,
	// for the pool-consistency check (a node differing from its pool is displaced).
	GPUNICPCIs []string
}

// ParseNICInfo parses the networking.gke.io/nic-info node annotation (a JSON
// array of {birthName, birthIP, birthIPv6, pciAddress}) into per-node topology.
//
// Displacement detection is PCI-address-free for pass/fail (the slot assignment
// is not a documented stable contract): it flags an interface beyond eth0..eth8,
// fewer than 8 of eth1..eth8, and (in the caller) a node whose GPU-NIC PCI set
// differs from the rest of its pool.
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

	info := &NodeNICInfo{Interfaces: map[string]string{}}
	for _, e := range entries {
		if e.BirthName == "" || e.PCIAddress == "" {
			continue
		}
		// A duplicate interface name means the annotation is malformed — treat the
		// node as unverifiable rather than silently keeping one entry.
		if _, dup := info.Interfaces[e.BirthName]; dup {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				NICInfoAnnotation+" annotation has a duplicate interface name "+strconv.Quote(e.BirthName))
		}
		info.Interfaces[e.BirthName] = e.PCIAddress
	}
	if len(info.Interfaces) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			NICInfoAnnotation+" annotation has no interface/PCI entries")
	}

	present := map[string]bool{}
	for _, ifName := range tcpXOInterfaces {
		pci, ok := info.Interfaces[ifName]
		if !ok {
			info.MissingInterfaces = append(info.MissingInterfaces, ifName)
			continue
		}
		info.GPUNICInterfaces++
		present[ifName] = true
		info.GPUNICPCIs = append(info.GPUNICPCIs, pci)
	}
	sort.Strings(info.GPUNICPCIs)

	// Extra interfaces: any ethN beyond eth8. Parse the numeric suffix — ethX,
	// eth08, eth1.100 etc. are all "extra", never silently ignored or miscounted.
	for ifName := range info.Interfaces {
		if !strings.HasPrefix(ifName, "eth") {
			continue
		}
		n, err := strconv.Atoi(ifName[len("eth"):])
		if err != nil || n > RequiredGPUNICInterfaces {
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

// ObservedGPUNICSlotDeviation reports whether any present eth1..eth8 sits at a
// PCI address outside the layout observed on live a3 nodes. Informational only —
// never a pass/fail input.
func (n *NodeNICInfo) ObservedGPUNICSlotDeviation() []string {
	slots := map[string]bool{}
	for _, s := range observedA3GPUNICSlots {
		slots[s] = true
	}
	var dev []string
	for _, ifName := range tcpXOInterfaces {
		if pci, ok := n.Interfaces[ifName]; ok && !slots[pci] {
			dev = append(dev, ifName+"@"+pci)
		}
	}
	sort.Strings(dev)
	return dev
}

// tcpXOInterfaces are the kernel names the 8 GPUDirect-TCPXO data NICs bind on
// an a3-megagpu-8g node (eth1..eth8 — the NCCL_FASTRAK_IFNAME contract).
var tcpXOInterfaces = []string{"eth1", "eth2", "eth3", "eth4", "eth5", "eth6", "eth7", "eth8"}
