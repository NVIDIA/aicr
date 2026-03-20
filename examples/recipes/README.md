# Example Recipes

This directory contains example recipe files demonstrating various configurations and features.

## Files

### Basic Recipes

- **`kind.yaml`** - Recipe for local Kind cluster with fake GPU
- **`eks-training.yaml`** - EKS recipe optimized for training workloads

### Advanced Examples

- **`eks-gb200-ubuntu-training-with-validation.yaml`** - Demonstrates multi-phase validation configuration
  - Shows how to configure validation checks for different deployment phases
  - Includes constraints with severity levels and remediation guidance
  - Demonstrates readiness, deployment, and performance validation phases

## Using Example Recipes

### Generate Bundle from Recipe

```shell
# Generate deployment bundle
aicr bundle --recipe eks-gb200-ubuntu-training-with-validation.yaml --output ./bundles

# Generate bundle with value overrides
aicr bundle \
  --recipe eks-gb200-ubuntu-training-with-validation.yaml \
  --set gpuoperator:driver.version=580.82.07 \
  --output ./bundles
```

### Validate Recipe Against Cluster

```shell
# Capture cluster snapshot
aicr snapshot --output snapshot.yaml

# Validate readiness phase (default)
aicr validate \
  --recipe eks-gb200-ubuntu-training-with-validation.yaml \
  --snapshot snapshot.yaml

# Validate all phases
aicr validate \
  --recipe eks-gb200-ubuntu-training-with-validation.yaml \
  --snapshot snapshot.yaml \
  --phase all

# Validate specific phase
aicr validate \
  --recipe eks-gb200-ubuntu-training-with-validation.yaml \
  --snapshot snapshot.yaml \
  --phase deployment
```

## Multi-Phase Validation

The `eks-gb200-ubuntu-training-with-validation.yaml` example demonstrates the multi-phase validation system:

### Validation Phases

1. **Readiness**: Validates infrastructure prerequisites
   - K8s version, OS, kernel compatibility
   - GPU hardware detection
   - System parameter configuration

2. **Deployment**: Validates component deployment
   - Component versions
   - Operator health checks
   - Expected resources present

3. **Performance**: Validates system performance
   - NCCL bandwidth testing
   - Network fabric health

4. **Conformance**: Validates workload-specific requirements
   - (Optional) AI/ML workload conformance

### Constraint Features

Constraints in the validation example demonstrate:
- **Severity levels**: `error` (blocks deployment) vs `warning` (informational)
- **Remediation guidance**: Actionable steps to resolve failures
- **Version comparisons**: `>=`, `<=`, `==` operators for semantic versioning

## See Also

- [CLI Reference](../../docs/user/cli-reference.md) - Complete CLI command documentation
- [Recipe Development Guide](../../docs/integrator/recipe-development.md) - Creating and modifying recipes
- [Validation Documentation](../../docs/user/cli-reference.md#aicr-validate) - Multi-phase validation details
