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

package networkoperatorplugin

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"text/template"

	"github.com/Masterminds/semver/v3"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/profiles"
)

// templateFuncs provides helper functions for Go templates
var templateFuncs = template.FuncMap{
	"add": func(a, b int) int { return a + b },
	"sub": func(a, b int) int { return a - b },
	"gt":  func(a, b int) bool { return a > b },
	"untilStep": func(start, stop, step int) []int {
		result := []int{}
		for i := start; i < stop; i += step {
			result = append(result, i)
		}
		return result
	},
	"replaceVars": func(template string, nicID, plane, rail int) string {
		// Replace template variables with actual values
		// Supports both old (%rail%) and new (%rail_id%) placeholder formats
		result := template
		result = strings.ReplaceAll(result, "%nic_id%", fmt.Sprintf("%d", nicID))
		result = strings.ReplaceAll(result, "%plane_id%", fmt.Sprintf("%d", plane))
		result = strings.ReplaceAll(result, "%plane%", fmt.Sprintf("%d", plane))
		result = strings.ReplaceAll(result, "%rail_id%", fmt.Sprintf("%d", rail))
		result = strings.ReplaceAll(result, "%rail%", fmt.Sprintf("%d", rail))
		return result
	},
	// suffix prepends a "-" if the string is non-empty, producing a name suffix.
	// e.g., suffix("group-0") → "-group-0", suffix("") → ""
	"suffix": func(s string) string {
		if s == "" {
			return ""
		}
		return "-" + s
	},
	// nsSuffix appends "-<current>" only when more than one network namespace
	// is being rendered, disambiguating the per-namespace copies of a
	// secondary-network CR (and the network name the example DaemonSet
	// references). With a single namespace it returns "" so the common case
	// keeps its existing names unchanged. Called as
	// `{{nsSuffix $.NetworkNamespaces $.PodNamespace}}` — type-agnostic so it
	// works whether the template root is the bare config or a per-group ctx.
	//
	// The `namespaces` it reads is the *per-render* set: the renderer
	// (`withRenderNamespaces`) narrows cfg.NetworkNamespaces to exactly the
	// namespaces a Kind is rendered into, so a shared (non-replicated) Kind
	// sees a single-entry slice and gets no suffix even if a template author
	// adds this helper — the "suffix iff this is one of several copies"
	// contract holds by construction, not by templates avoiding the call.
	"nsSuffix": func(namespaces []string, current string) string {
		if len(namespaces) > 1 && current != "" {
			return "-" + current
		}
		return ""
	},
	// boundedSuffix returns "-<value>" suitable for appending to a name or label
	// value such that the combined string never exceeds the k8s 63-char label-value
	// limit. Callers pass the maximum allowed length of the returned suffix —
	// typically 63 minus the length of the prefix the suffix is appended to.
	// Empty input returns "". Inputs whose natural "-<s>" form fits within maxLen
	// are used verbatim. Longer inputs are truncated and combined with an FNV-32a
	// hash of the original (same algorithm as MachineLabelValue), so the result
	// is deterministic across calls but never breaches maxLen.
	"boundedSuffix": boundedSuffix,
	// pfsPerNic computes how many PFs share the same physical NIC by grouping
	// east-west PFs by PCI bus:device prefix (everything before the last ".").
	// E.g., 8 PFs across 8 NICs → 1; 8 PFs across 4 NICs → 2.
	"pfsPerNic": func(pfs []config.PFConfig) int {
		return pfsPerNic(pfs)
	},
	"railPciGroups": func(pfs []config.PFConfig, planes int) [][]string {
		return railPciGroups(pfs, planes)
	},
	// railCount returns how many rails are present in the east-west PFs.
	// Authoritative source is the PFConfig.Rail field — a rail is a logical
	// group of NICs feeding the same fabric stripe, and the firmware splits
	// each NIC into the user-supplied numberOfPlanes. When every east-west PF
	// has Rail set, this is the count of distinct Rail values. When the data
	// is missing Rail (legacy / unparsed), we fall back to the old chunk
	// heuristic len(ew)/planes so existing pre-Rail configs still render.
	"railCount": func(pfs []config.PFConfig, planes int) int {
		return railCount(pfs, planes)
	},
	// spectrumXPfsPerNic is the value to render into
	// NicInterfaceNameTemplate.pfsPerNic for the spectrum-x profiles. It's
	// derived from the topology, not from the user's --number-of-planes
	// alone: pfsPerNic = numberOfPlanes / NICsPerRail. The cluster config
	// only lists master PFs (function 0) per NIC, so NICsPerRail is the
	// length of the first rail's railPciAddresses inner list.
	"spectrumXPfsPerNic": func(pfs []config.PFConfig, planes int) int {
		return spectrumXPfsPerNic(pfs, planes)
	},
	// nnpName produces a valid NicNodePolicy name from a group identifier.
	// Returns "l8k" for empty identifiers, truncates to 30 chars (NNP name limit).
	"nnpName": func(identifier string) string {
		return nnpName(identifier)
	},
	// versionGE/versionLT/versionEQ compare release identifiers (MAJOR.MINOR
	// or full semver). An empty `have` means "no release pinned" — treated as
	// "latest", so versionGE("", X) is always true and versionLT("", X) is
	// always false. This keeps existing configs (no --network-operator-release
	// set) rendering the newest gates by default.
	"versionGE": versionGE,
	"versionLT": versionLT,
	"versionEQ": versionEQ,
	// legacyRdmaSharedConfigList builds the rdmaSharedDevicePlugin.config JSON
	// (configList array body) used by the 26.1 NicClusterPolicy. NCP is a
	// cluster singleton in 26.1, so this aggregates east-west PFs across all
	// groups. In multirail mode each (group, PF) becomes a per-rail entry; in
	// non-multirail each group becomes one entry that lists all its
	// east-west netdevs.
	"legacyRdmaSharedConfigList": legacyRdmaSharedConfigList,
	// legacySriovDevicePluginConfigList does the same shape for the
	// sriovDevicePlugin used by the 26.1 host-device profile.
	"legacySriovDevicePluginConfigList": legacySriovDevicePluginConfigList,
}

