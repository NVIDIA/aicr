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

package measurement

import (
	"fmt"
	"slices"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// The catalog describes which constraint measurement paths are ADDRESSABLE —
// which {Type, Subtype, Key} triples a supported producer can emit and a path
// form can name — not which readings a particular snapshot happens to carry.
//
// Addressability is a static contract. A path passing ValidatePath may still
// return ErrCodeNotFound from Path.Extract against a given snapshot; what the
// catalog rules out is a path that could never resolve against ANY snapshot.
//
// The distinction matters. K8s.aks-gpu-pools omits gpu-driver entirely when a
// cluster has no GPU pools, yet gpu-driver belongs in the catalog: a snapshot
// legitimately missing that reading must still evaluate as ErrCodeNotFound
// (the designed graceful-exclusion signal), while a path nothing could ever
// emit must fail at load. See issue #1783.
//
// Two independent address spaces exist per subtype because Path.Extract reads
// them from different places:
//
//	{Type}.{Subtype}.{Key}          -> Subtype.Data          (scalar)
//	{Type}.{Subtype}[sel].{Key}     -> Items[i].Data/.Context (item)
//
// Subtype.Context has no entry anywhere in this catalog because Extract never
// reads it. Listing an emitted-but-unreadable field would admit paths that can
// only ever return NotFound — the exact defect this catalog removes.
//
// MAINTENANCE: a producer change needs an entry here when it adds a subtype
// (unless the Type is openSubtype), adds a key to a CLOSED space, or changes
// how a space is addressed (items added to a scalar-only subtype, subtype names
// starting to carry dots). Adding a key to an OPEN space needs nothing — open
// spaces already accept producer-defined keys.
//
// Omitting a required entry does not weaken a check; it makes a legitimate
// constraint path fail at load. When a producer's key space is not provably
// fixed, declare it open rather than guessing a closed set.

// keySet is one address space within a subtype. The zero value is a closed,
// empty set: nothing in that space is addressable.
type keySet struct {
	open bool
	keys []string
}

// openKeys is a producer-defined key space: any key is accepted.
func openKeys() keySet { return keySet{open: true} }

// closedKeys is a fixed key space.
func closedKeys(keys ...string) keySet { return keySet{keys: keys} }

func (k keySet) addressable() bool { return k.open || len(k.keys) > 0 }

func (k keySet) allows(key string) bool {
	return k.open || slices.Contains(k.keys, key)
}

// subtypeSpec describes the addressable surface of one subtype.
type subtypeSpec struct {
	scalar keySet
	item   keySet
}

// typeSpec describes the subtype space of one measurement Type. openSubtype
// means the subtype name itself is producer-defined, in which case fallback
// applies to every subtype of that Type.
//
// dottedSubtype marks a Type whose subtype names contain dots, which the
// {Type}.{Subtype}.{Key} grammar cannot otherwise disambiguate — see
// splitsOnLastDot.
type typeSpec struct {
	subtypes      map[string]subtypeSpec
	openSubtype   bool
	dottedSubtype bool
	fallback      subtypeSpec
}

// splitsOnLastDot reports whether a selector-free path of this Type separates
// subtype from key at the LAST dot rather than the first.
//
// The grammar is inherently ambiguous when both parts may contain dots:
// "SystemD.containerd.service.ActiveState" is either subtype "containerd" with
// key "service.ActiveState", or subtype "containerd.service" with key
// "ActiveState". Only the catalog knows which Types have dotted subtype names,
// so the split rule is resolved here rather than in the parser.
//
// Types without dotted subtypes keep the first-dot rule, which is what makes
// dotted KEYS work: OS.sysctl./proc/sys/kernel/osrelease and
// NodeTopology.label.nvidia.com/gpu.present.
func splitsOnLastDot(t Type) bool {
	return catalog[t].dottedSubtype
}

// catalog is deliberately unexported and read only through ValidatePath. An
// exported package-level map would be mutable by any importer, and this is an
// invariant.
var catalog = map[Type]typeSpec{
	TypeK8s: {
		subtypes: map[string]subtypeSpec{
			// pkg/collector/k8s/k8s.go
			"server": {scalar: closedKeys(KeyVersion, "platform", "goVersion")},
			// pkg/collector/k8s/node.go
			"node": {scalar: closedKeys(
				"source-node", "provider", "provider-id",
				"container-runtime-id", "container-runtime-name", "container-runtime-version",
				"kubelet-version", "kernel-version", "operating-system", "os-image",
			)},
			// pkg/collector/k8s/aksgpupools.go. gpu-driver is absent from a
			// cluster with no GPU pools; that is a runtime NotFound, not an
			// unaddressable path.
			"aks-gpu-pools": {scalar: closedKeys("gpu-pool-count", "gpu-pools", "gpu-driver")},
			// oke-addons is the OKE control-plane add-on projection supplied via
			// `aicr snapshot --oke-addons` / `aicr validate --oke-addons` —
			// pkg/collector/k8s/okeaddons.go. nvidia-gpu-plugin normalizes the
			// NvidiaGpuPlugin add-on state (installed/absent; any other
			// lifecycle state projects a fail-closed marker).
			"oke-addons": {scalar: closedKeys("addon-count", "nvidia-gpu-plugin")},
			// oke-legacy-plugin is in-cluster conflict evidence for OKE's legacy
			// addon-manager-shipped device plugin (invisible to list-addons) —
			// pkg/collector/k8s/okelegacyplugin.go. nvidia-gpu-device-plugin is
			// the collapsed constraint reading (none/active/unknown; only "none"
			// qualifies operator-managed); daemonset carries the uncollapsed
			// detail (absent/unlabeled/disabled/active/unknown).
			"oke-legacy-plugin": {scalar: closedKeys("nvidia-gpu-device-plugin", "daemonset")},
			// Keys are container image names (pkg/collector/k8s/image.go).
			"image": {scalar: openKeys()},
			// Keys are discovered cluster policies (pkg/collector/k8s/policy.go).
			"policy": {scalar: openKeys()},
			// pkg/collector/k8s/slinky.go. The scalar api-version is the
			// operator's; the item api-version is the resource's.
			"slinky-slurm": {
				scalar: closedKeys(
					"api-available", "api-version", "collection-state",
					"controller-count", "detected",
					"nodeset-count", "loginset-count", "restapi-count", "accounting-count",
				),
				item: closedKeys(
					"cluster-name", "external", "accounting-ref-present", "partition-enabled",
					"id", "kind", "namespace", "name", "api-version", "controller-id",
				),
			},
			// pkg/collector/k8s/mariadb.go
			"mariadb-operator": {scalar: closedKeys("collection-state", "api-available", "api-version")},
		},
	},

	TypeGPU: {
		subtypes: map[string]subtypeSpec{
			// pkg/collector/gpu/gpu.go
			"hardware": {scalar: closedKeys(
				KeyGPUPresent, KeyGPUCount, KeyGPUDriverLoaded, KeyGPUDetectionSource, KeyGPUModel,
			)},
		},
	},

	TypeOS: {
		subtypes: map[string]subtypeSpec{
			// /etc/os-release fields (pkg/collector/os/release.go) plus the
			// Talos projection (pkg/collector/talos/os.go).
			"release": {scalar: openKeys()},
			// sysctl paths, e.g. /proc/sys/kernel/osrelease.
			"sysctl": {scalar: openKeys()},
			// GRUB parameters (pkg/collector/os/grub.go).
			"grub": {scalar: openKeys()},
			// Kernel modules (pkg/collector/os/kmod.go).
			"kmod": {scalar: openKeys()},
			// Talos system extensions (pkg/collector/talos/os.go).
			"extensions": {scalar: openKeys()},
		},
	},

	TypeSystemD: {
		// The subtype IS the unit name (pkg/collector/systemd/systemd.go and
		// pkg/collector/talos/service.go both emit Subtype{Name: "<unit>"},
		// e.g. "containerd.service"), and the keys are D-Bus properties.
		//
		// Unit names carry a dot and D-Bus property names do not, so the
		// selector-free split for this Type happens at the LAST dot. Without
		// that, "SystemD.containerd.service.ActiveState" parsed as subtype
		// "containerd" with key "service.ActiveState" and could never resolve
		// against any emitted subtype.
		openSubtype:   true,
		dottedSubtype: true,
		fallback:      subtypeSpec{scalar: openKeys()},
	},

	TypeNodeTopology: {
		subtypes: map[string]subtypeSpec{
			// pkg/collector/topology/topology.go
			"summary": {scalar: closedKeys("node-count", "taint-count", "label-count")},
			// Keys are node label / taint keys.
			"label": {scalar: openKeys()},
			"taint": {scalar: openKeys()},
			// NOTE: no "gpu-nodes" entry. NodeTopology.gpu-nodes.label is a
			// synthesized node-set form that no producer emits; it is accepted
			// via virtualPaths before the catalog is consulted, so any OTHER
			// gpu-nodes path is correctly rejected.
		},
	},

	TypeNetworkTopology: {
		subtypes: map[string]subtypeSpec{
			// pkg/collector/network/translate.go. The five identity Context
			// keys (identifier, machineType, gpuType, linkType, nodeSelector)
			// are deliberately absent: Extract never reads Subtype.Context.
			"identity":     {scalar: closedKeys("pf-count", "rail-count")},
			"capabilities": {scalar: closedKeys("sriov", "rdma", "ib")},
			// Items only — an empty scalar space, so NetworkTopology.pfs.rail
			// is rejected with "subtype requires an item selector".
			"pfs": {item: closedKeys(
				"pciAddress", "deviceID", "psid", "partNumber", "rdmaDevice",
				"networkInterface", "model", "connectedGPU", "gpuProximity",
				"rail", "numaNode", "traffic",
			)},
			// Keys are storage.<i> / thirdParty.<i> with a varying index.
			"kernel-modules": {scalar: openKeys()},
		},
	},
}

// virtualPaths holds synthesized constraint paths that no producer emits and
// that therefore have no catalog row. They are accepted verbatim.
var virtualPaths = map[string]struct{}{
	PathGPUNodesLabel: {},
}

// maxSuggestionDistance bounds how far a nearest-match suggestion may be from
// the rejected token. Beyond this the "did you mean" is noise, not help.
const maxSuggestionDistance = 2

// ValidatePath reports whether name is addressable: whether a supported
// snapshot producer can emit it and a path form can name it.
//
// It says nothing about a particular snapshot. A path this accepts may still
// yield ErrCodeNotFound from Path.Extract when the reading is genuinely absent
// — the designed graceful-exclusion signal, deliberately left intact.
//
// It is the load-time typo gate for recipe constraint names (#1783): a
// grammatically valid but non-addressable path such as "K8s.server.versionn"
// would otherwise parse cleanly, evaluate as ErrCodeNotFound, and be treated by
// the resolver as "reading absent from this snapshot — exclude gracefully",
// silently skipping the gate the constraint exists to enforce.
//
// Returns nil when the path is addressable. Returns ErrCodeInvalidRequest for
// an authoring error, and ErrCodeInternal when a known measurement Type has no
// catalog entry — a catalog gap is an internal defect and must never read as
// "path accepted".
func ValidatePath(name string) error {
	return validatePathIn(catalog, name)
}

// validatePathIn is ValidatePath against an explicit catalog, so tests can
// exercise the missing-entry branch without mutating package state.
func validatePathIn(c map[Type]typeSpec, name string) error {
	if _, ok := virtualPaths[name]; ok {
		return nil
	}

	p, err := ParsePath(name)
	if err != nil {
		// Already ErrCodeInvalidRequest with path context; re-wrapping would
		// only bury it.
		return err
	}

	ts, ok := c[p.Type]
	if !ok {
		return aicrerrors.NewWithContext(aicrerrors.ErrCodeInternal,
			fmt.Sprintf("measurement type %q has no catalog entry", p.Type),
			map[string]any{keyType: p.Type, keyPath: name})
	}

	spec, err := lookupSubtype(ts, p, name)
	if err != nil {
		return err
	}

	if p.selector == nil {
		return validateScalarKey(spec, p, name)
	}
	return validateItemKey(spec, p, name)
}

// lookupSubtype resolves the subtype spec for a path, or explains why the
// subtype name is not addressable.
func lookupSubtype(ts typeSpec, p *Path, name string) (subtypeSpec, error) {
	if ts.openSubtype {
		return ts.fallback, nil
	}
	spec, ok := ts.subtypes[p.Subtype]
	if ok {
		return spec, nil
	}

	candidates := make([]string, 0, len(ts.subtypes))
	for sub := range ts.subtypes {
		candidates = append(candidates, sub)
	}
	ctx := map[string]any{keyType: p.Type, keySubtype: p.Subtype, keyPath: name}
	msg := fmt.Sprintf("unknown subtype %q for measurement type %q", p.Subtype, p.Type)
	if s, ok := suggest(p.Subtype, candidates); ok {
		ctx[keySuggestion] = s
		msg += fmt.Sprintf("; did you mean %q?", s)
	}
	return subtypeSpec{}, aicrerrors.NewWithContext(aicrerrors.ErrCodeInvalidRequest, msg, ctx)
}

// validateScalarKey checks a selector-free path against the subtype's
// Subtype.Data address space.
func validateScalarKey(spec subtypeSpec, p *Path, name string) error {
	if !spec.scalar.addressable() {
		msg := fmt.Sprintf("subtype %q of measurement type %q exposes no scalar readings", p.Subtype, p.Type)
		if spec.item.addressable() {
			msg += "; it requires an item selector, e.g. " +
				fmt.Sprintf("%s.%s[0].%s", p.Type, p.Subtype, p.Key)
		}
		return aicrerrors.NewWithContext(aicrerrors.ErrCodeInvalidRequest, msg,
			map[string]any{keyType: p.Type, keySubtype: p.Subtype, keyKey: p.Key, keyPath: name})
	}
	if spec.scalar.allows(p.Key) {
		return nil
	}

	ctx := map[string]any{keyType: p.Type, keySubtype: p.Subtype, keyKey: p.Key, keyPath: name}
	msg := fmt.Sprintf("unknown key %q in subtype %q of measurement type %q", p.Key, p.Subtype, p.Type)
	if s, ok := suggest(p.Key, spec.scalar.keys); ok {
		ctx[keySuggestion] = s
		msg += fmt.Sprintf("; did you mean %q?", s)
	}
	return aicrerrors.NewWithContext(aicrerrors.ErrCodeInvalidRequest, msg, ctx)
}

// validateItemKey checks a selector path against the subtype's ItemEntry
// address space, including the predicate key when one is present.
func validateItemKey(spec subtypeSpec, p *Path, name string) error {
	if !spec.item.addressable() {
		return aicrerrors.NewWithContext(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("subtype %q of measurement type %q has no items; an item selector is not valid here",
				p.Subtype, p.Type),
			map[string]any{keyType: p.Type, keySubtype: p.Subtype, keySelector: p.selector.Raw, keyPath: name})
	}

	// The predicate key is read from the same ItemEntry.Data/.Context space as
	// the result key (itemMatchesPredicate), so it validates against the same
	// set. The predicate VALUE is data, not schema, and is not checked.
	if pred := p.selector.Predicate; pred != nil && !spec.item.allows(pred.Key) {
		ctx := map[string]any{
			keyType: p.Type, keySubtype: p.Subtype,
			keyPredicateKey: pred.Key, keySelector: p.selector.Raw, keyPath: name,
		}
		msg := fmt.Sprintf("unknown item predicate key %q in subtype %q of measurement type %q",
			pred.Key, p.Subtype, p.Type)
		if s, ok := suggest(pred.Key, spec.item.keys); ok {
			ctx[keySuggestion] = s
			msg += fmt.Sprintf("; did you mean %q?", s)
		}
		return aicrerrors.NewWithContext(aicrerrors.ErrCodeInvalidRequest, msg, ctx)
	}

	// The index VALUE is not checked: bounds depend on the snapshot, and
	// out-of-range stays a legitimate runtime NotFound.

	if spec.item.allows(p.Key) {
		return nil
	}

	ctx := map[string]any{
		keyType: p.Type, keySubtype: p.Subtype,
		keyKey: p.Key, keySelector: p.selector.Raw, keyPath: name,
	}
	msg := fmt.Sprintf("unknown item key %q in subtype %q of measurement type %q", p.Key, p.Subtype, p.Type)
	if s, ok := suggest(p.Key, spec.item.keys); ok {
		ctx[keySuggestion] = s
		msg += fmt.Sprintf("; did you mean %q?", s)
	}
	return aicrerrors.NewWithContext(aicrerrors.ErrCodeInvalidRequest, msg, ctx)
}

// suggest returns the closest candidate to got within maxSuggestionDistance.
//
// Candidates are sorted before scanning and ties resolve to the lexically
// smaller name. Subtype candidates come from a Go map, so an unsorted scan
// would emit a different suggestion run to run — and the suggestion is part of
// an error string that tests and `--format json` consumers read.
func suggest(got string, candidates []string) (string, bool) {
	if got == "" || len(candidates) == 0 {
		return "", false
	}

	sorted := make([]string, len(candidates))
	copy(sorted, candidates)
	slices.Sort(sorted)

	best := ""
	bestDistance := maxSuggestionDistance + 1
	for _, candidate := range sorted {
		// Cheap reject: an edit distance of d requires |len difference| <= d.
		if abs(len(candidate)-len(got)) > maxSuggestionDistance {
			continue
		}
		if d := editDistance(got, candidate); d < bestDistance {
			best, bestDistance = candidate, d
		}
	}
	if bestDistance > maxSuggestionDistance {
		return "", false
	}
	return best, true
}

// editDistance computes the Levenshtein distance between a and b using a
// single rolling row.
func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}

	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}

	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, min(curr[j-1]+1, prev[j-1]+cost))
		}
		prev, curr = curr, prev
	}
	return prev[len(br)]
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
