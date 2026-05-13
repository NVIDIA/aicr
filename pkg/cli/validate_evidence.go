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

package cli

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"

	cdx "github.com/CycloneDX/cyclonedx-go"
	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bom"
	bundleattest "github.com/NVIDIA/aicr/pkg/bundler/attestation"
	k8scollector "github.com/NVIDIA/aicr/pkg/collector/k8s"
	"github.com/NVIDIA/aicr/pkg/config"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/oci"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
)

// recipeEvidenceConfig groups the inputs to `aicr validate --emit-attestation`.
type recipeEvidenceConfig struct {
	OutDir      string
	BOMPath     string
	Push        string
	PlainHTTP   bool
	InsecureTLS bool

	// OIDC token resolution is deferred until adjacent to SignBundle:
	// Fulcio binds the token to a fresh nonce at issue, and a multi-minute
	// validation run between resolve and sign invalidates it.
	OIDCResolve bundleattest.ResolveOptions
}

// buildRecipeEvidenceConfig parses the --emit-attestation flag family with
// CLI > config precedence. Returns nil when neither the flag nor
// spec.validate.evidence.attestation.out is set, signaling the validate
// run should not produce a recipe-evidence bundle.
func buildRecipeEvidenceConfig(cmd *cli.Command, resolved *config.ValidateResolved) *recipeEvidenceConfig {
	att := resolved.EvidenceAttestation
	if att == nil {
		att = &config.EvidenceAttestationResolved{}
	}
	out := stringFlagOrConfig(cmd, "emit-attestation", att.Out)
	if out == "" {
		return nil
	}
	return &recipeEvidenceConfig{
		OutDir:      out,
		BOMPath:     stringFlagOrConfig(cmd, "bom", att.BOM),
		Push:        stringFlagOrConfig(cmd, "push", att.Push),
		PlainHTTP:   boolFlagOrConfig(cmd, "plain-http", att.PlainHTTP),
		InsecureTLS: boolFlagOrConfig(cmd, "insecure-tls", att.InsecureTLS),
		OIDCResolve: bundleattest.ResolveOptions{
			IdentityToken: cmd.String("identity-token"),
			AmbientURL:    os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL"),
			AmbientToken:  os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN"),
			DeviceFlow:    cmd.Bool("oidc-device-flow"),
		},
	}
}

// signPushOutcome carries the artifacts the pointer file needs from the
// optional sign+push leg. All fields are nil when --push is absent.
type signPushOutcome struct {
	Sign    *attestation.SignResult
	Summary *attestation.PushResult
}