// legacyRdmaSharedConfigList builds the configList array body for
// rdmaSharedDevicePlugin in a 26.1 NicClusterPolicy. groups carries every
// cluster group (NCP is a singleton in 26.1), rdmaShared.ResourceName /
// HcaMax come from l8k-config. multirail switches between per-rail entries
// (one per east-west PF) and per-group entries (one selector listing all
// east-west netdev names).
func legacyRdmaSharedConfigList(groups []config.ClusterConfig, rdmaShared *config.RdmaSharedConfig, multirail bool) string {
	if rdmaShared == nil {
		return ""
	}
	var entries []string
	for _, g := range groups {
		ew := filterEastWestPFs(g.PFs)
		if multirail {
			for _, pf := range ew {
				rail := 0
				if pf.Rail != nil {
					rail = *pf.Rail
				}
				entries = append(entries, fmt.Sprintf(`          {
            "resourceName": "%s_rail_%d",
            "rdmaHcaMax": %d,
            "selectors": {
              "ifNames": [%q]
            }
          }`, rdmaShared.ResourceName, rail, rdmaShared.HcaMax, pf.NetworkInterface))
			}
		} else {
			names := make([]string, 0, len(ew))
			for _, pf := range ew {
				names = append(names, fmt.Sprintf("%q", pf.NetworkInterface))
			}
			entries = append(entries, fmt.Sprintf(`          {
            "resourceName": "%s",
            "rdmaHcaMax": %d,
            "selectors": {
              "ifNames": [%s]
            }
          }`, rdmaShared.ResourceName, rdmaShared.HcaMax, strings.Join(names, ", ")))
		}
	}
	if len(entries) == 0 {
		return ""
	}
	return "\n" + strings.Join(entries, ",\n") + "\n        "
}

// legacySriovDevicePluginConfigList builds the resourceList array body for
// sriovDevicePlugin in a 26.1 NicClusterPolicy (used by host-device-rdma).
// hostdev.ResourceName seeds the K8s extended-resource name.
func legacySriovDevicePluginConfigList(groups []config.ClusterConfig, hostdev *config.HostdevConfig, multirail bool, useNameTemplates bool) string {
	if hostdev == nil {
		return ""
	}
	var entries []string
	for _, g := range groups {
		ew := filterEastWestPFs(g.PFs)
		if multirail {
			for _, pf := range ew {
				rail := 0
				if pf.Rail != nil {
					rail = *pf.Rail
				}
				selector := ""
				switch {
				case useNameTemplates && pf.NetworkInterface != "":
					selector = fmt.Sprintf(`              "pfNames": [%q],`, pf.NetworkInterface)
				default:
					selector = fmt.Sprintf(`              "pciAddresses": [%q],`, pf.PciAddress)
				}
				entries = append(entries, fmt.Sprintf(`          {
            "resourcePrefix": "nvidia.com",
            "resourceName": "%s_rail_%d",
            "selectors": {
              "vendors": ["15b3"],
%s
              "isRdma": true
            }
          }`, hostdev.ResourceName, rail, selector))
			}
		} else {
			entries = append(entries, fmt.Sprintf(`          {
            "resourcePrefix": "nvidia.com",
            "resourceName": "%s",
            "selectors": {
              "vendors": ["15b3"],
              "isRdma": true
            }
          }`, hostdev.ResourceName))
		}
	}
	if len(entries) == 0 {
		return ""
	}
	return "\n" + strings.Join(entries, ",\n") + "\n        "
}

// normalizeRelease accepts catalog keys and returns a semver-parseable string.
// "26.4" -> "26.4.0", "v26.4" -> "26.4.0", "26.4.0" pass-through.
func normalizeRelease(s string) string {
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return s
	}
	if strings.Count(s, ".") == 1 {
		return s + ".0"
	}
	return s
}

func versionGE(have, target string) bool {
	if have == "" {
		return true
	}
	h, err := semver.NewVersion(normalizeRelease(have))
	if err != nil {
		return false
	}
	t, err := semver.NewVersion(normalizeRelease(target))
	if err != nil {
		return false
	}
	return h.GreaterThanEqual(t)
}

func versionLT(have, target string) bool {
	if have == "" {
		return false
	}
	h, err := semver.NewVersion(normalizeRelease(have))
	if err != nil {
		return false
	}
	t, err := semver.NewVersion(normalizeRelease(target))
	if err != nil {
		return false
	}
	return h.LessThan(t)
}

func versionEQ(have, target string) bool {
	if have == "" {
		return false
	}
	h, err := semver.NewVersion(normalizeRelease(have))
	if err != nil {
		return false
	}
	t, err := semver.NewVersion(normalizeRelease(target))
	if err != nil {
		return false
	}
	return h.Equal(t)
}

// nnpName produces a valid NicNodePolicy name from a group identifier.
// Returns "l8k" for empty identifiers, truncates to 30 chars (NNP name limit).
func nnpName(identifier string) string {
	if identifier == "" {
		return "l8k"
	}
	if len(identifier) > 30 {
		return identifier[:30]
	}
	return identifier
}

