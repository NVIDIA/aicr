// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

// Package attestation provides bundle attestation using Sigstore keyless signing.
//
// It implements the Attester interface with two implementations:
//   - KeylessAttester: Signs using OIDC-based Fulcio certificates and logs to Rekor.
//     Supports both ambient tokens (GitHub Actions) and interactive browser flow.
//   - NoOpAttester: Returns nil (used when --attest is not set).
//
// Attestations use industry-standard formats:
//   - DSSE (Dead Simple Signing Envelope) as the transport format
//   - in-toto Statement v1 as the attestation statement
//   - SLSA Build Provenance v1 as the predicate type
//   - Sigstore bundle (.sigstore.json) packaging the signed envelope,
//     certificate, and Rekor inclusion proof
//
// The attestation subject is checksums.txt (covering all bundle content files).
// The SLSA predicate records build metadata including the tool version, recipe,
// components, and resolvedDependencies (binary provenance + external data files).
//
// # OIDC Token Acquisition
//
// Two paths for obtaining OIDC tokens:
//   - FetchAmbientOIDCToken: Uses ACTIONS_ID_TOKEN_REQUEST_URL/TOKEN env vars
//     (GitHub Actions). No browser required.
//   - FetchInteractiveOIDCToken: Opens browser for Sigstore OIDC authentication
//     (GitHub, Google, or Microsoft accounts). Has a 5-minute timeout.
package attestation
