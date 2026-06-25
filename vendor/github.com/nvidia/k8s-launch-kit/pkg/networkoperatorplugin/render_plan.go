// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// helmValuesTemplateName is the conventional filename for the per-profile
// Helm values template. The renderer special-cases this template: it is
// always cluster-scoped (no per-group iteration, no Kind-based scope
// dispatch) and is emitted on disk as `values.yaml` so it matches the
// helm convention. `l8k deploy` reads `values.yaml` from the deployment
// directory to install or upgrade the network-operator chart in Phase 0,
// before applying the post-install CR manifests.
const helmValuesTemplateName = "00-values.yaml"

// helmValuesOutputName is the on-disk filename for the rendered helm values.
const helmValuesOutputName = "values.yaml"

// isHelmValuesTemplate reports whether a template file is the per-profile
// helm values file (see `helmValuesTemplateName`).
func isHelmValuesTemplate(name string) bool {
	return name == helmValuesTemplateName
}

// RenderBucket is the unit of work the rendering pipeline iterates over.
// One bucket per (gpuType, railCount) tuple that survived `--groups` /
// `--gpu-type` filtering.
type RenderBucket struct {
	// Merged is the merged ClusterConfig for this bucket. Used by
	// ScopeBucketed, ScopeAggregate, and ScopeSimpleSelect-in-Mode-A
	// templates. Its `Identifier` and `MergedIdentifier` are equal (the
	// bucket's id). When ModeB is true, `SourceMachineLabels` is
	// populated for ScopeAggregate's `In` selector.
	Merged config.ClusterConfig
	// Sources is the per-source ClusterConfig list for this bucket
	// (post-filter). Each entry's `MergedIdentifier` matches
	// `Merged.MergedIdentifier`, so per-source renders that reference
	// shared resourceName / poolName / etc. all converge on the same
	// stable name. Used by ScopeSimpleSelect-in-Mode-B and
	// ScopePerSource.
	Sources []config.ClusterConfig
	// ModeB is true when the filter selected a strict subset of this
	// bucket's original source groups. ScopeSimpleSelect templates
	// switch from "render once with Merged" to "render N times with
	// Sources" when ModeB is true.
	ModeB bool
	// HadPciConflict mirrors the existing per-bucket flag for
	// NicInterfaceNameTemplate gating.
	HadPciConflict bool
}

// planRender bucketises the filtered source groups by (gpuType,
// railCount), compares each filtered bucket against the original (pre-
// filter) bucket to decide Mode A vs B, and returns one RenderBucket
// per resulting bucket. Buckets and their sources are emitted in the
// same input order as `filteredGroups` for deterministic output.
//
// The second return value is the OR of `HadPciConflict` across all
// buckets — preserved for diagnostics; the renderer always emits
// NicInterfaceNameTemplate, so this is informational only.
func planRender(originalGroups, filteredGroups []config.ClusterConfig) ([]RenderBucket, bool) {
	originalByKey := bucketize(originalGroups)
	filteredOrder, filteredByKey := bucketizeOrdered(filteredGroups)

	plans := make([]RenderBucket, 0, len(filteredOrder))
	overallHadConflicts := false

	for _, key := range filteredOrder {
		filteredSources := filteredByKey[key]
		originalSources := originalByKey[key]
		modeB := !sourceIdentitySetEqual(filteredSources, originalSources)

		merged, hadConflict := buildBucketMerged(filteredSources)
		merged.MergedIdentifier = merged.Identifier
		if modeB {
			// Read each source's machine label from
			// `src.NodeSelector[MachineLabelKey]` — that's the
			// authoritative value discover wrote to both the node and
			// to cluster-config.yaml. Per-source NodePolicies render
			// from the same `src.NodeSelector` (flat-map), so reading
			// from there here keeps the aggregate IPPool's `In` list
			// in lockstep with the per-source NodePolicy selectors.
			//
			// Falls back to `MachineLabelValue(machineType, gpuType)`
			// when the source has no machine label set (legacy configs
			// from before Unit 6, or groups whose machineType/gpuType
			// resolution failed at discover time). Empty means the
			// source contributes no label and the merged group's
			// NodeSelector path takes over in templates.
			labels := make([]string, 0, len(filteredSources))
			for _, src := range filteredSources {
				label := src.NodeSelector[config.MachineLabelKey]
				if label == "" {
					label = config.MachineLabelValue(src.MachineType, src.GPUType)
				}
				if label != "" {
					labels = append(labels, label)
				}
			}
			merged.SourceMachineLabels = labels
		}

		sources := make([]config.ClusterConfig, len(filteredSources))
		for i, src := range filteredSources {
			sources[i] = src
			sources[i].MergedIdentifier = merged.MergedIdentifier
		}

		plans = append(plans, RenderBucket{
			Merged:         merged,
			Sources:        sources,
			ModeB:          modeB,
			HadPciConflict: hadConflict,
		})
		if hadConflict {
			overallHadConflicts = true
		}

		log.Log.V(1).Info("Render plan for bucket",
			"bucket", merged.MergedIdentifier,
			"modeB", modeB,
			"sources", len(filteredSources),
			"sourceMachineLabels", merged.SourceMachineLabels)
	}

	return plans, overallHadConflicts
}

