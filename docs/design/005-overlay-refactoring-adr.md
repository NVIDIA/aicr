# ADR-005: Refactor Overlay Structure to Reduce Duplication

## Status

**Proposed** — 2026-03-19

## Problem

The overlay system has **38 files** with significant duplication that grows
with each new accelerator and service:

- K8s version constraints repeated in **16 files**
- Ubuntu OS constraints repeated in **12 files**
- Validation checks repeated in **10+ files**
- GPU operator overrides duplicated across training/inference variants
- Single-parent inheritance forces deep inheritance chains (up to **6 levels**)
- **~350+ redundant lines** across the overlay tree

Adding new accelerators (B200, GB300) and services (OKE) under the current
structure would grow the tree to **96-120 files**, each carrying duplicated
boilerplate from sibling leaf overlays.

## Non-Goals

- No resolver model replacement (the criteria-matching resolver is not changing)
- No dimension auto-compose in this ADR (Auto-Compose option is documented for reference only)
- No changes to the recipe CLI interface or recipe output format

## Options Summary

| Option | Approach | Dedup | Code Change | Inheritance | Status |
|--------|----------|-------|-------------|-------------|--------|
| **Reorder** | Reorder inheritance tree | ~40% | None | Yes | Recommended now (Phase 1) |
| **Reorder + Mixins** | Reorder tree + OS/platform mixins | ~75% | ~80 lines | Yes (+ mixins) | Recommended now (Phase 2) |
| **Reorder + Deep Mixins** | Reorder + Mixins + validation mixins | ~90% | ~150 lines | Yes (+ mixins) | Deferred until merge upgrade proven |
| **Flat Mixins** | Flat mixin-only composition | ~95% | Moderate | No — flat | Only if inheritance model abandoned |
| **Auto-Compose** | Dimension-driven auto-composition | ~95% | Large | No — new model | Only if resolver model replaced |

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
│       │       └── h100-eks-ubuntu-inference-dynamo
│       └── gb200-eks-inference
│           └── gb200-eks-ubuntu-inference
│               └── gb200-eks-ubuntu-inference-dynamo
├── aks
│   ├── aks-training
│   │   └── h100-aks-training
│   │       └── h100-aks-ubuntu-training
│   │           └── h100-aks-ubuntu-training-kubeflow
│   └── aks-inference
│       └── h100-aks-inference
│           └── h100-aks-ubuntu-inference
│               └── h100-aks-ubuntu-inference-dynamo
├── gke-cos
│   ├── gke-cos-training
│   │   └── h100-gke-cos-training
│   │       └── h100-gke-cos-training-kubeflow
│   └── gke-cos-inference
│       └── h100-gke-cos-inference
│           └── h100-gke-cos-inference-dynamo
├── kind
│   ├── kind-inference
│   │   └── h100-kind-inference
│   └── h100-kind-training
└── monitoring-hpa
```

**Total: 38 overlay files.**

### Growth problem

Adding new accelerators (B200, GB300) and services (OKE) under the current
structure requires creating a full inheritance chain per combination:

| Dimension  | Current | Near-term |
|------------|---------|-----------|
| Services   | 4 (EKS, AKS, GKE, Kind) | 6+ (+ OKE, custom) |
| Accelerators | 2 (H100, GB200) | 4+ (+ B200, GB300) |
| Intents    | 2 (training, inference) | 2 |
| OS variants | 2 (ubuntu, cos) | 3+ (+ RHEL) |
| Platforms  | 3 (kubeflow, dynamo, none) | 4+ |

**Current files per new accelerator on EKS:** 6 files (training + inference
+ ubuntu variants + platform variants), each duplicating GPU operator config,
skyhook, K8s constraints from siblings.

**Projected file count at 6 services x 4 accelerators:** ~96-120 files.

### Duplication hotspots

| Duplicated Content | Occurrences | Impact |
|-------------------|-------------|--------|
| K8s `>= 1.32.4` constraint | 16 files | Must update in every accelerator leaf overlay |
| Ubuntu OS constraints (4 lines) | 12 files | Every `-ubuntu-` suffixed leaf overlay |
| GPU operator cdi/gdrcopy overrides | Duplicated across training + inference per accelerator | Change requires editing 2+ files |
| Conformance validation checks (10 lines) | 10+ files | Adding a check requires editing every leaf overlay |
| Deployment validation block | 5 files | Identical block repeated |
| Skyhook tuning config | 6 files | Same pattern with minor intent difference |
| Dynamo components | 4 files | Identical except storage class |

**Total: ~350+ redundant lines across 38 files.**

### Root cause

The current tree branches on intent (`training` vs `inference`) **before**
accelerator. This means every accelerator must duplicate the intent split,
and GPU operator config that's identical across intents is repeated.

The single-parent `spec.base` model prevents sharing across orthogonal
dimensions (e.g., a leaf overlay can't inherit from both `h100-eks-training`
and `os-ubuntu` simultaneously).

## Options

### Reorder: Reorder Inheritance Tree (No Code Changes)

Insert `{accelerator}-{service}` intermediate overlays that hold shared GPU
config. Both training and inference leaf overlays inherit from the accelerator
overlay instead of duplicating it.

**Inheritance chain example — `h100-eks-ubuntu-training-kubeflow`:**

```
base
└── eks                        # EBS CSI, EFA, Prometheus gp2 storage
    └── h100-eks  (NEW)        # gpu-operator cdi/gdrcopy, K8s >= 1.32.4, skyhook tuning
        └── h100-eks-training  # intent=training (skyhook multiNodeTraining only)
            └── h100-eks-ubuntu-training   # Ubuntu 24.04, kernel >= 6.8, validation
                └── h100-eks-ubuntu-training-kubeflow  # kubeflow-trainer
