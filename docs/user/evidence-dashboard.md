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

The dashboard shares the same five-level, CSP-first addressing space as the
TestGrid. Navigating it follows the same structure documented in
[TestGrid](./testgrid.md):

| Level | Value | Example |
|-------|-------|---------|
| **Group** | service (the CSP) | `eks` |
| **Dashboard** | accelerator + OS | `h100-ubuntu` |
| **Tab** | intent, optionally with platform | `training-kubeflow` |
| **Row** | validation `<phase>/<check>` | `conformance/gpu-operator-ready` |
| **Source column** | one signer | one allowlisted party |

From the catalog overview you navigate: pick a group → pick a dashboard → pick
a tab to reach the **consensus grid** for that recipe. Every cell in the grid
shows the row's consensus state alongside a **source dot-strip** — one
dot per signer, coloured by source class (see [Source classes](#source-classes)
below). Clicking a dot opens the **per-source drilldown**: that signer's
build-by-build history for the recipe, with a link to the signed evidence
artifact for each build.

The facet controls (aicr-version, k8s) let you scope the view; the default
scope is **latest-per-signer**: for each signer only the most recent
in-scope build counts toward the consensus. This prevents a stale build from a
previous Kubernetes minor from silently diluting a current result.

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
lives entirely in the URL fragment (`#/<group>/<dashboard>/<tab>[/<signer>]`).
The fragment is never sent to the server, so the same link resolves correctly
on any static host — GitHub Pages included — with no rewrite rules needed. A
path-based URL (without the `#/`) does **not** resolve.

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

### `not-run` is excluded from counting

A signer whose latest in-scope run skipped or left a check pending has a
`not-run` outcome for that row. `not-run` contributes to neither the pass
count nor the fail count, so it can neither promote a row to `CONFIRMED` nor
suppress a `CONTESTED`. A signer with all `not-run` outcomes is omitted from
the index grid entirely; its full history is still visible in the per-source
drilldown.

### Phase rollup

Each recipe tab also shows a per-phase rolled-up state (readiness, deployment,
performance, conformance). The rollup is **worst-first**: the phase state is the
worst-ranked state among all its rows, in this priority order (worst first):

`CONTESTED` → `FAILING` → `UNTESTED` → `SINGLE` → `CONFIRMED`

A single `CONTESTED` row forces the whole phase to `CONTESTED`; a phase with
all-`CONFIRMED` rows rolls up to `CONFIRMED`. A phase with no rows (because the
recipe declares no checks for it) rolls up to `UNTESTED`.

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
**reported** dot: it appears on the dot-strip with a distinct visual indicator
but is never counted toward `CONFIRMED`, `SINGLE`, `CONTESTED`, or `FAILING`.
Its reported count is shown as metadata only. See
[Sybil resistance](#sybil-resistance) below.

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
   `CODEOWNERS`. A new signer can appear on the dot-strip as a zero-weight
   reported dot without an allowlist entry; only a maintainer-merged allowlist
   edit promotes it to a weighted source.

2. **Distinct-signer counting on canonical identity.** The dashboard keys the
   signer count on the verified `(issuer, identity)` pair, never the
   contributor-controlled `idHash` in the pointer file. Two pointer files that
   share the same verified identity count as one signer — they cannot combine
   to manufacture `CONFIRMED`.

The allowlist also enforces that entries do not overlap (one verified
identity matches at most one entry) and that regex patterns are not
over-broad (an unbounded wildcard org/repo segment is rejected at load
time). A verified-but-unknown community signer can never promote a row to
`CONFIRMED` by itself: it is a zero-weight reported dot.

## Facets

Two facets let you slice the view:

- **aicr-version** — the `aicr` release that produced the run. UAT normally
  builds only the current checkout, so the default view is effectively `main`.
  The facet becomes meaningful when the multi-version pipeline is live.
- **k8s** (Kubernetes `major.minor`) — the cluster version observed in the
  run. A result from an old Kubernetes minor is never silently fused with a
  current-version result; the facet lets you scope to the version you care
  about, and the **latest-per-signer default** already avoids mixing stale and
  current results from the same signer without any manual filtering.

Both facets are parallel: you can combine them to view, for example, only
results from `v0.42.0` on Kubernetes `1.33`.

## How it relates to TestGrid and Recipe Health

### Relationship to TestGrid

The evidence corroboration dashboard (GP) and the [AICR TestGrid](./testgrid.md)
(TG) are **siblings that share the same foundation**:

- They read from the same GCS bucket and the same verified, source-keyed
  evidence tree.
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
deferred; the two surfaces are being built in parallel (see above). The
Recipe Health Evidence column deep-links here:docs/user/testgrid.md `https://validation.aicr.run/#/<group>/<dashboard>/<tab>` —
this site's origin plus `#/` plus the recipe's `Coordinate.Path()` — built
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

ADR-009's verify-gated in-cell freshness state (AttestedAt age, unattested-vs-aged
trust distinction) is a distinct, later refinement that ADR-009 tracks
independently. It coexists with the deep-link: the link points at the live
board; an optional freshness annotation can later appear in the same cell
without changing what the link resolves to.
