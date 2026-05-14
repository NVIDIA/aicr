# Example Recipes

Example recipe files demonstrating common configurations.

## Files

- **`kind.yaml`** — Local Kind cluster with fake GPU
- **`eks-training.yaml`** — EKS training workload
- **`aks-training.yaml`** — AKS training workload
- **`eks-gb200-ubuntu-training-with-validation.yaml`** — Multi-phase validation example with severity levels and remediation guidance

## Using Example Recipes

```shell
# Generate deployment bundle
aicr bundle --recipe eks-gb200-ubuntu-training-with-validation.yaml --output ./bundles

# With value overrides
aicr bundle \
  --recipe eks-gb200-ubuntu-training-with-validation.yaml \
  --set gpuoperator:driver.version=580.105.08 \
  --output ./bundles

# Validate against snapshot (default phase: readiness)
aicr snapshot --output snapshot.yaml
aicr validate --recipe eks-gb200-ubuntu-training-with-validation.yaml --snapshot snapshot.yaml

# All phases (readiness, deployment, performance, conformance)
aicr validate --recipe eks-gb200-ubuntu-training-with-validation.yaml --snapshot snapshot.yaml --phase all
```

## See Also

- [CLI Reference](../../docs/user/cli-reference.md)
- [Recipe Development Guide](../../docs/integrator/recipe-development.md)
- [Validation](../../docs/user/validation.md) — Multi-phase validation details
