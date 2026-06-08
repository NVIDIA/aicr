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

// Package catalog provides signing and verification for the AICR recipe
// catalog (registry.yaml + validators/catalog.yaml). It reuses the
// pkg/bundler/attestation and pkg/bundler/verifier primitives so catalog
// attestations are structurally identical to bundle attestations — same
// Sigstore bundle format, same SLSA predicate, different buildType URI
// (https://aicr.nvidia.com/recipe-catalog/v1).
//
// Sign computes a deterministic SHA-256 over the merged catalog content,
// builds an in-toto statement, and delegates to an attestation.Attester
// (keyless OIDC in CI, NoOpAttester in tests).
//
// Verify recomputes the same digest and calls
// verifier.VerifyBinaryAttestation so the consumer gets the same identity-
// pinned verification path used for binary attestations.
package catalog
