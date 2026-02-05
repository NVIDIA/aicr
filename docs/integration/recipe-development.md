# Recipe Development Guide

This guide covers how to create, modify, and validate recipe metadata.

## Table of Contents

- [Overview](#overview)
- [Multi-Level Inheritance](#multi-level-inheritance)
- [Component Value Configuration](#component-value-configuration)
- [Value Merge Precedence](#value-merge-precedence)
- [File Naming Conventions](#file-naming-conventions)
- [Recipe Constraints](#recipe-constraints)
- [Adding New Recipes](#adding-new-recipes)
- [Modifying Existing Recipes](#modifying-existing-recipes)
- [Best Practices](#best-practices)
- [Testing and Validation](#testing-and-validation)
- [Troubleshooting](#troubleshooting)

## Overview

Recipe metadata files define component configurations for GPU-accelerated Kubernetes deployments. The system uses a **base-plus-overlay architecture** with **multi-level inheritance**:

- **Base values** (`overlays/base.yaml`) provide default configurations
- **Intermediate recipes** (e.g., `eks.yaml`, `eks-training.yaml`) capture shared configurations
- **Leaf recipes** (e.g., `gb200-eks-ubuntu-training.yaml`) provide hardware-specific overrides
- **Inline overrides** allow per-recipe customization without creating new files

Recipe files are located in `pkg/recipe/data/` and are embedded into the CLI binary and API server at compile time.

For details on how the recipe generation process works (query matching, overlay merging), see the [Data Architecture](../architecture/data.md) document.

## Multi-Level Inheritance

Recipes support multi-level inheritance through the `spec.base` field. This enables inheritance chains where intermediate recipes capture shared configurations.

### Inheritance Structure

```yaml
kind: recipeMetadata
apiVersion: eidos.nvidia.com/v1alpha1
metadata:
  name: gb200-eks-ubuntu-training

spec:
  base: eks-training  # Inherits from eks-training (which inherits from eks)
  
  criteria:
    service: eks
    accelerator: gb200
    os: ubuntu
    intent: training
    
  # Only GB200-specific overrides here
  componentRefs:
    - name: gpu-operator
      overrides:
        driver:
          version: 580.82.07
```

### Inheritance Chain Example

```
overlays/base.yaml (foundation)
    │
    └── overlays/eks.yaml (EKS-specific settings)
            │
            └── overlays/eks-training.yaml (training optimizations)
                    │
                    └── overlays/h100-eks-training.yaml (H100 + training overrides)
                            │
                            └── overlays/h100-eks-ubuntu-training.yaml (+ OS specifics)
                                    │
                                    └── overlays/h100-eks-ubuntu-training-kubeflow.yaml (+ platform specifics)
```

**Note:** Platform (kubeflow) is always the most specific criteria and appears at the end of the inheritance chain.

### Creating an Intermediate Recipe

Intermediate recipes have **partial criteria** and are not matched directly by generic user queries (unless the query also has matching criteria). They capture shared configurations for a category:

```yaml
# overlays/eks.yaml - Intermediate recipe for all EKS deployments
kind: recipeMetadata
apiVersion: eidos.nvidia.com/v1alpha1
metadata:
  name: eks

spec:
  # No spec.base = inherits from overlays/base.yaml
  
  criteria:
    service: eks  # Only service specified (partial criteria)

  constraints:
    - name: K8s.server.version
      value: ">= 1.28"  # EKS minimum version
```

```yaml
# eks-training.yaml - Training settings for EKS
kind: recipeMetadata
apiVersion: eidos.nvidia.com/v1alpha1
metadata:
  name: eks-training

spec:
  base: eks  # Inherits from eks
  
  criteria:
    service: eks
    intent: training  # Added training intent (still partial)

  constraints:
    - name: K8s.server.version
      value: ">= 1.30"  # Training requires newer K8s

  componentRefs:
    - name: gpu-operator
      valuesFile: components/gpu-operator/values-eks-training.yaml
```

### Creating a Leaf Recipe

Leaf recipes have **complete criteria** (all required fields) and are matched by user queries:

```yaml
# gb200-eks-training.yaml - Intermediate: GB200 + EKS + training
kind: recipeMetadata
apiVersion: eidos.nvidia.com/v1alpha1
metadata:
  name: gb200-eks-training

spec:
  base: eks-training  # Inherits from eks-training
  
  criteria:
    service: eks
    accelerator: gb200
    intent: training  # Partial criteria (no OS)

  componentRefs:
    - name: gpu-operator
      overrides:
        driver:
          version: 580.82.07  # GB200-specific driver
```

```yaml
# gb200-eks-ubuntu-training.yaml - Full specification
kind: recipeMetadata
apiVersion: eidos.nvidia.com/v1alpha1
metadata:
  name: gb200-eks-ubuntu-training

spec:
  base: gb200-eks-training  # Inherits from gb200-eks-training
  
  criteria:
    service: eks
    accelerator: gb200
    os: ubuntu
    intent: training  # Complete criteria

  constraints:
    - name: OS.release.ID
      value: ubuntu
    - name: OS.release.VERSION_ID
      value: "24.04"

  componentRefs:
    - name: gpu-operator
      overrides:
        driver:
          version: 580.82.07
```

### Inheritance Merge Order

When resolving a leaf recipe, the system merges in order from root to leaf:

```
1. overlays/base.yaml (lowest precedence)
2. overlays/eks.yaml
3. overlays/eks-training.yaml
4. overlays/gb200-eks-ubuntu-training.yaml (highest precedence)
```

**Merge rules:**
- **Constraints**: Same-named constraints are overridden; new ones are added
- **ComponentRefs**: Same-named components merge field-by-field; new ones are added
- **Criteria**: Not inherited (each recipe defines its own)

## Component Types

The recipe system supports two deployment types for components:

### Helm Components

Helm components use Helm charts for deployment. They are configured via the `helm` section in the component registry and support values files and inline overrides.

```yaml
componentRefs:
  - name: gpu-operator
    type: Helm
    source: https://helm.ngc.nvidia.com/nvidia
    version: v25.3.3
    valuesFile: components/gpu-operator/values.yaml
    overrides:
      driver:
        version: 580.82.07
```

### Kustomize Components

Kustomize components use Kustomize for deployment. They are configured via the `kustomize` section in the component registry and support Git/OCI sources with path and tag specifications.

```yaml
componentRefs:
  - name: my-kustomize-app
    type: Kustomize
    source: https://github.com/example/my-app
    tag: v1.0.0
    path: deploy/production
    patches:
      - patches/custom-patch.yaml
```

**Note:** A component in the registry must have either `helm` OR `kustomize` configuration, not both. The component type is automatically determined based on which configuration is present.

## Component Value Configuration

The bundler supports three patterns for configuring Helm component values:

### Pattern 1: ValuesFile Only (Basic)

All configuration comes from a separate values file. Best for large configurations that are reusable across multiple recipes.

```yaml
componentRefs:
  - name: cert-manager
    type: Helm
    version: v1.17.3
    valuesFile: components/cert-manager/eks-values.yaml
    # No overrides - everything in the file
```

**When to use:**
- Large configurations (100+ lines)
- Reusable across multiple recipes
- Team collaboration with clear file ownership
- Separate overlay files already exist

### Pattern 2: Overrides Only (Self-Contained)

All configuration is inline in the recipe - no separate values file needed. Best for small configurations or recipe-specific deployments.

```yaml
componentRefs:
  - name: nvsentinel
    type: Helm
    version: v0.6.0
    # Note: No valuesFile specified
    overrides:
      namespace: nvsentinel
      sentinel:
        enabled: true
        logLevel: info
        metrics:
          enabled: true
      resources:
        limits:
          memory: 256Mi
        requests:
          cpu: 100m
          memory: 128Mi
```

**When to use:**
- Small configurations (<50 lines)
- Unique, recipe-specific settings
- One-off deployments or testing
- Self-contained recipes (no external dependencies)

### Pattern 3: Hybrid (ValuesFile + Overrides)

Base configuration in a values file, with recipe-specific tweaks as inline overrides. Best for large shared configurations with small per-recipe customizations.

```yaml
componentRefs:
  # Example 1: Override just one field
  - name: gpu-operator
    type: Helm
    version: v25.3.4
    valuesFile: components/gpu-operator/eks-gb200-training.yaml
    overrides:
      # Override just the driver version for this specific deployment
      driver:
        version: "570.86.16"
      # Add deployment-specific feature flag not in base file
      experimental:
        newFeature: true

  # Example 2: Override multiple sections with deep merge
  - name: network-operator
    type: Helm
    version: v25.4.0
    valuesFile: components/network-operator/values.yaml
    overrides:
      # Override operator configuration
      operator:
        repository: nvcr.io/custom-registry
        tag: v25.4.0-custom
      # Override RDMA settings
      rdma:
        enabled: true
        useHostMofed: false
      # Add new field not in base values
      sriov:
        enabled: true
        numVfs: 8
```

**When to use:**
- Large base configuration with small recipe-specific tweaks
- Environment-specific overrides (dev/staging/prod)
- Version pinning per deployment
- Feature flags or experimental settings

## Value Merge Precedence

Values are merged in this order (later sources override earlier ones):

```
Base Values (lowest precedence)
    ↓
ValuesFile (overlay)
    ↓
Overrides (highest precedence)
    ↓
CLI --set flags (user has last word)
```

**Deep merge behavior:**
- Only specified fields in overrides are replaced
- Unspecified fields are preserved from base/ValuesFile
- New fields in overrides are added to the final configuration
- Arrays are replaced entirely (not merged element-by-element)
  
> **Note:** Users can override the final recipe state with `--set` flags on `eidos bundle`.

**Example:**

Base values (`components/gpu-operator/base.yaml`):
```yaml
driver:
  version: "550.54.15"
  repository: nvcr.io/nvidia
  image: driver
gds:
  enabled: false
```

Overlay values (`components/gpu-operator/eks-gb200-training.yaml`):
```yaml
driver:
  version: "570.86.16"  # Override
gds:
  enabled: true         # Override
```

Recipe with inline overrides:
```yaml
valuesFile: components/gpu-operator/eks-gb200-training.yaml
overrides:
  driver:
    version: "580.13.01"  # Override again
```

**Final merged result:**
```yaml
driver:
  version: "580.13.01"      # From inline override (highest)
  repository: nvcr.io/nvidia  # From base (preserved)
  image: driver               # From base (preserved)
gds:
  enabled: true              # From overlay valuesFile
```

## File Naming Conventions

File names are for human readability only—the recipe engine matches based on `spec.criteria` fields, not file names. Consistent naming helps with discovery and maintenance.

**Overlay Naming Order:** `{accelerator}-{service}-{os}-{intent}-{platform}.yaml`

The naming convention places criteria in order of specificity, with **platform always at the end**:
1. Accelerator (h100, gb200)
2. Service (eks, gke)
3. OS (ubuntu, rhel)
4. Intent (training, inference)
5. Platform (kubeflow)

| File Type | Naming Pattern | Examples |
|-----------|---------------|----------|
| Base recipe | `overlays/base.yaml` | `overlays/base.yaml` |
| Service overlay | `{service}.yaml` | `eks.yaml`, `gke.yaml` |
| Service + intent | `{service}-{intent}.yaml` | `eks-training.yaml` |
| Accelerator + service + intent | `{accel}-{service}-{intent}.yaml` | `h100-eks-training.yaml` |
| Full criteria | `{accel}-{service}-{os}-{intent}.yaml` | `h100-eks-ubuntu-training.yaml` |
| Full criteria + platform | `{accel}-{service}-{os}-{intent}-{platform}.yaml` | `h100-eks-ubuntu-training-kubeflow.yaml` |
| Component values (base) | `base.yaml` or `values.yaml` | `components/gpu-operator/base.yaml` |
| Component values (overlay) | `values-{service}-{intent}.yaml` | `components/gpu-operator/values-eks-training.yaml` |

## Recipe Constraints

Constraints define deployment requirements that can be validated against a cluster snapshot before deployment. They ensure the target environment meets prerequisites (Kubernetes version, OS, kernel version, etc.).

### Constraint Structure

Each constraint has the following fields:

```yaml
constraints:
  - name: <measurement-path>   # What to check
    value: <expression>        # Expected value or comparison
    severity: <level>          # Optional: error (default), warning
    remediation: <string>      # Optional: How to fix if failed
    unit: <string>             # Optional: Unit of measurement
```

- **`name`**: A fully qualified measurement path in the format `{Type}.{Subtype}.{Key}`
- **`value`**: An exact match string or comparison expression with operator
- **`severity`**: Optional severity level (`error` or `warning`). Defaults to `error`.
- **`remediation`**: Optional guidance on how to resolve a constraint failure
- **`unit`**: Optional unit of measurement (e.g., `GB`, `cores`, `seconds`)

### Measurement Path Format

Constraint names use dot-notation paths that map to snapshot measurements:

| Path | Description | Example Values |
|------|-------------|----------------|
| `K8s.server.version` | Kubernetes API server version | `1.32.4`, `1.30.0` |
| `OS.release.ID` | Operating system identifier | `ubuntu`, `rhel`, `cos` |
| `OS.release.VERSION_ID` | OS version number | `24.04`, `22.04`, `9.4` |
| `OS.sysctl./proc/sys/kernel/osrelease` | Kernel version | `6.8.0-1028-aws` |
| `GPU.info.type` | GPU hardware type | `H100`, `GB200`, `A100` |
| `GPU.smi.driver-version` | NVIDIA driver version | `580.82.07` |
| `GPU.smi.cuda-version` | CUDA version | `13.1` |

### Supported Operators

| Operator | Example | Description |
|----------|---------|-------------|
| `>=` | `>= 1.30` | Greater than or equal (semantic version comparison) |
| `<=` | `<= 1.33` | Less than or equal |
| `>` | `> 1.30` | Greater than |
| `<` | `< 2.0` | Less than |
| `==` | `== ubuntu` | Explicit equality |
| `!=` | `!= rhel` | Not equal |
| *(none)* | `ubuntu` | Exact string match |

### When to Add Constraints

**Add constraints when:**
- Recipe requires specific Kubernetes version features
- Components need particular driver or CUDA versions
- Configuration assumes specific OS or kernel capabilities
- Hardware requirements must be validated before deployment

**Skip constraints when:**
- Requirements are universal (covered by base recipe)
- Validation would be redundant with component self-checks
- Flexibility is preferred over strict enforcement

### Example: GB200 Training Recipe Constraints

```yaml
# pkg/recipe/data/overlays/gb200-eks-ubuntu-training.yaml
spec:
  criteria:
    service: eks
    accelerator: gb200
    os: ubuntu
    intent: training

  constraints:
    # Kubernetes version for required APIs
    - name: K8s.server.version
      value: ">= 1.32.4"
    
    # OS family (exact match)
    - name: OS.release.ID
      value: ubuntu
    
    # Specific Ubuntu version for driver compatibility
    - name: OS.release.VERSION_ID
      value: "24.04"
    
    # Minimum kernel version for GPU features
    - name: OS.sysctl./proc/sys/kernel/osrelease
      value: ">= 6.8"

  componentRefs:
    - name: gpu-operator
      # ... component configuration
```

## Validation Configuration

Recipes can include a `validation` section that defines phase-specific validation checks beyond basic constraints. This enables multi-phase validation during the deployment lifecycle.

### Validation Structure

```yaml
validation:
  readiness:
    checks:
      - gpu-hardware-detection
      - kernel-parameters
      - os-prerequisites
  deployment:
    constraints:
      - name: gpu-operator.version
        value: "== v25.10.1"
    checks:
      - operator-health
      - expected-resources
    expectedResources:
      - apiVersion: v1
        kind: Pod
        namespace: gpu-operator
        selector:
          matchLabels:
            app: nvidia-operator-validator
  performance:
    infrastructure: nccl-doctor
    checks:
      - nccl-bandwidth-test
      - fabric-health-check
  conformance:
    checks:
      - ai-workload-validation
```

### Validation Phases

| Phase | Fields | Description |
|-------|--------|-------------|
| `readiness` | `checks` | Readiness validation checks (infrastructure readiness) |
| `deployment` | `constraints`, `checks`, `expectedResources` | Component deployment validation |
| `performance` | `infrastructure`, `checks` | Performance and network fabric validation |
| `conformance` | `checks` | Workload-specific conformance validation |

### Phase Fields

- **`checks`**: List of check names to execute in this phase
- **`constraints`**: Phase-specific constraints (in addition to recipe-level constraints)
- **`expectedResources`**: Kubernetes resources that should exist after deployment
- **`infrastructure`**: Infrastructure component to deploy for testing (e.g., `nccl-doctor`)

### Node Selection

Validation checks can target specific node types using `nodeSelection`:

```yaml
validation:
  performance:
    nodeSelection:
      gpu:
        nodeSelector:
          nvidia.com/gpu.present: "true"
        tolerations:
          - key: nvidia.com/gpu
            operator: Equal
            value: present
            effect: NoSchedule
```

### When to Add Validation Configuration

**Add validation configuration when:**
- Recipe deploys components that require health checks
- Performance testing is critical for the workload
- Specific Kubernetes resources must be verified
- Phase-specific validation is needed beyond basic constraints

**Example: Training Workload with Multi-Phase Validation**

```yaml
# gb200-eks-ubuntu-training.yaml
spec:
  criteria:
    service: eks
    accelerator: gb200
    os: ubuntu
    intent: training

  constraints:
    - name: K8s.server.version
      value: ">= 1.32.4"
    - name: OS.release.ID
      value: ubuntu

  validation:
    readiness:
      checks:
        - gpu-hardware-detection
        - kernel-parameters
    deployment:
      checks:
        - operator-health
        - expected-resources
    performance:
      infrastructure: nccl-doctor
      checks:
        - nccl-bandwidth-test
        - fabric-health-check

  componentRefs:
    - name: gpu-operator
      version: v25.3.4
      valuesFile: components/gpu-operator/gb200-eks-training.yaml
```

### Testing Validation Configuration

**Run phase-specific validation:**

```bash
# Validate readiness phase
eidos validate --recipe recipe.yaml --snapshot snapshot.yaml --phase readiness

# Validate all phases
eidos validate --recipe recipe.yaml --snapshot snapshot.yaml --phase all

# Validate specific phase
eidos validate --recipe recipe.yaml --snapshot snapshot.yaml --phase deployment
```

### Testing Constraints

**Validate constraints against a snapshot:**

```bash
# Validate recipe constraints against cluster snapshot
eidos validate --recipe recipe.yaml --snapshot snapshot.yaml

# With ConfigMap sources
eidos validate \
  --recipe cm://gpu-operator/eidos-recipe \
  --snapshot cm://gpu-operator/eidos-snapshot

# Fail on constraint violations (for CI/CD)
eidos validate \
  --recipe recipe.yaml \
  --snapshot snapshot.yaml \
  --fail-on-error
```

**Example output:**

```yaml
apiVersion: eidos.nvidia.com/v1alpha1
kind: ValidationResult
summary:
  passed: 3
  failed: 1
  skipped: 0
  status: fail
results:
  - constraint: K8s.server.version >= 1.32.4
    status: pass
    actual: "1.33.5"
  - constraint: OS.release.ID == ubuntu
    status: pass
    actual: "ubuntu"
  - constraint: OS.release.VERSION_ID == 24.04
    status: fail
    actual: "22.04"
    message: "expected 24.04, got 22.04"
```

**Run constraint syntax validation:**

```bash
# Verify constraint paths use valid measurement types
go test -v ./pkg/recipe/... -run TestConstraintPathsUseValidMeasurementTypes

# Verify constraint operators are valid
go test -v ./pkg/recipe/... -run TestConstraintValuesHaveValidOperators
```

## Adding New Recipes

### Adding a New Overlay Recipe

**When to add:**
- New platform (cloud provider)
- New hardware (GPU model)
- New workload type (training vs inference)
- Combined criteria (e.g., EKS + GB200 + training)

**Steps:**

1. **Create the recipe file** in `pkg/recipe/data/`:
   ```yaml
   # pkg/recipe/data/overlays/gke-h100-inference.yaml
   apiVersion: eidos.nvidia.com/v1alpha1
   kind: RecipeMetadata
   metadata:
     name: gke-h100-inference
     version: v1.0.0
   spec:
     criteria:
       service: gke
       accelerator: h100
       intent: inference
     componentRefs:
       - name: gpu-operator
         type: Helm
         version: v25.3.4
         valuesFile: components/gpu-operator/gke-h100-inference.yaml
   ```

2. **Create component values** if using `valuesFile`:
   ```yaml
   # pkg/recipe/data/components/gpu-operator/gke-h100-inference.yaml
   driver:
     version: "570.86.16"
   mig:
     strategy: single
   ```

3. **Run validation tests**:
   ```bash
   go test -v ./pkg/recipe/... -run TestAllMetadataFilesConformToSchema
   go test -v ./pkg/recipe/... -run TestNoDuplicateCriteriaAcrossOverlays
   ```

4. **Test recipe generation**:
   ```bash
   eidos recipe --service gke --gpu h100 --intent inference --format yaml
   ```

### Adding Component Values Files

**Steps:**

1. **Create the values file** in `pkg/recipe/data/components/{component}/`:
   ```yaml
   # pkg/recipe/data/components/network-operator/eks-gb200-training.yaml
   rdma:
     enabled: true
   sriov:
     enabled: true
     numVfs: 8
   ```

2. **Reference in recipe**:
   ```yaml
   componentRefs:
     - name: network-operator
       valuesFile: components/network-operator/eks-gb200-training.yaml
   ```

3. **Validate the file**:
   ```bash
   go test -v ./pkg/recipe/... -run TestAllValuesFileReferencesExist
   ```

## Modifying Existing Recipes

### Updating Version Numbers

**When to update:**
- New component releases (GPU Operator, Network Operator)
- Driver or CUDA version updates
- Security patches

**Steps:**

1. **Locate the recipe file** in `pkg/recipe/data/`

2. **Update the version**:
   ```yaml
   # Before
   componentRefs:
     - name: gpu-operator
       version: v25.3.3
   
   # After
   componentRefs:
     - name: gpu-operator
       version: v25.3.4
   ```

3. **Update any related values files** if driver/CUDA versions changed

4. **Test the change**:
   ```bash
   eidos recipe --service eks --gpu gb200 --intent training --format yaml
   ```

### Adding New Components

**Steps:**

1. **Add the component reference**:
   ```yaml
   componentRefs:
     - name: existing-component
       ...
     - name: new-component
       type: Helm
       version: v1.0.0
       valuesFile: components/new-component/values.yaml
       dependencyRefs:
         - existing-component  # If depends on another component
   ```

2. **Create the values file** if needed

3. **Verify the bundler exists**:
   ```bash
   go test -v ./pkg/recipe/... -run TestComponentNamesMatchRegisteredBundlers
   ```

## Best Practices

### Recipe Organization

1. **Overlay Ordering**
   - Place more general overlays first
   - Specific overlays (multiple key fields) later
   - Order doesn't affect matching, but aids readability

2. **Key Field Selection**
   - Use minimum fields needed for matching
   - Avoid over-specification (too many fields = fewer matches)
   - Combine related conditions in single overlay when possible

3. **Context Documentation**
   - Always explain **why** a setting exists
   - Describe **impact** on GPU workloads
   - Keep explanations concise (1-2 sentences)
   - Update context when values change

4. **Value Formats**
   - Use consistent formatting (lowercase for enums)
   - Include units where applicable (2M, 8192)
   - Use semantic versions (v1.33.5)
   - Boolean values as strings: "true"/"false"

### Common Pitfalls

❌ **Don't:**
- Add environment-specific settings to base
- Create overlays with no matching queries
- Forget to update context when changing values
- Use inconsistent naming conventions
- Over-specify overlay keys (too narrow)
- Create duplicate criteria combinations

✅ **Do:**
- Keep base universal and conservative
- Test overlays match expected queries
- Always provide context explanations
- Follow existing naming patterns
- Use wildcard fields in overlay keys
- Run validation tests before committing

## Testing and Validation

### Automated Test Suite

All recipe metadata and component values are automatically validated by the test suite located in [`pkg/recipe/data_test.go`](../../pkg/recipe/data_test.go).

| Test Category | What It Validates |
|---------------|-------------------|
| Schema Conformance | All YAML files parse correctly with expected structure |
| Criteria Validation | Valid enum values for service, accelerator, intent, OS, platform |
| Reference Validation | valuesFile paths exist, dependencyRefs resolve, component names valid |
| Constraint Syntax | Measurement paths use valid types, operators are valid |
| Uniqueness | No duplicate criteria combinations across overlays |
| Merge Consistency | Base + overlay merges without data loss |
| Dependency Cycles | No circular dependencies in componentRefs |
| Component Types | All bundler types are registered and available |
| Values Files | Component values files parse as valid YAML |

### Running Tests

```bash
# Run all recipe data tests
make test

# Run only recipe package tests
go test -v ./pkg/recipe/... -count=1

# Run specific validation test
go test -v ./pkg/recipe/... -run TestAllMetadataFilesConformToSchema
```

### Test Workflow for New Recipes

When adding new recipe metadata or component configurations:

1. **Create the new file** in `pkg/recipe/data/`

2. **Verify schema compliance**:
   ```bash
   go test -v ./pkg/recipe/... -run TestAllMetadataFilesConformToSchema
   ```

3. **Check for duplicate criteria**:
   ```bash
   go test -v ./pkg/recipe/... -run TestNoDuplicateCriteriaAcrossOverlays
   ```

4. **Verify file references** (if using valuesFile or dependencyRefs):
   ```bash
   go test -v ./pkg/recipe/... -run TestAllValuesFileReferencesExist
   go test -v ./pkg/recipe/... -run TestAllDependencyReferencesExist
   ```

5. **Test recipe generation**:
   ```bash
   eidos recipe --service eks --gpu gb200 --intent training --format yaml
   ```

6. **Generate and inspect bundle**:
   ```bash
   eidos bundle -r pkg/recipe/data/overlays/your-recipe.yaml -o ./test-bundles
   cat test-bundles/gpu-operator/values.yaml | grep -A5 "driver:"
   ```

### CI/CD Integration

Tests run automatically on:
- **Pull Requests**: All tests must pass before merge
- **Push to main**: Validates no regressions
- **Release builds**: Ensures data integrity in released binaries

The test file uses Go's `embed` directive to load recipe data at compile time, ensuring tests validate the same embedded data that ships in the CLI and API binaries.

## Troubleshooting

### Debugging Overlay Matching

**See which overlays matched:**
```bash
eidos recipe --service eks --accelerator gb200 --format json | jq '.metadata.appliedOverlays'
```

**Output:**
```json
[
  "base",
  "eks",
  "eks-training",
  "gb200-eks-training"
]
```

**Extract component versions:**
```bash
eidos recipe --service eks --accelerator gb200 --format json | \
  jq '.componentRefs[] | select(.name=="gpu-operator") | .version'
```

### Common Issues

| Issue | Cause | Solution |
|-------|-------|----------|
| Test fails: "duplicate criteria" | Two overlays have identical criteria | Combine overlays or differentiate criteria |
| Test fails: "valuesFile not found" | Referenced file doesn't exist | Create the file or fix the path |
| Test fails: "unknown component" | Component name doesn't match bundler | Use registered bundler name |
| Recipe returns empty | No overlay matches query | Check criteria fields match query |
| Wrong values in bundle | Merge precedence issue | Check override order |

### Validation Commands

```bash
# Validate YAML syntax
yamllint pkg/recipe/data/overlays/your-recipe.yaml

# Run all recipe tests
go test -v ./pkg/recipe/... -count=1

# Test specific recipe generation
eidos recipe --service eks --gpu gb200 --format yaml

# Full qualification
make qualify
```

---

## See Also

- [Data Architecture](../architecture/data.md) - Recipe generation process, overlay system, query matching algorithm
- [Bundler Development Guide](../architecture/component.md) - Creating new bundlers
- [CLI Reference](../user-guide/cli-reference.md) - CLI commands for recipe and bundle generation
- [API Reference](api-reference.md) - Programmatic recipe access
