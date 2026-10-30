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
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/Masterminds/semver/v3"
	"k8s.io/apimachinery/pkg/util/sets"
)

// ChangeKind says what a component did between the two tables.
//
// ChangeAdded and ChangeRemoved carry no Verdict: the verdict vocabulary
// describes a transition, and a component that merely arrived or departed did
// not make one. A departing component stays installed, because AICR dropping it
// from a recipe is a statement about what AICR now ships rather than an
// instruction to tear down a running workload.
//
// ChangeIdentity is the kind a version comparison cannot produce at all: the
// component held its version and moved anyway. It is its own kind rather than a
// flag on ChangeVersion because the remedy has nothing to do with a version
// bump, and a report that showed it as one would send the reader to the
// component's release notes for an answer they do not contain.
type ChangeKind string

const (
	ChangeVersion  ChangeKind = "version"
	ChangeIdentity ChangeKind = "identity"
	ChangeAdded    ChangeKind = "added"
	ChangeRemoved  ChangeKind = "removed"
	ChangeReplaced ChangeKind = "replaced"
)

// Span is a semver distance, as a magnitude at exactly one level: 1.2.3 to
// 2.0.1 is one major, not one major and one patch, because the lower levels
// reset across the boundary and their arithmetic difference names nothing an
// operator would recognize. All three are zero when two versions differ only
// in their prerelease identifiers. Direction is ComponentResult.Downgrade
// rather than a sign, so a renderer never has to test three fields for one.
type Span struct {
	Majors  int
	Minors  int
	Patches int
}

// Reason names why a result carries the verdict it does, as a stable code a
// consumer can branch on without parsing prose. It is empty exactly when
// Verdict is: on a ChangeAdded or ChangeRemoved row, where nothing was
// assessed because no transition was made.
//
// The four reasons that produce unknown stay distinct because they differ in
// what would close the gap, which is the same test that keeps unknown separate
// from unversioned: ReasonNoRecord needs somebody to author the first record,
// ReasonNoBoundaryCrossed needs an existing one widened (or confirmation that
// no boundary belongs there), ReasonDowngrade needs nothing because nothing
// can close it, and ReasonIdentityChanged needs the relocation performed as its
// own piece of work, because no record can close it either: the vocabulary
// describes version boundaries and says nothing about where a release lives.
// Rule 7 rejects every reverse record, so a downgrade is unassessable rather
// than merely unassessed.
//
// ReasonBeyondRecordCeiling is not one of them. A record exists, what it covers
// is known, and the target is known to sit past that, which is a fact about
// assessed ground being exceeded rather than an absence of information.
type Reason string

const (
	ReasonRecorded            Reason = "recorded"
	ReasonRecordBlocks        Reason = "record-blocks"
	ReasonMultipleBoundaries  Reason = "multiple-boundaries"
	ReasonUndefinedOrigin     Reason = "undefined-origin"
	ReasonNoRecord            Reason = "no-record"
	ReasonNoBoundaryCrossed   Reason = "no-boundary-crossed"
	ReasonBeyondRecordCeiling Reason = "beyond-record-ceiling"
	ReasonDowngrade           Reason = "downgrade"
	ReasonIdentityChanged     Reason = "identity-changed"
	ReasonNotComparable       Reason = "not-comparable"
)

// Identity is what a recipe pins for a component beyond its version. A move
// here is invisible to a version comparison but relocates, replaces or renames
// running objects. Helm cannot move a release between namespaces. A renamed
// chart or moved source shares no version line with the old one. A kustomize
// path selects a different manifest set at the same tag. A dropped manifest
// file deletes a live object under GitOps prune, and a pre-install manifest is
// a prerequisite the bundle applies before the release. A moved object name
// renames what the chart owns.
//
// Every field but Version, ManifestFiles, PreManifestFiles and ObjectNames
// counts only when both sides state it. The two manifest lists are sets,
// compared without regard to order.
type Identity struct {
	Version          string
	Namespace        string
	Chart            string
	Source           string
	Path             string
	Type             string
	ManifestFiles    []string
	PreManifestFiles []string

	// ObjectNames are the merged values naming the objects the chart owns,
	// keyed by dotted value path ("fullnameOverride",
	// "grafana.fullnameOverride").
	//
	// Empty is not the same as a component pinning none: it is also what a
	// caller that could not read the values hands over, because a resolved
	// recipe records them by reference rather than by value. The caller
	// reports that gap once for the whole run rather than per component, so
	// nothing here distinguishes the two.
	ObjectNames map[string]string
}

// The IdentityChange.Field of each Identity field. The two manifest file sets
// are the fields whose move also fills Added and Removed; any Field not listed
// here is an object-name value path.
const (
	fieldNamespace        = "namespace"
	fieldType             = "type"
	fieldChart            = "chart"
	fieldSource           = "source"
	fieldPath             = "path"
	fieldManifestFiles    = "manifestFiles"
	fieldPreManifestFiles = "preManifestFiles"
)

// IdentityChange names one field that moved between the compared artifacts.
//
// Field is either an Identity field's name in lower camel case ("namespace",
// "manifestFiles") or, for an object name, a dotted value path into the merged
// values ("fullnameOverride", "grafana.fullnameOverride"), so a consumer
// branches on it without parsing prose. Version is never one of them. The
// version axis is ComponentResult.From and To, and the records assess it.
//
// For manifestFiles and preManifestFiles From and To are the whole sorted sets
// joined by commas, and Added and Removed name the entries that differ. They
// are empty on every other field. From or To is empty where an object name
// appeared or disappeared, which is a rename rather than a missing
// observation; the scalar fields never report an empty side.
type IdentityChange struct {
	Field   string
	From    string
	To      string
	Added   []string
	Removed []string
}

