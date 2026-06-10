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

package health

import (
	"context"
	stderrors "errors"
	"sort"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// SchemaVersion is the version of the Report schema. It is forward-compat
// metadata, emitted if/when the report is serialized so consumers can detect
// shape changes across releases.
const SchemaVersion = "1.0.0"

// Per-dimension and rolled-up status values. These are the only legal values
// for a graded dimension's state and for ComboHealth status.
const (
	// StatusPass means the dimension (or recipe) is structurally sound.
	StatusPass = "pass"

	// StatusWarn means the dimension surfaced a non-fatal concern.
	StatusWarn = "warn"

	// StatusFail means a graded dimension failed; it forces the rollup to fail.
	StatusFail = "fail"

	// StatusUnknown is held: a transient resolver error (ErrCodeTimeout or
	// ErrCodeInternal) prevented a confident verdict. It is excluded from the
	// fail/warn rollup so a re-runnable hiccup never penalizes a recipe, but it
	// surfaces as the recipe status when nothing fails or warns — never as pass.
	StatusUnknown = "unknown"
)

// Graded dimension keys. Later work adds more keys (chart_pinned,
// declared_coverage, constraints_wellformed) without changing the rollup.
const (
	// DimResolves is the graded dimension scoring whether the recipe builder
	// resolves the criteria into a RecipeResult without error.
	DimResolves = "resolves"
)

// Options configures a Compute run.
type Options struct {
	// Provider is the recipe DataProvider to enumerate and resolve against.
	// Nil selects the package-global embedded catalog.
	Provider recipe.DataProvider

	// Version stamps the recipe builder version used during resolution. It
	// does not affect health scoring; it mirrors the CLI version for parity
	// with normal recipe generation.
	Version string

	// Filter narrows enumeration to leaf overlays matching every explicitly
	// set criteria dimension. Nil enumerates all leaf combos.
	Filter *recipe.Criteria
}

// PhaseCoverage records which named checks and how many phase-level
// constraints a single validation phase declares. It is a descriptor, never
// graded. Populated by the declared_coverage signal (added in later work);
// the resolves-only core leaves it at its zero value.
type PhaseCoverage struct {
	// Declared is true when the phase block is present after overlay merge.
	Declared bool `json:"declared" yaml:"declared"`

	// Checks are the named checks the phase declares, sorted.
	Checks []string `json:"checks,omitempty" yaml:"checks,omitempty"`

	// Constraints is the phase-level constraint count.
	Constraints int `json:"constraints" yaml:"constraints"`
}

// DeclaredCoverage captures, per validation phase, which validations a recipe
// defines. It is a descriptor — it never moves the rolled-up status, so a
// deliberately minimal recipe is not penalized for declaring fewer checks.
type DeclaredCoverage struct {
	Readiness   PhaseCoverage `json:"readiness"   yaml:"readiness"`
	Deployment  PhaseCoverage `json:"deployment"  yaml:"deployment"`
	Performance PhaseCoverage `json:"performance" yaml:"performance"`
	Conformance PhaseCoverage `json:"conformance" yaml:"conformance"`
}

// StructureHealth is the structural-soundness axis for one recipe.
type StructureHealth struct {
	// Status is the rolled-up verdict: pass | warn | fail | unknown (held).
	Status string `json:"status" yaml:"status"`

	// Dimensions maps each graded dimension key to its state. The rollup
	// iterates this map generically, so adding a dimension does not change
	// rollup logic.
	Dimensions map[string]string `json:"dimensions" yaml:"dimensions"`

	// Detail maps a graded dimension key to a human-readable note (e.g., the
	// resolver error for a fail or held-unknown). Absent for clean dimensions.
	Detail map[string]string `json:"detail,omitempty" yaml:"detail,omitempty"`

	// Coverage is a descriptor recording which validations the recipe defines.
	// It is never graded. Populated by later work; zero-valued here.
	Coverage DeclaredCoverage `json:"coverage" yaml:"coverage"`
}

// ComboHealth is the health of a single leaf recipe / criteria combination.
type ComboHealth struct {
	// Criteria is the leaf overlay's criteria combination.
	Criteria *recipe.Criteria `json:"criteria" yaml:"criteria"`

	// LeafOverlay is the name of the leaf overlay backing this combination.
	LeafOverlay string `json:"leafOverlay" yaml:"leafOverlay"`

	// Structure is the structural-soundness axis. A future validation-posture
	// axis is added here as a separate field, never fused into Structure.
	Structure StructureHealth `json:"structure" yaml:"structure"`
}

// Report is the catalog-wide health snapshot.
type Report struct {
	// SchemaVersion is the schema version, equal to SchemaVersion.
	SchemaVersion string `json:"schemaVersion" yaml:"schemaVersion"`

	// Combos are the per-recipe health entries, sorted by criteria string
	// (tie-broken by leaf overlay name) for deterministic output.
	Combos []ComboHealth `json:"combos" yaml:"combos"`
}

// Compute enumerates every leaf recipe in the catalog and scores each against
// the structural signals, returning a deterministic Report.
//
// Enumeration uses MetadataStore.ListCatalog filtered to leaf overlays.
// Resolution runs through a single shared recipe.Builder so the owner-stamp
// invariant holds identically across combos. The returned Report carries
// map-typed fields; callers serializing it must use
// serializer.MarshalYAMLDeterministic.
func Compute(ctx context.Context, opts Options) (*Report, error) {
	ctx, cancel := context.WithTimeout(ctx, defaults.HealthComputeTimeout)
	defer cancel()

	store, err := recipe.LoadMetadataStoreFor(ctx, opts.Provider)
	if err != nil {
		return nil, errors.PropagateOrWrap(err,
			errors.ErrCodeInternal, "failed to load recipe catalog for health computation")
	}

	// A single shared builder bound to the same provider used for enumeration,
	// so every combo resolves against one consistent metadata store and carries
	// the same owner stamp.
	builder := recipe.NewBuilder(
		recipe.WithVersion(opts.Version),
		recipe.WithDataProvider(opts.Provider),
	)

	report := &Report{SchemaVersion: SchemaVersion}
	for _, entry := range store.ListCatalog(opts.Filter) {
		// Fail loud on cancellation rather than emitting a truncated report:
		// once ctx is done, every remaining BuildFromCriteria short-circuits to
		// ErrCodeTimeout (graded unknown), so a partial run would otherwise look
		// byte-for-byte like a healthy catalog with transient unknowns.
		if err := ctx.Err(); err != nil {
			return nil, errors.Wrap(errors.ErrCodeTimeout,
				"health computation canceled before completing the catalog", err)
		}
		if !entry.IsLeaf {
			continue
		}
		report.Combos = append(report.Combos, computeCombo(ctx, builder, entry))
	}

	sort.Slice(report.Combos, func(i, j int) bool {
		ci, cj := report.Combos[i].Criteria.String(), report.Combos[j].Criteria.String()
		if ci != cj {
			return ci < cj
		}
		return report.Combos[i].LeafOverlay < report.Combos[j].LeafOverlay
	})

	return report, nil
}

// computeCombo scores a single leaf overlay's structural health.
func computeCombo(ctx context.Context, builder *recipe.Builder, entry recipe.CatalogEntry) ComboHealth {
	structure := StructureHealth{
		Dimensions: make(map[string]string),
		Detail:     make(map[string]string),
	}

	_, err := builder.BuildFromCriteria(ctx, entry.Criteria)
	state, detail := classifyResolve(err)
	structure.Dimensions[DimResolves] = state
	if detail != "" {
		structure.Detail[DimResolves] = detail
	}

	structure.Status = rollup(structure.Dimensions)

	return ComboHealth{
		Criteria:    entry.Criteria,
		LeafOverlay: entry.Name,
		Structure:   structure,
	}
}

// classifyResolve grades the resolves dimension from a build error.
//
// A nil error is pass. A genuinely re-runnable error (context cancellation or
// ErrCodeTimeout) is held as unknown — re-run rather than penalize. Any other
// error is a fail. The error message is returned as the dimension detail for
// the non-pass cases.
func classifyResolve(err error) (state, detail string) {
	if err == nil {
		return StatusPass, ""
	}
	if isTransient(err) {
		return StatusUnknown, err.Error()
	}
	return StatusFail, err.Error()
}

// isTransient reports whether err is genuinely re-runnable — a context
// cancellation/deadline or an ErrCodeTimeout — and so should be held as
// unknown rather than penalized.
//
// ErrCodeInternal is deliberately excluded. On the resolve path
// (BuildFromCriteria) cancellation and per-combo timeouts are both wrapped as
// ErrCodeTimeout (pkg/recipe builder.go, metadata_store.go), while
// ErrCodeInternal is reserved for deterministic structural defects — e.g. a
// registry healthCheck.assertFile pointing at a missing path, flattened to
// ErrCodeInternal in finalizeRecipeResult. Re-running never clears those, so
// holding them as unknown would mask a broken recipe forever instead of
// failing honestly. This narrows ADR-009 §2's literal "ErrCodeTimeout/
// ErrCodeInternal → unknown" to match its stated intent ("transient ...
// re-run rather than penalize"); see PR discussion on #1225.
func isTransient(err error) bool {
	if stderrors.Is(err, context.DeadlineExceeded) || stderrors.Is(err, context.Canceled) {
		return true
	}
	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		return false
	}
	return se.Code == errors.ErrCodeTimeout
}

// rollup reduces graded dimension states to a single status. It is generic
// over the dimension set: precedence is fail > warn > unknown > pass. unknown
// is held — it never forces fail/warn — but surfaces as the status when no
// dimension fails or warns, so a held verdict is never misread as pass.
func rollup(dimensions map[string]string) string {
	var warn, unknown bool
	for _, state := range dimensions {
		switch state {
		case StatusFail:
			return StatusFail
		case StatusWarn:
			warn = true
		case StatusUnknown:
			unknown = true
		}
	}
	switch {
	case warn:
		return StatusWarn
	case unknown:
		return StatusUnknown
	default:
		return StatusPass
	}
}