// applyPrefix substitutes %nic_id%, %plane%, %rail% placeholders in a prefix template.
func applyPrefix(prefix string, nicID, plane, rail int) string {
	result := prefix
	// Supports both old (%rail%) and new (%rail_id%) placeholder formats
	result = strings.ReplaceAll(result, "%nic_id%", fmt.Sprintf("%d", nicID))
	result = strings.ReplaceAll(result, "%plane_id%", fmt.Sprintf("%d", plane))
	result = strings.ReplaceAll(result, "%plane%", fmt.Sprintf("%d", plane))
	result = strings.ReplaceAll(result, "%rail_id%", fmt.Sprintf("%d", rail))
	result = strings.ReplaceAll(result, "%rail%", fmt.Sprintf("%d", rail))
	return result
}

// pfsPerNic computes PFs-per-NIC by grouping east-west PFs by PCI bus:device prefix.
func pfsPerNic(pfs []config.PFConfig) int {
	ewPFs := filterEastWestPFs(pfs)
	nicDevices := map[string]bool{}
	for _, pf := range ewPFs {
		if idx := strings.LastIndex(pf.PciAddress, "."); idx > 0 {
			nicDevices[pf.PciAddress[:idx]] = true
		}
	}
	if len(nicDevices) == 0 {
		return 1
	}
	return len(ewPFs) / len(nicDevices)
}

// railPciGroups returns NicInterfaceNameTemplate.railPciAddresses as one
// inner list per rail, where each inner list is the master PFs (function 0,
// or the lowest-function PF if .0 is missing) of the unique NICs assigned
// to that rail. The Rail field on PFConfig is authoritative. The operator
// infers any secondary PFs on each NIC from the master + the pfsPerNic
// hint, so we never list non-master functions here.
//
// Worked examples:
//
//	1 NIC per rail, 2 PFs per NIC firmware-split (UCSC pattern):
//	  PFs: [{0:0, rail=0}, {0:23, rail=1}, ...]
//	  → [["0:0"], ["0:23"], ...]
//
//	2 NICs per rail, 2 PFs per NIC:
//	  PFs (masters only): [{1a:0, rail=0}, {2a:0, rail=0}, {3a:0, rail=1}, {4a:0, rail=1}]
//	  → [["1a:0", "2a:0"], ["3a:0", "4a:0"]]
//
// Fallback (no Rail set): chunk east-west PFs into groups of `planes`,
// preserving the legacy pre-Rail-field rendering.
func railPciGroups(pfs []config.PFConfig, planes int) [][]string {
	ewPFs := filterEastWestPFs(pfs)
	if planes < 1 {
		planes = 1
	}

	if !allHaveRail(ewPFs) {
		// Legacy fallback for configs that predate the Rail field.
		var groups [][]string
		for i := 0; i < len(ewPFs); i += planes {
			end := i + planes
			if end > len(ewPFs) {
				end = len(ewPFs)
			}
			group := make([]string, 0, end-i)
			for _, pf := range ewPFs[i:end] {
				group = append(group, pf.PciAddress)
			}
			groups = append(groups, group)
		}
		return groups
	}

	rails, order := groupPFsByRail(ewPFs)
	groups := make([][]string, 0, len(order))
	for _, rail := range order {
		groups = append(groups, masterPFsByNIC(rails[rail]))
	}
	return groups
}

// railCount returns how many distinct rails are present in the east-west
// PFs. Falls back to len(ew)/planes when Rail is unset on any PF.
func railCount(pfs []config.PFConfig, planes int) int {
	ewPFs := filterEastWestPFs(pfs)
	if planes < 1 {
		planes = 1
	}
	if !allHaveRail(ewPFs) {
		return len(ewPFs) / planes
	}
	rails, _ := groupPFsByRail(ewPFs)
	return len(rails)
}

// spectrumXPfsPerNic is the value to render for the NicInterfaceNameTemplate
// `pfsPerNic` field: the number of PFs the operator should expect on each
// NIC. Computed as numberOfPlanes / NICsPerRail because all rails should be
// uniform (numberOfPlanes total planes per rail = pfsPerNic × NICsPerRail).
// NICsPerRail is read off the first rail's master-PF list. Falls back to
// numberOfPlanes (the legacy behaviour) when the rail layout is unknown.
func spectrumXPfsPerNic(pfs []config.PFConfig, planes int) int {
	if planes < 1 {
		planes = 1
	}
	groups := railPciGroups(pfs, planes)
	if len(groups) == 0 || len(groups[0]) == 0 {
		return planes
	}
	pfs_per_nic := planes / len(groups[0])
	if pfs_per_nic < 1 {
		return 1
	}
	return pfs_per_nic
}

// allHaveRail reports whether every PF in pfs has its Rail field set.
func allHaveRail(pfs []config.PFConfig) bool {
	if len(pfs) == 0 {
		return false
	}
	for _, pf := range pfs {
		if pf.Rail == nil {
			return false
		}
	}
	return true
}

// groupPFsByRail buckets PFs by their Rail value and returns both the
// bucket map and the rail indices in sorted order so callers can iterate
// deterministically. Caller must have verified allHaveRail.
func groupPFsByRail(pfs []config.PFConfig) (map[int][]config.PFConfig, []int) {
	buckets := map[int][]config.PFConfig{}
	for _, pf := range pfs {
		buckets[*pf.Rail] = append(buckets[*pf.Rail], pf)
	}
	rails := make([]int, 0, len(buckets))
	for r := range buckets {
		rails = append(rails, r)
	}
	sort.Ints(rails)
	return buckets, rails
}

// masterPFsByNIC dedupes a list of PFs by NIC (PCI bus:device prefix) and
// returns one PF per NIC in input order. The "master" is the lowest-function
// PF on that NIC — usually function 0, but we don't assume so the helper
// stays correct on configs that only list .1 (or any other function) on a
// given bus:device.
func masterPFsByNIC(pfs []config.PFConfig) []string {
	master := map[string]string{} // bus:device → lowest-function PCI address seen
	order := []string{}           // NIC bus:device prefixes in first-seen order
	for _, pf := range pfs {
		prefix, ok := pciBusDevicePrefix(pf.PciAddress)
		if !ok {
			continue
		}
		if existing, ok := master[prefix]; !ok {
			master[prefix] = pf.PciAddress
			order = append(order, prefix)
		} else if pf.PciAddress < existing {
			master[prefix] = pf.PciAddress
		}
	}
	out := make([]string, 0, len(order))
	for _, prefix := range order {
		out = append(out, master[prefix])
	}
	return out
}

