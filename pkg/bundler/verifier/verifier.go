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
	"sync"
	"syscall"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"

	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/trust"
)

// readBoundedFile applies a bounded compatibility context to verifier inputs.
func readBoundedFile(path string) ([]byte, error) {
	return readBoundedFileContext(context.Background(), path)
}

// readBoundedFileContext opens verifier inputs without following links or
// blocking on special files, validates the opened descriptor, streams at most
// MaxSigstoreBundleSize bytes, and honors caller cancellation.
func readBoundedFileContext(ctx context.Context, path string) ([]byte, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "verifier file read canceled", ctxErr)
	}
	ctx, cancel := context.WithTimeout(ctx, defaults.FileReadTimeout)
	defer cancel()

	f, err := os.OpenFile( //nolint:gosec // verifier paths are bundle-local
		path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if stderrors.Is(err, syscall.ELOOP) {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "refusing to follow file symlink "+path, err)
		}
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to open file "+path, err)
	}
	defer func() { _ = f.Close() }()

	opened, err := f.Stat()
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to inspect opened file "+path, err)
	}
	if !opened.Mode().IsRegular() {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "file is not regular: "+path)
	}
	if opened.Size() > defaults.MaxSigstoreBundleSize {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("file %s exceeds maximum size (%d bytes)", path, defaults.MaxSigstoreBundleSize))
	}
	linked, err := os.Lstat(path)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "file changed while opening: "+path, err)
	}
	if !linked.Mode().IsRegular() || !os.SameFile(opened, linked) {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "file changed while opening: "+path)
	}

	data := make([]byte, 0, opened.Size())
	buffer := make([]byte, verifierFileReadBufferSize)
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, errors.Wrap(errors.ErrCodeTimeout, "verifier file read canceled", ctxErr)
		}
		read, readErr := f.Read(buffer)
		if read > 0 {
			if int64(len(data))+int64(read) > defaults.MaxSigstoreBundleSize {
				return nil, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("file %s exceeds maximum size (%d bytes)", path, defaults.MaxSigstoreBundleSize))
			}
			data = append(data, buffer[:read]...)
		}
		if readErr != nil {
			if stderrors.Is(readErr, io.EOF) {
				break
			}
			return nil, errors.Wrap(errors.ErrCodeInternal, "failed to read file "+path, readErr)
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "verifier file read canceled", ctxErr)
	}
	after, err := os.Lstat(path)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "file changed while reading: "+path, err)
	}
	if !after.Mode().IsRegular() || !os.SameFile(opened, after) {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "file changed while reading: "+path)
	}
	return data, nil
}

// Identity pinning constants for NVIDIA CI.
const (
	verifierFileReadBufferSize = 32 * 1024

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
	// (awskms:// | gcpkms:// | azurekms:// | hashivault://) or a local PEM public-key file.
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

type verifierDependencies struct {
	stageVerifiedBundle func(
		context.Context,
		string,
		checksum.InventoryOptions,
	) (string, *checksum.Inventory, func() error, error)
	afterSnapshot func() error
	warn          func(string, ...any)
}

func defaultVerifierDependencies() verifierDependencies {
	return verifierDependencies{
		stageVerifiedBundle: checksum.StageVerifiedBundle,
		afterSnapshot:       func() error { return nil },
		warn:                slog.Warn,
	}
}

func normalizeVerifierDependencies(deps verifierDependencies) verifierDependencies {
	defaults := defaultVerifierDependencies()
	if deps.stageVerifiedBundle == nil {
		deps.stageVerifiedBundle = defaults.stageVerifiedBundle
	}
	if deps.afterSnapshot == nil {
		deps.afterSnapshot = defaults.afterSnapshot
	}
	if deps.warn == nil {
		deps.warn = defaults.warn
	}
	return deps
}

// Verify performs full verification of a bundle directory.
// Returns a VerifyResult describing the trust level and verification details.
// Any returned staged-snapshot cleanup failure clears an otherwise successful
// result and is reported as an internal error.
func Verify(
	ctx context.Context,
	bundleDir string,
	opts *VerifyOptions,
) (result *VerifyResult, err error) {

	return verifyWithDependencies(ctx, bundleDir, opts, defaultVerifierDependencies())
}

func verifyWithDependencies(
	ctx context.Context,
	bundleDir string,
	opts *VerifyOptions,
	deps verifierDependencies,
) (result *VerifyResult, retErr error) {

	deps = normalizeVerifierDependencies(deps)
	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "context cancelled before verification", err)
	}
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

	result = &VerifyResult{}

	// Step 1: verify the caller-controlled bundle, then carry only a private
	// staged snapshot through every subsequent trust decision.
	snapshot, done, checksumErr := verifyChecksumStepWithDependencies(ctx, bundleDir, result, deps)
	if snapshot != nil {
		defer func() {
			cleanupErr := snapshot.cleanup()
			if cleanupErr == nil {
				return
			}
			if retErr != nil {
				deps.warn("failed to clean verification snapshot after verification failure",
					"error", cleanupErr, "primaryError", retErr)
				return
			}
			result = nil
			retErr = errors.Wrap(
				errors.ErrCodeInternal, "failed to clean verification snapshot", cleanupErr)
		}()
		if err := deps.afterSnapshot(); err != nil {
			return nil, err
		}
	}
	if checksumErr != nil {
		return nil, checksumErr
	}
	if done {
		return result, nil
	}
	return verifyStagedSnapshot(
		ctx, snapshot, result, opts, bundleSource, identityPattern)
}

