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

package verifier

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"

	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/trust"
)

// readBoundedFile streams a file through io.LimitReader against maxBytes.
// Used in place of os.ReadFile on attacker-influenced verifier inputs so
// the process cannot be forced to allocate an unbounded buffer.
func readBoundedFile(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // verifier paths are bundle-local
	if err != nil {
		// Wrap with a code while preserving the cause chain: callers propagate
		// this error as-is, and the os.ErrNotExist sentinel stays reachable via
		// errors.Is (StructuredError.Unwrap returns the cause).
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to open file "+path, err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to read file "+path, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("file %s exceeds maximum size (%d bytes)", path, maxBytes))
	}
	return data, nil
}

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

	// Key selects public-key verification of the bundle attestation instead of
	// keyless certificate-identity verification. A KMS key URI
	// (awskms:// | gcpkms:// | azurekms://) or a local PEM public-key file.
	// Independent of CertificateIdentityRegexp, which pins the (separate) binary
	// attestation; the two coexist (see #1152).
	Key string

	// TrustRoot is a path to a sigstore-go trusted_root.json for verifying the
	// bundle attestation against a private Fulcio/Rekor. ADDITIVE: unioned with
	// AICR's public-good root, so NVIDIA-signed and privately-signed bundles
	// both verify. Counterpart to `bundle --fulcio-url`/`--rekor-url`.
	// Composable with Key. Does NOT affect the binary attestation, which is
	// always NVIDIA-public-CI-signed and stays pinned to the public-good root.
	TrustRoot string
}

// resolveBundleTrustRoot returns the TrustedRootSource for the bundle
// attestation: the public-good root by default, or its union with the private
// trusted_root.json named by opts.TrustRoot. The binary attestation is
// unaffected and always uses the public-good root (see VerifyBinaryAttestation).
func resolveBundleTrustRoot(opts *VerifyOptions) (attestation.TrustedRootSource, error) {
	if opts.TrustRoot == "" {
		return attestation.PublicGoodTrustedRoot, nil
	}
	return newUnionTrustedRoot(opts.TrustRoot)
}

