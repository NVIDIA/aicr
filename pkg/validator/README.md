# Validator Package

The validator package provides a comprehensive validation framework for GPU-accelerated Kubernetes clusters. It validates cluster state against recipe specifications across multiple phases using a Job-based execution model.

## Quick Start

```go
import (
    "context"
    "github.com/NVIDIA/aicr/pkg/validator"
    "github.com/NVIDIA/aicr/pkg/recipe"
    "github.com/NVIDIA/aicr/pkg/snapshotter"
)

// Load recipe and snapshot
recipe := recipe.Load("recipe.yaml")
snapshot := snapshotter.Load("snapshot.yaml")

// Create validator
v := validator.New(validator.WithKubeconfig("/path/to/kubeconfig"))

// Validate a specific phase
result, err := v.ValidatePhase(context.Background(), "deployment", recipe, snapshot)
if err != nil {
    log.Fatal(err)
}

fmt.Printf("Status: %s, Passed: %d, Failed: %d\n",
    result.Status, result.Summary.Passed, result.Summary.Failed)
```

## Architecture

### Validation Phases

| Phase | Execution | Data Source | Purpose |
|-------|-----------|-------------|---------|
| **Readiness** | Constraints evaluated inline (no cluster access) | Snapshot only | Validate prerequisites before deployment |
| **Deployment** | All in Jobs | Snapshot + Live cluster | Verify deployed resources |
| **Performance** | All in Jobs | Snapshot + Live cluster | Measure system performance |
| **Conformance** | All in Jobs | Snapshot + Live cluster | Validate API conformance |

### Execution Model

```
Recipe Definition
    ↓
┌─────────────────────────────────────────────────────┐
│ Readiness Phase                                     │
│ • Constraints: Evaluated inline (snapshot)          │
│ • No checks — constraint-only gate                  │
└─────────────────────────────────────────────────────┘
    ↓ (if passed)
┌─────────────────────────────────────────────────────┐
│ Deployment Phase                                    │
│ • Constraints: Run in Jobs (operator versions)      │
│ • Checks: Run in Jobs (operator health, resources)  │
└─────────────────────────────────────────────────────┘
    ↓ (if passed)
┌─────────────────────────────────────────────────────┐
│ Performance Phase                                   │
│ • Constraints: Run in Jobs (bandwidth thresholds)   │
│ • Checks: Run in Jobs (NCCL tests, fabric health)   │
└─────────────────────────────────────────────────────┘
    ↓ (if passed)
┌─────────────────────────────────────────────────────┐
│ Conformance Phase                                   │
│ • Constraints: Run in Jobs (API versions)           │
│ • Checks: Run in Jobs (API conformance, workloads)  │
└─────────────────────────────────────────────────────┘
    ↓
Validation Results
```

### Job-Based Execution

All checks run inside Kubernetes Jobs for:
- **Isolation**: Proper RBAC and resource limits
- **Observability**: Jobs visible in `kubectl get jobs`
- **Reproducibility**: Consistent execution environment
- **Flexibility**: Node affinity for GPU tests

```
Validator (CLI/API)
    ↓
Agent Deployer
    ├─► RBAC (ServiceAccount, Role, RoleBinding)
    ├─► ConfigMaps (snapshot.yaml, recipe.yaml, validation-result.yaml)
    └─► Job
         ├─► Executes: go test -json (all tests in phase)
         ├─► Test wrapper loads ValidationContext
         ├─► Check functions run with snapshot + K8s client
         └─► Results output to logs (JSON format)
              └─► Validator parses logs and updates ValidationResult ConfigMap
```

### Validator Image

Validation Jobs require a special image with Go toolchain to run tests in-cluster.

**Why a Separate Image?**
- Main aicr image (built with Ko): Contains only the compiled binary, no Go toolchain
- Validator image: Contains Go toolchain + source code to run `go test` commands

**Building the Validator Image:**
```bash
# Local development (with local registry)
make image-validator IMAGE_REGISTRY=localhost:5001 IMAGE_TAG=latest

# Production release (published to GHCR)
# Automatically built by goreleaser on git tags
docker pull ghcr.io/nvidia/aicr-validator:latest
docker pull ghcr.io/nvidia/aicr-validator:v0.4.0
```

