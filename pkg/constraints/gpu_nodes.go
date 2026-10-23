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

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/NVIDIA/aicr/pkg/collector/topology"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/fingerprint"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// GPUNodesLabelConstraintName names the node-set constraint form, which the
// gke-cos overlay's ADR-015 gpuStack profiles declare. Unlike a scalar
// constraint path, it names no reading in the snapshot. Its value is a
// Kubernetes label selector, such as "key=value", "!key", or
// "key in (a,b),other", and it passes when every GPU node matches it. GPU
// nodes are identified from NodeTopology.label readings by a label that
// depends on the service and on whether Evaluate or EvaluateReadiness is
// called.
//
// Evaluation fails closed on:
//   - an empty selector
//   - a truncated node list, from a snapshot taken with --max-nodes-per-entry
//   - an empty GPU-node universe
//
// The literal lives in pkg/measurement so the constraint-path catalog can
// accept it as a virtual path without importing this package.
const GPUNodesLabelConstraintName = measurement.PathGPUNodesLabel

// gpuNodeUniverseLabels defines the pre-Operator, pre-NFD GPU-node universe
// per service. A label listed here must be present on GPU nodes from pool
// creation, before the GPU Operator or NFD run — the property that makes this
// constraint form usable at pre-deployment. NFD's nvidia.com/gpu.* labels do
// not exist on such a cluster, so they cannot serve as the universe.
//
// Only GKE is populated today. Other services (AKS, EKS, OKE, LKE, …) have no
// current consumer of NodeTopology.gpu-nodes.label — AKS routes through the
// --aks-gpu-pools snapshot projection, OKE's disablement path is not yet
// settled (NKX-9804), and EKS has no consumer — so their entries are added by
// the profile PR that first needs one. A service with no entry fails closed
// at generation with an error naming the service.
var gpuNodeUniverseLabels = map[recipe.CriteriaServiceType]*labels.Requirement{
	recipe.CriteriaServiceGKE: mustRequirement("cloud.google.com/gke-accelerator", selection.Exists),
}

// readinessGPUNodeUniverse is the readiness universe for a service that is
// missing from gpuNodeUniverseLabels or unresolvable from the snapshot. A GPU
// node the GPU Operator has not labeled yet is outside it.
var readinessGPUNodeUniverse = mustRequirement(defaults.GPUPresentNodeLabelKey, selection.Equals, "true")

// mustRequirement is labels.NewRequirement for package-level literals. It
// panics if the key, operator, or values are invalid.
func mustRequirement(key string, op selection.Operator, vals ...string) *labels.Requirement {
	r, err := labels.NewRequirement(key, op, vals)
	if err != nil {
		panic(err)
	}
	return r
}

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
func evaluateGPUNodesLabel(value string, snap *snapshotter.Snapshot, phase evalPhase) EvalResult {
	selector, err := parseGPUNodesLabelValue(value)
	if err != nil {
		return EvalResult{Error: err}
	}

	labelSubtype := findLabelSubtype(snap)
	if labelSubtype == nil {
		return EvalResult{Error: errors.New(errors.ErrCodeNotFound,
			"snapshot carries no NodeTopology label readings — re-capture with a current aicr build and verify the snapshot agent can list nodes")}
	}

	// Ordering is load-bearing: the resolution runs AFTER findLabelSubtype so
	// snapshots that carry no label subtype at all keep their existing
	// ErrCodeNotFound diagnostic (nil snapshot, no NodeTopology, no label
	// subtype) — the ordering pkg/recipe's fail-closed handling relies on.
	universeReq, err := gpuNodeUniverseFor(snap, phase)
	if err != nil {
		return EvalResult{Error: err}
	}

	universeEntries, err := decodeLabelEntries(labelSubtype, universeReq.Key())
	if err != nil {
		return EvalResult{Error: err}
	}
	universe := make(map[string]bool)
	for _, e := range universeEntries {
		if !universeReq.Matches(labels.Set{universeReq.Key(): e.value}) {
			continue
		}
		for _, n := range e.nodes {
			universe[n] = true
		}
	}
	if len(universe) == 0 {
		// Every selector passes vacuously on an empty universe.
		msg := fmt.Sprintf("GPU-node universe is empty. No node in the snapshot carries %s, so the "+
			"constraint cannot be evaluated on a cluster without identifiable GPU nodes", universeReq)
		if universeReq == readinessGPUNodeUniverse {
			msg += ". The GPU Operator sets this label once it is running, " +
				"so capture the snapshot after the bundle is deployed"
		}
		return EvalResult{Error: errors.NewWithContext(errors.ErrCodeNotFound, msg,
			map[string]any{ctxConstraint: GPUNodesLabelConstraintName, "universe": universeReq.String()})}
	}

	reqs, _ := selector.Requirements()
	nodeLabels := make(map[string]labels.Set, len(universe))
	for _, r := range reqs {
		entries, err := decodeLabelEntries(labelSubtype, r.Key())
		if err != nil {
			return EvalResult{Error: err}
		}
		for _, e := range entries {
			for _, n := range e.nodes {
				if !universe[n] {
					continue
				}
				if nodeLabels[n] == nil {
					nodeLabels[n] = labels.Set{}
				}
				nodeLabels[n][r.Key()] = e.value
			}
		}
	}

	var failing []string
	for n := range universe {
		if !selector.Matches(nodeLabels[n]) {
			failing = append(failing, n)
		}
	}
	if len(failing) == 0 {
		return EvalResult{
			Passed: true,
			Actual: fmt.Sprintf("all %d GPU node(s) match %s", len(universe), selector),
		}
	}
	sort.Strings(failing)
	return EvalResult{
		Actual: fmt.Sprintf("%d of %d GPU node(s) do not match %s: %s",
			len(failing), len(universe), selector, summarizeNodes(failing)),
	}
}

