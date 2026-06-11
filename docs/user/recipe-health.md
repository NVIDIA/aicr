<!--
Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
-->

# Recipe Health

This page reports the **structural health** of every recipe AICR can resolve — one row per leaf criteria combination (service × accelerator × OS × intent × platform). It answers *"across the whole matrix, what is the current structural state of each recipe?"* and is the catalog-wide complement to per-recipe [conformance evidence](../design/007-recipe-evidence.md).

The matrix is computed **hermetically and offline**: every signal is a pure read of the resolved recipe — no Helm render, no GPU, no cluster, no network. It is regenerated from the recipe catalog by `make recipe-health-docs` and is kept current by a weekly bot PR. `make recipe-health-check` is an advisory staleness check (it is **not** wired into `make qualify` or the merge gate). The full design is recorded in [ADR-009](../design/009-recipe-health-tracking.md).

## What the columns mean

**Status** is the rolled-up structural verdict per recipe:

- `pass` — the recipe is structurally sound.
- `warn` — a non-fatal structural concern was surfaced.
- `fail` — a graded structural signal failed (e.g. the recipe does not resolve).
- `unknown` — a transient resolver error (a re-runnable timeout) prevented a confident verdict; the recipe is held rather than penalized. `unknown` is never silently read as `pass`.

> **Structural soundness is not a validation verdict.** A recipe that resolves cleanly is *structurally sound*, **not** *validated and performant*. Runtime/validation claims come only from signed conformance evidence, which is out of scope for this matrix today (see the Evidence column below).

**chart_pinned (folded into Status).** One of the graded signals behind `Status` checks that every resolved Helm component references an explicit chart version, per [ADR-006](../design/006-image-pinning-policy.md). This is **layer 1 only** — the chart-version pin — *not* image-digest pinning, and it is a render-free read of the resolved recipe (it does not pull or template the chart).

**Coverage** is a descriptor — it is *never* graded, so a deliberately minimal recipe is never penalized for declaring fewer checks. It is a compact per-phase summary of the **declared** validation checks, in the form `R:n D:n P:n C:n` — the count of named checks declared for the readiness, deployment, performance, and conformance phases respectively.

**Evidence** is a literal `pending` for every recipe today. No conformance attestations exist yet, so the column is honestly uniform: it reports the absence of evidence rather than overstating what is known. A differentiated, evidence-derived column lands once the first signed attestation does.

<!-- BEGIN AICR-HEALTH -->
<!-- AICR-HEALTH-DEFERRED: matrix publication intentionally withheld; see the note below. Remove this sentinel by running `make recipe-health-docs`, which overwrites this whole region with the generated matrix. -->
_Matrix publication is intentionally deferred. The generator (`tools/health`) and the `make recipe-health-docs` / `make recipe-health-check` targets are in place and fully functional today — what is withheld is the **committed matrix body**, not the tooling. One structural signal (`chart_pinned`) currently over-reports `fail` for manifest-only components (declared `type: Helm` but shipping local manifests with no external chart to pin), which would render a misleading matrix. The matrix will be populated by `make recipe-health-docs` once that signal is corrected. See [ADR-009](../design/009-recipe-health-tracking.md)._
<!-- END AICR-HEALTH -->
