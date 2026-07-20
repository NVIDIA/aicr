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

// Package corroborate computes the recipe corroboration consensus model and
// emits the deterministic interim-evidence dashboard (GP4, design doc
// docs/design/013-interim-evidence-dashboard.md).
//
// The model answers one question per recipe row (a CTRF check within a phase):
// how many DISTINCT, verified, allowlisted signers agree on the result? It
// counts signers, never builds — N nightly runs from a single CI loop count as
// one source, so a sybil cannot manufacture a CONFIRMED cell. Each signer's
// latest in-scope CTRF status is bucketed totally and explicitly:
//
//	passed         -> S_pass
//	failed, other  -> S_fail
//	skipped, pending, missing -> NOT-RUN (a coverage gap, never a corroboration)
//
// The five cell states fall out of the (S_pass, S_fail) cardinalities over
// allowlisted signers (see ComputeConsensus): CONFIRMED, SINGLE, CONTESTED,
// FAILING, UNTESTED. A verified-but-unallowlisted signer is admitted as a
// zero-weight "reported" dot that can never reach CONFIRMED on its own.
//
// Each recipe's consensus is baked two ways. The strict per-version grids
// (Tab.Versions, newest-first) count agreement only among runs at the SAME AICR
// version, because a re-run against a different tool release is not a reproduction
// of the same result. The relaxed cross-version grid (Tab.Combined) folds each
// distinct signer's single latest run, version-blind, into one grid — the
// dashboard's default "all versions" view, which surfaces every source that has
// attested the recipe (including sources whose latest run predates the newest
// release). Selecting a specific AICR version switches the renderer to that
// version's strict grid.
//
// The generator (Generate) reads the source-keyed GCS layout (Contract 3:
// results/<group>/<dashboard>/<tab>/<signer-id-hash>/<run-id>/{meta.json,
// ctrf/<phase>.json}) from a local directory, derives each recipe's coordinate
// via the shared pkg/recipe.CoordinateFor helper (never parsing metadata.name),
// and emits the Contract 4 dashboard JSON (index.json + per-recipe
// series/<recipe>.json) plus a self-contained static HTML/CSS/JS renderer that
// fetches them.
//
// Every emit is byte-deterministic from the same inputs: no time.Now, no
// random, no UUID on the emit path. All timestamps come from the bundle
// predicate's AttestedAt (carried in meta.json), and every collection is sorted
// (coordinate, PhaseOrder, CTRF name, signer-id-hash, JSON map keys).
package corroborate