```

**Full proposed tree:**

```
base
├── eks
│   ├── h100-eks  ← NEW (gpu-operator cdi+gdrcopy, skyhook tuning, K8s >= 1.32.4)
│   │   ├── h100-eks-training           (intent + skyhook multiNodeTraining)
│   │   │   └── h100-eks-ubuntu-training
│   │   │       └── +kubeflow
│   │   └── h100-eks-inference          (intent + kgateway)
│   │       └── h100-eks-ubuntu-inference
│   │           └── +dynamo
│   ├── gb200-eks ← NEW (gpu-operator cdi+gdrcopy, skyhook no-op, K8s >= 1.32.4)
│   │   ├── gb200-eks-training
│   │   │   └── gb200-eks-ubuntu-training
│   │   │       └── +kubeflow
│   │   └── gb200-eks-inference
│   │       └── gb200-eks-ubuntu-inference
│   │           └── +dynamo
│   └── (future: b200-eks, gb300-eks — one file each)
├── aks
│   ├── h100-aks  ← NEW
│   │   ├── h100-aks-training
│   │   │   └── ...
│   │   └── h100-aks-inference
│   │       └── ...
│   └── (future accelerators)
├── gke-cos
│   ├── h100-gke-cos ← NEW
│   │   ├── h100-gke-cos-training
│   │   │   └── +kubeflow
│   │   └── h100-gke-cos-inference
│   │       └── +dynamo
│   └── (future accelerators)
├── kind (unchanged — only H100, no ubuntu variants)
└── monitoring-hpa
```

**What's eliminated:**
- GPU operator overrides: defined once in `h100-eks.yaml`, shared by training + inference
- K8s `>= 1.32.4`: defined once per `{accelerator}-{service}`, not per intent
- Skyhook base config: defined once per accelerator+service

**What's NOT eliminated:**
- Ubuntu constraints still duplicated in every `-ubuntu-` leaf overlay (12 files)
- Validation checks still duplicated (10+ files)
- Platform components (kubeflow, dynamo) still duplicated

**File count:** ~42 files (adds ~4 intermediate files, removes none). Growth
for new accelerator on EKS: 5 files (1 shared + 2 intent + 2 ubuntu) vs 6
currently, but the shared file eliminates ~40% of per-file content.

**Tradeoffs:**

| Pro | Con |
|-----|-----|
| No code changes | Only addresses ~40% of duplication |
| Preserves existing inheritance model | Ubuntu/validation still duplicated |
| Low risk — just overlay restructuring | Inheritance chain depth increases by 1 level |
| Incremental — can be done per-service | Still grows linearly with new OS variants |

### Reorder + Mixins: Reorder Tree + OS/Platform Mixins

Combine the Reorder option's tree restructuring with a new `spec.mixins` field for the two
most duplicated orthogonal concerns: **OS** and **platform**. Each leaf
overlay still has a single parent via `spec.base`, but can compose additional
mixin fragments via `spec.mixins`. Validation stays in the inheritance chain.

**Mixin files (3 total):**

| Mixin | Content | Replaces |
|-------|---------|----------|
| `os-ubuntu.yaml` | Ubuntu 24.04, kernel >= 6.8 | 12 duplications |
| `platform-kubeflow.yaml` | kubeflow-trainer component | 4 duplications |
| `platform-dynamo.yaml` | dynamo-crds, dynamo-platform, K8s >= 1.34 | 4 duplications |

**Inheritance chain example — `h100-eks-ubuntu-training-kubeflow`:**

```
base
└── eks
    └── h100-eks                          # gpu-operator, skyhook, K8s >= 1.32.4
        └── h100-eks-training             # intent=training, validation checks
            └── h100-eks-ubuntu-training-kubeflow  (leaf overlay)
                  mixins:
                    - os-ubuntu                    # Ubuntu constraints
                    - platform-kubeflow            # kubeflow-trainer
                  combo-specific:
                    - nccl-all-reduce-bw >= 300    # EKS H100 threshold
