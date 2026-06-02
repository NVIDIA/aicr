# ADR-009: Recipe Health Tracking and Communication

## Status

**Proposed** — 2026-05-30 (design-only; not implemented).

This Architecture Decision Record (ADR) specifies the V1 contract for
computing, recording, and publishing
recipe health across every supported criteria combination. The package
layout, CLI surface, generator, and CI workflow described below are intent,
not current behavior. Implementation will be tracked under a follow-on epic
once this ADR is accepted.

The V1 scope here was deliberately narrowed after a multi-perspective design
review (architecture, supply-chain security, developer experience,
Continuous Integration/Site Reliability Engineering (CI/SRE) operability, and
scope / You Aren't Gonna Need It (YAGNI)). The review found that an
evidence-driven
"validation posture" axis and two of the originally proposed structural
signals could not satisfy this ADR's own hermeticity and trust constraints
while `recipes/evidence/` is empty and Kubernetes WithOut Kubelet (KWOK)
results are ephemeral. Those
pieces are preserved as documented intent in *What V1 does not ship*, each
with a concrete pull-trigger. See *Review notes* at the end.

This ADR is the catalog-wide complement to
[ADR-007](007-recipe-evidence.md): ADR-007 answers *"did this one validation
run pass, verifiably?"* for a single recipe; this ADR answers *"across the
whole matrix, what is the current structural state of each recipe?"* — and
lays the frame for incorporating ADR-007 evidence as it lands. It never
restates evidence.

## Problem

NVIDIA AI Cluster Runtime (AICR) ships roughly 50 leaf recipes resolved from
~79 overlays
(`recipes/overlays/`), spanning combinations of service × accelerator ×
intent × OS × platform. We continuously want to evaluate these recipes
across several dimensions — resolvability, constraint validity, declared
validation coverage, chart-pin hygiene, and (as it becomes available)
evidence freshness — but today there is **no durable record of recipe
health and no way to communicate it**.

Concretely, the following questions have no single answer a maintainer or
user can consult:

- **Enumeration.** There is no command that lists the resolvable recipes at
  all. `aicr query` extracts a single value from one *already-chosen* recipe
  (`pkg/cli/query.go`); nothing enumerates the catalog. A user cannot
  discover which criteria combinations are supported, and a maintainer
  cannot tell whether an overlay refactor has silently orphaned one.
- **Resolvability and structural validity.** Does every advertised
  combination still resolve, with well-formed constraints and explicitly
  pinned component charts?
- **Declared coverage.** Which validation phases (`readiness`,
  `deployment`, `performance`, `conformance` — `pkg/recipe/validation.go`)
  does each recipe declare, and **which named checks** within each? Today
  this is invisible — there is no view of what validations a recipe even
  defines, let alone whether they pass.
- **Communication.** None of the above is surfaced anywhere a user or
  maintainer can read, and any surface that *is* built must never overstate
  what we know — a recipe that merely resolves cleanly must not read as
  "validated."

The data needed to answer the first four questions already exists in the
recipe resolver. What is missing is something that **enumerates the matrix,
computes per-recipe structural health, and communicates it** —
conservatively, and from signals that are fully reproducible in CI.

## Goals

1. A command that enumerates every resolvable recipe / criteria combination,
   closing the catalog-discovery gap directly.
2. Per-recipe **structural health** computed from signals that are
   **hermetic, offline, and deterministic** — reproducible from a checkout,
   no GPU, no network.
3. A single conservative, user-facing **public matrix**, generated and kept
   current with the same machinery the Bill of Materials (BOM) already uses.
4. **No false confidence.** Structural signals must never be rendered as
   "validated and performant." Runtime/validation claims come only from
   evidence, and are out of V1 scope (see Non-Goals).
5. A design whose evidence-driven axis can be added later without rework —
   the conceptual frame is fixed now; the code lands when evidence exists.

## Non-Goals

- **A validation-posture axis in V1.** `recipes/evidence/` does not exist
  yet, so an evidence-freshness axis would have exactly
  one reachable value (`unattested`) for every recipe. The *frame* is
  documented in Decision §3; the code, schema, freshness model, and
  evidence-sourcing CI are deferred until the first attestation lands.
- **Running live validation in CI.** V1 derives health from static analysis
  only. Scheduling GPU validation Jobs is a future extension gated on a
  hardware pool.
- **A committed machine-readable health artifact.** V1 renders Markdown
  directly, like `bom.md`. A committed `recipe-health.json` is deferred
  until a consumer (e.g. a `/v1/health` endpoint) exists.
- **A `/v1/health` REST endpoint.** Health is a catalog-wide, build-time
  concern, not a per-request runtime operation.
- **A blocking merge gate.** Structural health is advisory, like
  `make bom-check`.
- **An external dashboard, time-series store, or status badges.** Git
  history of the generated doc gives free trend.
- **Extending the recipe schema.** Health is computed *about* recipes; no
  new recipe fields are introduced.

## Context

Three existing patterns make this tractable and constrain the design:

- **The BOM generate-and-PR loop.** `tools/bom/main.go` renders a
  deterministic artifact from in-repo inputs, supports a `-deterministic`
  flag that suppresses per-run metadata so output is committable, and
  `make bom-docs` splices the generated body into a marked region
  (`<!-- BEGIN AICR-BOM -->` / `<!-- END AICR-BOM -->`) of a human doc while
  preserving surrounding prose (`Makefile`). `.github/workflows/
  bom-refresh.yaml` runs **weekly** and opens a bot PR via
  `peter-evans/create-pull-request` only on drift, against a fixed
  `chore/bom-refresh` branch with `delete-branch: true`. `make bom-check`
  is deliberately advisory — *not* wired into `make qualify` or the merge
  gate. **Note:** `tools/bom` does a *live* `helm template --repo <url>`
  render (it exists precisely to catch upstream image drift on pinned
  charts), so it is network-dependent — V1 health must not depend on a live
  render (see Decision §2, `chart_pinned`).

- **The recipe resolver.** `pkg/recipe` resolves a `Criteria`
  (`pkg/recipe/criteria.go`) through base → overlay → mixin merge
  (`pkg/recipe/metadata_store.go`, `pkg/recipe/builder.go`), returning a
  `RecipeResult` whose `Metadata` records `AppliedOverlays`,
  `ExcludedOverlays`, and `ConstraintWarnings`. `MetadataStore` already has
  an (unexported) `filterToMaximalLeaves` (`metadata_store.go`) that drops
  overlays which are ancestors of a more specific match — the basis for
  enumeration.

- **KWOK scheduling validation is ephemeral.** `.github/workflows/
  kwok-recipes.yaml` validates per-recipe scheduling, but emits results only
  as GitHub Actions job conclusions and a `$GITHUB_STEP_SUMMARY` table; the
  KWOK action uploads artifacts only `if: failure()`
  (`.github/actions/kwok-test/action.yml`). There is **no durable,
  in-repo, per-recipe pass/fail record** a separate job could read
  hermetically. A KWOK-derived structural signal is therefore deferred
  (Decision §2 / deferred table) until KWOK commits such a record.

**One fact materially shaped V1: `recipes/evidence/` does not exist** — zero
recipes carry attestations today. An evidence-derived axis would be dead on
arrival; it is deferred (Non-Goals, Decision §3).

## Decision

### 1. V1 scope: structural soundness + enumeration + one public matrix

V1 ships exactly three things:

1. `aicr recipe list` — enumerate resolvable criteria combinations.
2. `pkg/health.Compute()` — compute **structural soundness** per combination
   from hermetic, offline signals.
3. One generated public Markdown matrix (`docs/user/recipe-health.md`),
   refreshed weekly by a bot PR, exactly as the BOM is.

Everything evidence-driven — the validation-posture axis, freshness model,
evidence sourcing, a committed JSON artifact, a detailed committed internal
doc, an `aicr recipe health` subcommand, and the PR-time sticky-comment
workflow — is deferred (see *What V1 does not ship*). Each has a pull-trigger
so the line between "ship" and "defer" is explicit and revisitable.

### 2. Structural soundness axis (the only computed axis in V1)

Each combination is scored from signals that are **all hermetic and offline**
(reproducible from a checkout, no GPU, no network). Three signals are
**graded** (they move the rolled-up status); one is a captured **descriptor**
(surfaced, never scored):

| Dimension | Role | Signal | Source |
|---|---|---|---|
| `resolves` | graded | `BuildFromCriteria` returns a `RecipeResult` without error | `pkg/recipe/builder.go` |
| `constraints_wellformed` | graded | Every `Constraint.Value` parses; inline replay under `--no-cluster` is clean; no `ConstraintWarning`s | `pkg/recipe/metadata.go`, `pkg/validator` (no-cluster mode) |
| `chart_pinned` | graded | Every resolved component references an explicit pinned chart version/digest per [ADR-006](006-image-pinning-policy.md) — a pure in-repo check on the resolved recipe, **no Helm render** | resolved `RecipeResult` components; ADR-006 policy |
| `declared_coverage` | descriptor | For each of readiness/deployment/performance/conformance: present-or-not, the **named checks** declared, and the phase-level constraint count | `pkg/recipe/validation.go` (`ValidationPhase.Checks`, `.Constraints`) |

`declared_coverage` answers *"which validations are defined for this
recipe?"* It records the `Checks []string` each phase declares (e.g.
`deployment: [operator-health, expected-resources, gpu-operator-version,
check-nvidia-smi]`), not merely whether a phase is present. It is the
**declared** (static) side of validation coverage; the **declared-vs-run**
side needs the evidence predicate's executed phases and is deferred with the
validation axis (`coverage_declared_vs_run`, deferred table). It is rendered
informationally and **does not move the status** — a deliberately-minimal
recipe (e.g. the Slurm leaf that drops K8s checks) must not be penalized for
declaring fewer checks.

