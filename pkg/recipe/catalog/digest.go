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
	"encoding/hex"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// digestAlgoSHA256 is the algorithm key used in SLSA-style digest maps.
const digestAlgoSHA256 = "sha256"

// ComputeDigest reads registry.yaml and validators/catalog.yaml through
// provider, deterministically re-serializes each, and returns an
// attestation.AttestSubject whose Digest is the SHA-256 of the concatenated
// canonical bytes. ResolvedDependencies holds the individual file digests.
func ComputeDigest(ctx context.Context, provider recipe.DataProvider) (attestation.AttestSubject, error) {
	regBytes, err := deterministicYAML(ctx, provider, recipe.RegistryFileName)
	if err != nil {
		return attestation.AttestSubject{}, errors.PropagateOrWrap(err, errors.ErrCodeNotFound,
			"failed to read registry file")
	}

	catBytes, err := deterministicYAML(ctx, provider, recipe.CatalogFileName)
	if err != nil {
		return attestation.AttestSubject{}, errors.PropagateOrWrap(err, errors.ErrCodeNotFound,
			"failed to read catalog file")
	}

	regHash := sha256.Sum256(regBytes)
	catHash := sha256.Sum256(catBytes)

	combined := sha256.New()
	combined.Write(regBytes)
	combined.Write(catBytes)
	combinedHex := hex.EncodeToString(combined.Sum(nil))

	return attestation.AttestSubject{
		Name: "recipe-catalog",
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

// deterministicYAML reads a YAML file through provider and re-marshals it
// with sorted keys so the output is byte-stable across runs.
func deterministicYAML(ctx context.Context, provider recipe.DataProvider, path string) ([]byte, error) {
	raw, err := provider.ReadFile(ctx, path)
	if err != nil {
		return nil, err
	}
	var v any
	if unmarshalErr := yaml.Unmarshal(raw, &v); unmarshalErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to parse "+path, unmarshalErr)
	}
	data, err := serializer.MarshalYAMLDeterministic(v)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to marshal "+path)
	}
	return data, nil
}