func verifyStagedSnapshot(
	ctx context.Context,
	snapshot *verificationSnapshot,
	result *VerifyResult,
	opts *VerifyOptions,
	bundleSource attestation.TrustedRootSource,
	identityPattern string,
) (*VerifyResult, error) {

	verifiedDir := snapshot.stagedDir
	checksumDigest := snapshot.checksumDigest[:]

	slog.Debug("checksums verified", "files", result.ChecksumFiles)

	// Step 2: Check for bundle attestation
	bundleAttestPath, joinErr := deployer.SafeJoin(verifiedDir, attestation.BundleAttestationFile)
	if joinErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "unsafe bundle attestation path", joinErr)
	}
	if _, statErr := os.Stat(bundleAttestPath); os.IsNotExist(statErr) {
		// No attestation — checksums valid but unverified
		result.setTrust(TrustUnverified, "checksums valid but no attestation files found (bundle created without --attest)")
		return result, nil
	}

	// Step 3: Verify bundle attestation with sigstore-go, binding to the
	// checksums.txt digest from the staged inventory.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "context cancelled during verification", ctxErr)
	}

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
		if stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
			return nil, err
		}
		result.Errors = append(result.Errors, fmt.Sprintf("bundle attestation verification failed: %v", err))
		result.setTrust(TrustUnknown, "bundle attestation verification failed")
		return result, nil
	}
	result.BundleAttested = true
	result.BundleCreator = bundleCreator
	toolVersion, toolVersionErr := extractToolVersionContext(ctx, bundleAttestPath)
	if toolVersionErr != nil {
		return nil, toolVersionErr
	}
	result.ToolVersion = toolVersion

	slog.Debug("bundle attestation verified", "creator", bundleCreator, "toolVersion", result.ToolVersion)

	// Steps 4-5: Check for and verify the binary attestation.
	done, stepErr := verifyBinaryStep(ctx, verifiedDir, bundleAttestPath, identityPattern, result)
	if stepErr != nil {
		return nil, stepErr
	}
	if done {
		return result, nil
	}

	// Full chain verified — check if external data caps trust at attested
	dataDir, joinErr := deployer.SafeJoin(verifiedDir, "data")
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

// verifyBinaryStep runs steps 4-5 of Verify: locate and verify the binary
// attestation, binding it to the binary digest recorded in the (already
// verified) bundle attestation at bundleAttestPath. Trust semantics (#1550):
//
//   - binary attestation file MISSING → TrustAttested (incomplete chain: the
//     bundle attestation itself verified, nothing claims more);
//   - binary attestation PRESENT but its digest cannot be extracted or its
//     verification FAILS → TrustUnknown (a claim exists and could not be
//     substantiated — a hard failure, not a degraded success).
//
// Returns done=true when Verify should return the populated result as-is
// (verification outcome, nil error); a non-nil error is a hard fault
// (unsafe path), for which Verify returns no result.
func verifyBinaryStep(ctx context.Context, bundleDir, bundleAttestPath, identityPattern string, result *VerifyResult) (bool, error) {
	binaryAttestPath, joinErr := deployer.SafeJoin(bundleDir, attestation.BinaryAttestationFile)
	if joinErr != nil {
		return false, errors.Wrap(errors.ErrCodeInternal, "unsafe binary attestation path", joinErr)
	}
	if _, statErr := os.Stat(binaryAttestPath); os.IsNotExist(statErr) {
		// Bundle attested but no binary attestation — chain incomplete
		result.setTrust(TrustAttested, "bundle attested but binary attestation not found (incomplete chain)")
		return true, nil
	}

	// Verify binary attestation with identity pinning. Extract the binary
	// digest from the bundle attestation's resolvedDependencies rather than
	// hashing the running binary — the verifying binary may be a different
	// version than the one that created the bundle.
	binaryDigest, err := extractBinaryDigestContext(ctx, bundleAttestPath)
	if err != nil {
		if stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
			return false, err
		}
		result.Errors = append(result.Errors, fmt.Sprintf("could not extract binary digest from bundle attestation: %v", err))
		result.setTrust(TrustUnknown, "binary attestation present but binary digest could not be extracted from bundle attestation")
		return true, nil
	}

	binaryBuilder, err := VerifyBinaryAttestation(ctx, binaryAttestPath, identityPattern, binaryDigest)
	if err != nil {
		if stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
			return false, err
		}
		result.Errors = append(result.Errors, fmt.Sprintf("binary attestation verification failed: %v", err))
		result.setTrust(TrustUnknown, "binary attestation present but failed verification")
		return true, nil
	}
	result.BinaryAttested = true
	result.IdentityPinned = true
	result.BinaryBuilder = binaryBuilder

	slog.Debug("binary attestation verified", "builder", binaryBuilder)
	return false, nil
}

