# Constraint Expression Reference

This document describes the constraint expression language used in Eidos validation recipes.

## Overview

Constraint expressions define expected values for cluster properties. The validator evaluates actual values against these expressions to determine if constraints are satisfied.

**Basic syntax:**
```yaml
constraints:
  - name: Property.name
    value: "operator expected_value"
```

## Operators

### Comparison Operators

| Operator | Name | Description | Example | Use Case |
|----------|------|-------------|---------|----------|
| `>=` | Greater than or equal | Version or number is at least the specified value | `">= v24.6.0"` | Minimum version requirements |
| `<=` | Less than or equal | Version or number is at most the specified value | `"<= v25.0.0"` | Maximum version constraints |
| `>` | Greater than | Version or number is strictly greater | `"> 100"` | Threshold validation |
| `<` | Less than | Version or number is strictly less | `"< 1000"` | Upper bound checks |
| `==` | Equal | Exact match (version or string) | `"== v24.6.0"` | Exact version pinning |
| `!=` | Not equal | Must not match | `"!= v23.0.0"` | Exclude specific versions |
| (none) | Exact string match | Case-sensitive string equality | `"ubuntu"` | OS or simple string matching |

### Operator Precedence

When parsing expressions, operators are checked in this order to avoid ambiguity:
1. `>=` (checked before `>`)
2. `<=` (checked before `<`)
3. `!=` (checked before `==`)
4. `==`
5. `>`
6. `<`

## Version Comparison

### Version Detection

The parser automatically detects version comparisons based on:

1. **Operator used**: `>=`, `<=`, `>`, `<` always trigger version comparison
2. **Value format**: Values containing digits and dots (e.g., `1.2.3`, `v24.6.0`)

### Version Formats

Supported version formats:

| Format | Example | Normalized As | Notes |
|--------|---------|---------------|-------|
| Semantic | `1.2.3` | `v1.2.3` | Standard semantic versioning |
| With 'v' prefix | `v24.6.0` | `v24.6.0` | Common in Kubernetes |
| Major.Minor | `1.32` | `v1.32` | Minor version only |
| With build metadata | `1.2.3-alpha` | `v1.2.3-alpha` | Pre-release versions |

### Version Comparison Rules

Versions are compared using semantic versioning logic:

```
v1.2.3 < v1.2.4  (patch increment)
v1.2.9 < v1.3.0  (minor increment)
v1.9.0 < v2.0.0  (major increment)

v1.2.3-alpha < v1.2.3  (pre-release is less than release)
```

**Examples:**

```yaml
# Minimum version
- name: Deployment.gpu-operator.version
  value: ">= v24.6.0"
  # Passes: v24.6.0, v24.6.1, v24.7.0, v25.0.0
  # Fails: v24.5.9, v24.3.0, v23.0.0

# Exact version
- name: Deployment.gpu-operator.version
  value: "== v24.6.0"
  # Passes: v24.6.0 only
  # Fails: v24.6.1, v24.5.9

# Version range (requires multiple constraints)
- name: Deployment.gpu-operator.version
  value: ">= v24.6.0"
- name: Deployment.gpu-operator.version
  value: "< v25.0.0"
  # Passes: v24.6.0 <= version < v25.0.0
```

## String Comparison

### Exact Match (No Operator)

When no operator is specified, the constraint performs case-sensitive string comparison:

```yaml
# OS validation
- name: OS.distribution
  value: "ubuntu"
  # Passes: "ubuntu"
  # Fails: "Ubuntu", "UBUNTU", "rhel", "ubuntu22.04"

# Architecture
- name: Hardware.architecture
  value: "x86_64"
  # Passes: "x86_64"
  # Fails: "amd64", "arm64"
```

### Equality Operators with Strings

Use `==` for explicit equality (same as no operator for strings):

```yaml
- name: Service.type
  value: "== LoadBalancer"
  # Identical to: value: "LoadBalancer"
```

