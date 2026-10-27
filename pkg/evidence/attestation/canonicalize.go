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

import "github.com/NVIDIA/aicr/pkg/evidence/canonical"

// The canonicalization and subject digests live in pkg/evidence/canonical, a
// leaf package, so producers outside the evidence pipeline can compute the
// same digest without this package's OCI dependencies. These wrappers keep
// the attestation API unchanged.

// CanonicalizeRecipeYAML sorts recipe YAML mapping keys recursively and
// strips comments; see canonical.CanonicalizeRecipeYAML. New evidence should
// use CanonicalizeRecipeYAMLV3. This form is retained only so already-signed
// V1/V2 evidence keeps verifying under the algorithm it was signed with.
func CanonicalizeRecipeYAML(input []byte) ([]byte, error) {
	return canonical.CanonicalizeRecipeYAML(input)
}

// CanonicalizeRecipeYAMLV3 applies CanonicalizeRecipeYAML, then strips the
// top-level metadata.version field; see canonical.CanonicalizeRecipeYAMLV3.
func CanonicalizeRecipeYAMLV3(input []byte) ([]byte, error) {
	return canonical.CanonicalizeRecipeYAMLV3(input)
}

// SubjectDigest returns the V1/V2 subject digest of a recipe; see
// canonical.SubjectDigest. New evidence should use SubjectDigestV3.
func SubjectDigest(recipeYAML []byte) (string, error) {
	return canonical.SubjectDigest(recipeYAML)
}

// SubjectDigestV3 returns the digest new evidence (PredicateTypeV3) is built
// and verified against; see canonical.SubjectDigestV3.
func SubjectDigestV3(recipeYAML []byte) (string, error) {
	return canonical.SubjectDigestV3(recipeYAML)
}

// SubjectDigestForType returns SubjectDigestV3 for PredicateTypeV3, and
// SubjectDigest for every other recorded type. Callers verifying existing
// evidence must pass the type actually recorded on the bundle being
// checked, never an assumed V3, so historic V1/V2 evidence keeps verifying
// under the algorithm it was signed with.
func SubjectDigestForType(recipeYAML []byte, predicateType string) (string, error) {
	if predicateType == PredicateTypeV3 {
		return SubjectDigestV3(recipeYAML)
	}
	return SubjectDigest(recipeYAML)
}

// DigestOfCanonical hashes already-canonicalized recipe bytes; see
// canonical.DigestOfCanonical.
func DigestOfCanonical(canon []byte) string {
	return canonical.DigestOfCanonical(canon)
}