```

**Merge order pseudocode (Phase 2):**

```
result = base_spec                          # start with base
for overlay in inheritance_chain:           # eks → h100-eks → h100-eks-training
    result = RecipeMetadataSpec.Merge(result, overlay.spec)
for mixin_name in leaf.spec.mixins:         # os-ubuntu, platform-kubeflow
    mixin = load_mixin(mixin_name)          # from recipes/mixins/
    result = RecipeMetadataSpec.Merge(result, mixin.spec)  # constraints + componentRefs only
result = RecipeMetadataSpec.Merge(result, leaf.spec)  # leaf's own content last
evaluate_constraints(result)                # runs on fully merged recipe
```

**Full proposed tree (leaf overlays shown with their mixins):**

```
base
├── eks
│   ├── h100-eks
│   │   ├── h100-eks-training                      (validation checks defined here)
│   │   │   ├── h100-eks-ubuntu-training           [mixins: os-ubuntu]
│   │   │   └── h100-eks-ubuntu-training-kubeflow  [mixins: os-ubuntu, platform-kubeflow]
│   │   └── h100-eks-inference                     (validation checks defined here)
│   │       ├── h100-eks-ubuntu-inference           [mixins: os-ubuntu]
│   │       └── h100-eks-ubuntu-inference-dynamo    [mixins: os-ubuntu, platform-dynamo]
│   └── gb200-eks
│       ├── gb200-eks-training                     (validation checks defined here)
│       │   ├── gb200-eks-ubuntu-training           [mixins: os-ubuntu]
│       │   └── gb200-eks-ubuntu-training-kubeflow  [mixins: os-ubuntu, platform-kubeflow]
│       └── gb200-eks-inference                    (validation checks defined here)
│           ├── gb200-eks-ubuntu-inference           [mixins: os-ubuntu]
│           └── gb200-eks-ubuntu-inference-dynamo    [mixins: os-ubuntu, platform-dynamo]
├── aks (same pattern)
├── gke-cos (same pattern)
└── kind (unchanged)