// emitRecipeEvidence builds, optionally signs, and optionally pushes a
// recipe-evidence v1 bundle. The pointer file is always written.
//
// Behavior matrix:
//
//	--push absent          → unsigned bundle on disk; pointer carries empty bundle.{oci,digest}.
//	--push set, no OIDC    → error: keyless signing requires an OIDC token.
//	--push set, OIDC       → sign with cosign keyless, push summary to OCI, populate pointer.
func emitRecipeEvidence(
	ctx context.Context,
	rec *recipe.RecipeResult,
	snap *snapshotter.Snapshot,
	results []*validator.PhaseResult,
	cfg *recipeEvidenceConfig,
) error {

	// Validate --push up front so a malformed ref doesn't waste a Fulcio
	// cert + Rekor inclusion proof on a sign the push would reject anyway.
	if cfg.Push != "" {
		if _, err := oci.ParseOutputTarget(cfg.Push); err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "invalid --push reference", err)
		}
	}

	cat, err := catalog.Load(version, commit)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to load validator catalog for evidence", err)
	}

	bomBody, err := loadOrGenerateBOM(cfg.BOMPath, rec, snap, cat, version)
	if err != nil {
		return err
	}

	recipeYAML, err := yaml.Marshal(rec)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to marshal recipe for evidence", err)
	}
	snapshotYAML, err := yaml.Marshal(snap)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to marshal snapshot for evidence", err)
	}

	buildCtx, buildCancel := context.WithTimeout(ctx, defaults.EvidenceBundleBuildTimeout)
	defer buildCancel()

	bundle, err := attestation.Build(buildCtx, attestation.BuildOptions{
		OutputDir:               cfg.OutDir,
		Recipe:                  rec,
		RecipeYAML:              recipeYAML,
		Snapshot:                snap,
		SnapshotYAML:            snapshotYAML,
		BOM:                     attestation.BOMInputs{Body: bomBody, CycloneDXVersion: attestation.DefaultCycloneDXVersion},
		PhaseResults:            results,
		AICRVersion:             version,
		ValidatorCatalogVersion: catalogVersion(cat),
		ValidatorImages:         validatorImagesForPredicate(cat),
	})
	if err != nil {
		return err
	}

	slog.Info("evidence bundle built",
		"summaryDir", bundle.SummaryDir,
		"recipe", bundle.RecipeName,
		"subjectDigest", bundle.SubjectDigest)

	out, err := signAndPushBundle(ctx, bundle, cfg)
	if err != nil {
		return err
	}

	pointer, err := attestation.BuildPointer(buildPointerInputs(bundle, out))
	if err != nil {
		return err
	}
	pointerPath, err := attestation.WritePointer(cfg.OutDir, pointer)
	if err != nil {
		return err
	}

	slog.Info("evidence pointer written",
		"path", pointerPath,
		"copyTo", "recipes/evidence/"+bundle.RecipeName+".yaml")

	if out.Summary != nil {
		slog.Info("evidence bundle pushed",
			"reference", out.Summary.Reference,
			"digest", out.Summary.Digest)
	}

	return nil
}

// signAndPushBundle handles the optional sign+push pipeline. Returns a
// zero-valued outcome when --push is absent.
//
// Sequence (--push set):
//  1. Push the bundle directory as an OCI artifact → artifactDigest.
//  2. Build an artifact-subject Statement (subject.digest = artifactDigest)
//     carrying the same predicate body.
//  3. Resolve the OIDC token (deferred until here — see recipeEvidenceConfig).
//  4. Sign the Statement → Sigstore Bundle JSON.
//  5. Attach the Sigstore Bundle as an OCI Referrer so cosign's
//     /v2/<name>/referrers/<digest> discovery finds the signature.
func signAndPushBundle(
	ctx context.Context,
	bundle *attestation.Bundle,
	cfg *recipeEvidenceConfig,
) (signPushOutcome, error) {

	if cfg.Push == "" {
		return signPushOutcome{}, nil
	}

	pushCtx, pushCancel := context.WithTimeout(ctx, defaults.EvidenceBundlePushTimeout)
	defer pushCancel()
	summary, err := pushArtifact(pushCtx, bundle.SummaryDir, cfg.Push, cfg)
	if err != nil {
		return signPushOutcome{}, err
	}

	artifactDigestHex := strings.TrimPrefix(summary.Digest, "sha256:")
	artifactStmt, err := attestation.BuildArtifactStatement(
		oci.TrimScheme(summary.Reference),
		artifactDigestHex,
		bundle.Predicate,
	)
	if err != nil {
		return signPushOutcome{}, err
	}

	// Info only when an interactive prompt is about to fire; non-interactive
	// paths log at Debug so build logs don't carry a misleading "may prompt" line.
	switch {
	case cfg.OIDCResolve.IdentityToken != "":
		slog.Debug("resolving OIDC token", "mode", "identity-token")
	case cfg.OIDCResolve.AmbientURL != "" && cfg.OIDCResolve.AmbientToken != "":
		slog.Debug("resolving OIDC token", "mode", "ambient-github-actions")
	case cfg.OIDCResolve.DeviceFlow:
		slog.Info("resolving OIDC token via device-code flow (will print a code to enter at the URL shown)")
	default:
		slog.Info("resolving OIDC token via browser flow (will open a local browser)")
	}
	resolveOpts := cfg.OIDCResolve
	resolveOpts.PromptWriter = os.Stderr
	token, tokenErr := bundleattest.ResolveOIDCToken(ctx, resolveOpts)
	if tokenErr != nil {
		return signPushOutcome{}, tokenErr
	}

	signCtx, signCancel := context.WithTimeout(ctx, defaults.EvidenceBundleSignTimeout)
	defer signCancel()
	signRes, err := attestation.NewKeylessSigner(token).Sign(signCtx, artifactStmt)
	if err != nil {
		return signPushOutcome{}, err
	}

	// Write signed bytes locally for inspection; the pushed artifact
	// itself doesn't carry them — the canonical signature reference is
	// the OCI Referrer attached below.
	if err := attestation.WriteSignedAttestation(bundle, signRes.BundleJSON); err != nil {
		return signPushOutcome{}, err
	}

	attachCtx, attachCancel := context.WithTimeout(ctx, defaults.EvidenceBundlePushTimeout)
	defer attachCancel()
	referrer, attachErr := attestation.AttachSigstoreBundleAsReferrer(attachCtx, attestation.AttachReferrerOptions{
		Reference:  cfg.Push,
		BundleJSON: signRes.BundleJSON,
		MainArtifact: attestation.MainArtifactDescriptor{
			Digest:    summary.Digest,
			MediaType: summary.MediaType,
			Size:      summary.Size,
		},
		PlainHTTP:   cfg.PlainHTTP,
		InsecureTLS: cfg.InsecureTLS,
	})
	if attachErr != nil {
		return signPushOutcome{}, attachErr
	}
	slog.Info("Sigstore Bundle attached as OCI Referrer",
		"referrerDigest", referrer.Digest,
		"mainArtifactDigest", summary.Digest)

	return signPushOutcome{Sign: signRes, Summary: summary}, nil
}

