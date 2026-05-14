# Validator Development Guide

Learn how to add new validation checks to AICR.

## Overview

AICR uses a container-per-validator model. Each validation check runs as an isolated Kubernetes Job with access to the cluster, a snapshot, and the recipe. Validators are organized into three phases:

| Phase | Purpose | Example |
|-------|---------|---------|
| `deployment` | Verify components are installed and healthy | GPU operator pods running, expected resources present |
| `performance` | Verify system meets performance thresholds | NCCL all-reduce bandwidth (training), AIPerf inference throughput & TTFT p99 (inference+Dynamo) |
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

### RBAC

The validator engine creates a per-run ServiceAccount and ClusterRoleBinding for every `aicr validate` invocation. Both are named `aicr-validator-<runID>` where `<runID>` is the unique identifier generated at the start of the run (see `pkg/validator/job/rbac.go`). Per-run naming prevents concurrent validation runs from clobbering each other's RBAC and ensures cleanup at run end deletes only the resources owned by that run.

External tooling that needs to match validator RBAC (e.g., for monitoring or cleanup) should select by the `app.kubernetes.io/name=aicr-validator` label rather than by literal resource name, since the suffix changes every run.

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
| `AICR_CHECK_TIMEOUT` | Parent-context timeout for the check, injected by the Job deployer from the catalog entry's `timeout` field (Go duration string, e.g. `30m`). Falls back to `defaults.CheckExecutionTimeout` when unset or malformed; a malformed value is logged at WARN. Use `ctx.Ctx` (set by `LoadContext`) to honor it. |
| `AICR_VALIDATOR_IMAGE_TAG` | Override the resolved image tag (e.g. `latest`). Bypasses the default `:v<version>` / `:sha-<commit>` resolution for feature-branch dev builds whose commit has no published image. |
| `AICR_NODE_SELECTOR` | User-provided node selector override for inner workloads (comma-separated `key=value` pairs). Set by the `--node-selector` CLI flag. Use `ctx.NodeSelector` to access the parsed value. |
| `AICR_TOLERATIONS` | User-provided toleration override for inner workloads (comma-separated `key=value:effect` entries). Set by the `--toleration` CLI flag. Use `ctx.Tolerations` to access the parsed value. |

## Context API

The `validators.Context` struct provides all dependencies a check needs:

```go
type Context struct {
    Ctx             context.Context        // Parent context with timeout
    Cancel          context.CancelFunc     // Release resources (caller must defer)
    Clientset       kubernetes.Interface   // Typed K8s client
    RESTConfig      *rest.Config           // For exec, port-forward, dynamic client
    DynamicClient   dynamic.Interface      // For CRD access
    Snapshot        *snapshotter.Snapshot  // Captured cluster state
    ValidationInput *v1.ValidationInput    // Validation specification (config + context)
    Namespace       string                 // Validation namespace
    NodeSelector    map[string]string      // User-provided node selector override (nil = use defaults)
    Tolerations     []corev1.Toleration    // User-provided toleration override (nil = use defaults)
}
```

