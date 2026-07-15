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
	"context"
	"log/slog"
	"strings"

	bundleattest "github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/oci"
)

// SignExistingOptions controls SignExisting. It signs a bundle that has
// already been pushed to a registry (e.g. by `--no-sign`) and attaches the
// Sigstore Bundle as an OCI referrer — the Fulcio-bound leg the fork-based
// CI workflow runs after a contributor commits an unsigned pointer.
type SignExistingOptions struct {
	// Pointer is the loaded, validated unsigned pointer. Its first
	// attestation's bundle.oci names the in-toto subject and registry; on
	// success its signer block is filled in and the pointer is written back
	// to PointerPath. Required.
	Pointer *Pointer

	// PointerPath is the file the patched pointer is written back to —
	// typically the committed recipes/evidence/<recipe>.yaml the caller
	// read. Required.
	PointerPath string

	// BundleDir is a local copy of the already-pushed bundle (typically a
	// freshly pulled temp dir). Only the unsigned in-toto Statement is read
	// from it — to reconstruct the exact predicate the signature binds — so
	// the bundle is never re-emitted. May be the summary-bundle dir or a
	// parent containing it (mirrors loadOnDiskBundle / publish).
	BundleDir string

	// Artifact is the subject descriptor of the already-pushed artifact:
	// the manifest Digest, MediaType, and Size resolved at pull time. All
	// three are required to attach a spec-compliant referrer; a pointer
	// alone carries only the digest.
	Artifact MainArtifactDescriptor

	PlainHTTP   bool
	InsecureTLS bool

	// OIDCResolve configures keyless-signing token resolution, deferred
	// until adjacent to SignStatement so Fulcio's nonce-binding window is
	// respected.
	OIDCResolve bundleattest.ResolveOptions
}

// SignExisting signs an already-pushed, unsigned evidence bundle, attaches
// the resulting Sigstore Bundle as an OCI referrer of that artifact, and
// patches the pointer's signer block in place — writing it back to
// PointerPath. It does not push or re-emit the bundle: the predicate is read
// verbatim from the local copy's statement.intoto.json so the signature
// binds the same bytes the unsigned push produced.
//
// Like Publish, it returns only an error and writes its artifact (the
// patched pointer) as a side effect: the populated success path needs a live
// Fulcio sign + registry attach that unit tests cannot reach, so only the
// guard paths are unit-tested.
func SignExisting(ctx context.Context, opts SignExistingOptions) error {
	if opts.PointerPath == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "pointer path is required")
	}
	// Enforce the signable-pointer invariant in the domain function itself, not
	// just the CLI: patching Attestations[0] on a multi-attestation or
	// already-signed pointer would clobber provenance for any non-CLI caller.
	if err := ValidateSignablePointer(opts.Pointer); err != nil {
		return err
	}
	reference := opts.Pointer.Attestations[0].Bundle.OCI
	if opts.Artifact.Digest == "" || opts.Artifact.MediaType == "" || opts.Artifact.Size <= 0 {
		return errors.New(errors.ErrCodeInvalidRequest,
			"artifact descriptor (digest, mediaType, size) is required to sign an existing bundle")
	}

	bundle, _, err := loadOnDiskBundle(opts.BundleDir)
	if err != nil {
		return err
	}

	artifactDigestHex := strings.TrimPrefix(opts.Artifact.Digest, "sha256:")
	artifactStmt, err := BuildArtifactStatement(oci.TrimScheme(reference), artifactDigestHex, bundle.Predicate)
	if err != nil {
		return err
	}

	logOIDCResolveMode(opts.OIDCResolve)
	resolveCtx, resolveCancel := context.WithTimeout(ctx, defaults.OIDCAuthTimeout)
	token, tokenErr := bundleattest.ResolveOIDCToken(resolveCtx, opts.OIDCResolve)
	resolveCancel()
	if tokenErr != nil {
		return tokenErr
	}

	signCtx, signCancel := context.WithTimeout(ctx, defaults.EvidenceBundleSignTimeout)
	defer signCancel()
	// Shared mapper keeps evidence signing in lockstep with bundle/catalog: the
	// Rekor v2 signing-config selection (ResolveOptions.UseTUFSigningConfig) is
	// carried through, not just the Fulcio/Rekor endpoints.
	signRes, err := bundleattest.SignStatement(signCtx, artifactStmt,
		bundleattest.SignOptionsFromResolve(token, opts.OIDCResolve))
	if err != nil {
		return err
	}

	attachCtx, attachCancel := context.WithTimeout(ctx, defaults.EvidenceBundlePushTimeout)
	defer attachCancel()
	referrer, attachErr := AttachSigstoreBundleAsReferrer(attachCtx, AttachReferrerOptions{
		Reference:     reference,
		BundleJSON:    signRes.BundleJSON,
		MainArtifact:  opts.Artifact,
		ExcludedRoots: []string{bundle.SummaryDir},
		PlainHTTP:     opts.PlainHTTP,
		InsecureTLS:   opts.InsecureTLS,
	})
	if attachErr != nil {
		return attachErr
	}

	// Patch the signer block and write the pointer back. Revert the in-memory
	// mutation if the write fails, so the caller's pointer is never left marked
	// signed while the file on disk stays unsigned — otherwise a retry with the
	// same object would trip the already-signed guard.
	opts.Pointer.Attestations[0].Signer = PointerSignerFromSignature(signRes)
	if _, err := WritePointerFile(opts.PointerPath, opts.Pointer); err != nil {
		opts.Pointer.Attestations[0].Signer = nil
		return err
	}

	slog.Info("evidence bundle signed and pointer patched in place",
		"referrerDigest", referrer.Digest,
		"mainArtifactDigest", opts.Artifact.Digest,
		"recipe", bundle.RecipeName,
		"signer", signRes.Identity)

	return nil
}

