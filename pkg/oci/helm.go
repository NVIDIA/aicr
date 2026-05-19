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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/opencontainers/go-digest"
	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"gopkg.in/yaml.v3"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote"

	"github.com/NVIDIA/aicr/pkg/defaults"
	apperrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// Helm OCI registry mediaTypes — pinned per the official Helm OCI spec
// (https://helm.sh/docs/topics/registries/). `helm pull`/`helm install`
// read the manifest config blob's mediaType to discover whether the
// artifact is a Helm chart. When AICR pushes with our generic AICR
// artifactType, helm v3 rejects the artifact during pull discovery and
// reports `unable to locate any tags in provided repository` (see
// #961). Pushing with these constants makes the argocd-helm bundle
// indistinguishable from a chart pushed via `helm push`.
const (
	helmConfigMediaType = "application/vnd.cncf.helm.config.v1+json"
	helmLayerMediaType  = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
)

// chartYAML is intentionally the OCI **config blob** (registry-visible
// metadata), NOT a full Chart.yaml model. Helm itself reads name +
// version off the chart tarball's internal Chart.yaml — the full
// Chart.yaml content ships in the tar layer, so any Helm chart key not
// modeled here is not lost. The config blob mirrors only the fields
// `helm pull` validates and that registries (Harbor, GCR, ghcr) render
// in their artifact-metadata UI.
//
// Do NOT widen this struct to model additional Chart.yaml fields. The
// fix for a missing chart property is to ensure it's in the in-source
// Chart.yaml (which the bundler writes), not to plumb it through the
// config blob.
type chartYAML struct {
	APIVersion  string `yaml:"apiVersion" json:"apiVersion"`
	Name        string `yaml:"name" json:"name"`
	Version     string `yaml:"version" json:"version"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Type        string `yaml:"type,omitempty" json:"type,omitempty"`
	AppVersion  string `yaml:"appVersion,omitempty" json:"appVersion,omitempty"`
}

// HelmChartOptions configures the Helm OCI push flow.
type HelmChartOptions struct {
	// SourceDir is the directory containing the Helm chart (Chart.yaml,
	// values.yaml, templates/, etc.). Mirrors PackageOptions.SourceDir.
	SourceDir string
	// OutputDir is where the temporary OCI Image Layout is created;
	// callers usually pass the bundle output directory so cleanup is
	// scoped to one tree.
	OutputDir string
	// Reference is the OCI registry reference. The reference's Tag MUST
	// match the chart version that helm install / helm pull will look
	// for; the push flow rewrites the in-source Chart.yaml so this
	// invariant holds even when the recipe metadata version differs
	// from the user-supplied --output tag.
	Reference *Reference
	// PlainHTTP uses HTTP instead of HTTPS (local test registries).
	PlainHTTP bool
	// InsecureTLS skips TLS certificate verification.
	InsecureTLS bool
	// Version threads the AICR CLI version into manifest annotations
	// alongside the chart-derived ones. Pure metadata; not consulted
	// by Helm at install time.
	Version string
}

// PackageAndPushHelmChart packages a Helm chart directory into a
// Helm-OCI compatible artifact and pushes it to a registry. Closes the
// gap described in #961: the standard AICR OCI flow uses
// `application/vnd.nvidia.aicr.artifact` for the manifest's
// artifactType and `application/vnd.oci.image.layer.v1.tar+gzip` for
// the layer; helm v3 rejects those during pull discovery and the user
// sees `unable to locate any tags in provided repository` — even
// though the tag exists and `/v2/<name>/tags/list` returns it.
//
// SourceDir is NOT mutated. The Chart.yaml rewrite (so its version
// matches Reference.Tag — see helm's chart-version invariant below)
// happens on a copy inside OutputDir, leaving the caller's source
// tree byte-identical. Earlier revisions of this function rewrote
// SourceDir in place, which (a) leaked the OCI tag back into the
// caller's working copy and (b) was non-atomic — a crash mid-write
// left a corrupt Chart.yaml the next run would refuse to parse.
//
// Helm OCI tags ARE chart versions: `helm install … --version <tag>`
// looks up `<repo>:<tag>` and verifies the chart manifest's version
// against `<tag>`. Without the rewrite, a user who supplies `--output
// oci://…/foo:5bc50950-helm` for a recipe whose metadata version is
// `0.1.0` ends up with `Chart.yaml: 0.1.0` at the `:5bc50950-helm`
// tag, and helm rejects the version mismatch.
func PackageAndPushHelmChart(ctx context.Context, opts HelmChartOptions) (*PackageAndPushResult, error) {
	if opts.Reference == nil || !opts.Reference.IsOCI {
		return nil, apperrors.New(apperrors.ErrCodeInvalidRequest, "OCI reference is required for PackageAndPushHelmChart")
	}
	if opts.Reference.Tag == "" {
		return nil, apperrors.New(apperrors.ErrCodeInvalidRequest, "tag is required for Helm OCI push")
	}
	if err := validateHelmTag(opts.Reference.Tag); err != nil {
		return nil, err
	}
	if err := validateRegistryReference(opts.Reference.Registry, opts.Reference.Repository); err != nil {
		return nil, err
	}

	absSourceDir, err := filepath.Abs(opts.SourceDir)
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to resolve source directory", err)
	}
	absOutputDir, err := filepath.Abs(opts.OutputDir)
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to resolve output directory", err)
	}

	// Stage a working copy of SourceDir so the rewrite + tar steps never
	// mutate the caller's tree. MUST be outside SourceDir: the CLI calls
	// this function with SourceDir == OutputDir, and putting the work
	// dir under either would cause copyDir's recursive walk to copy the
	// freshly-created work dir back into itself (observed: ~250 nested
	// `helm-chart-work/helm-chart-work/...` levels until ENAMETOOLONG).
	// Use the OS temp area instead and clean up unconditionally.
	workDir, mkdirErr := os.MkdirTemp("", "aicr-helm-chart-")
	if mkdirErr != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to create Helm staging dir", mkdirErr)
	}
	defer func() { _ = os.RemoveAll(workDir) }()
	if copyErr := copyDir(ctx, absSourceDir, workDir); copyErr != nil {
		return nil, apperrors.PropagateOrWrap(copyErr, apperrors.ErrCodeInternal,
			"failed to stage Helm chart source for OCI push")
	}

	chartMeta, err := loadAndRewriteChartYAML(workDir, opts.Reference.Tag)
	if err != nil {
		return nil, err
	}

	slog.Info("packaging Helm chart for OCI push",
		"registry", opts.Reference.Registry,
		"repository", opts.Reference.Repository,
		"tag", opts.Reference.Tag,
		"chart_name", chartMeta.Name,
		"chart_version", chartMeta.Version,
	)

	chartTGZ, err := buildHelmChartTGZ(ctx, workDir, chartMeta.Name)
	if err != nil {
		return nil, err
	}

	configBlob, err := json.Marshal(chartMeta)
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to marshal Helm chart config", err)
	}

	storePath := filepath.Join(absOutputDir, "oci-layout-helm")
	if mkdirErr := os.MkdirAll(storePath, 0o755); mkdirErr != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to create Helm OCI store directory", mkdirErr)
	}
	store, err := oci.New(storePath)
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to create Helm OCI store", err)
	}

	manifestDesc, err := stageHelmOCIManifest(ctx, store, chartMeta, chartTGZ, configBlob, opts.Version)
	if err != nil {
		return nil, err
	}
	if tagErr := store.Tag(ctx, manifestDesc, opts.Reference.Tag); tagErr != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to tag Helm OCI manifest", tagErr)
	}

	pushResult, err := pushHelmOCIFromStore(ctx, opts, store)
	if err != nil {
		return nil, err
	}

	return &PackageAndPushResult{
		Digest:    pushResult.Digest,
		MediaType: pushResult.MediaType,
		Size:      pushResult.Size,
		Reference: pushResult.Reference,
		StorePath: storePath,
	}, nil
}

