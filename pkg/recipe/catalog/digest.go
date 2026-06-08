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

package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"

	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// digestAlgoSHA256 is the algorithm key used in SLSA-style digest maps.
const digestAlgoSHA256 = "sha256"

// catalogSubjectName is the AttestSubject.Name reported for catalog signing.
const catalogSubjectName = "recipe-catalog"

// ComputeDigest reads registry.yaml and validators/catalog.yaml through
// provider and returns an attestation.AttestSubject whose Digest is the
// SHA-256 of an injective length-prefixed encoding of the two raw byte
// streams: u64-BE(len(reg)) || reg || u64-BE(len(cat)) || cat.
//
// The combined digest covers the exact shipped bytes (no YAML normalization),
// so it preserves comments and tolerates multi-document YAML — what the
// consumer pulls out of the binary or release archive is what is signed.
// The length prefixes make (registry, catalog) injectively encoded so two
// different file-boundary splits cannot collide on the same combined digest.
// Per-file SHA-256s are recorded in ResolvedDependencies for auditability.
func ComputeDigest(ctx context.Context, provider recipe.DataProvider) (attestation.AttestSubject, error) {
	regBytes, err := provider.ReadFile(ctx, recipe.RegistryFileName)
	if err != nil {
		return attestation.AttestSubject{}, errors.PropagateOrWrap(err, errors.ErrCodeNotFound,
			"failed to read registry file")
	}

	catBytes, err := provider.ReadFile(ctx, recipe.CatalogFileName)
	if err != nil {
		return attestation.AttestSubject{}, errors.PropagateOrWrap(err, errors.ErrCodeNotFound,
			"failed to read catalog file")
	}

	regHash := sha256.Sum256(regBytes)
	catHash := sha256.Sum256(catBytes)

	// Length-prefix each input so the encoding is injective: (regA, catBC) and
	// (regAB, catC) hash differently. sha256.Hash.Write never errors.
	combined := sha256.New()
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(len(regBytes)))
	_, _ = combined.Write(lenBuf[:])
	_, _ = combined.Write(regBytes)
	binary.BigEndian.PutUint64(lenBuf[:], uint64(len(catBytes)))
	_, _ = combined.Write(lenBuf[:])
	_, _ = combined.Write(catBytes)
	combinedHex := hex.EncodeToString(combined.Sum(nil))

	return attestation.AttestSubject{
		Name: catalogSubjectName,
		Digest: map[string]string{
			digestAlgoSHA256: combinedHex,
		},
		ResolvedDependencies: []attestation.Dependency{
			{
				URI:    "file://" + recipe.RegistryFileName,
				Digest: map[string]string{digestAlgoSHA256: hex.EncodeToString(regHash[:])},
			},
			{
				URI:    "file://" + recipe.CatalogFileName,
				Digest: map[string]string{digestAlgoSHA256: hex.EncodeToString(catHash[:])},
			},
		},
	}, nil
}
