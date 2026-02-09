# Deployment Phase Constraint Validators

This package implements constraint validators for the deployment phase. These validators query live Kubernetes clusters to evaluate constraints against deployed resources.

## Overview

Deployment phase constraints run **inside Kubernetes Jobs** with cluster access, unlike readiness constraints which evaluate inline from snapshot data.

**Key Difference:**
- **Readiness**: Validates prerequisites from snapshot (no cluster access needed)
- **Deployment**: Validates deployed resources (requires live cluster access)

## Registered Constraint Validators

### GPU Operator Version

**Constraint Name:** `Deployment.gpu-operator.version`

**Purpose:** Validates the deployed GPU operator version against version constraints.

**Constraint Syntax:**
```yaml
constraints:
  - name: Deployment.gpu-operator.version
    value: ">= v24.6.0"  # Supports: ==, !=, >=, <=, >, <, ~=
```

**Version Detection Strategy:**

The validator tries multiple strategies to determine the GPU operator version:

1. **Deployment Labels** - Checks `app.kubernetes.io/version` label
2. **Container Image Tag** - Parses version from image (e.g., `nvcr.io/nvidia/gpu-operator:v24.6.0`)
3. **Deployment Annotations** - Checks `nvidia.com/gpu-operator-version` annotation

**Namespace Search:**

Searches for GPU operator deployment in common namespaces:
- `gpu-operator`
- `nvidia-gpu-operator`
- `kube-system`

**Deployment Names:**

Looks for common deployment names:
- `gpu-operator`
- `nvidia-gpu-operator`

**Image Tag Handling:**

The validator handles various image tag formats:

| Image Tag | Extracted Version |
|-----------|-------------------|
| `nvcr.io/nvidia/gpu-operator:v24.6.0` | `v24.6.0` |
| `nvcr.io/nvidia/gpu-operator:v24.6.0-ubuntu22.04` | `v24.6.0` (strips OS suffix) |
| `docker.io/nvidia/gpu-operator:24.6.0` | `v24.6.0` (adds v prefix) |

**Example Usage:**

```go
import (
    "github.com/NVIDIA/eidos/pkg/recipe"
    "github.com/NVIDIA/eidos/pkg/validator/checks"
)

// Get the registered validator
validator, ok := checks.GetConstraintValidator("Deployment.gpu-operator.version")
if !ok {
    log.Fatal("Constraint validator not found")
}

// Create constraint
constraint := recipe.Constraint{
    Name:  "Deployment.gpu-operator.version",
    Value: ">= v24.6.0",
}

// Execute validator
actualVersion, passed, err := validator.Func(ctx, constraint)
if err != nil {
    log.Fatalf("Validation failed: %v", err)
}

log.Printf("Detected version: %s, Passed: %v", actualVersion, passed)
```

**Return Values:**

- `actual` (string): The detected GPU operator version (e.g., "v24.6.0")
- `passed` (bool): Whether the constraint was satisfied
- `error`: Non-nil if version detection or evaluation failed

**Error Conditions:**

- GPU operator deployment not found in any namespace
- Unable to determine version from any strategy
- Invalid constraint expression syntax
- Kubernetes API errors

## Testing

The package includes comprehensive tests:

### Unit Tests

- `TestValidateGPUOperatorVersion` - Tests constraint evaluation with various version scenarios
- `TestExtractVersionFromImage` - Tests image tag parsing logic
- `TestNormalizeVersion` - Tests version normalization (adding 'v' prefix)

### Integration Tests

- `TestConstraintValidatorRegistration` - Verifies validator is properly registered
- `TestConstraintValidatorIntegration` - Full end-to-end flow with passing constraint
- `TestConstraintValidatorWithFailingConstraint` - Full flow with failing constraint

Run tests:
```bash
go test -v ./pkg/validator/checks/deployment/...
```

## Architecture

```
Recipe Constraint
    ↓
Validator.ValidateDeployment()
    ↓
checks.GetConstraintValidator("Deployment.gpu-operator.version")
    ↓
ValidateGPUOperatorVersion(ctx, constraint)
    ↓
┌─────────────────────────────────────┐
│ 1. Search namespaces for deployment │
│ 2. Try version detection strategies  │
│ 3. Parse and normalize version       │
│ 4. Evaluate constraint expression    │
└─────────────────────────────────────┘
    ↓
Return (actualVersion, passed, error)
```

## Adding New Constraint Validators

1. **Define validator function** with signature:
   ```go
   func ValidateMyConstraint(ctx *checks.ValidationContext, constraint recipe.Constraint) (string, bool, error)
   ```

2. **Register in init()**:
   ```go
   func init() {
       checks.RegisterConstraintValidator(&checks.ConstraintValidator{
           Pattern:     "Deployment.my-resource.property",
           Description: "Validates my resource property",
           Func:        ValidateMyConstraint,
       })
   }
   ```

3. **Implement validation logic**:
   - Query cluster via `ctx.Clientset`
   - Extract actual value
   - Evaluate constraint using `validator.ParseConstraintExpression()`
   - Return actual value, pass/fail status, and error

4. **Write tests**:
   - Use `fake.NewSimpleClientset()` for unit tests
   - Test various constraint expressions
   - Test error conditions

## References

- Parent README: `pkg/validator/checks/README.md`
- Registry implementation: `pkg/validator/checks/registry.go`
- Constraint expression parser: `pkg/validator/constraint_expression.go`
- Example usage: `pkg/validator/phases.go::validateDeployment()`