// validateHelmTag rejects OCI tags that helm v3 won't accept as chart
// versions. Helm's registry client filters tags via
// `semver.StrictNewVersion` (pkg/registry/client.go::Tags()) — any tag
// that doesn't parse as semver is silently dropped, and `helm pull` /
// `helm install --version <tag>` reports `unable to locate any tags in
// provided repository` even when the artifact exists in the registry
// with correct Helm OCI mediaTypes. Returning a typed error here
// converts that silent helm-side failure into an actionable
// bundle-time error with the constraint spelled out.
//
// One concession to common usage: a leading `v` (e.g. `v1.2.3`) is
// stripped before the parse because helm itself strips it (see
// `pkg/registry/client.go::Tags()` → `strings.TrimPrefix(tag, "v")` is
// implicit in the version compare path). Users who reach for `v1.2.3`
// should not be punished for a stylistic choice helm accepts.
func validateHelmTag(tag string) error {
	candidate := strings.TrimPrefix(tag, "v")
	if _, err := semver.StrictNewVersion(candidate); err != nil {
		return apperrors.New(apperrors.ErrCodeInvalidRequest,
			fmt.Sprintf("Helm OCI requires a semver-compatible tag; got %q. "+
				"`helm pull` / `helm install --version <tag>` filter tags via "+
				"semver.StrictNewVersion and silently drop non-semver tags, "+
				"surfacing as `unable to locate any tags in provided repository`. "+
				"Wrap arbitrary identifiers as a semver pre-release, e.g. "+
				"`0.0.0-%s`.", tag, tag))
	}
	return nil
}

