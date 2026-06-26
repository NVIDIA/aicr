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

package attestation

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// SourceSlugLength is the number of hex characters in a source slug. 32
// hex chars = 128 bits of the sha256 digest. The slug is an authorization
// key — allowlist.Classify grants slug entries by this value and
// verifier.checkPointerFile uses it for path ownership — so it must be wide
// enough that a deliberate second-preimage collision (a different signer
// engineering an identity that hashes into another party's source directory)
// is infeasible. 128 bits clears that bar; a shorter slug (the original 48)
// did not. It is still short enough for a workable directory name.
const SourceSlugLength = 32

// SourceSlug derives the stable per-source directory slug from a verified
// signer's OIDC issuer + identity. It is the first SourceSlugLength hex
// characters of sha256(issuer + "\n" + identity).
//
// The slug is the <source> path segment in the per-source pointer layout
// recipes/evidence/<recipe>/<source>/<bundle-digest>.yaml (issue #1347,
// Option A). Deriving it deterministically — rather than from a free-form
// label — is what makes the path non-squattable: the CI path-ownership
// check (verifier.CheckEvidenceTree) recomputes the slug from the pointer's
// own verified signer and rejects any file that does not live under the
// directory its signer hashes to. The newline separator is a domain
// separator so ("a\nb", "") and ("a", "b") cannot collide.
//
// The same derivation keys the GP2 ingest tree so a committed community
// pointer and a first-party direct-ingest land under identical source
// identifiers without a side registry.
func SourceSlug(issuer, identity string) (string, error) {
	if issuer == "" || identity == "" {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			"source slug requires non-empty issuer and identity")
	}
	sum := sha256.Sum256([]byte(issuer + "\n" + identity))
	return hex.EncodeToString(sum[:])[:SourceSlugLength], nil
}
