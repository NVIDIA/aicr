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

package verifier

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/trust"
)

// Identity pinning constants for NVIDIA CI.
const (
	TrustedOIDCIssuer        = "https://token.actions.githubusercontent.com"
	TrustedRepositoryPattern = `^https://github\.com/NVIDIA/aicr/\.github/workflows/on-tag\.yaml@refs/tags/.*`

	// requiredRepoSubstring is the minimum substring that must appear in any
	// custom identity pattern. This ensures verification always pins to the
	// NVIDIA repository even when the workflow pattern is overridden.
	requiredRepoSubstring = "NVIDIA/aicr"
)

// VerifyOptions configures verification behavior.
type VerifyOptions struct {
	// CertificateIdentityRegexp overrides the default identity pinning pattern
	// for binary attestation verification. Must contain "NVIDIA/aicr".
	// Defaults to TrustedRepositoryPattern if empty.
	CertificateIdentityRegexp string
}

// ValidateIdentityPattern checks that a certificate identity pattern contains
// the required NVIDIA/aicr repository reference.
func ValidateIdentityPattern(pattern string) error {
	if pattern == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "certificate identity pattern cannot be empty")
	}
	if !strings.Contains(pattern, requiredRepoSubstring) {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("certificate identity pattern must contain %q to pin to the NVIDIA repository", requiredRepoSubstring))
	}
	return nil
}

// Verify performs full verification of a bundle directory.
// Returns a VerifyResult describing the trust level and verification details.
func Verify(ctx context.Context, bundleDir string, opts *VerifyOptions) (*VerifyResult, error) {
	if _, err := os.Stat(bundleDir); err != nil {
		if os.IsNotExist(err) {
			return nil, errors.New(errors.ErrCodeNotFound, "bundle directory not found: "+bundleDir)
		}
		return nil, errors.Wrap(errors.ErrCodeInternal, "cannot access bundle directory", err)
	}

	// Resolve options
	if opts == nil {
		opts = &VerifyOptions{}
	}
	identityPattern := opts.CertificateIdentityRegexp
	if identityPattern == "" {
		identityPattern = TrustedRepositoryPattern
	}
	// Validate the identity pattern to make sure it good and has not been tampered with
	if err := ValidateIdentityPattern(identityPattern); err != nil {
		return nil, err
	}

	result := &VerifyResult{}

	// Step 1: Verify checksums
	checksumPath := filepath.Join(bundleDir, checksum.ChecksumFileName)
	if _, err := os.Stat(checksumPath); os.IsNotExist(err) {
		result.TrustLevel = TrustUnknown
		result.Errors = append(result.Errors, "checksums.txt not found")
		return result, nil
	}

	checksumErrors := checksum.VerifyChecksums(bundleDir)
	if len(checksumErrors) > 0 {
		result.Errors = append(result.Errors, checksumErrors...)
		result.TrustLevel = TrustUnknown
		return result, nil
	}
	result.ChecksumsPassed = true
	result.ChecksumFiles = checksum.CountEntries(bundleDir)

	slog.Debug("checksums verified", "files", result.ChecksumFiles)

	// Step 2: Check for bundle attestation
	bundleAttestPath := filepath.Join(bundleDir, attestation.BundleAttestationFile)
	if _, err := os.Stat(bundleAttestPath); os.IsNotExist(err) {
		// No attestation — checksums valid but unverified
		result.TrustLevel = TrustUnverified
		return result, nil
	}

	// Step 3: Verify bundle attestation with sigstore-go
	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "context cancelled during verification", err)
	}

	bundleCreator, err := verifySigstoreBundle(ctx, bundleAttestPath)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("bundle attestation verification failed: %v", err))
		result.TrustLevel = TrustUnknown
		return result, nil
	}
	result.BundleAttested = true
	result.BundleCreator = bundleCreator

	slog.Debug("bundle attestation verified", "creator", bundleCreator)

	// Step 4: Check for binary attestation
	binaryAttestPath := filepath.Join(bundleDir, attestation.BinaryAttestationFile)
	if _, statErr := os.Stat(binaryAttestPath); os.IsNotExist(statErr) {
		// Bundle attested but no binary attestation — chain incomplete
		result.TrustLevel = TrustAttested
		return result, nil
	}

	// Step 5: Verify binary attestation with identity pinning
	binaryBuilder, err := verifyBinaryAttestation(ctx, binaryAttestPath, identityPattern)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("binary attestation verification failed: %v", err))
		result.TrustLevel = TrustAttested
		return result, nil
	}
	result.BinaryAttested = true
	result.IdentityPinned = true
	result.BinaryBuilder = binaryBuilder

	slog.Debug("binary attestation verified", "builder", binaryBuilder)

	// Full chain verified — check if external data caps trust at attested
	dataDir := filepath.Join(bundleDir, "data")
	if _, dataDirErr := os.Stat(dataDir); dataDirErr == nil {
		result.HasExternalData = true
		result.TrustLevel = TrustAttested
		return result, nil
	}

	result.TrustLevel = TrustVerified
	return result, nil
}

