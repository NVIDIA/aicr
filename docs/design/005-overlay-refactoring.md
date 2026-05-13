# ADR-005: Refactor Overlay Structure to Reduce Duplication

## Status

**Proposed** — 2026-03-19
**Revised** — 2026-04-06 (prototype findings, resequenced phases)

## Problem

The overlay system has **43 files** with significant duplication that grows
with each new accelerator and service:

- K8s version constraints repeated in **16 files**
- Ubuntu OS constraints repeated in **9 files**
- Validation checks repeated in **10+ files**
- GPU operator overrides duplicated across training/inference variants
- Platform components (kubeflow, dynamo) duplicated across accelerator+service combinations
- Single-parent inheritance forces deep inheritance chains (up to **6 levels**)

Adding new accelerators (B200, GB300) and services (OKE) under the current
structure would grow the tree to **96-120 files**, each carrying duplicated
boilerplate from sibling leaf overlays.

### Impact

The duplication is a **maintenance and correctness problem**, not primarily a
line-count problem:

- Upgrading an Ubuntu version or chart version requires editing 4-9 files
  instead of 1, increasing review burden and merge conflict surface
- Drift between sibling overlays (e.g., forgetting to update one of 9 Ubuntu
  leaves) produces silently wrong recipes
- Each new accelerator+service+OS+platform combination requires copy-pasting
  ~30 lines of constraints and components from a sibling overlay

## Non-Goals

- No dimension auto-compose in this ADR (Auto-Compose option is documented for
  reference only)
- No changes to the recipe CLI interface or recipe output format

## Context

AICR uses a layered overlay system to generate GPU-accelerated Kubernetes
configurations. Each overlay inherits from a single parent via `spec.base`,
and the resolver merges matching overlays from least-specific to most-specific
to produce a final recipe.

### Current overlay tree

The tree branches on service, then intent, then accelerator, then OS, then
platform — creating deep single-parent inheritance chains:

```
base
├── eks
│   ├── eks-training
│   │   ├── h100-eks-training
│   │   │   ├── h100-eks-ubuntu-training
│   │   │   │   └── h100-eks-ubuntu-training-kubeflow
│   │   │   └── (future: h100-eks-ubuntu-training-dynamo, etc.)
│   │   └── gb200-eks-training
│   │       └── gb200-eks-ubuntu-training
│   │           └── gb200-eks-ubuntu-training-kubeflow
│   └── eks-inference
│       ├── h100-eks-inference
│       │   └── h100-eks-ubuntu-inference
│       │       ├── h100-eks-ubuntu-inference-dynamo
│       │       └── h100-eks-ubuntu-inference-nim
│       └── gb200-eks-inference
│           └── gb200-eks-ubuntu-inference
│               └── gb200-eks-ubuntu-inference-dynamo
├── aks (same structure with H100 only)
├── gke-cos (same structure with H100 only, COS instead of Ubuntu)
├── kind (minimal, H100 only)
├── monitoring-hpa
├── b200-any-training
└── gb200-any-training
```

### Current resolver behavior

The resolver in `FindMatchingOverlays` returns **all overlays whose criteria
match the query**, sorted by specificity. `mergeOverlayChains` then resolves
each match's full inheritance chain and merges them, deduplicating overlays
that appear in multiple chains.

This all-match behavior is important context for the design choices below:
any overlay whose criteria match the query is applied independently, regardless
of whether it is an ancestor of a more-specific match. The current overlay
tree works correctly because the inheritance chains are linear — every
matching overlay is an ancestor of the most-specific leaf.

### Resolver correctness issue: `Specificity()` bug

`Specificity()` in `criteria.go` counts criteria fields where
`field != "any"`. However, when overlay YAML is parsed, omitted criteria
fields get the Go zero value `""` (empty string), not `"any"`. Since
`"" != "any"` evaluates to true, all overlays report specificity 4-5
regardless of how many criteria fields they actually specify.

The sort in `FindMatchingOverlays` becomes non-deterministic for overlays
with different numbers of specified criteria. This has been masked by the
linear chain structure — the current tree doesn't have competing overlays
at the same specificity level. `Matches()` and `MatchesCriteriaField()`
already treat `""` as equivalent to `"any"`, so the fix is to align
`Specificity()` with them.

### Duplication hotspots

| Duplicated Content | Occurrences | Impact |
|-------------------|-------------|--------|
| K8s `>= 1.32.4` constraint | 16 files | Must update in every accelerator leaf overlay |
| Ubuntu OS constraints (3 lines) | 9 files | Every `-ubuntu-` suffixed leaf overlay |
| Conformance validation checks (10 lines) | 10+ files | Adding a check requires editing every leaf overlay |
| Deployment validation block | 5 files | Identical block repeated |
| Kubeflow-trainer component (9 lines) | 4 files | Identical except constraints |
| Dynamo components (15 lines base) | 5 files | Shared base, service-specific storageClass |