Per-dimension state (graded dimensions): `pass | warn | fail |
not-applicable | unknown`. These roll up to a single **status** per recipe:
`fail` if any graded dimension fails (including `resolves`), else `warn` if
any warns, else `pass`. Transient resolver errors
(`ErrCodeTimeout`/`ErrCodeInternal`) yield `unknown` and are **held, not
rolled up** — re-run rather than penalize; `unknown` is a representable value,
never an empty field a consumer could misread as `pass`.

V1 deliberately surfaces only this tri-state, **not** an A–F letter grade. A
letter on a surface labelled "health" reads as a deploy-readiness verdict
beyond what v1 computes, and one symbol overloads two distinct questions —
*how structurally sound?* and *how validated?* — that a reader correctly
reads as one (a `B` could mean either). This is the same argument §3 applies
to the Supported/Preview/Experimental vocabulary, applied to the grade:
letter grades are deferred until the validation axis can fuse into them
(deferred table).

**Compute budget.** The dominant cost is the `--no-cluster` constraint replay
across the ~50 leaf combos. The target is a sub-minute generator run, well
inside the weekly cadence's tolerance. If it grows past a few minutes, the
redesign levers — in order — are: cache per-combo results keyed by the
resolved-recipe digest; parallelize the replay; or drop inline replay from
the scheduled job and gate it to PRs touching `recipes/**`. None are needed
at v1 scale.