// newUnionTrustedRoot loads the private trusted_root.json at path and returns a
// TrustedRootSource that unions it with AICR's public-good root. The public-good
// half keeps NVIDIA-signed bundles verifiable; the private half admits bundles
// signed against a private Fulcio/Rekor. The file is loaded eagerly so a bad
// --trust-root (missing, oversized, malformed) fails fast with the loader's
// ErrCodeInvalidRequest, rather than being deferred into the lazy source where
// Verify would fold it into a generic verification-failure result. The
// public-good half stays lazy inside the closure so it resolves under ctx.
func newUnionTrustedRoot(path string) (attestation.TrustedRootSource, error) {
	priv, err := trust.LoadTrustedMaterialFromFile(path)
	if err != nil {
		return nil, err // already ErrCodeInvalidRequest
	}
	return func(ctx context.Context) (root.TrustedMaterial, error) {
		pg, err := attestation.PublicGoodTrustedRoot(ctx)
		if err != nil {
			return nil, err
		}
		return root.TrustedMaterialCollection{pg, priv}, nil
	}, nil
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

	// Resolve the trust anchors for the bundle attestation up front, so a bad
	// --trust-root file fails fast with its ErrCodeInvalidRequest instead of
	// being folded into a verification-failure result downstream.
	bundleSource, srcErr := resolveBundleTrustRoot(opts)
	if srcErr != nil {
		return nil, srcErr
	}

	result := &VerifyResult{}

	// Step 1: Read and verify checksums (single read to prevent TOCTOU)
	checksumData, done := verifyChecksumStep(bundleDir, result)
	if done {
		return result, nil
	}

	slog.Debug("checksums verified", "files", result.ChecksumFiles)

	// Step 2: Check for bundle attestation
	bundleAttestPath, joinErr := deployer.SafeJoin(bundleDir, attestation.BundleAttestationFile)
	if joinErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "unsafe bundle attestation path", joinErr)
	}
	if _, statErr := os.Stat(bundleAttestPath); os.IsNotExist(statErr) {
		// No attestation — checksums valid but unverified
		result.setTrust(TrustUnverified, "checksums valid but no attestation files found (bundle created without --attest)")
		return result, nil
	}

	// Step 3: Verify bundle attestation with sigstore-go, binding to checksums.txt content
	// Uses the same checksumData bytes read in Step 1 — no second read.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "context cancelled during verification", ctxErr)
	}

	checksumHash := sha256.Sum256(checksumData)
	checksumDigest := checksumHash[:]

	var (
		bundleCreator string
		err           error
	)
	if opts.Key != "" {
		bundleCreator, err = verifyKeySignedBundle(ctx, bundleAttestPath, checksumDigest, opts.Key, bundleSource)
	} else {
		bundleCreator, err = verifySigstoreBundle(ctx, bundleAttestPath, checksumDigest, bundleSource)
	}
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("bundle attestation verification failed: %v", err))
		result.setTrust(TrustUnknown, "bundle attestation verification failed")
		return result, nil
	}
	result.BundleAttested = true
	result.BundleCreator = bundleCreator
	result.ToolVersion = extractToolVersion(bundleAttestPath)

	slog.Debug("bundle attestation verified", "creator", bundleCreator, "toolVersion", result.ToolVersion)

	// Step 4: Check for binary attestation
	binaryAttestPath, joinErr := deployer.SafeJoin(bundleDir, attestation.BinaryAttestationFile)
	if joinErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "unsafe binary attestation path", joinErr)
	}
	if _, statErr := os.Stat(binaryAttestPath); os.IsNotExist(statErr) {
		// Bundle attested but no binary attestation — chain incomplete
		result.setTrust(TrustAttested, "bundle attested but binary attestation not found (incomplete chain)")
		return result, nil
	}

	// Step 5: Verify binary attestation with identity pinning.
	// Extract the binary digest from the bundle attestation's resolvedDependencies
	// rather than hashing the running binary — the verifying binary may be a
	// different version than the one that created the bundle.
	binaryDigest, err := extractBinaryDigest(bundleAttestPath)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("could not extract binary digest from bundle attestation: %v", err))
		result.setTrust(TrustAttested, "could not extract binary digest from bundle attestation")
		return result, nil
	}

	binaryBuilder, err := VerifyBinaryAttestation(ctx, binaryAttestPath, identityPattern, binaryDigest)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("binary attestation verification failed: %v", err))
		result.setTrust(TrustAttested, "binary attestation verification failed")
		return result, nil
	}
	result.BinaryAttested = true
	result.IdentityPinned = true
	result.BinaryBuilder = binaryBuilder

	slog.Debug("binary attestation verified", "builder", binaryBuilder)

	// Full chain verified — check if external data caps trust at attested
	dataDir, joinErr := deployer.SafeJoin(bundleDir, "data")
	if joinErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "unsafe data directory path", joinErr)
	}
	if _, dataDirErr := os.Stat(dataDir); dataDirErr == nil {
		result.HasExternalData = true
		result.setTrust(TrustAttested, "external --data files included; verified requires only embedded recipe data")
		return result, nil
	}

	result.setTrust(TrustVerified, "full chain verified: checksums, bundle attestation, binary attestation with NVIDIA CI identity")
	return result, nil
}