// ValidateSignablePointer reports whether a pointer is in the exact state
// `aicr evidence sign` operates on: exactly one attestation, already pushed
// (bundle.oci + bundle.digest set), and not yet signed (nil Signer). It is the
// single source of truth for this domain rule — the CLI calls it for fail-fast
// validation before the registry pull, and SignExisting calls it before
// patching the signer block — so the two cannot drift. Failing closed here
// prevents re-signing (which would clobber an existing signature) or signing a
// pointer that has nothing pushed to sign.
func ValidateSignablePointer(p *Pointer) error {
	if p == nil || len(p.Attestations) != 1 {
		return errors.New(errors.ErrCodeInvalidRequest, "pointer must carry exactly one attestation to sign")
	}
	att := p.Attestations[0]
	if att.Signer != nil {
		return errors.New(errors.ErrCodeConflict,
			"pointer is already signed; refusing to overwrite its signer block")
	}
	if att.Bundle.OCI == "" || att.Bundle.Digest == "" {
		return errors.New(errors.ErrCodeInvalidRequest,
			"pointer has no pushed bundle to sign (bundle.oci/bundle.digest are empty); "+
				"push it first with `aicr evidence publish --no-sign`")
	}
	return nil
}

// PointerSignerFromSignature builds the pointer's signer block from a
// signing result, applying the same zero-Rekor rule as the emit path:
// SignedAttestation.RekorLogIndex uses 0 as the "no Rekor entry" sentinel
// (e.g. --no-rekor), so this maps 0 to a nil index. A genuine Rekor index 0
// is therefore indistinguishable from "no entry" at this boundary — an
// accepted limitation, since the index is informational, not the trust anchor.
func PointerSignerFromSignature(sig *bundleattest.SignedAttestation) *PointerSigner {
	if sig == nil {
		return nil
	}
	signer := &PointerSigner{Identity: sig.Identity, Issuer: sig.Issuer}
	if sig.RekorLogIndex > 0 {
		idx := sig.RekorLogIndex
		signer.RekorLogIndex = &idx
	}
	return signer
}
