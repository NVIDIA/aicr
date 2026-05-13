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

package oci

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/distribution/reference"

	apperrors "github.com/NVIDIA/aicr/pkg/errors"
)

// URIScheme is the URI scheme for OCI registry references
// (e.g., "oci://ghcr.io/org/repo:tag"). Exported so other packages can
// build references without re-declaring the literal.
const URIScheme = "oci://"

// uriScheme is the unexported alias retained for legacy in-package use.
const uriScheme = URIScheme

// EnsureScheme returns ref with the oci:// prefix added when missing.
// Used by callers that accept user input in either form.
//
// Refs that already carry a different URI scheme (e.g., "https://...")
// are returned unchanged so callers don't accidentally build
// "oci://https://..." when handed a non-oci URL by mistake.
func EnsureScheme(ref string) string {
	if strings.HasPrefix(ref, URIScheme) {
		return ref
	}
	if strings.Contains(ref, "://") {
		return ref
	}
	return URIScheme + ref
}

// TrimScheme returns ref with any oci:// prefix removed. Useful when
// emitting a registry/repo:tag form for cosign or for human-readable
// pointers that don't carry the URI scheme.
func TrimScheme(ref string) string {
	return strings.TrimPrefix(ref, URIScheme)
}

// Reference represents a parsed output target, which can be either an OCI registry
// reference or a local directory path.
type Reference struct {
	// IsOCI indicates whether this is an OCI registry reference (true) or local path (false).
	IsOCI bool
	// Registry is the OCI registry host (e.g., "ghcr.io", "localhost:5000").
	// Only populated when IsOCI is true.
	Registry string
	// Repository is the image repository path (e.g., "nvidia/bundle").
	// Only populated when IsOCI is true.
	Repository string
	// Tag is the image tag (e.g., "v1.0.0").
	// Empty string means no tag was specified; caller should apply a default.
	// Only populated when IsOCI is true.
	Tag string
	// LocalPath is the local directory path for non-OCI output.
	// Only populated when IsOCI is false.
	LocalPath string
}

// ParseOutputTarget parses an output target string to detect OCI URI or local directory.
// For OCI URIs (oci://registry/repository:tag), it extracts the components.
// For plain paths, it treats them as local directories.
//
// If no tag is specified in an OCI URI, Tag will be empty; the caller is responsible
// for applying a default (e.g., CLI version).
func ParseOutputTarget(target string) (*Reference, error) {
	if !strings.HasPrefix(target, uriScheme) {
		return &Reference{
			IsOCI:     false,
			LocalPath: target,
		}, nil
	}

	// Strip oci:// and parse as standard image reference
	ref, err := reference.ParseNormalizedNamed(strings.TrimPrefix(target, uriScheme))
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInvalidRequest, "invalid OCI reference", err)
	}

	// Extract components using the reference package
	registry := reference.Domain(ref)
	repository := reference.Path(ref)

	var tag string
	if tagged, ok := ref.(reference.Tagged); ok {
		tag = tagged.Tag()
	}
	// If no tag specified, return empty string; caller will apply default

	// Validate registry and repository format
	if err := validateRegistryReference(registry, repository); err != nil {
		return nil, err
	}

	return &Reference{
		IsOCI:      true,
		Registry:   registry,
		Repository: repository,
		Tag:        tag,
	}, nil
}

// String returns the full reference string.
// For OCI references: "oci://registry/repository:tag" (or without tag if empty).
// For local paths: the local path.
func (r *Reference) String() string {
	if !r.IsOCI {
		return r.LocalPath
	}
	if r.Tag == "" {
		return fmt.Sprintf("%s%s/%s", uriScheme, r.Registry, r.Repository)
	}
	return fmt.Sprintf("%s%s/%s:%s", uriScheme, r.Registry, r.Repository, r.Tag)
}

// WithTag returns a copy of the reference with the specified tag.
// For non-OCI references, returns the same reference unchanged.
func (r *Reference) WithTag(tag string) *Reference {
	if !r.IsOCI {
		return r
	}
	return &Reference{
		IsOCI:      true,
		Registry:   r.Registry,
		Repository: r.Repository,
		Tag:        tag,
	}
}

