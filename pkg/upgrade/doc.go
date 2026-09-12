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
// machine-readable data: per-component YAML under recipes/upgrades/, referenced
// from registry.yaml via upgrades.file, holding semver-range-keyed transitions
// with a verdict, operator steps grouped by deployer, and the evidence backing
// a safe claim.
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
// # Read-only contract
//
// A Set and everything reachable from it must not be mutated. Consumers share
// the same pointers, and nothing re-runs Validate afterwards.
package upgrade
