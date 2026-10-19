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
	"encoding/hex"

	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

// idHashLen is the number of hex characters retained from the signer
// digest. Thirty-two hex chars (128 bits) keeps the on-disk path segment
// compact while staying collision-resistant even against an adversary who
// grinds candidate identities: a 48-bit truncation needs only ~2^24
// attempts (seconds of compute) to manufacture a collision and land a
// hostile signer's runs inside a victim's subtree, whereas 128 bits puts
// both birthday (2^64) and second-preimage work out of reach.
const idHashLen = 32

// SignerIDHash returns the first idHashLen hex characters of
// attestation.HashIdentityPair(issuer, identity), the stable dedup key for
// a verified signer's (issuer, identity) pair. issuer and identity must be
// the verified values (Fulcio cert SAN and OIDC issuer), never an
// unverified pointer claim. Changing the algorithm breaks every
// already-persisted value derived from it.
func SignerIDHash(issuer, identity string) string {
	// A verified (issuer, identity) pair is never empty, so
	// HashIdentityPair's only error case cannot occur here.
	sum, _ := attestation.HashIdentityPair(issuer, identity)
	return hex.EncodeToString(sum[:])[:idHashLen]
}
