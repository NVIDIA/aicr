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
// # The nine rules
//
// Rule 1 is Load's; rules 2 through 9 are Validate's, one function each in
// wellformed.go. A reader told there are nine rules otherwise finds eight,
// numbered 2 to 9, because rule 1 is named nowhere in the package.
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
//
// # Matching
//
// Match answers "does this jump need attention?" over two component-to-version
// tables, and is pure: no filesystem, no cluster, no registry.
//
// A record is *crossed* when the source sits below the floor its `to` names and
// the target reaches it. Crossing is a property of the jump alone; `from` is
// not consulted, because a record whose `from` excludes the source still
// describes a boundary the jump flies over, and skipping it there is how a
// recorded block goes unreported. `from` answers the separate question of
// whether that record's guidance was authored for this starting point.
//
// Verdict selection runs in this order:
//
//	1  nothing crossed                              unknown
//	2  one crossed, from covers the source          that record's verdict
//	3  another crossed record authored blocked      blocked, stop at its to
//	4  two or more crossed                          blocked, stop at the lowest
//	5  one crossed, from does not cover the source  blocked, stop at its to
//
// Rule 2 is the only one that attaches a Transition, and it attaches one for
// every verdict including blocked: that record describes this exact move, so
// its blocked verdict means "not in one step" and its steps say what to do
// instead. Rules 3, 4 and 5 leave Transition nil, so no renderer can print one
// record's steps for a jump that record does not describe. Rule 5 is the
// outside-every-recorded-origin case, usually below the lowest `from` floor:
// nothing describes an upgrade from where the operator is, and an opt-in check
// errs toward safety there. Every blocked result names a StoppedAt, and every
// result carries a Reason code and an Explanation sentence saying which rule it
// was and what to do about it.
//
// Match takes an already validated Set and does not re-run Validate. Validate
// is therefore not optional: a record that violates a well-formedness rule
// still applies and still lends its verdict. A safe record missing its
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
// The deployer cannot be inferred. ADR-021 Decision 5 would take it from a `to`
// bundle, but no bundle artifact records which deployer built it, so
// RequiresDeployer reports when a caller has to supply one. It is true for a
// manual row, and for a blocked row that carries a record; the step-less
// blocked rows do not make it true, because a deployer would name a scope
// nothing renders.
//
// # Read-only contract
//
// A Set and everything reachable from it must not be mutated. Consumers share
// the same pointers, and nothing re-runs Validate afterwards.
package upgrade