// loadAndRewriteChartYAML reads Chart.yaml from sourceDir, sets its
// version to tag, and writes the result back. Returns the parsed chart
// metadata so callers can build the OCI config blob without re-parsing.
//
// The rewrite is intentional and idempotent — see
// PackageAndPushHelmChart for the chart-version-equals-OCI-tag
// invariant.
func loadAndRewriteChartYAML(sourceDir, tag string) (*chartYAML, error) {
	chartPath := filepath.Join(sourceDir, "Chart.yaml")

	// Bounded read: SourceDir is caller-supplied, so a hostile symlink
	// (/proc, NFS, FUSE) could trick os.ReadFile into allocating the
	// whole file into memory before yaml.Unmarshal would notice. Open
	// + LimitReader against the Chart.yaml-shaped cap from pkg/defaults
	// keeps the allocation bounded even under an attacker-influenced
	// source path. See CLAUDE.md anti-pattern table for the rule.
	f, err := os.Open(chartPath) //nolint:gosec // path is under caller-supplied source dir
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInvalidRequest, "failed to open Chart.yaml from source directory", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, defaults.MaxChartYAMLBytes+1))
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInvalidRequest, "failed to read Chart.yaml from source directory", err)
	}
	if int64(len(data)) > defaults.MaxChartYAMLBytes {
		return nil, apperrors.New(apperrors.ErrCodeInvalidRequest,
			fmt.Sprintf("Chart.yaml exceeds %d bytes — refusing to read unbounded source", defaults.MaxChartYAMLBytes))
	}

	var meta chartYAML
	if unmarshalErr := yaml.Unmarshal(data, &meta); unmarshalErr != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInvalidRequest, "failed to parse Chart.yaml", unmarshalErr)
	}
	if meta.Name == "" {
		return nil, apperrors.New(apperrors.ErrCodeInvalidRequest, "Chart.yaml: name is required")
	}
	if nameErr := validateChartName(meta.Name); nameErr != nil {
		return nil, nameErr
	}
	if meta.APIVersion == "" {
		meta.APIVersion = "v2"
	}
	meta.Version = tag

	// The marshaled bytes get written back to Chart.yaml, which is then
	// tarred into the chart.tgz that becomes the OCI artifact's layer
	// blob. The layer digest is consumed by helm OCI's content-
	// addressable cache and by any downstream attestation. Deterministic
	// marshal is therefore mandatory — the chartYAML struct itself is
	// stable, but `yaml.v3` makes formatting choices (block-vs-flow
	// scalar style, line wrapping) that can vary; using the project's
	// deterministic serializer is the lighter belt-and-suspenders
	// matching the rest of the digest-feeding paths in this repo.
	rewritten, err := serializer.MarshalYAMLDeterministic(meta)
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to marshal Chart.yaml", err)
	}
	if err := os.WriteFile(chartPath, rewritten, 0o600); err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to rewrite Chart.yaml with OCI tag version", err)
	}
	return &meta, nil
}