// bucketKey is the merge key — same shape as `mergeCompatibleGroups`.
type bucketKey struct {
	gpuType   string
	railCount int
}

// bucketize maps groups onto (gpuType, railCount) keys. Groups with an
// empty `gpuType` get a unique pseudo-key per index so they never
// merge — same convention as `mergeCompatibleGroups`.
func bucketize(groups []config.ClusterConfig) map[bucketKey][]config.ClusterConfig {
	out := map[bucketKey][]config.ClusterConfig{}
	for i, g := range groups {
		key := keyFor(g, i)
		out[key] = append(out[key], g)
	}
	return out
}

// bucketizeOrdered is like bucketize but also returns the bucket keys in
// first-occurrence order so the renderer's output is deterministic.
func bucketizeOrdered(groups []config.ClusterConfig) ([]bucketKey, map[bucketKey][]config.ClusterConfig) {
	out := map[bucketKey][]config.ClusterConfig{}
	order := []bucketKey{}
	for i, g := range groups {
		key := keyFor(g, i)
		if _, exists := out[key]; !exists {
			order = append(order, key)
		}
		out[key] = append(out[key], g)
	}
	return order, out
}

func keyFor(g config.ClusterConfig, idx int) bucketKey {
	ewCount := len(filterEastWestPFs(g.PFs))
	if g.GPUType == "" {
		return bucketKey{gpuType: fmt.Sprintf("__empty_%d", idx), railCount: ewCount}
	}
	return bucketKey{gpuType: g.GPUType, railCount: ewCount}
}

// sourceIdentitySetEqual returns true when two slices of source groups
// represent the same set of source groups, identified by the
// `Identifier` field. Order doesn't matter.
func sourceIdentitySetEqual(a, b []config.ClusterConfig) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	idsA := make([]string, len(a))
	for i, g := range a {
		idsA[i] = g.Identifier
	}
	idsB := make([]string, len(b))
	for i, g := range b {
		idsB[i] = g.Identifier
	}
	slices.Sort(idsA)
	slices.Sort(idsB)
	return slices.Equal(idsA, idsB)
}

// buildBucketMerged is the same logic as `mergeCompatibleGroups`'s
// per-bucket branch, but always produces a single merged ClusterConfig
// for the bucket regardless of source count. For a single-source
// bucket, the source group itself becomes the "merged" group (with no
// rail-PCI aggregation needed). The bool return signals whether the
// bucket has cross-rail PCI conflicts; the caller (`planRender`) uses
// it to gate NicInterfaceNameTemplate rendering. Even when conflicts
// + name-templates-disabled would prevent a clean merge, we still
// build a merged record so MergedIdentifier exists for shared-resource
// references — the SimpleSelect renderer falls back to per-source via
// Mode B in that case anyway.
func buildBucketMerged(sources []config.ClusterConfig) (config.ClusterConfig, bool) {
	if len(sources) == 0 {
		return config.ClusterConfig{}, false
	}
	if len(sources) == 1 {
		return sources[0], false
	}

	indices := make([]int, len(sources))
	for i := range sources {
		indices[i] = i
	}
	hadConflict := hasRailPciConflict(sources, indices)
	merged := buildMergedGroup(sources, indices)
	return merged, hadConflict
}

// ValidateGroupFilter is the launcher-level entry point for `--groups`
// / `--gpu-type` validation. It runs before profile selection so an
// unmatched filter errors immediately rather than silently succeeding
// (the empty-match check inside `GenerateProfileDeploymentFiles` only
// runs when a profile is configured). No-op when neither flag is set.
func (p *NetworkOperatorPlugin) ValidateGroupFilter(cfg *config.LaunchKitConfig) error {
	if cfg == nil {
		return nil
	}
	_, err := applyGroupFilter(cfg.ClusterConfig, p.Groups, p.GpuType)
	return err
}

