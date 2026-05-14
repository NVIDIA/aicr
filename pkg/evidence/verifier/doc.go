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

// Package verifier implements `aicr evidence verify`: offline
// verification of a recipe-evidence v1 bundle directory produced by
// `aicr validate --emit-attestation`. Four steps run:
//
//  1. Materialize — resolve the user-supplied directory to a bundle root.
//  2. Predicate parse — read the in-toto Statement; reject unknown
//     predicate types.
//  3. Manifest hash check — sha256(manifest.json) must match
//     predicate.Manifest.Digest, and every file the manifest names
//     must match its recorded sha256. Together these transitively
//     bind every bundled file to the predicate.
//  4. Render — Markdown / JSON; surfaces fingerprint, phase counts,
//     and BOM info from the bundled predicate.
//
// The predicate body is read but not yet cryptographically verified —
// the rendered report surfaces this via an empty Signer line. See
// docs/design/007-recipe-evidence.md for the trust model.
package verifier
