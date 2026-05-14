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
	"crypto/tls"
	"encoding/json"
	stderrors "errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/distribution/reference"
	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/file"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

// errReferrerFound is an unexported sentinel used to unwind the
// pagination callback in repo.Referrers after the first matching
// Sigstore Bundle referrer has been consumed. Without this sentinel,
// `return nil` from the callback only stops the current page; the
// next page would re-enter and overwrite attestation.intoto.jsonl.
var errReferrerFound = stderrors.New("referrer found")

// maxReferrerManifestBytes caps the in-memory read of a referrer
// manifest JSON. Manifests are small (KiB); anything past this is a
// bug or hostile.
const maxReferrerManifestBytes = 1 << 20 // 1 MiB

// MaterializedBundle is the verifier's view of a bundle on local disk.
type MaterializedBundle struct {
	// BundleDir is the local directory containing recipe.yaml,
	// manifest.json, ctrf/*, etc. Always populated.
	BundleDir string

	// Reference and Digest are populated when the bundle came from an
	// OCI source. Reference is the canonical registry/repo:tag string;
	// Digest is the resolved OCI manifest digest ("sha256:...").
	Reference string
	Digest    string

	cleanup func()
}

// Cleanup releases any temporary directories the verifier created.
func (m *MaterializedBundle) Cleanup() {
	if m == nil || m.cleanup == nil {
		return
	}
	m.cleanup()
	m.cleanup = nil
}

// MaterializeBundle dispatches on InputForm. Returns a directory the
// rest of the verifier reads from, plus optional OCI provenance.
func MaterializeBundle(
	ctx context.Context,
	opts VerifyOptions,
	form InputForm,
	pointer *attestation.Pointer,
) (*MaterializedBundle, error) {

	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable, "materialize canceled", err)
	}
	switch form {
	case InputFormDir:
		return materializeDir(opts.Input)
	case InputFormPointer:
		return materializeFromPointer(ctx, pointer, opts)
	case InputFormOCI:
		// Direct OCI input has no external digest pin; refuse tag-only
		// refs unless the operator explicitly opted in. Pointer-driven
		// pulls have their own check below using pointer.bundle.digest.
		return materializeOCIRefRequireDigest(ctx, opts.Input, opts, !opts.AllowUnpinnedTag)
	default:
		return nil, errors.New(errors.ErrCodeInvalidRequest, "unknown input form "+string(form))
	}
}

// materializeDir accepts either the summary-bundle root or a parent
// containing it. Bundles are recognized by recipe.yaml + manifest.json
// at the candidate root.
func materializeDir(input string) (*MaterializedBundle, error) {
	if hasBundleMarkers(input) {
		return &MaterializedBundle{BundleDir: filepath.Clean(input)}, nil
	}
	candidate := filepath.Join(input, attestation.SummaryBundleDirName)
	if hasBundleMarkers(candidate) {
		return &MaterializedBundle{BundleDir: filepath.Clean(candidate)}, nil
	}
	return nil, errors.New(errors.ErrCodeInvalidRequest,
		"directory "+input+" does not look like a summary bundle "+
			"(no recipe.yaml / manifest.json at root or under summary-bundle/)")
}

func hasBundleMarkers(dir string) bool {
	for _, f := range []string{attestation.RecipeFilename, attestation.ManifestFilename} {
		info, err := os.Stat(filepath.Join(dir, f))
		if err != nil || info.IsDir() {
			return false
		}
	}
	return true
}

