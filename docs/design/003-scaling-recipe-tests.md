# ADR-003: Scaling KWOK Recipe Tests for CI

## Status

**Proposed** — 2026-03-18

## Scope

This ADR applies exclusively to **KWOK scheduling simulation tests** — lightweight
tests that verify recipe overlays produce correct Kubernetes scheduling topology
(node selectors, taints, tolerations, resource requests) using simulated nodes.

KWOK simulates node topology but **not** GPU hardware, operator pod health, or NCCL
fabrics. This tiered strategy must not be generalized to hardware-dependent validation
(GPU operator health checks, NCCL bandwidth tests, real-cluster conformance) where
a post-merge failure gap carries significantly higher risk.

## Context

AICR validates every recipe overlay in CI using KWOK-simulated Kubernetes clusters.
The workflow (`.github/workflows/kwok-recipes.yaml`) dynamically discovers overlays
with a cloud `service` criteria and creates one parallel GitHub Actions job per overlay.

Each job:
1. Provisions a Kind cluster with KWOK controller
2. Builds `aicr` from source
3. Creates simulated GPU nodes matching the overlay's topology
4. Generates a bundle and validates scheduling

This runs on every PR that touches `recipes/**`, `kwok/**`, `.github/actions/kwok-test/**`,
or the workflow itself.

### Current scale

The discovery step finds every overlay where `spec.criteria.service` is set and is
not `any` — this includes cloud services (EKS, AKS, GKE) and local environments
(Kind). As of this writing, that produces **36 parallel jobs**:

| Service | Generic (accel=any) | H100 | GB200 | Total |
|---------|---------------------|------|-------|-------|
| EKS     | 3                   | 6    | 6     | **15** |
| AKS     | 3                   | 6    | —     | **9** |
| GKE     | 3                   | 4    | —     | **7** |
| Kind    | 2                   | 3    | —     | **5** |
|         |                     |      |       | **36** |

Each job takes 10-15 minutes. With `fail-fast: false`, a full run consumes
6-9 hours of runner time per PR.

### Growth problem

Overlay naming follows a Cartesian product: `{accelerator}-{service}-{os}-{intent}-{variant}`.
The current matrix is roughly `4 services x 2 GPUs x 2 intents x ~1.5 variants = 36`.

Adding just 3 more services (7 total) and only 3 more accelerators (5 total):

- **Conservative** (same variant ratio): 7 x 5 x 2 x 1.5 = **105 jobs**
- **Realistic** (each GPU gets OS/framework variants): **150-200+ jobs**

At 150 jobs x 12 minutes, a single PR validation would consume **30 hours of runner
time** and generate a wall of check statuses that obscures real failures.

The problem is structural: the number of overlays grows as a product of independent
dimensions (service, accelerator, intent, OS, framework), but the CI workflow treats
every overlay as equally important on every PR.

## Decision

We will replace the flat "test every overlay on every PR" model with a **tiered
testing strategy** combined with **diff-aware test selection**.

### Tier 1: PR gate (fast, always runs)

Run on every PR that touches recipe paths. Targets **generic overlays only** — those
where `accelerator` is `any` or unset. These validate service-level integration
(EKS, AKS, GKE, Kind) without accelerator specialization.

Today that is 11 overlays. With 3 new services it becomes ~18 — linear growth
with the number of services, not multiplicative.

Selection criteria in the discovery step:

```bash
# PR gate: only generic overlays (no accelerator specialization)
service=$(yq eval '.spec.criteria.service // ""' "$overlay")
accel=$(yq eval '.spec.criteria.accelerator // ""' "$overlay")
if [[ -n "$service" && "$service" != "null" && "$service" != "any" ]]; then
  if [[ -z "$accel" || "$accel" == "null" || "$accel" == "any" ]]; then
    # Include in PR gate
  fi
fi
```

### Tier 2: Diff-aware accelerator tests (PR, conditional)

When a PR modifies a specific accelerator overlay or its component values, test
only the affected overlays. The discovery step computes a dependency graph:

1. List changed files via `git diff --name-only $BASE..HEAD`
2. If a specific overlay file changed (e.g., `h100-eks-ubuntu-training.yaml`),
   include that overlay
3. If a shared component changed (e.g., `components/gpu-operator/values-eks.yaml`),
   include all overlays that reference it
4. If `registry.yaml` or `base.yaml` changed, fall through to Tier 1 only —
   the full matrix runs in Tier 3

This keeps PR jobs proportional to the scope of the change.

### Tier 3: Full matrix (merge to main, nightly)

Run the complete set of overlays on:
- Every merge to `main` (post-merge validation)
- A nightly schedule (catch regressions from external dependency updates)

This is the existing behavior, unchanged, but no longer blocking PRs.

**Release qualification:** Nightly Tier 3 runs double as a qualification gate for
release candidates. This separates recipe correctness (KWOK — will it schedule?)
from runtime correctness (real clusters — does it work?). Only SHAs where the
nightly full-matrix run passes are eligible for promotion as release candidates.