// OutputConfig configures the OCI package and push workflow.
type OutputConfig struct {
	// SourceDir is the directory containing artifacts to package.
	SourceDir string
	// OutputDir is where temporary OCI artifacts will be created.
	OutputDir string
	// Reference contains the parsed OCI registry reference.
	Reference *Reference
	// Version is used for OCI annotations (org.opencontainers.image.version).
	Version string
	// PlainHTTP uses HTTP instead of HTTPS for the registry connection.
	PlainHTTP bool
	// InsecureTLS skips TLS certificate verification.
	InsecureTLS bool
	// Annotations are additional manifest annotations to include.
	// If nil, default AICR annotations will be used.
	Annotations map[string]string
}

// PackageAndPushResult contains the result of a successful package and push operation.
type PackageAndPushResult struct {
	// Digest is the SHA256 digest of the pushed artifact.
	Digest string
	// MediaType is the manifest media type.
	MediaType string
	// Size is the manifest's byte length. Surfaced for OCI Referrers
	// attachment, which needs a full subject descriptor.
	Size int64
	// Reference is the full image reference (registry/repository:tag).
	Reference string
	// StorePath is the path to the local OCI Image Layout directory.
	StorePath string
}

// PackageAndPush packages a directory as an OCI artifact and pushes it to a registry.
// This is a convenience function that combines Package and PushFromStore operations.
func PackageAndPush(ctx context.Context, cfg OutputConfig) (*PackageAndPushResult, error) {
	if cfg.Reference == nil || !cfg.Reference.IsOCI {
		return nil, apperrors.New(apperrors.ErrCodeInvalidRequest, "OCI reference is required for PackageAndPush")
	}

	if cfg.Reference.Tag == "" {
		return nil, apperrors.New(apperrors.ErrCodeInvalidRequest, "tag is required for OCI packaging")
	}

	absSourceDir, err := filepath.Abs(cfg.SourceDir)
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to resolve source directory", err)
	}

	absOutputDir, err := filepath.Abs(cfg.OutputDir)
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to resolve output directory", err)
	}

	slog.Info("packaging and pushing bundle as OCI artifact",
		"registry", cfg.Reference.Registry,
		"repository", cfg.Reference.Repository,
		"tag", cfg.Reference.Tag,
	)

	// Build annotations
	annotations := cfg.Annotations
	if annotations == nil {
		annotations = map[string]string{
			"org.opencontainers.image.version": cfg.Version,
			"org.opencontainers.image.vendor":  "NVIDIA",
			"org.opencontainers.image.title":   "AICR Bundle",
			"org.opencontainers.image.source":  "https://github.com/NVIDIA/aicr",
		}
	}

	// Package locally first. Package returns properly coded errors
	// (ErrCodeInvalidRequest, ErrCodeInternal, ErrCodeUnavailable); preserve
	// the inner code rather than re-wrapping with ErrCodeInternal.
	packageResult, err := Package(ctx, PackageOptions{
		SourceDir:   absSourceDir,
		OutputDir:   absOutputDir,
		Registry:    cfg.Reference.Registry,
		Repository:  cfg.Reference.Repository,
		Tag:         cfg.Reference.Tag,
		Annotations: annotations,
	})
	if err != nil {
		return nil, err
	}

	slog.Info("OCI artifact packaged locally",
		"reference", packageResult.Reference,
		"digest", packageResult.Digest,
		"store_path", packageResult.StorePath,
	)

	// Push to remote registry
	slog.Info("pushing OCI artifact to remote registry",
		"registry", cfg.Reference.Registry,
		"repository", cfg.Reference.Repository,
		"tag", cfg.Reference.Tag,
	)

	// PushFromStore returns properly coded errors (ErrCodeUnavailable for
	// transient/canceled, ErrCodeInternal otherwise); preserve the code.
	pushResult, pushErr := PushFromStore(ctx, packageResult.StorePath, PushOptions{
		Registry:    cfg.Reference.Registry,
		Repository:  cfg.Reference.Repository,
		Tag:         cfg.Reference.Tag,
		PlainHTTP:   cfg.PlainHTTP,
		InsecureTLS: cfg.InsecureTLS,
	})
	if pushErr != nil {
		return nil, pushErr
	}

	slog.Info("OCI artifact pushed successfully",
		"reference", pushResult.Reference,
		"digest", pushResult.Digest,
	)

	return &PackageAndPushResult{
		Digest:    pushResult.Digest,
		MediaType: pushResult.MediaType,
		Size:      pushResult.Size,
		Reference: pushResult.Reference,
		StorePath: packageResult.StorePath,
	}, nil
}
