{/*
Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/}

# Evidence Corroboration Dashboard

The **evidence corroboration dashboard** is a static, public site published to
[`https://validation.aicr.run`](https://validation.aicr.run). It visualizes
corroboration across heterogeneous, cryptographically signed sources for each
AICR recipe and answers: *"how many independent parties have run this recipe
and do their results agree?"*

The site is rebuilt and deployed on every merge to `main`. Its generator
(`tools/corroborate`) is byte-deterministic — the same verified evidence
inputs always produce identical JSON and HTML — so every publish is a straight
deploy with no drift-PR, and a non-reproducible build fails loudly before it
ships. The full pipeline is described in
[Evidence Ingest](../contributor/evidence-ingest.md) (GP2) and
[Evidence Dashboard Publish](../contributor/evidence-dashboard-publish.md)
(GP5).

This is the **interim** evidence surface. The long-running live complement is
the [AICR TestGrid](./testgrid.md), which adds live workers, an AICR-native
API, a greenfield UI, and an always-on GKE cluster; that epic is built in
parallel and is not a replacement or deferral of either surface.

## How to read it — CSP-first navigation

The dashboard shares its first four CSP-first addressing levels with the
[TestGrid](./testgrid.md); the fifth column is organized per-signer here (see
[Consensus model](#consensus-model)) rather than per-build:

| Level | Value | Example |
|-------|-------|---------|
| **Group** | service (the CSP) | `eks` |
| **Dashboard** | accelerator + OS | `h100-ubuntu` |
| **Tab** | intent, optionally with platform | `training-kubeflow` |
| **Row** | validation `<phase>/<check>` | `conformance/gpu-operator-ready` |
| **Source column** | one signer | one allowlisted party (or a verified-but-unknown reported signer) |

The overview defaults to service cards. Switch **Overview layout** to
**all recipes** to scan every matching recipe in one page-scrolling grid. That
grid keeps full recipe names, uses filled status cells for overall and
per-phase state, and keeps its column headers visible while the page scrolls.
As recipe selectors are applied, the grid regroups from services to
accelerator/OS sections and fades the already-selected portions of each recipe
name so the remaining distinctions stand out.

You can navigate through either overview, the collapsible dashboard list, or
the recipe selectors: pick a group → pick a dashboard → pick a tab to reach the
**consensus grid** for that recipe. Group and dashboard headings link to their
intermediate views, so reaching an accelerator/OS dashboard does not require
opening a recipe and navigating back. The dashboard list is expanded by
default and its panel-level control collapses or restores the full tree.

In a recipe's consensus grid, the consensus column and every source result
fill their cells with the corresponding state color for fast scanning. Phase
headers can be collapsed, column headers stay visible while the grid scrolls,
and source headers include a **history** affordance. Selecting a source header
goes directly to that signer's build-by-build history for the recipe. Selecting
an individual result cell opens that run's evidence details; from there,
**open this source's run history** opens the same history focused on that test.
Each build's details link to its signed evidence artifact.

The control strip separates recipe selection from evidence filtering. The
service, accelerator, OS, intent, and platform selectors choose or narrow a
recipe destination. The `aicr`-version and Kubernetes filters scope the
evidence within that destination. The default is the **all-versions** view:
each signer's single most recent run is folded into one consensus grid,
version-blind. Selecting a specific `aicr` version switches to that version's
**strict same-version consensus** — only runs from the same release corroborate
one another (cross-version agreement is not reproduction), scoped
latest-per-signer at that version. In both views a signer's older builds never
dilute its latest, and the strict view additionally refuses to let a different
`aicr` release corroborate a current one. (Kubernetes minor is not a consensus
dimension: its evidence filter only dims mismatched columns for display — see
[Facets](#facets) — it never removes them from consensus.)

On narrow screens the dashboard list becomes an off-canvas drawer, selectors
use touch-sized controls, and wide evidence grids scroll horizontally without
shrinking their cells. Opening cell details overlays the grid instead of
resizing it.

### The coordinate and stable URLs

Every recipe cell has a stable canonical address `<group>/<dashboard>/<tab>`
derived by `pkg/recipe.CoordinateFor` from the recipe's resolved criteria
(see [ADR-012](../design/012-recipe-coordinate-mapping.md)). Kubernetes
version is deliberately **not** part of the coordinate — it is a per-build
column facet — so a link such as
`https://validation.aicr.run/#/eks/h100-ubuntu/training-kubeflow` remains
valid across Kubernetes upgrades and is safe to bookmark or link.

The `#/` is a **client-side hash route**, not a server path: the dashboard is
a single static `index.html` with no server-side routing, so navigation state
lives entirely in the URL fragment. Intermediate destinations are addressable
as `#/<group>` and `#/<group>/<dashboard>`; recipe and source history routes
extend that to `#/<group>/<dashboard>/<tab>[/<signer>]`. The fragment is never
sent to the server, so the same link resolves correctly on any static host —
GitHub Pages included — with no rewrite rules needed. A path-based URL (without
the `#/`) does **not** resolve.

## Consensus model

The dashboard counts **distinct verified signers, never builds**. Ten nightly
runs from the same CI loop are one source, not ten; a single actor cannot
manufacture a strong consensus by re-running.

### States

Each row in a recipe's consensus grid carries one of five states:

| State | Meaning |
|-------|---------|
| `CONFIRMED` | ≥ 2 distinct allowlisted signers ran the row; all passed; none failed. The strongest positive signal. |
| `SINGLE` | Exactly 1 allowlisted signer ran the row and it passed: reported, but not yet independently corroborated. |
| `CONTESTED` | Allowlisted signers disagree — at least 1 passed and at least 1 failed. Surfaced first-class; never averaged or hidden. |
| `FAILING` | Every allowlisted signer that ran the row failed it. |
| `UNTESTED` | No allowlisted signer ran the row. A coverage gap — visually distinct from FAILING. |

`UNTESTED` and `FAILING` are different signals: `UNTESTED` means no allowlisted
party has run the check yet; `FAILING` means every party that did run it
reported a failure. Do not read `UNTESTED` as a passing signal.

`CONTESTED` is displayed prominently and never collapsed into a neutral
average. When at least one allowlisted signer passes and at least one fails,
the grid surfaces the disagreement so a reader can investigate; the only
way a `CONTESTED` row disappears is by all signers converging on one result.

The five states are computed in two grids. The **default all-versions grid**
folds each signer's single most recent run version-blind, so `CONFIRMED` there
means ≥ 2 distinct allowlisted signers passed the row on their latest run —
which the board flags as weaker than same-version reproduction. Picking an
`aicr` version (see [Facets](#facets)) switches to that version's **strict
same-version grid**, where two signers corroborate only if they ran the same
release; the same row can therefore be `CONFIRMED` in the all-versions grid or
at one version and `SINGLE` at another.

### `not-run` is excluded from counting

A signer whose latest in-scope run skipped or left a check pending has a
`not-run` outcome for that row. `not-run` contributes to neither the pass
count nor the fail count, so it can neither promote a row to `CONFIRMED` nor
suppress a `CONTESTED`. A signer that never records a pass or fail — every
in-scope outcome is `not-run` — is absent from both the index grid and the
per-source drilldown; `not-run` cells appear in the drilldown only for signers
that have a pass or fail on some other row or build.

### Phase rollup

Each recipe tab also shows a per-phase rolled-up state (deployment, conformance,
performance). (Readiness is evaluated inline without containers, so it produces
no evidence rows and does not appear on the board.) The rollup is
**worst-first**: the phase state is the
worst-ranked state among all its rows, in this priority order (worst first):

`CONTESTED` → `FAILING` → `UNTESTED` → `SINGLE` → `CONFIRMED`

A single `CONTESTED` row forces the whole phase to `CONTESTED`; a phase with
all-`CONFIRMED` rows rolls up to `CONFIRMED`.

Worst-first has one exception, `PARTIAL`. A phase with a mix of passing
(`CONFIRMED`/`SINGLE`) and not-yet-run (`UNTESTED`) rows — but nothing failing
or contested — would be dragged all the way to `UNTESTED` by the precedence
above, hiding the coverage it does have. That mixed state rolls up to `PARTIAL`
instead: passing where it ran, but not every row has run. Real problems still
win — `CONTESTED` and `FAILING` outrank `UNTESTED`, so a phase only reaches
`PARTIAL` when nothing failed. (`PARTIAL` is a rollup state only; individual
rows never carry it.)

A phase with no rows — because the recipe declares no checks for it, or no
signer has run any of them yet — rolls up to `UNTESTED` and is still shown on
the board as an untested coverage gap rather than omitted, so a category nobody
has run is visible instead of silently missing.

## Source classes

A signer's source class is **derived from its verified OIDC identity** against
the in-tree allowlist (`recipes/evidence/allowlist.yaml`). It is never a
free-text flag that a contributor controls.

| Class | Who | Corroboration weight |
|-------|-----|----------------------|
| `first-party` | NVIDIA UAT CI — pinned OIDC issuer + exact workflow SAN | Full weight |
| `community` | Allowlisted community signers; also the class for verified-but-unknown signers (zero-weight) | Full weight (if allowlisted); zero weight if unknown |
| `partner` | Allowlisted partner signers | Full weight |

First-party AICR UAT runs ingest evidence directly — they do not commit a
per-run pointer to `main` (which would churn the repo nightly). Community and
partner submissions land their pointer via PR, reviewed under `CODEOWNERS`.

A verified signer that is **not** in the allowlist is admitted as a zero-weight
**reported** source column: its header is labeled `not counted` and its results
remain inspectable, but it is never counted toward `CONFIRMED`, `SINGLE`,
`CONTESTED`, or `FAILING`. See [Sybil resistance](#sybil-resistance) below.

## What corroboration proves — and does not prove

**Corroboration proves provenance**: who signed the evidence, what their
verified identity is, and what they asserted in their CTRF report. It does
**not** prove cluster-verified correctness.

Specifically:

- A `CONFIRMED` cell means ≥ 2 distinct allowlisted parties each independently
  ran their own validation and reported a passing result. It does not mean
  NVIDIA re-ran the validation on a second cluster independently — first-party
  NVIDIA UAT runs count as one of those parties.
- The **tab placement** (group, dashboard, tab) is **author-declared** from the
  recipe's resolved criteria. The ingest pipeline verifies the signer and the
  CTRF content, but it cannot independently observe which intent or platform a
  contributor's cluster actually exercised. A community result in the
  `training-kubeflow` tab means the contributor declared their run was a
  training-kubeflow workload and signed that assertion — not that an external
  party verified the cluster independently.
- Community and partner corroboration is therefore **provenance-grade**: it
  adds independent confirmation from a distinct, verifiable party, not a
  second NVIDIA-controlled reproduction.

This trust model is derived from [ADR-007](../design/007-recipe-evidence.md).
The full allowlist posture and signer-class derivation are documented in
[Artifact Verification — Per-Source Pointer Layout and the Signer Allowlist](./artifact-verification.md#per-source-pointer-layout-and-the-signer-allowlist).

## Sybil resistance

The dashboard is designed to fail closed against sybil attacks — where a single
actor tries to manufacture a strong consensus by contributing multiple
technically-distinct but organizationally-equivalent identities.

Two controls work together:

1. **Allowlisted signers only carry corroboration weight.** The allowlist
   (`recipes/evidence/allowlist.yaml`) entries are PR-reviewed under
   `CODEOWNERS`. A new signer can appear as a zero-weight `not counted` source
   column without an allowlist entry; only a maintainer-merged allowlist edit
   promotes it to a weighted source.

2. **Distinct-signer counting on canonical identity.** The dashboard keys the
   signer count on the verified `(issuer, identity)` pair, never the
   contributor-controlled `idHash` in the pointer file. Two pointer files that
   share the same verified identity count as one signer — they cannot combine
   to manufacture `CONFIRMED`.

The allowlist also enforces that entries do not overlap (one verified
identity matches at most one entry) and that regex patterns are not
over-broad (an unbounded wildcard org/repo segment is rejected at load
time). A verified-but-unknown community signer can never promote a row to
`CONFIRMED` by itself: it is a zero-weight reported source column.

## Facets

The control strip has two distinct jobs:

- **Select recipe** — service, accelerator, OS, intent, and platform identify
  the recipe coordinate. Partial selections narrow and regroup the overview or
  service catalog; a complete selection opens the matching recipe directly.
- **Filter evidence** — `aicr` and Kubernetes versions change how the evidence
  for the selected recipes is presented:

  - **aicr version** — the `aicr` release that produced the run. This is a
    whole-dashboard **consensus lens**, not a parallel dimming filter: consensus
    is baked both as a cross-version all-versions grid and per version, and the
    lens selects which one you see. The default (**all versions**) is the
    cross-version grid — each signer's single latest run, version-blind;
    picking a specific version switches every summary (grid, overview, catalog)
    to that version's strict same-version consensus. In the per-source
    drilldown the lens hard-**filters** columns to the selected version. Because
    UAT release cells run several `aicr` releases per coordinate, the lens is
    already meaningful today.
  - **k8s version** (Kubernetes `major.minor`) — the cluster version observed in
    the run. Unlike the version lens, k8s only **dims** non-matching build
    columns (in both the matrix and the per-source drilldown) — it never removes
    them — so an old Kubernetes minor is visually distinguished but never
    silently fused with a current-version result.

The two controls compose but are not symmetric: the `aicr` version lens chooses
*which* consensus grid you are viewing (and filters drilldown columns), while
k8s only dims columns within whatever the lens selected — for example, the
`v0.42.0` consensus grid with its non-`1.33` columns dimmed.

## How it relates to TestGrid and Recipe Health

### Relationship to TestGrid

The evidence corroboration dashboard (GP) and the [AICR TestGrid](./testgrid.md)
(TG) are **siblings that share the same foundation**:

- They read the same verified, source-keyed evidence tree in the same layout.
  (Whether the two surfaces share a single GCS bucket and publish service
  account, or stand up their own, is an open GP3/TG1 deconfliction tracked in
  [ADR-012](../design/012-recipe-coordinate-mapping.md) — the shared contract
  is the evidence tree and its layout, not a specific bucket.)
- They both derive recipe coordinates from the **single shared mapping
  function** `pkg/recipe.CoordinateFor` (see
  [ADR-012](../design/012-recipe-coordinate-mapping.md)), the anti-drift
  guarantee that ensures every consumer places a recipe in the same
  group/dashboard/tab.
- The GP JSON contract (`data/index.json` + `data/series/<recipe>.json`)
  uses the same coordinate scheme and is forward-compatible with TG's
  workers, API, and UI — it is not a throwaway interim format.

The difference is in the rendering stack:

- **GP (this site)** — static GitHub Pages, no server, no live workers, no
  GKE cluster. Rebuilt from the verified evidence tree on every merge to `main`
  by a deterministic Go generator.
- **TG** — live stack with upstream TestGrid workers/tabulator/summarizer,
  an AICR-native read-only API, a greenfield SPA, and an always-on GKE host
  cluster. TG is being built in parallel; its children (TG1–TG7) are Ready
  and in progress.

**RQ1 (#1283) targets this dashboard.** It is the link target today because
TG4a/TG4b's live API and UI have not shipped yet — not because TG work is
deferred; the two surfaces are being built in parallel (see above). Once RQ1
lands, the Recipe Health Evidence column will deep-link here —
`https://validation.aicr.run/#/<group>/<dashboard>/<tab>`, i.e. this site's
origin plus `/#/` plus the recipe's `Coordinate.Path()` — built
offline from resolved criteria via `pkg/recipe.CoordinateFor`, with no network
call from the generator. Only recipes with an actual dashboard presence get a
link; the rest keep an honest `pending` until real-hardware coverage broadens.
This `index.json` is also available as an interim coordinate-presence source
for the RQ2 link-integrity check while TG4a's own coordinate-presence endpoint
isn't live yet — a sequencing option for RQ2, not a re-point of
[ADR-012](../design/012-recipe-coordinate-mapping.md).

### Relationship to Recipe Health

The [Recipe Health](./recipe-health.md) matrix (#1224 / ADR-009) and this
dashboard are **structural siblings that never duplicate each other**:

- **Recipe Health** owns the **offline structural** signal — does the recipe
  resolve cleanly, are its charts pinned, are its constraints well-formed —
  computed hermetically without a cluster. Its design is in
  [ADR-009](../design/009-recipe-health-tracking.md).
- **This dashboard** owns the **live corroboration** signal — derived from
  real, signed validation runs attested by distinct parties.

The two surfaces share exactly one thing: the recipe's `metadata.name` join
key. Both enumerate recipes by overlay name; the coordinate is derived from
the same resolved criteria by the same mapping function. They line up on
identity without sharing computation.

Issue `#1224` shipped the Recipe Health **Evidence** column as a literal `pending`
for every recipe; that's still true today. RQ1 (`#1283`), the follow-on issue
that fills it in, turns a recipe's `pending` into a deep-link only once
that recipe has a published coordinate on this dashboard — see
[Relationship to TestGrid](#relationship-to-testgrid) above for the exact URL
form and the presence condition; a recipe with no dashboard coordinate yet
stays `pending`. Once a link exists it is **stable**, because Kubernetes
version is kept out of the coordinate path (see
[The coordinate and stable URLs](#the-coordinate-and-stable-urls)), so a
cluster upgrade never breaks it. The cross-link is advisory and
never a merge gate; the Evidence column links, it never copies this
dashboard's content.

ADR-009's verify-gated freshness signal (AttestedAt age, unattested-vs-aged
trust distinction) is a distinct, later refinement that ADR-009 tracks
independently as a **separate validation-posture (Evidence) column** that carries
that freshness distinction, not an in-cell state. It coexists with the
deep-link: the link points at the live board; the column can land later without
changing what the link resolves to.
