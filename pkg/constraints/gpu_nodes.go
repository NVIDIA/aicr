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

package constraints

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/validate/content"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/NVIDIA/aicr/pkg/collector/topology"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// GPUNodesLabelConstraintName is the node-set constraint form from issue
// #1755 (declared in the GKE overlays' readiness constraints; ADR-015's
// GKE gpuStack profile consumes it — #1761). Unlike scalar
// constraint paths, it does not name a reading the snapshot carries
// directly; the evaluator synthesizes the GPU-node set from the snapshot's
// NodeTopology.label readings and quantifies the value predicate over it.
//
// Value grammar (distinct from the scalar operator grammar):
//
//	key=value  every GPU node carries label key with exactly this value
//	           (an empty value — "key=" — is valid; Kubernetes permits it)
//	!key       no GPU node carries label key (any value)
//
// Both directions fail closed: on truncated node lists (snapshots taken
// with --max-nodes-per-entry), on an empty GPU-node universe, and on a
// value that parses as neither form.
//
// The literal lives in pkg/measurement so the constraint-path catalog can
// accept it as a virtual path (issue #1783) without importing this package.
const GPUNodesLabelConstraintName = measurement.PathGPUNodesLabel

// gpuNodeUniverseLabel defines the authoritative GPU-node universe: nodes
// carrying GKE's native accelerator label. It is present on GKE GPU nodes
// from pool creation — before the GPU Operator or NFD run — which is
// exactly the pre-deployment cluster this constraint form validates.
// (NFD's nvidia.com/gpu.* labels do not exist on such a cluster.)
const gpuNodeUniverseLabel = "cloud.google.com/gke-accelerator"

// maxReportedNodes caps the node names named in a failure message.
const maxReportedNodes = 5

// ctxValue / ctxConstraint key structured error context entries.
const (
	ctxValue      = "value"
	ctxConstraint = "constraint"
	ctxReading    = "reading"
	keyKey        = "key"
)

// labelNodeSet is one decoded NodeTopology.label entry: the label value,
// the set of nodes carrying it, the raw reading key it came from, and
// whether its node list was truncated by the collector.
type labelNodeSet struct {
	value     string
	nodes     []string
	raw       string
	truncated bool
}

// evaluateGPUNodesLabel evaluates the node-set constraint form against the
// snapshot's NodeTopology.label readings. See GPUNodesLabelConstraintName
// for the grammar and fail-closed semantics.
func evaluateGPUNodesLabel(value string, snap *snapshotter.Snapshot) EvalResult {
	key, want, negated, err := parseGPUNodesLabelValue(value)
	if err != nil {
		return EvalResult{Error: err}
	}

	labels := findLabelSubtype(snap)
	if labels == nil {
		return EvalResult{Error: errors.New(errors.ErrCodeNotFound,
			"snapshot carries no NodeTopology label readings — re-capture with a current aicr build and verify the snapshot agent can list nodes")}
	}

	universeEntries, err := decodeLabelEntries(labels, gpuNodeUniverseLabel)
	if err != nil {
		return EvalResult{Error: err}
	}
	universe := make(map[string]bool)
	for _, e := range universeEntries {
		for _, n := range e.nodes {
			universe[n] = true
		}
	}
	if len(universe) == 0 {
		// An empty universe must fail closed, never satisfy either predicate
		// vacuously (#1755 acceptance requirement 2).
		return EvalResult{Error: errors.NewWithContext(errors.ErrCodeNotFound,
			fmt.Sprintf("GPU-node universe is empty: snapshot has no %q label readings — "+
				"the constraint cannot be evaluated on a cluster without identifiable GPU nodes", gpuNodeUniverseLabel),
			map[string]any{ctxConstraint: GPUNodesLabelConstraintName})}
	}

	targetEntries, err := decodeLabelEntries(labels, key)
	if err != nil {
		return EvalResult{Error: err}
	}

	if negated {
		return evaluateNoGPUNodeHasKey(key, universe, targetEntries)
	}
	return evaluateEveryGPUNodeHasValue(key, want, universe, targetEntries)
}