// ComponentResult is one row of an upgrade check.
//
// Every pointer field points into the Set the result was matched against and
// inherits its read-only contract.
type ComponentResult struct {
	// Component is the name the result is keyed and sorted by. On a
	// ChangeReplaced row it is the arriving component, which owns the verdict.
	Component string

	Change ChangeKind

	// ReplacedComponent is the departing component a ChangeReplaced row joins
	// in, and is that row's FROM column: two different pieces of software
	// share no version line, so From stays empty and a renderer reads the
	// kind instead of sniffing the string.
	ReplacedComponent string

	// From and To are the version strings as the tables gave them, not
	// normalized, so a report quotes what the artifacts actually said. From is
	// empty for ChangeAdded and ChangeReplaced, To for ChangeRemoved.
	From string
	To   string

	// IdentityChanges are moves no version comparison can see. Non-empty on a
	// row whose Change is ChangeIdentity, and on a ChangeVersion row when both
	// axes moved in the same hop.
	IdentityChanges []IdentityChange

	// Verdict is empty for ChangeAdded and ChangeRemoved.
	Verdict Verdict

	// Transition is the record that describes this exact move in one upgrade,
	// or the last of a run of safe records taken as one. It is nil when no
	// single upgrade reaches the target, so a renderer never prints one
	// record's steps for a jump that record does not describe.
	Transition *Transition

	// Route is the upgrades, in order, of a blocked version transition that
	// takes more than one and whose records reach the target. It is nil on
	// every other result.
	Route []RouteStop

	// Crossed is every record this jump passes a boundary of, ordered by that
	// boundary. Unlike Transition it is set whatever the verdict turned out to
	// be, including where no single record describes the whole jump, so it is
	// what the at-risk scan reads: an intermediate record names resources the
	// jump disturbs whether or not its guidance was written for this starting
	// point. Empty on every result but a version transition, ChangeIdentity
	// included: a component that held its version passed no boundary, so the
	// scan draws nothing from a relocation however far it moves the release.
	Crossed []*Transition

	// Replaces is the arriving component's declaration, set only on a
	// ChangeReplaced row.
	Replaces *Replaces

	// StoppedAt is the range a blocked version transition stops at. It is:
	//
	//   - where the first upgrade lands, for ReasonMultipleBoundaries
	//   - the `to` whose ceiling the target is past, for ReasonBeyondRecordCeiling
	//   - the interval not to enter in one step, otherwise
	//
	// It is empty on every other result, a blocked ChangeReplaced row
	// included.
	StoppedAt string

	// Span is how far the matched record's claim reaches, from the source
	// version to the ceiling its `to` names. This is the width ADR-021
	// requires the report to state. It is zero when no single record matched
	// or the record names no ceiling. An exclusive ceiling counts as reached:
	// the highest version actually covered is not knowable from the range
	// alone.
	//
	// Span is never narrower than Jump where it is set at all: a target past
	// the record's ceiling no longer takes that record's verdict, so a claim
	// cannot end up narrower than the move it is covering.
	Span Span

	// Jump is the distance between the two versions actually compared. Zero
	// when either side is unversioned.
	Jump Span

	// Breaking reports a boundary semver makes no stability promise across: a
	// major bump, a minor bump while the major version is 0, or a changed
	// prerelease identifier over an otherwise equal release triple. False when
	// either side is unversioned, where there is no boundary to classify.
	//
	// Descriptive only. It used to decide whether an unknown result stopped a
	// strict run; ADR-021 Decision 6 dropped that calibration, because an
	// unassessed transition is unassessed at any distance.
	Breaking bool

	// Downgrade reports that the target orders below the source.
	Downgrade bool

	// Reason and Explanation say why Verdict is what it is: a stable code and
	// the sentence an operator reads, naming the versions involved. Both are
	// empty exactly when Verdict is.
	Reason      Reason
	Explanation string
}

// RouteStop is one upgrade of a route.
type RouteStop struct {
	// Versions is the range the upgrade lands in, or the target version for
	// the last one.
	Versions string

	// Verdict is the upgrade's record's verdict, safe for a run of safe
	// records.
	Verdict Verdict
}

// FailsRun reports whether this result should stop a strict run, per ADR-021
// Decision 6. It is a method rather than a switch in the CLI so the SDK and the
// command cannot disagree about what fails.
func (r ComponentResult) FailsRun() bool {
	if r.Change == ChangeAdded || r.Change == ChangeRemoved {
		return false
	}
	if len(r.IdentityChanges) > 0 {
		// Stated independently of the verdict rather than relying on
		// MatchIdentities having withdrawn a safe one, because a result can be
		// built without going through the matcher at all and a relocation that
		// slipped through as safe is the whole failure this axis exists to
		// catch.
		return true
	}
	// Only safe passes. A transition nobody assessed is not a transition anyone
	// approved, so unknown fails however far the versions moved; Breaking once
	// calibrated that and no longer does. A verdict outside the vocabulary is
	// reachable because a Set can be built without going through Load, and it
	// fails for the same reason an unrecognized one must not pass.
	return r.Verdict != VerdictSafe
}

// Match compares two component-to-version tables, for callers that hold only
// versions.
//
// It is MatchIdentities over identities that state a version and nothing else,
// which leaves the identity axis with nothing to compare, so the results are
// exactly the version results.
func Match(set Set, from, to map[string]string) []ComponentResult {
	return MatchIdentities(set, versionsOnly(from), versionsOnly(to))
}

func versionsOnly(versions map[string]string) map[string]Identity {
	out := make(map[string]Identity, len(versions))
	for name, v := range versions {
		out[name] = Identity{Version: v}
	}
	return out
}

// MatchIdentities compares two component-to-identity tables against the set's
// records and returns one result per component whose deployment changes, sorted
// by component name.
//
// It is pure: tables in, results out, with no filesystem, cluster or registry
// access. A component whose identity is the same on both sides produces no row.
//
// The two axes move independently, and only one of them is assessed by anybody.
//
// On the version axis the verdict comes from the route of records from the
// source to the target. Every result carries a Reason and an Explanation.
//
// On the identity axis nothing is recorded, so nothing lends a verdict. A
// component that moved there alone produces a ChangeIdentity row, which a
// version comparison reports as no change whatsoever; one that moved on both
// axes in the same hop carries the moves on its ChangeVersion row, and a safe
// verdict is withdrawn there, because the record vouched for a version hop and
// was never asked about a relocation.
//
// set must already have passed Validate; MatchIdentities does not re-run it. A
// malformed record cannot panic here either: a transition whose ranges do not
// parse, or whose `to` names no floor, simply never applies, leaving the
// component at unknown rather than lending it a verdict the record cannot
// support.
func MatchIdentities(set Set, from, to map[string]Identity) []ComponentResult {
	arrivals, superseded := replacements(set, from, to)

	names := make([]string, 0, len(from)+len(to))
	for name := range from {
		names = append(names, name)
	}
	for name := range to {
		if _, inBoth := from[name]; !inBoth {
			names = append(names, name)
		}
	}
	slices.Sort(names)

	results := make([]ComponentResult, 0, len(names))
	for _, name := range names {
		src, inFrom := from[name]
		tgt, inTo := to[name]
		switch {
		case inFrom && inTo:
			moved := identityChanges(src, tgt)
			// Versions compared as written but for a leading "v", which semver
			// assigns no meaning and the deployers apply inconsistently:
			// deployer.NormalizeVersion strips it from an Argo CD
			// targetRevision, so a cluster pinned at "v26.7.0" reads back
			// "26.7.0" and a bare comparison reports it as changed. Nothing
			// else is normalized, so two pins differing only in build metadata
			// still report: semver orders those as equal, and silence there
			// reads as safe.
			//
			// The identity axis is decided against the same test, so a hop
			// that respells the version and relocates the component is the one
			// ChangeIdentity row it is, rather than a ChangeVersion row whose
			// columns would print the two spellings and read as a bump.
			if trimVPrefix(src.Version) == trimVPrefix(tgt.Version) {
				if len(moved) == 0 {
					continue
				}
				results = append(results, relocation(name, src.Version, tgt.Version, moved))
				continue
			}
			r := matchVersions(set[name], name, src.Version, tgt.Version)
			results = append(results, withIdentityChanges(r, moved))
		case inTo:
			results = append(results, arrival(name, tgt.Version, arrivals[name]))
		default:
			if superseded[name] {
				continue // joined into the arriving component's row
			}
			results = append(results, ComponentResult{Component: name, Change: ChangeRemoved, From: src.Version})
		}
	}
	return results
}

