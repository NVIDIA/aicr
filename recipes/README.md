# Recipe Data Directory

This directory contains recipe metadata and component configurations for the NVIDIA Cloud Native Stack bundler system.

## Quick Reference

| Task | Documentation |
|------|--------------|
| Understand recipe architecture | [Data Architecture](../docs/contributor/data.md) |
| Create/modify recipes | [Recipe Development Guide](../docs/integrator/recipe-development.md) |
| Create new bundlers | [Bundler Development Guide](../docs/contributor/component.md) |
| CLI commands | [CLI Reference](../docs/user/cli-reference.md) |

## Directory Structure

```
recipes/
├── registry.yaml                  # Component registry (Helm & Kustomize configs)
├── overlays/                      # Recipe overlays (including base)
│   ├── base.yaml                  # Base recipe (universal defaults, root of inheritance)
│   ├── eks.yaml                   # EKS overlay
│   ├── eks-training.yaml          # EKS + training overlay
│   ├── gb200-eks-training.yaml    # GB200 + EKS + training overlay
│   ├── gb200-eks-ubuntu-training.yaml # Full criteria leaf recipe
│   ├── h100-eks-ubuntu-training-kubeflow.yaml # H100 + EKS + Ubuntu + training + Kubeflow
│   └── h100-ubuntu-inference.yaml # H100 inference overlay
└── components/                    # Component value configurations
    ├── cert-manager/
    ├── nvidia-dra-driver-gpu/
    ├── gpu-operator/
    └── ...
```

**Recipe Naming Convention:**
Recipe file names follow a specific ordering convention for consistency:
`{accelerator}-{service}-{os}-{intent}-{platform}.yaml`

Examples:
- `h100-eks-training.yaml` (accelerator + service + intent)
- `h100-eks-ubuntu-training.yaml` (accelerator + service + os + intent)
- `h100-eks-ubuntu-training-kubeflow.yaml` (accelerator + service + os + intent + platform)

## Overview

The recipe system uses a **base-plus-overlay architecture**:

- **Base values** (`overlays/base.yaml`) provide default configurations
- **Overlay values** (e.g., `eks-gb200-training.yaml`) provide environment-specific optimizations
- **Inline overrides** allow per-recipe customization without creating new files

All files in this directory are embedded into the CLI binary and API server at compile time.

### Run Validation Tests

```bash
# Run all recipe tests
make test

# Run specific validation
go test -v ./pkg/recipe/... -run TestAllMetadataFilesConformToSchema

# Check for duplicate criteria
go test -v ./pkg/recipe/... -run TestNoDuplicateCriteriaAcrossOverlays
```

## Automated Validation

All recipe metadata and component values are automatically validated. Tests run as part of `make test` and check:

- Schema conformance (YAML parses correctly)
- Criteria validation (valid enum values)
- Reference validation (valuesFile paths exist, dependencyRefs resolve)
- Constraint syntax (valid measurement paths and operators)
- Uniqueness (no duplicate criteria across overlays)
- Merge consistency (base + overlay merges without data loss)

```bash
# Generate bundle from recipe with overrides
aicr bundle -r recipes/overlays/your-recipe.yaml -o ./test-bundles

# Verify merged values
cat test-bundles/gpu-operator/values.yaml | grep -A5 "driver:"
```
For detailed test documentation, see [Automated Validation](../docs/contributor/data.md#automated-validation).

## See Also

- [Data Architecture](../docs/contributor/data.md) - Recipe generation process, overlay system, query matching
- [Recipe Development Guide](../docs/integrator/recipe-development.md) - How to create and modify recipes
- [Bundler Development Guide](../docs/contributor/component.md) - How to create new bundlers
