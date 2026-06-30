{/*
Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/}

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

{/* BEGIN AICR-HEALTH */}
## Summary

- Recipes: **43**
- Pass: **43** · Warn: **0** · Fail: **0** · Unknown: **0**

## Recipes

| Recipe | Service | Accelerator | OS | Intent | Platform | Status | Coverage | Evidence |
|--------|---------|-------------|----|--------|----------|--------|----------|----------|
| a100-any | — | a100 | — | — | — | pass | R:0 D:4 P:0 C:0 | pending |
| b200-any | — | b200 | — | — | — | pass | R:0 D:4 P:0 C:0 | pending |
| gb200-any | — | gb200 | — | — | — | pass | R:0 D:4 P:0 C:0 | pending |
| h100-any | — | h100 | — | — | — | pass | R:0 D:4 P:0 C:0 | pending |
| h200-any | — | h200 | — | — | — | pass | R:0 D:4 P:0 C:0 | pending |
| l40s-any | — | l40s | — | — | — | pass | R:0 D:4 P:0 C:0 | pending |
| rtx-pro-6000-any | — | rtx-pro-6000 | — | — | — | pass | R:0 D:4 P:0 C:0 | pending |
| monitoring-hpa | — | — | — | — | — | pass | R:0 D:0 P:0 C:0 | pending |
| a100-aks-ubuntu-training-kubeflow | aks | a100 | ubuntu | training | kubeflow | pass | R:0 D:4 P:0 C:10 | pending |
| h100-aks-ubuntu-inference-dynamo | aks | h100 | ubuntu | inference | dynamo | pass | R:0 D:4 P:1 C:11 | pending |
| h100-aks-ubuntu-training-kubeflow | aks | h100 | ubuntu | training | kubeflow | pass | R:0 D:4 P:0 C:10 | pending |
| bcm-inference | bcm | — | — | inference | — | pass | R:0 D:0 P:0 C:5 | pending |
| h100-bcm-ubuntu-training | bcm | h100 | ubuntu | training | — | pass | R:0 D:4 P:0 C:5 | pending |
| a100-eks-ubuntu-training-kubeflow | eks | a100 | ubuntu | training | kubeflow | pass | R:0 D:4 P:0 C:10 | pending |
| gb200-eks-ubuntu-inference-dynamo | eks | gb200 | ubuntu | inference | dynamo | pass | R:0 D:4 P:1 C:10 | pending |
| gb200-eks-ubuntu-training-kubeflow | eks | gb200 | ubuntu | training | kubeflow | pass | R:0 D:4 P:2 C:8 | pending |
| gb200-eks-ubuntu-training-slurm | eks | gb200 | ubuntu | training | slurm | pass | R:0 D:4 P:0 C:10 | pending |
| h100-eks-ubuntu-inference-dynamo | eks | h100 | ubuntu | inference | dynamo | pass | R:0 D:4 P:1 C:11 | pending |
| h100-eks-ubuntu-inference-nim | eks | h100 | ubuntu | inference | nim | pass | R:0 D:4 P:0 C:11 | pending |
| h100-eks-ubuntu-training-kubeflow | eks | h100 | ubuntu | training | kubeflow | pass | R:0 D:4 P:1 C:10 | pending |
| h100-eks-ubuntu-training-slurm | eks | h100 | ubuntu | training | slurm | pass | R:0 D:4 P:0 C:11 | pending |
| h200-eks-inference | eks | h200 | — | inference | — | pass | R:0 D:4 P:0 C:5 | pending |
| h200-eks-training | eks | h200 | — | training | — | pass | R:0 D:4 P:1 C:10 | pending |
| rtx-pro-6000-eks-ubuntu-inference-dynamo | eks | rtx-pro-6000 | ubuntu | inference | dynamo | pass | R:0 D:4 P:1 C:11 | pending |
| rtx-pro-6000-eks-ubuntu-inference-nim | eks | rtx-pro-6000 | ubuntu | inference | nim | pass | R:0 D:4 P:0 C:11 | pending |
| a100-gke-cos-training-kubeflow | gke | a100 | cos | training | kubeflow | pass | R:0 D:4 P:0 C:10 | pending |
| b200-gke-cos-inference-dynamo | gke | b200 | cos | inference | dynamo | pass | R:0 D:4 P:0 C:11 | pending |
| b200-gke-cos-training-kubeflow | gke | b200 | cos | training | kubeflow | pass | R:0 D:4 P:0 C:10 | pending |
| h100-gke-cos-inference-dynamo | gke | h100 | cos | inference | dynamo | pass | R:0 D:4 P:1 C:11 | pending |
| h100-gke-cos-training-kubeflow | gke | h100 | cos | training | kubeflow | pass | R:0 D:4 P:1 C:10 | pending |
| h100-gke-cos-training-slurm | gke | h100 | cos | training | slurm | pass | R:0 D:4 P:0 C:11 | pending |
| h100-kind-inference-dynamo | kind | h100 | — | inference | dynamo | pass | R:0 D:4 P:0 C:11 | pending |
| h100-kind-training-kubeflow | kind | h100 | — | training | kubeflow | pass | R:0 D:4 P:0 C:10 | pending |
| h100-kind-training-slurm | kind | h100 | — | training | slurm | pass | R:0 D:4 P:0 C:10 | pending |
| rtx-pro-6000-lke-ubuntu-inference | lke | rtx-pro-6000 | ubuntu | inference | — | pass | R:0 D:4 P:0 C:8 | pending |
| rtx-pro-6000-lke-ubuntu-training | lke | rtx-pro-6000 | ubuntu | training | — | pass | R:0 D:4 P:0 C:8 | pending |
| ocp-inference | ocp | — | — | inference | — | pass | R:0 D:3 P:0 C:1 | pending |
| ocp-training | ocp | — | — | training | — | pass | R:0 D:3 P:0 C:1 | pending |
| a100-oke-ubuntu-training-kubeflow | oke | a100 | ubuntu | training | kubeflow | pass | R:0 D:4 P:0 C:8 | pending |
| gb200-oke-ubuntu-inference-dynamo | oke | gb200 | ubuntu | inference | dynamo | pass | R:0 D:4 P:1 C:10 | pending |
| gb200-oke-ubuntu-training-kubeflow | oke | gb200 | ubuntu | training | kubeflow | pass | R:0 D:4 P:1 C:8 | pending |
| l40s-oke-inference | oke | l40s | ol | inference | — | pass | R:0 D:4 P:0 C:8 | pending |
| l40s-oke-training | oke | l40s | ol | training | — | pass | R:0 D:4 P:0 C:8 | pending |

{/* END AICR-HEALTH */}
