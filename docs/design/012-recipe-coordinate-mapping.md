# ADR-012: Recipe → Board-Coordinate Mapping

## Status

Accepted. `pkg/recipe.CoordinateFor` ships the ratified mapping; consumers may
import it as a stable contract.

## Problem

Multiple workstreams need to answer the same question — *where does this recipe
live on the validation board?* — and they must all answer it identically. The
TestGrid track (#1266–#1273) emits per-recipe build results into a board; RQ1
(#1283) deep-links from the recipe-health Evidence column into that board; GP4
(#1404) and GP5 (#1405) expose and render the board. If each of these computed
its own service/accelerator/intent placement, the four surfaces would drift the
moment any one of them changed a delimiter, a wildcard rule, or a segment order.

Two failure modes follow from an unowned mapping:

- **Independent forks diverge.** The recipe `metadata.name` is
  accelerator-first (`h100-eks-ubuntu-training`); the board coordinate is
  service-first (`eks/h100-ubuntu/training`). Anyone who derives the coordinate
  by string-munging the name will eventually produce a different layout than a
  peer who derives it from criteria.
- **Ambiguous placement.** Without a fail-closed contract, a recipe missing a
  required dimension (or carrying an `any` wildcard) silently maps to a
  plausible-looking but wrong cell, fusing unrelated results.

This ADR fixes the mapping as a single, pure Go function —
`pkg/recipe.CoordinateFor` — and pins the surrounding taxonomy, URL form, and
column-metadata schema that its consumers share.

## Scope

**In scope:**

- The canonical recipe → coordinate mapping (`pkg/recipe.CoordinateFor`, shipped
  in `pkg/recipe/coordinate.go`).
- The board taxonomy (group / dashboard / tab / row / column).
- The stable coordinate URL form.
- The pinned column-metadata key schema (`started.json` keys).
- Reconciliation with ADR-009 (recipe-health) — how the two surfaces coexist.
- The #1224 cross-link contract that recipe-health consumes.

**Out of scope:**

- The consumers themselves — TG2 (#1267), TG3, TG4a, RQ1 (#1283), GP4 (#1404).
  They *import* this mapping; their internals are owned by their own issues.
- The API and UI that render the board (GP5).
- The live board / hosting / navigation host.

The user-facing TestGrid page ([`docs/user/testgrid.md`](../user/testgrid.md))
presents this contract for end users; this ADR remains the internal source of
truth that the page and the dependent workstreams derive from.

## Taxonomy

The board is a five-level addressing space. `CoordinateFor` produces only the
first three levels — the **group, dashboard, and tab** — from resolved recipe
criteria. The **row and column come from the CTRF test data**, not the recipe,
and are out of this function's hands entirely.

| Level | Value | Source |
|---|---|---|
| Group | `service` | recipe criteria (`CoordinateFor`) |
| Dashboard | `<accelerator>-<os>` | recipe criteria (`CoordinateFor`) |
| Tab | `intent`, or `<intent>-<platform>` | recipe criteria (`CoordinateFor`) |
| Row | `<phase>/<check>` | CTRF test data (NOT the recipe) |
| Column | one build | CTRF test data (NOT the recipe) |

Stating this split explicitly matters: a recipe defines *which board cell* a
result belongs in (group/dashboard/tab); the validation run defines *what is in
that cell* (which checks ran, in which build). `CoordinateFor` therefore returns
a three-field `Coordinate{Group, Dashboard, Tab}` and nothing about rows or
columns.

## Mapping rules

The input is **resolved** `Criteria`. The function consumes already-resolved
criteria and **never parses `metadata.name`**. This is deliberate: the recipe
name is accelerator-first and the coordinate is service-first, and the two must
stay *independently* correct — coupling them through string parsing would make a
rename of either silently corrupt the other.

- **Required, concrete dimensions:** `service`, `accelerator`, `os`, `intent`.
  Each must be a concrete value. A `nil` Criteria, an empty value, or the `any`
  wildcard (`CriteriaAnyValue`, the string `"any"`) **fails closed** with
  `ErrCodeInvalidRequest`, naming the offending dimension. A recipe that fails
  this check is *not coordinatable*; callers skip it (it has no board cell).
- **Optional sub-segment — platform only:** `platform` is the single optional
  segment. An empty or `any` platform yields a **bare intent tab** (`training`);
  a concrete platform yields `<intent>-<platform>` (`training-kubeflow`). An
  optional sub-segment is **dropped, never `"unknown"`-substituted** — there is
  no placeholder token in the coordinate.
- **Purity:** the function is pure — no clock, no maps, no registry, no I/O. The
  same Criteria always yields the same Coordinate.
- **Join key:** the value by which recipes are *addressed* across systems is the
  overlay `metadata.name`. The coordinate is the *placement*; the name is the
  *identity*. (`CoordinateFor` does not read the name — addressing happens at the
  caller, which already holds both the name and the resolved criteria.)

This matches the shipped `Coordinate` struct: `Group = service`,
`Dashboard = accelerator + "-" + os`, `Tab = intent` (or `intent-platform`).

## Why Kubernetes version lives in the column

The Kubernetes version is **not** part of the coordinate. It lives **in the
column** (one build = one k8s version). Three facets of this decision must be
held together:

1. **Benefit — stable, addressable links.** Because k8s is a property of the
   *build*, the coordinate (`eks/h100-ubuntu/training`) is invariant across k8s
   versions. RQ1's deep-links and TG4a's exposed paths stay stable as clusters
   upgrade; the link target does not churn every time a new K8s minor ships.
2. **Cost — silent fusion.** The flip side: two columns for the *same*
   coordinate but *different* k8s versions sit in the same tab. A stale-version
   result could be read alongside a current-version result and silently fuse in
   a reader's mind into one "this recipe's posture" signal, even though one of
   them is no longer relevant.
3. **Countermeasure.** k8s version is exposed as a **UI facet / filter** (so a
   reader can scope to a version), **plus** a **latest-per-signer default
   scope** (so the default view does not mix stale and current results). Both
   the facet and the default scope are required to keep the benefit without
   paying the fusion cost.

## Stable URL form

The canonical coordinate string is:

```text
<group>/<dashboard>/<tab>
```

For example: `eks/h100-ubuntu/training-kubeflow` (with platform) or
`eks/h100-ubuntu/training` (bare intent). This is exactly what
`Coordinate.Path()` (and `String()`) returns.

**`Path()` is a stable *opaque identity*, not a decomposable key.** Dimension
values may themselves contain the `-` join character (`rtx-pro-6000`; a future
`--data` intent such as `fine-tuning`), so `eks/rtx-pro-6000-ubuntu/training`
has no positional split point — a consumer cannot recover `(rtx-pro-6000,
ubuntu)` from the string alone. Consumers therefore treat `Path()` as a value
that is stable and equal-comparable (deep-links, presence checks) and address
the dimensions via the `Coordinate` struct fields, **never** by string-splitting
the path. A decomposable key would require an explicit `ParseCoordinate`; that
is deferred to the first parsing consumer (RQ1 #1283 / TG4a #1284), which would
also pin a "values must not contain the active delimiter" invariant.

The one delimiter the mapping *does* enforce is `/`: `CoordinateFor` fails closed
(`ErrCodeInvalidRequest`) on any dimension value containing `/`, because the path
separator is the single character whose presence would change the segment count
and silently mis-place the recipe — the exact ambiguous-placement failure this
ADR exists to prevent.

This string is the canonical coordinate that RQ1 deep-links to and that TG4a / GP
expose. **The host and the navigation scheme (path vs. fragment) are owned by the
consumer** (GP5 / TG4a), not by this mapping. `pkg/recipe` emits only the
canonical segments; it makes no assumption about scheme, host, or trailing
decoration.

## Pinned column-metadata key schema

Each build column carries a fixed set of metadata keys in its `started.json`.
This schema is **specified here and enforced by TG2 (#1267)** — the emitter — and
referenced by **TG3's `Header.extra`**; no Go constant or struct pins these names
today, so TG2 must lock the key names, order, and count (e.g. in a shared
constant + a round-trip test) when it lands, rather than relying on this prose.
The contract is **7 keys**, in this order. The strings below are the canonical
`snake_case` key names; consumers use them verbatim.

| # | Key | Source | Example | Notes |
|---|---|---|---|---|
| 1 | `aicr_version` | binary that ran the validation | `v0.42.0` | The `aicr` release that produced the result. |
| 2 | `k8s_version` | observed cluster | `1.33` | Observed cluster version, `major.minor` only — drives the k8s-in-column facet. |
| 3 | `k8s_constraint` | recipe | `>= 1.32.4` | The recipe-declared `K8s.server.version` constraint, for at-a-glance "did the observed version satisfy intent?". |
| 4 | `signer_identity` | attestation | `https://github.com/NVIDIA/aicr/.github/workflows/...` | Signer identity (the SAN / subject of the signing identity). |
| 5 | `signer_issuer` | attestation | `https://token.actions.githubusercontent.com` | Signer OIDC issuer — pairs with identity to scope the latest-per-signer default. |
| 6 | `source_class` | provenance | `ci` | Source class (e.g. `ci` vs. ad-hoc / local), so a reader can weigh trust. |
| 7 | `evidence_digest` | evidence | `sha256:…` | Digest of the underlying evidence artifact — the verifiable anchor for the result. |

Notes:

- `k8s_version` is **observed** (what the cluster actually reported);
  `k8s_constraint` is **declared** (what the recipe asked for). Keeping both
  distinct is what lets the board show satisfied-vs-violated without re-resolving
  the recipe.
- `signer_identity` + `signer_issuer` together key the **latest-per-signer**
  default scope referenced in the k8s-in-column countermeasure.
- The key strings and their count (7) are the contract: TG2 emits exactly these;
  TG3 reads exactly these. Adding a key is a change to this table.

## Reconciliation with ADR-009 — coexist, not identity

This board and the recipe-health surface in
[ADR-009](009-recipe-health-tracking.md) are **two surfaces that coexist; they do
not duplicate each other**, and neither is the other's source.

- **recipe-health (ADR-009, [`docs/user/recipe-health.md`](../user/recipe-health.md))**
  owns the **offline structural / freshness** surface — computed without a live
  cluster, from the recipe catalog itself.
- **TestGrid / this coordinate board** owns the **live validation-posture**
  surface — derived from actual validation runs against real clusters.

ADR-009 fixed this split deliberately under
[its two-axis principle](009-recipe-health-tracking.md#3-the-two-axis-principle-documented-now-validation-axis-deferred):
structural soundness and validation posture "must never collapse into a single
fused score." This coordinate board *is* the home of the deferred
validation-posture axis — it is a *separate* surface, not a richer rendering of
the structural matrix.

The two surfaces share exactly one thing: the **`metadata.name` join key**. Both
enumerate recipes by overlay name — recipe-health (`pkg/health`) via
`MetadataStore.ListCatalog` filtered to leaf overlays (`CatalogEntry.IsLeaf`),
which yields the resolved criteria + leaf name per maximal-leaf overlay, the
[leaf-driven (not cartesian) enumeration](009-recipe-health-tracking.md#enumeration-is-leaf-driven-not-cartesian)
ADR-009 specifies. The coordinate board addresses the same recipes by the same
name, and the coordinate itself is derived from the *resolved criteria* of that
same leaf — so the two surfaces line up on identity without sharing computation.

## GP ⇄ TG — interim and live, sharing one foundation

GP (#1400) is the interim, static GitHub Pages surface for this evidence; TG
(#1263) is the live stack — upstream TestGrid workers, an AICR-native API, a
greenfield UI, and an always-on GKE host cluster. Both are built in parallel;
this ADR's mapping function, taxonomy, and column-metadata schema are shared,
not duplicated, by both.

- **Same foundation.** GP and TG read the same GCS bucket, the same verified,
  source-keyed evidence tree, and the same `pkg/recipe.CoordinateFor` mapping
  (§Mapping rules above). Neither forks the taxonomy.
- **Forward-compatible, not throwaway.** GP4's (#1404) coordinate-keyed JSON
  contract (`index.json` + `series/*.json`) is a forward-compatible input to
  TG's workers/API/UI — a future migration consumes GP's already-coordinate-keyed
  data rather than re-deriving it.
- **No deferral.** GP shipping first does not defer any TG child — TG1–TG7
  remain Ready / in progress. Build-both-vs-defer is a separate strategic
  decision, not implied by sequencing, and is not made by this ADR.

One concrete overlap is intentionally left for GP3 (#1403) and TG1 (#1266) to
deconflict directly, outside this ADR: whether the write-only publish SA and
bucket are shared between the two surfaces or stood up twice on purpose.

Operator-facing detail for each surface lives in
[docs/user/testgrid.md](../user/testgrid.md) (TG) and
[docs/user/evidence-dashboard.md](../user/evidence-dashboard.md) (GP).

## #1224 cross-link contract

recipe-health's **Evidence** column will **link** into this coordinate URL — it
will **never duplicate** the board's content. The interface is:

- recipe-health computes a recipe's `metadata.name` and its resolved criteria,
  calls `CoordinateFor`, and emits `Coordinate.Path()` as a link target (the deep
  link is RQ1 / #1283).
- The link is **bot-verifiable** via TG4a's coordinate-presence endpoint (RQ2 /
  #1284): an automated check can confirm the linked coordinate actually exists on
  the board, so the Evidence column never links into a void.

**Status of the recipe-health side:** the structural matrix has **shipped** —
`pkg/health` computes it and [`docs/user/recipe-health.md`](../user/recipe-health.md)
publishes it. Its **Evidence** column is a literal `pending` for every recipe
today: no coordinate deep-link exists yet. This contract is precisely the
*interface* that column will adopt — when the evidence-derived Evidence column
lands (RQ1 / #1283), it consumes `CoordinateFor` + `Coordinate.Path()` exactly as
described here.

## Consumers

The following workstreams **import `pkg/recipe.CoordinateFor`** and must never
fork or reimplement the mapping:

- **TG2 (#1267)** — emits the pinned column metadata; uses the coordinate to
  place build results.
- **TG3** — reads the column metadata via `Header.extra`; uses the coordinate for
  tab/dashboard placement.
- **TG4a** — exposes the coordinate URL form and the coordinate-presence endpoint
  (#1284).
- **RQ1 (#1283)** — deep-links into the coordinate URL from recipe-health.
- **GP4 (#1404)** — exposes the board built on these coordinates.

Single-sourcing the mapping in `pkg/recipe.CoordinateFor` is the **anti-drift
guarantee**: every surface derives the same placement from the same resolved
criteria, so a change to the taxonomy is a one-line change in one function, not a
hunt across five repositories of consumers.