`LoadContext()` builds this from the container environment: reads mounted ConfigMaps, creates in-cluster K8s clients, and sets the parent-context timeout via `validators/context.go:checkTimeoutFromEnv` — which honors `AICR_CHECK_TIMEOUT` (injected by the Job deployer from the catalog entry's `timeout` field) and falls back to `defaults.CheckExecutionTimeout` when unset or malformed.

### Scheduling Overrides

When creating inner workloads (pods, Jobs, TrainJobs), check `ctx.NodeSelector` and `ctx.Tolerations` before applying hardcoded platform selectors. If non-nil, these override the default scheduling constraints to support clusters with non-standard GPU node labels or taints.

```go
// Apply scheduling overrides when creating inner workload pods.
nodeSelector := map[string]string{"cloud.google.com/gke-accelerator": "nvidia-h100-mega-80gb"}
if ctx.NodeSelector != nil {
    nodeSelector = ctx.NodeSelector // user override replaces platform default
}

tolerations := []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
if ctx.Tolerations != nil {
    tolerations = ctx.Tolerations // user override replaces default tolerate-all
}
```

Validators that use `nodeName` pinning (e.g., nvidia-smi, DRA isolation) bypass the scheduler entirely and should not apply `ctx.NodeSelector`.

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
    if ctx.ValidationInput == nil {
        return validators.Skip("no validation input provided")
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
2. On non-release dev builds with a valid commit, `:latest` becomes `:sha-<commit>` (matches the tags `on-push.yaml` pushes for merges to `main`)
3. Explicit version tags (e.g., `:v1.2.3`) are not modified by steps 1-2
4. `AICR_VALIDATOR_IMAGE_TAG` overrides the resolved tag on every validator image, including explicit catalog tags. Use this when running `aicr validate` from a feature-branch dev build whose commit has not been merged to `main` (no `:sha-<commit>` image has been published). Typical value: `latest`. Example: `AICR_VALIDATOR_IMAGE_TAG=latest aicr validate --phase performance ...`
5. `AICR_VALIDATOR_IMAGE_REGISTRY` overrides the registry prefix

**Digest-pinned references** (`name@sha256:…`) are not rewritten by step 4. A tag override is meaningless against a content-addressable pin, and naive rewriting would corrupt the digest. Step 5's registry override still applies — only the registry prefix changes, the digest is preserved verbatim.

**Env-var forwarding to the validator pod:** `AICR_CLI_VERSION`, `AICR_CLI_COMMIT`, `AICR_VALIDATOR_IMAGE_REGISTRY`, and `AICR_VALIDATOR_IMAGE_TAG` are forwarded from the CLI invocation into the validator container so that validators resolving inner workload images at runtime (e.g. `inference-perf`'s AIPerf benchmark Job) apply the same semantics as `catalog.Load`. If you set `AICR_VALIDATOR_IMAGE_TAG=latest` on the CLI, the override reaches both the outer validator Job and the inner benchmark Job — they always travel together.

**Pull-policy behavior when the override is set:** both the outer validator Job and every inner workload Job it dispatches route through the shared `catalog.ImagePullPolicy(image)` helper (`pkg/validator/catalog/catalog.go`). The rule, in precedence order, is:

1. **Side-loaded refs** (`ko.local/*`, `kind.local/*`) → `Never` (no registry to pull from).
2. **Digest-pinned refs** (`name@sha256:…`) → `IfNotPresent`. Cryptographic immutability means a cached copy is always correct; forcing `Always` here would make kubelet re-contact the registry every run, which breaks disconnected / air-gapped clusters even though the image itself was never overridden.
3. **`AICR_VALIDATOR_IMAGE_TAG` is set** → `Always`. Override values are typically mutable (`latest`, `edge`, `main`, or any tag `on-push.yaml` recreates on every merge), so `IfNotPresent` would let a node's previously cached image win over the tag's current target.
4. **`:latest` suffix** → `Always`. Mutable tag by convention.
5. **Otherwise** → `IfNotPresent`. Versioned tag assumed immutable enough that caching is a win.

Callers in this repo: the outer validator Job's `Deployer.imagePullPolicy()` (`pkg/validator/job/deployer.go`) and the inner AIPerf benchmark pod spec in `buildAIPerfJob` (`validators/performance/inference_perf_constraint.go`). They both delegate to the same helper so their policy can't drift. When adding a new inner workload Job in `validators/<phase>/*`, set `ImagePullPolicy: catalog.ImagePullPolicy(<resolved image>)` on the container to keep the invariant.

**Performance phase example — inference perf:**

```yaml
- name: inference-perf
  phase: performance
  description: "Verify inference throughput and TTFT p99 meet thresholds using AIPerf"
  image: ghcr.io/nvidia/aicr-validators/performance:latest
  timeout: 50m
  args: ["inference-perf"]
```

Paired constraints in an overlay (one per metric the check produces):

```yaml
validation:
  performance:
    checks: [inference-perf]
    constraints:
      - name: inference-throughput   # output tokens/sec, >= threshold
        value: ">= 5000"
      - name: inference-ttft-p99     # time-to-first-token p99 in ms, <= threshold
        value: "<= 200"
```

## Performance Validators

Four performance checks ship today (see [`recipes/validators/catalog.yaml`](https://github.com/NVIDIA/aicr/blob/main/recipes/validators/catalog.yaml) for the authoritative list), registered in `validators/performance/main.go`:

| Check | Intent | Workload | Constraints |
|-------|--------|----------|-------------|
| `nccl-all-reduce-bw` | training | NCCL `all_reduce_perf` under a Kubeflow `TrainJob` | `nccl-all-reduce-bw >= N GB/s` |
| `nccl-all-reduce-bw-net` | training | NCCL `all_reduce_perf` over network fabric | `nccl-all-reduce-bw-net >= N GB/s` |
| `nccl-all-reduce-bw-nvls` | training | NCCL `all_reduce_perf` with NVLink Sharp | `nccl-all-reduce-bw-nvls >= N GB/s` |
| `inference-perf` | inference+Dynamo | `DynamoGraphDeployment` (vLLM, Qwen/Qwen3-0.6B) + AIPerf Job | `inference-throughput >= N tok/s`, `inference-ttft-p99 <= N ms` |

> **Constraint-name contract.** Each NCCL variant looks up a constraint with the *exact* same name as the check (`constraintNameForVariant` in `validators/performance/nccl_all_reduce_bw.go`). A recipe that runs the `-net` or `-nvls` variant **must** declare a same-named constraint; a generic `nccl-all-reduce-bw` constraint only satisfies the legacy default variant and the variant checks will Skip when it's the only one present.

Both follow a consistent lifecycle:

1. **Deploy** a fresh benchmark workload. `inference-perf` always provisions its own `DynamoGraphDeployment` into a per-run namespace (`aicr-inference-perf-<hash>`) derived from `AICR_RUN_ID`, so two concurrent runs cannot collide and a prior run's leftovers cannot be silently adopted. An earlier design sketch had a "discover existing frontend" path — it was intentionally dropped because it admitted ambiguity about which service was being benchmarked on shared clusters.
2. **Wait for readiness** via the watch API (not polling) on the workload CR's status.
3. **Run the benchmark** in a K8s Job, capturing stdout with sentinels that survive noisy logs.
4. **Parse and evaluate** against recipe constraints with a 10% tolerance.
5. **Defer cleanup** — the per-run namespace is torn down on both success and failure so leaked workloads from interrupted prior runs are reaped on the next invocation.

The inference check injects pod-scheduling (nodeSelector, tolerations, DRA `resourceClaims`) into the unstructured `DynamoGraphDeployment` programmatically rather than via text substitution, to avoid YAML-escape issues with taint values.

**AIPerf runner image.** The benchmark Job spawned by `inference-perf` pulls a pre-built image (`ghcr.io/nvidia/aicr-validators/aiperf-bench:<tag>`) with `aiperf` already `pip install`-ed. The image is published by the same `on-tag.yaml` workflow that publishes the three Go validator images; its Dockerfile at `validators/performance/aiperf-bench.Dockerfile` pins the `AIPERF_VERSION` build arg. Baking the install at release time (rather than `pip install` on every benchmark pod) removes the PyPI runtime dependency, eliminates a ~30 s warmup, and keeps the check air-gap-friendly on clusters with only ghcr.io access.

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
│   ├── main.go                       # Registers nccl-all-reduce-bw, inference-perf
│   ├── Dockerfile
│   ├── nccl_all_reduce_bw.go             # Training: NCCL CheckFunc wrapper
│   ├── nccl_all_reduce_bw_constraint.go  # Training: NCCL pipeline (deploy → bench → parse)
│   ├── inference_perf.go                 # Inference: AIPerf CheckFunc wrapper (constraint eval)
│   ├── inference_perf_constraint.go      # Inference: Dynamo deploy → AIPerf → parse pipeline
│   ├── aiperf-bench.Dockerfile           # Pre-built AIPerf benchmark runner image
│   └── testdata/                         # Workload YAML templates (NCCL TrainJob, Dynamo CR, DRA claim)
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

## Testing with Custom Images

When developing validators, you can build and push a custom image to test on a live cluster before merging.

Edit the embedded catalog to point at your custom image and rebuild the CLI:

```yaml
# recipes/validators/catalog.yaml
  - name: nccl-all-reduce-bw
    phase: performance
    image: my-registry.example.com/my-validator:dev  # custom image
    timeout: 30m
    args: ["nccl-all-reduce-bw"]
```

```shell
make build
./dist/aicr_*/aicr validate --recipe recipe.yaml --snapshot snapshot.yaml \
  --image-pull-secret my-registry-secret
```

The catalog is embedded in the binary at build time, so a rebuild is required. Revert before pushing:

```shell
git checkout -- recipes/validators/catalog.yaml
```

**Use a unique tag for every rebuild.** Catalog entries use pinned image tags, which Kubernetes resolves with `imagePullPolicy: IfNotPresent` by default — so re-pushing the same tag (e.g., `:dev`) leaves previously-pulled nodes running the stale image. In dev loops, suffix the tag per iteration (`:dev-v1`, `:dev-v2`, or `:dev-$(git rev-parse --short HEAD)`) so every rebuild forces a fresh pull cluster-wide. Release builds avoid this entirely because `on-tag.yaml` publishes semver tags that are never reused.

### Private Registry Authentication

If your image is in a private registry, create an image pull secret in the validation namespace and pass it to the CLI with `--image-pull-secret`:

```shell
# Create the secret (use --dry-run=client | apply for idempotent create-or-update)
kubectl create secret docker-registry my-registry-secret \
  --docker-server=nvcr.io \
  --docker-username='$oauthtoken' \
  --docker-password=$NGC_API_KEY \
  -n aicr-validation \
  --dry-run=client -o yaml | kubectl apply -f -

# Run validation with the secret
aicr validate \
  --recipe recipe.yaml \
  --snapshot snapshot.yaml \
  --image-pull-secret my-registry-secret
```

The secret must be of type `kubernetes.io/dockerconfigjson` and exist in the validation namespace. The `--image-pull-secret` flag can be repeated for multiple registries.

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