Shared mixin fragments (in recipes/mixins/):
├── os-ubuntu.yaml
├── platform-kubeflow.yaml
└── platform-dynamo.yaml
```

**Before/after example — `h100-eks-ubuntu-training-kubeflow.yaml`:**

```yaml
# BEFORE (current): 45 lines — duplicates Ubuntu constraints, kubeflow component, validation
kind: RecipeMetadata
spec:
  base: h100-eks-training
  criteria:
    service: eks
    accelerator: h100
    os: ubuntu
    intent: training
    platform: kubeflow
  constraints:
    - name: K8s.server.version        # ← duplicated from h100-eks-training
      value: ">= 1.32.4"
    - name: OS.release.ID             # ← duplicated in 12 files
      value: ubuntu
    - name: OS.release.VERSION_ID     # ← duplicated in 12 files
      value: "24.04"
    - name: OS.sysctl./proc/sys/kernel/osrelease  # ← duplicated in 12 files
      value: ">= 6.8"
  componentRefs:
    - name: kubeflow-trainer          # ← duplicated in 4 files
      namespace: kubeflow
      chart: kubeflow-trainer
      type: Helm
      source: oci://ghcr.io/kubeflow/charts
      version: 2.1.0
      valuesFile: components/kubeflow-trainer/values.yaml
      dependencyRefs: [cert-manager, kube-prometheus-stack, gpu-operator]
      manifestFiles: [components/kubeflow-trainer/manifests/torch-distributed-cluster-training-runtime.yaml]
  validation:                         # ← duplicated in 5+ files
    deployment:
      checks: [operator-health, expected-resources, gpu-operator-version, check-nvidia-smi]
    performance:
      checks: [nccl-all-reduce-bw]
      constraints: [{name: nccl-all-reduce-bw, value: ">= 300"}]
    conformance:
      checks: [platform-health, gpu-operator-health, dra-support, ...]

# AFTER (Reorder + Mixins): 15 lines — only criteria, mixins, and combo-specific overrides
kind: RecipeMetadata
spec:
  base: h100-eks-training      # inherits gpu-operator, skyhook, validation
  mixins:
    - os-ubuntu                # Ubuntu constraints (defined once in mixin)
    - platform-kubeflow        # kubeflow-trainer (defined once in mixin)
  criteria:
    service: eks
    accelerator: h100
    os: ubuntu
    intent: training
    platform: kubeflow
  constraints:
    - name: nccl-all-reduce-bw   # combo-specific only
      value: ">= 300"
```

**Mixin semantics:**

Mixins use a distinct schema (`kind: RecipeMixin`) and live in a separate
directory (`recipes/mixins/`). This ensures the loader never treats them as
matchable overlays and prevents double-application.

```yaml
kind: RecipeMixin        # distinct from RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: os-ubuntu
spec:
  # Mixins carry ONLY additive content — no criteria, no base
  constraints:
    - name: OS.release.ID
      value: ubuntu
    - name: OS.release.VERSION_ID
      value: "24.04"
    - name: OS.sysctl./proc/sys/kernel/osrelease
      value: ">= 6.8"
