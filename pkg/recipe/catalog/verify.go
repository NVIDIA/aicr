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
	"encoding/hex"
	"os"

	"github.com/NVIDIA/aicr/pkg/bundler/verifier"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// VerifyOptions configures a Verify call.
type VerifyOptions struct {
	// CertificateIdentityRegexp overrides the default NVIDIA CI identity
	// pinning pattern. Must contain "NVIDIA/aicr". Defaults to
	// verifier.TrustedRepositoryPattern when empty.
	CertificateIdentityRegexp string
}

// VerifyResult is returned by Verify on success.
type VerifyResult struct {
	// Identity is the SAN claim from the signing certificate.
	Identity string

	// Digest is the hex-encoded SHA-256 of the catalog content that was verified.
	Digest string
}

// Verify recomputes the catalog digest from provider and verifies it against
// the Sigstore bundle at bundlePath using identity pinning to NVIDIA CI.
func Verify(ctx context.Context, bundlePath string, provider recipe.DataProvider, opts VerifyOptions) (*VerifyResult, error) {
	if _, err := os.Stat(bundlePath); err != nil {
		if os.IsNotExist(err) {
			return nil, errors.New(errors.ErrCodeNotFound, "catalog bundle not found: "+bundlePath)
		}
		return nil, errors.Wrap(errors.ErrCodeInternal, "cannot access catalog bundle", err)
	}

	subject, err := ComputeDigest(ctx, provider)
	if err != nil {
		return nil, err
	}

	digestHex := subject.Digest[digestAlgoSHA256]
	digestBytes, err := hex.DecodeString(digestHex)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "invalid catalog digest", err)
	}

	identityPattern := opts.CertificateIdentityRegexp
	if identityPattern == "" {
		identityPattern = verifier.TrustedRepositoryPattern
	}
	// Enforce the NVIDIA/aicr repo-prefix invariant the field's godoc promises.
	// Without this, --identity-pattern '.*' (or env override) silently disables
	// repo pinning — leaving only the OIDC issuer pin, which any GitHub Actions
	// workflow satisfies.
	if validateErr := verifier.ValidateIdentityPattern(identityPattern); validateErr != nil {
		return nil, errors.PropagateOrWrap(validateErr, errors.ErrCodeInvalidRequest,
			"invalid catalog identity pattern")
	}

	identity, err := verifier.VerifyBinaryAttestation(ctx, bundlePath, identityPattern, digestBytes)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeUnauthorized, "catalog attestation verification failed")
	}

	return &VerifyResult{
		Identity: identity,
		Digest:   digestHex,
	}, nil
}
