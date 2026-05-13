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
// Flag values + spec.validate.evidence.attestation supply every field;
// recipe + snapshot YAML are marshaled lazily inside emitRecipeEvidence
// so a misconfigured run (e.g., invalid --push reference) fails before
// paying the cost of marshaling a multi-MB snapshot.
type recipeEvidenceConfig struct {
	OutDir      string
	BOMPath     string
	Push        string
	PlainHTTP   bool
	InsecureTLS bool

	// OIDCResolve carries the resolve-time inputs (flag values and
	// ambient env captures) needed to obtain a Sigstore identity token
	// at sign time. The token itself is resolved adjacent to SignBundle
	// — not up front — because Fulcio binds the token to a fresh nonce
	// at issue, and a 3+ minute validation run between resolve and sign
	// invalidates a pre-resolved token. The CI fail-fast story is
	// preserved by validating Push reachability up front in
	// emitRecipeEvidence; only the OIDC handshake is deferred.
	OIDCResolve OIDCResolveInputs
}

// OIDCResolveInputs is the resolve-time slice of recipeEvidenceConfig.
// Captured into cfg by buildRecipeEvidenceConfig from the urfave/cli
// command + the process env so the deferred resolution at sign time
// doesn't need to re-touch the cli object.
type OIDCResolveInputs struct {
	IdentityToken string
	AmbientURL    string
	AmbientToken  string
	DeviceFlow    bool
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
		OIDCResolve: OIDCResolveInputs{
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
// recipe-evidence v1 bundle. The pointer file is always written so the
// contributor can copy it into recipes/evidence/<recipe>.yaml.
//
// Behavior matrix:
//
//	--push absent          → unsigned bundle on disk; pointer carries empty bundle.{oci,digest}.
//	--push set, no OIDC    → error: keyless signing requires SIGSTORE_ID_TOKEN.
//	--push set, OIDC       → sign with cosign keyless, push summary to OCI, populate pointer.
func emitRecipeEvidence(
	ctx context.Context,
	rec *recipe.RecipeResult,
	snap *snapshotter.Snapshot,
	results []*validator.PhaseResult,
	cfg *recipeEvidenceConfig,
) error {

	// Validate the push reference up front so a malformed --push doesn't
	// waste a Fulcio cert + Rekor inclusion proof on a sign that the push
	// will reject seconds later.
	if cfg.Push != "" {
		if _, err := oci.ParseOutputTarget(cfg.Push); err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "invalid --push reference", err)
		}
	}

	// Load the catalog once and feed it to both the BOM (when --bom is
	// absent and the auto-generator runs) and the predicate's
	// ValidatorCatalogVersion + ValidatorImages fields. The catalog is
	// compiled in via go:embed, so Load is a parse, not I/O.
	cat, err := catalog.Load(version, commit)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to load validator catalog for evidence", err)
	}

	bomBody, err := loadOrGenerateBOM(cfg.BOMPath, rec, snap, cat, version)
	if err != nil {
		return err
	}

	// Marshal rec/snap only after the cheap precondition checks pass —
	// the snapshot is typically the largest in-memory object in a
	// validate run and a misconfigured --emit-attestation should fail
	// before we pay that cost.
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

	// attestation.Build creates the output dir tree itself, including
	// any missing parents — no explicit MkdirAll needed here.
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

// signAndPushBundle handles the optional sign+push pipeline. When --push
// is absent, returns a zero-valued outcome so the caller writes a
// pre-publish pointer.
//
// Sequence (--push set):
//  1. Push the bundle directory as an OCI artifact → artifactDigest.
//  2. Build an artifact-subject in-toto Statement
//     (subject.digest = artifactDigest) carrying the same predicate
//     body; recipe identity stays verifiable via predicate.recipe.
//  3. Resolve the OIDC token adjacent to signing — Fulcio binds the
//     token to a fresh nonce at issue and a multi-minute validation
//     run before sign invalidates a pre-resolved token.
//  4. Sign the artifact-subject Statement → Sigstore Bundle JSON.
//  5. Attach the Sigstore Bundle as an OCI Referrer of the main
//     artifact so cosign's /v2/<name>/referrers/<digest> discovery
//     finds the signature without a separate pull.
//
// The signed bytes are also written into the local summary-bundle as
// attestation.intoto.jsonl so the on-disk directory remains
// inspectable; the pushed artifact never contained the file (it was
// signed after push), but locally it's the standard place to find
// the signature.
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
		attestation.CleanOCIRef(summary.Reference),
		artifactDigestHex,
		bundle.Predicate,
	)
	if err != nil {
		return signPushOutcome{}, err
	}

	// Log at Info only when an interactive prompt is about to fire;
	// CI/programmatic paths (identity-token, ambient OIDC) stay quiet
	// at Info so they don't pollute build logs with a misleading
	// "may prompt" line that never actually prompts.
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
	token, tokenErr := bundleattest.ResolveOIDCToken(ctx, bundleattest.ResolveOptions{
		IdentityToken: cfg.OIDCResolve.IdentityToken,
		AmbientURL:    cfg.OIDCResolve.AmbientURL,
		AmbientToken:  cfg.OIDCResolve.AmbientToken,
		DeviceFlow:    cfg.OIDCResolve.DeviceFlow,
		PromptWriter:  os.Stderr,
	})
	if tokenErr != nil {
		return signPushOutcome{}, tokenErr
	}

	signCtx, signCancel := context.WithTimeout(ctx, defaults.EvidenceBundleSignTimeout)
	defer signCancel()
	signRes, err := attestation.NewKeylessSigner(token).Sign(signCtx, artifactStmt)
	if err != nil {
		return signPushOutcome{}, err
	}

	// Write the signed bytes into the local bundle dir for inspection.
	// The pushed OCI artifact does not carry attestation.intoto.jsonl —
	// it was already pushed before signing — so the canonical signature
	// reference is the OCI Referrer attached below, addressable via the
	// pointer file's bundle.{oci,digest}.
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