```

**Allowed mixin fields:** `constraints` and `componentRefs`.
Mixins **cannot** contain `criteria`, `base`, `mixins`, or `validation`.

**Why not validation in mixins (Reorder + Mixins):** Current validation merge in
`RecipeMetadataSpec.Merge()` is phase-replacement, not deep merge. If two
sources both set `conformance.checks`, the second silently replaces the
first. Until merge semantics are upgraded to deep-merge validation phases
(see Reorder + Deep Mixins below), validation stays in the inheritance chain where ordering
is explicit and predictable.

**Constraint evaluation:** The current constraint evaluator in
`BuildRecipeResultWithEvaluator()` runs per-overlay before merge. Phase 2
must update this to evaluate constraints on the **fully merged recipe**
(including mixin constraints). Without this change, mixin-contributed
constraints would be invisible to the evaluator.

**Conflict policy:** Mixin-vs-inheritance conflicts follow the same
semantics as child overriding parent — the mixin value wins (mixins apply
after the inheritance chain). Mixin-vs-mixin conflicts are **forbidden**: a
CI lint check rejects any leaf overlay where two mixins set the same
constraint name or add the same component. This avoids implicit ordering
dependence between mixins and keeps each mixin truly independent.

**Code change:** Add `Mixins []string` field to `RecipeMetadataSpec`. In
`mergeOverlayChains()`, after resolving the inheritance chain, merge each
mixin in order before applying the leaf overlay's own content. Loader
excludes `recipes/mixins/` from overlay discovery. Update
`BuildRecipeResultWithEvaluator()` to evaluate constraints post-merge.
~80 lines in `metadata_store.go` and `metadata.go`.

**File count:** ~42 leaf overlays + 3 mixin files = ~45 total. Leaf
overlay files shrink from ~45-80 lines to ~15-20 lines each.

**Growth for new accelerator (B200 on EKS):**
1. `b200-eks.yaml` — GPU operator config (1 file)
2. `b200-eks-training.yaml` — intent + validation (1 file)
3. `b200-eks-inference.yaml` — intent + validation (1 file)
4. `b200-eks-ubuntu-training.yaml` — leaf overlay with `mixins: [os-ubuntu]` (1 file)
5. `b200-eks-ubuntu-inference.yaml` — leaf overlay with `mixins: [os-ubuntu]` (1 file)
6. Platform variants as needed — each is a ~15-20 line leaf overlay

No Ubuntu constraints or platform components to duplicate. Validation checks
are inherited from the intent overlay (`b200-eks-training`).

**Tradeoffs:**

| Pro | Con |
|-----|-----|
| Eliminates ~75% of duplication | Requires code change (~80 lines) |
| Preserves inheritance model | New concept (mixins) to learn |
| Leaf overlays become trivial (~15 lines) | Two composition mechanisms (inheritance + mixins) |
| Adding new OS/platform = 1 mixin file | Validation stays in inheritance chain (this phase) |
| Backward compatible (mixins optional) | Constraint evaluator must be updated to post-merge |
| Distinct schema prevents loader confusion | Scoped to constraints + components (not validation) |

### Reorder + Deep Mixins: Add Validation Mixins

Extends Reorder + Mixins by adding validation mixins after upgrading merge semantics.

**Additional mixin files (+2):**

| Mixin | Content | Replaces |
|-------|---------|----------|
| `validation-training-full.yaml` | deployment + performance + conformance | 5 duplications |
| `validation-conformance-only.yaml` | conformance checks only | 5 duplications |

**Merge order pseudocode (Phase 3 — extends Phase 2):**

```
result = base_spec
for overlay in inheritance_chain:
    result = RecipeMetadataSpec.Merge(result, overlay.spec)
for mixin_name in leaf.spec.mixins:
    mixin = load_mixin(mixin_name)
    result = RecipeMetadataSpec.DeepMerge(result, mixin.spec)  # NEW: deep-merge validation
