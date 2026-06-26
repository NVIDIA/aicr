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

package project

import (
	"crypto/sha256"
	"encoding/hex"
)

// idHashLen is the number of hex characters retained from the signer
// digest. Thirty-two hex chars (128 bits) keeps the on-disk path segment
// compact while staying collision-resistant even against an adversary who
// grinds candidate identities: a 48-bit truncation needs only ~2^24
// attempts (seconds of compute) to manufacture a collision and land a
// hostile signer's runs inside a victim's subtree, whereas 128 bits puts
// both birthday (2^64) and second-preimage work out of reach.
const idHashLen = 32

// idHashSeparator joins the issuer and identity before hashing. A byte
// that cannot appear inside a URL-shaped OIDC issuer or SubjectAlternativeName
// keeps the two fields unambiguously delimited, so ("a", "bc") and
// ("ab", "c") never collide.
const idHashSeparator = "\n"

// SignerIDHash derives the stable source-dedup key for a verified
// signer from its (issuer, identity) pair. It is the contract between
// this producer (GP2 ingest) and the GP4 corroborate consumer, which
// counts consensus by distinct idHashes: the same verified signer must
// hash to the same value across every recipe and every run, and two
// different signers must not collide.
//
// The derivation is the first idHashLen (32) hex characters of
// sha256(issuer + "\n" + identity). It is intentionally simple and
// dependency-free so both sides can reproduce it byte-for-byte; do not
// change the algorithm without coordinating a migration of the GCS
// tree and the consumer.
//
// Inputs must be the *verified* issuer and identity (Fulcio cert SAN +
// OIDC issuer), never a raw, unverified pointer claim.
func SignerIDHash(issuer, identity string) string {
	sum := sha256.Sum256([]byte(issuer + idHashSeparator + identity))
	return hex.EncodeToString(sum[:])[:idHashLen]
}
