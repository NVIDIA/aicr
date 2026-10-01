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
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// NICInfoAnnotation is the node annotation GKE multi-networking populates with
// the node's NIC→PCI topology. The deployment-phase topology check reads it to
// detect a gVNIC additional network displacing a GPU NIC PCI slot (#2265 case 1).
const NICInfoAnnotation = "networking.gke.io/nic-info"

// nicInfoEntry is one interface record in the nic-info annotation. GKE's
// gke-networking-api emits an array of these (annotations.go): the interface's
// kernel name (birthName), its PCI address, and its IPs.
type nicInfoEntry struct {
	BirthName  string `json:"birthName"`
	BirthIP    string `json:"birthIP"`
	BirthIPv6  string `json:"birthIPv6"`
	PCIAddress string `json:"pciAddress"`
}

// tcpXOInterfaces are the kernel names the 8 GPUDirect-TCPXO data NICs bind on
// an a3-megagpu-8g node (eth1..eth8 — the NCCL_FASTRAK_IFNAME contract).
var tcpXOInterfaces = []string{"eth1", "eth2", "eth3", "eth4", "eth5", "eth6", "eth7", "eth8"}

// RequiredGPUNICInterfaces is the number of TCPXO interfaces a healthy a3 node maps.
const RequiredGPUNICInterfaces = 8

// a3GPUNICSlots are the PCI addresses a3-megagpu-8g assigns its 8 GPU data NICs
// (eth1..eth8). The doc-cited displacement is a gVNIC additional network taking
// one of these slots (typically 0000:06:00.0). MARKED FOR LIVE VALIDATION: the
// exact slot assignment is GKE-internal; this set must be confirmed against a
// real a3 node before the displacement detection is relied on.
var a3GPUNICSlots = []string{
	"0000:06:00.0", "0000:07:00.0", "0000:08:00.0", "0000:09:00.0",
	"0000:0a:00.0", "0000:0b:00.0", "0000:0c:00.0", "0000:0d:00.0",
}

// NodeNICInfo is the parsed per-node NIC topology from the nic-info annotation.
type NodeNICInfo struct {
	// Interfaces maps kernel interface name -> PCI address.
	Interfaces map[string]string
	// GPUNICInterfaces is the count of TCPXO interfaces (eth1..eth8) present.
	GPUNICInterfaces int
	// MissingInterfaces names TCPXO interfaces (eth1..eth8) that did not map.
	MissingInterfaces []string
	// ExtraAtGPUNICSlot names a non-TCPXO interface occupying a GPU NIC PCI slot
	// (the gVNIC-displacement failure mode); empty when healthy.
	ExtraAtGPUNICSlot string
	// WrongSlotInterfaces names TCPXO interfaces (eth1..eth8) at a PCI address
	// outside the GPU NIC slot set — a GPU NIC pushed off its slot by a gVNIC.
	WrongSlotInterfaces []string
	// ExtraInterfaces names ethN interfaces beyond eth8 (e.g. eth9) — an extra
	// gVNIC additional network alongside the 8 GPU NICs.
	ExtraInterfaces []string
}

// ParseNICInfo parses the networking.gke.io/nic-info node annotation (a JSON
// array of {birthName, birthIP, birthIPv6, pciAddress}) into per-node topology.
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
		if e.BirthName != "" && e.PCIAddress != "" {
			info.Interfaces[e.BirthName] = e.PCIAddress
		}
	}
	if len(info.Interfaces) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			NICInfoAnnotation+" annotation has no interface/PCI entries")
	}

	present := map[string]bool{}
	for _, ifName := range tcpXOInterfaces {
		if _, ok := info.Interfaces[ifName]; ok {
			info.GPUNICInterfaces++
			present[ifName] = true
		} else {
			info.MissingInterfaces = append(info.MissingInterfaces, ifName)
		}
	}

	// Displacement detection (best-effort until the slot set is live-validated —
	// see a3GPUNICSlots): (a) a non-TCPXO interface holds a GPU NIC PCI slot;
	// (b) a TCPXO interface sits at a PCI outside the GPU NIC slot set (a GPU NIC
	// pushed off its slot); (c) an ethN interface beyond eth8 exists (an extra
	// gVNIC alongside the 8 GPU NICs).
	slots := map[string]string{} // pci -> interface
	for ifName, pci := range info.Interfaces {
		slots[pci] = ifName
	}
	for _, slot := range a3GPUNICSlots {
		ifName, ok := slots[slot]
		if !ok {
			continue // slot unused — fine
		}
		if !present[ifName] {
			info.ExtraAtGPUNICSlot = ifName + "@" + slot
			break
		}
	}
	gpuSlot := map[string]bool{}
	for _, slot := range a3GPUNICSlots {
		gpuSlot[slot] = true
	}
	for _, ifName := range tcpXOInterfaces {
		pci, ok := info.Interfaces[ifName]
		if !ok {
			continue
		}
		if !gpuSlot[pci] {
			info.WrongSlotInterfaces = append(info.WrongSlotInterfaces, ifName+"@"+pci)
		}
	}
	for ifName := range info.Interfaces {
		if strings.HasPrefix(ifName, "eth") {
			n := ifName[len("eth"):]
			if len(n) > 1 || n > "8" {
				info.ExtraInterfaces = append(info.ExtraInterfaces, ifName)
			}
		}
	}
	return info, nil
}

// SortedMissing returns the missing TCPXO interfaces sorted for stable output.
func (n *NodeNICInfo) SortedMissing() []string {
	out := append([]string(nil), n.MissingInterfaces...)
	sort.Strings(out)
	return out
}
