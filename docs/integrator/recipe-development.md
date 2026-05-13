# Recipe Development Guide

This guide covers how to create, modify, and validate recipe metadata.

## Quick Start: Contributing a Recipe

New to recipe development? Follow these minimal steps to contribute:

**1. Copy an existing overlay** ([details](#working-with-recipes))
```bash
cp recipes/overlays/h100-eks-ubuntu-training.yaml recipes/overlays/gb200-eks-ubuntu-training.yaml
```

**2. Edit criteria and components** ([criteria](#recipe-structure), [components](#component-configuration))
```yaml
# recipes/overlays/gb200-eks-ubuntu-training.yaml
spec:
  base: eks-training  # Inherit from intermediate recipe
  criteria:
    service: eks
    accelerator: gb200  # Changed from h100
    os: ubuntu
    intent: training
  componentRefs:
    - name: gpu-operator
      version: v25.3.4
      valuesFile: components/gpu-operator/eks-gb200-training.yaml
      overrides:
        driver:
          version: "580.82.07"  # GB200-specific driver
```

**3. Run tests** ([details](#testing-and-validation))
```bash
make test  # Validates schema, criteria, references, constraints
make qualify  # Includes end to end tests before submitting
```

**4. Open PR** ([best practices](#best-practices))
- Include test output showing recipe generation works
- Explain why the recipe is needed (new hardware, workload, platform)

---

## Overview

Recipe metadata files define component configurations for GPU-accelerated Kubernetes deployments using a **base-plus-overlay architecture** with three composition mechanisms — single-parent inheritance, explicit mixin composition, and criteria-wildcard matching:

- **Base values** (`overlays/base.yaml`) - universal defaults
- **Intermediate recipes** (`eks.yaml`, `eks-training.yaml`) - shared configurations for categories
- **Leaf recipes** (`gb200-eks-ubuntu-training.yaml`) - hardware/workload-specific overrides
- **Mixins** (`mixins/*.yaml`) - composable fragments (OS constraints, platform components) that leaf overlays reference via `spec.mixins` instead of duplicating content
- **Criteria-wildcard overlays** (`gb200-any-training.yaml`) - cross-cutting overlays picked up automatically by the resolver when their wildcard criteria match the query, without being referenced via `spec.base` or `spec.mixins`
- **Inline overrides** - per-recipe customization without new files

Recipe files in `recipes/` are embedded at compile time. Integrators can extend or override using the `--data` flag (see [Advanced Topics](#advanced-topics)).

For query matching and overlay merging internals, see [Data Architecture](../contributor/data.md).

## Recipe Structure

### Multi-Level Inheritance

Recipes use `spec.base` to inherit configurations. Chains progress from general (base) to specific (leaf):

```
base.yaml → eks.yaml → eks-training.yaml → gb200-eks-ubuntu-training.yaml
```

**Intermediate recipes** (partial criteria) capture shared configs:
```yaml
# eks-training.yaml
spec:
  base: eks
  criteria:
    service: eks
    intent: training  # Partial - no accelerator/OS
  componentRefs:
    - name: gpu-operator
      valuesFile: components/gpu-operator/values-eks-training.yaml
```

**Leaf recipes** (complete criteria) match user queries:
```yaml
# gb200-eks-ubuntu-training.yaml
spec:
  base: eks-training  # Inherits from intermediate
  criteria:
    service: eks
    accelerator: gb200
    os: ubuntu
    intent: training  # Complete
  componentRefs:
    - name: gpu-operator
      overrides:
        driver:
          version: "580.82.07"  # Hardware-specific override
```

**Leaf recipes with mixins** compose shared fragments:
```yaml
# h100-eks-ubuntu-training-kubeflow.yaml
spec:
  base: h100-eks-ubuntu-training
  mixins:
    - os-ubuntu          # Shared Ubuntu constraints (from recipes/mixins/)
    - platform-kubeflow  # Kubeflow trainer component (from recipes/mixins/)
  criteria:
    service: eks
    accelerator: h100
    os: ubuntu
    intent: training
    platform: kubeflow
```

Mixins use `kind: RecipeMixin` and carry only `constraints` and `componentRefs`. They live in `recipes/mixins/` and are applied after inheritance chain merging. See [Data Architecture](../contributor/data.md#mixin-composition) for details.

When authoring a recipe targeting Talos (`criteria.os: talos`), append the `os-talos` mixin to your overlay's `spec.mixins` list (e.g. `spec.mixins: [os-talos]`, or `[platform-kubeflow, os-talos]` if you already mix in a non-OS fragment). OS-scoped mixins are mutually exclusive — combining `os-ubuntu` and `os-talos` in one overlay is a recipe authoring error, not a supported composition. The mixin overrides namespaces for affected components and supplies PSA-privileged Namespace manifests via `componentRefs[].preManifestFiles`, which are applied before each chart — see [Talos integration](talos-integration.md) for the component list and labels.

**Cross-cutting overlays with wildcard criteria** apply across one criteria dimension without being referenced via `spec.base` or listed in `spec.mixins`. The resolver can return multiple independent maximal-leaf overlays for a single query, so a `service: any` overlay is picked up alongside the service-specific maximal leaf and its inheritance chain:

```yaml
# gb200-any-training.yaml — applies to every GB200+training query
spec:
  base: base
  criteria:
    service: any         # Wildcard — matches eks, oke, gke, etc.
    accelerator: gb200
    intent: training
  validation:
    performance:
      checks:
        - nccl-all-reduce-bw   # Required: selects which validators run
      constraints:
        - name: nccl-all-reduce-bw
          value: ">= 720"
```

Only use this pattern when the content is truly uniform across the wildcard dimension — if values diverge per service, keep them inline in each service-specific overlay. See [Data Architecture](../contributor/data.md#criteria-wildcard-overlays) for when to use wildcard overlays vs mixins.

**Merge order:** `base.yaml` (lowest) → intermediate → leaf → mixins (highest)

**Merge rules:**
- Constraints: same-named overridden, new added
- ComponentRefs: same-named merged field-by-field, new added
- Criteria: not inherited (each recipe defines its own)
- Mixin constraints/components must not conflict with the inheritance chain or other mixins

### Component Types

**Helm components** (most common):
```yaml
componentRefs:
  - name: gpu-operator
    type: Helm
    version: v25.3.4
    valuesFile: components/gpu-operator/values.yaml
    overrides:
      driver:
        version: "580.82.07"
```

#### Kustomize components

```yaml
componentRefs:
  - name: my-app
    type: Kustomize
    source: https://github.com/example/my-app
    tag: v1.0.0
    path: deploy/production
```

A component must have either `helm` OR `kustomize` configuration, not both.

## Component Configuration

### Configuration Patterns

**Pattern 1: ValuesFile only** (large, reusable configs)
```yaml
componentRefs:
  - name: cert-manager
    valuesFile: components/cert-manager/eks-values.yaml
```

**Pattern 2: Overrides only** (small, recipe-specific configs)
```yaml
componentRefs:
  - name: nvsentinel
    overrides:
      namespace: nvsentinel
      sentinel:
        enabled: true
```

**Pattern 3: Hybrid** (shared base + recipe tweaks)
```yaml
componentRefs:
  - name: gpu-operator
    valuesFile: components/gpu-operator/eks-gb200-training.yaml
    overrides:
      driver:
        version: "580.82.07"  # Override just this field
```

### Value Merge Precedence

Values merge from lowest to highest precedence:

```
Base → ValuesFile → Overrides → CLI --set flags
```

**Deep merge:** only specified fields replaced, unspecified preserved. Arrays replaced entirely (not element-by-element).

**Example:**
```yaml
# Base: driver.version="550.54.15", driver.repository="nvcr.io/nvidia"
# ValuesFile: driver.version="570.86.16"
# Override: driver.version="580.13.01"
# Result: driver.version="580.13.01", driver.repository="nvcr.io/nvidia" (preserved)
```

## File Naming Conventions

File names are for human readability—matching uses `spec.criteria`, not file names.

**Overlay naming:** `{accelerator}-{service}-{os}-{intent}-{platform}.yaml` (platform always last)

| File Type | Pattern | Example |
|-----------|---------|---------|
| Service | `{service}.yaml` | `eks.yaml` |
| Service + intent | `{service}-{intent}.yaml` | `eks-training.yaml` |
| Full criteria | `{accel}-{service}-{os}-{intent}.yaml` | `gb200-eks-ubuntu-training.yaml` |
| + platform | `{accel}-{service}-{os}-{intent}-{platform}.yaml` | `gb200-eks-ubuntu-training-kubeflow.yaml` |
| Mixin (OS) | `os-{os}.yaml` | `os-ubuntu.yaml` |
| Mixin (platform) | `platform-{platform}.yaml` | `platform-kubeflow.yaml` |
| Component values | `values-{service}-{intent}.yaml` | `values-eks-training.yaml` |

## Constraints and Validation

### Constraints

Constraints validate deployment requirements against cluster snapshots:

```yaml
constraints:
  - name: K8s.server.version
    value: ">= 1.32.4"
  - name: OS.release.ID
    value: ubuntu
  - name: OS.release.VERSION_ID
    value: "24.04"
```

#### Common measurement paths

| Path | Example |
|------|---------|
| `K8s.server.version` | `1.32.4` |
| `OS.release.ID` | `ubuntu`, `rhel` |
| `OS.release.VERSION_ID` | `24.04` |
| `GPU.smi.driver-version` | `580.82.07` |

**Operators:** `>=`, `<=`, `>`, `<`, `==`, `!=`, or exact match (no operator)

**Add constraints when:** recipe needs specific K8s features, driver versions, OS capabilities, or hardware. Skip when universal or redundant with component self-checks.

### Validation Phases

Optional multi-phase validation beyond basic constraints:

```yaml
# expectedResources are declared on componentRefs, not under validation
componentRefs:
  - name: gpu-operator
    type: Helm
    expectedResources:
      - kind: Deployment
        name: gpu-operator
        namespace: gpu-operator
      - kind: DaemonSet
        name: nvidia-driver-daemonset
        namespace: gpu-operator

validation:
  # Readiness phase has no checks — constraints are evaluated inline from snapshot.
  deployment:
    checks: [expected-resources]
  performance:
    infrastructure: nccl-doctor
    checks: [nccl-bandwidth-test]
```

**Phases:** `deployment`, `performance`, `conformance` (readiness constraints are evaluated implicitly)

### Testing

```bash
# Validate constraints
aicr validate --recipe recipe.yaml --snapshot snapshot.yaml

# Phase-specific
aicr validate --recipe recipe.yaml --snapshot snapshot.yaml --phase deployment

# Run validation tests
go test -v ./pkg/recipe/... -run TestConstraintPathsUseValidMeasurementTypes
```

## Working with Recipes

### Adding a New Recipe

**When:** new platform, hardware, workload type, or combined criteria

**Steps:**
1. Create overlay in `recipes/overlays/` with criteria and componentRefs
2. If the recipe shares OS constraints or platform components with other overlays, reference existing mixins via `spec.mixins` instead of duplicating (or create new mixins in `recipes/mixins/`)
3. Create component values files if using `valuesFile`
4. Run tests: `make test`
5. Test generation: `aicr recipe --service eks --accelerator gb200 --format yaml`

**Example:**
```yaml
# recipes/overlays/gb200-eks-ubuntu-training.yaml
apiVersion: aicr.nvidia.com/v1alpha1
kind: RecipeMetadata
metadata:
  name: gb200-eks-ubuntu-training
spec:
  base: eks-training
  criteria:
    service: eks
    accelerator: gb200
    os: ubuntu
    intent: training
  componentRefs:
    - name: gpu-operator
      version: v25.3.4
      valuesFile: components/gpu-operator/eks-gb200-training.yaml
```

### Updating Recipes

**Updating versions:**
```yaml
# Update component version
componentRefs:
  - name: gpu-operator
    version: v25.3.4  # Changed from v25.3.3
```

**Adding components:**
```yaml
componentRefs:
  - name: new-component
    version: v1.0.0
    valuesFile: components/new-component/values.yaml
    dependencyRefs: [existing-component]  # Optional
```

**Test changes:** `aicr recipe --service eks --accelerator gb200 --format yaml`

## Best Practices

**Do:**
- Use minimum criteria fields needed for matching
- Keep base recipe universal and conservative
- Use mixins for shared OS constraints or platform components instead of duplicating across leaf overlays
- Always explain why settings exist (1-2 sentences)
- Follow naming conventions (`{accel}-{service}-{os}-{intent}-{platform}`)
- Run `make test` before committing
- Test recipe generation after changes

**Don't:**
- Add environment-specific settings to base
- Over-specify criteria (too narrow = fewer matches)
- Create duplicate criteria combinations
- Duplicate OS or platform content across leaf overlays (use mixins instead)
- Skip validation tests
- Forget to update context when values change

## Testing and Validation

### Automated Tests

Tests in [`pkg/recipe/yaml_test.go`](https://github.com/NVIDIA/aicr/blob/main/pkg/recipe/yaml_test.go) validate:
- Schema conformance (YAML structure)
- Criteria enum values (service, accelerator, intent, OS, platform)
- File references (valuesFile, dependencyRefs)
- Constraint syntax (measurement paths, operators)
- No duplicate criteria
- Merge consistency
- No dependency cycles

### Running Tests

```bash
make test  # All tests
go test -v ./pkg/recipe/...  # Recipe tests only
go test -v ./pkg/recipe/... -run TestAllMetadataFilesConformToSchema  # Specific test
```

### Test Workflow

1. Create recipe file in `recipes/`
2. Run `make test` to validate
3. Test generation: `aicr recipe --service eks --accelerator gb200 --format yaml`
4. Inspect bundle: `aicr bundle -r recipe.yaml -o ./test-bundles`

Tests run automatically on PRs, main pushes, and release builds.

## Advanced Topics

### External Data Sources

Integrators can extend or override embedded recipe data using the `--data` flag without modifying the OSS codebase. This enables:
- Custom recipes for proprietary hardware
- Private component values with organization-specific settings
- Extended registries with internal Helm charts
- Rapid iteration without rebuilding binaries

#### Directory structure

```
./my-data/
├── registry.yaml              # Extends/overrides component registry
├── overlays/
│   └── custom-recipe.yaml     # New or override existing recipe
├── mixins/
│   └── os-custom.yaml         # Custom mixin fragments
└── components/
    └── my-operator/
        └── values.yaml        # Component values
```

**Usage:**
```bash
# Recipe generation
aicr recipe --service eks --accelerator gb200 --data ./my-data --output recipe.yaml

# Bundle generation
aicr bundle --recipe recipe.yaml --data ./my-data --deployer argocd --output ./bundle

# Debug loading
aicr --debug recipe --service eks --data ./my-data
```

**Precedence:** Embedded data (lowest) → External data (highest)

**Behavior:**
- Overlays: Same `metadata.name` replaces embedded
- Registry: Merged; same-named components replaced
- Values: External valuesFile references take precedence

**Validation:**
```bash
aicr --debug recipe --service eks --data ./my-data --dry-run
aicr recipe --service eks --data ./my-data --output /dev/stdout
```

### Regional registry overrides

A handful of components ship images from regional, account-scoped container registries rather than a single public URI. The clearest example today is the AWS EFA device plugin, whose canonical home is `<account>.dkr.ecr.<region>.amazonaws.com/eks/aws-efa-k8s-device-plugin` — a per-region private ECR that every EKS node is auto-authorized to pull from. AWS publishes these add-ons regionally for three reasons: pulls go over the AWS internal backbone (no NAT egress), no Docker Hub / public-registry rate limits, and the image stays available even when the public internet or another region is degraded.

AICR ships a sensible default for each such image (e.g., us-west-2 for `aws-efa`), but customers deploying in a different region need to override the registry's region segment. Two override paths cover the common cases:

**Bundle-time override (single region per bundle).** Use `--set` to bake a specific region into the bundle:

```bash
aicr bundle --recipe recipe.yaml \
  --set awsefa:image.repository=602401143452.dkr.ecr.us-east-1.amazonaws.com/eks/aws-efa-k8s-device-plugin \
  -o ./bundle
```

**Install-time override (one bundle, many regions).** Use `--dynamic` to declare the path as install-time-fillable, then provide the value via `helm install --set` (or your GitOps tool):

```bash
aicr bundle --recipe recipe.yaml \
  --dynamic awsefa:image.repository \
  --deployer helm \
  -o ./bundle

# Per-cluster install
helm install ... --set image.repository=602401143452.dkr.ecr.eu-west-1.amazonaws.com/eks/aws-efa-k8s-device-plugin
```

`--dynamic` is supported with `helm`, `argocd-helm`, and `flux` deployers; `argocd` does not support it (use `argocd-helm` instead). See [Dynamic Install-Time Values](../user/cli-reference.md#dynamic-install-time-values) for the broader pattern.

**Partition-aware variants.** Standard AWS uses account ID `602401143452`. GovCloud and China use different accounts and URI suffixes:

| Partition | Account ID | URI shape |
|-----------|------------|-----------|
| `aws` (standard) | `602401143452` | `<account>.dkr.ecr.<region>.amazonaws.com` |
| `aws-us-gov` (GovCloud) | `013241004608` | `<account>.dkr.ecr.<region>.amazonaws.com` |
| `aws-cn` (China) | `961992271922` | `<account>.dkr.ecr.<region>.amazonaws.com.cn` |

Substitute the appropriate account and suffix in the `--set` / install-time value.

## Troubleshooting

**Debug overlay matching:**
```bash
aicr recipe --service eks --accelerator gb200 --format json | jq '.metadata.appliedOverlays'
aicr recipe --service eks --accelerator gb200 --format json | jq '.componentRefs[].version'
```

**Common issues:**
| Issue | Solution |
|-------|----------|
| Test: "duplicate criteria" | Combine overlays or differentiate criteria |
| Test: "valuesFile not found" | Create file or fix path in recipe |
| Test: "unknown component" | Use registered bundler name |
| Recipe returns empty | Check criteria fields match query |
| Wrong values in bundle | Verify merge precedence (base → valuesFile → overrides) |

**Validation:**
```bash
make qualify  # Full qualification
make test     # All tests
aicr recipe --service eks --accelerator gb200 --format yaml  # Test generation
```

---

## See Also

- [Data Architecture](../contributor/data.md) - Recipe generation process, overlay system, query matching algorithm
- [Bundler Development Guide](../contributor/component.md) - Creating new bundlers
- [CLI Reference](../user/cli-reference.md) - CLI commands for recipe and bundle generation
- [API Reference](../user/api-reference.md) - Programmatic recipe access
