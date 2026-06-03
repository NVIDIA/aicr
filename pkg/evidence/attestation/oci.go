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
	"os"
	"strings"

	digestpkg "github.com/opencontainers/go-digest"
	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/oci"
)

// SigstoreBundleMediaType identifies the Sigstore Bundle JSON
// (DSSE envelope + Fulcio cert + Rekor inclusion proof) attached as
// the OCI Referrer. cosign discovers signatures with this media type
// via the OCI 1.1 Referrers API.
const SigstoreBundleMediaType = "application/vnd.dev.sigstore.bundle.v0.3+json"

// PushOptions controls OCI publication of a bundle directory.
type PushOptions struct {
	// SourceDir is the bundle directory to package (summary or logs).
	SourceDir string

	// Reference is an OCI URI like
	// "oci://ghcr.io/myorg/aicr-evidence:<tag>" (or a non-prefixed
	// equivalent). A tag is required at this level; the emit/publish
	// orchestration derives a per-recipe tag from the bundle when the
	// operator's --push omits one (see effectiveEvidenceRef).
	Reference string

	// AICRVersion is recorded in the OCI manifest's
	// org.opencontainers.image.version annotation.
	AICRVersion string

	// PlainHTTP forces HTTP (used for local registry tests).
	PlainHTTP bool

	// InsecureTLS disables TLS verification for self-signed registries.
	InsecureTLS bool
}

// PushResult describes the OCI artifact produced by Push.
type PushResult struct {
	// Reference is the canonical "registry/repository:tag" string.
	Reference string

	// Digest is the OCI content digest, e.g., "sha256:abc...".
	Digest string

	// MediaType is the manifest media type.
	MediaType string

	// Size is the manifest's byte length, needed when constructing a
	// subject descriptor for OCI Referrers attachment.
	Size int64
}

// Push packages a bundle directory as an OCI artifact and pushes it.
func Push(ctx context.Context, opts PushOptions) (*PushResult, error) {
	if opts.SourceDir == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "SourceDir is required")
	}
	if opts.Reference == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "Reference is required")
	}

	ref, err := oci.ParseOutputTarget(oci.EnsureScheme(opts.Reference))
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "invalid reference")
	}
	if !ref.IsOCI {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "Reference must be an OCI registry reference")
	}
	if ref.Tag == "" {
		// The emit/publish orchestration derives a per-recipe tag from the
		// bundle before calling Push (see effectiveEvidenceRef); a tag is
		// required here so we never silently fall back to a shared constant.
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"evidence reference must include a tag")
	}

	tmpOut, err := os.MkdirTemp("", "aicr-evidence-oci-")
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create temp OCI store dir", err)
	}
	defer func() { _ = os.RemoveAll(tmpOut) }()

	cfg := oci.OutputConfig{
		SourceDir:   opts.SourceDir,
		OutputDir:   tmpOut,
		Reference:   ref,
		Version:     opts.AICRVersion,
		PlainHTTP:   opts.PlainHTTP,
		InsecureTLS: opts.InsecureTLS,
		Annotations: map[string]string{
			"org.opencontainers.image.version":     opts.AICRVersion,
			"org.opencontainers.image.vendor":      "NVIDIA",
			"org.opencontainers.image.title":       "AICR Recipe Evidence",
			"org.opencontainers.image.source":      "https://github.com/NVIDIA/aicr",
			"org.opencontainers.image.description": "Signed evidence bundle for an aicr recipe (recipe-evidence/v1).",
		},
	}

	// Encode the network-bound contract on the public function itself
	// (rather than trusting every caller to bound). EvidenceBundlePushTimeout
	// matches the cap the current call site already imposes, so existing
	// behavior is unchanged — but future callers that pass a longer-lived
	// ctx still get an opinionated upper bound on the registry round-trip.
	pushCtx, pushCancel := context.WithTimeout(ctx, defaults.EvidenceBundlePushTimeout)
	defer pushCancel()
	res, err := oci.PackageAndPush(pushCtx, cfg)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "package and push failed")
	}

	return &PushResult{
		Reference: res.Reference,
		Digest:    res.Digest,
		MediaType: res.MediaType,
		Size:      res.Size,
	}, nil
}

// maxEvidenceTagSlug caps the recipe-slug portion of a derived tag so the
// slug plus "-" plus the 12-char fingerprint stays well under the 128-char
// OCI tag limit.
const maxEvidenceTagSlug = 80

// effectiveEvidenceRef resolves the OCI reference to push a bundle to. When
// the operator's reference omits a tag, a per-attestation tag derived from
// the bundle (see deriveEvidenceTag) is applied, so distinct attestations
// never collide on a shared tag. Verification pins on the content digest
// regardless, so the tag is a human-readable label, not the trust anchor.
func effectiveEvidenceRef(userRef string, bundle *Bundle) (string, error) {
	ref, err := oci.ParseOutputTarget(oci.EnsureScheme(userRef))
	if err != nil {
		return "", errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "invalid reference")
	}
	if !ref.IsOCI {
		return "", errors.New(errors.ErrCodeInvalidRequest, "Reference must be an OCI registry reference")
	}
	if ref.Tag == "" {
		ref = ref.WithTag(deriveEvidenceTag(bundle))
	}
	return ref.String(), nil
}

