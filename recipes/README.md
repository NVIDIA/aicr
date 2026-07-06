# Recipe Data Directory

Recipe metadata and component configurations for the AICR bundler. Runtime recipe data is embedded into the CLI binary and API server at compile time — the embed patterns in `data.go` explicitly select what ships (overlays, mixins, the registry, the validator catalog, component values/manifests, and checks). Repository-only files sit deliberately outside those patterns and never ship: `version-pin-exemptions.yaml` (dev/CI version-pin policy) and `evidence/` (published recipe-evidence pointers).

## Quick Reference

| Task | Documentation |
|------|--------------|
| Recipe architecture | [Data Architecture](../docs/contributor/recipe.md) |
| Create/modify recipes | [Recipe Development Guide](../docs/integrator/recipe-development.md) |
| Add or modify components | [Components](../docs/contributor/component.md) |
| CLI commands | [CLI Reference](../docs/user/cli-reference.md) |

## Directory Structure

```
recipes/
├── registry.yaml                  # Component registry (Helm & Kustomize configs)
├── version-pin-exemptions.yaml    # Blessed overlay/mixin version divergences (NOT embedded; dev/CI only)
├── evidence/                      # Published recipe-evidence pointers (NOT embedded)
├── overlays/                      # Recipe overlays (including base.yaml as root)
├── mixins/                        # Composable mixin fragments
│   ├── os-ubuntu.yaml             # Ubuntu OS constraints
│   ├── platform-inference.yaml    # Inference gateway components
│   └── platform-kubeflow.yaml     # Kubeflow trainer component
├── checks/                        # Component health checks
├── validators/                    # Validator catalog (catalog.yaml)
└── components/                    # Per-component Helm/Kustomize values
```

**Recipe naming convention:** `{accelerator}-{service}-{os}-{intent}-{platform}.yaml` (each segment optional except where required by the base inheritance chain).

Examples: `h100-eks-training.yaml`, `h100-eks-ubuntu-training.yaml`, `h100-eks-ubuntu-training-kubeflow.yaml`.

## Architecture

The recipe system uses a **base-plus-overlay architecture** with **mixin composition**:

- **Base** (`overlays/base.yaml`) provides default configurations
- **Overlays** provide environment-specific optimizations and inherit from a parent via `spec.base`
- **Mixins** (`mixins/*.yaml`) provide shared fragments composed via `spec.mixins` instead of duplication
- **Inline overrides** allow per-recipe customization

## Validation

All recipe metadata and component values are validated as part of `make test`:

- Schema conformance, criteria enums, valuesFile path resolution
- Constraint syntax, no duplicate criteria across overlays, merge consistency

```bash
make test
go test -v ./pkg/recipe/... -run TestAllMetadataFilesConformToSchema
go test -v ./pkg/recipe/... -run TestNoDuplicateCriteriaAcrossOverlays
```

For details, see [Automated Validation](../docs/contributor/recipe.md#automated-validation).

## See Also

- [Data Architecture](../docs/contributor/recipe.md)
- [Recipe Development Guide](../docs/integrator/recipe-development.md)
- [Components](../docs/contributor/component.md)
