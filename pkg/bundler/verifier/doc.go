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

// Package verifier implements offline bundle verification with a four-level
// trust model.
//
// # Trust Levels
//
// Verification produces one of four trust levels (highest to lowest):
//
//   - verified: Checksums valid, bundle attestation verified, binary attestation
//     verified and identity-pinned to NVIDIA CI, no external data.
//   - attested: Full chain verified but external data (--data) was used, capping
//     trust because the data's own provenance is unknown.
//   - unverified: Checksums valid but no attestation files (--attest not used).
//   - unknown: Missing or invalid checksums, or attestation verification failed.
//
// # Verification Chain
//
// Verify performs a five-step offline verification:
//
//  1. Read and verify checksums.txt (single read to prevent TOCTOU)
//  2. Check for bundle attestation file
//  3. Verify bundle attestation against trusted root, binding to checksums.txt
//     digest and requiring a valid OIDC-issued certificate
//  4. Check for binary attestation file
//  5. Verify binary attestation with identity pinning to NVIDIA CI and binding
//     to the running binary's digest
//
// All verification is fully offline using the locally cached or embedded
// Sigstore trusted root. No network calls are made during verification.
//
// # Identity Pinning
//
// Binary attestation verification pins to NVIDIA's GitHub Actions OIDC issuer
// and a repository pattern matching NVIDIA/aicr workflows. This ensures the
// binary was built by NVIDIA CI. The pattern can be overridden via
// VerifyOptions.CertificateIdentityRegexp but must always contain the
// github.com/NVIDIA/aicr/ prefix.
package verifier