type verificationSnapshot struct {
	stagedDir      string
	checksumDigest [sha256.Size]byte
	remove         func() error
	cleanupOnce    sync.Once
	cleanupErr     error
}

func (s *verificationSnapshot) cleanup() error {
	if s == nil {
		return nil
	}
	s.cleanupOnce.Do(func() {
		if s.remove != nil {
			s.cleanupErr = s.remove()
		}
	})
	return s.cleanupErr
}

// verifyChecksumStep verifies the caller-controlled bundle into a fresh,
// private stage and reuses the manifest metadata from that final staged-tree
// verification. All later trust evaluation must use only the returned snapshot.
func verifyChecksumStep(ctx context.Context, bundleDir string, result *VerifyResult) (*verificationSnapshot, bool, error) {
	return verifyChecksumStepWithDependencies(
		ctx, bundleDir, result, defaultVerifierDependencies())
}

func verifyChecksumStepWithDependencies(
	ctx context.Context,
	bundleDir string,
	result *VerifyResult,
	deps verifierDependencies,
) (*verificationSnapshot, bool, error) {

	deps = normalizeVerifierDependencies(deps)
	verifyOpts := checksum.InventoryOptions{AllowedMetadataPaths: attestation.BundleMetadataPaths()}
	stagedDir, staged, cleanup, err := deps.stageVerifiedBundle(ctx, bundleDir, verifyOpts)
	if err != nil {
		return checksumStepFailure(err, result)
	}
	if cleanup == nil {
		return nil, false, errors.New(
			errors.ErrCodeInternal, "verified bundle stage returned no cleanup owner")
	}
	if staged == nil {
		primaryErr := errors.New(
			errors.ErrCodeInternal, "verified bundle stage returned no inventory")
		cleanupErr := cleanup()
		if cleanupErr != nil {
			deps.warn("failed to clean verification snapshot after checksum failure",
				"error", cleanupErr, "primaryError", primaryErr)
		}
		return nil, false, primaryErr
	}
	snapshot := &verificationSnapshot{
		stagedDir:      stagedDir,
		checksumDigest: staged.ChecksumDigest(),
		remove:         cleanup,
	}
	result.ChecksumsPassed = true
	result.ChecksumFiles = staged.ManifestLen()
	return snapshot, false, nil
}

func checksumStepFailure(err error, result *VerifyResult) (*verificationSnapshot, bool, error) {
	if stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		return nil, false, err
	}
	if stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		return nil, false, err
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		return nil, false, err
	}
	if stderrors.Is(err, checksum.ErrChecksumManifestMissing) {
		result.setTrust(TrustUnknown, "checksums.txt not found")
		result.Errors = append(result.Errors, "checksums.txt not found")
		return nil, true, nil
	}
	result.Errors = append(result.Errors, err.Error())
	result.setTrust(TrustUnknown, "checksum inventory verification failed")
	return nil, true, nil
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
	return parseDSSEPayloadContext(context.Background(), bundlePath)
}

func parseDSSEPayloadContext(ctx context.Context, bundlePath string) ([]byte, error) {
	data, err := readBoundedFileContext(ctx, bundlePath)
	if err != nil {
		return nil, err // already coded by readBoundedFileContext
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
	version, _ := extractToolVersionContext(context.Background(), bundlePath)
	return version
}

func extractToolVersionContext(ctx context.Context, bundlePath string) (string, error) {
	stmtJSON, err := parseDSSEPayloadContext(ctx, bundlePath)
	if err != nil {
		if stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
			return "", err
		}
		return "", nil
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
		return "", nil
	}

	return stmt.Predicate.BuildDefinition.InternalParameters.ToolVersion, nil
}

// extractBinaryDigest reads the bundle attestation and returns the binary
// digest from resolvedDependencies. This is the digest of the binary that
// created the bundle, not the currently running binary.
func extractBinaryDigest(bundlePath string) ([]byte, error) {
	return extractBinaryDigestContext(context.Background(), bundlePath)
}

func extractBinaryDigestContext(ctx context.Context, bundlePath string) ([]byte, error) {
	stmtJSON, err := parseDSSEPayloadContext(ctx, bundlePath)
	if err != nil {
		return nil, errors.PropagateOrWrap(
			err, errors.ErrCodeInternal, "failed to parse bundle attestation payload")
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

	data, err := readBoundedFileContext(ctx, bundlePath)
	if err != nil {
		return "", err // already coded by readBoundedFileContext
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
	data, err := readBoundedFileContext(ctx, bundlePath)
	if err != nil {
		return "", err // already coded by readBoundedFileContext
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

	data, err := readBoundedFileContext(ctx, bundlePath)
	if err != nil {
		return "", err // already coded by readBoundedFileContext
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
