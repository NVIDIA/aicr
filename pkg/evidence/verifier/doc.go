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

// Package verifier implements `aicr evidence verify` (ADR-007 PR-B).
//
// This is the first slice — it verifies an unpacked bundle directory
// produced by `aicr validate --emit-attestation`. Three steps run:
//
//  1. Materialize  — resolve the user-supplied directory to a bundle root.
//  2. Inventory    — recompute sha256 of every file listed in
//     manifest.json; transitively binds every file to the predicate.
//  3. Render       — Markdown / JSON; surfaces fingerprint, phase
//     counts, and BOM info from the bundled predicate.
//
// Signature verification, inline constraint replay, OCI pull, and
// pointer-file support ship in follow-up slices. Until the signature
// step lands, the predicate body is read but not cryptographically
// verified — the report calls this out via the Signer field staying
// empty.
//
// ADR-007 §"Verifier steps" enumerates twelve steps; this package
// collapses the redundant ones (per-field digest checks the manifest
// hash chain already covers) and folds display of signed fields into
// the renderer. See cli-reference docs for the full mapping.
package verifier
