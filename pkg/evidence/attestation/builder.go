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
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/fingerprint"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator"
)

// BuildOptions controls bundle construction. The zero value is not usable.
type BuildOptions struct {
	OutputDir string

	Recipe *recipe.RecipeResult

	// RecipeYAML must be the canonical post-resolution bytes; the builder
	// canonicalizes once and reuses the result for both the bundle's
	// recipe.yaml and the in-toto subject digest.
	RecipeYAML []byte

	Snapshot     *snapshotter.Snapshot
	SnapshotYAML []byte

	BOM BOMInputs

	PhaseResults []*validator.PhaseResult

	AICRVersion             string
	ValidatorCatalogVersion string

	// Digest fields stay blank: the catalog tracks refs by tag and
	// resolving to digest would require a registry round-trip per image.
	ValidatorImages []ValidatorImage

	// AttestedAt overrides the wall-clock for tests.
	AttestedAt time.Time
}

// BOMInputs carries the CycloneDX BOM the validate run produced.
type BOMInputs struct {
	Body []byte

	// CycloneDXVersion is the spec version (e.g., "1.5").
	CycloneDXVersion string
}

// Bundle is what the builder returns: a description of the on-disk
// artifacts and the in-memory predicate ready to be signed.
type Bundle struct {
	SummaryDir string

	RecipeName string

	// SubjectDigest is sha256(canonicalize(recipe.yaml)) as hex.
	SubjectDigest string

	Predicate *Predicate

	// StatementJSON is the protobuf-canonical JSON of the unsigned
	// in-toto Statement. The signer wraps it in DSSE.
	StatementJSON []byte
}

// Build produces an unsigned bundle on disk and an in-memory predicate.
// Signing is a separate step (see signer.go) so test code can exercise
// builder behavior without sigstore credentials.
//
// The summary-bundle directory contains every file referenced by the
// manifest *except* attestation.intoto.jsonl, which is the signature
// itself. The manifest digest recorded in the predicate is therefore
// stable: signing does not alter the predicate.
func Build(ctx context.Context, opts BuildOptions) (*Bundle, error) {
	if err := validateOpts(opts); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable, "build canceled", err)
	}

	recipeName := RecipeNameFor(opts.Recipe)
	if recipeName == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "recipe has no resolvable name")
	}

	summaryDir := filepath.Join(opts.OutputDir, SummaryBundleDirName)
	if err := os.MkdirAll(summaryDir, 0o755); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create summary bundle dir", err)
	}

	canon, err := CanonicalizeRecipeYAML(opts.RecipeYAML)
	if err != nil {
		return nil, err
	}
	if writeErr := os.WriteFile(filepath.Join(summaryDir, RecipeFilename), canon, 0o600); writeErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to write recipe.yaml", writeErr)
	}
	subjectDigest := DigestOfCanonical(canon)

	if writeErr := os.WriteFile(filepath.Join(summaryDir, SnapshotFilename), opts.SnapshotYAML, 0o600); writeErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to write snapshot.yaml", writeErr)
	}

	if writeErr := os.WriteFile(filepath.Join(summaryDir, BOMFilename), opts.BOM.Body, 0o600); writeErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to write bom.cdx.json", writeErr)
	}
	bomDigest := HashBytesSHA256(opts.BOM.Body)
	bomImageCount, err := countBOMComponents(opts.BOM.Body)
	if err != nil {
		return nil, err
	}

	phasesDir := filepath.Join(summaryDir, ctrfDirName)
	if mkErr := os.MkdirAll(phasesDir, 0o755); mkErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create ctrf dir", mkErr)
	}
	phaseSummaries := map[Phase]PhaseSummary{}
	for _, pr := range opts.PhaseResults {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, errors.Wrap(errors.ErrCodeUnavailable, "build canceled", ctxErr)
		}
		if pr == nil || pr.Report == nil {
			continue
		}
		phaseKey := Phase(pr.Phase)
		body, mErr := json.MarshalIndent(pr.Report, "", "  ")
		if mErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "failed to marshal CTRF report", mErr)
		}
		body = append(body, '\n')
		ctrfPath := filepath.Join(phasesDir, string(phaseKey)+".json")
		if writeErr := os.WriteFile(ctrfPath, body, 0o600); writeErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "failed to write CTRF report", writeErr)
		}
		phaseSummaries[phaseKey] = PhaseSummary{
			Passed:     pr.Report.Results.Summary.Passed,
			Failed:     pr.Report.Results.Summary.Failed,
			Skipped:    pr.Report.Results.Summary.Skipped,
			CTRFDigest: HashBytesSHA256(body),
		}
	}

	manifest, err := BuildManifest(summaryDir, ManifestFilename, StatementFilename, AttestationFilename)
	if err != nil {
		return nil, err
	}
	manifestDigest, err := WriteManifest(summaryDir, manifest)
	if err != nil {
		return nil, err
	}

	attestedAt := opts.AttestedAt
	if attestedAt.IsZero() {
		attestedAt = time.Now().UTC()
	}
	var snapMeasurements []*measurement.Measurement
	if opts.Snapshot != nil {
		snapMeasurements = opts.Snapshot.Measurements
	}
	fp := fingerprint.FromMeasurements(snapMeasurements)
	cm := fp.Match(criteriaOf(opts.Recipe))
	pred := BuildPredicate(PredicateInputs{
		AttestedAt:              attestedAt,
		AICRVersion:             opts.AICRVersion,
		ValidatorCatalogVersion: opts.ValidatorCatalogVersion,
		ValidatorImages:         opts.ValidatorImages,
		Recipe:                  RecipeRef{Name: recipeName, Digest: subjectDigest},
		Fingerprint:             *fp,
		CriteriaMatch:           cm,
		Phases:                  phaseSummaries,
		BOM: BOMRef{
			Format:     BOMFormat,
			Version:    opts.BOM.CycloneDXVersion,
			Digest:     bomDigest,
			ImageCount: bomImageCount,
		},
		Manifest: ManifestRef{
			Digest:    manifestDigest,
			FileCount: len(manifest.Files),
		},
	})

	stmt, err := BuildStatement(recipeName, subjectDigest, pred)
	if err != nil {
		return nil, err
	}

	// Persist the unsigned Statement so the bundle is self-contained: a
	// caller can sign it later with cosign or any DSSE signer.
	if writeErr := os.WriteFile(filepath.Join(summaryDir, StatementFilename), stmt, 0o600); writeErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to write unsigned statement", writeErr)
	}

	slog.Debug("built recipe evidence bundle",
		"recipe", recipeName,
		"summaryDir", summaryDir,
		"subjectDigest", subjectDigest,
		"manifestDigest", manifestDigest,
		"fileCount", len(manifest.Files))

	return &Bundle{
		SummaryDir:    summaryDir,
		RecipeName:    recipeName,
		SubjectDigest: subjectDigest,
		Predicate:     pred,
		StatementJSON: stmt,
	}, nil
}

