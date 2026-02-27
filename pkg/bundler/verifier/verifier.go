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
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/trust"
)

// Identity pinning constants for NVIDIA CI.
const (
	TrustedOIDCIssuer        = "https://token.actions.githubusercontent.com"
	TrustedRepositoryPattern = `^https://github\.com/NVIDIA/aicr/\.github/workflows/on-tag\.yaml@refs/tags/.*`

	// requiredRepoPrefix includes the scheme+domain to ensure github.com is the
	// actual domain, not a path segment (e.g., "evil.com/github.com/NVIDIA/aicr/"
	// would bypass a domain-less check). The escaped form handles regex patterns.
	requiredRepoPrefix        = "://github.com/NVIDIA/aicr/"
	requiredRepoPrefixEscaped = `://github\.com/NVIDIA/aicr/`
)

// VerifyOptions configures verification behavior.
type VerifyOptions struct {
	// CertificateIdentityRegexp overrides the default identity pinning pattern
	// for binary attestation verification. Must contain "NVIDIA/aicr".
	// Defaults to TrustedRepositoryPattern if empty.
	CertificateIdentityRegexp string
}

// ValidateIdentityPattern checks that a certificate identity pattern contains
// the required NVIDIA/aicr GitHub repository URL path. Accepts both literal
// and regex-escaped forms (e.g., "github.com" or "github\.com").
func ValidateIdentityPattern(pattern string) error {
	if pattern == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "certificate identity pattern cannot be empty")
	}
	// Accept both literal and regex-escaped dots in the domain
	if !strings.Contains(pattern, requiredRepoPrefix) &&
		!strings.Contains(pattern, requiredRepoPrefixEscaped) {

		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("certificate identity pattern must contain %q to pin to the NVIDIA repository", requiredRepoPrefix))
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

	// Step 1: Read and verify checksums (single read to prevent TOCTOU)
	checksumData, done := verifyChecksumStep(bundleDir, result)
	if done {
		return result, nil
	}

	slog.Debug("checksums verified", "files", result.ChecksumFiles)

	// Step 2: Check for bundle attestation
	bundleAttestPath := filepath.Join(bundleDir, attestation.BundleAttestationFile)
	if _, statErr := os.Stat(bundleAttestPath); os.IsNotExist(statErr) {
		// No attestation — checksums valid but unverified
		result.TrustLevel = TrustUnverified
		return result, nil
	}

	// Step 3: Verify bundle attestation with sigstore-go, binding to checksums.txt content
	// Uses the same checksumData bytes read in Step 1 — no second read.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "context cancelled during verification", ctxErr)
	}

	checksumHash := sha256.Sum256(checksumData)
	checksumDigest := checksumHash[:]

	bundleCreator, err := verifySigstoreBundle(ctx, bundleAttestPath, checksumDigest)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("bundle attestation verification failed: %v", err))
		result.TrustLevel = TrustUnknown
		return result, nil
	}
	result.BundleAttested = true
	result.BundleCreator = bundleCreator
	result.ToolVersion = extractToolVersion(bundleAttestPath)

	slog.Debug("bundle attestation verified", "creator", bundleCreator, "toolVersion", result.ToolVersion)

	// Step 4: Check for binary attestation
	binaryAttestPath := filepath.Join(bundleDir, attestation.BinaryAttestationFile)
	if _, statErr := os.Stat(binaryAttestPath); os.IsNotExist(statErr) {
		// Bundle attested but no binary attestation — chain incomplete
		result.TrustLevel = TrustAttested
		return result, nil
	}

	// Step 5: Verify binary attestation with identity pinning.
	// Extract the binary digest from the bundle attestation's resolvedDependencies
	// rather than hashing the running binary — the verifying binary may be a
	// different version than the one that created the bundle.
	binaryDigest, err := extractBinaryDigest(bundleAttestPath)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("could not extract binary digest from bundle attestation: %v", err))
		result.TrustLevel = TrustAttested
		return result, nil
	}

	binaryBuilder, err := VerifyBinaryAttestation(ctx, binaryAttestPath, identityPattern, binaryDigest)
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