// identityChanges lists the non-version fields that moved: the scalar fields,
// then the manifest file sets, then the object-name paths in sorted order.
//
// A scalar field counts only when both sides state it. An empty string is a fact
// the artifact did not carry rather than a move to the default, so treating it
// as a value would report a relocation nobody performed for every component the
// moment one of the two artifacts stops carrying the field.
//
// The manifest file sets and object names are the exceptions, because for them
// absence is itself a statement. An artifact that lists no manifests is
// stating that, and the move that matters most is the one that empties it. A
// chart with no fullnameOverride names its objects after itself, so dropping
// one renames every object it held, and both directions are moves.
func identityChanges(from, to Identity) []IdentityChange {
	var moved []IdentityChange
	for _, f := range []struct {
		field    string
		from, to string
	}{
		{fieldNamespace, from.Namespace, to.Namespace},
		{fieldType, from.Type, to.Type},
		{fieldChart, from.Chart, to.Chart},
		{fieldSource, from.Source, to.Source},
		{fieldPath, from.Path, to.Path},
	} {
		if f.from != "" && f.to != "" && f.from != f.to {
			moved = append(moved, IdentityChange{Field: f.field, From: f.from, To: f.to})
		}
	}
	for _, f := range []struct {
		field    string
		from, to []string
	}{
		{fieldManifestFiles, from.ManifestFiles, to.ManifestFiles},
		{fieldPreManifestFiles, from.PreManifestFiles, to.PreManifestFiles},
	} {
		was, now := sets.New(f.from...), sets.New(f.to...)
		if added, removed := now.Difference(was), was.Difference(now); added.Len() > 0 || removed.Len() > 0 {
			moved = append(moved, IdentityChange{
				Field:   f.field,
				From:    strings.Join(sets.List(was), ","),
				To:      strings.Join(sets.List(now), ","),
				Added:   slices.Sorted(maps.Keys(added)),
				Removed: slices.Sorted(maps.Keys(removed)),
			})
		}
	}
	// Sized from one side only, as in recipe.unionPaths: the two lengths come
	// from a bundle's own files, and their sum is not provably an int.
	paths := make([]string, 0, len(from.ObjectNames))
	for path := range from.ObjectNames {
		paths = append(paths, path)
	}
	for path := range to.ObjectNames {
		if _, inBoth := from.ObjectNames[path]; !inBoth {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	for _, path := range paths {
		src, tgt := from.ObjectNames[path], to.ObjectNames[path]
		if src == tgt {
			continue
		}
		moved = append(moved, IdentityChange{Field: path, From: src, To: tgt})
	}
	return moved
}

// identityAdvice is the tail both identity explanations share: why no record
// covers the move, and what the fields that moved will actually do.
//
// The clauses are selected rather than fixed. The fields fail differently — a
// relocation leaves a second copy running, a changed chart, source, path or
// type deploys different objects, a dropped manifest is pruned, and a rename
// replaces what the chart owns — and this sentence is what an operator reads
// at the moment they decide whether to upgrade, so naming the wrong
// consequence sends them to the wrong remedy.
//
// The selector clause is deliberately narrow. The immutable-selector failure
// needs an object that keeps its NAME while its selector labels change — the
// standard scaffold's nameOverride moving under a pinned fullnameOverride. A
// key that renames the object too is plain delete-and-recreate with no error,
// so the text names the one condition rather than promising a failure that
// may not happen. It ships in the JSON explanation, so it is worth getting
// exactly right once.
func identityAdvice(moved []IdentityChange) string {
	kinds := identityKindsOf(moved)
	parts := []string{"Transition records assess version boundaries, so none assesses a change of identity."}
	if kinds[kindReplace] {
		parts = append(parts, "Applying the new recipe would deploy different objects than the running release.")
	}
	if kinds[kindRelocate] {
		parts = append(parts, "A namespace move installs a second copy because Helm cannot move a release "+
			"between namespaces.")
	}
	if kinds[kindPrune] {
		parts = append(parts, "A dropped manifest file deletes a live object under GitOps prune.")
	}
	if kinds[kindRename] {
		parts = append(parts, "Renaming the objects a chart owns is applied as delete-and-recreate, "+
			"so expect a service gap, and an orphan for anything referenced by name or not owned by "+
			"the release. Where the moved key changes the selector labels of an object whose name "+
			"does not change, for example nameOverride moving while fullnameOverride is pinned, "+
			"spec.selector is immutable and the upgrade fails outright instead.")
	}
	return strings.Join(append(parts, "Make the change deliberately, then re-run this check."), " ")
}

// identityKind is what a move does to the running release, which is what the
// advice and the notes have to name.
type identityKind int

const (
	kindRelocate identityKind = iota
	kindReplace
	kindPrune
	kindRename
)

// identityKindsOf classifies every move. A manifest set can be two kinds at
// once: what it drops is pruned, what it adds is new objects.
func identityKindsOf(moved []IdentityChange) map[identityKind]bool {
	kinds := make(map[identityKind]bool)
	for _, c := range moved {
		switch c.Field {
		case fieldNamespace:
			kinds[kindRelocate] = true
		case fieldType, fieldChart, fieldSource, fieldPath:
			kinds[kindReplace] = true
		case fieldManifestFiles, fieldPreManifestFiles:
			if len(c.Removed) > 0 {
				kinds[kindPrune] = true
			}
			if len(c.Added) > 0 {
				kinds[kindReplace] = true
			}
		default:
			kinds[kindRename] = true
		}
	}
	return kinds
}

// isObjectNameField reports whether field is an object-name value path rather
// than an Identity field. Every Identity field name is a constant here, so
// anything else came from the merged values.
func isObjectNameField(field string) bool {
	switch field {
	case fieldNamespace, fieldType, fieldChart, fieldSource, fieldPath,
		fieldManifestFiles, fieldPreManifestFiles:
		return false
	}
	return true
}

// relocation is the row for a component that held its version and moved anyway.
//
// Unknown rather than blocked, for the reason that separates the two: nobody
// was asked. A block is an author's judgement, and no author can record one
// here, so this is a gap in what the vocabulary covers rather than a boundary
// somebody drew.
//
// Both spellings of the held version are carried. The equality test ignores a
// leading "v", so the two sides can write one version two ways, and printing
// either side's spelling in both columns would attribute to an artifact a
// string it does not contain.
func relocation(name, fromVer, toVer string, moved []IdentityChange) ComponentResult {
	return ComponentResult{
		Component:       name,
		Change:          ChangeIdentity,
		From:            fromVer,
		To:              toVer,
		IdentityChanges: moved,
		Verdict:         VerdictUnknown,
		Reason:          ReasonIdentityChanged,
		Explanation: fmt.Sprintf("%s %s, but %s. %s",
			name, heldPhrase(fromVer), movedPhrase(moved), identityAdvice(moved)),
	}
}

// heldPhrase says what the component held still at, for a component that may
// not pin a version at all. An unpinned one reaches here with an empty string,
// and naming a version that the artifact never stated would invent one.
func heldPhrase(version string) string {
	if version == "" {
		return "does not change version"
	}
	return "stays at " + version
}

// withIdentityChanges attaches the moves the version comparison could not see
// and withdraws a safe verdict that no longer covers the whole hop.
//
// The record assessed a version boundary and nothing else: reading its safe
// verdict as covering the relocation that shipped in the same hop would take a
// claim about one axis as evidence about another, which is the false confidence
// a wrong safe buys. Only safe is withdrawn, because every other verdict
// already stops a strict run and already sends the reader to the row.
func withIdentityChanges(r ComponentResult, moved []IdentityChange) ComponentResult {
	if len(moved) == 0 {
		return r
	}
	r.IdentityChanges = moved
	if r.Verdict != VerdictSafe {
		return r
	}
	r.Verdict = VerdictUnknown
	r.Reason = ReasonIdentityChanged
	r.Explanation = fmt.Sprintf(
		"the move from %s to %s is recorded safe, but %s, and no record assessed that. %s",
		r.From, r.To, movedPhrase(moved), identityAdvice(moved))
	return r
}

// movedPhrase states the moves as the clause both explanations embed.
//
// An object name appearing or disappearing gets its own wording, because the
// from-to form degenerates to "moves from skyhook-operator to" on exactly the
// row where the rename is the whole finding.
func movedPhrase(moved []IdentityChange) string {
	parts := make([]string, len(moved))
	for i, c := range moved {
		switch {
		case c.Field == fieldManifestFiles || c.Field == fieldPreManifestFiles:
			parts[i] = "its " + c.Field + " " + setChangePhrase(c.Added, c.Removed)
		case c.From == "":
			parts[i] = fmt.Sprintf("its %s is now set to %s", c.Field, c.To)
		case c.To == "":
			parts[i] = fmt.Sprintf("its %s is no longer set, dropping %s", c.Field, c.From)
		default:
			parts[i] = fmt.Sprintf("its %s moves from %s to %s", c.Field, c.From, c.To)
		}
	}
	return strings.Join(parts, " and ")
}

// setChangePhrase states a set move as what left and what arrived.
func setChangePhrase(added, removed []string) string {
	var parts []string
	if len(removed) > 0 {
		parts = append(parts, "drop "+strings.Join(removed, ", "))
	}
	if len(added) > 0 {
		parts = append(parts, "add "+strings.Join(added, ", "))
	}
	return strings.Join(parts, " and ")
}

// trimVPrefix drops the leading "v" a version may or may not be written with.
// It normalizes the same-version decision only; every reported version keeps
// the form the table it came from used.
func trimVPrefix(v string) string {
	return strings.TrimPrefix(v, "v")
}

// replacements resolves the declarations that join a departure and an arrival
// into one row, returning them keyed by arriving component alongside the set of
// departing components they consume.
//
// The matcher cannot derive a replacement: nothing in the tables tells "A was
// replaced by B" from "A went away and B arrived", so the arriving component's
// record says so.
func replacements(set Set, from, to map[string]Identity) (map[string]*Replaces, map[string]bool) {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	slices.Sort(names)

	arrivals := make(map[string]*Replaces)
	superseded := make(map[string]bool)
	for _, name := range names {
		u := set[name]
		if u == nil || u.Replaces == nil || u.Replaces.Component == "" {
			continue
		}
		departing := u.Replaces.Component
		_, arrivingInFrom := from[name]
		_, arrivingInTo := to[name]
		_, departingInFrom := from[departing]
		_, departingInTo := to[departing]
		// A declaration describes this comparison only when the swap happened
		// in it. Where the named component is still deployed, or the arriving
		// one already was, the two rows are about live software that a record
		// merely mentions, and joining them would report a migration nobody is
		// performing.
		if arrivingInFrom || !arrivingInTo || !departingInFrom || departingInTo {
			continue
		}
		// Two arrivals naming the same departure would each consume it. First
		// by name wins, so the report does not depend on map order.
		if superseded[departing] {
			continue
		}
		superseded[departing] = true
		arrivals[name] = u.Replaces
	}
	return arrivals, superseded
}

func arrival(name, version string, r *Replaces) ComponentResult {
	if r == nil {
		return ComponentResult{Component: name, Change: ChangeAdded, To: version}
	}
	return ComponentResult{
		Component:         name,
		Change:            ChangeReplaced,
		ReplacedComponent: r.Component,
		To:                version,
		Verdict:           r.Verdict,
		Replaces:          r,
		Reason:            ReasonRecorded,
		Explanation: fmt.Sprintf(
			"recorded %s: %s replaces %s, which is migration work rather than a version bump",
			r.Verdict, name, r.Component),
	}
}

func matchVersions(u *ComponentUpgrades, name, fromVer, toVer string) ComponentResult {
	r := ComponentResult{Component: name, Change: ChangeVersion, From: fromVer, To: toVer}
	src, srcErr := semver.NewVersion(fromVer)
	tgt, tgtErr := semver.NewVersion(toVer)
	if srcErr != nil || tgtErr != nil {
		// A gap in the inputs, not in the data: kept distinct from unknown
		// because pinning something comparable is what closes it.
		r.Verdict = VerdictUnversioned
		r.Reason = ReasonNotComparable
		r.Explanation = fmt.Sprintf(
			"version %q is not comparable to %q; pin a semver version on both sides", fromVer, toVer)
		return r
	}
	r.Downgrade = tgt.Compare(src) < 0
	r.Jump = spanBetween(src, tgt)
	r.Breaking = breaking(src, tgt)

	crossed := crossings(u, inclusiveEnd(src), tgt)
	// Recorded before the verdict is decided, because every branch below
	// returns r and only some of them keep a record around.
	r.Crossed = transitionsOf(crossed)
	if len(crossed) == 0 {
		return unmatched(u, r, src, tgt)
	}
	best, complete, ok := plan(u, crossed, src, tgt)
	if !ok {
		r.Verdict = VerdictBlocked
		r.Reason = ReasonUndefinedOrigin
		r.StoppedAt = targetRangeText(u, crossed[0].tr)
		r.Explanation = undefinedOriginExplanation(u, r, crossed, src, tgt)
		return r
	}
	ms := moves(u, best, src, tgt)
	if len(ms) == 1 {
		last := ms[0].hops[len(ms[0].hops)-1]
		if !complete {
			r = stalledResult(u, r, last, crossed)
			// A lone blocked record below the target holds the component at
			// its ceiling.
			if len(ms[0].hops) == 1 && last.tr.Verdict == VerdictBlocked && r.Reason == ReasonBeyondRecordCeiling {
				r.Explanation = heldExplanation(u, r, last)
			}
			return r
		}
		// A blocked record that describes the move keeps its steps.
		r = lend(u, r, src, last)
		if r.Verdict == VerdictBlocked {
			r.StoppedAt = targetRangeText(u, r.Transition)
		}
		return r
	}
	r.Verdict = VerdictBlocked
	if complete {
		r.Route = routeStops(u, ms, r.To)
	}
	// A blocked hop never joins a run, so it is its move's only hop.
	if i := slices.IndexFunc(ms, func(m move) bool { return m.verdict() == VerdictBlocked }); i >= 0 {
		b := ms[i].hops[0]
		r.Reason = ReasonRecordBlocks
		r.StoppedAt = targetRangeText(u, b.tr)
		r.Explanation = recordBlocksExplanation(u, r, ms, i, b)
		return r
	}
	r.Reason = ReasonMultipleBoundaries
	r.StoppedAt = landsText(u, ms[0])
	r.Explanation = multipleBoundariesExplanation(u, r, crossed, ms)
	return r
}

// routeStops returns ms as the result's Route, ending at target.
func routeStops(u *ComponentUpgrades, ms []move, target string) []RouteStop {
	out := make([]RouteStop, len(ms))
	for i, m := range ms {
		out[i] = RouteStop{Versions: target, Verdict: m.verdict()}
		if i < len(ms)-1 {
			out[i].Versions = landsText(u, m)
		}
	}
	return out
}

// multipleBoundariesExplanation returns r's Explanation for a route ms of
// several upgrades, none blocked.
func multipleBoundariesExplanation(u *ComponentUpgrades, r ComponentResult, crossed []crossing, ms []move) string {
	first := ms[0]
	s := fmt.Sprintf("%s; no single record describes the whole jump. Upgrade to %s first, then re-run this check",
		crossingPhrase(crossed), landsPhrase(u, first))
	switch first.why {
	case stopRequired:
		last := first.hops[len(first.hops)-1]
		s += fmt.Sprintf(". The record covering %s is a required stop: a version in %s has to run "+
			"before the component moves further", last.floor.ver, targetRangeText(u, last.tr))
	case stopStepLimit:
		s += fmt.Sprintf(". This component moves at most one %s version per upgrade", u.StepLimit)
	case stopSteps:
	}
	if len(r.Route) == 0 {
		return s
	}
	parts := make([]string, len(r.Route))
	for i, st := range r.Route {
		parts[i] = fmt.Sprintf("%s (%s)", st.Versions, st.Verdict)
	}
	return s + fmt.Sprintf(". The whole route takes %d upgrades: to %s", len(parts), strings.Join(parts, ", then to "))
}

// lend returns r with the verdict, Transition and Span of c's record.
func lend(u *ComponentUpgrades, r ComponentResult, src *semver.Version, c crossing) ComponentResult {
	r.Verdict = c.tr.Verdict
	r.Transition = c.tr
	r.Span = claimSpan(u, src, c.tr)
	r.Reason = ReasonRecorded
	r.Explanation = recordedExplanation(r, c)
	return r
}

// stalledResult returns r blocked for a route whose last hop stops short of
// the target, with ReasonUndefinedOrigin when a floor in crossed sits past
// last's ceiling and ReasonBeyondRecordCeiling otherwise.
func stalledResult(u *ComponentUpgrades, r ComponentResult, last crossing, crossed []crossing) ComponentResult {
	r.Verdict = VerdictBlocked
	var ahead []crossing
	if to, err := targetRange(u, last.tr); err == nil {
		for _, c := range crossed {
			if !lowerAtOrBelow(c.floor, to.upper) {
				ahead = append(ahead, c)
			}
		}
	}
	if len(ahead) == 0 {
		ceiling := targetCeiling(u, last.tr)
		r.Reason = ReasonBeyondRecordCeiling
		r.StoppedAt = targetRangeText(u, last.tr)
		r.Explanation = beyondCeilingExplanation(ceiling, r.To)
		if _, highest, ok := highestCeiling(u); ok && upperAfter(highest, ceiling) {
			r.Explanation = pathCeilingExplanation(ceiling, r.From, r.To)
		}
		return r
	}
	r.Reason = ReasonUndefinedOrigin
	r.StoppedAt = targetRangeText(u, ahead[0].tr)
	r.Explanation = fmt.Sprintf(
		"%s, but no record describes an upgrade starting from %s, where the records written for %s lead. "+
			"Author a record for that starting point, then re-run this check",
		crossingPhrase(ahead), landingPhrase(u, last), r.From)
	return r
}

// recordBlocksExplanation returns r's Explanation when b, the blocked hop of
// ms[i], stops the jump. It names where ms[0] lands when i > 0, and points to
// b's steps otherwise.
func recordBlocksExplanation(u *ComponentUpgrades, r ComponentResult, ms []move, i int, b crossing) string {
	s := fmt.Sprintf("blocked by the record covering %s: do not move from %s into %s in one step. ",
		b.floor.ver, r.From, targetRangeText(u, b.tr))
	if i > 0 {
		return s + fmt.Sprintf("Upgrade to %s first, then re-run this check", landsPhrase(u, ms[0]))
	}
	return s + fmt.Sprintf("Check the move from %s to %s on its own for that record's steps, "+
		"then re-run this check", r.From, landingPhrase(u, b))
}

// heldExplanation returns r's Explanation when b, a lone blocked hop, stops
// assessing below the target.
func heldExplanation(u *ComponentUpgrades, r ComponentResult, b crossing) string {
	ceiling := targetCeiling(u, b.tr)
	return fmt.Sprintf(
		"the record covering %s assesses only as far as %s, and %s lands past that. It also blocks moving "+
			"from %s into %s in one step. Check the move from %s to %s on its own for that record's steps, "+
			"then re-run this check",
		b.floor.ver, ceilingPhrase(ceiling), r.To, r.From, targetRangeText(u, b.tr), r.From, landingPhrase(u, b))
}

// landingPhrase returns c's floor version, or "a version in" c's `to` when the
// floor is exclusive.
func landingPhrase(u *ComponentUpgrades, c crossing) string {
	if c.floor.inclusive {
		return c.floor.ver.String()
	}
	return "a version in " + targetRangeText(u, c.tr)
}

// crossing pairs a transition with the floor its `to` names.
type crossing struct {
	tr    *Transition
	floor rangeEnd
}

// crossings returns the transitions a jump from at to tgt crosses, in
// crossingBefore order.
func crossings(u *ComponentUpgrades, at rangeEnd, tgt *semver.Version) []crossing {
	if u == nil {
		return nil
	}
	out := make([]crossing, 0, len(u.Transitions))
	for i := range u.Transitions {
		floor, ok := crosses(&u.Transitions[i], at, tgt)
		if !ok {
			continue
		}
		out = append(out, crossing{tr: &u.Transitions[i], floor: floor})
	}
	sort.SliceStable(out, func(i, j int) bool { return crossingBefore(out[i], out[j]) })
	return out
}

// crossingBefore reports whether a sorts before b by floor, then by `from`
// lower bound with unbounded first, then by `to` and `from` text.
func crossingBefore(a, b crossing) bool {
	if lowerBefore(a.floor, b.floor) || lowerBefore(b.floor, a.floor) {
		return lowerBefore(a.floor, b.floor)
	}
	af, aErr := parseRange(a.tr.From)
	bf, bErr := parseRange(b.tr.From)
	if aErr == nil && bErr == nil && (lowerBefore(af.lower, bf.lower) || lowerBefore(bf.lower, af.lower)) {
		return lowerBefore(af.lower, bf.lower)
	}
	if a.tr.To != b.tr.To {
		return a.tr.To < b.tr.To
	}
	return a.tr.From < b.tr.From
}

// crosses returns the floor t's `to` names. ok reports whether at sits below
// it and tgt reaches it, whatever t's `from` holds. Special cases are:
//
//   - an unparseable `from` or `to` crosses nothing
//   - a `to` with no floor crosses nothing
func crosses(t *Transition, at rangeEnd, tgt *semver.Version) (floor rangeEnd, ok bool) {
	if _, err := parseRange(t.From); err != nil {
		// An unparseable `from` leaves no way to tell whose guidance this is,
		// so the record stays inert rather than blocking every jump past it.
		return rangeEnd{}, false
	}
	toRange, err := parseRange(t.To)
	if err != nil {
		return rangeEnd{}, false
	}
	// A `to` with no floor names no boundary at all, so there is nothing to
	// cross. Rule 2 rejects the shape, but Match also runs on Sets that never
	// met Validate.
	if toRange.lower.unbounded {
		return rangeEnd{}, false
	}
	// `from` membership is fromCovers' question. Crossing ignores it so the
	// at-risk scan sees every boundary a jump passes.
	if !lowerBefore(at, toRange.lower) {
		return rangeEnd{}, false
	}
	cmp := tgt.Compare(toRange.lower.comparedAt())
	if cmp < 0 || (cmp == 0 && !toRange.lower.inclusive) {
		return rangeEnd{}, false
	}
	return toRange.lower, true
}

// targetCeiling returns the ceiling of t's `to`, or the zero rangeEnd when it has
// none or does not parse.
func targetCeiling(u *ComponentUpgrades, t *Transition) rangeEnd {
	b, err := targetRange(u, t)
	if err != nil || b.upper.unbounded || b.upper.ver == nil {
		return rangeEnd{}
	}
	return b.upper
}

// pastCeiling reports whether v sits above a bounded ceiling. A version exactly
// at an inclusive ceiling is covered by it; at an exclusive one it is not.
func pastCeiling(ceiling rangeEnd, v *semver.Version) bool {
	cmp := v.Compare(ceiling.comparedAt())
	return cmp > 0 || (cmp == 0 && !ceiling.inclusive)
}

// ceilingPhrase names the highest version a ceiling actually covers, as a place
// an operator can be told to stop. The bare number will not do: "<0.19.0" and
// "<=0.19.0" end at different versions, and only one of them is 0.19.0 itself.
func ceilingPhrase(b rangeEnd) string {
	if b.inclusive {
		return b.ver.String()
	}
	return "the last version below " + b.ver.String()
}

// inclusiveEnd returns the inclusive range end at v.
func inclusiveEnd(v *semver.Version) rangeEnd {
	return rangeEnd{ver: v, inclusive: true}
}

// onlyVersion returns the interval holding v and nothing else.
func onlyVersion(v *semver.Version) versionRange {
	return versionRange{lower: inclusiveEnd(v), upper: inclusiveEnd(v)}
}

// fromCovers reports whether t's `from` admits at. An unparseable `from`
// admits nothing.
func fromCovers(t *Transition, at rangeEnd) bool {
	b, err := parseRange(t.From)
	if err != nil {
		return false
	}
	return admits(b, at)
}

// admits reports whether b holds at. A non-inclusive at stands just above its
// version.
func admits(b versionRange, at rangeEnd) bool {
	if at.inclusive {
		return b.contains(at.comparedAt())
	}
	if !b.lower.unbounded && (b.lower.ver == nil || at.comparedAt().Compare(b.lower.comparedAt()) < 0) {
		return false
	}
	if !b.upper.unbounded && (b.upper.ver == nil || at.comparedAt().Compare(b.upper.comparedAt()) >= 0) {
		return false
	}
	return true
}

// leadsFrom returns the distinct parseable `to` ranges of records whose `from`
// covers src, in targetRangeBefore order.
func leadsFrom(u *ComponentUpgrades, src *semver.Version) []string {
	if u == nil {
		return nil
	}
	type lead struct {
		text string
		to   versionRange
	}
	var leads []lead
	for i := range u.Transitions {
		t := &u.Transitions[i]
		text := targetRangeText(u, t)
		if !fromCovers(t, inclusiveEnd(src)) || slices.ContainsFunc(leads, func(l lead) bool { return l.text == text }) {
			continue
		}
		to, err := targetRange(u, t)
		if err != nil {
			continue
		}
		leads = append(leads, lead{text: text, to: to})
	}
	sort.Slice(leads, func(i, j int) bool { return targetRangeBefore(leads[i].to, leads[j].to, leads[i].text, leads[j].text) })
	out := make([]string, len(leads))
	for i, l := range leads {
		out[i] = l.text
	}
	return out
}

// targetRangeBefore reports whether a sorts before b by floor, unbounded first, then by
// their text aText and bText.
func targetRangeBefore(a, b versionRange, aText, bText string) bool {
	if lowerBefore(a.lower, b.lower) || lowerBefore(b.lower, a.lower) {
		return lowerBefore(a.lower, b.lower)
	}
	return aText < bText
}

// transitionsOf is the crossings' records, in the order crossings put them.
func transitionsOf(crossed []crossing) []*Transition {
	if len(crossed) == 0 {
		return nil
	}
	out := make([]*Transition, len(crossed))
	for i, c := range crossed {
		out[i] = c.tr
	}

	return out
}

// floorNames returns the version of each distinct floor in crossed, in its
// order.
func floorNames(crossed []crossing) []string {
	names := make([]string, 0, len(crossed))
	for i, c := range crossed {
		if i > 0 && boundaryKeyOf(crossed[i-1].floor) == boundaryKeyOf(c.floor) {
			continue
		}
		names = append(names, c.floor.ver.String())
	}
	return names
}

func boundaryKeyOf(b rangeEnd) boundaryKey {
	return boundaryKey{ver: b.comparedAt().String(), inclusive: b.inclusive}
}

// crossingPhrase returns the clause naming the distinct floors in crossed,
// which must be sorted by floor.
func crossingPhrase(crossed []crossing) string {
	names := floorNames(crossed)
	if len(names) == 1 {
		return "crosses the boundary at " + names[0]
	}
	return fmt.Sprintf("crosses %d recorded boundaries (%s)", len(names), strings.Join(names, ", "))
}

// unmatched classifies a jump that crosses no recorded boundary.
//
// Direction comes first: a downgrade needs a reverse record rule 7 forbids, so
// reporting a coverage gap there would point the reader at a remedy that cannot
// exist. Otherwise coverage decides, and the highest `to` ceiling is the top of
// it. That ceiling stands for everything anyone assessed only because rules 2
// and 3 hold together: rule 3 leaves the `from` domains no hole below the pin,
// and rule 2 keeps every `to` ceiling at or under it, so a target above the
// highest ceiling is past the data rather than inside a quiet stretch of it.
//
// src distinguishes the two ways a jump can land past that ceiling. When src
// is still below it, the jump passes the ceiling and the explanation names
// where assessment stops. When src is already at or past it, nothing assesses
// any move from src at all, and the explanation says so instead.
func unmatched(u *ComponentUpgrades, r ComponentResult, src, tgt *semver.Version) ComponentResult {
	switch {
	case r.Downgrade:
		r.Verdict = VerdictUnknown
		r.Reason = ReasonDowngrade
		r.Explanation = fmt.Sprintf(
			"downgrade from %s to %s. Transition records describe forward moves only, so none "+
				"describes this one and none ever can: this is unassessable rather than merely "+
				"unassessed, and there is no intermediate version to land on. Review the "+
				"component's own downgrade guidance before proceeding",
			r.From, r.To)
	case u == nil || len(u.Transitions) == 0:
		r.Verdict = VerdictUnknown
		r.Reason = ReasonNoRecord
		r.Explanation = fmt.Sprintf(
			"no transition record exists for this component, so nothing assesses the move from "+
				"%s to %s. This is not a pass: read the component's own upstream release notes "+
				"and decide, then consider authoring the first record so the next operator does "+
				"not repeat the work",
			r.From, r.To)
	default:
		if top, ceiling, ok := highestCeiling(u); ok && pastCeiling(ceiling, tgt) {
			r.Verdict = VerdictBlocked
			r.Reason = ReasonBeyondRecordCeiling
			r.StoppedAt = targetRangeText(u, top)
			r.Explanation = beyondCeilingExplanation(ceiling, r.To)
			if pastCeiling(ceiling, src) || (ceiling.inclusive && src.Equal(ceiling.ver)) {
				r.Explanation = sourceAtCeilingExplanation(ceiling, src, r.From, r.To)
			}
			return r
		}
		r.Verdict = VerdictUnknown
		r.Reason = ReasonNoBoundaryCrossed
		r.Explanation = fmt.Sprintf(
			"a record exists for this component but says nothing about the range between %s and "+
				"%s, so no author flagged this move. This is not a pass: read the component's own "+
				"upstream release notes and decide whether this range needs a boundary, then "+
				"widen the record if it does",
			r.From, r.To)
	}
	return r
}

// beyondCeilingExplanation states what both routes to ReasonBeyondRecordCeiling
// have in common: assessment stops at a version the target is above.
func beyondCeilingExplanation(ceiling rangeEnd, target string) string {
	landing := ceilingPhrase(ceiling)
	return fmt.Sprintf(
		"transition records for this component assess only as far as %s, and %s lands past that, "+
			"so nothing assesses this move or anything above %s. Upgrade no further than %s and "+
			"re-run this check, or widen a record's `to` range to cover %s",
		landing, target, landing, landing, target)
}

// sourceAtCeilingExplanation returns the Explanation for a jump past ceiling
// from a source already at or past it, which leaves no recorded version to
// stop at.
func sourceAtCeilingExplanation(ceiling rangeEnd, src *semver.Version, source, target string) string {
	where := "at"
	if pastCeiling(ceiling, src) {
		where = "past"
	}
	return fmt.Sprintf(
		"transition records for this component assess only as far as %s, and %s is already %s that, "+
			"so nothing assesses any move from it. Author a record whose `from` admits %s and whose "+
			"`to` covers %s, then re-run this check",
		ceilingPhrase(ceiling), source, where, source, target)
}

// pathCeilingExplanation returns the Explanation for a jump from source to
// target whose route stops assessing at ceiling, below the component's highest.
func pathCeilingExplanation(ceiling rangeEnd, source, target string) string {
	landing := ceilingPhrase(ceiling)
	return fmt.Sprintf(
		"the records along the upgrade path from %s assess only as far as %s, and %s lands past that, "+
			"so nothing assesses this move. Upgrade no further than %s and re-run this check, or widen "+
			"a record's `to` range to cover %s",
		source, landing, target, landing, target)
}

// highestCeiling returns the transition whose `to` reaches furthest, and that
// ceiling, breaking ties by targetRangeBefore. ok is false when a `to` has no ceiling
// or none parses.
func highestCeiling(u *ComponentUpgrades) (top *Transition, highest rangeEnd, ok bool) {
	if u == nil {
		return nil, rangeEnd{}, false
	}
	var topTo versionRange
	for i := range u.Transitions {
		b, err := targetRange(u, &u.Transitions[i])
		if err != nil {
			continue
		}
		if b.upper.unbounded || b.upper.ver == nil {
			return nil, rangeEnd{}, false
		}
		tie := top != nil && !upperAfter(b.upper, highest) && !upperAfter(highest, b.upper)
		if top == nil || upperAfter(b.upper, highest) || (tie && targetRangeBefore(b, topTo, u.Transitions[i].To, top.To)) {
			top, topTo, highest = &u.Transitions[i], b, b.upper
		}
	}
	if top == nil {
		return nil, rangeEnd{}, false
	}
	return top, highest, true
}

func recordedExplanation(r ComponentResult, c crossing) string {
	s := fmt.Sprintf("recorded %s: the record covering %s describes the move from %s to %s",
		c.tr.Verdict, c.floor.ver, r.From, r.To)
	switch {
	case c.tr.Verdict == VerdictSafe && c.tr.VerifiedBy != "":
		return s + ", verified by " + strconv.Quote(c.tr.VerifiedBy)
	case c.tr.Verdict == VerdictManual:
		return s + ". Follow the steps below, then apply"
	case c.tr.Verdict == VerdictBlocked:
		return s + " and blocks it in one step. Follow the steps below to get there safely"
	default:
		return s
	}
}

func undefinedOriginExplanation(u *ComponentUpgrades, r ComponentResult, crossed []crossing, src, tgt *semver.Version) string {
	if c, passed, found := unlistedPassage(u, crossed, src, tgt); found {
		return fmt.Sprintf("%s, but the record covering %s whose from holds %s passes the boundary at %s "+
			"described for %s without listing it in skips, so it describes no move that far. Author a "+
			"record for this move, or list that boundary in skips if a cluster may pass it, then re-run "+
			"this check",
			crossingPhrase(crossed), c.floor.ver, r.From, passed.floor.ver, passed.tr.From)
	}
	if leads := leadsFrom(u, src); len(leads) > 0 {
		return fmt.Sprintf("%s, but the records written for %s lead only into %s, so none describes this move. "+
			"Author a record for this move, then re-run this check",
			crossingPhrase(crossed), r.From, strings.Join(leads, " and "))
	}
	s := fmt.Sprintf("%s, but no record describes an upgrade starting from %s", crossingPhrase(crossed), r.From)
	if origin := earliestOrigin(u); origin != nil && src.Compare(origin) < 0 {
		return fmt.Sprintf("%s; the earliest recorded starting point is %s. "+
			"Upgrade to a recorded version first, then re-run this check", s, origin)
	}
	return s + ", so nothing covers this move. Author a record for this starting point, then re-run this check"
}

// unlistedPassage returns the first record rec in crossed whose `from` covers
// src and whose move toward tgt passes a floor it must list but does not, with
// that floor's crossing as passed.
func unlistedPassage(u *ComponentUpgrades, crossed []crossing, src, tgt *semver.Version) (rec, passed crossing, ok bool) {
	at := onlyVersion(src)
	for _, c := range crossed {
		if !fromCovers(c.tr, inclusiveEnd(src)) {
			continue
		}
		to, err := targetRange(u, c.tr)
		if err != nil {
			continue
		}
		if to.contains(tgt) {
			to = onlyVersion(tgt)
		}
		ps := passagesOf(u, c.tr, at, to)
		if i := slices.IndexFunc(ps, func(p passage) bool { return p.unlistedBy(c.tr) }); i >= 0 {
			return c, ps[i].c, true
		}
	}
	return crossing{}, crossing{}, false
}

// earliestOrigin is the lowest version any of the component's `from` domains
// admits, or nil when one of them reaches down without limit.
func earliestOrigin(u *ComponentUpgrades) *semver.Version {
	if u == nil {
		return nil
	}
	var lowest *semver.Version
	for i := range u.Transitions {
		b, err := parseRange(u.Transitions[i].From)
		if err != nil {
			continue
		}
		if b.lower.unbounded || b.lower.ver == nil {
			return nil
		}
		if lowest == nil || b.lower.ver.Compare(lowest) < 0 {
			lowest = b.lower.ver
		}
	}
	return lowest
}

// claimSpan measures how far a matched record's claim reaches: from the source
// version to the ceiling its `to` names.
func claimSpan(u *ComponentUpgrades, src *semver.Version, t *Transition) Span {
	b, err := targetRange(u, t)
	if err != nil || b.upper.unbounded {
		return Span{}
	}
	return spanBetween(src, b.upper.ver)
}

func spanBetween(a, b *semver.Version) Span {
	switch {
	case a.Major() != b.Major():
		return Span{Majors: levelDiff(a.Major(), b.Major())}
	case a.Minor() != b.Minor():
		return Span{Minors: levelDiff(a.Minor(), b.Minor())}
	case a.Patch() != b.Patch():
		return Span{Patches: levelDiff(a.Patch(), b.Patch())}
	default:
		return Span{}
	}
}

// levelDiff is the magnitude of one level's difference. Semver levels are
// uint64, so the result is clamped rather than converted straight to int, where
// a pathological pin would wrap the count negative.
func levelDiff(a, b uint64) int {
	d := a - b
	if b > a {
		d = b - a
	}
	if d > uint64(math.MaxInt) {
		return math.MaxInt
	}
	return int(d)
}

// breaking reports a boundary semver makes no stability promise across. Below
// 1.0 there is no promise at all, so a minor bump counts there: treating 0.x
// minors as non-breaking would pass an unassessed 0.17.2 to 0.18.1, which is
// ADR-021's own worked example and an entire API-group rename.
//
// A changed prerelease identifier over an otherwise equal release triple counts
// for the same reason one level further down: semver excludes prereleases from
// every guarantee it makes about the release they precede, so they promise
// strictly less than a 0.x minor does. The pin that shipped a CRD migration
// moved only there, and without this an unassessed alpha-to-alpha bump crosses
// no boundary and passes a strict run.
func breaking(a, b *semver.Version) bool {
	switch {
	case a.Major() != b.Major():
		return true
	case a.Major() == 0 && a.Minor() != b.Minor():
		return true
	case a.Minor() == b.Minor() && a.Patch() == b.Patch():
		return a.Prerelease() != b.Prerelease()
	default:
		return false
	}
}
