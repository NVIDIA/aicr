// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
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
//
// SPDX-License-Identifier: Apache-2.0

// Package pfutil holds the small PF/NIC slice helpers shared between the
// network-operator discovery path and the manifest-render path. Internal
// to the l8k module — only callable from packages under
// pkg/networkoperatorplugin/ and its sub-packages (including discovery/).
//
// Kept here so the discovery sub-package can stay free of helm-tainted
// imports while still reusing the same PF-classification primitives the
// render path uses.
package pfutil

import (
	"regexp"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

// FilterEastWestPFs returns only PFs with traffic == "east-west", so that
// downstream indexers (rails, plane numbering, template naming) stay
// sequential. North-south PFs (BlueField DPUs, OOB management) are
// excluded from rails and manifests by design.
func FilterEastWestPFs(pfs []config.PFConfig) []config.PFConfig {
	var filtered []config.PFConfig
	for _, pf := range pfs {
		if pf.Traffic == "east-west" {
			filtered = append(filtered, pf)
		}
	}
	return filtered
}

// PciBusDevicePrefix returns the "domain:bus:device" portion of a PCI
// address — everything before the final ".<function>" — which is shared
// by all PFs that live on the same physical NIC. Returns ok=false for a
// malformed address that has no function suffix.
func PciBusDevicePrefix(pciAddress string) (string, bool) {
	idx := strings.LastIndex(pciAddress, ".")
	if idx <= 0 {
		return "", false
	}
	return pciAddress[:idx], true
}

// dualPortModelRe matches a port-count keyword that designates a genuinely
// dual-port adapter. "dual-port"/"dual port" are matched directly. The
// numeric form requires the "2" to sit on a word boundary (`\b`) so
// digit-adjacent tokens like "QSFP112" or "Gen2" cannot trigger a false
// positive; the separator may be a hyphen or a space ("2-port", "2 port").
// Case-insensitive.
var dualPortModelRe = regexp.MustCompile(`(?i)(dual[- ]port|\b2[- ]port)`)

// IsDualPortModel reports whether a NIC VPD model/description string
// indicates a genuinely dual-port adapter — two physical ports that each
// terminate a separate fabric and therefore each deserve their own rail.
// NVIDIA VPD strings spell this as "2-port"/"Dual-port" (e.g.
// "... QSFP112 2-port PCIe ...", "... Dual-port QSFP112 ..."). Single-port
// adapters say "1P"/"single-port" and multi-plane adapters — whose extra
// PFs are planes of one physical port — carry no port-count keyword at
// all. Matching is deliberately conservative: the numeric "2" must stand
// on a word boundary, so substrings like "QSFP112" or "Gen2" cannot
// trigger a false positive that would leave a multi-plane NIC uncollapsed.
// An empty/unknown string returns false, which makes the caller collapse
// to one rail per NIC by default.
func IsDualPortModel(model string) bool {
	return dualPortModelRe.MatchString(model)
}

// CollapsePFsToOnePerNIC reduces a set of PFs to one per NIC (PCI
// bus:device prefix): for a NIC whose VPD model is dual-port
// (IsDualPortModel) every PF is kept — each physical port is its own rail
// — while for any other NIC only the master (lowest-function) PF survives,
// since its sibling PFs are planes of a single rail rather than
// independent rails. Input order is preserved among the kept PFs, and the
// count of dropped PFs is returned so the caller can log a summary.
//
// It is intended to run on the east-west subset only; north-south PFs
// should be filtered out by the caller (they are excluded from rails and
// manifests anyway). A PF with a malformed PCI address (no function
// suffix) is always kept, never silently dropped.
func CollapsePFsToOnePerNIC(pfs []config.PFConfig) (kept []config.PFConfig, dropped int) {
	type nicInfo struct {
		dualPort bool
		master   string
	}
	nics := map[string]*nicInfo{}
	for i := range pfs {
		prefix, ok := PciBusDevicePrefix(pfs[i].PciAddress)
		if !ok {
			continue
		}
		info := nics[prefix]
		if info == nil {
			info = &nicInfo{master: pfs[i].PciAddress}
			nics[prefix] = info
		} else if pfs[i].PciAddress < info.master {
			info.master = pfs[i].PciAddress
		}
		if IsDualPortModel(pfs[i].Model) {
			info.dualPort = true
		}
	}

	kept = make([]config.PFConfig, 0, len(pfs))
	for i := range pfs {
		prefix, ok := PciBusDevicePrefix(pfs[i].PciAddress)
		if !ok {
			kept = append(kept, pfs[i])
			continue
		}
		info := nics[prefix]
		if info.dualPort || pfs[i].PciAddress == info.master {
			kept = append(kept, pfs[i])
		} else {
			dropped++
		}
	}
	return kept, dropped
}