**Image Configuration:**
```go
// Default image (overridable)
v := validator.New(
    validator.WithImage("ghcr.io/nvidia/aicr-validator:latest"),
)

// Or via CLI
aicr validate --image localhost:5001/aicr-validator:latest \
  -r recipe.yaml -s snapshot.yaml

// Or via environment variable (for CI)
export AICR_VALIDATOR_IMAGE=localhost:5001/aicr-validator:local
aicr validate -r recipe.yaml -s snapshot.yaml
```

**CI/CD:**
- E2E tests build validator image from current source code
- Release pipeline publishes to `ghcr.io/nvidia/aicr-validator`
- Multi-platform support (linux/amd64, linux/arm64)
- SLSA attestation for supply chain security

**Test Wrapper Infrastructure:**

Checks execute via Go's standard test framework:

```go
// Check function (registered in init())
func CheckOperatorHealth(ctx *checks.ValidationContext) error {
    // Access live cluster via K8s API
    pods, err := ctx.Clientset.CoreV1().Pods("gpu-operator").List(
        ctx.Context,
        metav1.ListOptions{LabelSelector: "app=gpu-operator"},
    )
    if err != nil {
        return fmt.Errorf("failed to list pods: %w", err)
    }
    // Verify at least one pod is running
    for _, pod := range pods.Items {
        if pod.Status.Phase == "Running" { return nil }
    }
    return fmt.Errorf("no GPU operator pods running")
}

// Test wrapper (enables Job execution)
func TestOperatorHealth(t *testing.T) {
    runner, err := checks.NewTestRunner(t)  // Loads context from Job env
    if err != nil {
        t.Skipf("Skipping (not in Kubernetes): %v", err)
        return
    }
    runner.RunCheck("operator-health")  // Executes check
}
```

The test wrapper pattern enables:
- ✅ Standard Go testing infrastructure (`go test`)
- ✅ Automatic context loading (snapshot, K8s client)
- ✅ Graceful skipping during local development
- ✅ JSON test output for result parsing

**See:** [`checks/README.md`](./checks/README.md) for complete guide, examples, and troubleshooting.

### Validation Run Management (RunID)

Each validation run is assigned a unique **RunID** for resource isolation and resumability:

**RunID Format:** `YYYYMMDD-HHMMSS-XXXXXXXXXXXXXXXX` (e.g., `20260206-140523-a3f9b2c1e7d04a68b2c1e7d04a68`)
- Timestamp: Date and time when validation started
- Random suffix: 16 hex characters for uniqueness

**Resource Naming:**
All resources created during a validation run include the RunID:
- Input ConfigMaps: `aicr-snapshot-{runID}`, `aicr-recipe-{runID}` (shared by all phases)
- Output ConfigMap: `aicr-validation-result-{runID}` (progressively updated)
- Jobs: `aicr-{runID}-deployment`, `aicr-{runID}-performance`, etc. (one per phase, readiness has no Job)

**Benefits:**
- **Concurrent Validations**: Multiple validation runs can execute simultaneously without conflicts
- **Resumability**: Failed validations can be resumed from the last successful phase (future feature)
- **Traceability**: All resources for a run are grouped by RunID label
- **Cleanup**: Resources can be cleaned up per-run using RunID labels

**CLI Output:**
```bash
$ aicr validate --phase all --recipe recipe.yaml --snapshot snapshot.yaml
Starting validation run: 20260206-140523-a3f9b2c1e7d04a68
...
```

**Querying Validation Runs:**
```bash
# List all validation runs
kubectl get configmaps -n aicr-validation \
  -l app.kubernetes.io/component=validation

# List resources for specific run
kubectl get jobs,configmaps -n aicr-validation \
  -l aicr.nvidia.com/run-id=20260206-140523-a3f9b2c1e7d04a68

# View run details
kubectl get configmap -n aicr-validation \
  -l aicr.nvidia.com/run-id=20260206-140523-a3f9b2c1e7d04a68 \
  -o yaml
```

**Cleanup by RunID:**
```bash
# Cleanup specific validation run
kubectl delete jobs,configmaps -n aicr-validation \
  -l aicr.nvidia.com/run-id=20260206-140523-a3f9b2c1e7d04a68

# Cleanup all validation runs (caution!)
kubectl delete jobs,configmaps -n aicr-validation \
  -l app.kubernetes.io/component=validation
```

### ValidationResult ConfigMap (Resumability)

The validator creates a single ValidationResult ConfigMap per validation run that is progressively updated:

**ConfigMap:** `aicr-validation-result-{runID}`

**Lifecycle:**
1. **Creation**: Created at validation start with empty structure
2. **Progressive Updates**: Updated after each phase completes with results
3. **Resume**: Read by `--resume` flag to continue from failed phase
4. **Cleanup**: Automatically deleted after validation completes

**Resume Functionality:**
```bash
# New validation (auto-generates RunID)
aicr validate --phase all --recipe recipe.yaml --snapshot snapshot.yaml
# Output: Starting validation run: 20260206-140523-a3f9b2c1e7d04a68

# Validation fails at deployment phase (readiness passed)
# Resume from failed phase
aicr validate --phase all --resume 20260206-140523-a3f9b2c1e7d04a68
# Reads existing results, skips readiness (passed), continues from deployment
```

**Query Validation State:**
```bash
# View current validation progress
kubectl get cm aicr-validation-result-20260206-140523-a3f9b2c1e7d04a68 -o yaml

# Check which phases passed/failed
kubectl get cm aicr-validation-result-20260206-140523-a3f9b2c1e7d04a68 \
  -o jsonpath='{.data.result\.yaml}' | yq '.phases'
```

**Implementation:**
- `createValidationResultConfigMap()` - Creates empty structure
- `updateValidationResultConfigMap()` - Updates after each phase
- `readValidationResultConfigMap()` - Reads for resume
- `determineStartPhase()` - Finds where to resume from

### ConfigMap Management

The validator automatically manages ConfigMaps for snapshot and recipe data:

**Lifecycle:**
1. **Creation**: ConfigMaps are created **once per validation run** before any phases execute
   - `aicr-snapshot-{runID}`: Contains the cluster snapshot (YAML)
   - `aicr-recipe-{runID}`: Contains the recipe configuration (YAML)
2. **Reuse**: All phases in a validation run share the same ConfigMaps
   - Readiness phase uses snapshot-{runID} and recipe-{runID}
   - Deployment phase uses snapshot-{runID} and recipe-{runID}
   - Performance phase uses snapshot-{runID} and recipe-{runID}
   - Conformance phase uses snapshot-{runID} and recipe-{runID}
3. **Mounting**: Jobs mount these ConfigMaps as volumes at:
   - `/data/snapshot/snapshot.yaml`
   - `/data/recipe/recipe.yaml`
4. **Cleanup**: ConfigMaps are automatically deleted **once** after all phases complete

**Implementation Details:**
- ConfigMaps are created once per validation run, not per phase (efficient)
- ConfigMaps are uniquely named per validation run using RunID
- Each ConfigMap includes labels for querying and cleanup:
  - `aicr.nvidia.com/run-id`: The validation run identifier
  - `aicr.nvidia.com/created-at`: Timestamp (format: YYYYMMDD-HHMMSS)
  - `aicr.nvidia.com/data-type`: `snapshot` or `recipe`
- Cleanup happens in defer blocks to ensure removal even on errors
- Test wrappers load data from mounted ConfigMaps using `LoadValidationContext()`

**Security Considerations:**
- ConfigMaps may contain sensitive cluster information
- Access is restricted by Kubernetes RBAC
- ConfigMaps are namespace-scoped (default: `aicr-validation`)
- RunID-based naming prevents conflicts between concurrent validations

## Recipe Format

### Constraints and Checks

**Constraints** - Expression-based validations:
```yaml
validation:
  deployment:
    constraints:
      - name: Deployment.gpu-operator.version
        value: ">= v24.6.0"
      - name: Deployment.device-plugin.replicas
        value: ">= 1"
```

**Checks** - Named validation tests:
```yaml
# expected-resources check requires expectedResources on componentRefs
componentRefs:
  - name: gpu-operator
    type: Helm
    expectedResources:
      - kind: Deployment
        name: gpu-operator
        namespace: gpu-operator

validation:
  deployment:
    checks:
      - operator-health
      - expected-resources
```

### Multi-Phase Recipe Example

