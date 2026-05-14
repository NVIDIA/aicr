# Data Architecture

This document describes the recipe metadata system used by the CLI and API to generate optimized system configuration recommendations (i.e. recipes) based on environment parameters.

## Overview

The recipe system is a rule-based configuration engine that generates tailored system configurations by:

1. **Starting with a base recipe** - Universal component definitions and constraints applicable to every recipe
2. **Matching environment-specific overlays** - Targeted configurations based on query criteria (service, accelerator, OS, intent)
3. **Resolving inheritance chains** - Overlays can inherit from intermediate recipes to reduce duplication
4. **Merging configurations** - Components, constraints, and values are merged with overlay precedence
5. **Computing deployment order** - Topological sort of components based on dependency references

The recipe data is organized in [`recipes/`](https://github.com/NVIDIA/aicr/tree/main/recipes) as multiple YAML files:

```
recipes/
├── registry.yaml                  # Component registry (Helm & Kustomize configs)
├── overlays/                      # Recipe overlays (including base)
│   ├── base.yaml                  # Root recipe - all recipes inherit from this
│   ├── eks.yaml                   # EKS-specific settings
│   ├── eks-training.yaml          # EKS + training workloads (inherits from eks)
│   ├── gb200-eks-ubuntu-training.yaml # GB200/EKS/Ubuntu/training (inherits from eks-training)
│   └── h100-ubuntu-inference.yaml # H100/Ubuntu/inference
├── mixins/                        # Composable mixin fragments (kind: RecipeMixin)
│   ├── os-ubuntu.yaml             # Ubuntu OS constraints (shared by leaf overlays)
│   ├── platform-inference.yaml    # Inference gateway components (shared by service-inference overlays)
│   └── platform-kubeflow.yaml     # Kubeflow trainer component (shared by leaf overlays)
└── components/                    # Component values files
    ├── cert-manager/
    │   └── values.yaml
    ├── gpu-operator/
    │   ├── values.yaml            # Base GPU Operator values
    │   └── values-eks-training.yaml # EKS training-optimized values
    ├── network-operator/
    │   └── values.yaml
    ├── nvidia-dra-driver-gpu/
    │   └── values.yaml
    ├── nvsentinel/
    │   └── values.yaml
    └── nodewright-operator/
        └── values.yaml
```

> Note: These files are embedded into both the CLI binary and API server at compile time, making the system fully self-contained with no external dependencies.
>
> **Extensibility**: The embedded data can be extended or overridden using the `--data` flag. See [External Data Provider](#external-data-provider) for details.

**Recipe Usage Patterns:**

1. **CLI Query Mode** - Direct recipe generation from criteria parameters:
   ```bash
   aicr recipe --os ubuntu --accelerator h100 --service eks --intent training
   ```

2. **CLI Snapshot Mode** - Analyze captured system state to infer criteria:
   ```bash
   aicr snapshot --output system.yaml
   aicr recipe --snapshot system.yaml --intent training
   ```

3. **API Server** - HTTP endpoint (query mode only):
   ```bash
   curl "http://localhost:8080/v1/recipe?os=ubuntu&accelerator=h100&service=eks&intent=training"
   ```

## Data Structure

### Recipe Metadata Format

Each recipe file follows this structure:

```yaml
kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: <recipe-name>  # Unique identifier (e.g., "eks-training", "gb200-eks-ubuntu-training")

spec:
  base: <parent-recipe>  # Optional - inherits from another recipe
  mixins:                # Optional - composable mixin fragments
    - os-ubuntu          #   OS constraints (from recipes/mixins/)
    - platform-kubeflow  #   Platform components (from recipes/mixins/)
  
  criteria:              # When this recipe/overlay applies
    service: eks         # Kubernetes platform
    accelerator: gb200   # GPU type
    os: ubuntu           # Operating system
    intent: training     # Workload purpose
    platform: kubeflow    # Platform/framework (optional)
  
  constraints:           # Deployment requirements
    - name: K8s.server.version
      value: ">= 1.32"
  
  componentRefs:         # Components to deploy
    - name: gpu-operator
      type: Helm
      source: https://helm.ngc.nvidia.com/nvidia
      version: v25.3.3
      valuesFile: components/gpu-operator/values.yaml
      dependencyRefs:
        - cert-manager
```

### Top-Level Fields

| Field | Description |
|-------|-------------|
| `kind` | Always `recipeMetadata` |
| `apiVersion` | Always `aicr.nvidia.com/v1alpha1` |
| `metadata.name` | Unique recipe identifier |
| `spec.base` | Parent recipe to inherit from (empty = inherits from `overlays/base.yaml`) |
| `spec.mixins` | List of mixin names to compose (e.g., `["os-ubuntu", "platform-kubeflow"]`) |
| `spec.criteria` | Query parameters that select this recipe |
| `spec.constraints` | Pre-flight validation rules |
| `spec.componentRefs` | List of components to deploy |

### Criteria Fields

Criteria define when a recipe matches a user query:

| Field | Type | Description | Example Values |
|-------|------|-------------|----------------|
| `service` | String | Kubernetes platform | `eks`, `gke`, `aks`, `oke`, `kind`, `lke` |
| `accelerator` | String | GPU hardware type | `h100`, `gb200`, `b200`, `a100`, `l40`, `rtx-pro-6000` |
| `os` | String | Operating system | `ubuntu`, `rhel`, `cos`, `amazonlinux` |
| `intent` | String | Workload purpose | `training`, `inference` |
| `platform` | String | Platform/framework type | `kubeflow` |
| `nodes` | Integer | Node count (0 = any) | `8`, `16` |

**All fields are optional.** Unpopulated fields act as wildcards (match any value).

### Constraint Format

Constraints use fully qualified measurement paths:

```yaml
constraints:
  - name: K8s.server.version    # {type}.{subtype}.{key}
    value: ">= 1.32"            # Expression or exact value
  
  - name: OS.release.ID
    value: ubuntu               # Exact match
  
  - name: OS.release.VERSION_ID
    value: "24.04"
  
  - name: OS.sysctl./proc/sys/kernel/osrelease
    value: ">= 6.8"
```

**Constraint Path Format:** `{MeasurementType}.{Subtype}.{Key}`

| Measurement Type | Common Subtypes |
|------------------|-----------------|
| `K8s` | `server`, `image`, `config` |
| `OS` | `release`, `sysctl`, `kmod`, `grub` |
| `GPU` | `smi`, `driver`, `device` |
| `SystemD` | `containerd.service`, `kubelet.service` |

**Supported Operators:** `>=`, `<=`, `>`, `<`, `==`, `!=`, or exact match (no operator)

### Component Reference Structure

Each component in `componentRefs` defines a deployable unit. Components can be either Helm or Kustomize based.

**Helm Component Example:**

```yaml
componentRefs:
  - name: gpu-operator           # Component identifier (must match registry name)
    type: Helm                   # Deployment type
    source: https://helm.ngc.nvidia.com/nvidia  # Helm repository URL
    version: v25.3.3             # Chart version
    valuesFile: components/gpu-operator/values.yaml  # Path to values file
    overrides:                   # Inline value overrides
      driver:
        version: 580.82.07
      cdi:
        enabled: true
    dependencyRefs:              # Components this depends on
      - cert-manager
```

**Kustomize Component Example:**

```yaml
componentRefs:
  - name: my-kustomize-app       # Component identifier (must match registry name)
    type: Kustomize              # Deployment type
    source: https://github.com/example/my-app  # Git repository or OCI reference
    tag: v1.0.0                  # Git tag, branch, or commit
    path: deploy/production      # Path to kustomization within repo
    patches:                     # Patch files to apply
      - patches/custom-patch.yaml
    dependencyRefs:
      - cert-manager
```

#### Component Fields

| Field | Required | Description |
|-------|----------|-------------|
| `name` | Yes | Unique component identifier (matches registry name) |
| `type` | Yes | `Helm` or `Kustomize` |
| `source` | Yes | Repository URL, OCI reference, or Git URL |
| `version` | No | Chart version (for Helm) |
| `tag` | No | Git tag, branch, or commit (for Kustomize) |
| `path` | No | Path to kustomization within repository (for Kustomize) |
| `valuesFile` | No | Path to values file (relative to data directory, for Helm) |
| `overrides` | No | Inline values that override valuesFile (for Helm) |
| `patches` | No | Patch files to apply (for Kustomize) |
| `dependencyRefs` | No | List of component names this depends on |

## Multi-Level Inheritance

Recipe files support **multi-level inheritance** through the `spec.base` field. This enables building inheritance chains where intermediate recipes capture shared configurations, reducing duplication and improving maintainability.

### Inheritance Mechanism

Each recipe can specify a parent recipe via `spec.base`:

```yaml
kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: gb200-eks-ubuntu-training

spec:
  base: eks-training  # Inherits from eks-training recipe
  
  criteria:
    service: eks
    accelerator: gb200
    os: ubuntu
    intent: training
    
  # Only GB200-specific overrides here
  componentRefs:
    - name: gpu-operator
      version: v25.3.3
      overrides:
        driver:
          version: 580.82.07
```

### Inheritance Chain Example

The system supports inheritance chains of arbitrary depth:

```
overlays/base.yaml
    │
    ├── overlays/eks.yaml (spec.base: empty → inherits from base)
    │       │
    │       └── overlays/eks-training.yaml (spec.base: eks)
    │               │
    │               └── overlays/gb200-eks-training.yaml (spec.base: eks-training)
    │                       │
    │                       └── overlays/gb200-eks-ubuntu-training.yaml (spec.base: gb200-eks-training)
    │
    └── overlays/h100-ubuntu-inference.yaml (spec.base: empty → inherits from base)
```

**Resolution Order:** When resolving `gb200-eks-ubuntu-training`:
1. Start with `overlays/base.yaml` (root)
2. Merge `overlays/eks.yaml` (EKS-specific settings)
3. Merge `overlays/eks-training.yaml` (training optimizations)
4. Merge `overlays/gb200-eks-training.yaml` (GB200 + training-specific overrides)
5. Merge `overlays/gb200-eks-ubuntu-training.yaml` (Ubuntu + full-spec overrides)

### Inheritance Rules

**1. Base Resolution**
- `spec.base: ""` or omitted → Inherits directly from `overlays/base.yaml`
- `spec.base: "eks"` → Inherits from the recipe named "eks"
- The root `overlays/base.yaml` has no parent (it's the foundation)

**2. Merge Precedence**
Later recipes in the chain override earlier ones:
```
base → eks → eks-training → gb200-eks-training → gb200-eks-ubuntu-training
(lowest)                                            (highest priority)
```

**3. Field Merging**
- **Constraints**: Same-named constraints are overridden; new constraints are added
- **ComponentRefs**: Same-named components are merged field-by-field; new components are added
- **Criteria**: Each recipe defines its own criteria (not inherited)

### Intermediate vs Leaf Recipes

**Intermediate Recipes** (e.g., `eks.yaml`, `eks-training.yaml`):
- Have **partial criteria** (not all fields specified)
- Capture shared configurations for a category
- Can be matched by user queries (but typically less specific)

**Leaf Recipes** (e.g., `gb200-eks-ubuntu-training.yaml`):
- Have **complete criteria** (all required fields)
- Matched by specific user queries
- Contain final, hardware-specific overrides

### Example: Inheritance Chain

```yaml
# overlays/base.yaml - Foundation for all recipes
kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: base

spec:
  constraints:
    - name: K8s.server.version
      value: ">= 1.25"

  componentRefs:
    - name: cert-manager
      type: Helm
      source: https://charts.jetstack.io
      version: v1.20.2
      valuesFile: components/cert-manager/values.yaml

    - name: gpu-operator
      type: Helm
      source: https://helm.ngc.nvidia.com/nvidia
      version: v25.10.1
      valuesFile: components/gpu-operator/values.yaml
      dependencyRefs:
        - cert-manager
```

```yaml
# eks.yaml - EKS-specific settings
kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: eks

spec:
  # Implicit base (inherits from overlays/base.yaml)
  
  criteria:
    service: eks  # Only service specified (partial criteria)

  constraints:
    - name: K8s.server.version
      value: ">= 1.28"  # EKS minimum version
```

```yaml
# eks-training.yaml - EKS training workloads
kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: eks-training

spec:
  base: eks  # Inherits EKS settings
  
  criteria:
    service: eks
    intent: training  # Added training intent (still partial)

  constraints:
    - name: K8s.server.version
      value: ">= 1.30"  # Training requires newer K8s

  componentRefs:
    # Training workloads use training-optimized values
    - name: gpu-operator
      valuesFile: components/gpu-operator/values-eks-training.yaml
```

### Benefits of Multi-Level Inheritance

| Benefit | Description |
|---------|-------------|
| **Reduced Duplication** | Shared settings defined once in intermediate recipes |
| **Easier Maintenance** | Update EKS settings in one place, all EKS recipes inherit |
| **Clear Organization** | Hierarchy reflects logical relationships |
| **Flexible Extension** | Add new leaf recipes without duplicating parent configs |
| **Testable** | Each level can be validated independently |

### Mixin Composition

Inheritance is single-parent (`spec.base`), which means cross-cutting concerns like OS constraints or platform components would need to be duplicated across leaf overlays. **Mixins** solve this by providing composable fragments that leaf overlays reference via `spec.mixins`.

Mixin files live in `recipes/mixins/` and use `kind: RecipeMixin`:

```yaml
# recipes/mixins/os-ubuntu.yaml
kind: RecipeMixin
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: os-ubuntu

spec:
  constraints:
    - name: OS.release.ID
      value: ubuntu
    - name: OS.release.VERSION_ID
      value: "24.04"
    - name: OS.sysctl./proc/sys/kernel/osrelease
      value: ">= 6.8"
```

Leaf overlays compose mixins alongside inheritance:

```yaml
# recipes/overlays/h100-eks-ubuntu-training-kubeflow.yaml
spec:
  base: h100-eks-training
  mixins:
    - os-ubuntu          # Ubuntu constraints
    - platform-kubeflow  # Kubeflow trainer component
  criteria:
    service: eks
    accelerator: h100
    os: ubuntu
    intent: training
    platform: kubeflow
```

**Mixin rules:**
- Mixins carry only `constraints` and `componentRefs` — no `criteria`, `base`, `mixins`, or `validation`
- Mixins are applied after inheritance chain merging but before constraint evaluation
- Conflict detection: a mixin constraint or component that conflicts with the inheritance chain or a previously applied mixin produces an error
- When a snapshot is provided, mixin constraints are evaluated against it after merging; if any fail, the entire composed candidate is invalid and falls back to base-only output. In plain query mode (no snapshot), mixin constraints are merged but not evaluated

### Criteria-Wildcard Overlays

Some overlays apply across a criteria dimension without being referenced via `spec.base` or included via `spec.mixins`. The resolver picks them up automatically because `FindMatchingOverlays` can return multiple independent maximal-leaf overlays for a single query, not just one. Ancestors of a matched leaf are filtered out of the candidate set, but sibling leaves whose criteria independently match are kept and their inheritance chains are resolved and merged in parallel. See [Criteria Matching Algorithm](#criteria-matching-algorithm) and [Recipe Generation Process](#recipe-generation-process) for details.

This is useful for content that cross-cuts one criteria dimension but must stay tied to others — for example, a GB200 NCCL bandwidth target that applies to every service (EKS, OKE, etc.) but only for GB200 + training.

```yaml
# recipes/overlays/gb200-any-training.yaml
spec:
  base: base
  criteria:
    service: any         # Wildcard — matches eks, oke, gke, etc.
    accelerator: gb200
    intent: training
  validation:
    performance:
      checks:
        - nccl-all-reduce-bw
      constraints:
        - name: nccl-all-reduce-bw
          value: ">= 720"
```

When a query specifies `{service: eks, accelerator: gb200, intent: training}`, the resolver returns three maximal leaves — `gb200-eks-training` (matched by explicit criteria), `gb200-any-training` (matched by wildcard `service: any`), and `monitoring-hpa` (matched by wildcard `intent: any`). Their inheritance chains are resolved and merged with the base spec:

```yaml
appliedOverlays:
  - base
  - monitoring-hpa
  - gb200-any-training      # matched by wildcard criteria, not via base:
  - eks
  - eks-training
  - gb200-eks-training
```

The `nccl-all-reduce-bw` constraint from `gb200-any-training` lands in the hydrated recipe without being duplicated in each service-specific overlay. (Adding `os: ubuntu` to the query would extend the chain with `gb200-eks-ubuntu-training` as the maximal leaf in place of `gb200-eks-training`; `gb200-any-training` would still match independently.)

**Naming convention.** The `-any-` segment signals this pattern: the static segments indicate the fixed criteria dimensions (accelerator, intent), and `any` marks the wildcard dimension. Examples: `gb200-any-training.yaml`, `b200-any-training.yaml`.

**When to use a criteria-wildcard overlay vs a mixin:**

| Use a criteria-wildcard overlay when... | Use a mixin when... |
|---|---|
| Content applies based on query criteria | Content applies based on explicit opt-in |
| The set of consumers is determined by criteria matching | The set of consumers is an enumerated list of overlays |
| Adopt-by-default is desired for new matching overlays | Each consumer should reference it explicitly |
| You want to add `validation` blocks (mixins don't carry validation) | You only need `constraints` / `componentRefs` |

**Precedence when a wildcard overlay and a service-specific leaf collide.** `FindMatchingOverlays` sorts its returned leaves by `Criteria.Specificity()` ascending, so less-specific overlays merge first and more-specific overlays merge last. Two different merge rules apply — they are not the same:

- **Top-level `spec.constraints`** merge by name. A same-named constraint from the more-specific leaf overrides the wildcard's value (the "overridden, new added" rule from the merge algorithm).
- **`spec.validation.<phase>`** blocks (deployment, performance, conformance) are **replaced wholesale** when a later overlay defines the same phase — no field-level merge. The leaf's `checks` and `constraints` replace the wildcard's entire block.

This distinction matters. To override only the threshold in the wildcard example above, a service-specific leaf must restate **both** `checks` and `constraints`:

```yaml
# recipes/overlays/gb200-eks-training.yaml
spec:
  validation:
    performance:
      checks:                        # Must restate — else the phase is dropped
        - nccl-all-reduce-bw
      constraints:
        - name: nccl-all-reduce-bw
          value: ">= 650"            # EKS-specific threshold
```

Setting only `constraints` drops the wildcard's `checks`, which causes `filterEntriesByRecipe` to return zero entries and the performance phase to be skipped entirely — the opposite of the "lower the threshold" intent.

Criteria-wildcard overlays are only appropriate when the content is genuinely uniform across the wildcard dimension. If the value diverges (e.g., H100 NCCL targets differ by cloud: AKS ≥ 100, EKS ≥ 300, GKE ≥ 250), keep it inline in each service-specific overlay — collapsing divergent values to a lowest-common-denominator wildcard silently weakens validation.

**See also:** [ADR-005: Overlay Refactoring](../design/005-overlay-refactoring.md) — rationale for the maximal-leaf resolver semantics (Phase 2) and why wildcard overlays are preferred over multi-parent inheritance or intermediate-reparenting approaches that were prototyped and rejected.

### Cycle Detection

The system detects circular inheritance to prevent infinite loops:

```yaml
# INVALID: Would create cycle
# a.yaml: spec.base: b
# b.yaml: spec.base: c
# c.yaml: spec.base: a  ← Cycle detected!
```

Tests in `pkg/recipe/yaml_test.go` automatically validate:
- All `spec.base` references point to existing recipes
- No circular inheritance chains exist
- Inheritance depth is reasonable (max 10 levels)

## Component Configuration

Components are configured through a three-tier system with clear precedence.

### Configuration Patterns

**Pattern 1: ValuesFile Only**
Traditional approach - all values in a separate file:
```yaml
componentRefs:
  - name: gpu-operator
    valuesFile: components/gpu-operator/values.yaml
```

**Pattern 2: Overrides Only**
Fully self-contained recipe with inline values:
```yaml
componentRefs:
  - name: gpu-operator
    overrides:
      driver:
        version: 580.82.07
      cdi:
        enabled: true
```

**Pattern 3: ValuesFile + Overrides (Hybrid)**
Reusable base with recipe-specific tweaks:
```yaml
componentRefs:
  - name: gpu-operator
    valuesFile: components/gpu-operator/values.yaml  # Base configuration
    overrides:                                        # Recipe-specific tweaks
      driver:
        version: 580.82.07
```

### Value Merge Precedence

When values are resolved, merge order is:

```
Base ValuesFile → Overlay ValuesFile → Overlay Overrides → CLI --set flags
     (lowest)                                                   (highest)
```

1. **Base ValuesFile**: Values from inherited recipes
2. **Overlay ValuesFile**: Values file specified in the matching overlay
3. **Overlay Overrides**: Inline `overrides` in the overlay's componentRef
4. **CLI --set flags**: Runtime overrides from `aicr bundle --set`

### Component Values Files

Values files are stored in `recipes/components/{component}/`:

```yaml
# components/gpu-operator/values.yaml
operator:
  upgradeCRD: true
  resources:
    limits:
      cpu: 500m
      memory: 700Mi

driver:
  version: 580.105.08
  enabled: true
  useOpenKernelModules: true
  rdma:
    enabled: true

devicePlugin:
  enabled: true
```

### Dependency Management

Components can declare dependencies via `dependencyRefs`:

```yaml
componentRefs:
  - name: cert-manager
    type: Helm
    version: v1.20.2

  - name: gpu-operator
    type: Helm
    version: v25.10.1
    dependencyRefs:
      - cert-manager  # Deploy cert-manager first
```

The system performs **topological sort** to compute deployment order, ensuring dependencies are deployed before dependents. The resulting order is exposed in `RecipeResult.DeploymentOrder`.

### Regenerating the BOM

`docs/user/container-images.md` is an auto-generated bill of materials listing every container image AICR pulls across all components. It is regenerated by `make bom-docs`, which renders each Helm chart against its live OCI source and extracts image references from the rendered templates.

**Run `make bom-docs` and commit the regenerated `docs/user/container-images.md` in the same PR whenever you:**

- Add or remove a component in `recipes/registry.yaml`
- Bump a chart version (in `registry.yaml`, an overlay, or a mixin)
- Modify a component's `values.yaml` in a way that changes which images render (image repo override, subchart enable/disable, etc.)

The regen can also surface drift from *upstream* chart updates — when a chart bumps an image inside its own templates without a registry pin change on our side. That drift will appear in the BOM diff whether you expected it or not. Land it as part of the same PR that triggered the regen, or split it out as a separate "BOM catch-up" PR if the unrelated diff would obscure the primary change.

**Freshness is not gated.** `make bom-check` verifies the committed `docs/user/container-images.md` matches a fresh regen, but it is opt-in — neither `make qualify` nor `make lint` runs it today, and the merge gate has no PR-time BOM-staleness check (it only runs `bom-pinning-check`, which is the chart-pin verification per ADR-006). Run `make bom-docs` explicitly whenever you touch a component; do not rely on local qualify or CI to catch a missed regen. Wiring `bom-check` into the gate is a desirable follow-up.

## Criteria Matching Algorithm

The recipe system uses an **asymmetric rule matching algorithm** where recipe criteria (rules) match against user queries (candidates).

### Matching Rules

A recipe's criteria matches a user query when **every non-"any" field in the criteria is satisfied by the query**:

1. **Empty/unpopulated fields in recipe criteria** = Wildcard (matches any query value)
2. **Populated fields must match exactly** (case-insensitive)
3. **Matching is asymmetric**: A recipe with specific fields (e.g., `accelerator: h100`) will NOT match a generic query (e.g., `accelerator: any`)

### Asymmetric Matching Explained

The key insight is that matching is **one-directional**:

- **Recipe "any"** (or empty) → Matches ANY query value (acts as wildcard)
- **Query "any"** → Only matches recipe "any" (does NOT match specific recipes)

This prevents overly specific recipes from being selected when the user hasn't specified those criteria.

### Matching Logic

```go
// Asymmetric matching: recipe criteria as receiver, query as parameter
func (c *Criteria) Matches(other *Criteria) bool {
    // If recipe (c) is "any" (or empty), it matches any query value (wildcard).
    // If query (other) is "any" but recipe is specific, it does NOT match.
    // If both have specific values, they must match exactly.
    
    // For each field, call matchesCriteriaField(recipeValue, queryValue)
    // ...
    return true
}

// matchesCriteriaField implements asymmetric matching for a single field.
func matchesCriteriaField(recipeValue, queryValue string) bool {
    recipeIsAny := recipeValue == "any" || recipeValue == ""
    queryIsAny := queryValue == "any" || queryValue == ""

    // If recipe is "any", it matches any query value (recipe is generic)
    if recipeIsAny {
        return true
    }

    // Recipe has a specific value
    // Query must also have that specific value (not "any")
    if queryIsAny {
        // Query is generic but recipe is specific - no match
        return false
    }

    // Both have specific values - must match exactly
    return recipeValue == queryValue
}
```

### Specificity Scoring

When multiple recipes match, they are sorted by **specificity** (number of non-"any" fields). More specific recipes are applied later, giving them higher precedence:

```go
func (c *Criteria) Specificity() int {
    score := 0
    if c.Service != "any" { score++ }
    if c.Accelerator != "any" { score++ }
    if c.Intent != "any" { score++ }
    if c.OS != "any" { score++ }
    if c.Nodes != 0 { score++ }
    return score
}
```

### Matching Examples

**Example 1: Broad Recipe Matches Specific Query**
```yaml
Recipe Criteria: { service: "eks" }
User Query:      { service: "eks", os: "ubuntu", accelerator: "h100" }
Result:          ✅ MATCH - Recipe only requires service=eks, other fields are wildcards
Specificity:     1
```

**Example 2: Specific Recipe Doesn't Match Different Specific Query**
```yaml
Recipe Criteria: { service: "eks", accelerator: "gb200", intent: "training" }
User Query:      { service: "eks", os: "ubuntu", accelerator: "h100" }
Result:          ❌ NO MATCH - Accelerator doesn't match (gb200 ≠ h100)
```

**Example 3: Specific Recipe Doesn't Match Generic Query (Asymmetric)**
```yaml
Recipe Criteria: { service: "eks", accelerator: "gb200", intent: "training" }
User Query:      { service: "eks", intent: "training" }  # accelerator unspecified = "any"
Result:          ❌ NO MATCH - Recipe requires gb200, query has "any" (wildcard doesn't match specific)
```

This asymmetric behavior ensures that a generic query like `--service eks --intent training` only matches generic recipes, not hardware-specific ones like `gb200-eks-training.yaml`.

**Example 4: Multiple Maximal Matches (Fully Specific Query)**

```yaml
User Query: { service: "eks", os: "ubuntu", accelerator: "gb200", intent: "training" }

Overlay criteria matches (pre-filter):
  1. overlays/monitoring-hpa.yaml             { intent: any }                                           Specificity: 0
  2. overlays/eks.yaml                        { service: eks }                                          Specificity: 1
  3. overlays/eks-training.yaml               { service: eks, intent: training }                        Specificity: 2
  4. overlays/gb200-any-training.yaml         { service: any, accelerator: gb200, intent: training }    Specificity: 2
  5. overlays/gb200-eks-training.yaml         { service: eks, accelerator: gb200, intent: training }    Specificity: 3
  6. overlays/gb200-eks-ubuntu-training.yaml  { service: eks, accelerator: gb200, os: ubuntu, intent: training }  Specificity: 4

(base.yaml is the root spec, not an overlay candidate: FindMatchingOverlays
iterates s.Overlays only. The base spec is always applied as the seed for
the merged output — it is not selected by criteria matching.)

Maximal leaves (after filterToMaximalLeaves):
  - monitoring-hpa             (no matching descendant)
  - gb200-any-training         (no matching descendant)
  - gb200-eks-ubuntu-training  (most-specific overlay; eks, eks-training,
                                gb200-eks-training are ancestors and are filtered out)

Result: Each maximal leaf's inheritance chain is resolved and merged onto
the base spec. Ancestors removed by the filter re-enter the output via
chain resolution (step 3), so the final appliedOverlays is
[base, monitoring-hpa, gb200-any-training, eks, eks-training,
gb200-eks-training, gb200-eks-ubuntu-training].
```

Note that multiple maximal leaves can coexist when their inheritance chains are independent — `gb200-any-training` (via wildcard `service: any`) and `gb200-eks-ubuntu-training` (via explicit criteria) are both kept because neither is an ancestor of the other. This is what enables the [criteria-wildcard overlay pattern](#criteria-wildcard-overlays).

## Cluster Fingerprint

`aicr snapshot` emits a structured `fingerprint:` block alongside the raw
measurements. The fingerprint is a normalized, schema-stable view of the
cluster-identity dimensions used to bind a snapshot to a recipe — service,
accelerator, OS, Kubernetes server version, region, total node count, and
GPU node count — so an evidence bundle (per
[ADR-007](../design/007-recipe-evidence.md)) can prove the recipe was
tested on hardware matching its declared criteria.

The fingerprint is derived from the same collector outputs that populate
`measurements:`; it is not a separate collection pass. Dimensions whose
source signal is missing surface as zero-value entries — the verifier
treats those as "unknown" rather than fabricating a match.

> **The persisted `fingerprint:` block is advisory only.** It is a
> convenience for humans reading the snapshot YAML, not a trust-bearing
> claim. The snapshot file is not signed at this layer — an attacker
> controlling it could swap the embedded fingerprint without touching
> the measurements that back it. Trust-bearing consumers — the
> evidence bundler ([#754](https://github.com/NVIDIA/aicr/issues/754)),
> the verifier ([#753](https://github.com/NVIDIA/aicr/issues/753)),
> and any downstream policy gate — MUST recompute via
> `fingerprint.FromMeasurements(snap.Measurements)` before acting on
> the result, and treat zero-value entries as "unknown" per the match
> semantics below.

### Fingerprint Schema

```yaml
fingerprint:
  service:
    value: eks                       # eks | gke | aks | oke | kind | lke
    source: k8s.node.provider
  accelerator:
    value: h100                      # h100 | gb200 | b200 | a100 | l40 | rtx-pro-6000
    source: gpu.smi.gpu.model
  os:
    value: ubuntu                    # ubuntu | rhel | cos | amazonlinux | talos
    version: "22.04"                 # raw VERSION_ID for audit; not in criteria
    source: os.release
  k8sVersion:
    value: "1.33.4"                  # leading "v" stripped
    source: k8s.server.version
  region:                            # value empty when multi-region or no label
    value: us-west-2
    source: nodeTopology.label.topology.kubernetes.io/region
  nodeCount:                         # all nodes including control plane
    value: 12
    source: nodeTopology.summary.node-count
  gpuNodeCount:                      # nodes carrying the GPU operator label
    value: 8
    source: nodeTopology.label.nvidia.com/gpu.product
```

#### Heterogeneous and stale-registry dimensions

When `accelerator` or `region` cannot be collapsed to a single Value,
the fingerprint surfaces the reason via an optional `note:` field
instead of fabricating one. The verifier renders this distinct from
"value not captured" in its Markdown output. Three notes are emitted
today:

- `multi-region` — nodes carry different `topology.kubernetes.io/region` labels
- `multi-gpu` — nodes carry different `nvidia.com/gpu.product` labels
- `unknown-sku` — nvidia-smi or the GPU operator reported a product
  string that is not in the recipe accelerator registry (likely
  registry staleness; the raw model is still recoverable from the
  underlying measurement)

```yaml
fingerprint:
  accelerator:                       # nodes disagree on GPU SKU
    value: ""
    source: nodeTopology.label.nvidia.com/gpu.product
    note: multi-gpu
  region:                            # nodes disagree on region
    value: ""
    source: nodeTopology.label.topology.kubernetes.io/region
    note: multi-region
  # Or, for an unrecognized SKU:
  # accelerator:
  #   value: ""
  #   source: gpu.smi.gpu.model
  #   note: unknown-sku
```

Every dimension carries a `value` (the resolved, normalized string the
recipe `criteria` block can be compared against), a `source` string
identifying which collector signal produced it, and an optional `note`
string carrying a short audit hint when Value is empty for a reason
other than missing data (the cases above). ADR-007 reserves additional
optional fields (`signals[]`, `confidence`) for a future multi-signal
corroboration extension; V1 records `source` and `note` only.

### Detection Sources

| Dimension | Source | Normalization |
|-----------|--------|---------------|
| `service` | `k8s.node.provider` (parsed from `spec.providerID`) | `aws → eks`, `gce → gke`, `azure → aks`, `oci → oke`, else passthrough |
| `accelerator` | `gpu.smi.gpu.model` (nvidia-smi `ProductName`) | Substring match against the recipe accelerator enum (`GB200` matched before `B200`) |
| `os.value` | `/etc/os-release` `ID` | Mapped to the `oskind` enum; aliases like `redhat → rhel` and `al2 → amazonlinux` are recognized |
| `os.version` | `/etc/os-release` `VERSION_ID` | Retained verbatim for audit |
| `k8sVersion` | `k8s.server.version` | Leading `v` stripped |
| `region` | `nodeTopology.label.topology.kubernetes.io/region` | Single-region clusters surface the value; multi-region clusters surface `note: multi-region` with empty value |
| `nodeCount` | `nodeTopology.summary.node-count` | All nodes, control plane included |
| `gpuNodeCount` | `nodeTopology.label.nvidia.com/gpu.product` | Union of nodes across one or more GPU-product label entries (the canonical GPU-operator presence signal); zero when no GPU operator labels are present |
| `accelerator` (cluster-wide override) | same as `gpuNodeCount` | When the topology label data shows multiple distinct GPU SKUs across nodes, accelerator surfaces `note: multi-gpu` with empty value — preferring honesty over the smi reading from a single node |

A dimension whose source signal is missing keeps its zero value. The
verifier reports it as `unknown` rather than mismatched.

### Match Semantics

`fingerprint.Fingerprint.Match` compares a fingerprint against a
recipe's criteria and returns a per-dimension diff plus an overall
`matched` flag. Each criteria dimension resolves to one of three
outcomes:

- **`matched`** — the recipe is generic (`any` / empty) for this
  dimension, OR the fingerprint captured the same value the recipe
  requires.
- **`mismatched`** — the recipe requires a specific value and the
  fingerprint captured a different specific value.
- **`unknown`** — the recipe requires a specific value but the
  fingerprint cannot prove or disprove it. Two cases produce
  `unknown`: a dimension the cluster does not reveal (`intent`,
  `platform` — recipe-author choices) and a dimension the
  fingerprint failed to detect (e.g., no GPU collector output).

The overall `matched` flag is `true` when no dimension is `mismatched`.
Unknowns surface in the per-dimension diff for human review without
flipping the overall outcome — the fingerprint cannot disprove a
match it does not capture.

### Worked Example

Recipe criteria: `service=eks, accelerator=h100, intent=training, os=ubuntu, platform=kubeflow`
plus the fingerprint above.

```yaml
matched: true
perDimension:
- {dimension: service,     recipeRequires: eks,      fingerprintProvides: eks,    match: matched}
- {dimension: accelerator, recipeRequires: h100,     fingerprintProvides: h100,   match: matched}
- {dimension: os,          recipeRequires: ubuntu,   fingerprintProvides: ubuntu, match: matched}
- {dimension: intent,      recipeRequires: training,                              match: unknown}
- {dimension: platform,    recipeRequires: kubeflow,                              match: unknown}
- {dimension: nodes,                                 fingerprintProvides: 12,     match: matched}
```

`perDimension` is an ordered list so iteration is deterministic and
serialization is byte-stable; consumers needing lookup by name use
`MatchResult.Find` rather than indexing.

The bundle's predicate body (per [ADR-007](../design/007-recipe-evidence.md)
PR-A / #754) records this diff as `criteriaMatch.perDimension`; the
verifier (#753) renders it in a Markdown summary so the maintainer
sees exactly which dimensions the fingerprint corroborated.

The predicate body preserves the three-way `match:` state verbatim
(`matched | mismatched | unknown`) rather than collapsing to a bool.
The ADR-007 example shows `match: true` for the happy-path case where
every dimension is matched, but the schema must keep `unknown`
distinguishable from `matched` — a maintainer reviewing a bundle
needs to see "intent and platform were not corroborated by the
fingerprint" rather than "everything matched." A CI gate keyed on
`criteriaMatch.matched: true` alone gives unknown dimensions a free
pass; gates that need stronger guarantees should also assert that
no per-dimension entry has `match: unknown`.

## Recipe Generation Process

The recipe builder (`pkg/recipe/metadata_store.go`) generates recipes through the following steps:

### Step 1: Load Metadata Store

```go
store, err := loadMetadataStore(ctx)
```

- Embedded YAML files are parsed into Go structs
- Cached in memory on first access (singleton pattern with `sync.Once`)
- Contains base recipe, all overlays, mixins, and component values files

### Step 2: Find Matching Overlays

```go
overlays := store.FindMatchingOverlays(criteria)
```

- Iterate all overlays in `s.Overlays` (the base recipe is held separately in `s.Base` and is not a candidate here — it is injected as the merge seed by `initBaseMergedSpec()` in Step 4)
- Check if each overlay's criteria matches the user query
- **Filter to maximal leaves** via `filterToMaximalLeaves()`: drop any match that is an ancestor (via `spec.base`) of another match. Ancestors are re-added later via chain resolution; this filter ensures that a matched descendant isn't double-counted with its own chain
- Sort maximal-leaf matches by specificity (least specific first)

Multiple maximal leaves can be returned for one query when they sit on independent inheritance chains — for example, a `service: any` wildcard overlay and the most-specific service-specific leaf are both kept (see [Criteria-Wildcard Overlays](#criteria-wildcard-overlays)).

### Step 3: Resolve Inheritance Chains

For each maximal-leaf match from step 2:

```go
chain, err := store.resolveInheritanceChain(overlay.Metadata.Name)
```

- Build the chain from root (base) to the target overlay by walking `spec.base`
- Detect cycles to prevent infinite loops
- Example: `["base", "eks", "eks-training", "gb200-eks-ubuntu-training"]`

Ancestors filtered out in step 2 re-enter the output here as part of their descendant's chain.

### Step 4: Merge Specifications

The merge starts from a seed containing the base spec, then applies each resolved chain on top:

```go
mergedSpec, appliedOverlays := s.initBaseMergedSpec()  // seed with s.Base
// ... then for each chain from Step 3:
for _, recipe := range chain {
    mergedSpec.Merge(&recipe.Spec)
}
```

This is why `base` always appears first in `appliedOverlays` even though it is not returned by `FindMatchingOverlays`.

#### Merge Algorithm

- **Constraints**: Same-named constraints are overridden; new constraints are added
- **ComponentRefs**: Same-named components are merged field-by-field using `mergeComponentRef()`

```go
func mergeComponentRef(base, overlay ComponentRef) ComponentRef {
    result := base
    if overlay.Type != "" { result.Type = overlay.Type }
    if overlay.Source != "" { result.Source = overlay.Source }
    if overlay.Version != "" { result.Version = overlay.Version }
    if overlay.ValuesFile != "" { result.ValuesFile = overlay.ValuesFile }
    // Merge overrides maps
    if overlay.Overrides != nil {
        result.Overrides = deepMerge(base.Overrides, overlay.Overrides)
    }
    // Merge dependency refs
    if len(overlay.DependencyRefs) > 0 {
        result.DependencyRefs = mergeDependencyRefs(base.DependencyRefs, overlay.DependencyRefs)
    }
    return result
}
```

### Step 5: Apply Mixins

```go
mixinConstraintNames, err := store.mergeMixins(mergedSpec)
```

- If the leaf overlay declares `spec.mixins`, each named mixin is loaded from `recipes/mixins/`
- Mixin constraints and componentRefs are appended to the merged spec
- Conflict detection prevents duplicates between the inheritance chain, previously applied mixins, and the current mixin
- When a snapshot evaluator is provided, mixin constraints are evaluated against it after merging; failure invalidates the entire composed candidate. In plain query mode (no snapshot), mixin constraints are merged but not evaluated

### Step 6: Validate Dependencies

```go
if err := mergedSpec.ValidateDependencies(); err != nil {
    return nil, err
}
```

- Verify all `dependencyRefs` reference existing components
- Detect circular dependencies

### Step 7: Compute Deployment Order

```go
deployOrder, err := mergedSpec.TopologicalSort()
```

- Topologically sort components based on `dependencyRefs`
- Ensures dependencies are deployed before dependents

### Step 8: Build RecipeResult

```go
return &RecipeResult{
    Kind:            "RecipeResult",
    APIVersion:      "aicr.nvidia.com/v1alpha1",
    Metadata:        metadata,
    Criteria:        criteria,
    Constraints:     mergedSpec.Constraints,
    ComponentRefs:   mergedSpec.ComponentRefs,
    DeploymentOrder: deployOrder,
}, nil
```

### Complete Flow Diagram

```mermaid
flowchart TD
    Start["User Query<br/>{service: 'eks', accelerator: 'gb200', intent: 'training'}"]

    Start --> Load[Load Metadata Store]

    Load --> Find["FindMatchingOverlays(criteria)<br/>iterates s.Overlays (base is separate)"]

    Find --> RawMatches["Overlay criteria matches (pre-filter):<br/>• monitoring-hpa (intent: any)<br/>• eks<br/>• eks-training<br/>• gb200-any-training (service: any wildcard)<br/>• gb200-eks-training"]

    RawMatches --> Filter["filterToMaximalLeaves():<br/>drop ancestors of other matches"]

    Filter --> Leaves["Maximal leaves:<br/>• monitoring-hpa<br/>• gb200-any-training<br/>• gb200-eks-training"]

    Leaves --> Resolve["Resolve Inheritance Chains<br/>(for each leaf, build chain root→leaf via spec.base)"]

    Resolve --> Chain1["base → monitoring-hpa"]
    Resolve --> Chain2["base → gb200-any-training"]
    Resolve --> Chain3["base → eks → eks-training → gb200-eks-training"]

    Chain1 --> Merge["Merge onto base spec (deduplicated)<br/>base spec is injected by initBaseMergedSpec()"]
    Chain2 --> Merge
    Chain3 --> Merge

    Merge --> Mixins["Apply Mixins (spec.mixins on merged leaves)"]

    Mixins --> Merged["Merged Spec<br/>base + monitoring-hpa + gb200-any-training + eks + eks-training + gb200-eks-training"]

    Merged --> Validate["Validate Dependencies"]

    Validate --> Sort["Topological Sort"]

    Sort --> Result["RecipeResult<br/>• componentRefs: [cert-manager, gpu-operator, ...]<br/>• deploymentOrder: [cert-manager, gpu-operator, ...]<br/>• constraints: [K8s.server.version >= 1.32.4]<br/>• validation.performance.constraints: [nccl-all-reduce-bw >= 720]<br/>• appliedOverlays: [base, monitoring-hpa, gb200-any-training, eks, eks-training, gb200-eks-training]"]
```

## Usage Examples

### CLI Usage

**Basic recipe generation (query mode):**
```bash
aicr recipe --os ubuntu --service eks --accelerator h100 --intent training
```

**Full specification:**
```bash
aicr recipe \
  --os ubuntu \
  --service eks \
  --accelerator gb200 \
  --intent training \
  --nodes 8 \
  --format yaml \
  --output recipe.yaml
```

**From snapshot (snapshot mode):**
```bash
aicr snapshot --output snapshot.yaml
aicr recipe --snapshot snapshot.yaml --intent training --output recipe.yaml
```

### API Usage

**Basic query:**
```bash
curl "http://localhost:8080/v1/recipe?os=ubuntu&service=eks&accelerator=h100"
```

**Full specification:**
```bash
curl "http://localhost:8080/v1/recipe?os=ubuntu&service=eks&accelerator=gb200&intent=training&nodes=8"
```

### Example Response (RecipeResult)

```json
{
  "kind": "RecipeResult",
  "apiVersion": "aicr.nvidia.com/v1alpha1",
  "metadata": {
    "version": "v0.8.0",
    "appliedOverlays": [
      "base",
      "eks",
      "eks-training",
      "gb200-eks-ubuntu-training"
    ]
  },
  "criteria": {
    "service": "eks",
    "accelerator": "gb200",
    "os": "ubuntu",
    "intent": "training"
  },
  "constraints": [
    {
      "name": "K8s.server.version",
      "value": ">= 1.32.4"
    },
    {
      "name": "OS.release.ID",
      "value": "ubuntu"
    },
    {
      "name": "OS.release.VERSION_ID",
      "value": "24.04"
    }
  ],
  "componentRefs": [
    {
      "name": "cert-manager",
      "type": "Helm",
      "source": "https://charts.jetstack.io",
      "version": "v1.20.2",
      "valuesFile": "components/cert-manager/values.yaml"
    },
    {
      "name": "gpu-operator",
      "type": "Helm",
      "source": "https://helm.ngc.nvidia.com/nvidia",
      "version": "v25.3.3",
      "valuesFile": "components/gpu-operator/values-eks-training.yaml",
      "overrides": {
        "driver": {
          "version": "580.82.07"
        },
        "cdi": {
          "enabled": true
        }
      },
      "dependencyRefs": ["cert-manager"]
    },
    {
      "name": "nvsentinel",
      "type": "Helm",
      "source": "oci://ghcr.io/nvidia/nvsentinel",
      "version": "v0.6.0",
      "valuesFile": "components/nvsentinel/values.yaml",
      "dependencyRefs": ["cert-manager"]
    },
    {
      "name": "nodewright-operator",
      "type": "Helm",
      "source": "oci://ghcr.io/nvidia/skyhook",
      "version": "v0.15.0",
      "valuesFile": "components/nodewright-operator/values.yaml",
      "overrides": {
        "customization": "ubuntu"
      }
    }
  ],
  "deploymentOrder": [
    "cert-manager",
    "gpu-operator",
    "nvsentinel",
    "nodewright-operator"
  ]
}
```

## Maintenance Guide

### Adding a New Recipe

1. **Create the recipe file** in `recipes/`:
   ```yaml
   kind: RecipeMetadata
   apiVersion: aicr.nvidia.com/v1alpha1
   metadata:
     name: l40-gke-ubuntu-inference  # Unique name
   
   spec:
     base: gke-inference  # Inherit from appropriate parent
     
     criteria:
       service: gke
       accelerator: l40
       os: ubuntu
       intent: inference
     
     constraints:
       - name: K8s.server.version
         value: ">= 1.29"
     
     componentRefs:
       - name: gpu-operator
         version: v25.3.3
         overrides:
           driver:
             version: 560.35.03
   ```

2. **Create intermediate recipes** if needed (e.g., `gke.yaml`, `gke-inference.yaml`)

3. **Add component values files** if using new configurations:
   ```yaml
   # components/gpu-operator/values-gke-inference.yaml
   driver:
     enabled: true
     version: 560.35.03
   ```

4. **Run tests** to validate:
   ```bash
   go test -v ./pkg/recipe/... -run TestAllMetadataFilesParseCorrectly
   ```

### Modifying Existing Recipes

1. **Update constraints** - Change version requirements:
   ```yaml
   constraints:
     - name: K8s.server.version
       value: ">= 1.33"  # Updated from 1.32
   ```

2. **Update component versions** - Bump chart versions:
   ```yaml
   componentRefs:
     - name: gpu-operator
       version: v25.4.0  # Updated from v25.3.3
   ```

3. **Add inline overrides** - Recipe-specific tweaks:
   ```yaml
   componentRefs:
     - name: gpu-operator
       overrides:
         newFeature:
           enabled: true
   ```

### Updating Component Values

1. **Modify values file** in `recipes/components/{component}/values.yaml`

2. **Create variant values file** for specific environments:
   - `values.yaml` - Base configuration
   - `values-eks-training.yaml` - EKS training optimization
   - `values-gke-inference.yaml` - GKE inference optimization

3. **Reference in recipe**:
   ```yaml
   componentRefs:
     - name: gpu-operator
       valuesFile: components/gpu-operator/values-gke-inference.yaml
   ```

---

## Automated Validation

The recipe data system includes comprehensive automated tests to ensure data integrity. These tests run automatically as part of `make test` and validate all recipe metadata files and component values.

### Test Suite Overview

The test suite is organized in `pkg/recipe/`:

| File | Responsibility |
|------|----------------|
| `yaml_test.go` | Static YAML file validation (parsing, references, enums, inheritance) |
| `metadata_test.go` | Runtime behavior tests (Merge, TopologicalSort, inheritance resolution) |
| `recipe_test.go` | Recipe struct validation (Validate, ValidateStructure) |

### Test Categories

| Test Category | What It Validates |
|---------------|-------------------|
| Schema Conformance | All YAML files parse correctly with expected structure |
| Criteria Validation | Valid enum values for service, accelerator, intent, OS |
| Reference Validation | valuesFile paths exist, dependencyRefs resolve, component names valid |
| Constraint Syntax | Measurement paths use valid types, operators are valid |
| Dependency Cycles | No circular dependencies in componentRefs |
| Inheritance Chains | Base references valid, no circular inheritance, reasonable depth |
| Values Files | Component values files parse as valid YAML |

### Inheritance-Specific Tests

| Test | What It Validates |
|------|-------------------|
| `TestAllBaseReferencesPointToExistingRecipes` | All `spec.base` references resolve to existing recipes |
| `TestNoCircularBaseReferences` | No circular inheritance chains (a→b→c→a) |
| `TestInheritanceChainDepthReasonable` | Inheritance depth ≤ 10 levels |

### Running Tests

```bash
# Run all recipe data tests
make test

# Run only recipe package tests
go test -v ./pkg/recipe/... -count=1

# Run specific test patterns
go test -v ./pkg/recipe/... -run TestAllMetadataFilesParseCorrectly
go test -v ./pkg/recipe/... -run TestAllBaseReferencesPointToExistingRecipes
go test -v ./pkg/recipe/... -run TestAllOverlayCriteriaUseValidEnums
```

### CI/CD Integration

Tests run automatically on:
- **Pull Requests**: All tests must pass before merge
- **Push to main**: Validates no regressions
- **Release builds**: Ensures data integrity in released binaries

```yaml
# GitHub Actions workflow snippet
jobs:
  validate:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: ./.github/actions/go-ci
        with:
          go_version: '1.26'
          golangci_lint_version: 'v2.11.3'
```

### Adding New Tests

When adding new recipe metadata or component configurations:

1. **Create the new file** in `recipes/`
2. **Run tests** to verify the file is valid:
   ```bash
   go test -v ./pkg/recipe/... -run TestAllMetadataFilesParseCorrectly
   ```
3. **Check for conflicts** with existing recipes:
   ```bash
   go test -v ./pkg/recipe/... -run TestNoDuplicateCriteria
   ```
4. **Verify references** if using valuesFile or dependencyRefs:
   ```bash
   go test -v ./pkg/recipe/... -run TestValuesFileReferencesExist
   go test -v ./pkg/recipe/... -run TestDependencyRefsResolve
   ```

---

## External Data Provider

The recipe system supports extending or overriding embedded data with external files via the `--data` CLI flag. This enables customization without rebuilding the CLI binary.

### Architecture Overview

```mermaid
flowchart TD
    subgraph Embedded["Embedded Data (compile-time)"]
        E1[overlays/base.yaml]
        E2[overlays/*.yaml]
        E3[components/*/values.yaml]
        E4[registry.yaml]
    end

    subgraph External["External Directory (runtime)"]
        X1[registry.yaml - REQUIRED]
        X2[overlays/custom.yaml]
        X3[components/custom/values.yaml]
    end

    Embedded --> LP[Layered Data Provider]
    External --> LP

    LP --> |ReadFile| MR{Is registry.yaml?}
    MR -->|Yes| Merge[Merge Components<br/>by Name]
    MR -->|No| Replace[External Replaces<br/>Embedded]

    Merge --> Output[Recipe Generation]
    Replace --> Output
```

### Data Provider Interface

The system uses a `DataProvider` interface to abstract file access:

```go
type DataProvider interface {
    // ReadFile reads a file by path (relative to data directory)
    ReadFile(path string) ([]byte, error)

    // WalkDir walks the directory tree rooted at root
    WalkDir(root string, fn fs.WalkDirFunc) error

    // Source returns where data came from (for debugging)
    Source(path string) string
}
```

**Provider Types:**
- `EmbeddedDataProvider`: Wraps Go's `embed.FS` for compile-time embedded data
- `LayeredDataProvider`: Overlays external directory on top of embedded data

### Merge Behavior

| File Type | Behavior | Example |
|-----------|----------|---------|
| `registry.yaml` | **Merged** by component name | External adds/replaces components |
| `overlays/base.yaml` | **Replaced** if exists externally | External completely overrides embedded |
| `overlays/*.yaml` | **Replaced** if same path | External overlay replaces embedded |
| `components/*/values.yaml` | **Replaced** if same path | External values override embedded |

### Registry Merge Algorithm

When merging `registry.yaml`, components are matched by their `name` field:

```go
func mergeRegistries(embedded, external *ComponentRegistry) *ComponentRegistry {
    // 1. Index external components by name
    externalByName := make(map[string]*ComponentConfig)
    for _, comp := range external.Components {
        externalByName[comp.Name] = comp
    }

    // 2. Add embedded components, replacing with external if present
    for _, comp := range embedded.Components {
        if ext, found := externalByName[comp.Name]; found {
            result.Components = append(result.Components, *ext)  // External wins
        } else {
            result.Components = append(result.Components, comp)  // Keep embedded
        }
    }

    // 3. Add new components from external (not in embedded)
    for _, comp := range external.Components {
        if !addedNames[comp.Name] {
            result.Components = append(result.Components, comp)
        }
    }

    return result
}
```

**Merge Order:**
1. Start with all embedded components
2. Replace any that have same name in external
3. Add any new components from external

### Security Validations

The `LayeredDataProvider` enforces security constraints:

| Validation | Behavior |
|------------|----------|
| **Directory exists** | External directory must exist and be a directory |
| **registry.yaml required** | External directory must contain `registry.yaml` |
| **No path traversal** | Paths containing `..` are rejected |
| **No symlinks** | Symlinks are rejected by default (`AllowSymlinks: false`) |
| **File size limit** | Files exceeding 10MB are rejected (configurable) |

### Configuration Options

```go
type LayeredProviderConfig struct {
    // ExternalDir is the path to the external data directory
    ExternalDir string

    // MaxFileSize is the maximum allowed file size in bytes (default: 10MB)
    MaxFileSize int64

    // AllowSymlinks allows symlinks in the external directory (default: false)
    AllowSymlinks bool
}
```

### Usage Example

**Creating an external data directory:**

```
my-data/
├── registry.yaml              # Required - merged with embedded
├── overlays/
│   └── my-custom-overlay.yaml # Adds new overlay
└── components/
    └── gpu-operator/
        └── values.yaml        # Replaces embedded gpu-operator values
```

**External registry.yaml (adds custom Helm component):**

```yaml
apiVersion: aicr.nvidia.com/v1alpha1
kind: ComponentRegistry
components:
  - name: my-custom-operator
    displayName: My Custom Operator
    helm:
      defaultRepository: https://my-charts.example.com
      defaultChart: my-custom-operator
      defaultVersion: v1.0.0
```

**External registry.yaml (adds custom Kustomize component):**

```yaml
apiVersion: aicr.nvidia.com/v1alpha1
kind: ComponentRegistry
components:
  - name: my-kustomize-app
    displayName: My Kustomize App
    valueOverrideKeys:
      - mykustomize
    kustomize:
      defaultSource: https://github.com/example/my-app
      defaultPath: deploy/production
      defaultTag: v1.0.0
```

**Note:** A component must have either `helm` OR `kustomize` configuration, not both.

**CLI usage:**

```bash
# Generate recipe with external data
aicr recipe --service eks --accelerator h100 --data ./my-data

# Bundle with external data
aicr bundle --recipe recipe.yaml --data ./my-data --output ./bundles
```

### Debugging

Use `--debug` flag to see detailed logging about external data loading:

```bash
aicr --debug recipe --service eks --data ./my-data
```

Debug logs include:
- Files discovered in external directory
- Source resolution for each file (embedded vs external vs merged)
- Component merge details (added, overridden, retained)

### Implementation Details

The data provider is initialized early in CLI command execution:

```go
// pkg/cli/root.go
func initDataProvider(cmd *cli.Command) error {
    dataDir := cmd.String("data")
    if dataDir == "" {
        return nil  // Use default embedded provider
    }

    embedded := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "data")
    layered, err := recipe.NewLayeredDataProvider(embedded, recipe.LayeredProviderConfig{
        ExternalDir:   dataDir,
        AllowSymlinks: false,
    })
    if err != nil {
        return err
    }

    recipe.SetDataProvider(layered)
    return nil
}
```

**Global Provider Pattern:**
- `SetDataProvider()` sets the global data provider
- `GetDataProvider()` returns the current provider (defaults to embedded)
- `GetDataProviderGeneration()` returns a counter for cache invalidation

---

## See Also

- [Recipe Development Guide](../integrator/recipe-development.md) - Adding and modifying recipe data
- [CLI Architecture](cli.md) - How the CLI uses recipe data
- [CLI Reference](../user/cli-reference.md) - Complete CLI flag reference
- [API Server Architecture](api-server.md) - How the API serves recipes
- [OpenAPI Specification](https://github.com/NVIDIA/aicr/blob/main/api/aicr/v1/server.yaml) - Recipe API contract
- [Recipe Package Documentation](https://github.com/NVIDIA/aicr/tree/main/pkg/recipe) - Go implementation details