**Two originally proposed structural signals are deferred, not dropped:**
- `chart_drift` (upstream image drift on a pinned chart) requires a live
  Helm render and is therefore non-hermetic; it is already what the weekly
  `bom-refresh` job detects. Deferred to avoid a second network-dependent
  cron racing `bom-refresh` on the same root cause.
- `kwok_scheduling` has no durable in-repo source (see Context). Deferred
  until `kwok-recipes.yaml` commits a per-recipe result artifact.

**Failure modes (fail honestly, never silently pass).**
- A non-transient resolution error for one combo renders that combo as
  `fail`, with the error captured. The report still emits for all other
  combos — one broken recipe must not blank the matrix.
- Self-resolution is *not* assumed clean: although a leaf's own criteria
  always matches itself (`Criteria.Matches` is asymmetric,
  `pkg/recipe/criteria.go`), constraint-aware resolution
  (`BuildRecipeResultWithEvaluator`) can still exclude overlays — those
  exclusions surface as `constraints_wellformed: warn`/`fail`, not silent
  passes.

### 3. The two-axis principle (documented now; validation axis deferred)

Health is conceptually **two orthogonal axes that must never collapse into a
single fused score**: *structural soundness* (computed in V1) and
*validation posture* (deferred). This frame is fixed now so the deferred
axis lands without rework, and so V1's public surface is built to never
overstate:

- Structural soundness **never** asserts runtime behavior. A recipe that
  resolves cleanly is "structurally sound," **not** "validated."
