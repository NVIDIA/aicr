# Validation Checks and Constraint Registry

This package provides a registration framework for validation checks and constraint validators that run inside Kubernetes Jobs.

## Table of Contents

- [Overview](#overview)
  - [Architecture Overview](#architecture-overview)
  - [Two Types of Validation](#two-types-of-validation)
  - [Phase-Specific Execution](#phase-specific-execution)
  - [Directory Structure](#directory-structure)
- [Getting Started](#getting-started)
  - [Quick Start (5 minutes)](#quick-start-5-minutes)
  - [Key Principles](#key-principles)
  - [Example Recipe Usage](#example-recipe-usage)
- [Registration Pattern](#registration-pattern)
  - [Registering a Check](#registering-a-check)
  - [Registering a Constraint Validator](#registering-a-constraint-validator)
  - [Validation Context](#validation-context)
- [Test Wrappers for Job Execution](#test-wrappers-for-job-execution)
  - [Why Test Wrappers?](#why-test-wrappers)
  - [Adding a Test Wrapper](#adding-a-test-wrapper)
  - [How the Test Runner Works](#how-the-test-runner-works)
  - [Complete Test Wrapper Example](#complete-test-wrapper-example)
  - [Environment Variables](#environment-variables)
  - [Local vs Job Execution](#local-vs-job-execution)
- [How-To Guide](#how-to-guide)
  - [Adding a Check](#adding-a-check)
  - [Adding a Constraint Validator](#adding-a-constraint-validator)
  - [Phase-Specific Considerations](#phase-specific-considerations)
  - [Testing](#testing)
  - [Common Patterns](#common-patterns)
- [Troubleshooting](#troubleshooting)
  - [Test Wrapper Issues](#test-wrapper-issues)
  - [Job Execution Issues](#job-execution-issues)
  - [RBAC Permission Errors](#rbac-permission-errors)
  - [Timeout Problems](#timeout-problems)
  - [Check Registration Issues](#check-registration-issues)
  - [Constraint Evaluation Errors](#constraint-evaluation-errors)
  - [Kubernetes Client Errors](#kubernetes-client-errors)
  - [Test Mode vs Production](#test-mode-vs-production)
  - [Debugging Techniques](#debugging-techniques)
- [Migration from Inline Validation](#migration-from-inline-validation)
- [References](#references)

## Overview

### Architecture Overview

Validation checks run inside Kubernetes Jobs to verify cluster configuration and state. This architecture enables:

- **Cluster Access**: Checks query live Kubernetes resources
- **Isolation**: Each check runs in a dedicated Job for resource control
- **Testability**: Graceful degradation when cluster access is unavailable
- **Observability**: Captured results and logs for debugging

### Two Types of Validation

| Type | Purpose | Returns | Example |
|------|---------|---------|---------|
| **Check** | Named validation test | `error` | `"operator-health"` checks if pods are running |
| **Constraint Validator** | Evaluates constraint expressions | `(actual string, passed bool, error)` | `"Deployment.gpu-operator.version"` checks version >= v24.6.0 |

**Key difference:**
- **Checks** verify a condition and return pass/fail
- **Constraint Validators** extract a value and evaluate it against a constraint expression

### Phase-Specific Execution

| Phase | Constraints | Checks | Execution Context |
|-------|-------------|--------|-------------------|
| **Readiness** | Evaluated inline from snapshot | N/A (constraint-only) | Snapshot data only |
| **Deployment** | Run in Jobs (need cluster access) | Run in Jobs | Snapshot + Live cluster |
| **Performance** | Run in Jobs (need measurements) | Run in Jobs | Snapshot + Live cluster |
| **Conformance** | Run in Jobs (need cluster access) | Run in Jobs | Snapshot + Live cluster |

**Key Insight:** Readiness = Constraints Only. It validates prerequisites from snapshot data with no cluster access and no Jobs. All other phases need live cluster access, so their constraints AND checks run inside Jobs.

### Directory Structure

```
pkg/validator/checks/
├── README.md                    # This file - Complete documentation
├── registry.go                  # Registration infrastructure
├── runner.go                    # Test runner for Job execution
├── generator.go                 # Code generator for new checks/constraints
├── deployment/                  # Deployment phase checks + constraints
│   ├── operator_health_check.go           # Check registration and implementation
│   ├── operator_health_check_test.go      # Integration test (runs in Jobs)
│   ├── operator_health_check_unit_test.go # Unit test (runs locally)
│   ├── gpu_operator_version_constraint.go           # Constraint validator
│   ├── gpu_operator_version_constraint_test.go      # Integration test
│   └── gpu_operator_version_constraint_unit_test.go # Unit test
├── performance/                 # Performance phase checks + constraints
└── conformance/                 # Conformance phase checks + constraints
```

### File Naming Convention

| Type | Files Generated |
|------|-----------------|
| **Check** | `<name>_check.go`, `<name>_check_test.go`, `<name>_check_unit_test.go` |
| **Constraint** | `<name>_constraint.go`, `<name>_constraint_test.go`, `<name>_constraint_unit_test.go` |

## Getting Started

### Quick Start (5 minutes)

Use the generator to create a new check or constraint with all required files:

**1. Generate a check:**
```bash
make generate-validator ARGS="--check my-check --phase deployment --description 'Verify my component is healthy'"
```

This creates:
- `my_check_check.go` - Registration and validator function
- `my_check_check_test.go` - Integration test (runs in Kubernetes Jobs)
- `my_check_check_unit_test.go` - Unit test (runs locally)
- `my_check_recipe.yaml` - Sample recipe for testing
- `my_check_README.md` - Instructions

**2. Implement the validator function:**
```go
// pkg/validator/checks/deployment/my_check_check.go
func validateMyCheck(ctx *checks.ValidationContext) error {
    pods, err := ctx.Clientset.CoreV1().Pods("my-namespace").List(
        ctx.Context,
        metav1.ListOptions{LabelSelector: "app=my-component"},
    )
    if err != nil {
        return fmt.Errorf("failed to list pods: %w", err)
    }

    if len(pods.Items) == 0 {
        return fmt.Errorf("no pods found")
    }

    for _, pod := range pods.Items {
        if pod.Status.Phase == "Running" {
            return nil
        }
    }
    return fmt.Errorf("no pods running")
}
```

**3. Add unit tests:**
```go
// pkg/validator/checks/deployment/my_check_check_unit_test.go
func TestValidateMyCheck(t *testing.T) {
    tests := []struct {
        name    string
        setup   func() *checks.ValidationContext
        wantErr bool
    }{
        {
            name: "pods running",
            setup: func() *checks.ValidationContext {
                return &checks.ValidationContext{
                    Context:   context.Background(),
                    Clientset: fake.NewSimpleClientset(&runningPod),
                }
            },
            wantErr: false,
        },
        {
            name: "no pods found",
            setup: func() *checks.ValidationContext {
                return &checks.ValidationContext{
                    Context:   context.Background(),
                    Clientset: fake.NewSimpleClientset(),
                }
            },
            wantErr: true,
        },
    }
    // ... test execution
}
```

**4. Run unit tests:**
```bash
go test -short -v ./pkg/validator/checks/deployment/... -run TestValidateMyCheck
```

**5. Use in recipe:**
```yaml
validation:
  deployment:
    checks:
      - my-check
```

Done! Your check will run inside validation Jobs.

**Generate a constraint validator:**
```bash
make generate-validator ARGS="--constraint Deployment.my-app.version --phase deployment"
```

### Key Principles

1. **Readiness = Constraints Only** - Pre-deployment constraints evaluated inline from snapshot data (no checks, no Jobs, no cluster access)
2. **Other Phases = Cluster Access Required** - Deployment/Performance/Conformance need live queries
3. **Self-Registration** - Checks auto-discover via init()
4. **Job Isolation** - Each check runs in its own Job for resource control
5. **Graceful Degradation** - Test mode handles missing cluster gracefully

### Example Recipe Usage

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
  deployment:
    constraints:
      # These run INSIDE the Job with cluster access
      - name: Deployment.gpu-operator.version
        value: ">= v25.10.1"
      - name: Deployment.device-plugin.replicas
        value: ">= 1"
    checks:
      # These also run inside the Job
      - operator-health
      - expected-resources  # validates componentRefs[].expectedResources
```

## Registration Pattern

### Registering a Check

Checks use Go's `init()` pattern for self-registration. Use `TestName` to specify which test function runs in Jobs:

```go
// pkg/validator/checks/deployment/my_check_check.go
package deployment

import "github.com/NVIDIA/aicr/pkg/validator/checks"

func init() {
    checks.RegisterCheck(&checks.Check{
        Name:        "my-check",
        Description: "Verify my component is healthy",
        Phase:       "deployment",
        TestName:    "TestCheckMyCheck",  // Test function name for Job execution
    })
}

// validateMyCheck is the validator function (private for encapsulation)
func validateMyCheck(ctx *checks.ValidationContext) error {
    // Validation logic here
    return nil
}
```

### Registering a Constraint Validator

Constraint validators evaluate constraints that need cluster access:

```go
// pkg/validator/checks/deployment/my_constraint_constraint.go
package deployment

import (
    "github.com/NVIDIA/aicr/pkg/recipe"
    "github.com/NVIDIA/aicr/pkg/validator/checks"
)

func init() {
    checks.RegisterConstraintValidator(&checks.ConstraintValidator{
        Pattern:     "Deployment.my-app.version",
        Description: "Validates my-app deployment version",
        TestName:    "TestMyAppVersion",  // Test function name for Job execution
        Phase:       "deployment",
    })
}

// validateMyAppVersion is the validator function (private for encapsulation)
func validateMyAppVersion(
    ctx *checks.ValidationContext,
    constraint recipe.Constraint,
) (actual string, passed bool, err error) {
    // Query live cluster
    deployment, err := ctx.Clientset.AppsV1().Deployments("my-namespace").Get(
        ctx.Context, "my-app", metav1.GetOptions{})
    if err != nil {
        return "", false, err
    }

    // Extract actual value (e.g., version from image tag)
    actual = extractVersion(deployment.Spec.Template.Spec.Containers[0].Image)

    // Evaluate constraint expression
    passed, err = evaluateVersionConstraint(actual, constraint.Value)

    return actual, passed, err
}
```

### Validation Context

The `ValidationContext` provides runtime access to:

```go
type ValidationContext struct {
    Context   context.Context          // Cancellation and timeouts
    Snapshot  *snapshotter.Snapshot    // Captured cluster state
    Clientset kubernetes.Interface     // Live Kubernetes API access
    RecipeData map[string]interface{}  // Recipe metadata
}
```

- **Snapshot**: Hardware, OS, and pre-capture cluster state
- **Clientset**: Query live cluster (deployments, pods, services, etc.)
- **RecipeData**: Access recipe configuration if needed

## Test Wrappers for Job Execution

### Why Test Wrappers?

Validation checks run inside Kubernetes Jobs via `go test`. The Jobs execute:
```bash
go test -v -json ./pkg/validator/checks/deployment -run operator-health
```

For `go test` to discover and run your check, you need a `Test*` function that:
1. Loads ValidationContext from the Job environment (snapshot, K8s client)
2. Executes the registered check by name
3. Reports results in standard Go test format

### Adding a Test Wrapper

**Note:** When using the generator (`make generate-validator`), test wrappers are automatically created. The following is for manual creation.

**Step 1: Add Test Wrapper to Your Check's Integration Test File**

The integration test file (`*_check_test.go`) contains the test wrapper that runs in Kubernetes Jobs:

```go
// pkg/validator/checks/deployment/operator_health_check_test.go

// TestOperatorHealth is the integration test for operator-health.
// This runs inside validator Jobs and invokes the validator.
func TestOperatorHealth(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping integration test in short mode")
    }

    runner, err := checks.NewTestRunner(t)
    if err != nil {
        // Skip if not running in Kubernetes (expected during local test runs)
        t.Skipf("Not in Job environment: %v", err)
    }
    defer runner.Cancel()

    runner.RunCheck("operator-health")
}
```

**Step 2: Naming Convention**

The test wrapper function name must match the check name pattern:

| Check Name | Test Wrapper Function |
|------------|----------------------|
| `operator-health` | `TestOperatorHealth` |
| `nccl-bandwidth` | `TestNCCLBandwidth` |

**Pattern:** Convert kebab-case to PascalCase and prefix with `Test`.

### How the Test Runner Works

The `checks.NewTestRunner(t)` function:

1. **Creates in-cluster Kubernetes client** using `rest.InClusterConfig()`
2. **Loads snapshot** from mounted file at `$AICR_SNAPSHOT_PATH` (default: `/data/snapshot/snapshot.yaml`)
3. **Loads recipe data** from `$AICR_RECIPE_DATA` environment variable (optional)
4. **Returns TestRunner** with fully initialized ValidationContext

The `runner.RunCheck("check-name")` method:

1. **Looks up check** in registry by name
2. **Executes check function** with the loaded ValidationContext
3. **Reports results** via `t.Fatalf()` on failure, or returns on success

### Complete Test Wrapper Example

```go
// pkg/validator/checks/performance/nccl_bandwidth.go
package performance

import (
    "fmt"
    "github.com/NVIDIA/aicr/pkg/validator/checks"
)

func init() {
    checks.RegisterCheck(&checks.Check{
        Name:        "nccl-bandwidth",
        Description: "Measure NCCL all-reduce bandwidth",
        Phase:       "performance",
        Func:        CheckNCCLBandwidth,
    })
}

func CheckNCCLBandwidth(ctx *checks.ValidationContext) error {
    // Implementation...
    return nil
}
```

```go
// pkg/validator/checks/performance/nccl_bandwidth_test.go
package performance

import (
    "testing"
    "github.com/NVIDIA/aicr/pkg/validator/checks"
)

// Test wrapper for Job execution
func TestNCCLBandwidth(t *testing.T) {
    runner, err := checks.NewTestRunner(t)
    if err != nil {
        t.Skipf("Skipping integration test (not in Kubernetes): %v", err)
        return
    }

    runner.RunCheck("nccl-bandwidth")
}

// Unit tests with mocked context
func TestCheckNCCLBandwidth(t *testing.T) {
    tests := []struct {
        name    string
        // ...
    }{
        // Test cases...
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            ctx := &checks.ValidationContext{
                // Mocked context...
            }
            err := CheckNCCLBandwidth(ctx)
            // Assertions...
        })
    }
}
```

### Environment Variables

The validation Job automatically sets these environment variables:

| Variable | Purpose | Example |
|----------|---------|---------|
| `AICR_SNAPSHOT_PATH` | Path to mounted snapshot file | `/data/snapshot/snapshot.yaml` |
| `AICR_RECIPE_PATH` | Path to mounted recipe file | `/data/recipe/recipe.yaml` |
| `AICR_NAMESPACE` | Namespace where Job is running | `aicr-validation` |
| `AICR_RESULT_CONFIGMAP` | ConfigMap name for results | `aicr-validation-deployment-operator-health-result` |

### Local vs Job Execution

**Local execution** (`go test ./pkg/validator/checks/...`):
- Test wrappers **skip** (no in-cluster config available)
- Unit tests **run** (use mocked context)
- Fast feedback during development

**Job execution** (`go test -run operator-health`):
- Test wrappers **run** (inside Kubernetes)
- Unit tests **excluded** by `-run` pattern
- Real validation against live cluster

## How-To Guide

### Adding a Check

**Step 1: Create Check File**

Create a file in the appropriate phase directory:
- `pkg/validator/checks/deployment/` - For deployment checks
- `pkg/validator/checks/performance/` - For performance checks
- `pkg/validator/checks/conformance/` - For conformance checks

Example: `pkg/validator/checks/deployment/operator_health.go`

**Step 2: Implement Check Function**

```go
// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
// [Standard license header...]

package deployment

import (
    "fmt"

    "github.com/NVIDIA/aicr/pkg/validator/checks"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func init() {
    checks.RegisterCheck(&checks.Check{
        Name:        "operator-health",              // ← Used in recipe
        Description: "Verify GPU operator is healthy",
        Phase:       "deployment",                   // ← Must match phase
        Func:        CheckOperatorHealth,
    })
}

// CheckOperatorHealth verifies the GPU operator pods are running.
func CheckOperatorHealth(ctx *checks.ValidationContext) error {
    // Access live cluster via ctx.Clientset
    pods, err := ctx.Clientset.CoreV1().Pods("gpu-operator").List(
        ctx.Context,
        metav1.ListOptions{LabelSelector: "app=gpu-operator"},
    )
    if err != nil {
        return fmt.Errorf("failed to list GPU operator pods: %w", err)
    }

    if len(pods.Items) == 0 {
        return fmt.Errorf("no GPU operator pods found")
    }

    // Verify at least one pod is running
    for _, pod := range pods.Items {
        if pod.Status.Phase == "Running" {
            return nil // Success!
        }
    }

    return fmt.Errorf("no GPU operator pods in Running state")
}
```

**Step 3: Add Test Wrapper**

```go
// pkg/validator/checks/deployment/operator_health_test.go
package deployment

import (
    "testing"
    "github.com/NVIDIA/aicr/pkg/validator/checks"
)

func TestOperatorHealth(t *testing.T) {
    runner, err := checks.NewTestRunner(t)
    if err != nil {
        t.Skipf("Skipping integration test (not in Kubernetes): %v", err)
        return
    }
    runner.RunCheck("operator-health")
}
```

**Step 4: Use in Recipe**

```yaml
validation:
  deployment:
    checks:
      - operator-health  # ← Must match Check.Name
```

**Step 5: Import Package (if needed)**

If the package isn't already imported, add it to trigger `init()`:

```go
// In main.go or test file
import _ "github.com/NVIDIA/aicr/pkg/validator/checks/deployment"
```

### Adding a Constraint Validator

**Step 1: Create Constraints File**

Create `constraints.go` in the phase directory:
- `pkg/validator/checks/deployment/constraints.go`
- `pkg/validator/checks/performance/constraints.go`
- `pkg/validator/checks/conformance/constraints.go`

**Step 2: Implement Constraint Validator**

```go
// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
// [Standard license header...]

package deployment

import (
    "context"
    "fmt"

    "github.com/NVIDIA/aicr/pkg/recipe"
    "github.com/NVIDIA/aicr/pkg/validator"
    "github.com/NVIDIA/aicr/pkg/validator/checks"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/client-go/kubernetes"
)

func init() {
    checks.RegisterConstraintValidator(&checks.ConstraintValidator{
        Pattern:     "Deployment.device-plugin.replicas",  // ← Constraint name pattern
        Description: "Validates device plugin replica count",
        Func:        ValidateDevicePluginReplicas,
    })
}

// ValidateDevicePluginReplicas checks the device plugin replica count.
// Constraint format: "Deployment.device-plugin.replicas"
// Constraint value: ">= 1", "== 3", etc.
func ValidateDevicePluginReplicas(
    ctx *checks.ValidationContext,
    constraint recipe.Constraint,
) (string, bool, error) {
    // 1. Query cluster to get actual value
    replicas, err := getDevicePluginReplicas(ctx.Context, ctx.Clientset)
    if err != nil {
        return "", false, fmt.Errorf("failed to get replica count: %w", err)
    }

    // 2. Convert to string for comparison
    actualValue := fmt.Sprintf("%d", replicas)

    // 3. Evaluate constraint expression
    passed, err := evaluateConstraint(actualValue, constraint.Value)
    if err != nil {
        return actualValue, false, fmt.Errorf("constraint evaluation failed: %w", err)
    }

    // 4. Return: (actual value, pass/fail, error)
    return actualValue, passed, nil
}

// Helper: Get actual replica count from cluster
func getDevicePluginReplicas(ctx context.Context, clientset kubernetes.Interface) (int, error) {
    deployment, err := clientset.AppsV1().Deployments("gpu-operator").Get(
        ctx,
        "nvidia-device-plugin",
        metav1.GetOptions{},
    )
    if err != nil {
        return 0, err
    }

    if deployment.Spec.Replicas == nil {
        return 0, nil
    }

    return int(*deployment.Spec.Replicas), nil
}

// Helper: Evaluate constraint expression
func evaluateConstraint(actualValue, constraintExpr string) (bool, error) {
    parsed, err := validator.ParseConstraintExpression(constraintExpr)
    if err != nil {
        return false, fmt.Errorf("invalid constraint expression: %w", err)
    }

    passed, err := parsed.Evaluate(actualValue)
    if err != nil {
        return false, fmt.Errorf("evaluation failed: %w", err)
    }

    return passed, nil
}
```

**Step 3: Use in Recipe**

```yaml
validation:
  deployment:
    constraints:
      - name: Deployment.device-plugin.replicas  # ← Must match Pattern
        value: ">= 1"                             # ← Constraint expression
```

**Step 4: Import Package (if needed)**

Same as checks - ensure the package is imported to trigger `init()`.

### Phase-Specific Considerations

#### Deployment Phase

**Typical validations:**
- Operator health and readiness
- Deployment resource versions
- Pod counts and statuses
- ConfigMap/Secret presence

**Example constraint names:**
- `Deployment.gpu-operator.version`
- `Deployment.device-plugin.replicas`
- `Deployment.dcgm-exporter.enabled`

**Access patterns:**
```go
// Deployments
deployment, _ := ctx.Clientset.AppsV1().Deployments(ns).Get(ctx.Context, name, metav1.GetOptions{})

// Pods
pods, _ := ctx.Clientset.CoreV1().Pods(ns).List(ctx.Context, metav1.ListOptions{LabelSelector: "app=foo"})

// ConfigMaps
cm, _ := ctx.Clientset.CoreV1().ConfigMaps(ns).Get(ctx.Context, name, metav1.GetOptions{})
```

#### Performance Phase

**Typical validations:**
- NCCL bandwidth measurements
- Network fabric health
- GPU-to-GPU communication latency
- Storage I/O performance

**Example constraint names:**
- `Performance.nccl.bandwidth`
- `Performance.network.latency`
- `Performance.gpu.peer-access`

**Access patterns:**
```go
// Run Jobs for performance tests
job, _ := ctx.Clientset.BatchV1().Jobs(ns).Create(ctx.Context, perfJob, metav1.CreateOptions{})

// Wait for completion and read results from logs
pods, _ := ctx.Clientset.CoreV1().Pods(ns).List(ctx.Context, metav1.ListOptions{LabelSelector: fmt.Sprintf("job-name=%s", jobName)})
logs, _ := ctx.Clientset.CoreV1().Pods(ns).GetLogs(podName, &corev1.PodLogOptions{}).Stream(ctx.Context)
```

#### Conformance Phase

**Typical validations:**
- Kubernetes API version compatibility
- RBAC policy conformance
- CRD schema validation
- AI workload compatibility

**Example constraint names:**
- `Conformance.k8s.version`
- `Conformance.api.gpu-device`
- `Conformance.workload.pytorch`

**Access patterns:**
```go
// API version
version, _ := ctx.Clientset.Discovery().ServerVersion()

// CRDs
crdClient := ctx.Clientset.ApiextensionsV1().CustomResourceDefinitions()
crd, _ := crdClient.Get(ctx.Context, "gpus.nvidia.com", metav1.GetOptions{})

// Run conformance test workloads
job, _ := ctx.Clientset.BatchV1().Jobs(ns).Create(ctx.Context, testJob, metav1.CreateOptions{})
```

### Testing

#### Unit Test for Check

```go
func TestCheckOperatorHealth(t *testing.T) {
    tests := []struct {
        name    string
        pods    []corev1.Pod
        wantErr bool
    }{
        {
            name: "healthy operator",
            pods: []corev1.Pod{
                {
                    ObjectMeta: metav1.ObjectMeta{Name: "gpu-operator-abc"},
                    Status:     corev1.PodStatus{Phase: "Running"},
                },
            },
            wantErr: false,
        },
        {
            name:    "no pods found",
            pods:    []corev1.Pod{},
            wantErr: true,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            // Create fake clientset with test data
            var objects []runtime.Object
            for i := range tt.pods {
                objects = append(objects, &tt.pods[i])
            }
            clientset := fake.NewSimpleClientset(objects...)

            ctx := &checks.ValidationContext{
                Context:   context.Background(),
                Clientset: clientset,
            }

            err := CheckOperatorHealth(ctx)
            if (err != nil) != tt.wantErr {
                t.Errorf("error = %v, wantErr %v", err, tt.wantErr)
            }
        })
    }
}
```

#### Unit Test for Constraint Validator

```go
func TestValidateDevicePluginReplicas(t *testing.T) {
    tests := []struct {
        name          string
        deployment    *appsv1.Deployment
        constraint    recipe.Constraint
        wantActual    string
        wantPassed    bool
        wantErr       bool
    }{
        {
            name: "constraint satisfied",
            deployment: &appsv1.Deployment{
                ObjectMeta: metav1.ObjectMeta{
                    Name:      "nvidia-device-plugin",
                    Namespace: "gpu-operator",
                },
                Spec: appsv1.DeploymentSpec{
                    Replicas: ptr.To(int32(3)),
                },
            },
            constraint: recipe.Constraint{
                Name:  "Deployment.device-plugin.replicas",
                Value: ">= 1",
            },
            wantActual: "3",
            wantPassed: true,
            wantErr:    false,
        },
        {
            name: "constraint not satisfied",
            deployment: &appsv1.Deployment{
                ObjectMeta: metav1.ObjectMeta{
                    Name:      "nvidia-device-plugin",
                    Namespace: "gpu-operator",
                },
                Spec: appsv1.DeploymentSpec{
                    Replicas: ptr.To(int32(0)),
                },
            },
            constraint: recipe.Constraint{
                Name:  "Deployment.device-plugin.replicas",
                Value: ">= 1",
            },
            wantActual: "0",
            wantPassed: false,
            wantErr:    false,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            clientset := fake.NewSimpleClientset(tt.deployment)

            ctx := &checks.ValidationContext{
                Context:   context.Background(),
                Clientset: clientset,
            }

            actual, passed, err := ValidateDevicePluginReplicas(ctx, tt.constraint)

            if (err != nil) != tt.wantErr {
                t.Errorf("error = %v, wantErr %v", err, tt.wantErr)
            }
            if actual != tt.wantActual {
                t.Errorf("actual = %v, want %v", actual, tt.wantActual)
            }
            if passed != tt.wantPassed {
                t.Errorf("passed = %v, want %v", passed, tt.wantPassed)
            }
        })
    }
}
```

#### Integration Test

```go
func TestConstraintValidatorRegistration(t *testing.T) {
    // Verify the validator is registered
    validator, ok := checks.GetConstraintValidator("Deployment.device-plugin.replicas")
    if !ok {
        t.Fatal("Constraint validator not registered")
    }

    if validator.Pattern != "Deployment.device-plugin.replicas" {
        t.Errorf("Pattern = %v, want Deployment.device-plugin.replicas", validator.Pattern)
    }

    if validator.Func == nil {
        t.Fatal("Func is nil")
    }
}
```

#### Testing Checks Locally

```go
func TestOperatorHealthLocal(t *testing.T) {
    deployment := createTestDeployment("gpu-operator", "gpu-operator")
    clientset := fake.NewSimpleClientset(deployment)

    ctx := &checks.ValidationContext{
        Context:   context.Background(),
        Clientset: clientset,
    }

    check, ok := checks.GetCheck("operator-health")
    if !ok {
        t.Fatal("check not registered")
    }

    if err := check.Func(ctx); err != nil {
        t.Errorf("check failed: %v", err)
    }
}
```

### Common Patterns

#### Pattern 1: Version Constraint Validator

```go
func ValidateComponentVersion(ctx *checks.ValidationContext, constraint recipe.Constraint) (string, bool, error) {
    // 1. Get version from deployment label
    version := deployment.Labels["app.kubernetes.io/version"]

    // 2. Fallback: parse from image tag
    if version == "" {
        version = extractVersionFromImage(container.Image)
    }

    // 3. Normalize version (add 'v' prefix if missing)
    version = normalizeVersion(version)

    // 4. Evaluate constraint
    passed, err := evaluateVersionConstraint(version, constraint.Value)

    return version, passed, err
}
```

#### Pattern 2: Count/Numeric Constraint Validator

```go
func ValidateResourceCount(ctx *checks.ValidationContext, constraint recipe.Constraint) (string, bool, error) {
    // 1. Query cluster for resources
    items, err := ctx.Clientset....List(...)

    // 2. Count items
    count := len(items.Items)
    actualValue := fmt.Sprintf("%d", count)

    // 3. Evaluate numeric constraint
    passed, err := evaluateConstraint(actualValue, constraint.Value)

    return actualValue, passed, err
}
```

#### Pattern 3: Boolean/State Constraint Validator

```go
func ValidateFeatureEnabled(ctx *checks.ValidationContext, constraint recipe.Constraint) (string, bool, error) {
    // 1. Check feature state (ConfigMap, annotation, etc.)
    enabled := checkFeatureState(ctx)
    actualValue := fmt.Sprintf("%t", enabled)

    // 2. Evaluate boolean constraint ("== true", "== false")
    passed, err := evaluateConstraint(actualValue, constraint.Value)

    return actualValue, passed, err
}
```

#### Pattern 4: Multi-Namespace Search

```go
func findResourceAcrossNamespaces(ctx context.Context, clientset kubernetes.Interface,
    namespaces []string, names []string) (*appsv1.Deployment, error) {

    for _, ns := range namespaces {
        for _, name := range names {
            deployment, err := clientset.AppsV1().Deployments(ns).Get(
                ctx, name, metav1.GetOptions{},
            )
            if err == nil {
                return deployment, nil
            }
        }
    }

    return nil, fmt.Errorf("resource not found in any namespace")
}
```

#### Pattern 5: Performance Test with Job

```go
func CheckPerformance(ctx *checks.ValidationContext) error {
    // 1. Create test Job
    job := &batchv1.Job{
        ObjectMeta: metav1.ObjectMeta{
            Name:      "perf-test",
            Namespace: "aicr-validation",
        },
        Spec: batchv1.JobSpec{
            Template: corev1.PodTemplateSpec{
                Spec: corev1.PodSpec{
                    Containers: []corev1.Container{
                        {
                            Name:  "test",
                            Image: "nvcr.io/nvidia/nccl-tests:latest",
                            Args:  []string{"all_reduce_perf", "-b", "8", "-e", "256M"},
                        },
                    },
                    RestartPolicy: "Never",
                },
            },
        },
    }

    // 2. Create and wait for Job
    _, err := ctx.Clientset.BatchV1().Jobs("aicr-validation").Create(
        ctx.Context, job, metav1.CreateOptions{},
    )
    if err != nil {
        return err
    }

    // 3. Wait for completion
    // 4. Read logs
    // 5. Parse results

    return nil
}
```

## Adding Constraint Validators (New Approach)

For constraint validators, AICR provides an automated code generator that scaffolds all necessary files with proper structure. This ensures consistency and catches registration issues automatically.

### Quick Start with Generator

**1. Generate validator scaffolding:**

```bash
make generate-validator ARGS="--constraint Deployment.my-app.version --phase deployment --description 'Validates my-app version'"
```

This creates three files with TODOs guiding implementation:
```
pkg/validator/checks/deployment/
├── my_app_version.go                    # Helper functions
├── my_app_version_test.go               # Unit tests
└── my_app_version_integration_test.go   # Integration test with registration
```

**2. Implement helper functions:**

Edit `my_app_version.go` and fill in the TODOs:

```go
// getMyAppVersion queries the cluster to get the actual version
func getMyAppVersion(ctx context.Context, clientset kubernetes.Interface) (string, error) {
    // TODO: Implement version detection
    // Search common namespaces
    namespaces := []string{"my-app", "default", "kube-system"}
    names := []string{"my-app", "myapp"}

    for _, ns := range namespaces {
        for _, name := range names {
            deployment, err := clientset.AppsV1().Deployments(ns).Get(
                ctx, name, metav1.GetOptions{},
            )
            if err == nil {
                // Try version from label
                if version := deployment.Labels["app.kubernetes.io/version"]; version != "" {
                    return normalizeVersion(version), nil
                }
                // Try version from image tag
                if len(deployment.Spec.Template.Spec.Containers) > 0 {
                    return extractVersionFromImage(deployment.Spec.Template.Spec.Containers[0].Image), nil
                }
            }
        }
    }

    return "", fmt.Errorf("my-app not found")
}

// evaluateVersionConstraint evaluates version constraint expressions
func evaluateVersionConstraint(actualValue, constraintValue string) (bool, error) {
    // TODO: Implement constraint evaluation
    // Parse constraint (>=, ==, !=, <, >, ~=)
    // Compare versions using semver
    // Return pass/fail
}
```

**3. Add unit test cases:**

Edit `my_app_version_test.go`:

```go
func TestGetMyAppVersion(t *testing.T) {
    tests := []struct {
        name       string
        deployment *appsv1.Deployment
        want       string
        wantErr    bool
    }{
        {
            name: "version from label",
            deployment: &appsv1.Deployment{
                ObjectMeta: metav1.ObjectMeta{
                    Name:      "my-app",
                    Namespace: "default",
                    Labels: map[string]string{
                        "app.kubernetes.io/version": "v1.2.3",
                    },
                },
            },
            want:    "v1.2.3",
            wantErr: false,
        },
        // Add more test cases...
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            clientset := fake.NewSimpleClientset(tt.deployment)
            got, err := getMyAppVersion(context.Background(), clientset)

            if (err != nil) != tt.wantErr {
                t.Errorf("error = %v, wantErr %v", err, tt.wantErr)
            }
            if got != tt.want {
                t.Errorf("got %v, want %v", got, tt.want)
            }
        })
    }
}
```

**4. Integration test is auto-generated with registration:**

The generator creates `my_app_version_integration_test.go` with proper registration:

```go
func init() {
    checks.RegisterConstraintTest(&checks.ConstraintTest{
        TestName:    "TestMyAppVersion",
        Pattern:     "Deployment.my-app.version",
        Description: "Validates my-app version",
        Phase:       "deployment",
    })
}

func TestMyAppVersion(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping integration test in short mode")
    }

    runner, err := checks.NewTestRunner(t)
    if err != nil {
        t.Skipf("Skipping integration test (not in Kubernetes): %v", err)
    }

    // Get constraint from recipe
    constraint := runner.GetConstraint("deployment", "Deployment.my-app.version")
    if constraint == nil {
        t.Skip("Constraint not defined in recipe")
    }

    // Execute validation logic
    ctx := runner.Context()
    actualValue, err := getMyAppVersion(ctx.Context, ctx.Clientset)
    if err != nil {
        t.Fatalf("Failed to get my-app version: %v", err)
    }

    passed, err := evaluateVersionConstraint(actualValue, constraint.Value)
    if err != nil {
        t.Fatalf("Failed to evaluate constraint: %v", err)
    }

    if !passed {
        t.Errorf("Version constraint not satisfied: actual=%s, expected=%s",
            actualValue, constraint.Value)
    }
}
```

**5. Run tests:**

```bash
# Unit tests only (fast, no cluster needed)
make test

# Integration test in Job (requires cluster)
aicr validate --recipe recipe.yaml --snapshot snapshot.yaml --phase deployment
```

**6. Submit PR:**

The CI pipeline automatically validates:
- Code compiles
- Unit tests pass
- Registration is complete (enforced by `pkg/validator/checks/registration_test.go`)
- Coverage meets threshold

### How It Works

#### Recipe → Test Execution Flow

```yaml
# Recipe
validation:
  deployment:
    constraints:
      - name: Deployment.my-app.version
        value: ">= v1.2.0"
```

↓

```go
// Registry lookup (buildTestPattern in phases.go)
testName, _ := checks.GetTestNameForConstraint("Deployment.my-app.version")
// Returns: "TestMyAppVersion"
```

↓

```go
// Pattern building
pattern := "^(TestMyAppVersion)$"
```

↓

```bash
# Job command
go test -v -json ./pkg/validator/checks/deployment -run '^(TestMyAppVersion)$'
```

↓

```go
// Integration test runs with cluster access
func TestMyAppVersion(t *testing.T) {
    // Queries cluster, evaluates constraint
}
```

### Architecture Principles

**Key Insight:** Integration tests ARE the validators. They contain the validation logic directly, not wrapper functions.

**File Structure:**
- `*_version.go` - Helper functions (query cluster, evaluate constraints)
- `*_version_test.go` - Unit tests with table-driven cases using fake clientset
- `*_version_integration_test.go` - Integration test that runs in Jobs with real cluster access

**Separation:**
- **Unit tests**: Fast, use fake clientset, test helper functions
- **Integration tests**: Run in Jobs, use real cluster, test full constraint validation

**Test Runner Pattern:**

```go
runner, err := checks.NewTestRunner(t)
// Provides:
// - runner.Context() - Kubernetes clientset, context, snapshot, recipe
// - runner.GetConstraint(phase, name) - Lookup constraint from recipe
```

### Enforcement Mechanism

Three layers ensure validators are properly implemented:

**1. Automated Registration Tests**

`pkg/validator/checks/registration_test.go` runs in every `make test` and fails if:
- Registered constraint has no test implementation
- Integration test exists without registration
- Registered check has no test implementation

```go
func TestConstraintRegistrationCompleteness(t *testing.T) {
    constraintTests := checks.ListConstraintTests("")
    existingTests := findTestFunctions(t)  // AST parsing

    var missing []string
    for _, ct := range constraintTests {
        if !existingTests[ct.TestName] {
            missing = append(missing, ct.TestName)
        }
    }

    if len(missing) > 0 {
        t.Errorf("Registered constraints missing test implementations")
    }
}
```

**2. Code Generator**

`make generate-validator` scaffolds all files correctly:
- Includes registration automatically in integration test
- Provides TODOs for implementation
- Follows naming conventions

**3. Documentation**

- Comprehensive development guide (this section)
- Generated code has inline examples and TODOs
- Contributing guide integration

### What Gets Caught

| Mistake | How It's Caught |
|---------|-----------------|
| Registered constraint without test | `TestConstraintRegistrationCompleteness` fails |
| Integration test without registration | `TestIntegrationTestsAreRegistered` fails |
| Wrong test function name | Pattern matching fails (test not found) |
| Forgot to implement helpers | Compilation fails (undefined functions) |
| Missing test cases | Coverage check fails |

### Testing Locally

```bash
# Unit tests only (skips integration tests)
go test ./pkg/validator/checks/deployment -short

# Run specific integration test (will skip if not in Kubernetes)
go test ./pkg/validator/checks/deployment -run TestMyAppVersion -v

# All tests including registration validation
make test
```

### Using in Recipe

```yaml
validation:
  deployment:
    constraints:
      - name: Deployment.my-app.version  # Must match registered Pattern
        value: ">= v1.2.0"                # Constraint expression
```

### Common Patterns

#### Multi-Strategy Version Detection

```go
func getComponentVersion(ctx context.Context, clientset kubernetes.Interface) (string, error) {
    deployment := findDeployment(ctx, clientset)

    // Strategy 1: Label
    if version := deployment.Labels["app.kubernetes.io/version"]; version != "" {
        return normalizeVersion(version), nil
    }

    // Strategy 2: Annotation
    if version := deployment.Annotations["version"]; version != "" {
        return normalizeVersion(version), nil
    }

    // Strategy 3: Image tag
    if len(deployment.Spec.Template.Spec.Containers) > 0 {
        image := deployment.Spec.Template.Spec.Containers[0].Image
        return extractVersionFromImage(image), nil
    }

    return "", fmt.Errorf("version not found")
}
```

#### Version Constraint Evaluation

```go
func evaluateVersionConstraint(actualValue, constraintValue string) (bool, error) {
    // Parse operator and expected version
    // Supports: ==, !=, >=, <=, >, <, ~= (compatible)
    op, expected := parseConstraint(constraintValue)

    // Compare using semver
    actual, err := semver.Parse(actualValue)
    if err != nil {
        return false, fmt.Errorf("invalid actual version: %w", err)
    }

    expectedVer, err := semver.Parse(expected)
    if err != nil {
        return false, fmt.Errorf("invalid expected version: %w", err)
    }

    switch op {
    case ">=":
        return actual.GTE(expectedVer), nil
    case "==":
        return actual.Equal(expectedVer), nil
    // ... other operators
    }
}
```

### Benefits

**1. Impossible to Forget Registration** - Tests fail locally and in CI if registration is missing

**2. Easy to Add New Validators** - One command scaffolds everything correctly

**3. Consistent Architecture** - Generated code follows established patterns

**4. Fast Feedback** - Catches issues locally before PR

**5. Self-Documenting** - Generated code has examples and TODOs

**6. CI Enforced** - Can't merge without complete implementation

## Troubleshooting

### Test Wrapper Issues

#### Test Wrapper Not Found by go test

**Symptom:**
```
Job logs: testing: warning: no tests to run
```

**Causes:**
1. Test wrapper function doesn't follow naming convention
2. Test wrapper is not exported (lowercase name)
3. Package doesn't compile

**Solutions:**

Check test function naming:
```go
// Correct
func TestOperatorHealth(t *testing.T) { ... }

// Wrong - lowercase
func TestOperatorhealth(t *testing.T) { ... }

// Wrong - underscore separator
func Test_operator_health(t *testing.T) { ... }
```

**Naming rule:** Convert kebab-case check name to PascalCase:
- `operator-health` → `TestOperatorHealth`
- `nccl-bandwidth` → `TestNCCLBandwidth`
- `expected-resources` → `TestExpectedResources`

Verify test file compiles:
```bash
go test -c ./pkg/validator/checks/deployment/
```

#### Test Wrapper Fails: "check not found in registry"

**Symptom:**
```
Job logs: Check "operator-health" not found in registry
```

**Causes:**
1. Check not registered in `init()` function
2. Package not imported (init() never runs)
3. Check name mismatch between registration and runner call

**Solutions:**

Verify check registration:
```go
// Must be in same package as check function
func init() {
    checks.RegisterCheck(&checks.Check{
        Name:  "operator-health",  // ← Must match exactly
        Phase: "deployment",
        Func:  CheckOperatorHealth,
    })
}
```

Verify test wrapper uses same name:
```go
func TestOperatorHealth(t *testing.T) {
    runner, err := checks.NewTestRunner(t)
    if err != nil {
        t.Skipf("Skipping integration test: %v", err)
        return
    }
    runner.RunCheck("operator-health")  // ← Must match registration
}
```

#### Test Wrapper Fails: "failed to load validation context"

**Symptom (during local testing):**
```
SKIP: Skipping integration test (not in Kubernetes): failed to create in-cluster config
```

**Expected behavior:** Test should skip gracefully when not in Kubernetes.

**Verify skip logic:**
```go
func TestMyCheck(t *testing.T) {
    runner, err := checks.NewTestRunner(t)
    if err != nil {
        // Should skip, not fail
        t.Skipf("Skipping integration test (not in Kubernetes): %v", err)
        return
    }
    runner.RunCheck("my-check")
}
```

**Symptom (inside Job):**
```
Job logs: Failed to create test runner: failed to load validation context:
          failed to read snapshot file: open /data/snapshot/snapshot.yaml: no such file
```

**Causes:**
1. Snapshot ConfigMap not mounted correctly
2. Volume mount path mismatch
3. ConfigMap doesn't exist

**Solutions:**

Check Job pod volumes:
```bash
kubectl get pod <pod-name> -n aicr-validation -o yaml | grep -A 10 volumes
```

Expected volumes:
```yaml
volumes:
- name: snapshot
  configMap:
    name: <snapshot-configmap>
- name: recipe
  configMap:
    name: <recipe-configmap>
volumeMounts:
- name: snapshot
  mountPath: /data/snapshot
  readOnly: true
```

Verify ConfigMap exists:
```bash
kubectl get cm -n <namespace> <snapshot-configmap>
kubectl describe cm -n <namespace> <snapshot-configmap>
```

Check ConfigMap contains snapshot data:
```bash
kubectl get cm -n <namespace> <snapshot-configmap> -o jsonpath='{.data.snapshot\.yaml}' | head -20
```

### Job Execution Issues

#### Job Not Found

**Symptom:**
```
Error: failed to wait for Job completion: Job "aicr-validation-deployment" not found
```

**Causes:**
1. Namespace doesn't exist
2. Job was cleaned up too quickly
3. Job creation failed silently

**Solutions:**

Check if namespace exists:
```bash
kubectl get namespace aicr-validation
```

Create namespace if missing:
```bash
kubectl create namespace aicr-validation
```

Check Job status:
```bash
kubectl get jobs -n aicr-validation
kubectl describe job aicr-validation-deployment -n aicr-validation
```

#### Job Failed to Start

**Symptom:**
```
Error: Job failed with status: ImagePullBackOff
```

**Causes:**
1. Image not accessible
2. Image tag doesn't exist
3. Registry authentication issues

**Solutions:**

Check Job events:
```bash
kubectl describe job aicr-validation-deployment -n aicr-validation
```

Check Pod logs:
```bash
kubectl get pods -n aicr-validation
kubectl describe pod <pod-name> -n aicr-validation
```

Verify image exists:
```bash
docker pull ghcr.io/nvidia/aicr-validator:latest
# or
kubectl run test --image=ghcr.io/nvidia/aicr-validator:latest --rm -it --restart=Never -- /bin/sh
```

#### Job Pods Crash

**Symptom:**
```
Error: Job pod exited with code 1
```

**Solutions:**

View pod logs:
```bash
# Get pod name
kubectl get pods -n aicr-validation -l job-name=aicr-validation-deployment

# View logs
kubectl logs <pod-name> -n aicr-validation

# View logs of crashed pod
kubectl logs <pod-name> -n aicr-validation --previous
```

Common causes in logs:
- `panic: runtime error` - Code bug in check
- `context deadline exceeded` - Timeout
- `permission denied` - RBAC issue
- `connection refused` - Network/API issue

### RBAC Permission Errors

#### Forbidden: User Cannot Access Resource

**Symptom:**
```
Error: failed to list GPU operator pods: pods is forbidden:
User "system:serviceaccount:aicr-validation:aicr-validator" cannot list resource "pods"
in API group "" in the namespace "gpu-operator"
```

**Cause:** ServiceAccount lacks necessary RBAC permissions

**Solutions:**

Check current permissions:
```bash
kubectl auth can-i list pods --namespace=gpu-operator \
  --as=system:serviceaccount:aicr-validation:aicr-validator
```

View current Role/RoleBinding:
```bash
kubectl get role aicr-validator -n aicr-validation -o yaml
kubectl get rolebinding aicr-validator -n aicr-validation -o yaml
```

**Fix:** Create proper RBAC resources:

```yaml
# role.yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: aicr-validator
rules:
  # Deployment phase
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "list"]
  - apiGroups: ["apps"]
    resources: ["deployments", "daemonsets", "statefulsets"]
    verbs: ["get", "list"]
  - apiGroups: [""]
    resources: ["pods", "services", "configmaps"]
    verbs: ["get", "list"]

  # Performance phase
  - apiGroups: ["batch"]
    resources: ["jobs"]
    verbs: ["get", "list", "create", "delete"]
  - apiGroups: [""]
    resources: ["pods/log"]
    verbs: ["get"]

  # Conformance phase
  - apiGroups: ["apiextensions.k8s.io"]
    resources: ["customresourcedefinitions"]
    verbs: ["get", "list"]
```

Apply RBAC:
```bash
kubectl apply -f role.yaml
kubectl create clusterrolebinding aicr-validator \
  --clusterrole=aicr-validator \
  --serviceaccount=aicr-validation:aicr-validator
```

#### RBAC for Cross-Namespace Access

**Issue:** Check needs to access resources in `gpu-operator` namespace but only has permissions in `aicr-validation`

**Solution:** Use ClusterRole instead of Role:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: aicr-validator
rules:
  - apiGroups: ["apps"]
    resources: ["deployments"]
    verbs: ["get", "list"]
  # Add other rules...

---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: aicr-validator
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: aicr-validator
subjects:
  - kind: ServiceAccount
    name: aicr-validator
    namespace: aicr-validation
```

### Timeout Problems

#### Context Deadline Exceeded

**Symptom:**
```
Error: context deadline exceeded
Check: operator-health
Duration: 2m0s
```

**Causes:**
1. Check takes too long to execute
2. Kubernetes API is slow
3. External resource is unresponsive

**Solutions:**

Increase timeout in validator:
```go
validator := validator.New(
    validator.WithTimeout(10 * time.Minute),  // Increase from default 2m
)
```

Increase timeout for specific check:
```go
func CheckOperatorHealth(ctx *checks.ValidationContext) error {
    // Create new context with longer timeout for this check
    checkCtx, cancel := context.WithTimeout(ctx.Context, 5*time.Minute)
    defer cancel()

    pods, err := ctx.Clientset.CoreV1().Pods("gpu-operator").List(
        checkCtx,  // Use extended timeout
        metav1.ListOptions{LabelSelector: "app=gpu-operator"},
    )
    // ...
}
```

Add context cancellation checks for long operations:
```go
func LongRunningCheck(ctx *checks.ValidationContext) error {
    for i := 0; i < 1000; i++ {
        // Check if context is cancelled
        select {
        case <-ctx.Context.Done():
            return ctx.Context.Err()  // Return context error
        default:
            // Continue processing
        }

        // Do work...
    }
    return nil
}
```

#### Job Timeout

**Symptom:**
```
Error: Job did not complete within timeout period
Job: aicr-validation-performance
Timeout: 5m0s
```

**Solutions:**

Increase Job timeout:
```go
config := agent.Config{
    Timeout: 15 * time.Minute,  // Increase for performance tests
}
```

Check if Job is actually running:
```bash
kubectl get pods -n aicr-validation -l job-name=aicr-validation-performance
kubectl logs <pod-name> -n aicr-validation --follow
```

Check Job status:
```bash
kubectl describe job aicr-validation-performance -n aicr-validation
```

### Check Registration Issues

#### Check Not Found

**Symptom:**
```
Error: check "operator-health" not registered
```

**Causes:**
1. Package not imported
2. `init()` not running
3. Check name mismatch

**Solutions:**

Verify check is registered:
```go
func TestCheckRegistered(t *testing.T) {
    check, ok := checks.GetCheck("operator-health")
    if !ok {
        t.Fatal("Check not registered")
    }
    assert.Equal(t, "operator-health", check.Name)
}
```

Ensure package is imported:
```go
// Import with blank identifier to trigger init()
import _ "github.com/NVIDIA/aicr/pkg/validator/checks/deployment"
```

List all registered checks:
```go
func TestListChecks(t *testing.T) {
    allChecks := checks.ListChecks("")
    t.Logf("Registered checks: %d", len(allChecks))
    for _, check := range allChecks {
        t.Logf("  - %s (%s)", check.Name, check.Phase)
    }
}
```

#### Constraint Validator Not Found

**Symptom:**
```
Error: no validator found for constraint "Deployment.gpu-operator.version"
```

**Solutions:**

Check if validator is registered:
```bash
# Run test to list validators
go test -v ./pkg/validator/checks/... -run TestList
```

Verify import:
```go
import _ "github.com/NVIDIA/aicr/pkg/validator/checks/deployment"
```

Check pattern match:
```go
func TestValidatorRegistration(t *testing.T) {
    validator, ok := checks.GetConstraintValidator("Deployment.gpu-operator.version")
    if !ok {
        t.Fatal("Validator not registered")
    }
    assert.Equal(t, "Deployment.gpu-operator.version", validator.Pattern)
}
```

#### Duplicate Registration Panic

**Symptom:**
```
panic: constraint validator for pattern "Deployment.gpu-operator.version" is already registered
```

**Cause:** Same pattern registered twice (likely imported in multiple places)

**Solution:** Only import check packages once, typically in main:
```go
// cmd/aicr/main.go
import (
    _ "github.com/NVIDIA/aicr/pkg/validator/checks/deployment"  // Once here
    _ "github.com/NVIDIA/aicr/pkg/validator/checks/performance"
    _ "github.com/NVIDIA/aicr/pkg/validator/checks/conformance"
)
```

### Constraint Evaluation Errors

#### Invalid Constraint Expression

**Symptom:**
```
Error: invalid constraint expression: cannot parse expected version
Constraint: Deployment.gpu-operator.version
Value: ">= invalid-version"
```

**Solution:** Fix constraint value in recipe:
```yaml
# Wrong
constraints:
  - name: Deployment.gpu-operator.version
    value: ">= invalid-version"

# Correct
constraints:
  - name: Deployment.gpu-operator.version
    value: ">= v24.6.0"
```

#### Version Parse Error

**Symptom:**
```
Error: cannot parse actual version
Actual: "latest"
Expected: ">= v24.6.0"
```

**Cause:** Actual value is not a valid version string

**Solution:** Fix validator to return valid version:
```go
func getVersion(deployment *appsv1.Deployment) string {
    version := deployment.Labels["app.kubernetes.io/version"]
    if version == "latest" {
        // Don't return "latest" - try other strategies
        version = extractVersionFromImage(deployment.Spec.Template.Spec.Containers[0].Image)
    }
    return normalizeVersion(version)
}
```

#### Constraint Always Fails

**Symptom:**
```
Constraint: OS.distribution
Expected: "ubuntu"
Actual: "Ubuntu"
Status: FAIL
```

**Cause:** Case sensitivity in string comparison

**Solution:** Normalize strings in validator:
```go
func ValidateOSDistribution(ctx *checks.ValidationContext, constraint recipe.Constraint) (string, bool, error) {
    actual := strings.ToLower(getOSDistribution(ctx))  // Normalize to lowercase
    expected := strings.ToLower(constraint.Value)

    passed := actual == expected
    return actual, passed, nil
}
```

### Kubernetes Client Errors

#### Cannot Connect to Cluster

**Symptom:**
```
Error: failed to create Kubernetes client: unable to load kubeconfig
```

**Solutions:**

Check kubeconfig:
```bash
kubectl cluster-info
echo $KUBECONFIG
ls -la ~/.kube/config
```

Test connectivity:
```bash
kubectl get nodes
```

Verify in code:
```go
clientset, err := k8sclient.GetKubeClient()
if err != nil {
    log.Fatalf("Failed to create k8s client: %v", err)
}

// Test connection
nodes, err := clientset.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
if err != nil {
    log.Fatalf("Cannot connect to cluster: %v", err)
}
log.Printf("Connected to cluster with %d nodes", len(nodes.Items))
```

#### Resource Not Found

**Symptom:**
```
Error: deployments.apps "gpu-operator" not found
```

**Causes:**
1. Resource doesn't exist
2. Wrong namespace
3. Wrong name

**Solutions:**

Check if resource exists:
```bash
kubectl get deployments -A | grep gpu-operator
```

Use multi-namespace search in validator:
```go
func findGPUOperator(ctx context.Context, clientset kubernetes.Interface) (*appsv1.Deployment, error) {
    namespaces := []string{"gpu-operator", "nvidia-gpu-operator", "kube-system"}
    names := []string{"gpu-operator", "nvidia-gpu-operator"}

    for _, ns := range namespaces {
        for _, name := range names {
            deployment, err := clientset.AppsV1().Deployments(ns).Get(
                ctx, name, metav1.GetOptions{},
            )
            if err == nil {
                return deployment, nil
            }
        }
    }

    return nil, fmt.Errorf("GPU operator not found in any common namespace")
}
```

### Test Mode vs Production

#### Tests Pass but Production Fails

**Symptom:**
- Unit tests pass with fake clientset
- Production validation fails with real cluster

**Causes:**
1. Fake clientset doesn't match real cluster state
2. RBAC works in test but not production
3. Timing issues (context timeout)

**Solutions:**

Test with real cluster:
```bash
# Integration test against real cluster
export USE_REAL_CLUSTER=true
go test -v ./pkg/validator/checks/deployment/... -run TestIntegration
```

Add integration tests:
```go
func TestOperatorHealthIntegration(t *testing.T) {
    if os.Getenv("USE_REAL_CLUSTER") != "true" {
        t.Skip("Skipping integration test")
    }

    clientset, err := k8sclient.GetKubeClient()
    require.NoError(t, err)

    ctx := &checks.ValidationContext{
        Context:   context.Background(),
        Clientset: clientset,
    }

    err = CheckOperatorHealth(ctx)
    assert.NoError(t, err)
}
```

#### Validation Passes in Test Mode

**Symptom:**
```
WARN Job deployment failed (likely test mode), returning skeleton check
Check: operator-health
Status: pass
Reason: skipped - Job deployment failed (test mode)
```

**Cause:** Test environment doesn't have namespace, so checks are skipped

**Solutions:**

Create test namespace:
```bash
kubectl create namespace aicr-validation
```

Or run tests with fake clientset:
```go
func TestWithFakeCluster(t *testing.T) {
    deployment := createTestDeployment()
    clientset := fake.NewSimpleClientset(deployment)

    ctx := &checks.ValidationContext{
        Context:   context.Background(),
        Clientset: clientset,
    }

    // Test directly against check function, not Job execution
    err := CheckOperatorHealth(ctx)
    assert.NoError(t, err)
}
```

### Debugging Techniques

#### Enable Debug Logging

```go
import "log/slog"

func init() {
    slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
        Level: slog.LevelDebug,
    })))
}
```

#### View Job Logs in Real-Time

```bash
# Watch for new Jobs
kubectl get jobs -n aicr-validation -w

# Stream logs from running Job
POD=$(kubectl get pods -n aicr-validation -l job-name=aicr-validation-deployment -o name | head -1)
kubectl logs -n aicr-validation $POD --follow
```

#### Check Job Results ConfigMap

```bash
# List result ConfigMaps
kubectl get configmaps -n aicr-validation

# View specific result
kubectl get configmap aicr-validation-deployment-result -n aicr-validation -o yaml
```

#### Debug Check Function Directly

```go
func TestDebugCheck(t *testing.T) {
    // Set up test data
    deployment := createGPUOperatorDeployment("gpu-operator", "gpu-operator",
        map[string]string{"app.kubernetes.io/version": "v24.6.0"},
        "nvcr.io/nvidia/gpu-operator:v24.6.0")

    clientset := fake.NewSimpleClientset(deployment)

    ctx := &checks.ValidationContext{
        Context:   context.Background(),
        Clientset: clientset,
    }

    // Call check directly (no Job)
    err := CheckOperatorHealth(ctx)
    if err != nil {
        t.Logf("Check failed: %v", err)
        t.Fail()
    }
}
```

#### Trace Constraint Evaluation

```go
func ValidateGPUOperatorVersion(ctx *checks.ValidationContext, constraint recipe.Constraint) (string, bool, error) {
    slog.Debug("Starting version validation",
        "constraint", constraint.Name,
        "expectedValue", constraint.Value)

    version, err := getGPUOperatorVersion(ctx.Context, ctx.Clientset)
    slog.Debug("Detected version", "version", version, "error", err)

    if err != nil {
        return "", false, err
    }

    passed, err := evaluateVersionConstraint(version, constraint.Value)
    slog.Debug("Constraint evaluation result",
        "version", version,
        "constraint", constraint.Value,
        "passed", passed,
        "error", err)

    return version, passed, err
}
```

#### Use kubectl debug

```bash
# Debug a running Job pod
kubectl debug -n aicr-validation <pod-name> -it --image=busybox

# Check environment and mounts
env | grep AICR
ls -la /aicr/snapshot
cat /aicr/snapshot/snapshot.yaml
```

#### Collect Diagnostic Information

When reporting issues, include:

```bash
# Cluster info
kubectl version
kubectl get nodes

# Validation namespace
kubectl get all -n aicr-validation

# Job details
kubectl describe job <job-name> -n aicr-validation

# Pod logs
kubectl logs <pod-name> -n aicr-validation

# RBAC
kubectl auth can-i --list --as=system:serviceaccount:aicr-validation:aicr-validator
```

#### Common kubectl Commands

```bash
# List all validation Jobs
kubectl get jobs -n aicr-validation

# Delete failed Jobs
kubectl delete job -n aicr-validation -l status=failed

# Clean up validation namespace
kubectl delete namespace aicr-validation

# Re-create validation namespace
kubectl create namespace aicr-validation

# View events
kubectl get events -n aicr-validation --sort-by='.lastTimestamp'
```

## Migration from Inline Validation

**Before (inline constraint evaluation):**
```go
// phases.go - deployment phase
for _, constraint := range recipe.Validation.Deployment.Constraints {
    result := evaluateConstraint(constraint, snapshot) // Wrong - no cluster access
}
```

**After (Job-based constraint validation):**
```go
// deployment/constraints.go
func ValidateDeploymentConstraint(ctx *ValidationContext, constraint recipe.Constraint) {
    // Correct - has cluster access via ctx.Clientset
    deployment := ctx.Clientset.AppsV1().Deployments(...).Get(...)
}
```

## References

### Summary

| Task | File Location | Key Function | Registry Call |
|------|---------------|--------------|---------------|
| Add Check | `pkg/validator/checks/<phase>/*.go` | `func(ctx *ValidationContext) error` | `RegisterCheck()` |
| Add Constraint | `pkg/validator/checks/<phase>/constraints.go` | `func(ctx *ValidationContext, constraint recipe.Constraint) (string, bool, error)` | `RegisterConstraintValidator()` |

Both use `init()` for self-registration and are discovered automatically at runtime.

### Key Files

- **registry.go**: Check and constraint validator registration infrastructure
- **runner.go**: Test runner for Job execution
- **deployment/operator_health_check.go**: Example check implementation
- **deployment/constraints.go**: Example constraint validator implementation
- **Constraint Parser**: `pkg/validator/constraint_expression.go`