// validateChartName rejects chart names whose layout would let the tar
// entry prefix (`<chartName>/<rel>`) escape the chart root on extraction.
// Helm itself enforces the Chart.yaml name to be `[a-z0-9-]+` at install
// time, but the OCI push path here is exposed as a public function and
// a future caller with a name derived from less-validated input could
// otherwise produce a tarball with absolute or parent-traversal entries.
//
// filepath.IsLocal catches `/`, `..`, drive letters, and parent
// references; the extra ContainsAny adds belt-and-suspenders against
// control characters and slashes (IsLocal accepts an embedded NUL on
// some platforms).
func validateChartName(name string) error {
	if name == "" {
		return apperrors.New(apperrors.ErrCodeInvalidRequest, "chart name is required")
	}
	if !filepath.IsLocal(name) || strings.ContainsAny(name, "/\\\x00") {
		return apperrors.New(apperrors.ErrCodeInvalidRequest,
			fmt.Sprintf("chart name %q contains unsafe characters — must be a single path segment without `/`, `..`, or control chars", name))
	}
	return nil
}

// buildHelmChartTGZ produces a Helm-conformant chart.tgz: a gzipped tar
// where every entry is prefixed with `<chart-name>/` (the Helm "chart
// root" convention). `helm package` produces exactly this layout; helm
// v3's pull / install both depend on the prefix so the chart's
// templates/ resolution roots correctly inside the extracted directory.
//
// Reproducible-build hygiene: mtimes are zeroed and uid/gid/uname/
// gname are stripped (left at their zero values) so two AICR runs
// against the same source tree produce the same chart.tgz digest,
// which the OCI registry then dedups.
//
// ctx is checked on entry and per directory entry so a parent timeout
// (push deadline, server shutdown) terminates the walk without
// orphaning a partially-written buffer.
func buildHelmChartTGZ(ctx context.Context, sourceDir, chartName string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeUnavailable, "chart packaging canceled", err)
	}
	// Defense-in-depth: loadAndRewriteChartYAML already validated the
	// name, but this function is exported and a future caller might
	// construct chartName from a different source. Reject here too so
	// the tar-entry prefix is always a safe single segment.
	if err := validateChartName(chartName); err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	walkErr := filepath.Walk(sourceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		rel, relErr := filepath.Rel(sourceDir, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			// Skip the synthetic chart-root directory entry; helm v3
			// extracts by file path and tolerates its absence, and
			// emitting it adds noise to the tar that defeats the
			// reproducible-digest goal whenever a tool inserts/omits it.
			return nil
		}
		entry := chartName + "/" + filepath.ToSlash(rel)

		if info.IsDir() {
			hdr := &tar.Header{
				Name:     entry + "/",
				Mode:     0o755,
				Typeflag: tar.TypeDir,
			}
			return tw.WriteHeader(hdr)
		}
		// Skip non-regular files (symlinks, sockets, etc.) — chart
		// bundles produced by argocdhelm are always plain files;
		// surfacing the rest defensively avoids pushing an unsupported
		// tar entry the registry would otherwise accept silently.
		if !info.Mode().IsRegular() {
			return nil
		}
		hdr := &tar.Header{
			Name:     entry,
			Mode:     int64(info.Mode().Perm()),
			Size:     info.Size(),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, openErr := os.Open(filepath.Clean(path)) //nolint:gosec // walking caller-supplied source dir
		if openErr != nil {
			return openErr
		}
		_, copyErr := io.Copy(tw, f)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if walkErr != nil {
		// filepath.Walk callbacks return ctx.Err() on cancellation
		// (see the entry-point check at the top of the closure).
		// Surfacing those as ErrCodeInternal would misclassify a
		// caller-initiated cancel or a parent-timeout as a bug, and
		// flatten the CI's distinction between "chart packaging
		// failed" and "parent context expired". Classify by the
		// inner error so the same path stays ErrCodeInternal for
		// real I/O failures and ErrCodeTimeout for cancellation.
		if stderrors.Is(walkErr, context.Canceled) || stderrors.Is(walkErr, context.DeadlineExceeded) {
			return nil, apperrors.PropagateOrWrap(walkErr, apperrors.ErrCodeTimeout, "chart packaging canceled")
		}
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to build Helm chart tarball", walkErr)
	}
	if err := tw.Close(); err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to close tar writer", err)
	}
	if err := gz.Close(); err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to close gzip writer", err)
	}
	return buf.Bytes(), nil
}