result = RecipeMetadataSpec.DeepMerge(result, leaf.spec)
evaluate_constraints(result)
```

**Key difference from Reorder + Mixins:** `DeepMerge` for validation phases — check
lists are concatenated (deduplicated), not replaced. This requires a new
merge function that understands validation phase structure.

**Prerequisite:** `RecipeMetadataSpec.Merge()` must be upgraded to deep-merge
validation phases (additive `checks` lists, constraint-level override). This
is non-trivial and must be proven safe before adoption.

**Tradeoffs vs Reorder + Mixins:**

| Pro | Con |
|-----|-----|
| Reaches ~90% deduplication | Requires merge semantics upgrade (~70 additional lines) |
| Validation defined once per pattern | Deep-merge introduces new conflict surface |
| | Must prove additive check merging is safe |

### Flat Mixins: Flat Mixin-Only Composition (No Inheritance Chains)

> **Trigger condition:** Choose this only if the team accepts abandoning
> multi-level inheritance in favor of flat composition.

Eliminate intermediate overlays entirely. Every leaf overlay directly composes
from `base` plus a set of mixins. No multi-level inheritance chains.

**Mixin files (~15 shared fragments):**

| Mixin | Content |
|-------|---------|
| `service-eks.yaml` | EBS CSI, EFA, gp2 storage, K8s >= 1.28 |
| `service-gke.yaml` | GKE gpu-operator values, standard-rwo, skyhook copyDirRoot |
| `service-aks.yaml` | Network operator, managed-csi storage |
| `intent-training.yaml` | K8s >= 1.30 |
| `intent-inference.yaml` | K8s >= 1.30, kgateway-crds, kgateway |
| `accelerator-h100-eks.yaml` | cdi+gdrcopy, skyhook tuning, K8s >= 1.32.4 |
| `accelerator-h100-gke.yaml` | cdi, NCCL TCPXO, tuning-gke, K8s >= 1.32 |
| `accelerator-h100-aks.yaml` | gdrcopy, K8s >= 1.32.4 |
| `accelerator-gb200-eks.yaml` | cdi+gdrcopy, skyhook no-op, K8s >= 1.32.4 |
| `os-ubuntu.yaml` | Ubuntu 24.04, kernel >= 6.8 |
| `platform-kubeflow.yaml` | kubeflow-trainer |
| `platform-dynamo.yaml` | dynamo-crds, dynamo-platform, K8s >= 1.34 |
| `validation-training-full.yaml` | deployment + performance + conformance |
| `validation-conformance-only.yaml` | conformance only |

**Inheritance chain example — `h100-eks-ubuntu-training-kubeflow`:**

```
base  (single parent, no inheritance chain)
  + service-eks              (mixin)
  + intent-training          (mixin)
  + accelerator-h100-eks     (mixin)
  + os-ubuntu                (mixin)
  + platform-kubeflow        (mixin)
  + validation-training-full (mixin)
  = h100-eks-ubuntu-training-kubeflow  (leaf overlay, combo-specific overrides only)
```

**File count:** ~25 leaf overlays + ~15 mixin files = ~40 total.

**Tradeoffs:**

| Pro | Con |
|-----|-----|
| Maximum deduplication (~95%) | Breaks the inheritance model — major design shift |
| Simplest mental model (flat) | Every leaf overlay must list all mixins explicitly |
| Easiest to add new dimensions | Mixin ordering matters (later overrides earlier) |
| No deep inheritance chain to reason about | Loses implicit sharing from parent chains |
| Leaf overlays are self-describing | Larger code change in resolver |
| | Hard to express "all EKS leaf overlays share X" without a parent |
| | Accelerator mixins are per-service (h100-eks, h100-gke) — still some Cartesian |

### Auto-Compose: Dimension-Driven Auto-Composition

> **Trigger condition:** Choose this only if the team accepts replacing the
> overlay resolver with a dimension-based model and can tolerate implicit
> composition.

Each overlay declares which **dimension** it belongs to (`service`,
`accelerator`, `intent`, `os`, `platform`). The resolver automatically
selects one overlay per dimension matching the query and merges them in a
fixed order.

**Dimension overlays (no inheritance, no leaf overlay files):**

```
recipes/dimensions/
├── service/
│   ├── eks.yaml          # EBS CSI, EFA, gp2 storage
│   ├── gke.yaml          # GKE gpu-operator, standard-rwo
│   ├── aks.yaml          # Network operator, managed-csi
│   └── kind.yaml         # Dev overrides
├── accelerator/
│   ├── h100-eks.yaml     # cdi+gdrcopy, skyhook tuning
│   ├── h100-gke.yaml     # cdi, NCCL TCPXO
│   ├── h100-aks.yaml     # gdrcopy
│   ├── gb200-eks.yaml    # cdi+gdrcopy, skyhook no-op
│   └── (b200-eks, gb300-eks, etc.)
├── intent/
│   ├── training.yaml     # K8s >= 1.30
│   └── inference.yaml    # K8s >= 1.30, kgateway
├── os/
│   ├── ubuntu.yaml       # Ubuntu 24.04, kernel >= 6.8
│   └── cos.yaml          # (empty — GKE COS is default)
├── platform/
│   ├── kubeflow.yaml     # kubeflow-trainer
│   └── dynamo.yaml       # dynamo-crds, dynamo-platform
└── validation/
    ├── training-full.yaml
    └── conformance-only.yaml