Use `!=` to exclude specific values:

```yaml
- name: Service.type
  value: "!= ClusterIP"
  # Passes: "LoadBalancer", "NodePort"
  # Fails: "ClusterIP"
```

## Numeric Comparison

Numeric constraints work with integer and decimal values:

```yaml
# Replica count
- name: Deployment.replicas
  value: ">= 1"
  # Passes: 1, 2, 3, ...
  # Fails: 0

# CPU cores
- name: Hardware.cpu.cores
  value: ">= 64"

# Memory in GB
- name: Hardware.memory.total
  value: ">= 512"

# Percentage
- name: Performance.gpu.utilization
  value: ">= 80"
  # Validates 80% or higher
```

## Boolean Comparison

Boolean constraints use string comparison with `"true"` or `"false"`:

```yaml
# Feature flag
- name: Feature.nccl.enabled
  value: "== true"
  # Passes: "true"
  # Fails: "false", "True", "TRUE", "1"

# Disabled feature
- name: Feature.legacy.enabled
  value: "== false"
```

## Common Patterns

### Pattern 1: Minimum Version Requirement

**Use case:** Ensure a component meets minimum version requirements

```yaml
constraints:
  - name: Deployment.gpu-operator.version
    value: ">= v24.6.0"
  - name: Kubernetes.version
    value: ">= v1.28.0"
```

### Pattern 2: Version Exclusion

**Use case:** Exclude known problematic versions

```yaml
constraints:
  - name: Deployment.gpu-operator.version
    value: "!= v24.5.0"  # Known bug in this version
```

### Pattern 3: Exact Version Pinning

**Use case:** Ensure specific version for compatibility

```yaml
constraints:
  - name: CUDA.version
    value: "== 12.4"
```

### Pattern 4: Resource Count Validation

**Use case:** Ensure sufficient resources deployed

```yaml
constraints:
  - name: Deployment.device-plugin.replicas
    value: ">= 1"
  - name: GPU.count
    value: ">= 8"
```

### Pattern 5: Threshold Validation

**Use case:** Performance or capacity thresholds

```yaml
constraints:
  - name: Performance.nccl.bandwidth
    value: ">= 200"  # GB/s
  - name: Performance.network.latency
    value: "< 10"    # milliseconds
```

### Pattern 6: String Enumeration

**Use case:** Validate specific configuration choices

```yaml
constraints:
  - name: OS.distribution
    value: "ubuntu"
  - name: Hardware.accelerator
    value: "h100"
```

## Constraint Validator Implementation

When implementing constraint validators, they receive the parsed constraint:

```go
func ValidateMyConstraint(
    ctx *checks.ValidationContext,
    constraint recipe.Constraint,
) (string, bool, error) {
    // constraint.Name  = "Deployment.my-resource.property"
    // constraint.Value = ">= 1.2.3"

    // 1. Query cluster for actual value
    actualValue := getActualValue(ctx)

    // 2. Parse and evaluate constraint
    parsed, err := validator.ParseConstraintExpression(constraint.Value)
    if err != nil {
        return "", false, err
    }

    passed, err := parsed.Evaluate(actualValue)
    if err != nil {
        return actualValue, false, err
    }

    // 3. Return: (actual, passed, error)
    return actualValue, passed, nil
}
```

## Examples by Phase

### Readiness Phase Constraints

```yaml
validation:
  readiness:
    constraints:
      # Hardware requirements
      - name: GPU.count
        value: ">= 8"

      # OS requirements
      - name: OS.distribution
        value: "ubuntu"
      - name: OS.version
        value: ">= 22.04"

      # Kernel requirements
      - name: Kernel.version
        value: ">= 5.15.0"

      # Driver requirements
      - name: NVIDIA.driver.version
        value: ">= 535.0.0"
```

### Deployment Phase Constraints

