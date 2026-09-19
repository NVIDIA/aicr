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

package upgrade

import (
	"sort"
	"strings"
)

// NotScannedOffline is why an artifact comparison carries no at-risk findings.
// It is the default a report is built with, so a caller that never asked for a
// scan cannot accidentally publish an all-clear.
const NotScannedOffline = "no cluster access requested"

// ResourceKind is one group and kind an upgrade's records name as affected,
// with the components whose crossed transitions named it. An empty Group is
// the core API group, as it is everywhere in Kubernetes.
//
// The components travel with the kind because the warning is meant to be acted
// on, and "something you are upgrading may delete these" is materially weaker
// guidance than naming whose transition puts them at risk: the operator
// deciding whether to proceed needs to know whether it is the component they
// care about. Several is unusual rather than wrong — two components can own
// overlapping kinds — and costs nothing, since the scan still lists a kind
// once however many records named it.
type ResourceKind struct {
	Group      string   `json:"group,omitempty" yaml:"group,omitempty"`
	Kind       string   `json:"kind" yaml:"kind"`
	Components []string `json:"components,omitempty" yaml:"components,omitempty"`
}

// AtRiskReport is the advisory scan's answer: what it looked at, and what it
// found that nothing appears to own.
//
// Scanned is carried rather than inferred from the other two fields, because
// they cannot answer it. A scan that examined every object and found none at
// risk and a run that never contacted a cluster both leave Findings empty, and
// those are opposite facts.
//
// It restates nothing from pkg/inventory, which performs the scan: this
// package must not import it, for the reason Component gives. The caller that
// holds both adapts.
type AtRiskReport struct {
	// Scanned reports a cluster having been examined. False is the offline
	// case and the failed-scan case alike, which Reason separates.
	Scanned bool `json:"scanned" yaml:"scanned"`

	// Reason says why no scan ran, and is empty exactly when Scanned.
	Reason string `json:"reason,omitempty" yaml:"reason,omitempty"`

	// Kinds is what the scan resolved and read, one entry per kind whatever
	// came of it. It is what separates "checked and clean" from "the kind is
	// not installed here", which an empty Findings alone conflates.
	Kinds []AtRiskKind `json:"kinds,omitempty" yaml:"kinds,omitempty"`

	// Findings is every object carrying no recognized ownership marker.
	Findings []AtRiskFinding `json:"findings,omitempty" yaml:"findings,omitempty"`
}

// AtRiskKind accounts for one kind the scan looked for.
type AtRiskKind struct {
	Group string `json:"group,omitempty" yaml:"group,omitempty"`
	Kind  string `json:"kind" yaml:"kind"`

	// Components is whose crossed records named this kind, sorted. See
	// ResourceKind, which carries it into the scan.
	Components []string `json:"components,omitempty" yaml:"components,omitempty"`

	// Present reports the cluster serving this kind. False means discovery
	// found no match, which is ordinary: the CRD a record names may simply not
	// be installed. Examined is then zero because nothing was listed, not
	// because nothing exists.
	Present bool `json:"present" yaml:"present"`

	// Examined is how many objects of this kind the scan read, at risk or not.
	// It is the denominator Findings is read against.
	Examined int `json:"examined" yaml:"examined"`
}

// AtRiskFinding is one object an upgrade could disturb and AICR does not own.
type AtRiskFinding struct {
	Group string `json:"group,omitempty" yaml:"group,omitempty"`
	Kind  string `json:"kind" yaml:"kind"`

	// Components is whose crossed records put this object at risk, sorted. It
	// is the actionable half of the finding: the object's identity says what
	// might be lost, and this says which upgrade would do it.
	Components []string `json:"components,omitempty" yaml:"components,omitempty"`

	// Namespace is empty for a cluster-scoped object.
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Name      string `json:"name" yaml:"name"`
}

// AffectedKinds is the union of the resource kinds named by the transitions
// results actually crossed, deduplicated and ordered by group then kind.
//
// Only crossed records contribute. An upgrade that is not being made cannot
// put anything at risk, so a component whose records describe boundaries this
// jump never reaches contributes nothing, however many resources those records
// name. ComponentResult.Crossed rather than Transition is what carries them,
// because the jumps most likely to destroy something are exactly the ones no
// single record describes.
//
// A kind naming nothing is dropped: an empty kind would resolve to no resource
// at best and to an unintended one at worst. A result with no component name
// contributes its kinds without attributing them, rather than being dropped:
// the objects are at risk either way, and a nameless row is a matcher bug that
// must not also silence the warning.
func AffectedKinds(results []ComponentResult) []ResourceKind {
	// Keyed on the comparable half only. Deduplication is per group and kind,
	// so the components accumulate into one entry rather than producing a
	// second List of the same resource.
	type key struct{ group, kind string }
	owners := make(map[key]map[string]struct{})
	for _, r := range results {
		for _, tr := range r.Crossed {
			if tr == nil {
				continue
			}
			for _, affected := range tr.AffectedResources {
				for _, kind := range affected.Kinds {
					kind = strings.TrimSpace(kind)
					if kind == "" {
						continue
					}
					k := key{group: strings.TrimSpace(affected.Group), kind: kind}
					if owners[k] == nil {
						owners[k] = make(map[string]struct{})
					}
					if r.Component != "" {
						owners[k][r.Component] = struct{}{}
					}
				}
			}
		}
	}
	if len(owners) == 0 {
		return nil
	}

	kinds := make([]ResourceKind, 0, len(owners))
	for k, components := range owners {
		kinds = append(kinds, ResourceKind{Group: k.group, Kind: k.kind, Components: sortedNames(components)})
	}
	// Sorted because the set was accumulated through a map, and an order that
	// changes run to run over identical inputs would move rows in the report.
	sort.Slice(kinds, func(i, j int) bool {
		if kinds[i].Group != kinds[j].Group {
			return kinds[i].Group < kinds[j].Group
		}

		return kinds[i].Kind < kinds[j].Kind
	})

	return kinds
}

// sortedNames flattens a name set, or nil when it is empty so the field stays
// absent rather than rendering as an empty list.
func sortedNames(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)

	return names
}

// clone deep-copies the report so it owns what it carries, matching the
// contract every other field of Report holds.
func (a *AtRiskReport) clone() AtRiskReport {
	out := AtRiskReport{Scanned: a.Scanned, Reason: a.Reason}
	if len(a.Kinds) > 0 {
		out.Kinds = make([]AtRiskKind, len(a.Kinds))
		for i, kind := range a.Kinds {
			// Copying the struct alone would leave the report sharing the
			// caller's Components backing array, which is the aliasing a
			// shallow deep-copy always misses.
			kind.Components = copyNames(kind.Components)
			out.Kinds[i] = kind
		}
	}
	if len(a.Findings) > 0 {
		out.Findings = make([]AtRiskFinding, len(a.Findings))
		for i, finding := range a.Findings {
			finding.Components = copyNames(finding.Components)
			out.Findings[i] = finding
		}
	}

	return out
}

func copyNames(names []string) []string {
	if len(names) == 0 {
		return nil
	}

	return append([]string(nil), names...)
}
