# Validator Development Guide

Learn how to add new validation checks to AICR.

## Overview

AICR uses a container-per-validator model. Each validation check runs as an isolated Kubernetes Job with access to the cluster, a snapshot, and the recipe. Validators are organized into three phases:

| Phase | Purpose | Example |
|-------|---------|---------|
| `deployment` | Verify components are installed and healthy | GPU operator pods running, Helm values match recipe |
| `performance` | Verify system meets performance thresholds | NCCL bandwidth, GPU utilization |
| `conformance` | Verify workload-specific requirements | DRA support, gang scheduling, autoscaling |

**Architecture:**

- **Declarative Catalog**: Validators are defined in `recipes/validators/catalog.yaml`
- **Container Contract**: Exit code 0 = pass, 1 = fail, 2 = skip
- **Evidence via stdout**: Check output printed to stdout is captured as CTRF evidence
- **Debug via stderr**: Structured logs go to stderr and are streamed to the user
- **CTRF Reports**: Results are aggregated into [Common Test Report Format](https://ctrf.io/) JSON

## Quick Start

Adding a new check to an existing validator container requires three steps.

### Step 1: Implement the Check Function

Create a new file in the appropriate phase directory (e.g., `validators/deployment/`):

```go
package main

import (
    "fmt"
    "log/slog"

    "github.com/NVIDIA/aicr/pkg/errors"
    "github.com/NVIDIA/aicr/validators"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func checkMyComponent(ctx *validators.Context) error {
    slog.Info("checking my-component health")

    pods, err := ctx.Clientset.CoreV1().Pods("my-namespace").List(
        ctx.Ctx,
        metav1.ListOptions{LabelSelector: "app=my-component"},
    )
    if err != nil {
        return errors.Wrap(errors.ErrCodeInternal, "failed to list pods", err)
    }

    if len(pods.Items) == 0 {
        return errors.New(errors.ErrCodeNotFound, "no my-component pods found")
    }

    // Evidence to stdout (captured in CTRF report)
    fmt.Printf("Found %d my-component pod(s)\n", len(pods.Items))
    for _, pod := range pods.Items {
        fmt.Printf("  %s: %s\n", pod.Name, pod.Status.Phase)
    }

    return nil
}
```

### Step 2: Register in `main.go`

Add the check function to the dispatch map in `validators/deployment/main.go`:

```go
func main() {
    validators.Run(map[string]validators.CheckFunc{
        "operator-health":    checkOperatorHealth,
        "expected-resources": checkExpectedResources,
        // Add your check here:
        "my-component":       checkMyComponent,
    })
}
```

### Step 3: Add Catalog Entry

Add an entry to `recipes/validators/catalog.yaml`:

```yaml
validators:
  # ... existing entries ...

  - name: my-component
    phase: deployment
    description: "Verify my-component pods are running and healthy"
    image: ghcr.io/nvidia/aicr-validators/deployment:latest
    timeout: 2m
    args: ["my-component"]
    env: []
```

The `args` field must match the key used in the `validators.Run()` dispatch map.

## Container Contract

Every validator container must follow this contract:

### Exit Codes

| Code | Meaning | CTRF Status |
|------|---------|-------------|
| `0` | Check passed | `passed` |
| `1` | Check failed | `failed` |
| `2` | Check skipped (not applicable) | `skipped` |

### I/O Channels

| Channel | Purpose | Captured By |
|---------|---------|-------------|
| **stdout** | Evidence output (human-readable check results) | CTRF report `message` field |
| **stderr** | Debug/progress logs (`slog` output) | Streamed live to user terminal |
| `/dev/termination-log` | Failure reason (max 4096 bytes) | CTRF report on failure |

### Mounted Data

The validator engine mounts snapshot and recipe data as ConfigMaps:

| Path | Content | Environment Override |
|------|---------|---------------------|
| `/data/snapshot/snapshot.yaml` | Cluster snapshot | `AICR_SNAPSHOT_PATH` |
| `/data/recipe/recipe.yaml` | Recipe with constraints | `AICR_RECIPE_PATH` |

### Environment Variables

| Variable | Description |
|----------|-------------|
| `AICR_NAMESPACE` | Validation namespace (fallback if ServiceAccount namespace unavailable) |
| `AICR_SNAPSHOT_PATH` | Override snapshot mount path |
| `AICR_RECIPE_PATH` | Override recipe mount path |
| `AICR_VALIDATOR_IMAGE_REGISTRY` | Override image registry prefix (set by user) |

## Context API

The `validators.Context` struct provides all dependencies a check needs:

```go
type Context struct {
    Ctx           context.Context        // Parent context with timeout
    Cancel        context.CancelFunc     // Release resources (caller must defer)
    Clientset     kubernetes.Interface   // Typed K8s client
    RESTConfig    *rest.Config           // For exec, port-forward, dynamic client
    DynamicClient dynamic.Interface      // For CRD access
    Snapshot      *snapshotter.Snapshot  // Captured cluster state
    Recipe        *recipe.RecipeResult   // Recipe with validation config
    Namespace     string                 // Validation namespace
}
```

`LoadContext()` builds this from the container environment: reads mounted ConfigMaps, creates in-cluster K8s clients, and sets a timeout from `defaults.CheckExecutionTimeout`.

### Helper Methods

**`ctx.Timeout(d)`** — Create a child context with a specific timeout:

```go
subCtx, cancel := ctx.Timeout(30 * time.Second)
defer cancel()
pods, err := ctx.Clientset.CoreV1().Pods(ns).List(subCtx, opts)
```

### Runner Utilities

**`validators.Run(checks)`** — Main entry point for validator containers. Handles context loading, check dispatch by `os.Args[1]`, exit codes, and termination log writing.

**`validators.Skip(reason)`** — Return from a `CheckFunc` to indicate the check is not applicable. The runner exits with code 2:

```go
func checkFeatureX(ctx *validators.Context) error {
    if ctx.Recipe.Validation == nil {
        return validators.Skip("no validation section in recipe")
    }
    // ... actual check logic ...
    return nil
}
```

## Catalog Entry Schema

Each entry in `recipes/validators/catalog.yaml`:

```yaml
- name: operator-health           # Unique identifier, used in Job names
  phase: deployment               # deployment | performance | conformance
  description: "Human-readable"   # Shown in CTRF report
  image: ghcr.io/.../img:latest   # OCI image reference
  timeout: 2m                     # Job activeDeadlineSeconds
  args: ["operator-health"]       # Container args (check name)
  env:                            # Optional environment variables
    - name: MY_VAR
      value: "my-value"
  resources:                      # Optional resource requests (omit for defaults)
    cpu: "100m"
    memory: "128Mi"
```

**Image tag resolution** (applied by `catalog.Load`):

1. `:latest` tags are replaced with the CLI version (e.g., `:v0.9.5`) for release builds
2. Explicit version tags (e.g., `:v1.2.3`) are never modified
3. `AICR_VALIDATOR_IMAGE_REGISTRY` overrides the registry prefix

## Code Walkthrough

The `operator_health.go` check demonstrates the standard pattern:

```go
// validators/deployment/operator_health.go

func checkOperatorHealth(ctx *validators.Context) error {
    // 1. Use slog for debug output (goes to stderr, streamed to user)
    slog.Info("listing pods", "namespace", gpuOperatorNamespace)

    // 2. Use ctx.Clientset for K8s API calls
    pods, err := ctx.Clientset.CoreV1().Pods(gpuOperatorNamespace).List(
        ctx.Ctx,
        metav1.ListOptions{LabelSelector: gpuOperatorLabel},
    )
    if err != nil {
        // 3. Return wrapped errors for failures
        return errors.Wrap(errors.ErrCodeInternal, "failed to list pods", err)
    }

    // 4. Print evidence to stdout (captured in CTRF report)
    fmt.Printf("Found %d gpu-operator pod(s):\n", len(pods.Items))
    for _, pod := range pods.Items {
        fmt.Printf("  %s: %s\n", pod.Name, pod.Status.Phase)
    }

    // 5. Return nil for pass, non-nil error for fail
    if runningCount == 0 {
        return errors.New(errors.ErrCodeInternal, "no pods in Running state")
    }
    return nil
}
```

**Key patterns:**

- `slog.*` → stderr → streamed live to user
- `fmt.Printf` → stdout → captured as CTRF evidence
- `return nil` → exit 0 → passed
- `return errors.*` → exit 1 → failed (message written to termination log)
- `return validators.Skip(reason)` → exit 2 → skipped

## Directory Layout

```
validators/
├── context.go              # Shared Context type and LoadContext()
├── runner.go               # Run() entry point, exit code handling
├── deployment/             # Deployment phase validators
│   ├── main.go             # Check dispatch map
│   ├── Dockerfile          # Container image build
│   ├── operator_health.go  # Individual check implementation
│   ├── expected_resources.go
│   └── ...
├── performance/            # Performance phase validators
│   ├── main.go
│   ├── Dockerfile
│   └── ...
├── conformance/            # Conformance phase validators
│   ├── main.go
│   ├── Dockerfile
│   └── ...
└── chainsaw/               # Chainsaw test runner utilities
    └── ...
```

Each phase directory produces one container image. Multiple checks are compiled into a single binary and selected via the first argument.

## Testing

### Unit Tests

Use fake K8s clients for isolated testing:

```go
func TestCheckMyComponent(t *testing.T) {
    tests := []struct {
        name    string
        pods    []corev1.Pod
        wantErr bool
    }{
        {
            name: "healthy pods",
            pods: []corev1.Pod{
                {
                    ObjectMeta: metav1.ObjectMeta{
                        Name:   "my-pod",
                        Labels: map[string]string{"app": "my-component"},
                    },
                    Status: corev1.PodStatus{Phase: corev1.PodRunning},
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
            objects := make([]runtime.Object, len(tt.pods))
            for i := range tt.pods {
                objects[i] = &tt.pods[i]
            }
            ctx := &validators.Context{
                Ctx:       context.TODO(),
                Clientset: fake.NewClientset(objects...),
                Namespace: "test",
            }
            err := checkMyComponent(ctx)
            if (err != nil) != tt.wantErr {
                t.Errorf("error = %v, wantErr %v", err, tt.wantErr)
            }
        })
    }
}
```

### Local Testing with Docker

Build and run a validator locally against mounted data:

```shell
# Build the validator image
docker build -f validators/deployment/Dockerfile -t my-validator .

# Run with mounted snapshot and recipe
docker run --rm \
  -v ./snapshot.yaml:/data/snapshot/snapshot.yaml \
  -v ./recipe.yaml:/data/recipe/recipe.yaml \
  my-validator my-component

# Check exit code
echo $?  # 0=pass, 1=fail, 2=skip
```

Note: K8s API calls will fail locally unless you mount a kubeconfig. For checks that only read snapshot/recipe data, this works without cluster access.

## Checklist

When adding a new upstream check:

1. Create `validators/{phase}/my_check.go` implementing `CheckFunc`
2. Register in `validators/{phase}/main.go` dispatch map
3. Add catalog entry in `recipes/validators/catalog.yaml`
4. Add the check name to the recipe's `validation.{phase}.checks[]` (or omit to run all)
5. Write table-driven unit tests with fake K8s clients
6. Test locally with `docker run` and mounted data
7. Run `make test` with race detector

## See Also

- [Validator Extension Guide](../integrator/validator-extension.md) — External validators via `--data`
- [Validator Catalog Reference](https://github.com/NVIDIA/aicr/tree/main/recipes/validators) — Catalog schema and entries
- [Validator V2 ADR](https://github.com/NVIDIA/aicr/blob/main/docs/design/002-validatorv2-adr.md) — Architecture decision record
- [CLI Reference](../user/cli-reference.md#aicr-validate) — Validate command flags