// materializeFromPointer pulls the OCI artifact named in the pointer's
// first attestation, or falls back to opts.BundleRef when the pointer
// has no OCI ref. The pointer's digest claim is cross-checked against
// the actual pulled digest. A tag-only ref is allowed only when the
// pointer carries a sha256-prefixed bundle.digest (the validator in
// pointer.go rejects any other shape when bundle.oci is set) — that
// digest is the pin.
func materializeFromPointer(
	ctx context.Context,
	pointer *attestation.Pointer,
	opts VerifyOptions,
) (*MaterializedBundle, error) {

	if pointer == nil || len(pointer.Attestations) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "pointer has no attestations")
	}
	att := pointer.Attestations[0]
	ref := att.Bundle.OCI
	if ref == "" {
		ref = opts.BundleRef
	}
	if ref == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"pointer carries no bundle.oci — re-run with --bundle <oci-ref> or point at the unpacked directory")
	}
	// Pointer-driven path: pointer.bundle.digest is the pin. When it
	// is empty (e.g., a local-only pointer plus --bundle override),
	// fall through to the direct-OCI digest-pinning rule.
	requirePin := !opts.AllowUnpinnedTag && att.Bundle.Digest == ""
	mat, err := materializeOCIRefRequireDigest(ctx, ref, opts, requirePin)
	if err != nil {
		return nil, err
	}
	if att.Bundle.Digest != "" && mat.Digest != "" && att.Bundle.Digest != mat.Digest {
		mat.Cleanup()
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"pointer digest "+att.Bundle.Digest+" does not match pulled digest "+mat.Digest)
	}
	return mat, nil
}

// materializeOCIRefRequireDigest pulls an OCI artifact into a temp
// directory using oras.Copy. When requirePin is true the reference
// must resolve to a digest (not a bare tag) — registry-rewritable
// tags are not content-addressable and would let a registry compromise
// substitute the artifact.
//
// The file store unpacks the gzip-tar layer the emitter writes, so the
// result is the bundle tree on disk.
func materializeOCIRefRequireDigest(ctx context.Context, ref string, opts VerifyOptions, requirePin bool) (*MaterializedBundle, error) {
	registry, repo, refTarget, err := parseOCIReference(ref)
	if err != nil {
		return nil, err
	}
	if requirePin && !isDigestPinned(refTarget) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"OCI reference "+ref+" is tag-only — refusing to pull an unpinned reference. "+
				"Use a digest-bound reference (registry/repo@sha256:<hex>), supply a pointer with "+
				"bundle.digest set, or pass --allow-unpinned-tag for one-off debugging.")
	}

	tmp, err := os.MkdirTemp("", "aicr-evidence-pull-")
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create temp dir for OCI pull", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }

	fs, fsErr := file.New(tmp)
	if fsErr != nil {
		cleanup()
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create file store for OCI pull", fsErr)
	}
	defer func() { _ = fs.Close() }()

	remoteRepo, rErr := remote.NewRepository(registry + "/" + repo)
	if rErr != nil {
		cleanup()
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to initialize remote repository", rErr)
	}
	remoteRepo.PlainHTTP = opts.PlainHTTP
	remoteRepo.Client = newAuthClient(opts.PlainHTTP, opts.InsecureTLS)

	pullCtx, pullCancel := context.WithTimeout(ctx, defaults.EvidenceBundlePushTimeout)
	defer pullCancel()
	desc, copyErr := oras.Copy(pullCtx, remoteRepo, refTarget, fs, refTarget, oras.DefaultCopyOptions)
	if copyErr != nil {
		cleanup()
		return nil, errors.Wrap(errors.ErrCodeUnavailable, "OCI pull failed", copyErr)
	}

	resolved, dErr := resolveBundleDir(tmp)
	if dErr != nil {
		cleanup()
		return nil, dErr
	}

	// The Sigstore Bundle is attached as an OCI Referrer of the main
	// artifact, not part of the artifact's own layers. Discover and
	// stage it as attestation.intoto.jsonl so signature verification
	// can read it from disk the same way it does for directory input.
	//
	// "No referrer at all" is a legitimate unsigned-bundle state →
	// debug-log and let the signature step record Skipped. ANY other
	// error (malformed manifest, oversized layer, registry returning
	// junk) is fail-closed: a registry that mid-MITMs the Referrers
	// response could otherwise silently downgrade a signed bundle to
	// "unsigned."
	if err := discoverAndWriteReferrer(pullCtx, remoteRepo, desc, resolved); err != nil {
		if stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
			slog.Debug("no Sigstore Bundle referrer discovered",
				"reference", registry+"/"+repo)
		} else {
			cleanup()
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				"registry returned a malformed Sigstore Bundle referrer", err)
		}
	}

	return &MaterializedBundle{
		BundleDir: resolved,
		Reference: formatOCIReference(registry, repo, refTarget),
		Digest:    desc.Digest.String(),
		cleanup:   cleanup,
	}, nil
}