- When the validation axis lands (first `recipes/evidence/<slug>.yaml`), it
  will be a *separate* column derived from **verified** evidence — gated on
  `aicr evidence verify` success, reading the signed predicate's
  `AttestedAt`, never trusting an unsigned in-tree timestamp (this is how
  `.github/scripts/recipe-evidence-check.sh` already establishes trust). It
  will distinguish `unattested` (never validated) from aged-but-verified
  states; these are different trust statements and will not share a label.

#### V1 public rendering

The matrix surfaces, per recipe:

- **Rolled-up structural status** (`pass | warn | fail`) — the lead signal,
  which varies across recipes and is real today.
- **Coverage** column, derived from `declared_coverage` — a compact per-phase
  summary (e.g. `R:2 D:4 P:1 C:10`, counts of declared checks per phase) so a
  reader sees at a glance which validations each recipe defines; the detailed
  view (`$GITHUB_STEP_SUMMARY`) lists the named checks per phase in full.
- **Evidence** column — a literal `pending` for every recipe in V1 (honest:
  no evidence exists yet), with a one-line prose note in the hand-written
  header explaining why.
- **Conservative tri-state vocabulary** (Supported / Preview / Experimental)
  — introduced in v1.1 *with* the validation axis, when it can actually
  differentiate recipes; folding everything into a uniform "Preview" today
  would convey no information and undersell structurally sound recipes.

### 4. Tooling surface

#### Enumeration is leaf-driven, not cartesian

The product of all criteria
values (~6,000) is wrong — most combinations resolve to nothing but `base`.
The canonical list is the de-duplicated `spec.criteria` of the
*maximal-leaf* overlays. Expose a new, explicitly-named seam on
`MetadataStore` that enumerates leaves over the whole overlay set and
returns exactly what callers need — criteria plus leaf name — rather than
exporting the internal filter or the heavyweight metadata struct:

```go
// pkg/recipe
type LeafCombo struct {
    Criteria    *Criteria
    OverlayName string
}

// LeafCriteria enumerates the maximal-leaf overlays of the whole catalog.
func (s *MetadataStore) LeafCriteria() []LeafCombo
```

Each combo is resolved back through `BuildFromCriteria` to prove
resolvability. ~50 guaranteed-resolvable combos.

#### CLI — `aicr recipe list` only in V1

Added as a subcommand of the
existing `recipe` command, which retains its current bare generate action
(urfave/cli supports a command with both an Action and subcommands). Flags
follow existing conventions (`pkg/cli/root.go`, `pkg/cli/consts.go`):
`--format json|yaml|table` (serialization; reuse `formatFlag()`, default
`table`), `-o`/`--output` (destination), `--data`, `--criteria-strict`. The
`table` view has **one column per criteria dimension** (service / accelerator
/ os / intent / platform) plus the leaf name, structural status, and a compact
declared-coverage summary, so a row maps 1:1 to
`aicr recipe --service … --accelerator …` — i.e. users can round-trip
enumeration → generate/bundle. The `json`/`yaml` views emit the full
`declared_coverage` (named checks per phase) for programmatic consumers. `pkg/cli` stays logic-free;
the command parses flags and delegates to `pkg/health`. A nested-struct
report does not serialize usefully through the generic table writer
(`pkg/serializer/writer.go`), so the command renders its own curated grid.

`aicr recipe health` (ad-hoc per-combo health) is **deferred** — the
`tools/health` generator calls `pkg/health.Compute` directly and needs no
user-facing verb. Pull-trigger: a user asks to compute health for one
ad-hoc combo outside the generated doc.

#### Business logic — new `pkg/health` package

V1 has no dependency on
`pkg/evidence`, so health hosted in `pkg/recipe` would compile cleanly today;
the standalone package is a **forward-looking boundary choice, not a v1
requirement.** It exists to keep the v1.1 evidence import acyclic:
`pkg/evidence/attestation` already imports `pkg/recipe`, so once the
validation axis imports evidence, health living in `pkg/recipe` would close a
`recipe → evidence → recipe` cycle. A standalone `pkg/health` depending on
both stays acyclic and avoids a v1.1 package move. V1 dependencies:
`pkg/recipe`, `pkg/serializer`, `pkg/errors`, `pkg/defaults`. Sketch:

