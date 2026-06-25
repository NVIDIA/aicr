// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"bufio"
	"fmt"
	"strings"
)

// CRScope describes how a Kubernetes Kind rendered by `l8k generate` is
// scoped relative to merged source groups. Used by the renderer to decide
// how many CRs to emit per (filtered, merged) bucket and which template
// context to feed each render.
type CRScope int

const (
	// ScopeUnknown is the default for Kinds not in the registry. The
	// renderer falls back to legacy per-render-group behaviour.
	ScopeUnknown CRScope = iota

	// ScopeClusterWide: a single CR per cluster, regardless of source
	// groups. Example: NicClusterPolicy.
	ScopeClusterWide

	// ScopeAggregate: one CR per merged bucket. The CR uses an extended
	// `nodeSelectorTerms` / `matchExpressions In: [...]` selector so it
	// can pick the strict-subset of nodes targeted by `--groups` under
	// Mode B. Examples: IPPool, the example DaemonSet.
	ScopeAggregate

	// ScopeBucketed: one CR per merged bucket. The CR has no node
	// selector — it's named (referenced by other CRs) and bound to nodes
	// indirectly via the resourceName its companion NodePolicy
	// registers, or via Multus annotation. Examples: SriovNetwork,
	// HostDeviceNetwork, IPoIBNetwork, MacvlanNetwork, OVSNetwork,
	// SriovIBNetwork, CIDRPool.
	ScopeBucketed

	// ScopeSimpleSelect: one CR per merged bucket in Mode A; under Mode
	// B (filtered subset), N CRs per bucket — one per source group, each
	// with its own flat-map nodeSelector. All N CRs register the same
	// `MergedIdentifier`-keyed shared resource (resourceName,
	// poolName-equivalent) so companion ScopeBucketed / ScopeAggregate
	// CRs can reference one stable name. Examples: NicNodePolicy,
	// SriovNetworkNodePolicy, SriovNetworkPoolConfig,
	// SpectrumXRailPoolConfig, NicConfigurationTemplate.
	ScopeSimpleSelect

	// ScopePerSource: always one CR per source group, regardless of
	// merge. Used for Kinds whose body depends on machine-specific data
	// (e.g. PCI addresses) that cannot meaningfully merge across
	// source machineTypes. Example: NicInterfaceNameTemplate.
	ScopePerSource
)

// String returns a short name for the scope, for log lines.
func (s CRScope) String() string {
	switch s {
	case ScopeClusterWide:
		return "ClusterWide"
	case ScopeAggregate:
		return "Aggregate"
	case ScopeBucketed:
		return "Bucketed"
	case ScopeSimpleSelect:
		return "SimpleSelect"
	case ScopePerSource:
		return "PerSource"
	default:
		return "Unknown"
	}
}

// crScopeByKind maps every Kubernetes Kind that `l8k generate` emits to
// the rendering scope it belongs in. Kinds not listed default to
// ScopeUnknown — the renderer treats them as per-render-group (legacy
// behaviour).
//
// When adding a profile template that emits a new Kind, register it
// here so the renderer dispatches correctly under `--groups`/`--gpu-type`
// filtering.
var crScopeByKind = map[string]CRScope{
	// Cluster-wide singleton — multus, CNI plugins, NV-IPAM, etc. live
	// inside a single NicClusterPolicy.
	"NicClusterPolicy": ScopeClusterWide,

	// Aggregate (extended selector — `In: [machine-labels]` under Mode B)
	"IPPool":    ScopeAggregate,
	"DaemonSet": ScopeAggregate,

	// Bucketed (no node selector; names reference shared bucket id)
	"SriovNetwork":      ScopeBucketed,
	"SriovIBNetwork":    ScopeBucketed,
	"HostDeviceNetwork": ScopeBucketed,
	"IPoIBNetwork":      ScopeBucketed,
	"MacvlanNetwork":    ScopeBucketed,
	"OVSNetwork":        ScopeBucketed,
	"CIDRPool":          ScopeBucketed,

	// Simple flat-map nodeSelector — render once per bucket in Mode A,
	// once per source under Mode B. Shared resources (resourceName,
	// poolName, etc.) reference the merged bucket identifier so all
	// per-source CRs register the same kubelet resource.
	"NicNodePolicy":            ScopeSimpleSelect,
	"SriovNetworkNodePolicy":   ScopeSimpleSelect,
	"SriovNetworkPoolConfig":   ScopeSimpleSelect,
	"SpectrumXRailPoolConfig":  ScopeSimpleSelect,
	"NicConfigurationTemplate": ScopeSimpleSelect,

	// Per-source — body depends on machine-specific data like PCI
	// addresses; never merged across source groups.
	"NicInterfaceNameTemplate": ScopePerSource,
}

// ScopeForKind returns the registered scope for a Kubernetes Kind.
// Returns ScopeUnknown if the Kind isn't in the registry.
func ScopeForKind(kind string) CRScope {
	return crScopeByKind[kind]
}

// extractKindFromTemplate finds the first `kind: X` line in a template
// source and returns X. Comments (`#`) and empty lines are ignored. Go
// template constructs (`{{...}}`) inside `kind:` lines are not
// supported — every profile template in this repo uses a literal Kind.
// Returns an error if no `kind:` line is found.
func extractKindFromTemplate(src string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(src))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "kind:"); ok {
			kind := strings.TrimSpace(rest)
			// Strip a trailing comment if present.
			if hash := strings.IndexByte(kind, '#'); hash >= 0 {
				kind = strings.TrimSpace(kind[:hash])
			}
			if kind == "" {
				return "", fmt.Errorf("template has empty `kind:` value")
			}
			return kind, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan template: %w", err)
	}
	return "", fmt.Errorf("template has no `kind:` line")
}