// verifyChecksumStep reads and verifies checksums.txt in a single read (TOCTOU-safe).
// Returns the raw checksum data and whether Verify should return early (done=true
// means result is populated with either an error or TrustUnknown).
func verifyChecksumStep(bundleDir string, result *VerifyResult) ([]byte, bool) {
	checksumPath := filepath.Join(bundleDir, checksum.ChecksumFileName)
	checksumData, err := os.ReadFile(checksumPath)
	if err != nil {
		if os.IsNotExist(err) {
			result.TrustLevel = TrustUnknown
			result.Errors = append(result.Errors, "checksums.txt not found")
			return nil, true
		}
		result.TrustLevel = TrustUnknown
		result.Errors = append(result.Errors, fmt.Sprintf("failed to read checksums.txt: %v", err))
		return nil, true
	}

	checksumErrors := checksum.VerifyChecksumsFromData(bundleDir, checksumData)
	if len(checksumErrors) > 0 {
		result.Errors = append(result.Errors, checksumErrors...)
		result.TrustLevel = TrustUnknown
		return nil, true
	}
	result.ChecksumsPassed = true
	result.ChecksumFiles = checksum.CountEntries(bundleDir)
	return checksumData, false
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

// resolveExecutablePath returns the best path for reading the running binary.
// On Linux, /proc/self/exe refers to the original inode even if the binary
// has been replaced on disk, preventing TOCTOU races. On other platforms,
// falls back to os.Executable().
func resolveExecutablePath() string {
	if runtime.GOOS == "linux" {
		return "/proc/self/exe"
	}
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	return path
}

// parseDSSEPayload extracts and decodes the base64 DSSE payload from a
// sigstore bundle JSON file. Returns the decoded in-toto statement JSON.
func parseDSSEPayload(bundlePath string) ([]byte, error) {
	data, err := os.ReadFile(bundlePath)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to read bundle file", err)
	}

	var raw map[string]json.RawMessage
	if err = json.Unmarshal(data, &raw); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to unmarshal bundle", err)
	}

	envelopeJSON, ok := raw["dsseEnvelope"]
	if !ok {
		return nil, errors.New(errors.ErrCodeInternal, "missing dsseEnvelope")
	}

	var envelope struct {
		Payload string `json:"payload"`
	}
	if err = json.Unmarshal(envelopeJSON, &envelope); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to unmarshal dsseEnvelope", err)
	}

	decoded, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to decode DSSE payload", err)
	}
	return decoded, nil
}

// extractToolVersion reads a sigstore bundle file and extracts the tool version
// from the SLSA predicate's internalParameters.toolVersion field.
// Returns empty string if extraction fails (best-effort, non-fatal).
func extractToolVersion(bundlePath string) string {
	stmtJSON, err := parseDSSEPayload(bundlePath)
	if err != nil {
		return ""
	}

	var stmt struct {
		Predicate struct {
			BuildDefinition struct {
				InternalParameters struct {
					ToolVersion string `json:"toolVersion"`
				} `json:"internalParameters"`
			} `json:"buildDefinition"`
		} `json:"predicate"`
	}
	if err := json.Unmarshal(stmtJSON, &stmt); err != nil {
		return ""
	}

	return stmt.Predicate.BuildDefinition.InternalParameters.ToolVersion
}

