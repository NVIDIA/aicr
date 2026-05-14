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
	"log/slog"
	"strings"

	bundleattest "github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/oci"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
)

// EmitOptions controls a single Emit run. The same options struct is used
// by `aicr validate --emit-attestation` and by the (forthcoming) API
// surface so the orchestration stays callable from both pkg/cli and
// pkg/api with no business-logic duplication.
type EmitOptions struct {
	// OutDir receives pointer.yaml and the summary-bundle/ tree.
	OutDir string

	// BOMPath is the path to a pre-built CycloneDX BOM. When empty Emit
	// auto-generates a recipe-bound BOM via BuildAutoBOM.
	BOMPath string

	// Push is the optional OCI reference (oci://… or registry path) the
	// summary bundle is pushed to. When empty Emit produces an unsigned,
	// local-only bundle.
	Push        string
	PlainHTTP   bool
	InsecureTLS bool

	Recipe       *recipe.RecipeResult
	Snapshot     *snapshotter.Snapshot
	PhaseResults []*validator.PhaseResult

	// Catalog is the validator catalog snapshot used for the auto-BOM and
	// the predicate's validatorImages list. Pass nil when no catalog was
	// loaded — the predicate's validatorImages will be omitted.
	Catalog *catalog.ValidatorCatalog

	// AICRVersion is stamped into the predicate and into the auto BOM's
	// metadata.tools entry.
	AICRVersion string

	// OIDCResolve is consulted only when Push is set. Resolution is
	// deferred until adjacent to SignStatement so Fulcio's nonce-binding
	// window is respected — a long Helm-render-and-push phase between
	// resolve and sign otherwise invalidates the token.
	OIDCResolve bundleattest.ResolveOptions
}

// EmitResult describes the artifacts a successful Emit run produced.
// Sign and PushSummary are nil when --push is absent; Bundle and
// PointerPath are always populated on success.
type EmitResult struct {
	Bundle      *Bundle
	PointerPath string
	Sign        *bundleattest.SignedAttestation
	PushSummary *PushResult
}

// Emit builds, optionally signs, and optionally pushes a recipe-evidence
// v1 bundle, then writes the pointer file. The pointer is always
// written.
//
// Behavior matrix:
//
//	Push absent           → unsigned bundle on disk; pointer carries empty bundle.{oci,digest}.
//	Push set, no OIDC     → error: keyless signing requires an OIDC token.
//	Push set, OIDC        → sign with cosign keyless, push summary to OCI, populate pointer.
func Emit(ctx context.Context, opts EmitOptions) (*EmitResult, error) {
	// Validate --push up front so a malformed ref doesn't waste a Fulcio
	// cert + Rekor inclusion proof on a sign the push would reject anyway.
	if opts.Push != "" {
		if _, err := oci.ParseOutputTarget(opts.Push); err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid push reference", err)
		}
	}

	bomBody, err := LoadOrGenerateBOM(opts.BOMPath, opts.Recipe, opts.Snapshot, opts.Catalog, opts.AICRVersion)
	if err != nil {
		return nil, err
	}

	// Use the deterministic marshaller: snapshot.yaml is written into the
	// bundle as-is and its hash feeds the signed manifest digest, so
	// yaml.v3's randomized map iteration would otherwise produce a
	// different manifest digest on every run for the same inputs. The
	// recipe is canonicalized further down by Build, but using the same
	// helper here keeps the contract uniform across both fields.
	recipeYAML, err := serializer.MarshalYAMLDeterministic(opts.Recipe)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to marshal recipe for evidence", err)
	}
	snapshotYAML, err := serializer.MarshalYAMLDeterministic(opts.Snapshot)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to marshal snapshot for evidence", err)
	}

	buildCtx, buildCancel := context.WithTimeout(ctx, defaults.EvidenceBundleBuildTimeout)
	defer buildCancel()

	bundle, err := Build(buildCtx, BuildOptions{
		OutputDir:               opts.OutDir,
		Recipe:                  opts.Recipe,
		RecipeYAML:              recipeYAML,
		Snapshot:                opts.Snapshot,
		SnapshotYAML:            snapshotYAML,
		BOM:                     BOMInputs{Body: bomBody, CycloneDXVersion: DefaultCycloneDXVersion},
		PhaseResults:            opts.PhaseResults,
		AICRVersion:             opts.AICRVersion,
		ValidatorCatalogVersion: CatalogVersion(opts.Catalog),
		ValidatorImages:         ValidatorImagesForPredicate(opts.Catalog),
	})
	if err != nil {
		return nil, err
	}

	slog.Info("evidence bundle built",
		"summaryDir", bundle.SummaryDir,
		"recipe", bundle.RecipeName,
		"subjectDigest", bundle.SubjectDigest)

	out, err := signAndPush(ctx, bundle, opts)
	if err != nil {
		return nil, err
	}

	pointer, err := BuildPointer(buildPointerInputsFromOutcome(bundle, out))
	if err != nil {
		return nil, err
	}
	pointerPath, err := WritePointer(opts.OutDir, pointer)
	if err != nil {
		return nil, err
	}

	slog.Info("evidence pointer written",
		"path", pointerPath,
		"copyTo", "recipes/evidence/"+bundle.RecipeName+".yaml")

	if out.PushSummary != nil {
		slog.Info("evidence bundle pushed",
			"reference", out.PushSummary.Reference,
			"digest", out.PushSummary.Digest)
	}

	return &EmitResult{
		Bundle:      bundle,
		PointerPath: pointerPath,
		Sign:        out.Sign,
		PushSummary: out.PushSummary,
	}, nil
}

