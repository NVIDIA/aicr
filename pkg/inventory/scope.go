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

package inventory

import (
	"slices"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// nameSeparator joins the parts of every name this package matches on. Both
// record names are DNS-1123 labels built by concatenating identifiers with it:
// helm-controller's "<targetNamespace>-<name>", the bundle writer's
// "<component>-<phase>", and Argo's namePrefix.
const nameSeparator = "-"

// injectedFolderPhases are what the bundle writer appends to a component's
// name for an auxiliary folder ("<component>-<phase>", see
// pkg/bundler/deployer/localformat). The writer is this project's own, so a
// name in that shape is as certainly a component's as the bare name is: these
// define the confident tier, not the whole set of transforms a record name can
// have undergone.
var injectedFolderPhases = []string{"pre", "post", "readiness"}

// confidence is how sure a scope is that a record belongs to a component the
// read answers for. It is what decides whether a record that cannot be read
// fails the run or is merely counted, so the tiers are defined by how certain
// the name makes the attribution: confident when the name is one this project
// itself would have written, possible when a deployer's own naming could have
// produced it from a component name, and outOfScope when no component's name
// appears in it at all.
type confidence int

const (
	outOfScope confidence = iota
	possible
	confident
)

// scope is the set of component names an inventory read answers for.
//
// It is what keeps a cluster's unrelated workloads out of the answer. Both
// readers list cluster-wide, so they see every Helm release and every Argo CD
// Application in the cluster, most of which belong to no component this
// project knows: another team's Application with no spec.destination.namespace
// is well-formed for its own purposes, and a release record in a format this
// build cannot read is only a problem if something reads it. Validating those
// strictly would let one foreign record fail a run it has nothing to do with.
//
// The governing rule is that a record which cannot affect the comparison
// cannot fail it either. Scope decides that, and it is decided before any
// validation rather than after, so an out-of-scope record costs nothing and is
// never parsed, decoded or type-checked.
//
// The caller supplies the set because only it knows which components are being
// compared; the readers deliberately do not reach for the registry themselves.
type scope map[string]struct{}

// newScope builds a scope from component names. An empty name is dropped: it
// would match on any record whose name contains an empty token, and it can
// only have arrived by accident.
func newScope(components ...string) scope {
	within := make(scope, len(components))
	for _, component := range components {
		if component == "" {
			continue
		}
		within[component] = struct{}{}
	}

	return within
}

// validate rejects a scope that answers for nothing.
//
// Such a read is not an empty cluster, it is an unanswerable question, and the
// two are indistinguishable in the result: an empty inventory says every
// component is being installed for the first time. A caller that forgot to
// pass its component set would get that verdict with no error to show for it,
// which is the failure this whole command exists to prevent.
func (s scope) validate() error {
	if len(s) == 0 {
		return errors.New(errors.ErrCodeInvalidRequest,
			"the installed inventory was requested for no components, so it could only report that nothing is "+
				"installed; pass the names of the components being compared")
	}

	return nil
}

// covers reports how sure this scope is that a record belongs to a component
// it answers for.
//
// A component's name is not always the name the cluster stores it under. Only
// the helm and helmfile deployers install under the component's own name;
// helm-controller composes a Flux release as "<targetNamespace>-<name>", so
// gpu-operator is stored as gpu-operator-gpu-operator, and Argo CD prepends a
// user-settable namePrefix to every child Application. The bundle writer's own
// injected folders append a phase. No single rule recognizes all of those, and
// this package does not know which deployer ran.
//
// So the match is loose, and the tier says how loose. A name this project
// itself would have written is confident. A name some deployer's naming could
// have produced from a component name is possible, under two anchored rules:
//
//   - a token run, where the component's hyphen-separated tokens appear
//     contiguously in the record's, which catches gpu-operator-gpu-operator
//     and tenant-a-gpu-operator; and
//   - a raw suffix, where the record name simply ends with the component name,
//     which catches tenantgpu-operator from a namePrefix that does not end in
//     a separator, a shape the token rule cannot see because the prefix fuses
//     with the component's first token.
//
// Both rules are anchored deliberately. Bare strings.Contains would be looser
// than either and much harder to reason about: eight registry components are a
// single token, so it would draw in arbitrary unrelated records on a
// coincidental substring.
//
// Looseness is safe here only because strictness follows the tier. A possible
// record that cannot be read is skipped and counted rather than fatal, so
// widening this predicate cannot make a run fail. What it can do is draw in a
// foreign record that reads perfectly well: the raw suffix rule anchors at the
// end of the string rather than at a token boundary, so an unrelated workload
// whose name happens to end in a component's is reported as installed, and
// only the comparison can tell. That is the cost, and it is the survivable
// one. Under-matching has no such safety valve: a component whose record is
// never seen reports as newly installed.
//
// Deciding which component a record actually belongs to is the comparison's
// job, not this one's. It has the recipe and the deployer; this has neither.
func (s scope) covers(name string) confidence {
	tokens := strings.Split(name, nameSeparator)
	best := outOfScope
	for component := range s {
		if name == component || isInjectedFolder(name, component) {
			return confident
		}
		tokenRun := containsRun(tokens, strings.Split(component, nameSeparator))
		if tokenRun || strings.HasSuffix(name, component) || endsWithInjectedFolder(name, component) {
			best = possible
		}
	}

	return best
}

// isInjectedFolder reports whether name is component's auxiliary folder.
func isInjectedFolder(name, component string) bool {
	for _, phase := range injectedFolderPhases {
		if name == component+nameSeparator+phase {
			return true
		}
	}

	return false
}

// endsWithInjectedFolder reports whether name ends with component's auxiliary
// folder name.
//
// The two possible rules do not compose, which is why this third one exists.
// Argo CD prepends its namePrefix to the folder name the bundle writer already
// produced, so a prefix that does not end in a separator yields
// tenantgpu-operator-pre: the raw suffix rule cannot see it because the tail is
// the phase rather than the component, and the token rule cannot because the
// prefix fuses with the component's first token. Nothing false is produced by
// missing it today, since injected-folder Applications are path-based and
// carry no chart, but a component dropping out of scope is the direction with
// no safety valve.
func endsWithInjectedFolder(name, component string) bool {
	for _, phase := range injectedFolderPhases {
		if strings.HasSuffix(name, component+nameSeparator+phase) {
			return true
		}
	}

	return false
}

// containsRun reports whether want appears in tokens as a contiguous run.
func containsRun(tokens, want []string) bool {
	for start := 0; start+len(want) <= len(tokens); start++ {
		if slices.Equal(tokens[start:start+len(want)], want) {
			return true
		}
	}

	return false
}