// ValidateGPUNodesLabelValue checks a node-set constraint value against the
// grammar documented on GPUNodesLabelConstraintName ("key=value" or "!key"),
// snapshot-free. It exists for hermetic structural checks (the recipe-health
// constraints_wellformed dimension) that must mirror Evaluate's dispatch:
// the node-set form is deliberately NOT valid under the scalar
// ParseConstraintExpression grammar, so grading it with the scalar parser
// would fail every recipe carrying a well-formed node-set constraint.
func ValidateGPUNodesLabelValue(raw string) error {
	_, _, _, err := parseGPUNodesLabelValue(raw) //nolint:dogsled // validation-only wrapper; the parsed fields are the evaluator's concern
	return err
}

// parseGPUNodesLabelValue parses the node-set value grammar: "key=value"
// (positive) or "!key" (negated). Anything else is rejected — in particular
// the scalar operator grammar (">= x", "!= x") is not valid here. Keys and
// values are validated with Kubernetes's own label validators: a key no
// node can legally carry (e.g. a double slash) would otherwise make the
// negated predicate pass vacuously — the fail-open direction.
func parseGPUNodesLabelValue(raw string) (key, want string, negated bool, err error) {
	v := strings.TrimSpace(raw)
	if after, ok := strings.CutPrefix(v, "!"); ok {
		key = strings.TrimSpace(after)
		if errs := content.IsLabelKey(key); len(errs) > 0 {
			return "", "", false, errors.NewWithContext(errors.ErrCodeInvalidRequest,
				"invalid node-set constraint value: negated form is \"!<label-key>\" with a valid label key and no value",
				map[string]any{ctxValue: raw, "key_errors": strings.Join(errs, "; ")})
		}
		return key, "", true, nil
	}
	// "key=" (empty value) is deliberately valid: Kubernetes permits empty
	// label values and the collector encodes them ("|<nodes>"; the
	// disambiguated map key "<key>." cannot collide with a real label key,
	// which may not end in a dot).
	key, want, ok := strings.Cut(v, "=")
	if !ok {
		return "", "", false, errors.NewWithContext(errors.ErrCodeInvalidRequest,
			"invalid node-set constraint value: expected \"<label-key>=<value>\" or \"!<label-key>\"",
			map[string]any{ctxValue: raw})
	}
	if errs := append(content.IsLabelKey(key), content.IsLabelValue(want)...); len(errs) > 0 {
		return "", "", false, errors.NewWithContext(errors.ErrCodeInvalidRequest,
			"invalid node-set constraint value: label key or value is not valid",
			map[string]any{ctxValue: raw, "errors": strings.Join(errs, "; ")})
	}
	return key, want, false, nil
}

// findLabelSubtype returns the NodeTopology label subtype, or nil when the
// snapshot does not carry it.
func findLabelSubtype(snap *snapshotter.Snapshot) *measurement.Subtype {
	if snap == nil {
		return nil
	}
	for _, m := range snap.Measurements {
		if m == nil || m.Type != measurement.TypeNodeTopology {
			continue
		}
		return m.GetSubtype("label")
	}
	return nil
}

// decodeLabelEntries collects the decoded entries for one label key. The item
// encoding keeps key and value apart, so the impostor and collision reasoning
// in decodeLabelEntriesFromData cannot arise there; older Data-only snapshots
// still need it in full.
//
// Both paths reject truncated node lists and non-canonical node names
// (#1755) — the item encoding removes ambiguity in the key, not the
// possibility of a corrupted list.
func decodeLabelEntries(labels *measurement.Subtype, key string) ([]labelNodeSet, error) {
	if topology.HasLosslessReadings(labels) {
		return decodeLabelEntriesFromItems(labels, key)
	}
	return decodeLabelEntriesFromData(labels.Data, key)
}