func pushArtifact(ctx context.Context, sourceDir, ref string, cfg *recipeEvidenceConfig) (*attestation.PushResult, error) {
	return attestation.Push(ctx, attestation.PushOptions{
		SourceDir:   sourceDir,
		Reference:   ref,
		AICRVersion: version,
		PlainHTTP:   cfg.PlainHTTP,
		InsecureTLS: cfg.InsecureTLS,
	})
}

// loadOrGenerateBOM returns the CycloneDX BOM bytes to embed. When --bom
// is set the path wins; otherwise aicr synthesizes a recipe-bound BOM.
// Helm charts are not rendered at validate time (would require the helm
// binary and a 60s+ budget); observed snapshot images cover the same
// information for the typical post-deployment flow.
func loadOrGenerateBOM(bomPath string, rec *recipe.RecipeResult, snap *snapshotter.Snapshot, cat *catalog.ValidatorCatalog, version string) ([]byte, error) {
	if bomPath != "" {
		body, err := os.ReadFile(bomPath)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to read BOM", err)
		}
		return body, nil
	}
	return buildAutoBOM(rec, snap, cat, version)
}

// buildAutoBOM synthesizes a CycloneDX 1.6 BOM from the recipe's enabled
// component refs, validator catalog images, and cluster-observed images.
// Observed images are registry-stripped because the constraint-evaluation
// collector strips them for mirror stability; a full-ref BOM still requires
// --bom.
func buildAutoBOM(rec *recipe.RecipeResult, snap *snapshotter.Snapshot, cat *catalog.ValidatorCatalog, version string) ([]byte, error) {
	results := make([]bom.ComponentResult, 0, len(rec.ComponentRefs)+1)
	for _, c := range rec.ComponentRefs {
		if !c.IsEnabled() {
			continue
		}
		results = append(results, bom.ComponentResult{
			Name:        c.Name,
			DisplayName: c.Name,
			Type:        string(c.Type),
			Repository:  c.Source,
			Chart:       c.Chart,
			Version:     c.Version,
			Namespace:   c.Namespace,
			Pinned:      c.Version != "",
		})
	}

	if images := dedupValidatorImages(cat); len(images) > 0 {
		results = append(results, bom.ComponentResult{
			Name:        "validators",
			DisplayName: "AICR validators",
			Type:        "validators",
			Images:      images,
		})
	}

	if observed := observedImagesFromSnapshot(snap); len(observed) > 0 {
		results = append(results, bom.ComponentResult{
			Name:        "observed-images",
			DisplayName: "Cluster-observed container images",
			Type:        "snapshot",
			Images:      observed,
		})
	}

	doc := bom.BuildBOM(bom.Metadata{
		Name:        attestation.RecipeNameFor(rec),
		Version:     version,
		Description: "Recipe-bound CycloneDX BOM auto-generated by aicr validate",
		ToolName:    "aicr",
		ToolVersion: version,
	}, results)

	var buf bytes.Buffer
	enc := cdx.NewBOMEncoder(&buf, cdx.BOMFileFormatJSON)
	enc.SetPretty(true)
	if encErr := enc.EncodeVersion(doc, cdx.SpecVersion1_6); encErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to encode auto-generated BOM", encErr)
	}
	return buf.Bytes(), nil
}