```go
package health

const SchemaVersion = "1.0.0" // forward-compat; emitted if/when the report is serialized

type Options struct {
    Provider recipe.DataProvider // nil ⇒ package-global catalog
    Version  string
    Filter   *recipe.Criteria    // nil ⇒ all leaf combos
}

type PhaseCoverage struct {
    Declared    bool     // phase block present after merge
    Checks      []string // named checks declared (sorted)
    Constraints int      // phase-level constraint count
}
type DeclaredCoverage struct {
    Readiness, Deployment, Performance, Conformance PhaseCoverage
}

type StructureHealth struct {
    Status     string            // pass | warn | fail | unknown (held); rolled up from graded dimensions
    Dimensions map[string]string // graded dimension → state
    Detail     map[string]string // graded dimension → human note
    Coverage   DeclaredCoverage  // descriptor: which validations are defined (not graded)
}

type ComboHealth struct {
    Criteria    *recipe.Criteria
    LeafOverlay string
    Structure   StructureHealth
    // Validation ValidationHealth — added in v1.1 with the evidence axis
}

type Report struct {
    SchemaVersion string        // = SchemaVersion
    Combos        []ComboHealth // sorted by criteria string for determinism
}

func Compute(ctx context.Context, opts Options) (*Report, error)
```

`Compute` enumerates leaves via `LeafCriteria()`, builds each through
`recipe.NewBuilder(...).BuildFromCriteria`, and scores the four structural
dimensions from `RecipeResult.Metadata` and the resolved components. Combos
are sorted by criteria string. **V1 has no time-dependent input**, so output
is a pure function of the checkout — there is no `time.Now()` anywhere, and
the determinism/wall-clock tension that an evidence-freshness axis would
introduce simply does not arise.

**No REST endpoint in V1** (Non-Goals). Because all logic is in
`pkg/health.Compute`, a future `/v1/health` handler is a thin add.

### 5. Publication

Mirror the BOM precedent precisely:

- `make health-docs` runs `go run ./tools/health -deterministic -no-title`
  and splices the matrix into `docs/user/recipe-health.md` between
  `<!-- BEGIN AICR-HEALTH -->` / `<!-- END AICR-HEALTH -->`. `make
  health-check` is the advisory staleness check (paralleling `bom-check`),
  **not** wired into `make qualify`.