// gpuNodeUniverseFor returns the GPU-node universe for snap's service in
// phase:
//
//   - A service in gpuNodeUniverseLabels gets its entry in every phase.
//   - Any other service gets readinessGPUNodeUniverse at readiness.
//   - At generation, any other service gets an ErrCodeNotFound error naming
//     the service, or the missing k8s.node.provider when snap resolves none.
func gpuNodeUniverseFor(snap *snapshotter.Snapshot, phase evalPhase) (*labels.Requirement, error) {
	// The recipe layer resolves the service the same way, so both agree on it.
	fp := fingerprint.FromMeasurements(snap.Measurements)
	service := recipe.CriteriaServiceType(fp.Service.Value)
	if u, ok := gpuNodeUniverseLabels[service]; ok {
		return u, nil
	}
	if phase == phaseReadiness {
		return readinessGPUNodeUniverse, nil
	}
	if service == "" {
		return nil, errors.NewWithContext(errors.ErrCodeNotFound,
			"snapshot has no resolvable k8s.node.provider. The NodeTopology.gpu-nodes.label "+
				"constraint form needs a known service to select its pre-deployment GPU-node "+
				"universe label at recipe generation. Re-capture the snapshot with a current aicr build, "+
				"or declare the constraint under validation.readiness.constraints",
			map[string]any{ctxConstraint: GPUNodesLabelConstraintName})
	}
	return nil, errors.NewWithContext(errors.ErrCodeNotFound,
		fmt.Sprintf("no pre-deployment GPU-node universe label is declared for service %q, so the "+
			"NodeTopology.gpu-nodes.label constraint form cannot be evaluated at recipe generation. "+
			"Declare it under validation.readiness.constraints, where GPU nodes are identified by %s, "+
			"or add an entry to gpuNodeUniverseLabels in pkg/constraints/gpu_nodes.go",
			service, readinessGPUNodeUniverse),
		map[string]any{ctxConstraint: GPUNodesLabelConstraintName, "service": string(service)})
}

// ValidateGPUNodesLabelValue returns an ErrCodeInvalidRequest error if raw is
// not a valid node-set constraint value. It needs no snapshot. Use it instead
// of ParseConstraintExpression, whose scalar grammar rejects every node-set
// value.
func ValidateGPUNodesLabelValue(raw string) error {
	_, err := parseGPUNodesLabelValue(raw)
	return err
}

// parseGPUNodesLabelValue parses raw as a Kubernetes label selector. It
// rejects an empty selector and any key or value that is not a valid label.
func parseGPUNodesLabelValue(raw string) (labels.Selector, error) {
	// labels.Parse validates keys, so a key no node can carry cannot make a
	// negative requirement pass vacuously.
	selector, err := labels.Parse(raw)
	if err != nil {
		return nil, errors.WrapWithContext(errors.ErrCodeInvalidRequest,
			"invalid node-set constraint value: expected a Kubernetes label selector", err,
			map[string]any{ctxValue: raw})
	}
	if selector.Empty() {
		return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
			"invalid node-set constraint value: the label selector is empty and would match every node",
			map[string]any{ctxValue: raw})
	}
	return selector, nil
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
	// cluster, and the node would get whichever value was read last.
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

// summarizeNodes renders a sorted node list capped at maxReportedNodes.
func summarizeNodes(nodes []string) string {
	if len(nodes) <= maxReportedNodes {
		return strings.Join(nodes, ",")
	}
	return strings.Join(nodes[:maxReportedNodes], ",") +
		fmt.Sprintf(" (+%d more)", len(nodes)-maxReportedNodes)
}