// containsCertChainError checks if an error message indicates a certificate chain
// verification failure, which typically means the trusted root is stale.
func containsCertChainError(errMsg string) bool {
	staleIndicators := []string{
		"certificate signed by unknown authority",
		"certificate chain",
		"x509",
		"unable to verify certificate",
		"root certificate",
	}
	lower := strings.ToLower(errMsg)
	for _, indicator := range staleIndicators {
		if strings.Contains(lower, indicator) {
			return true
		}
	}
	return false
}

// loadSigstoreBundle reads a .sigstore.json file and returns a parsed Bundle.
func loadSigstoreBundle(path string) (*bundle.Bundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to read sigstore bundle: "+path, err)
	}

	var pb protobundle.Bundle
	if unmarshalErr := protojson.Unmarshal(data, &pb); unmarshalErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to parse sigstore bundle", unmarshalErr)
	}

	b, err := bundle.NewBundle(&pb)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "invalid sigstore bundle", err)
	}

	return b, nil
}

// verifySigstoreBundle verifies a Sigstore bundle (.sigstore.json) against the
// public-good trusted root. Returns the signer identity on success.
func verifySigstoreBundle(_ context.Context, bundlePath string) (string, error) {
	b, err := loadSigstoreBundle(bundlePath)
	if err != nil {
		return "", err
	}

	trustedMaterial, err := trust.GetTrustedMaterial()
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to load trusted root", err)
	}

	return verifyBundle(b, trustedMaterial, nil)
}

// verifyBinaryAttestation verifies the binary attestation with identity pinning
// to NVIDIA's GitHub Actions OIDC issuer and the given repository pattern.
func verifyBinaryAttestation(_ context.Context, bundlePath string, identityPattern string) (string, error) {
	b, err := loadSigstoreBundle(bundlePath)
	if err != nil {
		return "", err
	}

	trustedMaterial, err := trust.GetTrustedMaterial()
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to load trusted root", err)
	}

	// Pin identity to NVIDIA CI using the provided pattern
	identity, err := verify.NewShortCertificateIdentity(
		TrustedOIDCIssuer, "",
		"", identityPattern,
	)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to create identity matcher", err)
	}

	return verifyBundle(b, trustedMaterial, &identity)
}

// verifyBundle performs sigstore-go verification on a bundle.
// If identity is non-nil, it pins to that certificate identity.
// Returns the SubjectAlternativeName from the signing certificate.
func verifyBundle(b *bundle.Bundle, trustedMaterial root.TrustedMaterial, identity *verify.CertificateIdentity) (string, error) {
	v, err := verify.NewVerifier(trustedMaterial,
		verify.WithTransparencyLog(1),
		verify.WithObserverTimestamps(1),
	)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to create sigstore verifier", err)
	}

	// Build policy
	var policy verify.PolicyBuilder
	if identity != nil {
		policy = verify.NewPolicy(verify.WithoutArtifactUnsafe(), verify.WithCertificateIdentity(*identity))
	} else {
		policy = verify.NewPolicy(verify.WithoutArtifactUnsafe(), verify.WithoutIdentitiesUnsafe())
	}

	result, err := v.Verify(b, policy)
	if err != nil {
		// Detect staleness: if the error mentions certificate chain issues,
		// suggest updating the trusted root
		errMsg := err.Error()
		if containsCertChainError(errMsg) {
			return "", errors.New(errors.ErrCodeUnauthorized,
				"sigstore verification failed — the signing certificate may have been issued "+
					"by a CA not present in your trusted root. This usually means Sigstore rotated "+
					"their keys since your last update.\n\n  To fix: aicr trust update")
		}
		return "", errors.Wrap(errors.ErrCodeUnauthorized, "sigstore verification failed", err)
	}

	// Extract signer identity from certificate
	if result.Signature != nil && result.Signature.Certificate != nil {
		return result.Signature.Certificate.SubjectAlternativeName, nil
	}

	return "", nil
}