// dedupValidatorImages returns validator catalog image refs deduplicated
// by image string, preserving discovery order. Multiple checks share an
// image; collapsing keeps the BOM and predicate from duplicating refs.
func dedupValidatorImages(cat *catalog.ValidatorCatalog) []string {
	if cat == nil || len(cat.Validators) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(cat.Validators))
	out := make([]string, 0, len(cat.Validators))
	for _, v := range cat.Validators {
		if v.Image == "" {
			continue
		}
		if _, dup := seen[v.Image]; dup {
			continue
		}
		seen[v.Image] = struct{}{}
		out = append(out, v.Image)
	}
	return out
}

// catalogVersion returns the catalog metadata version, or "" when the
// catalog has no metadata block (legacy catalogs predate the field).
func catalogVersion(cat *catalog.ValidatorCatalog) string {
	if cat == nil || cat.Metadata == nil {
		return ""
	}
	return cat.Metadata.Version
}

// validatorImagesForPredicate adapts the dedup'd list to predicate form.
// Digest stays empty: the catalog records refs by tag, and resolving to
// digest would require a registry round-trip per image. Operators wanting
// digest pinning ship an exhaustive BOM via --bom.
func validatorImagesForPredicate(cat *catalog.ValidatorCatalog) []attestation.ValidatorImage {
	images := dedupValidatorImages(cat)
	if len(images) == 0 {
		return nil
	}
	out := make([]attestation.ValidatorImage, 0, len(images))
	for _, img := range images {
		out = append(out, attestation.ValidatorImage{Image: img})
	}
	return out
}

// observedImagesFromSnapshot returns cluster-observed image refs in
// "<name>:<tag>" form. Refs lack a registry because the collector
// strips registries for measurement-key stability across mirrors.
func observedImagesFromSnapshot(snap *snapshotter.Snapshot) []string {
	if snap == nil {
		return nil
	}
	var (
		seen   = map[string]struct{}{}
		images []string
	)
	for _, m := range snap.Measurements {
		if m == nil || m.Type != measurement.TypeK8s {
			continue
		}
		for _, st := range m.Subtypes {
			if st.Name != k8scollector.SubtypeImage {
				continue
			}
			for name, reading := range st.Data {
				if name == "" || reading == nil {
					continue
				}
				ref := name + ":" + reading.String()
				if _, dup := seen[ref]; dup {
					continue
				}
				seen[ref] = struct{}{}
				images = append(images, ref)
			}
		}
	}
	return images
}

func buildPointerInputs(bundle *attestation.Bundle, out signPushOutcome) attestation.PointerInputs {
	in := attestation.PointerInputs{Bundle: bundle}
	if out.Summary != nil {
		in.BundleOCI = oci.TrimScheme(out.Summary.Reference)
		in.BundleHash = out.Summary.Digest
	}
	if out.Sign != nil {
		signer := &attestation.PointerSigner{
			Identity: out.Sign.Identity,
			Issuer:   out.Sign.Issuer,
		}
		// --no-rekor signing returns RekorLogIndex == 0 with no entry
		// created; treat zero as "no Rekor entry" at this boundary.
		if out.Sign.RekorLogIndex > 0 {
			idx := out.Sign.RekorLogIndex
			signer.RekorLogIndex = &idx
		}
		in.Signer = signer
	}
	return in
}