// applyGroupFilter narrows `originalGroups` by `--groups` (identifier
// list) or `--gpu-type` (single value, case-insensitive). At most one
// of the two may be non-empty — the caller already enforces mutual
// exclusivity. An empty filter returns `originalGroups` unchanged.
// Returns an error when the filter matches no group (a typo
// otherwise produces an empty `./deployment/` silently).
func applyGroupFilter(originalGroups []config.ClusterConfig, groupIDs []string, gpuType string) ([]config.ClusterConfig, error) {
	if len(groupIDs) == 0 && gpuType == "" {
		return originalGroups, nil
	}
	if len(groupIDs) > 0 && gpuType != "" {
		// Defence in depth — cobra's MarkFlagsMutuallyExclusive should
		// have caught this earlier.
		return nil, fmt.Errorf("--groups and --gpu-type are mutually exclusive")
	}

	var matched []config.ClusterConfig
	if len(groupIDs) > 0 {
		want := map[string]bool{}
		for _, id := range groupIDs {
			want[id] = true
		}
		seen := map[string]bool{}
		for _, g := range originalGroups {
			if want[g.Identifier] {
				matched = append(matched, g)
				seen[g.Identifier] = true
			}
		}
		var missing []string
		for id := range want {
			if !seen[id] {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			slices.Sort(missing)
			return nil, fmt.Errorf(
				"--groups: no group with identifier(s): %s. Available identifiers: %s",
				strings.Join(missing, ", "),
				availableIdentifiersList(originalGroups),
			)
		}
		return matched, nil
	}

	wantGpu := strings.ToLower(strings.TrimSpace(gpuType))
	for _, g := range originalGroups {
		if strings.ToLower(g.GPUType) == wantGpu {
			matched = append(matched, g)
		}
	}
	if len(matched) == 0 {
		return nil, fmt.Errorf(
			"--gpu-type %q: no group matched. Available gpuTypes: %s",
			gpuType,
			availableGpuTypesList(originalGroups),
		)
	}
	return matched, nil
}

// availableIdentifiersList returns a sorted, comma-separated list of
// every group's `Identifier`. Used in error messages so the user can
// pick a valid value without having to grep the config.
func availableIdentifiersList(groups []config.ClusterConfig) string {
	if len(groups) == 0 {
		return "(no groups in cluster-config.yaml)"
	}
	ids := make([]string, 0, len(groups))
	for _, g := range groups {
		if g.Identifier != "" {
			ids = append(ids, g.Identifier)
		}
	}
	if len(ids) == 0 {
		return "(no identifiers set)"
	}
	slices.Sort(ids)
	return strings.Join(ids, ", ")
}

// availableGpuTypesList returns a sorted, deduplicated, comma-separated
// list of every group's `GPUType`. Used in `--gpu-type` error messages.
func availableGpuTypesList(groups []config.ClusterConfig) string {
	seen := map[string]bool{}
	for _, g := range groups {
		if g.GPUType != "" {
			seen[g.GPUType] = true
		}
	}
	if len(seen) == 0 {
		return "(no gpuType set on any group)"
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	slices.Sort(out)
	return strings.Join(out, ", ")
}

// renderForScope dispatches a single template's rendering across the
// render plans according to the Kind→Scope registry. Per-plan subnets
// (computed by `preallocatePlanSubnets`) are embedded into the per-call
// cfg so ProcessTemplate doesn't auto-allocate from offset 0 each time.
//
// Scopes:
//   - ScopeClusterWide / ScopeUnknown: render once with all merged
//     groups in `cfg.ClusterConfig` (legacy compatible).
//   - ScopeAggregate / ScopeBucketed: render once per plan with that
//     plan's merged group as the sole entry.
//   - ScopeSimpleSelect: Mode A → once per plan with merged; Mode B →
//     once per source group within the plan, each rendering with its
//     own per-source ClusterConfig (shared MergedIdentifier).
//   - ScopePerSource: render once per source group, regardless of mode.
func renderForScope(
	templatePath string,
	cfg *config.LaunchKitConfig,
	plans []RenderBucket,
	planSubnets [][]config.NvIpamSubnetConfig,
) (map[string]string, error) {
	// Helm values template: cluster-scoped (no per-group iteration, no
	// Kind-based dispatch since the file isn't a K8s manifest). Render
	// once against the merged ClusterConfig — same context as
	// ScopeClusterWide CR manifests — and emit under the helm-convention
	// filename `values.yaml`.
	if isHelmValuesTemplate(filepath.Base(templatePath)) {
		merged := mergedClusterConfigs(plans)
		renderCfg := withClusterConfig(cfg, merged, allSubnets(planSubnets))
		rendered, err := ProcessTemplate(templatePath, renderCfg, "")
		if err != nil {
			return nil, err
		}
		out := map[string]string{}
		for _, content := range rendered {
			out[helmValuesOutputName] = content
		}
		return out, nil
	}

	body, err := os.ReadFile(templatePath)
	if err != nil {
		return nil, fmt.Errorf("read template %s: %w", templatePath, err)
	}
	kind, kerr := extractKindFromTemplate(string(body))
	if kerr != nil {
		log.Log.V(1).Info("template kind unrecognized; using legacy render path",
			"template", templatePath, "err", kerr.Error())
	}
	scope := ScopeForKind(kind)

	results := map[string]string{}
	merge := func(rendered map[string]string) {
		for f, c := range rendered {
			results[f] = c
		}
	}

	switch scope {
	case ScopeClusterWide, ScopeUnknown:
		merged := mergedClusterConfigs(plans)
		renderCfg := withClusterConfig(cfg, merged, allSubnets(planSubnets))
		rendered, err := ProcessTemplate(templatePath, renderCfg, "")
		if err != nil {
			return nil, err
		}
		merge(rendered)

	case ScopeAggregate, ScopeBucketed:
		// Secondary-network CRs and their example test DaemonSets are fanned
		// out across cfg.NetworkNamespaces — one independent copy per
		// namespace. Every other Kind (IPPool, CIDRPool, …) renders once into
		// the current namespace, so shared resources are never duplicated.
		nsList := networkNamespacesForKind(cfg, kind)
		multiNS := len(nsList) > 1
		for _, ns := range nsList {
			nsCfg := withRenderNamespaces(cfg, ns, nsList)
			for i, plan := range plans {
				renderCfg := withClusterConfig(nsCfg, []config.ClusterConfig{plan.Merged}, subnetsAt(planSubnets, i))
				rendered, err := ProcessTemplate(templatePath, renderCfg, "")
				if err != nil {
					return nil, err
				}
				merge(suffixFilenamesWithNamespace(rendered, ns, multiNS))
			}
		}

	case ScopeSimpleSelect:
		for i, plan := range plans {
			if plan.ModeB {
				for _, src := range plan.Sources {
					renderCfg := withClusterConfig(cfg, []config.ClusterConfig{src}, subnetsAt(planSubnets, i))
					rendered, err := ProcessTemplate(templatePath, renderCfg, "")
					if err != nil {
						return nil, err
					}
					merge(rendered)
				}
			} else {
				renderCfg := withClusterConfig(cfg, []config.ClusterConfig{plan.Merged}, subnetsAt(planSubnets, i))
				rendered, err := ProcessTemplate(templatePath, renderCfg, "")
				if err != nil {
					return nil, err
				}
				merge(rendered)
			}
		}

	case ScopePerSource:
		for i, plan := range plans {
			for _, src := range plan.Sources {
				renderCfg := withClusterConfig(cfg, []config.ClusterConfig{src}, subnetsAt(planSubnets, i))
				rendered, err := ProcessTemplate(templatePath, renderCfg, "")
				if err != nil {
					return nil, err
				}
				merge(rendered)
			}
		}
	}

	return results, nil
}

// networkNSReplicatedKinds is the set of Kinds rendered once per
// cfg.NetworkNamespaces entry — the secondary-network attachment CRs plus the
// example test DaemonSet. Every other Kind renders once. CIDRPool and IPPool
// are deliberately absent: they are shared, never duplicated. OVSNetwork is a
// network CR but only Spectrum-X emits it, and Spectrum-X is excluded from
// namespace fan-out wholesale (see networkNamespacesForKind), so it isn't
// listed here.
var networkNSReplicatedKinds = map[string]bool{
	"SriovNetwork":      true,
	"SriovIBNetwork":    true,
	"HostDeviceNetwork": true,
	"MacvlanNetwork":    true,
	"IPoIBNetwork":      true,
	"DaemonSet":         true,
}

// networkNamespacesForKind returns the namespaces a Kind should be rendered
// into. Replicated network Kinds fan out across cfg.NetworkNamespaces;
// everything else (and every Spectrum-X Kind, which uses a distinct combined-CR
// rendering path not amenable to independent per-namespace copies) renders once
// into the current namespace (cfg.PodNamespace).
func networkNamespacesForKind(cfg *config.LaunchKitConfig, kind string) []string {
	if !isSpectrumX(cfg) && networkNSReplicatedKinds[kind] && len(cfg.NetworkNamespaces) > 0 {
		return cfg.NetworkNamespaces
	}
	return []string{cfg.PodNamespace}
}

// withRenderNamespaces returns a shallow copy of cfg scoped to a single
// render: PodNamespace is the "current" namespace the templates read, and
// NetworkNamespaces is narrowed to nsList — the exact set being rendered for
// this Kind. For a replicated Kind nsList is the full cfg.NetworkNamespaces
// (so `nsSuffix` sees len>1 and suffixes); for a shared Kind nsList is the
// single current namespace (so `nsSuffix` returns "" even if a template author
// adds it). This makes the nsSuffix contract — "suffix iff this render is one
// of several namespace copies" — correct by construction rather than relying
// on shared templates simply never calling the helper.
func withRenderNamespaces(cfg *config.LaunchKitConfig, ns string, nsList []string) *config.LaunchKitConfig {
	out := *cfg
	out.PodNamespace = ns
	out.NetworkNamespaces = nsList
	return &out
}

// suffixFilenamesWithNamespace appends "-<ns>" before the extension of every
// rendered filename when multiNS is true, so the per-namespace copies of a
// network Kind don't collide in the output map. A single namespace leaves
// filenames (and thus the on-disk output) byte-identical to the pre-feature
// behaviour.
func suffixFilenamesWithNamespace(rendered map[string]string, ns string, multiNS bool) map[string]string {
	if !multiNS {
		return rendered
	}
	out := make(map[string]string, len(rendered))
	for name, content := range rendered {
		ext := filepath.Ext(name)
		out[fmt.Sprintf("%s-%s%s", strings.TrimSuffix(name, ext), ns, ext)] = content
	}
	return out
}

// withClusterConfig returns a shallow copy of cfg with ClusterConfig
// overridden and (when subnets is non-nil) NvIpam.Subnets replaced.
func withClusterConfig(cfg *config.LaunchKitConfig, groups []config.ClusterConfig, subnets []config.NvIpamSubnetConfig) *config.LaunchKitConfig {
	out := *cfg
	out.ClusterConfig = groups
	if subnets != nil && cfg.NvIpam != nil {
		nv := *cfg.NvIpam
		nv.Subnets = subnets
		out.NvIpam = &nv
	}
	return &out
}

// preallocatePlanSubnets computes per-plan subnet slices when the
// auto-gen criteria are met (StartingSubnet+Mask+Offset set, no
// explicit Subnets). Returns nil to leave cfg.NvIpam untouched in the
// non-auto-gen case.
func preallocatePlanSubnets(cfg *config.LaunchKitConfig, plans []RenderBucket) ([][]config.NvIpamSubnetConfig, error) {
	if cfg.NvIpam == nil ||
		cfg.NvIpam.StartingSubnet == "" ||
		cfg.NvIpam.Mask == 0 ||
		cfg.NvIpam.Offset == 0 ||
		len(cfg.NvIpam.Subnets) > 0 {
		return nil, nil
	}
	multirail := cfg.Profile != nil && cfg.Profile.Multirail
	counts := make([]int, len(plans))
	total := 0
	for i, plan := range plans {
		ewPFs := filterEastWestPFs(plan.Merged.PFs)
		if multirail && len(ewPFs) > 0 {
			counts[i] = len(ewPFs)
		} else {
			counts[i] = 1
		}
		total += counts[i]
	}
	if total == 0 {
		return nil, nil
	}
	all, err := config.GenerateSubnets(cfg.NvIpam.StartingSubnet, cfg.NvIpam.Mask, cfg.NvIpam.Offset, total)
	if err != nil {
		return nil, fmt.Errorf("auto-generate subnets: %w", err)
	}
	if err := config.ApplyReservedExclusions(all, cfg.NvIpam.ReserveFirstIPs, cfg.NvIpam.ReserveLastIPs); err != nil {
		return nil, fmt.Errorf("auto-generate subnets: %w", err)
	}
	out := make([][]config.NvIpamSubnetConfig, len(plans))
	off := 0
	for i, n := range counts {
		out[i] = all[off : off+n]
		off += n
	}
	return out, nil
}

func mergedClusterConfigs(plans []RenderBucket) []config.ClusterConfig {
	out := make([]config.ClusterConfig, len(plans))
	for i, plan := range plans {
		out[i] = plan.Merged
	}
	return out
}

func allSubnets(planSubnets [][]config.NvIpamSubnetConfig) []config.NvIpamSubnetConfig {
	if planSubnets == nil {
		return nil
	}
	var out []config.NvIpamSubnetConfig
	for _, s := range planSubnets {
		out = append(out, s...)
	}
	return out
}

func subnetsAt(planSubnets [][]config.NvIpamSubnetConfig, i int) []config.NvIpamSubnetConfig {
	if planSubnets == nil || i >= len(planSubnets) {
		return nil
	}
	return planSubnets[i]
}