### Root cause

The single-parent `spec.base` model prevents sharing across orthogonal
dimensions. A leaf overlay can't inherit from both `h100-eks-training`
and `os-ubuntu` simultaneously. This forces Ubuntu constraints, platform
components, and validation blocks to be inlined in every leaf that needs them.

## Prototype Findings (2026-04-06)

A full prototype implementation validated both the intermediate/reparenting
approach and the mixin approach. Key findings:

### Intermediates + reparenting: not viable without resolver changes

The original Phase 1 proposed inserting `{accelerator}-{service}` intermediate
overlays and reparenting intent overlays to inherit from them. Prototype
revealed:

1. **Reparenting breaks under all-match semantics.** When `h100-eks-training`
   is reparented from `base: eks-training` to `base: h100-eks`, the old parent
   `eks-training` still matches the query independently. Its content (K8s >=
   1.30, valuesFile) competes with the intermediate's content at the same
   specificity level, producing non-deterministic results.

2. **Content can't be shared as cleanly as assumed.** `cdi: enabled` and
   `gdrcopy: enabled` are training-only on EKS/GB200/AKS (inference doesn't
   set them). `kernel >= 6.8` can't be lifted without changing semantics for
   non-Ubuntu queries.

3. **Net line count increases.** Intent overlays must absorb content from their
   former parents (gpu-operator valuesFile, kgateway components), adding ~251
   lines while removing ~117 — a net increase of ~134 lines.

### Maximal leaf candidate selection: prerequisite for composition

Implementing candidate selection (filter `FindMatchingOverlays` to maximal
leaves only) produces zero semantic change to all existing overlay
combinations while making the resolver behavior predictable. This is a
prerequisite for both intermediates and mixins — without it, any composition
mechanism that introduces overlays at the same specificity level will hit
non-deterministic merge ordering.

### Mixins: validated, modest line savings, significant maintenance wins

The mixin prototype (3 mixins: `os-ubuntu`, `platform-kubeflow`,
`platform-dynamo`) produces identical hydrated output for all converted
overlays. Quantified impact:

| Mixin | Files affected | Lines removed | Mixin cost | Net savings |
|-------|---------------|--------------|------------|-------------|
| `os-ubuntu` | 9 | 54 | 26 | 28 |
| `platform-kubeflow` | 4 | 36 | 29 | 7 |
| `platform-dynamo` | 5 | 75 | 30 | 45 |
| **Total** | **18** | **165** | **85** | **80** |

**Net line reduction: ~80 lines (4% of overlay content).** The primary value
is not line count but:

- **Maintenance cost:** Ubuntu version bump is 1 file instead of 9; kubeflow
  chart upgrade is 1 file instead of 4
- **Drift resistance:** single source of truth for OS constraints and platform
  components — no risk of updating 8 of 9 files and missing one
- **Review burden:** PRs that bump chart versions or change OS constraints
  touch 1 file, reducing merge conflict surface
- **Extensibility:** new OS variant (RHEL) or platform (Ray) is one mixin
  file; new leaf overlays reference it via `mixins: [os-rhel]`

## Options Summary

| Option | Approach | Net Line Savings | Code Change | Status |
|--------|----------|-----------------|-------------|--------|
| **Mixins** | OS/platform mixins composed via `spec.mixins` | ~80 lines | ~140 lines | Recommended (Phase 3) |
| **Intermediates + Reparenting** | Insert `{accel}-{service}` intermediates, reparent intent overlays | Net negative (+134 lines) | YAML-only after Phase 2 prerequisite | Deferred — only viable after candidate selection, only justified by future accelerator growth |
| **Deep Mixins** | Validation mixins via deep-merge | ~67 additional lines | ~70 lines (merge upgrade) | Deferred — requires deep-merge semantics proven safe |
| **Flat Mixins** | Flat mixin-only composition, no inheritance | ~95% dedup | Moderate | Only if inheritance model abandoned |
| **Auto-Compose** | Dimension-driven auto-composition | ~95% dedup | Large | Only if resolver model replaced |

## Decision

**Proposed:** Fix resolver correctness first, then add mixins for
maintenance-critical shared fragments.

The justification is **maintenance cost and drift resistance**, not line-count
reduction. Mixins are only worth the abstraction cost when the shared fragments
are truly orthogonal and reused enough to justify the indirection.

## Implementation Plan

### Phase 1: Correctness fix + structure cleanup

1. Fix `Specificity()` to treat `""` as equivalent to `"any"`, consistent
   with `Matches()` and `MatchesCriteriaField()`. Add regression test
   covering YAML-parsed zero-value criteria (not just explicit `Criteria*Any`
   values).
