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

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/oci"
)

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
}

// Push packages a bundle directory as an OCI artifact and pushes it.
// The OCI tag is taken from the supplied reference; when the reference
// has no tag, the digest of the local store is read after Package and
// used as both the tag and the manifest reference returned in
// PushResult.Reference.
func Push(ctx context.Context, opts PushOptions) (*PushResult, error) {
	if opts.SourceDir == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "SourceDir is required")
	}
	if opts.Reference == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "Reference is required")
	}

	ref, err := oci.ParseOutputTarget(oci.EnsureScheme(opts.Reference))
	if err != nil {
		return nil, err
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

	res, err := oci.PackageAndPush(ctx, cfg)
	if err != nil {
		return nil, err
	}

	return &PushResult{
		Reference: res.Reference,
		Digest:    res.Digest,
	}, nil
}

// CleanOCIRef returns the registry/repository:tag form (no oci:// scheme,
// no leading slash). Used by pointer file population so the on-disk YAML
// matches the conventions cosign tooling expects.
func CleanOCIRef(ref string) string {
	return oci.TrimScheme(ref)
}