// pciBusDevicePrefix returns the "domain:bus:device" portion of a PCI address
// — everything before the final ".<function>" — which is shared by all PFs that
// live on the same physical NIC. Returns ok=false for a malformed address that
// has no function suffix.
func pciBusDevicePrefix(pciAddress string) (string, bool) {
	idx := strings.LastIndex(pciAddress, ".")
	if idx <= 0 {
		return "", false
	}
	return pciAddress[:idx], true
}

// dualPortModelRe matches a port-count keyword that designates a genuinely
// dual-port adapter. "dual-port"/"dual port" are matched directly. The numeric
// form requires the "2" to sit on a word boundary (`\b`) so digit-adjacent
// tokens like "QSFP112" or "Gen2" cannot trigger a false positive; the
// separator may be a hyphen or a space ("2-port", "2 port"). Case-insensitive.
var dualPortModelRe = regexp.MustCompile(`(?i)(dual[- ]port|\b2[- ]port)`)

// isDualPortModel reports whether a NIC VPD model/description string indicates a
// genuinely dual-port adapter — two physical ports that each terminate a
// separate fabric and therefore each deserve their own rail. NVIDIA VPD strings
// spell this as "2-port"/"Dual-port" (e.g. "... QSFP112 2-port PCIe ...",
// "... Dual-port QSFP112 ..."). Single-port adapters say "1P"/"single-port" and
// multi-plane adapters — whose extra PFs are planes of one physical port — carry
// no port-count keyword at all. Matching is deliberately conservative: the
// numeric "2" must stand on a word boundary, so substrings like "QSFP112" or
// "Gen2" cannot trigger a false positive that would leave a multi-plane NIC
// uncollapsed. An empty/unknown string returns false, which makes the caller
// collapse to one rail per NIC by default.
func isDualPortModel(model string) bool {
	return dualPortModelRe.MatchString(model)
}

