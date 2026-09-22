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

// SourceSlug returns the first SourceSlugLength hex characters of
// sha256(issuer + "\n" + identity). The "\n" separator keeps ("a\nb", "")
// and ("a", "b") from colliding.
func SourceSlug(issuer, identity string) (string, error) {
	sum, err := HashIdentityPair(issuer, identity)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(sum[:])[:SourceSlugLength], nil
}

// HashIdentityPair returns sha256(issuer + "\n" + identity). It returns an
// error if issuer or identity is empty. Changing the formula breaks every
// value derived from it, since each is persisted and re-verified across
// runs.
func HashIdentityPair(issuer, identity string) ([sha256.Size]byte, error) {
	if issuer == "" || identity == "" {
		return [sha256.Size]byte{}, errors.New(errors.ErrCodeInvalidRequest,
			"source slug requires non-empty issuer and identity")
	}
	return sha256.Sum256([]byte(issuer + "\n" + identity)), nil
}
