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
	"fmt"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/constraints"
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

// Graded dimension keys. Later work adds more keys (constraints_wellformed)
// without changing the rollup.
const (
	// DimResolves is the graded dimension scoring whether the recipe builder
	// resolves the criteria into a RecipeResult without error.
	DimResolves = "resolves"

	// DimChartPinned is the graded dimension scoring whether every resolved
	// Helm component references an explicit chart version (ADR-006 layer 1).
	DimChartPinned = "chart_pinned"

	// DimConstraintsWellformed is the graded dimension scoring whether every
	// merged constraint parses with the snapshot-free constraint parsers
	// (fail), and whether the constraint-aware resolution surfaced any
	// composition warnings (warn). It is parse-only well-formedness — V1 is
	// hermetic and never replays a constraint against cluster/snapshot state.
	// See classifyConstraintsWellformed for why the warn branch is inert under
	// the current satisfied-stub resolution.
	DimConstraintsWellformed = "constraints_wellformed"
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
// graded.
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

// Compact renders the per-phase declared-check counts as a single line in
// phase order — readiness, deployment, performance, conformance — e.g.
// "R:2 D:4 P:1 C:10". It is the single source of truth for this format,
// shared by the recipe-health matrix (tools/health) and the `aicr recipe
// list` table (pkg/cli) so the two cannot drift. The caller owns the nil
// case: a nil *DeclaredCoverage means coverage is unknown (the recipe did
// not resolve), which each surface renders with its own placeholder rather
// than a misleading all-zero block, so this method is only valid on a
// non-nil receiver.
func (c *DeclaredCoverage) Compact() string {
	return fmt.Sprintf("R:%d D:%d P:%d C:%d",
		len(c.Readiness.Checks),
		len(c.Deployment.Checks),
		len(c.Performance.Checks),
		len(c.Conformance.Checks),
	)
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
	// resolver error for a fail/held-unknown, or the not-applicable note on a
	// vacuous chart_pinned pass). A genuinely clean dimension has no entry.
	Detail map[string]string `json:"detail,omitempty" yaml:"detail,omitempty"`

	// Coverage is a descriptor recording which validations the recipe defines.
	// It is never graded. Non-nil only when the recipe resolves; nil otherwise
	// (no RecipeResult to read), so the field is omitted rather than emitting a
	// misleading all-zero block for a combo whose coverage is simply unknown.
	Coverage *DeclaredCoverage `json:"coverage,omitempty" yaml:"coverage,omitempty"`
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

	// Resolve through the constraint-aware path with a hermetic satisfied stub.
	// With every constraint reported satisfied this merges exactly the overlays
	// BuildFromCriteria would (no cluster-dependent exclusions), so the resolves
	// grade is unchanged — but the result additionally carries the merged
	// Constraints and any ConstraintWarnings/ExcludedOverlays the constraint-aware
	// path surfaces, which the constraints_wellformed signal reads. One build,
	// no snapshot, no cluster.
	result, err := builder.BuildFromCriteriaWithEvaluator(ctx, entry.Criteria, satisfiedEvaluator)
	state, detail := classifyResolve(err)
	structure.Dimensions[DimResolves] = state
	if detail != "" {
		structure.Detail[DimResolves] = detail
	}

	// The remaining structural signals are pure reads of the resolved
	// RecipeResult, so they only apply when resolution succeeded — a failed or
	// held combo has no result to inspect.
	if err == nil {
		pinnedState, pinnedDetail := classifyChartPinned(result)
		structure.Dimensions[DimChartPinned] = pinnedState
		if pinnedDetail != "" {
			structure.Detail[DimChartPinned] = pinnedDetail
		}

		cwState, cwDetail := classifyConstraintsWellformed(result)
		structure.Dimensions[DimConstraintsWellformed] = cwState
		if cwDetail != "" {
			structure.Detail[DimConstraintsWellformed] = cwDetail
		}

		cov := computeCoverage(result)
		structure.Coverage = &cov
	}

	structure.Status = rollup(structure.Dimensions)

	return ComboHealth{
		Criteria:    entry.Criteria,
		LeafOverlay: entry.Name,
		Structure:   structure,
	}
}

// classifyChartPinned grades the chart_pinned dimension: every enabled Helm
// component must reference an explicit chart version (ADR-006 layer 1 — the
// chart-version pin, not image digests). An unpinned Helm component is a fail.
//
// This is a pure in-repo field check — no Helm render. It is a presence check
// (non-empty Version), not range/exact-pin validation: a floating version
// would score pass, but the registry pins exact versions, so layer 1 is
// satisfied by presence. Disabled components (overrides.enabled: false) are
// skipped — they are never bundled or deployed, so an unpinned disabled
// component must not flip the recipe to fail; this matches every other
// consumer of ComponentRefs (bundler, deployers, mirror, BOM).
//
// Manifest-only "Helm" components (typed Helm but carrying local manifestFiles
// with no external chart/source — e.g. nodewright-customizations) are also
// skipped: there is no external chart version to pin, so grading them against
// the pin requirement is a false positive. See isManifestOnlyHelm.
//
// A recipe with no enabled Helm components (e.g. pure-Kustomize) scores a
// vacuous pass with an explanatory detail, since Kustomize defaultTag pinning
// is out of scope and the column must not be misread as "supply-chain pinned".
//
// The dimension therefore has three distinguishable states for a downstream
// renderer: pass with a detail (vacuous — no Helm to pin), pass with no detail
// (genuinely pinned), and absent from Dimensions entirely (resolve failed/held,
// so it was never scored).
func classifyChartPinned(result *recipe.RecipeResult) (state, detail string) {
	if result == nil {
		return StatusPass, ""
	}

	helmCount := 0
	var unpinned []string
	for _, ref := range result.ComponentRefs {
		if ref.Type != recipe.ComponentTypeHelm || !ref.IsEnabled() {
			continue
		}
		// Manifest-only "Helm" components (e.g. nodewright-customizations) are
		// typed Helm but ship local manifestFiles with no external chart, so
		// they have no chart version to pin. Skip them — exactly like disabled
		// components — rather than flag a non-existent pin as unpinned.
		if isManifestOnlyHelm(ref) {
			continue
		}
		helmCount++
		if ref.Version == "" {
			unpinned = append(unpinned, ref.Name)
		}
	}

	if len(unpinned) > 0 {
		sort.Strings(unpinned)
		return StatusFail, "unpinned Helm chart version for: " + strings.Join(unpinned, ", ")
	}
	if helmCount == 0 {
		return StatusPass, "no enabled Helm components; chart-version pinning not applicable (Kustomize tag pinning is out of scope)"
	}
	return StatusPass, ""
}

// satisfiedEvaluator is the hermetic stub fed to BuildFromCriteriaWithEvaluator
// so the constraint-aware resolution path executes offline. It reports every
// constraint satisfied, so no overlay is excluded and no ConstraintWarning is
// emitted — the resolution exercises the merge/compose machinery without any
// snapshot measurement, which is all the parse-only signal needs.
func satisfiedEvaluator(recipe.Constraint) recipe.ConstraintEvalResult {
	return recipe.ConstraintEvalResult{Passed: true}
}

// classifyConstraintsWellformed grades the constraints_wellformed dimension
// hermetically — no snapshot, no cluster.
//
// Primary (fail): every merged constraint must parse. Both the path
// (ParseConstraintPath over Name) and the value expression
// (ParseConstraintExpression over Value) are checked with the exported,
// snapshot-free parsers. The first malformed constraint fails the dimension,
// naming the offending constraint and the parser error in the detail — a
// malformed constraint is never a silent pass. The merged constraint order is
// deterministic for a given leaf (sorted by name once any overlay/mixin merges,
// base-file order otherwise), so the reported failure is stable across runs.
//
// This is structural parse well-formedness only: it does not validate value
// semantics. A version-comparison value that parses but is not a usable version
// (e.g. ">= not-a-version") scores pass here — semantic/range checking and any
// snapshot-dependent constraint replay are deferred to the evidence-driven
// validation axis (ADR-009 coverage_declared_vs_run).
//
// Secondary (warn): if every constraint parses but the resolution surfaced
// ConstraintWarnings or ExcludedOverlays, the dimension warns. NOTE: these
// fields are populated only when the injected evaluator reports a constraint
// failed or errored (see metadata_store.evaluateOverlayConstraints /
// evaluateMixinConstraints). satisfiedEvaluator never does, so under the
// current hermetic resolution the warn branch does not fire — composition
// problems surface instead as resolve errors. The branch is retained because it
// is the correct reading of a resolved result and is forward-compatible with a
// future evaluator wired into this path; it is exercised by unit tests via
// direct field injection.
//
// A nil result (resolve failed or held) is never scored by the caller; pass is
// returned defensively.
func classifyConstraintsWellformed(result *recipe.RecipeResult) (state, detail string) {
	if result == nil {
		return StatusPass, ""
	}

	for _, c := range result.Constraints {
		if _, err := constraints.ParseConstraintPath(c.Name); err != nil {
			return StatusFail, fmt.Sprintf("constraint %q: malformed path: %v", c.Name, err)
		}
		if _, err := constraints.ParseConstraintExpression(c.Value); err != nil {
			return StatusFail, fmt.Sprintf("constraint %q: malformed value %q: %v", c.Name, c.Value, err)
		}
	}

	if warnings := result.Metadata.ConstraintWarnings; len(warnings) > 0 {
		w := warnings[0]
		return StatusWarn, fmt.Sprintf(
			"%d constraint warning(s) during resolution; first: overlay %q constraint %q: %s",
			len(warnings), w.Overlay, w.Constraint, w.Reason)
	}
	if excluded := result.Metadata.ExcludedOverlays; len(excluded) > 0 {
		return StatusWarn, fmt.Sprintf(
			"%d overlay(s) excluded during constraint-aware resolution; first: %q (%s)",
			len(excluded), excluded[0].Name, excluded[0].Reason)
	}

	return StatusPass, ""
}

// isManifestOnlyHelm reports whether a Helm-typed ComponentRef is a
// manifest-only component: it references no external chart (empty Chart and
// Source) but ships local manifest files. Such a component has no chart
// version to pin, so chart_pinned must not grade it. This mirrors the
// manifest-only detection in the bundler's deployers (ref.Chart == "" &&
// ref.Source == "" with manifests present), keeping the health signal aligned
// with what is actually bundled and deployed.
func isManifestOnlyHelm(ref recipe.ComponentRef) bool {
	return ref.Chart == "" && ref.Source == "" &&
		(len(ref.ManifestFiles) > 0 || len(ref.PreManifestFiles) > 0)
}

// computeCoverage builds the declared_coverage descriptor from the resolved
// recipe's validation config. A nil config (or nil phase) yields a zero-value
// PhaseCoverage (Declared=false) — a minimal recipe that drops phases is not
// penalized, since coverage is descriptive and never graded.
func computeCoverage(result *recipe.RecipeResult) DeclaredCoverage {
	if result == nil || result.Validation == nil {
		return DeclaredCoverage{}
	}
	v := result.Validation
	return DeclaredCoverage{
		Readiness:   phaseCoverage(v.Readiness),
		Deployment:  phaseCoverage(v.Deployment),
		Performance: phaseCoverage(v.Performance),
		Conformance: phaseCoverage(v.Conformance),
	}
}

// phaseCoverage describes one validation phase: whether it is declared, the
// sorted names of its checks, and its phase-level constraint count.
func phaseCoverage(p *recipe.ValidationPhase) PhaseCoverage {
	if p == nil {
		return PhaseCoverage{}
	}
	pc := PhaseCoverage{
		Declared:    true,
		Constraints: len(p.Constraints),
	}
	if len(p.Checks) > 0 {
		pc.Checks = make([]string, len(p.Checks))
		copy(pc.Checks, p.Checks)
		sort.Strings(pc.Checks)
	}
	return pc
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