2. Move validation blocks from `-ubuntu-training` leaf overlays to
   `{accel}-{service}-training` intent overlays. Validation checks are not
   OS-specific — this is a structure/ownership cleanup that eliminates ~67
   redundant lines.

**Exit criteria:**
- `make test` passes with `-race`
- Specificity regression test covers YAML-parsed `""` fields
- Golden-file diffs (via `aicr query --selector . --format yaml`) confirm
  identical hydrated output for all leaf overlays discovered from
  `recipes/overlays/`
- No `-ubuntu-training` leaf overlay contains validation blocks that duplicate
  its intent parent

### Phase 2: Candidate selection refinement

1. Implement maximal leaf candidate selection in `FindMatchingOverlays`:
   after collecting all matches, filter out any overlay that is an ancestor
   (via `spec.base` chain) of another match.
2. Implement as a shared helper used by both
   [`BuildRecipeResult`](https://github.com/NVIDIA/aicr/blob/main/pkg/recipe/metadata_store.go#L313)
   and
   [`BuildRecipeResultWithEvaluator`](https://github.com/NVIDIA/aicr/blob/main/pkg/recipe/metadata_store.go#L352).
   Both entry points must use the same candidate selection semantics.

**Exit criteria:**
- `make test` passes with `-race`
- Characterization/golden tests verify identical hydrated output for all
  leaf overlays discovered from `recipes/overlays/` through both build paths
- `appliedOverlays` lists may shrink (ancestors no longer listed as
  independent candidates) but recipe content is unchanged

### Phase 3: Mixins (OS + platform)

1. Define `RecipeMixin` kind and loader:
   - Distinct `kind: RecipeMixin` schema; loaded from `recipes/mixins/`,
     excluded from overlay discovery
   - Allowed fields: `constraints` and `componentRefs` only (no `criteria`,
     `base`, `mixins`, or `validation`)
2. Add `Mixins []string` field to `RecipeMetadataSpec`. Accumulate during
   `Merge()`. Strip from materialized result before output.
3. Add `mergeMixins()` helper called from both `BuildRecipeResult` and
   `BuildRecipeResultWithEvaluator` after `mergeOverlayChains` returns.
4. Update `BuildRecipeResultWithEvaluator` to evaluate constraints on the
   **fully composed candidate** (inheritance chain + mixins), not per-overlay
   before merge. The current evaluator runs before merge and would not see
   mixin-contributed constraints (e.g., `os-ubuntu`'s `OS.release.ID`).
   Without this change, mixin constraints appear in hydrated output but are
   invisible to constraint evaluation — the evaluator would never exclude a
   candidate based on a failing mixin constraint.
5. Extract 3 mixin files:
   - `os-ubuntu.yaml` — Ubuntu 24.04 release constraints (3 constraints)
   - `platform-kubeflow.yaml` — kubeflow-trainer component
   - `platform-dynamo.yaml` — dynamo-crds, dynamo-platform base components
6. Migrate leaf overlays to use `spec.mixins` for OS and platform content.
7. Enforce mixin conflict policy:
   - **Constraint names:** no duplicates between a mixin and the inheritance
     chain, or between mixins composed into the same leaf. Constraints don't
     compose, so a name collision is unambiguously a conflict.
   - **Component names:** name collisions are allowed only when the mixin
     entry sets nothing beyond the additive set
     `{Namespace, ManifestFiles, PreManifestFiles}`. Identity / sourcing
     fields (`Chart`, `Type`, `Source`, `Version`, `Tag`, `Path`,
     `ValuesFile`, `Overrides`, `Patches`, `DependencyRefs`, `Cleanup`,
     `ExpectedResources`, `HealthCheckAsserts`) still produce a hard error
     — those are exactly the fields the original "Silent constraint override"
     risk row (see Risk Table) was protecting. The carve-out keeps that
     mitigation intact while letting OS-conditional mixins (e.g.
     `os-talos`) contribute namespace and pre/post manifest overrides to
     components already declared upstream without forcing every Talos leaf
     overlay to re-author those fields by hand.
   Implemented as loader-time validation in `mergeMixins()` plus a field-set
   helper `mixinComponentRefSafeForMerge()` that returns the first offending
   identity field so error messages are precise.

**Exit criteria:**
- `make test` passes with `-race`
- `golangci-lint` passes on changed packages
- Golden-file diffs confirm identical hydrated output for all leaf overlays
  discovered from `recipes/overlays/`
- No leaf overlay contains Ubuntu constraints or platform components that
  exist in a mixin
- `spec.mixins` does not appear in materialized recipe output
- Constraint evaluation in `BuildRecipeResultWithEvaluator` runs on the
  fully composed candidate (including mixin constraints), not per-overlay
  before composition
- Mixin conflict policy is enforced via loader-time validation:
  - Duplicate constraint names between a mixin and the inheritance chain
    or between mixins in the same leaf produce a hard error.
  - Duplicate component names are allowed when the mixin's entry sets only
    fields in the additive set `{Namespace, ManifestFiles, PreManifestFiles}`;
    setting any identity/sourcing field on a colliding name still errors.

### Deferred A: Intermediates + Reparenting

Insert `{accelerator}-{service}` intermediate overlays and reparent intent
overlays to share GPU config across training/inference. **Only viable after
Phase 2** (candidate selection) and **only justified when future accelerator
growth makes the file-count reduction worth the +4 intermediate files.**

Prototype showed net +134 lines for current accelerator set. Reassess when
B200 or GB300 overlays are needed.

### Deferred B: Deep-merge Validation Mixins

Extract validation blocks into mixins after upgrading
`RecipeMetadataSpec.Merge()` to support deep-merge for validation phases
(additive `checks` lists, constraint-level override). **Only attempted after
Phase 3 mixin behavior is proven stable** and deep-merge semantics are
verified safe via regression tests.

### Risk Table

| Risk | Impact | Mitigation | Phase |
|------|--------|------------|-------|
| Specificity fix changes overlay merge order | Silent recipe regression | Golden-file tests for all leaf overlays; regression test for zero-value criteria | 1 |
| Candidate selection changes recipe output | Unexpected constraint or component changes | Characterization tests through both build paths | 2 |
| Mixin loaded as normal overlay by resolver | Double-application of constraints/components | Distinct `kind: RecipeMixin` schema; loader excludes `recipes/mixins/` | 3 |
| Mixin-vs-inheritance constraint conflict | Silent constraint override | Loader-time validation in `mergeMixins()`: constraint name collisions always error; component name collisions error only when the mixin sets identity/sourcing fields (additive-only fields are explicitly allowed for OS-conditional namespace + pre/post manifest overrides) | 3 |
| Constraint evaluator misses mixin constraints | Mixin OS/platform constraints not validated against snapshot | Move constraint evaluation to run on fully composed candidate (post-merge) | 3 |
| `spec.mixins` leaks into recipe output | Downstream consumer confusion | Strip `Mixins` field after merge, before materialization | 3 |

### Rollback Strategy

**Phase 1** is a bug fix + YAML restructure — revert by restoring
`Specificity()` and moving validation blocks back to ubuntu leaves.

**Phase 2** is a resolver refinement — revert by removing the ancestor filter
in `FindMatchingOverlays`. Existing overlays produce identical hydrated recipe
content under both all-match and leaf-candidate semantics because the current
tree is linear. `appliedOverlays` metadata may differ (ancestors no longer
listed as independent candidates), but constraints, components, validation,
and deployment order are unchanged.

**Phase 3** is backward compatible: remove `spec.mixins` from leaf overlays
and inline the mixin content back into each leaf. The `RecipeMixin` loader
can remain dormant (no mixins referenced = no code path exercised). No recipe
output format changes, so downstream consumers (bundler, validator) are
unaffected.

## Consequences

### Positive

- Resolver correctness: `Specificity()` and candidate selection fixes
  eliminate non-deterministic merge ordering that was masked by linear chains
- Single source of truth for OS constraints and platform components —
  version bumps and additions are 1-file changes
- Reduced review burden and merge conflict surface for chart/constraint PRs
- New OS variant or platform is one mixin file; new leaf overlays reference
  it instead of copy-pasting

### Negative

- Two composition mechanisms to understand (inheritance + mixins)
- Mixin conflict policy must be documented and enforced
- Abstraction cost: mixin indirection hides content behind a reference;
  only justified when fragments are truly orthogonal and reused enough

### Neutral

- Total file count stays roughly the same (~43 overlays + 3 mixins) but
  per-file content shrinks for leaves that use mixins
- KWOK test count unchanged (still one test per leaf overlay)
- Net line reduction is modest (~80 lines from mixins + ~67 from validation
  lift-up); the value is maintenance cost, not line count

## References

- [Issue #305: Refactor overlay system to reduce training/inference redundancy](https://github.com/NVIDIA/aicr/issues/305)
- [ADR-003: Scaling KWOK Recipe Tests](003-scaling-recipe-tests.md)
- `RecipeMetadataSpec.Merge()` in `pkg/recipe/metadata.go` — component/constraint merge semantics
- `BuildRecipeResultWithEvaluator()` in `pkg/recipe/metadata_store.go` — overlay selection, constraint evaluation, and merge logic
- `Specificity()` in `pkg/recipe/criteria.go` — overlay sort ordering
- `FindMatchingOverlays()` in `pkg/recipe/metadata_store.go` — candidate selection