// collapsePFsToOnePerNIC reduces a set of PFs to one per NIC (PCI bus:device
// prefix): for a NIC whose VPD model is dual-port (isDualPortModel) every PF is
// kept — each physical port is its own rail — while for any other NIC only the
// master (lowest-function) PF survives, since its sibling PFs are planes of a
// single rail rather than independent rails. Input order is preserved among the
// kept PFs, and the count of dropped PFs is returned so the caller can log a
// summary.
//
// It is intended to run on the east-west subset only; north-south PFs should be
// filtered out by the caller (they are excluded from rails and manifests
// anyway). A PF with a malformed PCI address (no function suffix) is always
// kept, never silently dropped.
func collapsePFsToOnePerNIC(pfs []config.PFConfig) (kept []config.PFConfig, dropped int) {
	type nicInfo struct {
		dualPort bool
		master   string
	}
	nics := map[string]*nicInfo{}
	for i := range pfs {
		prefix, ok := pciBusDevicePrefix(pfs[i].PciAddress)
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
		if isDualPortModel(pfs[i].Model) {
			info.dualPort = true
		}
	}

	kept = make([]config.PFConfig, 0, len(pfs))
	for i := range pfs {
		prefix, ok := pciBusDevicePrefix(pfs[i].PciAddress)
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

// templateContext wraps the full config but presents a single ClusterConfig group.
// The ClusterConfig field shadows the slice field in LaunchKitConfig,
// so templates can use .ClusterConfig.PFs, .ClusterConfig.NodeSelector, etc.
type templateContext struct {
	*config.LaunchKitConfig
	ClusterConfig *config.ClusterConfig
}

// ProcessTemplate processes a Go template file with the given config.
// Returns a map of filename → rendered content. Templates that reference
// .ClusterConfig are rendered once per group (producing separate files),
// while other templates are rendered once.
func ProcessTemplate(templatePath string, cfg *config.LaunchKitConfig, groupFilter string) (map[string]string, error) {
	// Read the template file
	templateContent, err := os.ReadFile(templatePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read template file %s: %w", templatePath, err)
	}

	// Parse the template with helper functions
	tmpl, err := template.New(filepath.Base(templatePath)).Funcs(templateFuncs).Parse(string(templateContent))
	if err != nil {
		return nil, fmt.Errorf("failed to parse template %s: %w", templatePath, err)
	}

	baseName := filepath.Base(templatePath)
	// Per-group rendering is triggered by access to a per-group property
	// (`.ClusterConfig.PFs`, `.ClusterConfig.Identifier`, etc.). Bare
	// `.ClusterConfig` — used to iterate the slice in once-render templates
	// like the 26.1 legacy NCP block — does NOT trigger per-group mode.
	usesClusterConfig := strings.Contains(string(templateContent), ".ClusterConfig.")

	if !usesClusterConfig || len(cfg.ClusterConfig) == 0 {
		// Render once — no per-group variation
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, cfg); err != nil {
			return nil, fmt.Errorf("failed to execute template %s: %w", templatePath, err)
		}
		// Skip empty output (e.g., template guarded by a false condition)
		if strings.TrimSpace(buf.String()) == "" {
			return map[string]string{}, nil
		}
		return map[string]string{baseName: buf.String()}, nil
	}

	// Determine which groups to render
	groups := cfg.ClusterConfig
	singleGroupMode := false
	if groupFilter != "" {
		filtered := []config.ClusterConfig{}
		for _, g := range groups {
			if g.Identifier == groupFilter {
				filtered = append(filtered, g)
			}
		}
		if len(filtered) == 0 {
			return nil, fmt.Errorf("group %q not found in ClusterConfig", groupFilter)
		}
		groups = filtered
		singleGroupMode = true
	}

	// Render once per group → separate file per group
	results := map[string]string{}
	ext := filepath.Ext(baseName)
	nameNoExt := strings.TrimSuffix(baseName, ext)

	// Auto-generate per-group subnets if configured (startingSubnet + mask + offset
	// are set, but the explicit subnets list is empty).
	var groupSubnets [][]config.NvIpamSubnetConfig
	autoGenSubnets := cfg.NvIpam != nil &&
		cfg.NvIpam.StartingSubnet != "" &&
		cfg.NvIpam.Mask > 0 &&
		cfg.NvIpam.Offset > 0 &&
		len(cfg.NvIpam.Subnets) == 0

	if autoGenSubnets {
		multirail := cfg.Profile != nil && cfg.Profile.Multirail

		// Calculate per-group subnet counts
		totalSubnets := 0
		groupCounts := make([]int, len(groups))
		for gi := range groups {
			ewPFs := filterEastWestPFs(groups[gi].PFs)
			if multirail && len(ewPFs) > 0 {
				groupCounts[gi] = len(ewPFs)
			} else {
				groupCounts[gi] = 1
			}
			totalSubnets += groupCounts[gi]
		}

		allSubnets, err := config.GenerateSubnets(
			cfg.NvIpam.StartingSubnet, cfg.NvIpam.Mask, cfg.NvIpam.Offset, totalSubnets)
		if err != nil {
			return nil, fmt.Errorf("failed to auto-generate subnets: %w", err)
		}
		if err := config.ApplyReservedExclusions(
			allSubnets, cfg.NvIpam.ReserveFirstIPs, cfg.NvIpam.ReserveLastIPs); err != nil {
			return nil, fmt.Errorf("failed to auto-generate subnets: %w", err)
		}

		groupSubnets = make([][]config.NvIpamSubnetConfig, len(groups))
		subnetOffset := 0
		for gi := range groups {
			groupSubnets[gi] = allSubnets[subnetOffset : subnetOffset+groupCounts[gi]]
			subnetOffset += groupCounts[gi]
		}
	}

	for i := range groups {
		// When --group is used, clear the identifier so CR names and filenames
		// don't carry the group suffix (only one group is being rendered).
		renderGroup := groups[i]
		if singleGroupMode {
			renderGroup.Identifier = ""
		}
		// Filter out north-south PFs so templates only see east-west devices.
		// This keeps indices sequential for naming (a, b, c, d instead of a, d, e, f).
		renderGroup.PFs = filterEastWestPFs(renderGroup.PFs)

		// For non-Spectrum-X profiles with NicInterfaceNameTemplate enabled,
		// pre-compute NetworkInterface and RdmaDevice names on each PF.
		// Templates use $pf.NetworkInterface for pfNames selectors.
		if cfg.NicConfigurationOperator != nil &&
			cfg.NicConfigurationOperator.DeployNicInterfaceNameTemplate &&
			!isSpectrumX(cfg) {
			for j := range renderGroup.PFs {
				if renderGroup.PFs[j].Rail != nil {
					rail := *renderGroup.PFs[j].Rail
					renderGroup.PFs[j].NetworkInterface = applyPrefix(
						cfg.NicConfigurationOperator.NetdevPrefix, 0, 0, rail)
					renderGroup.PFs[j].RdmaDevice = applyPrefix(
						cfg.NicConfigurationOperator.RdmaPrefix, 0, 0, rail)
				}
			}
		}

		// When auto-generating subnets, give each group its own unique slice
		// so subnets don't overlap between groups.
		cfgForGroup := cfg
		if autoGenSubnets {
			groupCfg := *cfg
			groupNvIpam := *cfg.NvIpam
			groupNvIpam.Subnets = groupSubnets[i]
			groupCfg.NvIpam = &groupNvIpam
			cfgForGroup = &groupCfg
		}

		ctx := &templateContext{
			LaunchKitConfig: cfgForGroup,
			ClusterConfig:          &renderGroup,
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, ctx); err != nil {
			return nil, fmt.Errorf("failed to execute template %s for group %s: %w", templatePath, groups[i].Identifier, err)
		}
		id := renderGroup.Identifier
		fileName := baseName
		if id != "" {
			fileName = fmt.Sprintf("%s-%s%s", nameNoExt, id, ext)
		}
		// Skip empty output (e.g., template guarded by a false condition)
		if strings.TrimSpace(buf.String()) != "" {
			results[fileName] = buf.String()
		}
	}

	return results, nil
}

// filterEastWestPFs returns only PFs with traffic == "east-west",
// so that template indices stay sequential for naming (a, b, c, d).
func filterEastWestPFs(pfs []config.PFConfig) []config.PFConfig {
	var filtered []config.PFConfig
	for _, pf := range pfs {
		if pf.Traffic == "east-west" {
			filtered = append(filtered, pf)
		}
	}
	return filtered
}

// groupFabric returns the discovered fabric ("Ethernet" or "InfiniBand")
// for a group, plus a bool indicating whether the field is set. Reads
// directly from `group.LinkType`, which `discoverGroupFabric` populates
// only when every east-west port produced a confirmed and unanimous
// verdict — when the field is empty, discovery couldn't prove the
// cluster's fabric and downstream code should treat it as unknown.
//
// Used by Unit 8's declarative defaults to fill `--fabric` when the user
// doesn't supply it.
func groupFabric(group config.ClusterConfig) (string, bool) {
	return group.LinkType, group.LinkType != ""
}

// GenerateProfileDeploymentFiles processes all template files in a profile directory
func (p *NetworkOperatorPlugin) GenerateProfileDeploymentFiles(profile *profiles.Profile, cfg *config.LaunchKitConfig) (map[string]string, error) {
	// Apply --groups / --gpu-type filter to source groups before merging.
	// `applyGroupFilter` enforces mutual exclusivity, errors on empty match,
	// and is a no-op when neither flag is set.
	filtered, err := applyGroupFilter(cfg.ClusterConfig, p.Groups, p.GpuType)
	if err != nil {
		return nil, err
	}

	// Temporary workaround for the NicInterfaceNameTemplate collision
	// case: when a single physical NIC has multiple PFs and l8k wants
	// them on different rails, the nic-configuration-operator's data
	// model can't express it (one NicDevice → one railIndex), and the
	// rendered udev rules give every PF on that NIC the same name
	// (e.g. both 08:00.0 and 08:00.1 → "eth_r0"). Detection of that
	// specific case is non-trivial; for now we skip the template when
	// there's only one source group to render — the PF layout is
	// homogeneous across the group's nodes, so PCI-based selectors in
	// the SR-IOV / Network CRs are stable. Exception: rdma_shared
	// profiles whose ifNames selector has no PCI fallback — keep the
	// template enabled there so the rdmaSharedDevicePlugin can still
	// find its NICs.
	cfg = maybeDisableInterfaceNameTemplate(cfg, filtered)

	// Bucket the filtered set by (gpuType, railCount), build the merged
	// ClusterConfig per bucket, and decide Mode A vs B per bucket by
	// comparing to the original (pre-filter) bucket. The plans drive
	// the per-template scope dispatch below.
	plans, _ := planRender(cfg.ClusterConfig, filtered)

	// Pre-allocate subnets across all plans' merged groups so per-plan
	// `ProcessTemplate` calls don't independently re-allocate from
	// offset 0.
	planSubnets, err := preallocatePlanSubnets(cfg, plans)
	if err != nil {
		return nil, err
	}

	results := make(map[string]string)
	skipWorkloadTemplates := cfg.Workload != nil && cfg.Workload.Manifest != ""

	for _, templatePath := range profile.Templates {
		if skipWorkloadTemplates && isWorkloadTemplate(filepath.Base(templatePath)) {
			continue
		}
		rendered, err := renderForScope(templatePath, cfg, plans, planSubnets)
		if err != nil {
			return nil, fmt.Errorf("failed to process template %s: %w", templatePath, err)
		}
		for filename, content := range rendered {
			results[filename] = content
		}
	}

	// Render user-provided workload manifest per group. Filter has already
	// been applied to cfg.ClusterConfig above.
	if skipWorkloadTemplates {
		for _, group := range cfg.ClusterConfig {
			ewGroup := group
			ewGroup.PFs = filterEastWestPFs(group.PFs)
			rendered, err := patchWorkloadManifest(cfg.Workload.Manifest, cfg, &ewGroup)
			if err != nil {
				return nil, fmt.Errorf("failed to patch workload manifest for group %s: %w", group.Identifier, err)
			}
			filename := "90-workload.yaml"
			if group.Identifier != "" {
				filename = fmt.Sprintf("90-workload-%s.yaml", group.Identifier)
			}
			results[filename] = rendered
		}
	}

	return results, nil
}

// hasEmptyNetworkInterfaceNames returns true if any east-west PF across all
// groups has an empty NetworkInterface. This happens when discovery finds
// multiple nodes per group and omits device names for safety. In rdma_shared
// deployments the rdmaSharedDevicePlugin needs interface names (ifNames
// selector), so NicInterfaceNameTemplate must be enabled to provide them.
func hasEmptyNetworkInterfaceNames(groups []config.ClusterConfig) bool {
	for i := range groups {
		for _, pf := range filterEastWestPFs(groups[i].PFs) {
			if pf.NetworkInterface == "" {
				return true
			}
		}
	}
	return false
}

// isRdmaShared returns true if the config targets an rdma_shared deployment.
func isRdmaShared(cfg *config.LaunchKitConfig) bool {
	return cfg.Profile != nil && cfg.Profile.Deployment == "rdma_shared"
}

// maybeDisableInterfaceNameTemplate returns a (possibly shallow-copied)
// cfg with NicConfigurationOperator.DeployNicInterfaceNameTemplate forced
// to false when there's exactly one source group to render and the
// profile can fall back to PCI-based selectors. See the long comment at
// the call site for the rationale. Returns the original cfg unmodified
// when the workaround doesn't apply.
func maybeDisableInterfaceNameTemplate(cfg *config.LaunchKitConfig, filteredGroups []config.ClusterConfig) *config.LaunchKitConfig {
	if cfg.NicConfigurationOperator == nil || !cfg.NicConfigurationOperator.DeployNicInterfaceNameTemplate {
		return cfg
	}
	if len(filteredGroups) != 1 {
		return cfg
	}
	// Spectrum-X uses a distinct rendering path that always needs the
	// template — don't touch it.
	if isSpectrumX(cfg) {
		return cfg
	}
	// rdma_shared profiles select PFs via `ifNames` (no PCI fallback in
	// the device-plugin selector), so the template is the only way to
	// give the plugin a stable name when discovery didn't already
	// populate one (multi-node groups). Skip the workaround when the
	// template is the only source of names.
	if isRdmaShared(cfg) && hasEmptyNetworkInterfaceNames(filteredGroups) {
		return cfg
	}

	log.Log.Info("Disabling NicInterfaceNameTemplate: single source group, PCI-based selectors are sufficient",
		"group", filteredGroups[0].Identifier,
		"deployment", profileDeploymentName(cfg))

	// Shallow-copy cfg and the operator struct so we don't mutate the
	// caller's config.
	ncoCopy := *cfg.NicConfigurationOperator
	ncoCopy.DeployNicInterfaceNameTemplate = false
	cfgCopy := *cfg
	cfgCopy.NicConfigurationOperator = &ncoCopy
	return &cfgCopy
}

func profileDeploymentName(cfg *config.LaunchKitConfig) string {
	if cfg == nil || cfg.Profile == nil {
		return ""
	}
	return cfg.Profile.Deployment
}

// isSpectrumX returns true if the config targets a Spectrum-X profile.
func isSpectrumX(cfg *config.LaunchKitConfig) bool {
	return cfg.Profile != nil && cfg.Profile.SpectrumX != nil && cfg.Profile.SpectrumX.Enable
}

// sanitizeIdentifier converts a product type string to a valid K8s name component.
// Lowercases the string and replaces spaces with hyphens.
func sanitizeIdentifier(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "-")
	return s
}