// stageHelmOCIManifest pushes the chart layer + config blob into store
// and packs an OCI 1.0 manifest with the Helm config mediaType. v1.0
// is intentional: helm v3's pull path reads manifest.config.mediaType
// to discover the chart artifactType. v1.1's separate `artifactType`
// field with an empty config works for some registries but not all
// (notably older registry:2 builds observed in #961's repro); v1.0
// keeps the legacy interpretation where the config blob's mediaType
// IS the artifact type.
func stageHelmOCIManifest(ctx context.Context, store *oci.Store, meta *chartYAML, chartTGZ, configBlob []byte, aicrVersion string) (ociv1.Descriptor, error) {
	layerDesc := ociv1.Descriptor{
		MediaType: helmLayerMediaType,
		Digest:    digest.FromBytes(chartTGZ),
		Size:      int64(len(chartTGZ)),
	}
	if err := store.Push(ctx, layerDesc, bytes.NewReader(chartTGZ)); err != nil {
		return ociv1.Descriptor{}, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to add chart layer to OCI store", err)
	}

	configDesc := ociv1.Descriptor{
		MediaType: helmConfigMediaType,
		Digest:    digest.FromBytes(configBlob),
		Size:      int64(len(configBlob)),
	}
	if err := store.Push(ctx, configDesc, bytes.NewReader(configBlob)); err != nil {
		return ociv1.Descriptor{}, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to add chart config to OCI store", err)
	}

	annotations := map[string]string{
		ociv1.AnnotationCreated:           reproducibleTimestamp,
		ociv1.AnnotationTitle:             meta.Name,
		ociv1.AnnotationVersion:           meta.Version,
		"org.opencontainers.image.vendor": "NVIDIA",
	}
	if meta.Description != "" {
		annotations[ociv1.AnnotationDescription] = meta.Description
	}
	if aicrVersion != "" {
		annotations["com.nvidia.aicr.version"] = aicrVersion
	}

	packOpts := oras.PackManifestOptions{
		Layers:              []ociv1.Descriptor{layerDesc},
		ConfigDescriptor:    &configDesc,
		ManifestAnnotations: annotations,
	}
	manifestDesc, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_0, helmConfigMediaType, packOpts)
	if err != nil {
		return ociv1.Descriptor{}, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to pack Helm OCI manifest", err)
	}
	return manifestDesc, nil
}

// pushHelmOCIFromStore uploads the staged manifest tree from an OCI
// Image Layout to the remote registry. Wraps the same retry policy
// as the generic AICR push path so the two flows share transient-
// failure handling.
func pushHelmOCIFromStore(ctx context.Context, opts HelmChartOptions, store *oci.Store) (*PushResult, error) {
	registryHost := stripProtocol(opts.Reference.Registry)
	refString := fmt.Sprintf("%s/%s:%s", registryHost, opts.Reference.Repository, opts.Reference.Tag)

	repo, err := remote.NewRepository(fmt.Sprintf("%s/%s", registryHost, opts.Reference.Repository))
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to initialize remote repository", err)
	}
	repo.PlainHTTP = opts.PlainHTTP

	authClient, err := createAuthClientForHost(registryHost, opts.PlainHTTP, opts.InsecureTLS)
	if err != nil {
		slog.Warn("failed to initialize Docker credential store, continuing without authentication", "error", err)
	}
	repo.Client = authClient

	copyOpts := oras.DefaultCopyOptions
	copyOpts.Concurrency = defaults.OCIPushConcurrency
	desc, err := copyWithRetry(ctx, store, opts.Reference.Tag, repo, opts.Reference.Tag, copyOpts, oras.Copy)
	if err != nil {
		return nil, err
	}

	return &PushResult{
		Digest:    desc.Digest.String(),
		MediaType: desc.MediaType,
		Size:      desc.Size,
		Reference: refString,
	}, nil
}
