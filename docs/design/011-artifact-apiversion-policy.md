# ADR-011: Artifact apiVersion Policy and Compatibility Gate

> **Amended by [ADR-015](015-recipe-configuration-profiles.md).** ADR-015
> introduces kind-scoped version evolution: `RecipeMetadata` and
> `RecipeResult` accept `aicr.run/v1alpha3` for profile-bearing artifacts
> (dual-accepted alongside the legacy version), while snapshots, configs,
> and `RecipeCriteria` stay on `aicr.run/v1alpha2`. The single-version
> posture below is otherwise unchanged.

> **Amended by [ADR-013](013-aicr-run-domain-migration.md).** This ADR
> established the single-sourced, enforced `apiVersion` contract while the
> domain was `aicr.nvidia.com` and the version was `v1alpha1`. ADR-013 later
> migrated the domain to `aicr.run` and bumped the version to `v1alpha2`
> (build spec `v1beta2`) as a **hard break** — the legacy value is rejected,
> not dual-accepted. The current canonical value is `header.GroupVersion` =
> `aicr.run/v1alpha2`. The general evolution policy and compatibility gate
> below remain in force; only the concrete domain/version values changed.

## Problem

`aicr validate` consumes three artifacts that may have been produced by
different `aicr` versions: the snapshot, the recipe, and the running binary.
Until now nothing enforced a compatibility contract between them. Two failure
modes followed:

- **Silent schema drift.** A recipe or snapshot produced by an incompatible
  `aicr` version could be loaded structurally with no signal, surfacing later
  as a confusing validation failure with no obvious cause.
- **No meaningful version contract.** Every artifact carries an `apiVersion`
  (then `aicr.nvidia.com/v1alpha1`), but the string was:
  - **frozen since repo init** — never bumped, with breaking schema changes
    shipped *within* `v1alpha1`;
  - **redefined as ~5 independent string literals** (`pkg/snapshotter`,
    `pkg/recipe` result + criteria, `pkg/config`) that agreed only by
    coincidence, with doc drift (`/v1` vs `/v1alpha1`) already present;
  - **unenforced on read** — snapshot loading ignored it entirely; recipe
    loading checked only `Kind`.

So an `apiVersion` match guaranteed nothing and a mismatch never occurred. This
ADR makes the `apiVersion` a real, single-sourced, enforced contract.

A predecessor change (#1386) added a *soft, advisory* warning comparing the
embedded **binary version** strings. That remains useful as a debugging
breadcrumb but is unsigned and noisy across dev builds; it is not a contract.
This ADR establishes the durable, schema-level gate.

## Non-Goals

- **Bumping the version segment now.** *(At the time of this ADR.)* No breaking
  schema change was being made, so the version stayed `v1alpha1`; bumping with
  no schema change would have orphaned every existing artifact for zero benefit.
  This non-goal was later overridden by [ADR-013](013-aicr-run-domain-migration.md):
  the `aicr.run` domain rename is itself a breaking change, so it carried a
  signal-only bump to `v1alpha2` to make the cut explicit.
- **Gating non-artifact `apiVersion`s.** The validator catalog
  (`validator.nvidia.com/...`), provenance predicate, and Zarf/Hauler mirror
  formats are separate schemas with their own domains/versions and are out of
  scope.
- **Signing or authenticating the artifact header.** The embedded `apiVersion`
  (like the snapshot fingerprint) is unsigned, advisory metadata. This gate
  catches accidental version skew, not a motivated attacker editing the file.
- **A `--skip-version-check` escape hatch.** The gate fails closed with no
  bypass; the well-formed path is to regenerate the artifact with a matching
  `aicr` version.

## Decision

### 1. Single source of truth

`pkg/header` is the canonical home for the artifact group/version:

- `header.Domain` = `aicr.run` (single source of truth; see ADR-013)
- `header.APIGroup` = `header.Domain` = `aicr.run`
- `header.APIVersionV1Alpha2` = `v1alpha2`
- `header.GroupVersion` = `aicr.run/v1alpha2`

All package-local constants alias `header.GroupVersion` rather than redeclaring
the literal: `snapshotter.FullAPIVersion`, `recipe.RecipeAPIVersion`,
`recipe.RecipeCriteriaAPIVersion`, and `config.APIVersion`.

### 2. Evolution rule

- Schema changes within a version MUST be **additive-only** (new optional
  fields). Existing artifacts must continue to deserialize. This matches the
  current backward-compat strategy already covered by
  `pkg/serializer/reader_recipe_compat_test.go`.
- A **breaking** change (removing/renaming a field, changing semantics, or a
  required-field addition) requires a **new version segment**
  (e.g. `v1alpha2`).

### 3. Compatibility gate (accept-known / reject-unknown)

`header.IsSupportedAPIVersion(v)` reports whether a non-empty `apiVersion` is
one this binary understands. The snapshot and recipe loaders apply it:

- **Empty `apiVersion`** → accepted (older artifacts predate the field; matches
  the existing empty-`Kind` tolerance).
- **Known `apiVersion`** → accepted.
- **Non-empty unknown `apiVersion`** → rejected with
  `ErrCodeInvalidRequest` and a message naming the value, the expected value,
  and the remediation (regenerate/recapture with a matching `aicr` version).

The gate lives in the shared loaders — `recipe.LoadFromFileWithProvider` (used
by both CLI and server via `pkg/client/v1`) and the new
`snapshotter.LoadFromFile` / `LoadFromFileWithKubeconfig`, which `validate`,
`query`, and `diff` route through — so enforcement is uniform across entry
points. This mirrors the strict reject already in `pkg/config` (`Validate`) and
`pkg/recipe` criteria parsing.

### 4. Transition window on a future bump

When the schema is bumped (e.g. to `v1alpha2`), add the new value to
`header.IsSupportedAPIVersion` **while keeping the old one**, so a transition
window accepts both. Retire the old value only after the deprecation window
closes. Add a regression test asserting an out-of-window version is rejected.

**Exception — the `aicr.run` domain migration (ADR-013) was a hard break.**
Because it was a pre-v1, simultaneous domain-and-version change with all
fixtures regenerated in lockstep, the dual-accept window this policy normally
allows was intentionally *not* used: the legacy `aicr.nvidia.com/v1alpha1` value
is rejected outright rather than accepted alongside `aicr.run/v1alpha2`. See
ADR-013 for the rationale.

## Consequences

- An artifact stamped with an unsupported `apiVersion` now fails fast at load
  with a clear, actionable error instead of failing obscurely downstream.
- The `apiVersion` literal exists exactly once; future bumps are a one-line
  change plus a transition-window entry.
- Current artifacts are `aicr.run/v1alpha2` (accepted) or carry an empty
  `apiVersion` (tolerated). Per ADR-013's hard break, legacy
  `aicr.nvidia.com/v1alpha1` artifacts are rejected and must be regenerated.
- The gate is intentionally not a security control; the unsigned header can
  still be edited. Authenticated provenance remains the supply-chain workstream.
