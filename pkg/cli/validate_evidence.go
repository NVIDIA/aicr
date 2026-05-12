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

	cdx "github.com/CycloneDX/cyclonedx-go"
	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bom"
	"github.com/NVIDIA/aicr/pkg/config"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
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

	// OIDCToken is the resolved Sigstore identity token for keyless
	// signing. Populated by the Action body via
	// bundleattest.ResolveOIDCToken before runValidation starts; empty
	// when Push is unset (no signing needed). Carried on the config
	// struct rather than re-resolved at sign time so an interactive or
	// device-code flow prompts the operator up front, before validation
	// work begins.
	OIDCToken string
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

	bomBody, err := loadOrGenerateBOM(cfg.BOMPath, rec, version, commit)
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
		OutputDir:    cfg.OutDir,
		Recipe:       rec,
		RecipeYAML:   recipeYAML,
		Snapshot:     snap,
		SnapshotYAML: snapshotYAML,
		BOM:          attestation.BOMInputs{Body: bomBody, CycloneDXVersion: attestation.DefaultCycloneDXVersion},
		PhaseResults: results,
		AICRVersion:  version,
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
// cfg.OIDCToken is populated by the Action body via
// bundleattest.ResolveOIDCToken so token acquisition (which may prompt
// a browser or device-code flow) happens up front, not after validation
// already ran.
func signAndPushBundle(
	ctx context.Context,
	bundle *attestation.Bundle,
	cfg *recipeEvidenceConfig,
) (signPushOutcome, error) {

	if cfg.Push == "" {
		return signPushOutcome{Sign: &attestation.SignResult{}}, nil
	}

	signCtx, signCancel := context.WithTimeout(ctx, defaults.EvidenceBundleSignTimeout)
	defer signCancel()
	signRes, err := attestation.SignBundle(signCtx, bundle, attestation.NewKeylessSigner(cfg.OIDCToken))
	if err != nil {
		return signPushOutcome{}, err
	}

	pushCtx, pushCancel := context.WithTimeout(ctx, defaults.EvidenceBundlePushTimeout)
	defer pushCancel()
	summary, err := pushArtifact(pushCtx, bundle.SummaryDir, cfg.Push, cfg)
	if err != nil {
		return signPushOutcome{}, err
	}
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
// the recipe's component refs and the validator catalog images that
// ran during this session. The auto-generated BOM does not render
// individual container images inside helm charts — that requires the
// helm binary which the validate hot path deliberately avoids
// depending on; auditors who need that detail can resolve each chart
// ref via standard tooling or supply `make bom` via --bom.
func loadOrGenerateBOM(bomPath string, rec *recipe.RecipeResult, version, commit string) ([]byte, error) {
	if bomPath != "" {
		body, err := os.ReadFile(bomPath)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to read BOM", err)
		}
		return body, nil
	}
	return buildAutoBOM(rec, version, commit)
}

// buildAutoBOM synthesizes a CycloneDX 1.6 BOM from:
//   - The recipe's enabled component refs (chart-level metadata: repo,
//     chart, version, namespace; images intentionally not enumerated —
//     see loadOrGenerateBOM rationale).
//   - The validator catalog images, surfaced as a single synthetic
//     "validators" component holding every container the catalog ships
//     for the session's compiled-in version/commit.
//
// Returns the JSON bytes ready to embed in BOMInputs.Body.
func buildAutoBOM(rec *recipe.RecipeResult, version, commit string) ([]byte, error) {
	cat, err := catalog.Load(version, commit)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to load validator catalog for auto BOM", err)
	}

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

	if len(cat.Validators) > 0 {
		// Catalog lists one entry per validator-check, which often share
		// container images (the deployment phase's expected-resources
		// and chainsaw checks ship from the same image). Dedupe so the
		// BOM doesn't list the same `img:` ref dozens of times under
		// the validators dependency.
		seen := make(map[string]struct{}, len(cat.Validators))
		images := make([]string, 0, len(cat.Validators))
		for _, v := range cat.Validators {
			if v.Image == "" {
				continue
			}
			if _, dup := seen[v.Image]; dup {
				continue
			}
			seen[v.Image] = struct{}{}
			images = append(images, v.Image)
		}
		results = append(results, bom.ComponentResult{
			Name:        "validators",
			DisplayName: "AICR validators",
			Type:        "validators",
			Images:      images,
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

func buildPointerInputs(bundle *attestation.Bundle, out signPushOutcome) attestation.PointerInputs {
	in := attestation.PointerInputs{Bundle: bundle}
	if out.Summary != nil {
		in.BundleOCI = attestation.CleanOCIRef(out.Summary.Reference)
		in.BundleHash = out.Summary.Digest
	}
	if out.Sign != nil {
		in.Signer = attestation.PointerSigner{
			Identity:      out.Sign.Identity,
			Issuer:        out.Sign.Issuer,
			RekorLogIndex: out.Sign.RekorLogIndex,
		}
	}
	return in
}
