{/*
Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/}

# AICR TestGrid

The AICR TestGrid is the **live validation-posture** board: it shows the actual pass/fail of AICR recipes from real validation runs against real clusters, organized so you can navigate straight from a cloud service provider down to a single check. It answers *"is this recipe passing right now, against which Kubernetes version, and who signed the result?"*

It is the live complement to two **offline, structural** surfaces:

- [Recipe Health](./recipe-health.md) — the catalog-wide structural state of every recipe, computed hermetically with no cluster.
- [Recipe & CLI Coverage Matrix](./coverage-matrix.md) — which journeys and CLI verbs are exercised in-repo, and how often.

Neither of those reports live pass/fail; the TestGrid does. The three surfaces coexist and never duplicate each other — see [How it relates to recipe health](#how-it-relates-to-recipe-health) below. The full design contract is recorded in [ADR-012](../design/012-recipe-coordinate-mapping.md).

## How to read it — CSP-first navigation

The board is a five-level addressing space. You navigate it the way you reason about a deployment: start from your cloud, narrow to your hardware and OS, then your workload.

| Level | Value | Example |
|-------|-------|---------|
| **Group** | service (the CSP) | `eks` |
| **Dashboard** | accelerator + OS | `h100-ubuntu` |
| **Tab** | intent, optionally with platform | `training-kubeflow` |
| **Row** | validation `<phase>/<check>` | `conformance/gpu-operator-ready` |
| **Column** | one build | one validation run |

The first three levels (group, dashboard, tab) come from the **recipe** — they identify *which cell* a result belongs in. The last two (row, column) come from the **validation run** — they describe *what is in that cell*: which checks ran, in which build. A recipe never decides its own rows or columns.

This split is why the board is CSP-first even though recipe names are accelerator-first: the recipe `h100-eks-ubuntu-training-kubeflow` lands at the coordinate `eks/h100-ubuntu/training-kubeflow`. The mapping is derived from the recipe's resolved criteria, never from string-parsing its name, so the two stay independently correct.

### The coordinate

Every cell has a stable, canonical address:

```text
<group>/<dashboard>/<tab>
```

For example `eks/h100-ubuntu/training-kubeflow` (with a platform) or `eks/h100-ubuntu/training` (bare intent — when a recipe declares no platform, the segment is dropped, never filled with a placeholder).

Treat the coordinate as a **stable opaque identity**, not a key you take apart: a dimension value can itself contain `-` (for example `rtx-pro-6000`), so the path has no reliable positional split point. It is something to link to and compare for equality, not to decompose.

## The Kubernetes version facet

The Kubernetes version is deliberately **not** part of the coordinate. It lives **in the column** — one build is one K8s version. This keeps a coordinate (`eks/h100-ubuntu/training`) invariant as clusters upgrade, so links into the board do not churn every time a new K8s minor ships.

The trade-off is that one tab can hold columns for the *same* coordinate at *different* K8s versions. Two countermeasures keep a stale result from being read as a current one:

- a **K8s-version facet / filter**, so you can scope the view to a version; and
- a **latest-per-signer default scope**, so the default view does not mix stale and current results from the same signer.

Each column also carries the recipe's declared K8s constraint alongside the observed version, so you can tell at a glance whether the cluster the build ran on actually satisfied what the recipe asked for.

## Provenance columns — who produced the result

Trust in a column comes from its provenance metadata. Each build column carries a fixed set of keys so you can weigh a result before relying on it:

| Key | Meaning |
|-----|---------|
| `aicr_version` | the `aicr` release that produced the result |
| `k8s_version` | the cluster version actually observed (`major.minor`) |
| `k8s_constraint` | the K8s version the recipe declared it needs |
| `signer_identity` | the signing identity (workflow / subject) that attested the result |
| `signer_issuer` | the OIDC issuer that vouched for that identity |
| `source_class` | how the result was produced (for example `ci` versus ad-hoc) |
| `evidence_digest` | the digest of the underlying signed evidence artifact |

`signer_identity` and `signer_issuer` together are what let you distinguish **community-submitted** results from **NVIDIA UAT** runs, and they key the latest-per-signer default scope. `evidence_digest` is the verifiable anchor: every cell traces back to a signed [conformance evidence](../design/007-recipe-evidence.md) artifact you can verify independently with [artifact verification](./artifact-verification.md).

## How it relates to recipe health

The TestGrid and the [Recipe Health](./recipe-health.md) matrix are **two surfaces that coexist; neither is a richer rendering of the other**:

- **Recipe Health** owns the **offline structural / freshness** signal — does the recipe resolve cleanly, are its charts pinned — computed without a live cluster.
- **The TestGrid** owns the **live validation-posture** signal — derived from real runs against real clusters.

AICR keeps these axes deliberately separate so a "resolves cleanly" verdict never gets fused with a "validated and performant" one. The two surfaces share exactly one thing: the recipe's `metadata.name`, the identity by which both address the same recipe.

The Recipe Health **Evidence** column is the cross-link between them. Today it reads `pending` for every recipe. Once a recipe has signed evidence, that column will **link** into the recipe's TestGrid coordinate — it links, it never copies the board's content — and the link is automatically checkable so it can never point at a coordinate that does not exist on the board.
