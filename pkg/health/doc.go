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

// Package health computes per-recipe structural health across the whole
// criteria matrix, as specified by ADR-009 (Recipe Health Tracking).
//
// # Overview
//
// Compute enumerates every leaf recipe in the catalog and scores each
// against a set of hermetic, offline, deterministic signals — reproducible
// from a checkout with no GPU, no network, and no wall-clock. The V1 core
// (this package) ships the enumeration→resolution loop, the graded resolves
// signal, the per-recipe status rollup, and a byte-deterministic report.
//
// # Standalone package
//
// health is a standalone package rather than living in pkg/recipe to keep a
// future evidence-driven validation axis acyclic: pkg/evidence/attestation
// already imports pkg/recipe, so hosting health in pkg/recipe would later
// close a recipe → evidence → recipe import cycle once health consumes
// evidence. A standalone package depending on both stays acyclic.
//
// # Signals and rollup
//
// Each leaf is scored on graded dimensions whose per-dimension state is one
// of pass, warn, fail, or unknown. The resolves dimension (this package) is
// graded from whether the recipe builder resolves the criteria without
// error. Additional dimensions (chart_pinned, declared_coverage,
// constraints_wellformed) are layered on by later work and feed the same
// generic rollup without changing rollup logic.
//
// The rolled-up per-recipe status is fail if any graded dimension fails,
// else warn if any warns, else unknown if any is held (transient resolver
// errors are held rather than penalized — re-run rather than mark fail),
// else pass. unknown is a representable value, never an empty field a
// consumer could misread as pass.
//
// # Determinism
//
// There is no time.Now() on the computed path, so output is a pure function
// of the checkout. Report carries map-typed fields whose nondeterminism
// only manifests at marshal time; callers serializing the report must use
// serializer.MarshalYAMLDeterministic. The sole non-deterministic input is
// the unknown-from-transient mapping, which depends on real timeouts; tests
// inject errors via a fake DataProvider and assert byte-determinism on the
// non-error path only.
package health
