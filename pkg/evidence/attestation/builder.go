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
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
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

	// Per-file sha256s are always pre-committed in the manifest;
	// IncludeLogs only controls whether the logs bundle directory is
	// emitted alongside the summary bundle.
	PhaseLogs   map[Phase][]LogFile
	IncludeLogs bool

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

// LogFile describes one log file under a phase. The builder copies it
// into logs-bundle/phases/<phase>/logs/<basename>.
type LogFile struct {
	SourcePath string

	// Basename defaults to filepath.Base(SourcePath) when empty.
	Basename string
}

// Bundle is what the builder returns: a description of the on-disk
// artifacts and the in-memory predicate ready to be signed.
type Bundle struct {
	SummaryDir string

	// LogsDir is "" when IncludeLogs is false or no logs were supplied.
	LogsDir string

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
		return nil, errors.Wrap(errors.ErrCodeTimeout, "build canceled", err)
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

	logsDir := ""
	if opts.IncludeLogs {
		logsDir = filepath.Join(opts.OutputDir, LogsBundleDirName)
		if logErr := writeLogsBundle(logsDir, opts.PhaseLogs); logErr != nil {
			return nil, logErr
		}
	}

	// Manifest pre-commits per-file hashes for any logs the contributor
	// may publish later, so the integrity chain extends to log content
	// the bundle doesn't physically carry.
	manifest, err := buildManifestWithLogPreCommits(summaryDir, opts.PhaseLogs)
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
		LogsDir:       logsDir,
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

// logBasename resolves the in-bundle filename for a log entry and
// rejects any value that would escape the per-phase logs directory.
// f.Basename is operator-controlled, so we treat it as untrusted.
func logBasename(f LogFile) (string, error) {
	name := f.Basename
	if name == "" {
		name = filepath.Base(f.SourcePath)
	}
	cleaned := filepath.Base(filepath.Clean(name))
	if cleaned != name || cleaned == "." || cleaned == ".." {
		return "", errors.New(errors.ErrCodeInvalidRequest, "log basename must not contain path separators")
	}
	return cleaned, nil
}

// logRelPath returns the manifest-relative path used to address a log
// inside (or alongside) the summary bundle. The same path is used for
// the on-disk logs bundle and for the manifest pre-commit, so a
// contributor who publishes logs later still hashes against the same
// entry the signer pre-committed.
func logRelPath(p Phase, basename string) string {
	return phasesDirName + "/" + string(p) + "/" + logsDirName + "/" + basename
}

// writeLogsBundle copies log files into logsDir/phases/<phase>/logs/.
// Streams bytes so multi-GB logs don't materialize in memory.
func writeLogsBundle(logsDir string, phaseLogs map[Phase][]LogFile) error {
	if len(phaseLogs) == 0 {
		return nil
	}
	for _, p := range AllPhases {
		files, ok := phaseLogs[p]
		if !ok || len(files) == 0 {
			continue
		}
		dest := filepath.Join(logsDir, phasesDirName, string(p), logsDirName)
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to create logs phase dir", err)
		}
		for _, f := range files {
			name, nameErr := logBasename(f)
			if nameErr != nil {
				return nameErr
			}
			if err := streamCopy(f.SourcePath, filepath.Join(dest, name)); err != nil {
				return err
			}
		}
	}
	return nil
}

// streamCopy copies src→dst via io.Copy so neither body is held in RAM.
func streamCopy(src, dst string) (retErr error) {
	in, err := os.Open(filepath.Clean(src)) //nolint:gosec // src is operator-supplied validator output
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to open source log "+src, err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(filepath.Clean(dst), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // dst is package-controlled
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create log in bundle", err)
	}
	defer func() {
		if closeErr := out.Close(); closeErr != nil && retErr == nil {
			retErr = errors.Wrap(errors.ErrCodeInternal, "failed to close log in bundle", closeErr)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to copy log into bundle", err)
	}
	return nil
}

// buildManifestWithLogPreCommits walks the summary bundle and appends
// stream-hashed pre-commit entries for every log file in phaseLogs.
// Pre-committing means the manifest binds the log content even when the
// contributor publishes it later as a separate logs bundle.
func buildManifestWithLogPreCommits(summaryDir string, phaseLogs map[Phase][]LogFile) (*Manifest, error) {
	// Exclude self-referential files: the manifest can't enumerate
	// itself, and the statement / signed attestation derive from the
	// manifest digest so they're bound to the signature, not the
	// manifest.
	m, err := BuildManifest(summaryDir, ManifestFilename, StatementFilename, AttestationFilename)
	if err != nil {
		return nil, err
	}
	if len(phaseLogs) == 0 {
		return m, nil
	}

	for _, p := range AllPhases {
		for _, f := range phaseLogs[p] {
			info, statErr := os.Stat(f.SourcePath)
			if statErr != nil {
				return nil, errors.Wrap(errors.ErrCodeInternal, "failed to stat log for pre-commit", statErr)
			}
			digest, hashErr := HashFileSHA256(f.SourcePath)
			if hashErr != nil {
				return nil, hashErr
			}
			name, nameErr := logBasename(f)
			if nameErr != nil {
				return nil, nameErr
			}
			m.Files = append(m.Files, ManifestFile{
				Path:      logRelPath(p, name),
				Size:      info.Size(),
				SHA256:    "sha256:" + digest,
				MediaType: "text/plain",
			})
		}
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	return m, nil
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