**Concurrency policy for Tier 3:** The current workflow uses `cancel-in-progress: true`,
which means rapid successive merges to `main` can cancel in-flight Tier 3 runs. To
guarantee full coverage on every merge, Tier 3 must use a separate concurrency group
with `cancel-in-progress: false`. The nightly schedule provides a backstop — if a
merge run is lost due to operational issues, the nightly run catches it.

```yaml
# Tier 3 concurrency: never cancel main runs
concurrency:
  group: kwok-tier3-${{ github.sha }}
  cancel-in-progress: false
```

### Workflow structure

The single `kwok-recipes.yaml` workflow splits into three jobs:

```
discover
├── tier1-recipes: [eks, aks, gke, kind, ...]          # generic only
├── tier2-recipes: [h100-eks-ubuntu-training, ...]     # diff-affected only
└── tier3-recipes: [all 36+]                           # full matrix

test-tier1  (PR + push to main)
  matrix: tier1-recipes

test-tier2  (PR only, skip if empty)
  matrix: tier2-recipes

test-tier3  (push to main + schedule, skip on PR)
  matrix: tier3-recipes

summary
  needs: [test-tier1, test-tier2, test-tier3]
```

### Required checks

Branch protection requires a **stable aggregate check**, not individual matrix job
names (which drift as overlays are added or removed). The `summary` job serves this
role — it already aggregates results from all tiers.

- **Required check:** `KWOK Test Summary` (the `summary` job)
- **Not required:** Individual `KWOK (recipe-name)` matrix jobs

The `summary` job gates on Tier 1 and Tier 2 for PRs, and on all three tiers for
pushes to `main`. This avoids branch protection brittleness when the overlay set
changes.

## Consequences

### Positive

- **PR jobs scale linearly with services**, not multiplicatively with the full
  dimension product. Adding a new accelerator does not increase PR gate jobs.
- **Runner time drops ~70% on typical PRs.** From 36 jobs to ~11 (Tier 1) plus
  a handful of diff-affected overlays (Tier 2).
- **Full coverage is preserved.** Every overlay is tested on every merge to main
  (with `cancel-in-progress: false`) and on a nightly schedule.
- **Diff-aware selection keeps targeted PRs fast.** A change to a single overlay
  tests only that overlay plus generics — not 36 jobs.
- **No overlay refactoring required.** The overlay files and naming conventions
  are unchanged. Only the CI discovery logic changes.

### Negative

- **Accelerator-specific regressions may land on main.** A PR that breaks
  `gb200-eks-training` without touching its overlay file will not be caught
  until the post-merge Tier 3 run. This is an accepted trade-off — the nightly
  and post-merge runs provide a safety net.
- **Discovery logic becomes more complex.** The diff-aware dependency graph adds
  code to the workflow. This must be tested and maintained.
- **Two-stage failure debugging.** A failure in Tier 3 after merge requires
  a follow-up PR to fix, rather than blocking the original PR.

### Neutral

- **`workflow_dispatch` still supports single-recipe runs.** The manual trigger
  bypasses tier logic and tests exactly the requested recipe.
- **`make kwok-test-all` is unchanged.** Local development can still run the
  full matrix in a shared cluster.

## Alternatives Considered

### 1. Matrix dimensions in CI instead of discrete overlay files

Define `service`, `accelerator`, `intent` as explicit CI matrix axes with
`exclude` rules for invalid combinations.

**Rejected:** Duplicates the overlay resolution logic that already lives in the
recipe engine. The overlay files are the source of truth for valid combinations;
the CI should discover them, not redefine them.

### 2. Equivalence-class grouping

Group overlays that share identical node topology and only test one representative
per group (e.g., `h100-eks-ubuntu-training` and `h100-eks-ubuntu-training-kubeflow`
schedule identically).

**Deferred:** Requires a topology fingerprinting mechanism that does not exist
today. Worth revisiting if Tier 1 + diff-aware selection proves insufficient at
~200 overlays.

### 3. Shared cluster, sequential recipes

Run all recipes sequentially in a single Kind cluster (as `make kwok-test-all`
does locally) instead of one job per recipe.

**Rejected for CI:** Eliminates parallelism, making wall-clock time worse.
A single failure in recipe N blocks recipes N+1 through N+K. Acceptable for
local development but not for CI where fast feedback matters.

### 4. Test only on merge to main, never on PR

**Rejected:** Violates the principle that broken code should not land on main.
Recipe changes are frequent enough that post-merge-only testing would cause
regular regressions.

## References

- [KWOK recipes workflow](../../.github/workflows/kwok-recipes.yaml)
- [KWOK test action](../../.github/actions/kwok-test/)
- [Recipe overlays](../../recipes/overlays/)
- [KWOK project](https://kwok.sigs.k8s.io/)