// verifyChecksumStep reads and verifies checksums.txt in a single read (TOCTOU-safe).
// Returns the raw checksum data and whether Verify should return early (done=true
// means result is populated with either an error or TrustUnknown).
func verifyChecksumStep(bundleDir string, result *VerifyResult) ([]byte, bool) {
	checksumPath, joinErr := deployer.SafeJoin(bundleDir, checksum.ChecksumFileName)
	if joinErr != nil {
		result.setTrust(TrustUnknown, "unsafe checksum path")
		result.Errors = append(result.Errors, fmt.Sprintf("unsafe checksum path: %v", joinErr))
		return nil, true
	}
	checksumData, err := readBoundedFile(checksumPath, defaults.MaxChecksumFileBytes)
	if err != nil {
		if stderrors.Is(err, os.ErrNotExist) {
			result.setTrust(TrustUnknown, "checksums.txt not found")
			result.Errors = append(result.Errors, "checksums.txt not found")
			return nil, true
		}
		result.setTrust(TrustUnknown, "failed to read checksums.txt")
		result.Errors = append(result.Errors, fmt.Sprintf("failed to read checksums.txt: %v", err))
		return nil, true
	}

	checksumErrors := checksum.VerifyChecksumsFromData(bundleDir, checksumData)
	if len(checksumErrors) > 0 {
		result.Errors = append(result.Errors, checksumErrors...)
		result.setTrust(TrustUnknown, "checksum verification failed")
		return nil, true
	}
	result.ChecksumsPassed = true
	result.ChecksumFiles = checksum.CountEntries(bundleDir)
	return checksumData, false
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
	data, err := readBoundedFile(bundlePath, defaults.MaxSigstoreBundleSize)
	if err != nil {
		return nil, err // already coded by readBoundedFile
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

// verifySigstoreBundle verifies a Sigstore bundle (.sigstore.json) against the
// public-good trusted root, binding the attestation to the given artifact digest.
// Requires a valid OIDC-issued certificate from any issuer (bundle attestation
// proves someone signed it, not necessarily NVIDIA — identity pinning to NVIDIA
// is enforced separately on the binary attestation).
// Returns the signer identity on success.
func verifySigstoreBundle(ctx context.Context, bundlePath string, artifactDigest []byte, src attestation.TrustedRootSource) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", errors.Wrap(errors.ErrCodeTimeout, "context cancelled before bundle attestation verification", err)
	}

	data, err := readBoundedFile(bundlePath, defaults.MaxSigstoreBundleSize)
	if err != nil {
		return "", err // already coded by readBoundedFile (oversize -> ErrCodeInvalidRequest)
	}

	// Require any valid OIDC-issued certificate — confirms a real identity signed this
	identity, err := verify.NewShortCertificateIdentity("", ".+", "", ".+")
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to create bundle identity matcher", err)
	}

	return attestation.VerifyStatementWith(ctx, data,
		attestation.NewKeylessVerificationIdentity(identity, src),
		attestation.NewRequireTLogPolicy(), artifactDigest)
}

// verifyKeySignedBundle verifies a public-key-signed bundle attestation (#407
// KMS signing or a local PEM) against the key named by keyRef, binding it to
// artifactDigest. Returns the key identity (KMS URI or pem:<fp>) as the bundle
// creator, since a key-signed bundle has no certificate SAN.
func verifyKeySignedBundle(ctx context.Context, bundlePath string, artifactDigest []byte, keyRef string, src attestation.TrustedRootSource) (string, error) {
	id, err := attestation.NewKeyVerificationIdentity(ctx, keyRef, src)
	if err != nil {
		return "", err // already classified (ErrCodeInvalidRequest / ErrCodeUnavailable)
	}
	data, err := readBoundedFile(bundlePath, defaults.MaxSigstoreBundleSize)
	if err != nil {
		return "", err // already coded by readBoundedFile (oversize -> ErrCodeInvalidRequest)
	}
	signer, err := attestation.VerifyStatementWith(ctx, data, id, attestation.NewRequireTLogPolicy(), artifactDigest)
	if err != nil {
		return "", err
	}
	if signer == "" {
		// Key-signed bundles carry no cert SAN; fall back to the key identity.
		if ki, ok := id.(interface{ Identity() string }); ok {
			return ki.Identity(), nil
		}
	}
	return signer, nil
}

// VerifyBinaryAttestation verifies the binary attestation with identity pinning
// to the given OIDC issuer and repository pattern, binding the attestation to
// the given artifact digest. Returns the signer identity on success.
func VerifyBinaryAttestation(ctx context.Context, bundlePath string, identityPattern string, artifactDigest []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", errors.Wrap(errors.ErrCodeTimeout, "context cancelled before binary attestation verification", err)
	}

	data, err := readBoundedFile(bundlePath, defaults.MaxSigstoreBundleSize)
	if err != nil {
		return "", err // already coded by readBoundedFile (oversize -> ErrCodeInvalidRequest)
	}

	// Pin identity to NVIDIA CI using the provided pattern
	identity, err := verify.NewShortCertificateIdentity(
		TrustedOIDCIssuer, "",
		"", identityPattern,
	)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to create identity matcher", err)
	}

	// Binary attestation is always signed by NVIDIA's public GitHub-OIDC CI, so
	// it verifies against the public-good root only (nil source) regardless of
	// any --trust-root for the bundle attestation.
	return attestation.VerifyStatementWith(ctx, data,
		attestation.NewKeylessVerificationIdentity(identity, nil),
		attestation.NewRequireTLogPolicy(), artifactDigest)
}