```yaml
# expectedResources are declared on componentRefs (used by expected-resources check)
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
  # Phase 1: Readiness (pre-deployment validation, constraints only, no cluster access)
  readiness:
    constraints:
      - name: GPU.count
        value: ">= 8"
      - name: OS.version
        value: "== ubuntu"
      - name: Kernel.version
        value: ">= 5.15.0"

  # Phase 2: Deployment (verify deployed resources)
  deployment:
    constraints:
      - name: Deployment.gpu-operator.version
        value: ">= v24.6.0"
    checks:
      - operator-health
      - expected-resources

  # Phase 3: Performance (measure system performance)
  performance:
    constraints:
      - name: Performance.nccl.bandwidth
        value: ">= 200"  # GB/s
    checks:
      - nccl-bandwidth-test
      - fabric-health

  # Phase 4: Conformance (validate compatibility)
  conformance:
    checks:
      - ai-workload-validation
```

## Result Format

```go
type ValidationResult struct {
    Phase     string              // "readiness", "deployment", etc.
    Status    ValidationStatus    // "pass", "fail", "skipped"
    StartTime time.Time
    EndTime   time.Time
    Duration  time.Duration

    // Constraints evaluated
    Constraints []ConstraintValidation

    // Checks executed
    Checks []CheckResult

    // Summary statistics
    Summary ValidationSummary
}

type ConstraintValidation struct {
    Name     string  // e.g., "Deployment.gpu-operator.version"
    Expected string  // e.g., ">= v24.6.0"
    Actual   string  // e.g., "v24.6.0"
    Passed   bool
    Message  string
}

type CheckResult struct {
    Name     string  // e.g., "operator-health"
    Status   ValidationStatus
    Message  string
    Duration time.Duration
}
```

## CLI Usage

```bash
# Validate all phases
aicr validate --phase all \
  --recipe recipe.yaml \
  --snapshot snapshot.yaml

# Validate specific phase
aicr validate --phase deployment \
  --recipe recipe.yaml \
  --snapshot snapshot.yaml

# Output formats
aicr validate --phase all -o json
aicr validate --phase all -o yaml
aicr validate --phase all -o table
```

## Documentation

### Core Documentation

- **[Constraint Expression Reference](./CONSTRAINTS.md)** - Syntax and operators
- **[Agent Architecture](./agent/README.md)** - Job execution model

### Check Development

