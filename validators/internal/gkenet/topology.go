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
	"regexp"
	"strings"
)

// NICInfoAnnotation is the node annotation GKE multi-networking populates with
// the node's NIC→PCI topology. The deployment-phase topology check reads it to
// detect a gVNIC displacing a GPU NIC PCI slot (#2265 case 1).
const NICInfoAnnotation = "networking.gke.io/nic-info"

// RequiredGPUNICInterfaces is the number of GPU NIC PCI mappings (eth1..eth8)
// a healthy a3-megagpu-8g node must report for TCPXO. A gVNIC additional network
// takes a GPU NIC PCI slot (typically 0000:06:00.0), so a displaced node reports
// fewer than 8 (docs/integrator/gke-tcpxo-networking.md).
const RequiredGPUNICInterfaces = 8

var ethNamePattern = regexp.MustCompile(`^eth[0-9]+$`)

// NodeNICInfo is the parsed per-node NIC topology from the nic-info annotation.
type NodeNICInfo struct {
	// PCIByInterface maps interface name (eth0..eth8) to its PCI address.
	PCIByInterface map[string]string
	// GPUNICInterfaces is the count of GPU NIC interfaces (eth1..eth8) present.
	GPUNICInterfaces int
}

// ParseNICInfo parses the networking.gke.io/nic-info node annotation into the
// per-node NIC topology. The annotation is GKE-internal JSON; the parser accepts
// the documented shapes (a list of {name/interface, pciAddress} entries, or a
// name→PCI map) and is deliberately tolerant of extra fields.
//
// NOTE: the exact schema is confirmed against a live a3-megagpu-8g node (the
// integrator doc gives the jsonpath, not the structure). The displacement
// *detection* (8 GPU NICs mapped to eth1..eth8, none displaced) is the contract;
// the field names here may need a small adjustment once validated live.
func ParseNICInfo(annotation string) (*NodeNICInfo, error) {
	annotation = strings.TrimSpace(annotation)
	if annotation == "" {
		return nil, fmt.Errorf("node has no %s annotation", NICInfoAnnotation)
	}

	pciByIf, err := parseNICInfoShapes(annotation)
	if err != nil {
		return nil, fmt.Errorf("cannot parse %s annotation: %w", NICInfoAnnotation, err)
	}

	info := &NodeNICInfo{PCIByInterface: pciByIf}
	for ifName := range pciByIf {
		if !ethNamePattern.MatchString(ifName) || ifName == "eth0" {
			continue
		}
		// eth1..eth8 are the GPU NIC slots on a3-megagpu-8g.
		n := ifName[len("eth"):]
		if n >= "1" && n <= "8" && len(n) == 1 {
			info.GPUNICInterfaces++
		}
	}
	return info, nil
}

// parseNICInfoShapes decodes the annotation across its documented shapes.
func parseNICInfoShapes(annotation string) (map[string]string, error) {
	// Shape A: a list of entries with name/interface + pciAddress.
	var list []map[string]any
	if err := json.Unmarshal([]byte(annotation), &list); err == nil {
		out := map[string]string{}
		for _, e := range list {
			name := firstString(e, "interfaceName", "name", "interface", "ifname")
			pci := firstString(e, "pciAddress", "pci", "pciAddr", "address")
			if name != "" && pci != "" {
				out[name] = pci
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	// Shape B: a name→PCI (or PCI→name) object map.
	var m map[string]string
	if err := json.Unmarshal([]byte(annotation), &m); err == nil && len(m) > 0 {
		return m, nil
	}
	return nil, fmt.Errorf("unrecognized annotation shape")
}

// firstString returns the first non-empty string value among keys.
func firstString(e map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := e[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}
