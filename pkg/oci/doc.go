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
// The package provides six main operations:
//   - ParseOutputTarget: Parses output targets (file paths or OCI URIs) into Reference
//   - Package: Creates a local OCI artifact in OCI Image Layout format
//   - PushFromStore: Pushes one required immutable descriptor from a local layout
//   - PackageAndPush: Retains layout ownership through remote publication
//   - PackageAndPushHelmChart: Publishes an immutable Helm-compatible OCI chart
//   - PushReferrer: Publishes a single-blob OCI 1.1 referrer
//
// The Reference type encapsulates parsed output target information, making it easy to
// determine if output is destined for the local filesystem or an OCI registry.
//
// # Core Types
//
//   - Reference: Parsed output target (file path or OCI URI with registry/repository/tag)
//   - OutputConfig: Configuration for PackageAndPush workflow
//   - HelmChartOptions: Configuration for PackageAndPushHelmChart
//   - PackageAndPushResult: Combined result of package and push operations
//   - PackageOptions: Configuration for local OCI packaging
//   - PackageResult: Result of local packaging (digest, reference, store path)
//   - PushOptions: Configuration for pushing to remote registries
//   - PushResult: Result of a successful push (digest, reference)
//   - ReferrerOptions: Configuration for referrer publication and optional
//     trusted-root exclusions
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
//	        Reference: ref,
//	        SourceDir: "/path/to/bundle",
//	        OutputDir: "/var/tmp/aicr-oci-layouts",
//	    })
//	    if err != nil {
//	        return err
//	    }
//	    defer func() {
//	        if cleanupErr := os.RemoveAll(result.StorePath); cleanupErr != nil {
//	            slog.Error("failed to remove OCI layout", "error", cleanupErr)
//	        }
//	    }()
//	}
//
// OutputDir must be an existing real directory outside SourceDir. On success,
// the returned StorePath is transferred to the caller, which owns its cleanup.
//
// Publish a Helm-compatible chart without modifying its source:
//
//	helmRef, err := oci.ParseOutputTarget(
//	    "oci://ghcr.io/nvidia/my-chart:1.2.3_build.5")
//	if err != nil {
//	    return err
//	}
//	// Chart.yaml must declare name: my-chart and version: 1.2.3+build.5.
//	result, err := oci.PackageAndPushHelmChart(ctx, oci.HelmChartOptions{
//	    Reference:   helmRef,
//	    SourceDir:   "/path/to/chart",
//	    OutputDir:   "/var/tmp/aicr-oci-layouts",
//	    SourceFiles: []string{"Chart.yaml", "values.yaml", "templates/app.yaml"},
//	    Version:     "v1.0.0", // AICR tool version annotation, not chart SemVer
//	})
//	if err != nil {
//	    return err
//	}
//	defer func() {
//	    if cleanupErr := os.RemoveAll(result.StorePath); cleanupErr != nil {
//	        slog.Error("failed to remove Helm OCI layout", "error", cleanupErr)
//	    }
//	}()
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
//	pushResult, err := oci.PushFromStore(ctx, pkgResult.StorePath, pkgResult.Descriptor, oci.PushOptions{
//	    Registry:   "ghcr.io",
//	    Repository: "nvidia/bundle",
//	    Tag:        "v1.0.0",
//	})
//
// A successful standalone Package transfers ownership of StorePath to the
// caller. PackageAndPush and PackageAndPushHelmChart each retain their unique
// local layout until graph copy, immutable remote-digest verification, and tag
// update have all succeeded; they remove the layout on failure and transfer
// StorePath only on success.
//
// PushFromStore requires the exact manifest descriptor. It verifies that root
// in the local store before invoking any destination operation, copies by
// descriptor rather than by mutable local tag, verifies the remote digest
// descriptor, and only then updates the requested remote tag.
//
// Every graph-copy attempt owns a capability-narrow source wrapper. Readers
// returned by Fetch are tracked, canceled by closing the underlying reader,
// and synchronously finalized before remote resolve or tag operations. A
// storage implementation can still block before Exists or Fetch returns, or
// inside an underlying Close. In particular, cancellation cannot interrupt a
// filesystem Open that blocks synchronously before returning a file handle.
// Callers must supply implementations whose own synchronous methods honor
// their contexts and avoid unresponsive filesystem mounts.
//
// PushReferrer stages its local manifest in a unique private workspace. Callers
// with trusted local roots set ReferrerOptions.ExcludedRoots so staging fails
// closed when the configured temporary directory is equal to, below, or a
// resolved alias of any such root. Store closure and workspace removal are
// checked before a successful result is returned.
//
// # Reference Type
//
// The Reference type represents a parsed output target:
//
//   - IsOCI: True if target is an OCI registry (oci:// scheme)
//   - Registry: OCI registry hostname (e.g., "ghcr.io")
//   - Repository: Image repository path (e.g., "nvidia/bundle")
//   - Tag: Raw Distribution tag (e.g., "1.2.3_build.5")
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
//   - SourceFiles: Nil recursively discovers files; non-nil empty is invalid;
//     non-empty is the exact complete file set
//   - SubDir: Valid only with nil SourceFiles; limits discovery while preserving
//     its source-relative prefix
//
// Generic and Helm publication copy the selected regular files into a private
// stage before packaging and never modify caller source bytes. Explicit paths
// are canonical slash-relative paths. Helm publication must discover Chart.yaml
// when SourceFiles is nil; when SourceFiles is non-nil, that exact set must
// explicitly include Chart.yaml. It also requires the OCI repository basename
// to equal the chart name, preserves the raw Distribution tag in the registry
// reference, and requires its strict SemVer form (for example, 1.2.3+build.5
// for tag 1.2.3_build.5) in Helm metadata.
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
