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
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/Masterminds/semver/v3"
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
// here is invisible to a version comparison but relocates or renames running
// objects, and Helm can do neither in place.
type Identity struct {
	Version   string
	Namespace string

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

// IdentityChange names one field that moved between the compared artifacts.
//
// Field is either an Identity field's name lowercased ("namespace") or a dotted
// value path into the merged values ("fullnameOverride",
// "grafana.fullnameOverride"), so a consumer branches on it without parsing
// prose. Version is never one of them: the version axis is
// ComponentResult.From and To, and the records assess it.
//
// From or To is empty where the move is an object name appearing or
// disappearing, which is a rename rather than a missing observation. Namespace
// never reports an empty side; see identityChanges for why the two differ.
type IdentityChange struct {
	Field string
	From  string
	To    string
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

	// Transition is the record that describes this exact move: one crossed
	// record whose `from` covers the source. It is nil wherever no single
	// record does, including the blocked results computed from several records
	// or from none that names this starting point, so a renderer cannot print
	// one record's steps for a jump that record does not describe.
	Transition *Transition

	// Replaces is the arriving component's declaration, set only on a
	// ChangeReplaced row.
	Replaces *Replaces

	// StoppedAt is the `to` range a blocked jump stops at: the interval it must
	// not enter in one step. Set on every blocked version transition, and empty
	// on every other result including a blocked ChangeReplaced row, where two
	// pieces of software share no version line for a boundary to sit on.
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
// On the version axis a record is crossed when the source sits below the floor
// its `to` names and the target reaches it. Verdict selection then runs in this
// order: nothing crossed is unknown; one crossed record whose `from` covers the
// source lends its verdict, blocked included, because it describes this exact
// move, unless the target lands past the ceiling that record's `to` names,
// which blocks the jump at that ceiling; any other crossed record authored
// blocked blocks the jump; two or more crossed records block it; one crossed
// record whose `from` does not cover the source blocks it, because nothing
// describes an upgrade from where the operator is. Every result carries a
// Reason and an Explanation saying which of those it was.
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
	sort.Strings(names)

	results := make([]ComponentResult, 0, len(names))
	for _, name := range names {
		src, inFrom := from[name]
		tgt, inTo := to[name]
		switch {
		case inFrom && inTo:
			moved := identityChanges(src, tgt)
			// Versions compared as written rather than as parsed semver: two
			// pins differing only in build metadata order as equal, and
			// silence there reads as safe.
			if src.Version == tgt.Version {
				if len(moved) == 0 {
					continue
				}
				results = append(results, relocation(name, src.Version, moved))
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

// identityChanges lists the non-version fields that moved, namespace first and
// then the object-name paths in sorted order.
//
// The two axes treat an empty value oppositely, and the difference is what each
// absence means. A namespace counts only when both sides state it: an empty
// string is a fact the artifact did not carry rather than a move to the default
// namespace, so treating it as a value would report a relocation nobody
// performed for every component the moment one artifact stops carrying the
// field. An absent object name is the other thing entirely — a chart with no
// fullnameOverride names its objects after itself — so dropping one renames
// every object it held, and both directions are moves.
func identityChanges(from, to Identity) []IdentityChange {
	var moved []IdentityChange
	if from.Namespace != "" && to.Namespace != "" && from.Namespace != to.Namespace {
		moved = append(moved, IdentityChange{
			Field: "namespace",
			From:  from.Namespace,
			To:    to.Namespace,
		})
	}
	paths := make([]string, 0, len(from.ObjectNames)+len(to.ObjectNames))
	for path := range from.ObjectNames {
		paths = append(paths, path)
	}
	for path := range to.ObjectNames {
		if _, inBoth := from.ObjectNames[path]; !inBoth {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
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
// covers the move, and what the axes that moved will actually do.
//
// The clauses are selected rather than fixed. The two axes fail differently —
// a relocation leaves a second copy running, a rename replaces objects — and
// this sentence is what an operator reads at the moment they decide whether to
// upgrade, so naming the wrong consequence sends them to the wrong remedy.
//
// The selector clause is deliberately conditional. fullnameOverride renames
// objects without touching the selector in the standard chart scaffold;
// nameOverride feeds app.kubernetes.io/name, which does sit in spec.selector.
// Which applies is a property of the chart, so the text says where to look
// rather than promising a failure that may not happen.
func identityAdvice(moved []IdentityChange) string {
	parts := []string{"Transition records assess version boundaries, so none assesses this move."}
	if movedNamespace(moved) {
		parts = append(parts, "Helm cannot move a release between namespaces, so applying the new "+
			"recipe installs a second copy beside the running one.")
	}
	if movedObjectName(moved) {
		parts = append(parts, "Renaming the objects a chart owns is applied as delete-and-recreate, "+
			"so expect a service gap, and an orphan for anything referenced by name or not owned by "+
			"the release. Where the moved key feeds the chart's selector labels, spec.selector is "+
			"immutable and the upgrade fails outright instead.")
	}
	return strings.Join(append(parts, "Make the move deliberately, then re-run this check."), " ")
}

// movedNamespace reports whether the component was relocated.
func movedNamespace(moved []IdentityChange) bool {
	for _, c := range moved {
		if c.Field == identityFieldNamespace {
			return true
		}
	}
	return false
}

// movedObjectName reports whether any object the chart owns was renamed. Every
// field that is not the namespace is a value path, so this is the complement.
func movedObjectName(moved []IdentityChange) bool {
	for _, c := range moved {
		if c.Field != identityFieldNamespace {
			return true
		}
	}
	return false
}

// identityFieldNamespace is the one IdentityChange.Field value that is not a
// dotted value path.
const identityFieldNamespace = "namespace"

// relocation is the row for a component that held its version and moved anyway.
//
// Unknown rather than blocked, for the reason that separates the two: nobody
// was asked. A block is an author's judgement, and no author can record one
// here, so this is a gap in what the vocabulary covers rather than a boundary
// somebody drew.
func relocation(name, version string, moved []IdentityChange) ComponentResult {
	return ComponentResult{
		Component:       name,
		Change:          ChangeIdentity,
		From:            version,
		To:              version,
		IdentityChanges: moved,
		Verdict:         VerdictUnknown,
		Reason:          ReasonIdentityChanged,
		Explanation: fmt.Sprintf("%s %s, but %s. %s",
			name, heldPhrase(version), movedPhrase(moved), identityAdvice(moved)),
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
	sort.Strings(names)

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

	crossed := crossings(u, src, tgt)
	if len(crossed) == 0 {
		return unmatched(u, r, tgt)
	}
	// One record, authored for this starting point and reaching the target:
	// its verdict stands whatever it is. A blocked verdict here is that record
	// saying "not in one step", and its own steps say what to do instead, so
	// the result carries it and a renderer can show them.
	if len(crossed) == 1 && fromCovers(crossed[0].tr, src) {
		only := crossed[0]
		if ceiling, past := beyondCeiling(only.tr, tgt); past {
			// The record describes this starting point but stops assessing
			// before the target, so lending its verdict would reach forward
			// over ground nobody read the migration notes for. Rule 2 forbids
			// the same reach at authoring time; this is it at match time.
			r.Verdict = VerdictBlocked
			r.Reason = ReasonBeyondRecordCeiling
			r.StoppedAt = only.tr.To
			r.Explanation = beyondCeilingExplanation(ceiling, r.To)
			return r
		}
		r.Verdict = only.tr.Verdict
		r.Transition = only.tr
		r.Span = claimSpan(src, only.tr)
		r.Reason = ReasonRecorded
		r.Explanation = recordedExplanation(r, only)
		if r.Verdict == VerdictBlocked {
			r.StoppedAt = only.tr.To
		}
		return r
	}
	if blocking, ok := lowestBlocked(crossed); ok {
		// A block reached from somewhere its author did not describe, or
		// alongside other boundaries, stops the jump regardless: an authored
		// blocked verdict anywhere in the way is still a block.
		r.Verdict = VerdictBlocked
		r.Reason = ReasonRecordBlocks
		r.StoppedAt = blocking.tr.To
		r.Explanation = fmt.Sprintf(
			"blocked by the record covering %s: do not move from %s into %s in one step. "+
				"Upgrade to %s first, then re-run this check",
			blocking.floor.ver, r.From, blocking.tr.To, blocking.floor.ver)
		return r
	}
	if len(crossed) > 1 {
		// A safe boundary asks nothing of the operator, so crossing it composes
		// nothing and skips nothing. When it is the only thing standing beside
		// one substantive record that does describe this jump, defer to that
		// record rather than stopping a move whose extra boundary is "nothing
		// to do". Reached only after the blocked checks above, so an authored
		// block still outranks everything here.
		// N safe boundaries compose exactly as one does: none carries steps,
		// so there is no work to skip and no origin whose guidance could be
		// wrong. Coverage (rule 3) is what makes the chain trustworthy, since
		// it leaves no hole below the pin, so the only question left is
		// whether the assessment reaches the target.
		// crossings orders by floor, so crossed[0] is the boundary that has to
		// own the origin while top is the one that has to reach the target.
		// Both must hold: skipping the origin check let a jump from a version
		// no record assessed come back safe purely because it crossed two
		// boundaries instead of one.
		if top, ok := everySafeCrossing(crossed); ok && fromCovers(crossed[0].tr, src) {
			if _, past := beyondCeiling(top.tr, tgt); !past {
				r.Verdict = VerdictSafe
				r.Transition = top.tr
				r.Span = claimSpan(src, top.tr)
				r.Reason = ReasonRecorded
				r.Explanation = recordedExplanation(r, top)
				return r
			}
		}
		if only, ok := loneSubstantive(crossed); ok && fromCovers(only.tr, src) {
			if _, past := beyondCeiling(only.tr, tgt); !past {
				r.Verdict = only.tr.Verdict
				r.Transition = only.tr
				r.Span = claimSpan(src, only.tr)
				r.Reason = ReasonRecorded
				r.Explanation = recordedExplanation(r, only)
				return r
			}
		}
		// Composing both records' steps would be wrong rather than merely
		// cautious: an intermediate record's work never runs on a jump straight
		// past it, so the report names where to stop instead.
		r.Verdict = VerdictBlocked
		r.Reason = ReasonMultipleBoundaries
		r.StoppedAt = crossed[0].tr.To
		r.Explanation = fmt.Sprintf(
			"crosses %d recorded boundaries (%s); no single record describes the whole jump. "+
				"Upgrade to %s first, then re-run this check",
			len(crossed), strings.Join(floorNames(crossed), ", "), crossed[0].floor.ver)
		return r
	}

	// Nothing describes an upgrade from where the operator actually is. The
	// tool is opt-in, so it errs toward safety rather than lending a verdict
	// authored for a starting point this jump never had.
	only := crossed[0]
	r.Verdict = VerdictBlocked
	r.Reason = ReasonUndefinedOrigin
	r.StoppedAt = only.tr.To
	r.Explanation = undefinedOriginExplanation(u, r, only, src)
	return r
}

// everySafeCrossing returns the boundary nearest the target when every crossed
// boundary is safe, so the caller can lend that verdict instead of blocking a
// jump across boundaries that each ask nothing. crossings orders by floor, so
// the last is the one whose ceiling has to reach the target.
//
// If one safe boundary composes nothing and skips nothing, N of them compose
// nothing either: rule 4 forbids a safe record carrying steps, so there is no
// intermediate work a jump past it could miss. Blocking such a jump names a
// stopping point where nothing happens, which is the false-confidence
// direction rather than the cautious one.
//
// This answers only "does anything ask something of the operator". The origin
// and the ceiling are separate questions the call site still has to ask, and
// conflating them is a mistake worth naming: a safe record carrying no steps
// says nothing about whether the starting version was ever assessed, which is
// what undefined-origin is about.
func everySafeCrossing(crossed []crossing) (crossing, bool) {
	if len(crossed) == 0 {
		return crossing{}, false
	}
	for _, c := range crossed {
		if c.tr.Verdict != VerdictSafe {
			return crossing{}, false
		}
	}
	return crossed[len(crossed)-1], true
}

// loneSubstantive returns the single crossed boundary that asks something of
// the operator, when every other one crossed is safe. A safe record carries no
// steps by construction (rule 4 forbids them), so it is never the skipped
// migration multiple-boundaries exists to prevent.
func loneSubstantive(crossed []crossing) (crossing, bool) {
	var only crossing
	found := 0
	for _, c := range crossed {
		if c.tr.Verdict == VerdictSafe {
			continue
		}
		only, found = c, found+1
	}
	return only, found == 1
}

// crossing pairs a transition with the floor its `to` names.
type crossing struct {
	tr    *Transition
	floor bound
}

// crossings returns every transition the jump crosses, ordered by that floor.
// The lowest comes first because that is the boundary a blocked jump has to
// stop at, which declaration order does not track.
func crossings(u *ComponentUpgrades, src, tgt *semver.Version) []crossing {
	if u == nil {
		return nil
	}
	out := make([]crossing, 0, len(u.Transitions))
	for i := range u.Transitions {
		floor, ok := crosses(&u.Transitions[i], src, tgt)
		if !ok {
			continue
		}
		out = append(out, crossing{tr: &u.Transitions[i], floor: floor})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return lowerBefore(out[i].floor, out[j].floor)
	})
	return out
}

// crosses reports whether the jump passes the boundary a transition describes,
// and returns the floor its `to` names.
//
// Crossing is a property of the jump alone: the source sits below the floor and
// the target reaches it. `from` is deliberately not consulted, because a record
// whose `from` excludes the source still describes a boundary the jump flies
// over, and skipping it there is how a recorded block goes unreported. Whether
// that record's *guidance* was authored for this starting point is fromCovers'
// separate question.
func crosses(t *Transition, src, tgt *semver.Version) (bound, bool) {
	if _, err := parseBounds(t.From); err != nil {
		// An unparseable `from` leaves no way to tell whose guidance this is,
		// so the record stays inert rather than blocking every jump past it.
		return bound{}, false
	}
	toRange, err := parseBounds(t.To)
	if err != nil {
		return bound{}, false
	}
	// A `to` with no floor names no boundary at all, so there is nothing to
	// cross. Rule 2 rejects the shape, but Match also runs on Sets that never
	// met Validate.
	if toRange.lower.unbounded {
		return bound{}, false
	}
	if cmp := src.Compare(toRange.lower.ver); cmp > 0 || (cmp == 0 && toRange.lower.inclusive) {
		return bound{}, false
	}
	cmp := tgt.Compare(toRange.lower.ver)
	if cmp < 0 || (cmp == 0 && !toRange.lower.inclusive) {
		return bound{}, false
	}
	return toRange.lower, true
}

// beyondCeiling reports whether the target lands past the ceiling a
// transition's `to` names, and returns that ceiling.
//
// A `to` with no ceiling reaches forward without limit, so nothing is past it,
// and an unparseable one never became a crossing in the first place. A target
// exactly at an inclusive ceiling is covered; at an exclusive one it is not.
func beyondCeiling(t *Transition, tgt *semver.Version) (bound, bool) {
	b, err := parseBounds(t.To)
	if err != nil || b.upper.unbounded || b.upper.ver == nil {
		return bound{}, false
	}
	return b.upper, pastBound(b.upper, tgt)
}

// pastBound reports whether v sits above a bounded ceiling. A version exactly
// at an inclusive ceiling is covered by it; at an exclusive one it is not.
func pastBound(ceiling bound, v *semver.Version) bool {
	cmp := v.Compare(ceiling.ver)
	return cmp > 0 || (cmp == 0 && !ceiling.inclusive)
}

// ceilingPhrase names the highest version a ceiling actually covers, as a place
// an operator can be told to stop. The bare number will not do: "<0.19.0" and
// "<=0.19.0" end at different versions, and only one of them is 0.19.0 itself.
func ceilingPhrase(b bound) string {
	if b.inclusive {
		return b.ver.String()
	}
	return "the last version below " + b.ver.String()
}

// fromCovers reports whether a transition's guidance was authored for this
// starting point.
func fromCovers(t *Transition, src *semver.Version) bool {
	b, err := parseBounds(t.From)
	if err != nil {
		return false
	}
	return b.contains(src)
}

// lowestBlocked returns the lowest-floor crossed transition an author marked
// blocked. crossings is already ordered by floor, so the first match is it.
func lowestBlocked(crossed []crossing) (crossing, bool) {
	for _, c := range crossed {
		if c.tr.Verdict == VerdictBlocked {
			return c, true
		}
	}
	return crossing{}, false
}

func floorNames(crossed []crossing) []string {
	names := make([]string, len(crossed))
	for i, c := range crossed {
		names[i] = c.floor.ver.String()
	}
	return names
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
func unmatched(u *ComponentUpgrades, r ComponentResult, tgt *semver.Version) ComponentResult {
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
		if top, ceiling, ok := highestCeiling(u); ok && pastBound(ceiling, tgt) {
			r.Verdict = VerdictBlocked
			r.Reason = ReasonBeyondRecordCeiling
			r.StoppedAt = top.To
			r.Explanation = beyondCeilingExplanation(ceiling, r.To)
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
func beyondCeilingExplanation(ceiling bound, target string) string {
	landing := ceilingPhrase(ceiling)
	return fmt.Sprintf(
		"transition records for this component assess only as far as %s, and %s lands past that, "+
			"so nothing assesses this move or anything above %s. Upgrade no further than %s and "+
			"re-run this check, or widen a record's `to` range to cover %s",
		landing, target, landing, landing, target)
}

// highestCeiling returns the transition whose `to` reaches furthest forward and
// the ceiling it names. ok is false when no ceiling bounds the coverage, either
// because a record reaches forward without limit or because none parsed.
func highestCeiling(u *ComponentUpgrades) (*Transition, bound, bool) {
	if u == nil {
		return nil, bound{}, false
	}
	var (
		top     *Transition
		highest bound
	)
	for i := range u.Transitions {
		b, err := parseBounds(u.Transitions[i].To)
		if err != nil {
			continue
		}
		if b.upper.unbounded || b.upper.ver == nil {
			return nil, bound{}, false
		}
		if top == nil || upperAfter(b.upper, highest) {
			top, highest = &u.Transitions[i], b.upper
		}
	}
	if top == nil {
		return nil, bound{}, false
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

func undefinedOriginExplanation(u *ComponentUpgrades, r ComponentResult, c crossing, src *semver.Version) string {
	s := fmt.Sprintf("crosses the boundary at %s, but no record describes an upgrade starting from %s",
		c.floor.ver, r.From)
	if origin := earliestOrigin(u); origin != nil && src.Compare(origin) < 0 {
		return fmt.Sprintf("%s; the earliest recorded starting point is %s. "+
			"Upgrade to a recorded version first, then re-run this check", s, origin)
	}
	return s + ", so nothing covers this move. Author a record for this starting point, then re-run this check"
}

// earliestOrigin is the lowest version any of the component's `from` domains
// admits, or nil when one of them reaches down without limit.
func earliestOrigin(u *ComponentUpgrades) *semver.Version {
	if u == nil {
		return nil
	}
	var lowest *semver.Version
	for i := range u.Transitions {
		b, err := parseBounds(u.Transitions[i].From)
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
func claimSpan(src *semver.Version, t *Transition) Span {
	b, err := parseBounds(t.To)
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
