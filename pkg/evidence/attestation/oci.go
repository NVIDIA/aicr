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

	// Reference is an OCI URI like "oci://ghcr.io/myorg/aicr-evidence"
	// (or a non-prefixed equivalent). When the reference omits a tag,
	// the bundle's content digest is used as the tag.
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
		// Placeholder tag; the OCI digest is the canonical address.
		ref = ref.WithTag(defaultEvidenceTag)
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