```

**Composition example — query `{eks, h100, training, ubuntu, kubeflow}`:**

```
base
  + service/eks.yaml                 (auto-selected: service=eks)
  + accelerator/h100-eks.yaml        (auto-selected: accelerator=h100, service=eks)
  + intent/training.yaml             (auto-selected: intent=training)
  + os/ubuntu.yaml                   (auto-selected: os=ubuntu)
  + platform/kubeflow.yaml           (auto-selected: platform=kubeflow)
  + validation/training-full.yaml    (auto-selected by convention)
  = merged recipe
```

**No leaf overlay files at all.** The resolver composes automatically.

**File count:** ~20 dimension files + ~5 exception files = ~25 total.

**Tradeoffs:**

| Pro | Con |
|-----|-----|
| Minimum file count (~25) | Largest code change — new resolver model |
| O(S + A) growth instead of O(S x A) | Accelerator files are still per-service |
| No explicit leaf overlays needed | Implicit composition harder to debug |
| Adding any dimension = 1 file | Exceptions model adds complexity |
| Self-documenting structure | Dimension merge ordering is rigid |
| | Cannot express "this combo is invalid" |
| | Testing: must validate all valid combinations |
| | Breaks fundamentally from inheritance model |

## Decision

**TBD** — awaiting team discussion.

**Current recommendation:** Reorder + Mixins now, Reorder + Deep Mixins after merge upgrade is proven.

## Recommendation

**Reorder + Mixins** balances deduplication,
maintainability, and design continuity:

1. **Preserves inheritance** — the core AICR design principle. Leaf overlays
   still specialize through progressively more specific parents.
2. **Eliminates ~75% of duplication** — GPU config shared via reordered
   inheritance tree, OS/platform shared via mixins.
3. **Moderate code change** — ~80 lines in `metadata_store.go` for mixin
   support, backward compatible.
4. **Easy to extend** — new accelerator = 1 parent overlay + thin leaf
   overlays. New OS or platform = 1 mixin file.
5. **Can be phased** — Phase 1 (reorder tree, no code) delivers ~40%.
   Phase 2 (OS + platform mixins) delivers ~75%. Phase 3 (validation
   mixins via Reorder + Deep Mixins, requires merge upgrade) reaches ~90%.

Flat Mixins and Auto-Compose offer higher deduplication but at the cost of
abandoning the inheritance model. Reorder alone is safe but leaves
significant duplication unresolved. Reorder + Deep Mixins is the long-term
target but should only be attempted after Reorder + Mixins is proven stable.

## Implementation Plan

### Phase 1: Reorder Inheritance Tree (no code changes)

1. Create `{accelerator}-{service}` intermediate overlays:
   `h100-eks.yaml`, `h100-gke-cos.yaml`, `h100-aks.yaml`, `gb200-eks.yaml`
2. Move shared GPU operator overrides, skyhook config, and K8s constraints
   from `h100-eks-training.yaml`/`h100-eks-inference.yaml` into `h100-eks.yaml`
3. Re-parent intent leaf overlays: `h100-eks-training.base = h100-eks`
4. Update tests, verify `make kwok-test-all` passes

**Delivers ~40% deduplication with zero code risk.**

**Exit criteria:**
- `make kwok-test-all` passes
- `make test` passes with `-race`
- All existing recipe generation commands produce identical output
- No leaf overlay contains GPU operator overrides that exist in its parent

### Phase 2: Reorder + Mixins (OS + platform only)

1. Define `RecipeMixin` kind and loader in `metadata.go` / `metadata_store.go`
   - Distinct `kind: RecipeMixin` schema
   - Loaded from `recipes/mixins/`, excluded from overlay discovery
   - Allowed fields: `constraints`, `componentRefs` only
2. Update constraint evaluator in `BuildRecipeResultWithEvaluator()` to
   evaluate constraints on the fully merged recipe (including mixin
   constraints), not per-overlay before merge. Current pre-merge evaluation
   would miss mixin-contributed constraints.
3. Extract 3 mixin files:
   - `os-ubuntu.yaml` — Ubuntu 24.04 constraints
   - `platform-kubeflow.yaml` — kubeflow-trainer component
   - `platform-dynamo.yaml` — dynamo-crds, dynamo-platform, K8s >= 1.34
4. Migrate leaf overlays to use `spec.mixins` for OS and platform
5. Add CI lint: no duplicate constraint names across mixins in same leaf overlay

**Delivers ~75% deduplication. Validation stays in inheritance chain.**

**Exit criteria:**
- `make kwok-test-all` passes
- `make test` passes with `-race`
- `golangci-lint` passes on changed packages
- All existing recipe generation commands produce identical output
- No leaf overlay contains Ubuntu constraints or platform components that
  exist in a mixin
- CI lint validates no mixin-vs-mixin conflicts

### Phase 3: Reorder + Deep Mixins (validation mixins, requires merge upgrade)

1. Upgrade `RecipeMetadataSpec.Merge()` to support deep merge for validation
   phases (additive `checks` lists, constraint-level override)
2. Extract validation mixins:
   `validation-training-full.yaml`, `validation-conformance-only.yaml`
3. Migrate remaining leaf overlay validation blocks to mixins
4. Extend constraint evaluator tests to cover validation-mixin constraints

**Delivers ~90% deduplication. Only attempted after Reorder + Mixins
behavior is proven stable in production.**

**Exit criteria:**
- All Phase 2 exit criteria
- Deep-merge produces identical output to current phase-replacement for
  all existing leaf overlays (regression test)
- No leaf overlay contains validation blocks that exist in a mixin

### Risk Table

| Risk | Impact | Mitigation | Phase |
|------|--------|------------|-------|
| Re-parented leaf overlay produces different recipe output | Recipe regression — wrong components deployed | Golden-file tests: compare recipe output before/after for every leaf overlay | 1 |
| Mixin loaded as normal overlay by resolver | Double-application of constraints/components | Distinct `kind: RecipeMixin` schema; loader excludes `recipes/mixins/` | 2 |
| Constraint evaluator misses mixin constraints | OS/platform constraints not validated at build time | Update `BuildRecipeResultWithEvaluator()` to post-merge evaluation; add test | 2 |
| Mixin-vs-mixin field conflict | CI failure / blocked merge | CI lint rejects duplicate constraint names/component names across mixins | 2 |
| Deep-merge validation silently duplicates or drops checks | Wrong conformance/deployment checks in recipe | Phase 3 regression test: compare deep-merge output to current for all leaf overlays | 3 |

### Rollback Strategy

Phase 1 is pure overlay restructuring — revert by restoring the original
overlay files and re-parenting. Phase 2 is backward compatible: if mixin
behavior regresses, remove `spec.mixins` from leaf overlays and inline the
mixin content back into each leaf overlay. The `RecipeMixin` loader and
merge code can remain dormant (no mixins referenced = no code path
exercised). No recipe output format changes, so downstream consumers
(bundler, validator) are unaffected by rollback.

## Consequences

### Positive

- New accelerator/service/OS requires fewer files with less boilerplate
- Single source of truth for shared concerns (Ubuntu constraints, platform
  components)
- Reduced risk of drift between training/inference variants of the same
  accelerator+service

### Negative

- Two composition mechanisms to understand (inheritance + mixins)
- Mixin conflict policy must be documented and enforced via CI lint
- Migration effort for existing 38 leaf overlays

### Neutral

- Total file count stays roughly the same (~42 + 3 mixins) but per-file
  content shrinks significantly
- KWOK test count unchanged (still one test per leaf overlay)

## References

- [Issue #305: Refactor overlay system to reduce training/inference redundancy](https://github.com/NVIDIA/aicr/issues/305)
- [ADR-003: Scaling KWOK Recipe Tests](003-scaling-recipe-tests.md)
- `RecipeMetadataSpec.Merge()` in `pkg/recipe/metadata.go` — component/constraint merge semantics
- `BuildRecipeResultWithEvaluator()` in `pkg/recipe/metadata_store.go` — overlay selection, constraint evaluation, and merge logic