// emitOutcome carries the artifacts the pointer file needs from the
// optional sign+push leg. All fields are nil when Push is absent.
type emitOutcome struct {
	Sign        *bundleattest.SignedAttestation
	PushSummary *PushResult
}

// signAndPush handles the optional sign+push pipeline. Returns a
// zero-valued outcome when Push is absent.
//
// Sequence (Push set):
//  1. Push the bundle directory as an OCI artifact → artifactDigest.
//  2. Build an artifact-subject Statement (subject.digest = artifactDigest)
//     carrying the same predicate body.
//  3. Resolve the OIDC token (deferred until here — see EmitOptions.OIDCResolve).
//  4. Sign the Statement → Sigstore Bundle JSON.
//  5. Attach the Sigstore Bundle as an OCI Referrer so cosign's
//     /v2/<name>/referrers/<digest> discovery finds the signature.
func signAndPush(ctx context.Context, bundle *Bundle, opts EmitOptions) (emitOutcome, error) {
	if opts.Push == "" {
		return emitOutcome{}, nil
	}

	pushCtx, pushCancel := context.WithTimeout(ctx, defaults.EvidenceBundlePushTimeout)
	defer pushCancel()
	summary, err := Push(pushCtx, PushOptions{
		SourceDir:   bundle.SummaryDir,
		Reference:   opts.Push,
		AICRVersion: opts.AICRVersion,
		PlainHTTP:   opts.PlainHTTP,
		InsecureTLS: opts.InsecureTLS,
	})
	if err != nil {
		return emitOutcome{}, err
	}

	artifactDigestHex := strings.TrimPrefix(summary.Digest, "sha256:")
	artifactStmt, err := BuildArtifactStatement(
		oci.TrimScheme(summary.Reference),
		artifactDigestHex,
		bundle.Predicate,
	)
	if err != nil {
		return emitOutcome{}, err
	}

	logOIDCResolveMode(opts.OIDCResolve)
	// OIDCAuthTimeout caps interactive browser / device-code flows so a
	// stalled user does not hold the parent (sign) context open
	// indefinitely; pre-fetched and ambient paths complete well below this
	// bound, so the cap only kicks in on the genuinely interactive paths
	// that match defaults.OIDCAuthTimeout's intent.
	resolveCtx, resolveCancel := context.WithTimeout(ctx, defaults.OIDCAuthTimeout)
	token, tokenErr := bundleattest.ResolveOIDCToken(resolveCtx, opts.OIDCResolve)
	resolveCancel()
	if tokenErr != nil {
		return emitOutcome{}, tokenErr
	}

	signCtx, signCancel := context.WithTimeout(ctx, defaults.EvidenceBundleSignTimeout)
	defer signCancel()
	signRes, err := bundleattest.SignStatement(signCtx, artifactStmt, bundleattest.SignOptions{
		OIDCToken: token,
		FulcioURL: bundleattest.DefaultFulcioURL,
		RekorURL:  bundleattest.DefaultRekorURL,
	})
	if err != nil {
		return emitOutcome{}, err
	}

	// Write signed bytes locally for inspection; the pushed artifact
	// itself doesn't carry them — the canonical signature reference is
	// the OCI Referrer attached below.
	if err := WriteSignedAttestation(bundle, signRes.BundleJSON); err != nil {
		return emitOutcome{}, err
	}

	attachCtx, attachCancel := context.WithTimeout(ctx, defaults.EvidenceBundlePushTimeout)
	defer attachCancel()
	referrer, attachErr := AttachSigstoreBundleAsReferrer(attachCtx, AttachReferrerOptions{
		Reference:  opts.Push,
		BundleJSON: signRes.BundleJSON,
		MainArtifact: MainArtifactDescriptor{
			Digest:    summary.Digest,
			MediaType: summary.MediaType,
			Size:      summary.Size,
		},
		PlainHTTP:   opts.PlainHTTP,
		InsecureTLS: opts.InsecureTLS,
	})
	if attachErr != nil {
		return emitOutcome{}, attachErr
	}
	slog.Info("Sigstore Bundle attached as OCI Referrer",
		"referrerDigest", referrer.Digest,
		"mainArtifactDigest", summary.Digest)

	return emitOutcome{Sign: signRes, PushSummary: summary}, nil
}

// logOIDCResolveMode emits a single info-or-debug line describing which
// OIDC source the resolver is about to consult. Interactive flows log at
// Info (the user may be about to be prompted); non-interactive sources
// log at Debug to keep build logs quiet.
func logOIDCResolveMode(opts bundleattest.ResolveOptions) {
	switch {
	case opts.IdentityToken != "":
		slog.Debug("resolving OIDC token", "mode", "identity-token")
	case opts.AmbientURL != "" && opts.AmbientToken != "":
		slog.Debug("resolving OIDC token", "mode", "ambient-github-actions")
	case opts.DeviceFlow:
		slog.Info("resolving OIDC token via device-code flow (will print a code to enter at the URL shown)")
	default:
		slog.Info("resolving OIDC token via browser flow (will open a local browser)")
	}
}

func buildPointerInputsFromOutcome(bundle *Bundle, out emitOutcome) PointerInputs {
	in := PointerInputs{Bundle: bundle}
	if out.PushSummary != nil {
		in.BundleOCI = oci.TrimScheme(out.PushSummary.Reference)
		in.BundleHash = out.PushSummary.Digest
	}
	if out.Sign != nil {
		signer := &PointerSigner{
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