// deriveEvidenceTag builds a human-readable, per-attestation OCI tag for a
// bundle pushed without one: "<recipe-slug>-<fingerprint>", where the
// fingerprint is the first 12 hex chars of the bundle's manifest digest.
// The slug keeps the tag readable; the fingerprint keeps it unique per
// attestation so a later push never silently floats an existing tag.
func deriveEvidenceTag(bundle *Bundle) string {
	slug := sanitizeOCITag(bundle.RecipeName)
	fp := manifestFingerprint(bundle)
	if fp == "" {
		return slug
	}
	return slug + "-" + fp
}

// sanitizeOCITag coerces s into a valid OCI tag: lowercased, with any
// character outside [a-z0-9_.-] replaced by '-', leading/trailing '-' and
// '.' trimmed, and the result capped at maxEvidenceTagSlug. Falls back to
// defaultRecipeName when nothing usable remains.
func sanitizeOCITag(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if len(out) > maxEvidenceTagSlug {
		out = strings.Trim(out[:maxEvidenceTagSlug], "-.")
	}
	if out == "" {
		return defaultRecipeName
	}
	return out
}

// manifestFingerprint returns the first 12 hex chars of the bundle's
// manifest digest, identifying this attestation's content. Empty when the
// digest is unavailable.
func manifestFingerprint(bundle *Bundle) string {
	if bundle == nil || bundle.Predicate == nil {
		return ""
	}
	hexDigest := strings.TrimPrefix(bundle.Predicate.Manifest.Digest, "sha256:")
	if len(hexDigest) < 12 {
		return hexDigest
	}
	return hexDigest[:12]
}

// AttachSigstoreBundleAsReferrer pushes a Sigstore Bundle blob as an OCI
// Referrer of the main artifact so cosign's /v2/<name>/referrers/<digest>
// discovery finds it. Referrers are addressed by digest, not by tag, so
// the tag in opts.Reference is ignored.
func AttachSigstoreBundleAsReferrer(ctx context.Context, opts AttachReferrerOptions) (*PushResult, error) {
	if opts.Reference == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "Reference is required")
	}
	if len(opts.BundleJSON) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "BundleJSON is required")
	}
	if opts.MainArtifact.Digest == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "MainArtifact.Digest is required")
	}
	if opts.MainArtifact.MediaType == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "MainArtifact.MediaType is required")
	}
	if opts.MainArtifact.Size <= 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "MainArtifact.Size is required")
	}

	ref, err := oci.ParseOutputTarget(oci.EnsureScheme(opts.Reference))
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "invalid reference")
	}
	if !ref.IsOCI {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "Reference must be an OCI registry reference")
	}

	subjectDesc := ociv1.Descriptor{
		MediaType: opts.MainArtifact.MediaType,
		Digest:    digestpkg.Digest(opts.MainArtifact.Digest),
		Size:      opts.MainArtifact.Size,
	}

	// Self-bound for the same reason as Push above: encode the contract
	// on the public function instead of trusting callers.
	attachCtx, attachCancel := context.WithTimeout(ctx, defaults.EvidenceBundlePushTimeout)
	defer attachCancel()
	res, err := oci.PushReferrer(attachCtx, oci.ReferrerOptions{
		Registry:     ref.Registry,
		Repository:   ref.Repository,
		PlainHTTP:    opts.PlainHTTP,
		InsecureTLS:  opts.InsecureTLS,
		ArtifactType: SigstoreBundleMediaType,
		LayerContent: opts.BundleJSON,
		Subject:      subjectDesc,
		Annotations: map[string]string{
			"org.opencontainers.image.vendor": "NVIDIA",
		},
	})
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "push referrer failed")
	}
	return &PushResult{
		Reference: res.Reference,
		Digest:    res.Digest,
		MediaType: res.MediaType,
		Size:      res.Size,
	}, nil
}

// AttachReferrerOptions configures AttachSigstoreBundleAsReferrer.
type AttachReferrerOptions struct {
	// Reference is the OCI reference of the main artifact (any tag).
	// Used only to identify the registry+repository.
	Reference string

	// BundleJSON is the Sigstore Bundle bytes (.sigstore.json /
	// attestation.intoto.jsonl equivalent) to attach.
	BundleJSON []byte

	// MainArtifact describes the artifact this referrer points at.
	// All three fields are required: cosign matches on Digest, the
	// registry validates MediaType, and the size completes the
	// subject descriptor per the OCI 1.1 spec.
	MainArtifact MainArtifactDescriptor

	// PlainHTTP forces HTTP (local registry tests only).
	PlainHTTP bool

	// InsecureTLS disables TLS verification (self-signed registries).
	InsecureTLS bool
}

// MainArtifactDescriptor is the subset of an OCI descriptor needed to
// reference an existing artifact as the subject of a Referrer manifest.
type MainArtifactDescriptor struct {
	Digest    string
	MediaType string
	Size      int64
}
