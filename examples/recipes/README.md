# Example Recipes

These hand-curated files illustrate `RecipeResult` structure. They are
intentionally incomplete and differ from actual generated output: component
sets, metadata, validation, chart pins, paths, and defaults may differ.
Generate a current recipe before bundling or validation, even with the same
AICR version. Do not use the checked-in illustrations as deployment inputs.

## Files

- **`kind.yaml`** — Selected supporting components for a local Kind cluster; omits the GPU stack and validation.
- **`eks-training.yaml`** — Selected EKS training components; omits validation and resolved metadata.
- **`aks-training.yaml`** — AKS training structure; omits `metadata.selectedProfile`, its ownership locks, profile-specific overrides, and readiness constraints. The legacy `aicr.run/v1` payload does not represent a current profiled AKS recipe.
- **`eks-gb200-ubuntu-training-with-validation.yaml`** — Authored severity and remediation examples with selected deployment and performance checks; conformance and parts of the component graph are deliberately omitted.

## Generate a Recipe for Operational Use

For an EKS GB200 cluster running Ubuntu, generate a recipe from explicit
criteria with the installed AICR version:

```shell
# Generate a current recipe
aicr recipe --service eks --accelerator gb200 --os ubuntu --intent training \
  --output generated-recipe.yaml

# Generate a deployment bundle from that recipe
aicr bundle --recipe generated-recipe.yaml --output ./bundles
```

The generated recipe supplies the current validation configuration; the GB200
illustration above deliberately omits conformance. Reapply any illustrative
severity or remediation customizations you need to the generated recipe after
reviewing its checks and constraints.

For AKS, follow [AKS GPU Setup](../../docs/integrator/aks-gpu-setup.md) to capture
the GPU pool projection and select the matching `gpuStack` profile during
recipe generation. Current profiled output records `metadata.selectedProfile`
and ownership locks; adding that field to the legacy `aicr.run/v1` illustration
is not a substitute for generating a recipe.

## See Also

- [CLI Reference](../../docs/user/cli-reference.md)
- [Recipe Development Guide](../../docs/integrator/recipe-development.md)
- [Validation](../../docs/user/validation.md) — Multi-phase validation details