// decodeLabelEntriesFromItems matches the key exactly — nothing to
// disambiguate, no impostor to reject.
func decodeLabelEntriesFromItems(labels *measurement.Subtype, key string) ([]labelNodeSet, error) {
	readings, err := topology.LabelReadings(labels)
	if err != nil {
		return nil, errors.WrapWithContext(errors.ErrCodeInvalidRequest,
			"failed to decode NodeTopology label readings", err,
			map[string]any{ctxConstraint: GPUNodesLabelConstraintName, keyKey: key})
	}

	var entries []labelNodeSet
	for _, r := range readings {
		if r.Key != key {
			continue
		}
		// Rejected outright rather than validated: a partial node list cannot
		// support a node-set predicate, and its "(+N more)" tail is not a node
		// name anyway.
		if r.Truncated {
			return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("label reading %q is truncated — node-set constraints cannot be evaluated on a partial "+
					"node list; regenerate the snapshot without --max-nodes-per-entry", r.RawKey),
				map[string]any{ctxConstraint: GPUNodesLabelConstraintName, ctxReading: r.RawKey})
		}
		if !validNodeNames(r.Nodes) {
			return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("label reading %q is malformed (nodes %v) — expected canonical node names; "+
					"regenerate the snapshot with a current aicr build", r.RawKey, r.Nodes),
				map[string]any{ctxConstraint: GPUNodesLabelConstraintName, ctxReading: r.RawKey})
		}
		entries = append(entries, labelNodeSet{
			value:     r.Value,
			nodes:     r.Nodes,
			raw:       r.RawKey,
			truncated: false,
		})
	}

	// A node carries exactly one value of a given label key, so the entries
	// must partition their nodes. Overlap means the snapshot records no real
	// cluster, and accepting it lets one node satisfy key=a and key=b at once
	// because evaluateEveryGPUNodeHasValue skips the non-matching entry.
	// Mirrors the folded encoding's guard in decodeLabelEntriesFromData.
	//
	// Compared on value, not mere reappearance: a node repeated under the same
	// value is redundant rather than contradictory, and the Data path accepts
	// that shape.
	seen := make(map[string]string, len(entries))
	for _, e := range entries {
		for _, n := range e.nodes {
			if prev, dup := seen[n]; dup && prev != e.value {
				return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("label readings for %q are inconsistent: node %q appears under both value %q and "+
						"value %q, but a node carries exactly one value per label key — the snapshot is corrupt "+
						"or hand-edited; regenerate it with a current aicr build", key, n, prev, e.value),
					map[string]any{ctxConstraint: GPUNodesLabelConstraintName, keyKey: key, "node": n})
			}
			seen[n] = e.value
		}
	}
	return entries, nil
}

