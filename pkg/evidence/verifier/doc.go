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
// verification of a recipe-evidence v1 bundle produced by
// `aicr validate --emit-attestation`. Five steps run:
//
//  1. Materialize — resolve the input (directory / pointer file / OCI
//     reference) to a bundle root on disk. Pointer and OCI forms pull
//     the artifact via ORAS, then discover the Sigstore Bundle attached
//     as an OCI Referrer and stage it as attestation.intoto.jsonl so
//     the signature step finds it on disk the same way it does for
//     directory input.
//  2. Signature verify — when attestation.intoto.jsonl is present,
//     sigstore-go verifies the DSSE-wrapped in-toto Statement against
//     the Sigstore trusted root (Fulcio cert chain, optional Rekor
//     entry). The cryptographically anchored predicate body is
//     extracted from the verified payload.
//  3. Predicate parse — use the verified predicate when the signature
//     step produced one; otherwise fall back to the unsigned
//     statement.intoto.json (self-consistency only).
//  4. Manifest hash check — sha256(manifest.json) must match
//     predicate.Manifest.Digest, and every file the manifest names
//     must match its recorded sha256. Together these transitively
//     bind every bundled file to the (now signature-anchored)
//     predicate.
//  5. Render — Markdown / JSON; surfaces signer identity, fingerprint,
//     phase counts, and BOM info.
//
// The trust chain when a signature is present:
//
//	Sigstore trusted root → Fulcio cert (Rekor-logged)
//	  → DSSE-signed Statement
//	    → predicate.Manifest.Digest
//	      → manifest.json
//	        → every bundled file's sha256
//
// Tampering anywhere below the signature breaks the chain. The OCI
// input form adds a freshness check: the signed Statement's subject
// digest is locked to the pulled artifact's OCI manifest digest, so a
// substituted artifact paired with a stale signature fails too.
//
// See docs/design/007-recipe-evidence.md for the full trust model.
package verifier