- **[Checks Architecture](./checks/README.md)** - Overview and design
- **[How-To Guide](./checks/README.md#adding-constraint-validators-new-approach)** - Registering checks and constraints
- **[Troubleshooting](./checks/README.md#troubleshooting)** - Common issues and solutions

### Phase-Specific Guides

- **[Deployment Checks](./checks/deployment/README.md)** - Deployment phase validations

## Key Concepts

### Checks vs Constraints

| Aspect | Check | Constraint |
|--------|-------|------------|
| **Definition** | Named validation test | Expression-based validation |
| **Returns** | Pass/fail (error) | Actual value + pass/fail |
| **Registration** | `RegisterCheck()` | `RegisterConstraintValidator()` |
| **Recipe Syntax** | `checks: [name]` | `constraints: [{name, value}]` |
| **Example** | `operator-health` | `Deployment.gpu-operator.version: ">= v24.6.0"` |

### ValidationContext

Validation functions receive a context with:

```go
type ValidationContext struct {
    Context   context.Context        // Cancellation and timeouts
    Snapshot  *snapshotter.Snapshot  // Captured cluster state
    Clientset kubernetes.Interface   // Live Kubernetes API access
    RecipeData map[string]interface{} // Recipe metadata
}
```

- **Snapshot**: Hardware inventory, OS info, pre-capture state
- **Clientset**: Query live cluster (deployments, pods, etc.)
- **RecipeData**: Access recipe configuration

### Phase Dependencies

Phases execute sequentially with early exit:

1. **Readiness** must pass before Deployment
2. **Deployment** must pass before Performance
3. **Performance** must pass before Conformance

If any phase fails, subsequent phases are skipped.

## Testing

### Unit Testing Validators

```go
func TestValidateOperatorVersion(t *testing.T) {
    // Create fake Kubernetes client
    deployment := createTestDeployment("v24.6.0")
    clientset := fake.NewSimpleClientset(deployment)

    ctx := &checks.ValidationContext{
        Context:   context.Background(),
        Clientset: clientset,
    }

    constraint := recipe.Constraint{
        Name:  "Deployment.gpu-operator.version",
        Value: ">= v24.6.0",
    }

    actual, passed, err := ValidateGPUOperatorVersion(ctx, constraint)
    assert.NoError(t, err)
    assert.True(t, passed)
    assert.Equal(t, "v24.6.0", actual)
}
```

### Integration Testing

```bash
# Run all validator tests
go test -v ./pkg/validator/...

# Run with race detector
go test -v -race ./pkg/validator/...

# Run specific phase tests
go test -v ./pkg/validator/checks/deployment/...
```

## Design Decisions

### Why Job-Based Execution?

1. **Cluster Context**: Checks run with proper RBAC inside the cluster
2. **Resource Control**: Jobs can have CPU/memory limits
3. **Node Scheduling**: Performance tests can target GPU nodes
4. **Observability**: Jobs appear in `kubectl get jobs`
5. **Isolation**: Each check is independent

### Why Constraint Validators Run in Jobs?

Deployment, performance, and conformance constraints need **live cluster access**:
- Query deployed operator versions
- Measure network bandwidth
- Check API conformance

Readiness is constraint-only and evaluates inline because it only needs snapshot data (no cluster access, no Jobs).

### Why ConfigMaps for Results?

**Single ValidationResult ConfigMap per validation run:**
- ConfigMap: `aicr-validation-result-{runID}`
- Progressively updated as each phase completes
- Enables resumability (--resume flag)
- Persists even if CLI crashes or disconnects

**Benefits:**
1. **Resumability**: Continue from failed phase using `--resume {runID}`
2. **Observability**: Query current validation state with `kubectl get cm`
3. **Persistence**: Results survive Job deletion and CLI disconnection
4. **Progressive Updates**: Real-time visibility into validation progress
5. **Accessibility**: Easy to retrieve and inspect with kubectl

**Example:**
```bash
# Check validation progress
kubectl get cm aicr-validation-result-20260206-140523-a3f9b2c1e7d04a68 -o yaml

# Resume from failure
aicr validate --resume 20260206-140523-a3f9b2c1e7d04a68
```

## Examples

### Example 1: Validate GPU Operator Deployment

```yaml
validation:
  deployment:
    constraints:
      - name: Deployment.gpu-operator.version
        value: ">= v24.6.0"
    checks:
      - operator-health
```

### Example 2: Performance Validation

```yaml
validation:
  performance:
    constraints:
      - name: Performance.nccl.bandwidth
        value: ">= 200"
    checks:
      - nccl-bandwidth-test
      - fabric-health
```

### Example 3: Full Multi-Phase Validation

```yaml
validation:
  readiness:
    constraints:
      - name: GPU.count
        value: ">= 8"

  deployment:
    constraints:
      - name: Deployment.gpu-operator.version
        value: ">= v24.6.0"
    checks:
      - operator-health

  performance:
    checks:
      - nccl-bandwidth-test

  conformance:
    checks:
      - ai-workload-validation
```

## API Reference

### Main API

```go
// Create validator
validator := validator.New(
    validator.WithKubeconfig(kubeconfigPath),
    validator.WithTimeout(5 * time.Minute),
)

// Validate specific phase
result, err := validator.ValidatePhase(ctx, "deployment", recipe, snapshot)

// Validate all phases
results, err := validator.ValidateAll(ctx, recipe, snapshot)

// Validate with phase filter
results, err := validator.ValidatePhases(ctx,
    []string{"readiness", "deployment"}, recipe, snapshot)
```

### Registry API

```go
// Get registered check
check, ok := checks.GetCheck("operator-health")

// Get registered constraint validator
validator, ok := checks.GetConstraintValidator("Deployment.gpu-operator.version")

// List all checks for a phase
checkList := checks.ListChecks("deployment")

// List all constraint validators
validators := checks.ListConstraintValidators()
```

## Troubleshooting

See **[Troubleshooting Guide](./checks/README.md#troubleshooting)** for:
- Common errors and solutions
- RBAC permission issues
- Job timeout debugging
- How to view Job logs
- Test mode vs production mode

## Contributing

To add new validation checks or constraint validators:

1. Read **[How-To Guide](./checks/README.md#adding-constraint-validators-new-approach)** for step-by-step instructions
2. Follow existing patterns in `pkg/validator/checks/`
3. Write comprehensive tests
4. Update documentation

## References

- **Constraint Syntax**: [CONSTRAINTS.md](./CONSTRAINTS.md)
- **Check Development**: [checks/README.md](./checks/README.md#adding-constraint-validators-new-approach)
- **Architecture Details**: [checks/README.md](./checks/README.md)
- **Agent Implementation**: [agent/README.md](./agent/README.md)