// decodeLabelEntriesFromData decodes the legacy folded encoding
// ("<value>|<node1,node2,...>"). It handles both encoded shapes: the plain
// key, and the "<key>.<value>" disambiguation encodeLabels applies when the
// key carries multiple distinct values across the cluster.
//
// The encoding is lossy: a *different* label key literally named
// "<key>.<v>" whose value is "<v>" produces the same map entry as a genuine
// disambiguated reading of <key>. Counting such an entry would let e.g.
// "<key>.true=true" satisfy "<key>=true" — fail-open. encodeLabels gives us
// two invariants to reject impostors with: a disambiguated key never
// coexists with its plain form, and genuine disambiguation always yields at
// least two entries (it happens only when the key carries ≥2 distinct
// values). Prefixed matches are therefore accepted only when the plain key
// is absent AND two or more prefixed entries match. Exactly ONE matching
// prefixed entry with the plain key absent is an ambiguous shape and fails
// closed: it is either a distinct dotted label (harmless) or the surviving
// remnant of a collision that overwrote the key's other disambiguated
// entries — and treating it as the former lets the negated predicate pass
// while the key is genuinely present on GPU nodes.
//
// An accepted set is additionally required to partition its nodes: one node
// carries exactly one value of one label, so overlapping node sets prove the
// map entries collided — encodeLabels writes a genuine disambiguated entry
// and a distinct label named "<key>.<v>" to the same map key, one silently
// overwriting the other by map iteration order. When the distinct label
// wins, its node list can cover nodes whose genuine value differs, which
// would pass the positive predicate on a mixed cluster — fail-open. Overlap
// therefore fails closed as an ambiguous reading. The residual ambiguity
// (colliding entries with identical node sets, ≥2 distinct dotted labels
// forming a clean partition with the plain key absent, or every
// disambiguated entry overwritten by value≠suffix distinct labels) is what
// the lossy encoding cannot express; the durable fix is a lossless
// collector encoding (#2003).
//
// Any accepted entry whose node list is truncated fails closed (a partial
// membership list can falsely satisfy both predicate directions — #1755
// acceptance requirement 1), and a structurally malformed reading is
// rejected rather than decoded: no "|" separator, more than one "|", an
// empty node list, or a node token that is not a canonical Kubernetes node
// name (RFC 1123 subdomain — rejects whitespace, empties, and embedded
// separators). A malformed token would otherwise never equal a real
// universe member, letting the negated predicate pass vacuously. Truncated
// readings skip token validation — their "(+N more)" tail is not a node
// name by design — and keep their distinct truncation diagnostic.
func decodeLabelEntriesFromData(data map[string]measurement.Reading, key string) ([]labelNodeSet, error) {
	prefix := key + "."
	var plain, prefixed []labelNodeSet
	for k, reading := range data {
		suffix, hasPrefix := strings.CutPrefix(k, prefix)
		if k != key && !hasPrefix {
			continue
		}
		raw := reading.String()
		value, nodesRaw, wellFormed := cutLabelEncoding(raw)
		if k != key && value != suffix {
			continue // different label key sharing the dotted prefix
		}
		truncated := topology.IsTruncatedNodeList(nodesRaw)
		var nodes []string
		ok := wellFormed && strings.Count(raw, "|") == 1
		if ok && !truncated {
			nodes, ok = splitNodes(nodesRaw)
		}
		if !ok {
			return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("label reading %q is malformed (%q) — expected \"<value>|<node1,node2,...>\" with "+
					"canonical node names; regenerate the snapshot with a current aicr build", k, raw),
				map[string]any{ctxConstraint: GPUNodesLabelConstraintName, ctxReading: k})
		}
		entry := labelNodeSet{value: value, nodes: nodes, raw: k, truncated: truncated}
		if k == key {
			plain = append(plain, entry)
		} else {
			prefixed = append(prefixed, entry)
		}
	}

	entries := plain
	if len(plain) == 0 && len(prefixed) == 1 {
		return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("label readings for %q are ambiguous: a single disambiguated-shape entry %q exists without "+
				"the plain key — either a distinct label sharing the dotted name, or the remnant of an encoding "+
				"collision that overwrote the key's other entries (#2003); rename the conflicting label or use a "+
				"lossless snapshot encoding", key, prefixed[0].raw),
			map[string]any{ctxConstraint: GPUNodesLabelConstraintName, keyKey: key, ctxReading: prefixed[0].raw})
	}
	if len(plain) == 0 && len(prefixed) >= 2 {
		seen := make(map[string]bool)
		for _, e := range prefixed {
			for _, n := range e.nodes {
				if seen[n] {
					return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
						fmt.Sprintf("label readings for %q are ambiguous: node %q appears under two values, so a "+
							"distinct dotted label collided with the disambiguated encoding (reading %q is one of "+
							"the colliding parties); rename the conflicting label or use a lossless snapshot "+
							"encoding", key, n, e.raw),
						map[string]any{ctxConstraint: GPUNodesLabelConstraintName, keyKey: key, "node": n})
				}
				seen[n] = true
			}
		}
		entries = prefixed
	}
	for _, e := range entries {
		if e.truncated {
			return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("label reading %q is truncated — node-set constraints cannot be evaluated on a partial "+
					"node list; regenerate the snapshot without --max-nodes-per-entry", e.raw),
				map[string]any{ctxConstraint: GPUNodesLabelConstraintName, ctxReading: e.raw})
		}
	}
	return entries, nil
}