// formatOCIReference assembles a canonical OCI reference from its
// parts. Digest targets get separated by "@" per the OCI spec; tag
// targets use ":". Joining with ":" unconditionally produces invalid
// refs like "registry/repo:sha256:..." for digest pulls.
func formatOCIReference(registry, repo, target string) string {
	sep := ":"
	if isDigestPinned(target) {
		sep = "@"
	}
	return registry + "/" + repo + sep + target
}

// referrerFetcher is the minimal subset of *remote.Repository that
// fetchAndWriteReferrerLayer uses. Exists so tests can substitute an
// in-memory fake without spinning up a real registry.
type referrerFetcher interface {
	Fetch(ctx context.Context, target ociv1.Descriptor) (io.ReadCloser, error)
}

// discoverAndWriteReferrer queries the Referrers API for a Sigstore
// Bundle attached to the subject artifact, fetches its single layer,
// and writes it to bundleDir as attestation.intoto.jsonl.
//
// Returns ErrCodeNotFound when no Sigstore Bundle referrer is present;
// callers treat that as "unsigned bundle." Other errors propagate.
//
// "Take the first matching referrer" is enforced via an unexported
// errReferrerFound sentinel — `return nil` from the callback would
// only stop the current page, letting a later page overwrite the file.
func discoverAndWriteReferrer(ctx context.Context, repo *remote.Repository, subject ociv1.Descriptor, bundleDir string) error {
	cbErr := repo.Referrers(ctx, subject, attestation.SigstoreBundleMediaType,
		func(refs []ociv1.Descriptor) error {
			for _, r := range refs {
				if r.ArtifactType != attestation.SigstoreBundleMediaType {
					continue
				}
				if err := fetchAndWriteReferrerLayer(ctx, repo, r, bundleDir); err != nil {
					return err
				}
				// Multi-signature bundles aren't a V1 case; if one ever
				// lands, we'd need a selection policy. For now: first
				// match wins and stops pagination.
				return errReferrerFound
			}
			return nil
		})
	if stderrors.Is(cbErr, errReferrerFound) {
		return nil
	}
	if cbErr != nil {
		return errors.Wrap(errors.ErrCodeUnavailable, "referrers query failed", cbErr)
	}
	return errors.New(errors.ErrCodeNotFound, "no Sigstore Bundle referrer for artifact")
}

// fetchAndWriteReferrerLayer pulls the referrer's manifest, extracts
// its single layer descriptor, fetches the layer blob (the Sigstore
// Bundle bytes), and writes them to attestation.intoto.jsonl.
func fetchAndWriteReferrerLayer(ctx context.Context, repo referrerFetcher, referrerDesc ociv1.Descriptor, bundleDir string) error {
	manifestRdr, err := repo.Fetch(ctx, referrerDesc)
	if err != nil {
		return errors.Wrap(errors.ErrCodeUnavailable, "failed to fetch referrer manifest", err)
	}
	defer func() { _ = manifestRdr.Close() }()

	manifestBytes, err := io.ReadAll(io.LimitReader(manifestRdr, maxReferrerManifestBytes+1))
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to read referrer manifest", err)
	}
	if int64(len(manifestBytes)) > maxReferrerManifestBytes {
		return errors.New(errors.ErrCodeInvalidRequest,
			"referrer manifest exceeds size limit")
	}

	var manifest ociv1.Manifest
	if uErr := json.Unmarshal(manifestBytes, &manifest); uErr != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest,
			"referrer manifest is not valid JSON", uErr)
	}
	if len(manifest.Layers) != 1 {
		return errors.New(errors.ErrCodeInvalidRequest,
			"expected single-layer Sigstore Bundle referrer manifest")
	}
	layerDesc := manifest.Layers[0]
	if layerDesc.Size > defaults.MaxSigstoreBundleSize {
		return errors.New(errors.ErrCodeInvalidRequest,
			"referrer layer exceeds Sigstore Bundle size limit")
	}

	layerRdr, err := repo.Fetch(ctx, layerDesc)
	if err != nil {
		return errors.Wrap(errors.ErrCodeUnavailable, "failed to fetch referrer layer", err)
	}
	defer func() { _ = layerRdr.Close() }()

	outPath := filepath.Join(bundleDir, attestation.AttestationFilename)
	out, err := os.Create(outPath) //nolint:gosec // verifier-controlled temp dir
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create attestation file", err)
	}
	if _, copyErr := io.Copy(out, io.LimitReader(layerRdr, defaults.MaxSigstoreBundleSize)); copyErr != nil {
		_ = out.Close()
		return errors.Wrap(errors.ErrCodeInternal, "failed to write attestation file", copyErr)
	}
	if closeErr := out.Close(); closeErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to close attestation file", closeErr)
	}
	return nil
}

