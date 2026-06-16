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

// Package attestation provides bundle attestation using Sigstore signing.
//
// It implements the Attester interface with three implementations:
//   - KeylessAttester: Signs using OIDC-based Fulcio certificates and logs to Rekor.
//     The OIDC token can come from any of the helpers below or be supplied directly
//     by the caller (e.g., a token fetched out of band).
//   - KMSAttester: Signs with a cloud-KMS-backed key (awskms:// | gcpkms:// |
//     azurekms://) instead of OIDC, for CI/CD environments without an OIDC token.
//     The resulting Sigstore bundle carries public-key verification material (no
//     Fulcio certificate) and, by default, still logs to Rekor.
//   - NoOpAttester: Returns nil (used when --attest is not set).
//
// # Signing Composition
//
// Signing decomposes into two orthogonal, composable axes consumed by the
// SignStatementWith primitive:
//   - SigningIdentity (identity.go): supplies the signing keypair and, for
//     keyless, the Fulcio certificate provider. keylessIdentity uses an
//     ephemeral key + Fulcio; kmsIdentity uses a KMS key and no certificate.
//   - TransparencyPolicy (transparency.go): selects the Rekor transparency log
//     or none (the latter reserved for offline/air-gapped signing, #409).
//
// SignStatement is the keyless specialization (keyless identity + Rekor);
// KMSAttester pairs a KMS identity with Rekor. This keeps the keyless and KMS
// paths a single composition apart rather than parallel code.
//
// HashiCorp Vault (hashivault://) is intentionally unsupported: its client
// libraries are MPL-2.0, which this project's license policy disallows.
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
// # Retry Contract
//
// SignStatementWith wraps the underlying sign.Bundle call with bounded
// exponential-backoff retry to absorb transient Fulcio/Rekor failures
// (e.g., Sigstore Rekor public-good infrastructure slowness — see #1249
// for the failure pattern observed in #1244 and #1245). Each attempt is
// bounded by defaults.SigstoreAttemptTimeout; the outer signing flow is
// bounded by defaults.SigstoreSignTimeout (an upper-bound ceiling that
// covers up to defaults.SigstoreRetryBudget attempts plus
// SigstoreRetryInitialBackoff × SigstoreRetryBackoffFactor^(N-1)
// backoffs between them). Retry semantics:
//
//   - Outer ctx DeadlineExceeded   → ErrCodeTimeout, no further retries
//     (the whole signing budget is gone).
//   - Outer ctx Canceled           → ErrCodeUnavailable, no retries
//     (caller signaled don't-wait).
//   - Per-attempt failure with outer ctx alive → retry on the same
//     content / identity / policy until budget is exhausted, then
//     ErrCodeUnavailable wrapping the last attempt's error.
//
// pkg/defaults.TestSigstoreRetryBudgetInvariant guards the math so a
// future tuning that overflows the SigstoreSignTimeout ceiling fails
// loudly at unit-test time.
//
// # OIDC Token Acquisition
//
// Three flows are exposed for obtaining a Sigstore OIDC identity token; the
// CLI selects one and may also accept a pre-fetched token directly:
//   - FetchAmbientOIDCToken: Uses ACTIONS_ID_TOKEN_REQUEST_URL/TOKEN env vars
//     (GitHub Actions). No browser required. The token request is wrapped in
//     the same defaults.Sigstore* bounded exponential-backoff retry as the
//     signing path: transient transport failures (e.g. a TLS handshake
//     timeout to GitHub's idtoken endpoint) and 5xx responses are retried,
//     while a 4xx or an empty/undecodable token body fails fast (see #1363).
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
// ResolveAttester returns a ready-to-use Attester from ResolveOptions. A
// non-empty SigningKey selects the KMS path (KMSAttester); otherwise it walks
// the four-tier keyless OIDC source precedence (identity-token → ambient →
// device-flow → interactive). CLI/API callers should populate ResolveOptions
// from their own surface (flags, env vars, request bodies) and call
// ResolveAttester rather than re-implementing the precedence — the resolver
// itself reads no environment.
package attestation