// loadOrGenerateBOM returns the CycloneDX BOM bytes to embed in the
// evidence bundle. When the operator passes --bom, the path wins (so
// `make bom`-produced exhaustive BOMs continue to be authoritative).
// When --bom is empty, aicr synthesizes a recipe-bound BOM enumerating
// the recipe's component refs, the validator catalog images that ran
// this session, and any container images observed running on the
// snapshot's cluster (pkg/collector/k8s/image.go captures these).
//
// Helm charts are not rendered at validate time — that would require
// the helm binary and a 60s+ rendering budget. Observed snapshot
// images give the same information for the typical post-deployment
// validate flow; when the snapshot is empty or pre-deployment, the
// BOM falls back to chart refs + validator images only.
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

// buildAutoBOM synthesizes a CycloneDX 1.6 BOM from:
//   - The recipe's enabled component refs (chart-level metadata: repo,
//     chart, version, namespace).
//   - The validator catalog images, surfaced as a synthetic "validators"
//     component holding every container the catalog ships for the
//     session's compiled-in version/commit.
//   - Container images observed running on the cluster snapshot, when
//     present, as a synthetic "observed-images" component. These come
//     from the K8s.image.* measurements pkg/collector/k8s populates;
//     refs are registry-stripped (`gpu-operator:v25.10.1` rather than
//     `nvcr.io/nvidia/cloud-native/gpu-operator:v25.10.1`) because the
//     constraint-evaluation collector deliberately strips for
//     registry-mirror stability. A more authoritative full-ref BOM
//     still requires `make bom` via --bom.
//
// Returns the JSON bytes ready to embed in BOMInputs.Body.
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

	recipeName := attestation.RecipeNameFor(rec)
	if recipeName == "" {
		recipeName = "aicr-recipe"
	}
	doc := bom.BuildBOM(bom.Metadata{
		Name:        recipeName,
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

// dedupValidatorImages returns the validator catalog's container image
// refs deduplicated by image string, preserving discovery order. The
// catalog lists one entry per validator-check, and the deployment
// phase's expected-resources + chainsaw checks frequently share an
// image, so this collapse is required to keep both the BOM and the
// predicate's ValidatorImages list from repeating the same `img:` ref.
//
// Returns nil for a catalog with no validators.
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

// catalogVersion returns the catalog's metadata version, or "" when
// the catalog has no metadata block (legacy catalogs predating the
// metadata field). Acts as the predicate.ValidatorCatalogVersion source.
func catalogVersion(cat *catalog.ValidatorCatalog) string {
	if cat == nil || cat.Metadata == nil {
		return ""
	}
	return cat.Metadata.Version
}

// validatorImagesForPredicate adapts the dedup'd image list to the
// attestation.ValidatorImage slice the predicate carries. The Digest
// field stays empty — the catalog records image refs by tag, not by
// digest; resolving image refs to digests would require a registry
// round-trip per image, which validate's hot path deliberately avoids.
// Operators wanting digest pinning can supply an exhaustive BOM via
// --bom; see audit item #10 for the longer-term resolver path.
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

// observedImagesFromSnapshot returns the cluster-observed image refs
// in "<name>:<tag>" form, drawn from the K8s/image measurement that
// pkg/collector/k8s populates. Returns nil when no such measurement is
// present (e.g., --no-cluster runs or pre-deployment snapshots).
//
// The collector registry-strips refs for measurement-key stability
// across registry mirrors (a constraint-evaluation requirement), so
// the output here also lacks registries. An operator wanting fully
// qualified refs ships their own BOM via --bom.
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
			if st.Name != "image" {
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
		in.BundleOCI = attestation.CleanOCIRef(out.Summary.Reference)
		in.BundleHash = out.Summary.Digest
	}
	if out.Sign != nil {
		signer := &attestation.PointerSigner{
			Identity: out.Sign.Identity,
			Issuer:   out.Sign.Issuer,
		}
		// --no-rekor signing returns RekorLogIndex == 0 with no Rekor
		// entry actually created; the SignResult struct has no separate
		// "did we hit Rekor?" boolean. Treat zero as "no Rekor entry"
		// at the pointer-emit boundary. (Rekor index 0 is a legitimate
		// position, but the signer call path that produces it always
		// also sets a non-empty UUID — when we wire UUID through we can
		// distinguish; today, zero from a SignResult means absent.)
		if out.Sign.RekorLogIndex > 0 {
			idx := out.Sign.RekorLogIndex
			signer.RekorLogIndex = &idx
		}
		in.Signer = signer
	}
	return in
}
