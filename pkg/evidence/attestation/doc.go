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

// Package attestation implements the recipe-test-attestation evidence
// kind defined in ADR-007 and docs/spec/recipe-evidence-v1.md.
//
// A recipe-test-attestation bundle is a signed, content-addressed
// artifact that ties an AICR recipe to an `aicr validate` run on real
// hardware. The signed payload is an in-toto Statement carrying a
// custom predicate (predicateType
// https://aicr.nvidia.com/recipe-evidence/v1); the supporting files
// (recipe, snapshot, BOM, CTRF, manifest) ship alongside in an OCI
// artifact for reviewer convenience and offline verification.
//
// The package is layered:
//
//   - types.go              Predicate, Pointer, Manifest, Bundle structs.
//   - canonicalize.go       Recipe canonicalization for subject digest.
//   - manifest.go           Per-file sha256 inventory; closes integrity chain.
//   - predicate.go          Build the v1 predicate body from inputs.
//   - builder.go            Build(ctx, opts) writes the bundle directories.
//   - signer.go             Cosign keyless signing of the in-toto Statement.
//   - oci.go                Push bundle as an OCI artifact via oras-go.
//   - pointer.go            Generate the recipes/evidence/<recipe>.yaml pointer.
//
// Cluster fingerprint and criteria matching are delegated to
// pkg/fingerprint (FromMeasurements + Fingerprint.Match), whose types
// the Predicate uses directly so the on-the-wire schema stays in lock-
// step with what fingerprint.Fingerprint and MatchResult serialize to.
//
// The verifier (`aicr verify-evidence`, #753) consumes the bundle but
// is not implemented here — this package only emits.
package attestation