```yaml
validation:
  deployment:
    constraints:
      # Operator versions
      - name: Deployment.gpu-operator.version
        value: ">= v24.6.0"

      # Replica counts
      - name: Deployment.device-plugin.replicas
        value: ">= 1"

      # Configuration flags
      - name: Deployment.gpu-operator.dcgm.enabled
        value: "== true"
```

### Performance Phase Constraints

```yaml
validation:
  performance:
    constraints:
      # Bandwidth thresholds
      - name: Performance.nccl.bandwidth
        value: ">= 200"  # GB/s

      # Latency limits
      - name: Performance.network.latency
        value: "< 10"  # ms

      # GPU utilization
      - name: Performance.gpu.utilization
        value: ">= 80"  # percent
```

### Conformance Phase Constraints

```yaml
validation:
  conformance:
    constraints:
      # API version compatibility
      - name: Kubernetes.api.version
        value: ">= v1.28.0"

      # CRD versions
      - name: CRD.gpu-device.version
        value: "== v1"
```

## Error Messages

### Parse Errors

```
Error: invalid constraint expression: cannot parse expected version
Constraint: Deployment.gpu-operator.version
Value: ">= invalid"
```

### Evaluation Errors

```
Error: constraint evaluation failed: cannot parse actual version
Constraint: Deployment.gpu-operator.version
Expected: ">= v24.6.0"
Actual: "latest"
```

### Resource Not Found

```
Error: failed to detect GPU operator version: could not find GPU operator deployment in common namespaces
Constraint: Deployment.gpu-operator.version
```

## Best Practices

### 1. Use Semantic Versioning

✅ **Good:**
```yaml
value: ">= v24.6.0"
```

❌ **Avoid:**
```yaml
value: ">= 24.6"  # Missing patch version
```

### 2. Be Explicit with Operators

✅ **Good:**
```yaml
value: ">= v24.6.0"  # Clear minimum version
```

❌ **Avoid:**
```yaml
value: "v24.6.0"  # Ambiguous - exact match or minimum?
```

### 3. Document Rationale

```yaml
constraints:
  # Minimum version for NVSwitch fabric support
  - name: Deployment.gpu-operator.version
    value: ">= v24.6.0"
```

### 4. Group Related Constraints

```yaml
constraints:
  # GPU operator requirements
  - name: Deployment.gpu-operator.version
    value: ">= v24.6.0"
  - name: Deployment.device-plugin.replicas
    value: ">= 1"
```

### 5. Handle Pre-release Versions Carefully

```yaml
# Production deployment
- name: Deployment.gpu-operator.version
  value: ">= v24.6.0"  # Excludes v24.6.0-rc1, v24.6.0-beta

# Development/testing
- name: Deployment.gpu-operator.version
  value: ">= v24.6.0-rc1"  # Allows release candidates
```

## Troubleshooting

### Issue: Constraint Always Fails

**Symptom:**
```
Constraint: Deployment.gpu-operator.version
Expected: ">= v24.6.0"
Actual: "24.6.0"
Status: FAIL
```

**Cause:** Version missing 'v' prefix

**Solution:** The validator normalizes versions, but check actual value format

### Issue: String Match Fails

**Symptom:**
```
Constraint: OS.distribution
Expected: "ubuntu"
Actual: "Ubuntu"
Status: FAIL
```

**Cause:** Case-sensitive string matching

**Solution:** Ensure exact case match or use lowercase in constraints

### Issue: Numeric Comparison Not Working

**Symptom:**
```
Constraint: Deployment.replicas
Expected: ">= 1"
Actual: "one"
Status: ERROR
```

**Cause:** Actual value is string, not number

**Solution:** Ensure validator returns numeric string ("1", "2", etc.)

## Reference

- **Parser Implementation:** `pkg/validator/constraint.go`
- **Constraint Validators:** `pkg/validator/checks/*/constraints.go`
- **How to Write Validators:** `pkg/validator/checks/HOWTO.md`
- **Main Documentation:** `pkg/validator/README.md`
