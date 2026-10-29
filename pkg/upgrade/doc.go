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

// Package upgrade implements ADR-021 component upgrade transition records.
//
// A record answers "is this component version transition safe?" as
// machine-readable data: recipes/components/<component>/upgrades.yaml,
// referenced from registry.yaml via upgrades.file, holding semver-range-keyed
// transitions with a verdict, operator steps grouped by deployer, and the
// evidence backing a safe claim.
//
// # Loading versus validating
//
// Load answers "can I read this?" — decode, the apiVersion and kind gate, and
// the verdict-independent required fields. It fails closed: an unreadable or
// unrecognized record returns ErrCodeInvalidRequest naming what was found and
// what was expected. It is never skipped and never degraded to the unknown
// verdict, because "a record exists and I could not read it" is not "no record
// exists", and collapsing the two hides which action closes the gap.
//
// Set.Validate answers "is this well-formed?" — the pin-relative,
// verdict-dependent, and cross-record rules. It aggregates every violation
// rather than returning the first, so an author sees all of a record's problems
// in one run. The lint gate calls both.
//
// # The ten rules
//
// Rule 1 is Load's. Rules 2 through 10 are Validate's, one function each in
// wellformed.go.
//
//	1  apiVersion and kind are recognized                        decodeRecord
//	2  to is bounded, its ceiling at or below the pin            checkPinCeiling
//	3  the from domains have no hole up to the pin               checkCoverage
//	4  safe names its verifiedBy                                 checkVerdictFields
//	5  manual and blocked carry steps in every group             checkVerdictFields
//	6  deployer groups partition the deployers                   checkStepGroups
//	7  from is forward-only against to                           checkDirectional
//	8  no two transitions share a to floor for the same from     checkDistinctBoundaries
//	9  hooks name a phase and a local manifests/migrations file  checkHooks
//	10 affectedResources name no core Secret                     checkAffectedResources
//
// # Matching
//
// MatchIdentities answers "does this move need attention?" over two
// component-to-identity tables, and is pure: no filesystem, no cluster, no
// registry. Match is the same question for a caller holding versions alone.
//
// An identity moves on two axes. Only the version axis is assessed by anybody,
// because that is what a record describes. A move of namespace, chart, source,
// kustomize path, deployment type, manifest file set, pre-manifest file set or
// object name is invisible to a version comparison yet relocates, replaces or
// renames running objects. Helm cannot move a release between namespaces, and
// it applies a rename as delete-and-recreate, or refuses it outright where an
// object keeps its name while its selector labels change, because
// spec.selector is immutable. So a component that moved on the identity axis
// alone gets a ChangeIdentity row that a version comparison would not report
// at all, a component that moved on both gets one row carrying both, and a
// safe verdict is withdrawn to unknown wherever the identity moved. The record
// vouched for a version hop and was never asked about the rest.
//
// The identity fields do not all read an empty value the same way, which is
// the one thing to hold onto here. An absent scalar field is a fact the
// artifact did not record, so it is not compared. An empty manifest set and an
// absent object name are facts it did record — a chart with no
// fullnameOverride names its objects after itself — so dropping one is a move,
// and reporting it is the point.
//
// On the version axis, a record is *crossed* when the source sits below the
// floor its `to` names and the target reaches it. ComponentResult.Crossed lists
// every crossed record for the at-risk scan.
//
// The verdict follows the source's path. From the source, the matcher takes
// the lowest-floor crossed record whose `from` covers where it stands and lands
// on that record's `to` floor. It repeats until a `to` holds the target or no
// crossed record covers where it stands. A record is on the path when its
// `from` covers a version the walk stood at. Records off the path describe a
// move this operator does not make and do not decide the verdict, unless they
// are blocked and in the way. A record is in the way when its `from` admits a
// version the jump passes before reaching its floor. Records that share a
// floor are ordered by where their `from` starts, lowest first, so declaration
// order decides nothing.
//
// Verdict selection runs in this order:
//
//	1  nothing crossed                                    unknown, or blocked at the highest to ceiling
//	                                                      when the target is past it
//	2  nothing crossed was written for the source         blocked, stop at the lowest
//	3  one record on the path, its to holds the target,   that record's verdict
//	   and no other blocked record in the way
//	4  a blocked record in the way                        blocked, stop at its to
//	5  the path stops short, all safe or one hop asking   blocked, stop at the next record's to,
//	                                                      or the last hop's to when none lies ahead
//	6  two or more records on the path, one asks          blocked, stop where the hop before lands
//
// Rule 3 attaches a Transition even when blocked, because that record's steps
// describe this exact move. Safe records carry no steps, so a path that is all
// safe and reaches the target takes its last hop's verdict. Ahead of rules 5
// and 6, a record on the path that covers the source and holds the target
// takes its own verdict when every other record on the path is safe. Every
// other result leaves Transition nil, so no renderer prints steps for a move
// their record does not describe.
//
// Rule 4 yields to rule 5 when the blocked record is the path's only hop and no
// crossed record lies past where it lands. Rule 5 reports beyond-record-ceiling
// when the path's records never assessed the target, and undefined-origin when
// no record starts where they land.
//
// Where ComponentResult.StoppedAt is the interval not to enter in one step, the
// Explanation names a landing short of it, or asks for a record when nothing
// recorded leads there. When the blocked record covers the source and is the
// path's first hop, it instead says to check the move to that record's floor on
// its own, including where rule 4 yields to rule 5.
//
// Matching takes an already validated Set and does not re-run Validate.
// Validate is therefore not optional: a record that violates a well-formedness
// rule still applies and still lends its verdict. A safe record missing its
// verifiedBy (rule 4) is the case that matters, because it reports safe and
// passes a strict run, which is exactly the false confidence a wrong safe
// buys. Only two malformed shapes are inert here, and only because they leave
// nothing to compare against: ranges that do not parse, and a to naming no
// floor. Callers that did not build the Set through Load plus Validate own
// that gap.
//
// # Reporting
//
// NewReport projects match results into the shape a reader and a CI consumer
// both see: one row per changed component, each semver distance already
// rendered as a phrase, and every step list narrowed to the one deployer named.
// It is a projection rather than an alias because a result points into the Set,
// and a report has to outlive it. WriteTable renders that report; a blocked row
// computed from several records, or from none naming the operator's starting
// point, renders no steps, so its detail block is the Explanation alone.
//
// A report built from a cluster read carries a Source block, which WriteTable
// renders above the rows. A read that recognizes nothing is reported rather
// than failed, so every row under it reads "added"; the block is what
// separates that from a kubeconfig on the wrong context, and it is useless
// below the table a reader has already drawn a conclusion from. The two
// readers are accounted for in separate types whose counts are in different
// units and must never be summed.
//
// An identity row held its version, so its FROM and TO columns carry the fields
// that moved rather than the version printed twice, which is the one rendering
// that would read as nothing having happened. A row that moved on both axes
// keeps its versions in those columns and names the move in its notes, and in
// its detail block where it has one: the steps there were authored for a
// version boundary and neither perform the move nor account for it.
//
// A caller that could not read an axis says so on the Report rather than on
// every row, because an unread axis is one fact about the run and not one fact
// per component. ObjectNamesCompared is that flag, and it is stated rather
// than inferred: every versions-only caller leaves it false, which is the
// reading that claims nothing.
//
// Below the rows, the at-risk section reports objects of the kinds the crossed
// records name that carry no deployer ownership marker. It is the one section
// with no omitempty and no skip: a run that scanned nothing says so, because an
// absent warning reads as an all-clear over resources AICR cannot restore. It
// is advisory throughout, scoped to what the rows actually crossed, and reaches
// neither Summary nor FailsRun; a relocation contributes nothing to it, having
// crossed no boundary at all.
//
// The deployer is not inferred. ADR-021 Decision 5 would take it from a `to`
// bundle, which does record it in bundle-info.yaml, but the check reads that
// file only to locate release values and does not take the deployer from it
// yet, so RequiresDeployer reports when a caller has to supply one. It is true for a
// manual row, and for a blocked row that carries a record; the step-less
// blocked rows do not make it true, because a deployer would name a scope
// nothing renders.
//
// # Bundle guidance
//
// A bundle knows the versions it pins and not the ones a cluster runs, so it
// cannot use Match. BundleNotes instead selects every manual or blocked
// transition whose `to` contains a pin, and WriteGuide, WriteNotice and
// NoteLines render them as guidance conditional on each record's `from`, for
// the one deployer the bundle was built with. Nothing is selected for a safe
// transition, so a bundle whose pins cross only safe boundaries carries none.
//
// # Read-only contract
//
// A Set and everything reachable from it must not be mutated. Consumers share
// the same pointers, and nothing re-runs Validate afterwards.
package upgrade
