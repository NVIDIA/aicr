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

// Package attestation provides bundle attestation using Sigstore keyless signing.
//
// It implements the Attester interface with two implementations:
//   - KeylessAttester: Signs using OIDC-based Fulcio certificates and logs to Rekor.
//     The OIDC token can come from any of the helpers below or be supplied directly
//     by the caller (e.g., a token fetched out of band).
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
// Three flows are exposed for obtaining a Sigstore OIDC identity token; the
// CLI selects one and may also accept a pre-fetched token directly:
//   - FetchAmbientOIDCToken: Uses ACTIONS_ID_TOKEN_REQUEST_URL/TOKEN env vars
//     (GitHub Actions). No browser required.
//   - FetchInteractiveOIDCToken: Opens a browser and binds a localhost
//     redirect callback (default for workstations). Has a 5-minute timeout.
//   - FetchDeviceCodeOIDCToken: OAuth 2.0 Device Authorization Grant
//     (RFC 8628). Works on headless hosts — the user enters a code on a
//     separate device. Has a 5-minute timeout.
//
// Both interactive helpers accept an io.Writer for user-facing prompts (the
// verification URL and code) instead of writing directly to stdout, so the
// package stays usable from non-CLI consumers (pass io.Discard to suppress
// or os.Stderr for typical CLI behavior).
//
// ResolveAttester walks the four-tier source precedence (identity-token →
// ambient → device-flow → interactive) and returns a ready-to-use Attester.
// CLI/API callers should populate ResolveOptions from their own surface
// (flags, env vars, request bodies) and call ResolveAttester rather than
// re-implementing the precedence — the resolver itself reads no environment.
package attestation