// boundedSuffix returns "-<value>" bounded to at most maxLen chars (including
// the leading "-"). When the natural "-<s>" form fits, it is returned as-is;
// otherwise the prefix is truncated and an 8-hex FNV-32a hash of the original
// is appended, matching MachineLabelValue's algorithm. Empty input returns "".
//
// Used by templates that compose label values from ClusterConfig.Identifier:
// the rendered "<prefix><boundedSuffix>" combination must respect k8s'
// 63-char label-value limit. Callers pass `63 - len(prefix)` as maxLen.
func boundedSuffix(maxLen int, s string) string {
	if s == "" {
		return ""
	}
	natural := "-" + s
	if len(natural) <= maxLen {
		return natural
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	hashTail := fmt.Sprintf("-%08x", h.Sum32())
	// 1 (leading "-") + prefixBudget + len(hashTail) == maxLen
	prefixBudget := maxLen - 1 - len(hashTail)
	if prefixBudget < 1 {
		// Degenerate budget — return the hash alone with its leading "-".
		return hashTail
	}
	prefix := strings.TrimRight(s[:prefixBudget], "-_.")
	return "-" + prefix + hashTail
}

// mergeCompatibleGroups merges ClusterConfig groups that share the same gpuType
// and number of east-west rails into a single group per gpuType.
// For each merged group:
//   - Identifier = sanitized gpuType (lowercase, spaces→hyphens)
//   - NodeSelector = {"nvidia.com/gpu.product": gpuType}
//   - RailPciAddresses = per-rail list of all PCI addresses from source groups
//   - WorkerNodes = union of all worker nodes
//   - Capabilities = aggregated (union)
//
// Groups with empty gpuType are never merged.
// Single-entry buckets are returned as-is.
func mergeCompatibleGroups(groups []config.ClusterConfig, useNameTemplates bool) ([]config.ClusterConfig, bool) {
	type mergeKey struct {
		gpuType string
		railCount   int
	}

	// Group indices by (gpuType, railCount). Source groups carry a
	// per-(machineType, gpuType) machine-label identifier (set by
	// `applyMachineLabelToGroups` at discovery time), but auto-merge keys
	// on gpuType only — two server SKUs with the same GPU type *do*
	// auto-merge. The machine label still drives the per-source nodeSelector
	// and identifier; the merged group falls back to the GPU-product label
	// since the source groups may not share a machine label.
	bucketOrder := []mergeKey{}
	buckets := map[mergeKey][]int{}

	for i, g := range groups {
		ewCount := len(filterEastWestPFs(g.PFs))
		key := mergeKey{gpuType: g.GPUType, railCount: ewCount}
		if g.GPUType == "" {
			// Never merge groups without a gpuType — use a unique key per group
			key = mergeKey{gpuType: fmt.Sprintf("__empty_%d", i), railCount: ewCount}
		}
		log.Log.V(1).Info("Computed merge key for source group",
			"sourceIndex", i,
			"identifier", g.Identifier,
			"machineType", g.MachineType,
			"gpuType", g.GPUType,
			"eastWestRailCount", ewCount,
			"mergeKey", fmt.Sprintf("%s/%d", key.gpuType, key.railCount))
		if _, exists := buckets[key]; !exists {
			bucketOrder = append(bucketOrder, key)
		}
		buckets[key] = append(buckets[key], i)
	}

	var result []config.ClusterConfig
	hadPciConflicts := false
	for _, key := range bucketOrder {
		indices := buckets[key]

		first := groups[indices[0]]
		if len(indices) == 1 || first.GPUType == "" {
			reason := "single source group in bucket"
			if first.GPUType == "" {
				reason = "gpuType empty — never merge"
			}
			log.Log.V(1).Info("Bucket kept separate (not merged)",
				"mergeKey", fmt.Sprintf("%s/%d", key.gpuType, key.railCount),
				"identifier", first.Identifier,
				"reason", reason)
			result = append(result, first)
			continue
		}

		// Check for cross-rail PCI address conflicts before merging.
		// When NicInterfaceNameTemplate is enabled, merge despite conflicts
		// (renamed pfNames avoid the conflict) and track that it happened.
		if hasRailPciConflict(groups, indices) {
			if !useNameTemplates {
				log.Log.V(1).Info("Bucket kept separate (cross-rail PCI conflict; name templates disabled)",
					"mergeKey", fmt.Sprintf("%s/%d", key.gpuType, key.railCount),
					"sourceIndices", indices)
				for _, idx := range indices {
					result = append(result, groups[idx])
				}
				continue
			}
			hadPciConflicts = true
			log.Log.V(1).Info("Bucket merged despite PCI conflict; name templates will resolve it",
				"mergeKey", fmt.Sprintf("%s/%d", key.gpuType, key.railCount),
				"sourceIndices", indices)
		}

		// Merge all groups in this bucket
		merged := buildMergedGroup(groups, indices)
		log.Log.V(1).Info("Bucket merged into single render group",
			"mergeKey", fmt.Sprintf("%s/%d", key.gpuType, key.railCount),
			"sourceIndices", indices,
			"mergedIdentifier", merged.Identifier,
			"mergedNodeCount", len(merged.WorkerNodes))
		result = append(result, merged)
	}

	log.Log.V(1).Info("Group merge complete",
		"sourceGroups", len(groups),
		"renderGroups", len(result),
		"pciConflictResolvedByNameTemplates", hadPciConflicts)
	return result, hadPciConflicts
}

// hasRailPciConflict returns true if any PCI address appears at different rail
// positions across the groups identified by indices. This would cause the device
// plugin to claim a device for the wrong rail on some nodes.
func hasRailPciConflict(groups []config.ClusterConfig, indices []int) bool {
	// Map each PCI address to the rail index where it first appears
	addrToRail := map[string]int{}
	for _, idx := range indices {
		for _, pf := range filterEastWestPFs(groups[idx].PFs) {
			if pf.Rail == nil {
				continue
			}
			if prevRail, seen := addrToRail[pf.PciAddress]; seen {
				if prevRail != *pf.Rail {
					return true
				}
			} else {
				addrToRail[pf.PciAddress] = *pf.Rail
			}
		}
	}
	return false
}

// buildMergedGroup creates a single ClusterConfig from multiple groups
// sharing the same gpuType and east-west rail count. Source groups may
// span different machineTypes, so the merged group's identifier and
// NodeSelector fall back to the GPU product (sanitized gpuType /
// `nvidia.com/gpu.product`) rather than the per-source machine label.
func buildMergedGroup(groups []config.ClusterConfig, indices []int) config.ClusterConfig {
	first := groups[indices[0]]
	gpuType := first.GPUType

	// Collect east-west PFs per group (all have the same count)
	ewPFsByGroup := make([][]config.PFConfig, len(indices))
	for i, idx := range indices {
		ewPFsByGroup[i] = filterEastWestPFs(groups[idx].PFs)
	}
	railCount := len(ewPFsByGroup[0])

	// Build RailPciAddresses: for each rail, collect unique PCI addresses across groups,
	// preserving first-occurrence order. Nodes often share a PCI layout, so without dedup
	// downstream templates (e.g. SriovNetworkNodePolicy rootDevices) would emit duplicates.
	railPciAddresses := make([][]string, railCount)
	for rail := 0; rail < railCount; rail++ {
		seen := map[string]bool{}
		addrs := make([]string, 0, len(indices))
		for _, pfs := range ewPFsByGroup {
			addr := pfs[rail].PciAddress
			if seen[addr] {
				continue
			}
			seen[addr] = true
			addrs = append(addrs, addr)
		}
		railPciAddresses[rail] = addrs
	}

	// Collect all worker nodes
	var allNodes []string
	for _, idx := range indices {
		allNodes = append(allNodes, groups[idx].WorkerNodes...)
	}
	slices.Sort(allNodes)

	// Aggregate capabilities
	caps := &config.ClusterCapabilities{
		Nodes: &config.NodesCapabilities{},
	}
	for _, idx := range indices {
		g := groups[idx]
		if g.Capabilities != nil && g.Capabilities.Nodes != nil {
			caps.Nodes.Sriov = caps.Nodes.Sriov || g.Capabilities.Nodes.Sriov
			caps.Nodes.Rdma = caps.Nodes.Rdma || g.Capabilities.Nodes.Rdma
			caps.Nodes.Ib = caps.Nodes.Ib || g.Capabilities.Nodes.Ib
		}
	}

	// Merge OFED-dependent modules (union across all groups, deduplicated and sorted)
	depModSet := map[string]bool{}
	for _, idx := range indices {
		for _, mod := range groups[idx].ThirdPartyRDMAModules {
			depModSet[mod] = true
		}
	}
	var mergedDepMods []string
	if len(depModSet) > 0 {
		mergedDepMods = make([]string, 0, len(depModSet))
		for mod := range depModSet {
			mergedDepMods = append(mergedDepMods, mod)
		}
		slices.Sort(mergedDepMods)
	}

	// Merge storage modules (same union pattern)
	storageModSet := map[string]bool{}
	for _, idx := range indices {
		for _, mod := range groups[idx].StorageModules {
			storageModSet[mod] = true
		}
	}
	var mergedStorageMods []string
	if len(storageModSet) > 0 {
		mergedStorageMods = make([]string, 0, len(storageModSet))
		for mod := range storageModSet {
			mergedStorageMods = append(mergedStorageMods, mod)
		}
		slices.Sort(mergedStorageMods)
	}

	// Source groups may have different machineTypes (and therefore different
	// per-source machine labels), so the merged group can't carry a single
	// MachineLabelKey nodeSelector. Identifier follows the resource-name
	// convention (lowercase via sanitizeIdentifier); NodeSelector keys on
	// GPULabelKey, whose raw value matches the GPU operator's
	// `nvidia.com/gpu.product` value — `l8k discover` wrote it onto every
	// node alongside the machine label, so it's stable across merged
	// source machineTypes by construction.
	return config.ClusterConfig{
		Identifier:           sanitizeIdentifier(gpuType),
		MachineType:          first.MachineType,
		GPUType:              gpuType,
		LinkType:             first.LinkType,
		Capabilities:         caps,
		PFs:                  first.PFs, // Representative PFs from first group
		WorkerNodes:          allNodes,
		ThirdPartyRDMAModules: mergedDepMods,
		StorageModules:        mergedStorageMods,
		NodeSelector:         map[string]string{config.GPULabelKey: gpuType},
		RailPciAddresses:     railPciAddresses,
	}
}