- `.github/workflows/health-refresh.yaml` structurally clones
  `bom-refresh.yaml`: **weekly** cron, fixed `chore/health-refresh` branch,
  `delete-branch: true`, `peter-evans/create-pull-request` on drift, built-in
  `GITHUB_TOKEN`, labels `documentation` / `area/docs` / `area/recipes`.
  Weekly (not daily) is correct: V1 health changes only on code / registry /
  chart changes — all merge events — not on the calendar, so there is no
  daily signal to catch. The workflow inherits the repo's `/ok`
  reviewer-comment policy to re-fire CI on the bot PR (so its own
  `fern-docs-ci`/lychee checks don't sit pending).
- The weekly job also writes the per-dimension detail to
  `$GITHUB_STEP_SUMMARY` for at-a-glance maintainer triage. A *committed*
  internal detail doc is deferred — with the validation axis gone from V1,
  the detail content (structural per-dimension states) is thin and the step
  summary suffices.
- **Doc obligations (same PR):** register `docs/user/recipe-health.md` in
  `docs/index.yml` under *User Guide*; document `aicr recipe list` and its
  flags in `docs/user/cli-reference.md`; cross-link from
  `docs/user/component-catalog.md`. The matrix is plain Markdown only —
  `fern-docs-ci.yaml` runs lychee and MDX-safety checks on `docs/**`.

### 6. Ownership

Add a `CODEOWNERS` entry for `docs/user/recipe-health.md`, `tools/health/**`,
and `pkg/health/**`, and state a triage expectation: the recipes/area owners
review the weekly `chore/health-refresh` PR within a few days. The existing
`inactive-pr-reminder.yaml` daily nudge surfaces a neglected bot PR. Without
an owner, an advisory matrix silently goes stale — and a stale public
"health" surface is worse than none, because users act on it.

### What V1 does *not* ship

| Deferred feature | Pull-trigger to bring it in |
|---|---|
| Validation-posture axis (evidence freshness, `coverage_declared_vs_run`, freshness model, `--evidence-dir`/`--max-age`) | The first `recipes/evidence/<slug>.yaml` lands |
| Committed machine-readable `recipe-health.json` | A consumer needs it (e.g. the `/v1/health` endpoint) |
| Committed internal detail doc (`docs/contributor/recipe-health-detail.md`) | Detail content grows beyond what `$GITHUB_STEP_SUMMARY` conveys (i.e. once the validation axis lands) |
| `aicr recipe health` subcommand | A user needs ad-hoc per-combo health outside the generated doc |
| PR-time sticky-comment two-workflow apparatus | The weekly bot PR proves noisy enough that contributors want pre-merge deltas |
| `kwok_scheduling` structural signal | `kwok-recipes.yaml` commits a durable per-recipe result artifact |
| `chart_drift` (upstream image drift) structural signal | A hermetic chart cache lands, or the signal is folded into `bom-refresh` output health reads offline |
| OCI-sourced evidence freshness | A trusted-registry allowlist for attestation pulls in CI |
| `/v1/health` REST endpoint | A first hosted/automation consumer that needs health per request |
| Conservative tri-state public vocabulary (Supported/Preview/Experimental) | The validation axis lands and can differentiate recipes |
| Letter grades (A–F) on the matrix | The validation axis lands and can fuse into a single graded verdict |
| Blocking merge gate on health | Health signals mature enough that failures are author-fixable |
| External dashboard / Pages site / status badges | Demand for a surface outside the docs site |

## Consequences

**Positive.** V1 ships real value against data that exists today: the
long-missing enumeration gap is closed by `aicr recipe list`, and the
structural matrix flags broken/unpinned/constraint-warning recipes. Every
V1 signal is hermetic and offline, so the generated doc is reproducible from
a checkout with no network and no wall-clock — deterministic under the listed
V1 inputs and constraints, and strictly simpler than the originally proposed
evidence-driven design. The two-axis *principle* is fixed now, so the
evidence axis lands later as an additive column, not a redesign. The CI
surface is a faithful clone of the proven, weekly BOM loop — one workflow,
no new fork-safety apparatus.

**Negative.** V1's value is structural-only until ADR-007 evidence lands;
the headline "is this recipe validated?" question is explicitly out of scope.
The public matrix's Evidence column is a uniform `pending` on day one (honest,
but not differentiating). A maintainer must act on the weekly bot PR;
CODEOWNERS + the inactivity nudge mitigate but do not force this.

**Neutral.** Adds a `pkg/health` package and a `tools/health` generator
parallel to `pkg/bom` / `tools/bom`, one weekly workflow alongside the
existing weekly BOM and nightly KWOK schedules, one new public doc, and one
new CLI subcommand.

## Review notes

This ADR was narrowed from an initial design that shipped both axes (with a
committed JSON artifact, a daily cron, a PR-time sticky-comment apparatus,
and an `aicr recipe health` command) after a multi-perspective design review.
The decisive findings:

- **Hermeticity violations (blocking).** `kwok_scheduling` had no durable
  in-repo source, and `chart_drift` via a live BOM render reintroduced
  the very network dependency the design rejected for evidence — both
  contradicting the stated "hermetic, offline, deterministic" goal. Resolved
  by deferring `kwok_scheduling`, redefining the pinning signal as a
  render-free `chart_pinned` check, and deferring upstream-drift detection
  to the existing `bom-refresh` job.
- **Dead-on-arrival axis (scope).** With `recipes/evidence/` empty, the
  entire validation-posture axis had one reachable value. Deferred to v1.1
  with the frame preserved.
- **Forgeable freshness (trust).** Reading `AttestedAt` from an unsigned
  in-tree pointer would have let a PR author fake "fresh." The deferred axis
  will gate on `aicr evidence verify`, matching `recipe-evidence-check.sh`.
- **DX / correctness fixes folded in:** a `--view`/`--format` flag collision
  avoided by deferring the only command that needed a view selector; a
  `schemaVersion` constant added for forward-compat; the import-cycle
  rationale corrected; the enumeration seam changed from exporting an
  internal filter to a purpose-built `LeafCriteria()`; explicit fail-honest
  behavior; and the missing `docs/index.yml` / `cli-reference.md` / CODEOWNERS
  obligations made explicit.