// resolveBundleDir picks the bundle root from a temp dir holding the
// pulled or extracted layer contents.
func resolveBundleDir(dir string) (string, error) {
	if hasBundleMarkers(dir) {
		return dir, nil
	}
	candidate := filepath.Join(dir, attestation.SummaryBundleDirName)
	if hasBundleMarkers(candidate) {
		return candidate, nil
	}
	return "", errors.New(errors.ErrCodeInvalidRequest,
		"pulled artifact does not contain a recognizable summary bundle")
}

// isDigestPinned reports whether an OCI reference target (the tag-or-
// digest portion ORAS uses) is content-addressed. Digest targets are
// "sha256:<hex>"; tag targets are anything else.
func isDigestPinned(target string) bool {
	return strings.HasPrefix(target, "sha256:")
}

// parseOCIReference splits a reference into (registry, repository, target).
// target is the tag or digest portion ORAS resolves against the remote.
func parseOCIReference(ref string) (registry, repo, target string, err error) {
	clean := strings.TrimPrefix(ref, "oci://")
	named, parseErr := reference.ParseNormalizedNamed(clean)
	if parseErr != nil {
		return "", "", "", errors.Wrap(errors.ErrCodeInvalidRequest, "invalid OCI reference", parseErr)
	}
	registry = reference.Domain(named)
	repo = reference.Path(named)
	if digested, ok := named.(reference.Digested); ok {
		target = digested.Digest().String()
	} else if tagged, ok := named.(reference.Tagged); ok {
		target = tagged.Tag()
	}
	if target == "" {
		return "", "", "", errors.New(errors.ErrCodeInvalidRequest,
			"OCI reference "+ref+" must include a tag or digest")
	}
	return registry, repo, target, nil
}

// newAuthClient builds an oras-go auth.Client that honors ambient
// docker credentials and the operator's TLS preferences. Mirrors the
// producer-side pattern in pkg/oci.createAuthClientForHost so both
// sides have consistent registry behavior.
//
// Docker credential store load is best-effort: if a developer has no
// docker config, public-registry pulls still work (the client just
// goes anonymous).
func newAuthClient(plainHTTP, insecureTLS bool) *auth.Client {
	transport := defaults.NewHTTPTransport()
	if !plainHTTP && insecureTLS {
		slog.Warn("TLS verification disabled for OCI registry")
		// Clone any existing TLS config so hardening defaults from
		// defaults.NewHTTPTransport (MinVersion, ciphers) survive.
		var cfg *tls.Config
		if transport.TLSClientConfig != nil {
			cfg = transport.TLSClientConfig.Clone()
		} else {
			// defaults.NewHTTPTransport currently leaves TLSClientConfig
			// nil; set MinVersion explicitly so we don't fall through to
			// Go's historical client default. TLS 1.2 is the project floor.
			cfg = &tls.Config{MinVersion: tls.VersionTLS12} //nolint:gosec // InsecureSkipVerify set on next line
		}
		cfg.InsecureSkipVerify = true //nolint:gosec // explicit operator opt-in via --registry-insecure-tls
		transport.TLSClientConfig = cfg
	}

	client := &auth.Client{
		Client: &http.Client{Timeout: defaults.HTTPClientTimeout, Transport: transport},
		Cache:  auth.NewCache(),
	}

	if credStore, err := credentials.NewStoreFromDocker(credentials.StoreOptions{}); err == nil && credStore != nil {
		client.Credential = credentials.Credential(credStore)
	} else if err != nil {
		slog.Debug("docker credential store unavailable; continuing anonymously",
			"error", err.Error())
	}

	return client
}
