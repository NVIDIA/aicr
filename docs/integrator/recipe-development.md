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

## Table of Contents

- [Recipe Development Guide](#recipe-development-guide)
  - [Quick Start: Contributing a Recipe](#quick-start-contributing-a-recipe)
  - [Table of Contents](#table-of-contents)
  - [Overview](#overview)
  - [Recipe Structure](#recipe-structure)
    - [Multi-Level Inheritance](#multi-level-inheritance)
    - [Component Types](#component-types)
  - [Component Configuration](#component-configuration)
    - [Configuration Patterns](#configuration-patterns)
    - [Value Merge Precedence](#value-merge-precedence)
  - [File Naming Conventions](#file-naming-conventions)
  - [Constraints and Validation](#constraints-and-validation)
    - [Constraints](#constraints)
    - [Validation Phases](#validation-phases)
    - [Testing](#testing)
  - [Working with Recipes](#working-with-recipes)
    - [Adding a New Recipe](#adding-a-new-recipe)
    - [Updating Recipes](#updating-recipes)
  - [Best Practices](#best-practices)
  - [Testing and Validation](#testing-and-validation)
    - [Automated Tests](#automated-tests)
    - [Running Tests](#running-tests)
    - [Test Workflow](#test-workflow)
  - [Advanced Topics](#advanced-topics)
    - [External Data Sources](#external-data-sources)
  - [Troubleshooting](#troubleshooting)
  - [See Also](#see-also)

## Overview

Recipe metadata files define component configurations for GPU-accelerated Kubernetes deployments using a **base-plus-overlay architecture** with **multi-level inheritance**:

- **Base values** (`overlays/base.yaml`) - universal defaults
- **Intermediate recipes** (`eks.yaml`, `eks-training.yaml`) - shared configurations for categories
- **Leaf recipes** (`gb200-eks-ubuntu-training.yaml`) - hardware/workload-specific overrides
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

**Merge order:** `base.yaml` (lowest) → intermediate → leaf (highest)

**Merge rules:**
- Constraints: same-named overridden, new added
- ComponentRefs: same-named merged field-by-field, new added
- Criteria: not inherited (each recipe defines its own)

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

**Kustomize components:**
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

**Common measurement paths:**
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
2. Create component values files if using `valuesFile`
3. Run tests: `make test`
4. Test generation: `aicr recipe --service eks --accelerator gb200 --format yaml`

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
- Always explain why settings exist (1-2 sentences)
- Follow naming conventions (`{accel}-{service}-{os}-{intent}-{platform}`)
- Run `make test` before committing
- Test recipe generation after changes

**Don't:**
- Add environment-specific settings to base
- Over-specify criteria (too narrow = fewer matches)
- Create duplicate criteria combinations
- Skip validation tests
- Forget to update context when values change

## Testing and Validation

### Automated Tests

Tests in [`pkg/recipe/yaml_test.go`](../../pkg/recipe/yaml_test.go) validate:
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

**Directory structure:**
```
./my-data/
├── registry.yaml              # Extends/overrides component registry
├── overlays/
│   └── custom-recipe.yaml     # New or override existing recipe
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