func validateOpts(opts BuildOptions) error {
	if opts.OutputDir == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "OutputDir is required")
	}
	if opts.Recipe == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "Recipe is required")
	}
	if len(opts.RecipeYAML) == 0 {
		return errors.New(errors.ErrCodeInvalidRequest, "RecipeYAML is required")
	}
	if len(opts.SnapshotYAML) == 0 {
		return errors.New(errors.ErrCodeInvalidRequest, "SnapshotYAML is required")
	}
	if len(opts.BOM.Body) == 0 {
		return errors.New(errors.ErrCodeInvalidRequest, "BOM.Body is required")
	}
	return nil
}

// defaultRecipeName is the fallback name used when a RecipeResult has
// no concrete (non-wildcard) criteria values to derive a name from.
const defaultRecipeName = "recipe"

// criteriaWildcard mirrors the wildcard literal pkg/recipe uses for
// criteria fields.
const criteriaWildcard = "any"

// RecipeNameFor derives the bundle's recipe identifier from the resolved
// criteria: hyphen-joined non-wildcard accelerator/service/os/intent/
// platform values, or "recipe" when every slot is empty or wildcard.
func RecipeNameFor(r *recipe.RecipeResult) string {
	if r == nil || r.Criteria == nil {
		return ""
	}
	c := r.Criteria
	parts := make([]string, 0, 5)
	for _, v := range []string{
		string(c.Accelerator),
		string(c.Service),
		string(c.OS),
		string(c.Intent),
		string(c.Platform),
	} {
		if v != "" && v != criteriaWildcard {
			parts = append(parts, v)
		}
	}
	if len(parts) == 0 {
		return defaultRecipeName
	}
	return strings.Join(parts, "-")
}

func criteriaOf(r *recipe.RecipeResult) *recipe.Criteria {
	if r == nil {
		return nil
	}
	return r.Criteria
}

// countBOMComponents reports the number of components[] entries in a
// CycloneDX BOM body. Errors are surfaced rather than silently zeroed
// because a malformed BOM is a build-time problem, not a runtime one.
func countBOMComponents(body []byte) (int, error) {
	var doc struct {
		Components []json.RawMessage `json:"components"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return 0, errors.Wrap(errors.ErrCodeInvalidRequest, "BOM is not valid JSON", err)
	}
	return len(doc.Components), nil
}