// splitNodes splits the comma-joined node list. It reports ok=false for an
// empty list or any member that is not a canonical Kubernetes node name
// (RFC 1123 subdomain) — shapes the collector never emits for a present
// label. A non-canonical token (embedded "|", surrounding whitespace,
// empty) can never equal a real universe member, so decoding it would
// vacuously pass the negated predicate.
func splitNodes(nodesRaw string) ([]string, bool) {
	if nodesRaw == "" {
		return nil, false
	}
	nodes := strings.Split(nodesRaw, ",")
	if !validNodeNames(nodes) {
		return nil, false
	}
	return nodes, true
}

// validNodeNames reports whether every token is a canonical Kubernetes node
// name. Shared by both decode paths so the fail-closed rule cannot drift.
func validNodeNames(nodes []string) bool {
	if len(nodes) == 0 {
		return false
	}
	for _, n := range nodes {
		if len(validation.IsDNS1123Subdomain(n)) > 0 {
			return false
		}
	}
	return true
}

// cutLabelEncoding splits the topology collector's "<value>|<nodes>"
// encoding at the first pipe. Kubernetes label values cannot contain "|",
// so the first pipe is always the separator; a reading without one is
// malformed (ok=false).
func cutLabelEncoding(raw string) (value, nodes string, ok bool) {
	return strings.Cut(raw, "|")
}

// evaluateEveryGPUNodeHasValue passes when every node in the GPU universe
// carries the target label with exactly the wanted value. A node carrying a
// different value, or not carrying the label at all, fails.
func evaluateEveryGPUNodeHasValue(key, want string, universe map[string]bool, entries []labelNodeSet) EvalResult {
	matched := make(map[string]bool)
	for _, e := range entries {
		if e.value != want {
			continue
		}
		for _, n := range e.nodes {
			matched[n] = true
		}
	}

	var missing []string
	for n := range universe {
		if !matched[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) == 0 {
		return EvalResult{
			Passed: true,
			Actual: fmt.Sprintf("all %d GPU node(s) carry %s=%s", len(universe), key, want),
		}
	}
	sort.Strings(missing)
	return EvalResult{
		Actual: fmt.Sprintf("%d of %d GPU node(s) missing %s=%s: %s",
			len(missing), len(universe), key, want, summarizeNodes(missing)),
	}
}

// evaluateNoGPUNodeHasKey passes when no node in the GPU universe carries
// the target label key with any value.
func evaluateNoGPUNodeHasKey(key string, universe map[string]bool, entries []labelNodeSet) EvalResult {
	offenders := make(map[string]bool)
	for _, e := range entries {
		for _, n := range e.nodes {
			if universe[n] {
				offenders[n] = true
			}
		}
	}
	if len(offenders) == 0 {
		return EvalResult{
			Passed: true,
			Actual: fmt.Sprintf("none of %d GPU node(s) carry label %s", len(universe), key),
		}
	}
	names := make([]string, 0, len(offenders))
	for n := range offenders {
		names = append(names, n)
	}
	sort.Strings(names)
	return EvalResult{
		Actual: fmt.Sprintf("%d of %d GPU node(s) carry label %s: %s",
			len(names), len(universe), key, summarizeNodes(names)),
	}
}

// summarizeNodes renders a sorted node list capped at maxReportedNodes.
func summarizeNodes(nodes []string) string {
	if len(nodes) <= maxReportedNodes {
		return strings.Join(nodes, ",")
	}
	return strings.Join(nodes[:maxReportedNodes], ",") +
		fmt.Sprintf(" (+%d more)", len(nodes)-maxReportedNodes)
}