// extractBinaryDigest reads the bundle attestation and returns the binary
// digest from resolvedDependencies. This is the digest of the binary that
// created the bundle, not the currently running binary.
func extractBinaryDigest(bundlePath string) ([]byte, error) {
	stmtJSON, err := parseDSSEPayload(bundlePath)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to parse bundle attestation payload", err)
	}

	var stmt struct {
		Predicate struct {
			BuildDefinition struct {
				ResolvedDependencies []struct {
					Digest map[string]string `json:"digest"`
				} `json:"resolvedDependencies"`
			} `json:"buildDefinition"`
		} `json:"predicate"`
	}
	if err := json.Unmarshal(stmtJSON, &stmt); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to parse bundle attestation statement", err)
	}

	for _, dep := range stmt.Predicate.BuildDefinition.ResolvedDependencies {
		if hexDigest, ok := dep.Digest["sha256"]; ok && hexDigest != "" {
			digest, decErr := hex.DecodeString(hexDigest)
			if decErr != nil {
				continue
			}
			return digest, nil
		}
	}

	return nil, errors.New(errors.ErrCodeNotFound, "no binary digest found in bundle attestation resolvedDependencies")
}

// loadSigstoreBundle reads a .sigstore.json file and returns a parsed Bundle.
// Rejects files larger than defaults.MaxSigstoreBundleSize.
func loadSigstoreBundle(path string) (*bundle.Bundle, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to stat sigstore bundle: "+path, err)
	}
	if info.Size() > defaults.MaxSigstoreBundleSize {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("sigstore bundle %s exceeds maximum size (%d bytes)", path, defaults.MaxSigstoreBundleSize))
	}

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
// public-good trusted root, binding the attestation to the given artifact digest.
// Requires a valid OIDC-issued certificate from any issuer (bundle attestation
// proves someone signed it, not necessarily NVIDIA — identity pinning to NVIDIA
// is enforced separately on the binary attestation).
// Returns the signer identity on success.
func verifySigstoreBundle(ctx context.Context, bundlePath string, artifactDigest []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", errors.Wrap(errors.ErrCodeTimeout, "context cancelled before bundle attestation verification", err)
	}

	b, err := loadSigstoreBundle(bundlePath)
	if err != nil {
		return "", err
	}

	trustedMaterial, err := trust.GetTrustedMaterial()
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to load trusted root", err)
	}

	// Require any valid OIDC-issued certificate — confirms a real identity signed this
	identity, err := verify.NewShortCertificateIdentity("", ".+", "", ".+")
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to create bundle identity matcher", err)
	}

	return verifyBundle(b, trustedMaterial, identity, artifactDigest)
}

// VerifyBinaryAttestation verifies the binary attestation with identity pinning
// to the given OIDC issuer and repository pattern, binding the attestation to
// the given artifact digest. Returns the signer identity on success.
func VerifyBinaryAttestation(ctx context.Context, bundlePath string, identityPattern string, artifactDigest []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", errors.Wrap(errors.ErrCodeTimeout, "context cancelled before binary attestation verification", err)
	}

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

	return verifyBundle(b, trustedMaterial, identity, artifactDigest)
}

// verifyBundle performs sigstore-go verification on a bundle.
// Both identity and artifactDigest are required — verification refuses to proceed
// without content binding (artifact digest) and signer validation (identity).
// Returns the SubjectAlternativeName from the signing certificate.
func verifyBundle(b *bundle.Bundle, trustedMaterial root.TrustedMaterial, identity verify.CertificateIdentity, artifactDigest []byte) (string, error) {
	v, err := verify.NewVerifier(trustedMaterial,
		verify.WithTransparencyLog(1),
		verify.WithObserverTimestamps(1),
	)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to create sigstore verifier", err)
	}

	// Artifact digest is required — refuse to verify without content binding
	if len(artifactDigest) == 0 {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			"artifact digest is required for attestation verification")
	}

	policy := verify.NewPolicy(
		verify.WithArtifactDigest("sha256", artifactDigest),
		verify.WithCertificateIdentity(identity),
	)

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
	if result != nil && result.Signature != nil && result.Signature.Certificate != nil {
		return result.Signature.Certificate.SubjectAlternativeName, nil
	}

	return "", nil
}
