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

// Package oci provides functionality for packaging and pushing artifacts to OCI-compliant registries.
//
// This package enables bundled artifacts to be pushed to any OCI-compliant registry
// (Docker Hub, GHCR, ECR, local registries, etc.) using the ORAS (OCI Registry As Storage) library.
// Artifacts are packaged as OCI Image Layout format and can be pushed to remote registries.
//
// # Overview
//
// The package provides three main operations:
//   - ParseOutputTarget: Parses output targets (file paths or OCI URIs) into Reference
//   - Package: Creates a local OCI artifact in OCI Image Layout format
//   - PushFromStore: Pushes a previously packaged artifact to a remote registry
//   - PackageAndPush: High-level workflow combining Package and PushFromStore
//
// The Reference type encapsulates parsed output target information, making it easy to
// determine if output is destined for the local filesystem or an OCI registry.
//
// # Core Types
//
//   - Reference: Parsed output target (file path or OCI URI with registry/repository/tag)
//   - OutputConfig: Configuration for PackageAndPush workflow
//   - PackageAndPushResult: Combined result of package and push operations
//   - PackageOptions: Configuration for local OCI packaging
//   - PackageResult: Result of local packaging (digest, reference, store path)
//   - PushOptions: Configuration for pushing to remote registries
//   - PushResult: Result of a successful push (digest, reference)
//
// # URI Scheme
//
// OCI output targets use the "oci://" URI scheme:
//
//	oci://registry/repository:tag
//	oci://ghcr.io/nvidia/bundles:v1.0.0
//	oci://localhost:5000/test/bundle:latest
//
// Local file paths are detected by absence of the oci:// scheme.
//
// # Usage
//
// Parse output target and use high-level workflow:
//
//	ref, err := oci.ParseOutputTarget("oci://ghcr.io/nvidia/bundle:v1.0.0")
//	if err != nil {
//	    return err
//	}
//
//	if ref.IsOCI {
//	    result, err := oci.PackageAndPush(ctx, oci.OutputConfig{
//	        Ref:       ref,
//	        SourceDir: "/path/to/bundle",
//	    })
//	}
//
// Or use low-level Package and PushFromStore separately:
//
//	pkgResult, err := oci.Package(ctx, oci.PackageOptions{
//	    SourceDir:  "/path/to/bundle",
//	    OutputDir:  "/path/to/output",
//	    Registry:   "ghcr.io",
//	    Repository: "nvidia/bundle",
//	    Tag:        "v1.0.0",
//	})
//	if err != nil {
//	    return err
//	}
//
//	pushResult, err := oci.PushFromStore(ctx, pkgResult.StorePath, oci.PushOptions{
//	    Registry:   "ghcr.io",
//	    Repository: "nvidia/bundle",
//	    Tag:        "v1.0.0",
//	})
//
// # Reference Type
//
// The Reference type represents a parsed output target:
//
//   - IsOCI: True if target is an OCI registry (oci:// scheme)
//   - Registry: OCI registry hostname (e.g., "ghcr.io")
//   - Repository: Image repository path (e.g., "nvidia/bundle")
//   - Tag: Image tag (e.g., "v1.0.0")
//   - LocalPath: File system path for non-OCI targets
//
// The Reference.WithTag() method returns a copy with the tag modified, useful for
// applying a default tag when none was specified.
//
// # PackageOptions
//
//   - SourceDir: Directory containing artifacts to package
//   - OutputDir: Where the OCI Image Layout will be created
//   - Registry, Repository, Tag: Image reference components
//   - SubDir: Optionally limit packaging to a subdirectory
//   - ReproducibleTimestamp: Fixed timestamp for reproducible builds
//
// # PushOptions
//
//   - PlainHTTP: Use HTTP instead of HTTPS (for local development registries)
//   - InsecureTLS: Skip TLS certificate verification
//
// # Authentication
//
// The package automatically uses Docker credential helpers for authentication.
// Credentials are loaded from the standard Docker configuration (~/.docker/config.json)
// using the ORAS credentials package.
//
// # Artifact Type
//
// Artifacts are pushed with the media type "application/vnd.nvidia.aicr.artifact".
// This custom media type identifies AICR bundles and distinguishes them from
// runnable container images. Consumers that don't understand this type should
// treat the artifact as a non-executable blob.
package oci
