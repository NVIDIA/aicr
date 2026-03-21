# CLI Reference

Complete reference for the `aicr` command-line interface.

## Overview

AICR provides a four-step workflow for optimizing GPU infrastructure:

```
┌──────────────┐      ┌──────────────┐      ┌──────────────┐      ┌──────────────┐
│   Snapshot   │─────▶│    Recipe    │─────▶│   Validate   │─────▶│    Bundle    │
└──────────────┘      └──────────────┘      └──────────────┘      └──────────────┘
```

**Step 1**: Capture system configuration  
**Step 2**: Generate optimization recipes  
**Step 3**: Validate constraints against cluster  
**Step 4**: Create deployment bundles  

## Global Flags

Available for all commands:

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--debug` | `-d` | bool | false | Enable debug logging (text mode with full metadata) |
| `--log-json` | | bool | false | Enable JSON logging (structured output for machine parsing) |
| `--help` | `-h` | bool | false | Show help |
| `--version` | `-v` | bool | false | Show version |

### Logging Modes

AICR supports three logging modes:

1. **CLI Mode (default)**: Minimal user-friendly output
   - Just message text without timestamps or metadata
   - Error messages display in red (ANSI color)
   - Example: `Snapshot captured successfully`

2. **Text Mode (`--debug`)**: Debug output with full metadata
   - Key=value format with time, level, source location
   - Example: `time=2025-01-06T10:30:00.123Z level=INFO module=aicr version=v1.0.0 msg="snapshot started"`

3. **JSON Mode (`--log-json`)**: Structured JSON for automation
   - Machine-readable format for log aggregation
   - Example: `{"time":"2025-01-06T10:30:00.123Z","level":"INFO","msg":"snapshot started"}`

**Examples:**
```shell
# Default: Clean CLI output
aicr snapshot

# Debug mode: Full metadata
aicr --debug snapshot

# JSON mode: Structured logs
aicr --log-json snapshot

# Combine with other flags
aicr --debug --output system.yaml snapshot
```

## Commands

### aicr snapshot

Capture comprehensive system configuration including OS, GPU, Kubernetes, and SystemD settings.

**Synopsis:**
```shell
aicr snapshot [flags]
```

**Flags:**
| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--output` | `-o` | string | stdout | Output destination: file path, ConfigMap URI (cm://namespace/name), or stdout |
| `--format` | `-f` | string | yaml | Output format: json, yaml, table |
| `--kubeconfig` | `-k` | string | ~/.kube/config | Path to kubeconfig file (overrides KUBECONFIG env) |
| `--namespace` | `-n` | string | gpu-operator | Kubernetes namespace for agent deployment |
| `--image` | | string | ghcr.io/nvidia/aicr:latest | Container image for agent Job |
| `--job-name` | | string | aicr | Name for the agent Job |
| `--service-account-name` | | string | aicr | ServiceAccount name for agent Job |
| `--node-selector` | | string[] | | Node selector for agent scheduling (key=value, repeatable) |
| `--toleration` | | string[] | all taints | Tolerations for agent scheduling (key=value:effect, repeatable). **Default: all taints tolerated** (uses `operator: Exists`). Only specify to restrict which taints are tolerated. |
| `--timeout` | | duration | 5m | Timeout for agent Job completion |
| `--no-cleanup` | | bool | false | Skip removal of Job and RBAC resources on completion. **Warning:** leaves a cluster-admin ClusterRoleBinding active. |
| `--privileged` | | bool | true | Run agent in privileged mode (required for GPU/SystemD collectors). Set to false for PSS-restricted namespaces. |
| `--template` | | string | | Path to Go template file for custom output formatting (requires YAML format) |
| `--max-nodes-per-entry` | | int | 0 | Maximum node names per taint/label entry in topology collection (0 = unlimited) |

**Output Destinations:**
- **stdout**: Default when no `-o` flag specified
- **File**: Local file path (`/path/to/snapshot.yaml`)
- **ConfigMap**: Kubernetes ConfigMap URI (`cm://namespace/configmap-name`)

**What it captures:**
- **SystemD Services**: containerd, docker, kubelet configurations
- **OS Configuration**: grub, kmod, sysctl, release info
- **Kubernetes**: server version, images, ClusterPolicy
- **GPU**: driver version, CUDA, MIG settings, hardware info
- **NodeTopology**: node topology (cluster-wide taints and labels across all nodes)

**Examples:**

```shell
# Output to stdout (YAML)
aicr snapshot

# Save to file (JSON)
aicr snapshot --output system.json --format json

# Save to Kubernetes ConfigMap (requires cluster access)
aicr snapshot --output cm://gpu-operator/aicr-snapshot

# Debug mode
aicr --debug snapshot

# Table format (human-readable)
aicr snapshot --format table

# With custom kubeconfig
aicr snapshot --kubeconfig ~/.kube/prod-cluster

# Targeting specific nodes
aicr snapshot \
  --namespace gpu-operator \
  --node-selector accelerator=nvidia-h100 \
  --node-selector zone=us-west1-a

# With tolerations for tainted nodes
# (By default all taints are tolerated - only needed to restrict tolerations)
aicr snapshot \
  --toleration nvidia.com/gpu=present:NoSchedule

# Full example with all options
aicr snapshot \
  --kubeconfig ~/.kube/config \
  --namespace gpu-operator \
  --image ghcr.io/nvidia/aicr:v0.8.0 \
  --job-name snapshot-gpu-nodes \
  --service-account-name aicr \
  --node-selector accelerator=nvidia-h100 \
  --toleration nvidia.com/gpu:NoSchedule \
  --timeout 10m \
  --output cm://gpu-operator/aicr-snapshot \
  --no-cleanup

# Custom template formatting
aicr snapshot --template examples/templates/snapshot-template.md.tmpl

# Template with file output
aicr snapshot --template examples/templates/snapshot-template.md.tmpl --output report.md

# With custom template
aicr snapshot \
  --namespace gpu-operator \
  --template examples/templates/snapshot-template.md.tmpl \
  --output cluster-report.yaml
```

**Custom Templates:**

The `--template` flag enables custom output formatting using Go templates with [Sprig functions](https://masterminds.github.io/sprig/). Templates receive the full Snapshot struct:

```yaml
# Available template data structure:
.Kind           # Resource kind ("Snapshot")
.APIVersion     # API version string
.Metadata       # Map of key-value pairs (timestamp, version, source-node)
.Measurements   # Array of Measurement objects
  .Type         # Measurement type (K8s, GPU, OS, SystemD, NodeTopology)
  .Subtypes     # Array of Subtype objects
    .Name       # Subtype name (e.g., "server", "smi", "grub")
    .Data       # Map of readings (key -> Reading with .String method)

# NodeTopology measurement type has subtypes: summary, taint, label
# Taint encoding: effect|value|node1,node2,...  (parseable with Sprig splitList "|")
# Label encoding: value|node1,node2,...
```

Example template extracting key cluster info:
```go
cluster:
  kubernetes: {{ with index .Measurements 0 }}{{ range .Subtypes }}{{ if eq .Name "server" }}
    version: {{ (index .Data "version").String }}{{ end }}{{ end }}{{ end }}
  gpu: {{ range .Measurements }}{{ if eq .Type.String "GPU" }}{{ range .Subtypes }}{{ if eq .Name "smi" }}
    model: {{ (index .Data "gpu.model").String }}
    count: {{ (index .Data "gpu-count").String }}{{ end }}{{ end }}{{ end }}{{ end }}
```

See `examples/templates/snapshot-template.md.tmpl` for a complete example template that generates a concise cluster report.

**Agent Deployment Mode:**

When running against a cluster, AICR deploys a Kubernetes Job to capture the snapshot:

1. **Deploys RBAC**: ServiceAccount, Role, RoleBinding, ClusterRole, ClusterRoleBinding
2. **Creates Job**: Runs `aicr snapshot` as a container on the target node
3. **Waits for completion**: Monitors Job status with configurable timeout
4. **Retrieves snapshot**: Reads snapshot from ConfigMap after Job completes
5. **Writes output**: Saves snapshot to specified output destination
6. **Cleanup**: Deletes Job and RBAC resources (use `--no-cleanup` to keep for debugging)

**Benefits of agent deployment:**
- Capture configuration from actual cluster nodes (not local machine)
- No need to run kubectl manually
- Programmatic deployment for automation/CI/CD
- Reusable RBAC resources across multiple runs

**Agent deployment requirements:**
- Kubernetes cluster access (via kubeconfig)
- Cluster admin permissions (for RBAC creation)
- GPU nodes with nvidia-smi (for GPU metrics)

```

**ConfigMap Output:**

When using ConfigMap URIs (`cm://namespace/name`), the snapshot is stored directly in Kubernetes:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: aicr-snapshot
  namespace: gpu-operator
  labels:
    app.kubernetes.io/name: aicr
    app.kubernetes.io/component: snapshot
    app.kubernetes.io/version: v0.17.0
data:
  snapshot.yaml: |
    # Full snapshot content
  format: yaml
  timestamp: "2025-12-31T10:30:00Z"
```

**Snapshot Structure:**
```yaml
apiVersion: aicr.nvidia.com/v1alpha1
kind: Snapshot
metadata:
  created: "2025-12-31T10:30:00Z"
  hostname: gpu-node-1
measurements:
  - type: SystemD
    subtypes: [...]
  - type: OS
    subtypes: [...]
  - type: K8s
    subtypes: [...]
  - type: GPU
    subtypes: [...]
```

---

### aicr recipe

Generate optimized configuration recipes from query parameters or captured snapshots.

**Synopsis:**
```shell
aicr recipe [flags]
```

**Modes:**

#### Criteria File Mode (Recommended)
Generate recipes using a Kubernetes-style criteria file:

**Flags:**
| Flag | Short | Type | Description |
|------|-------|------|-------------|
| `--criteria` | `-c` | string | Path to criteria file (YAML/JSON), alternative to individual flags |
| `--output` | `-o` | string | Output file (default: stdout) |
| `--format` | `-f` | string | Format: json, yaml (default: yaml) |
| `--data` | | string | External data directory to overlay on embedded data (see [External Data](#external-data-directory)) |

The criteria file uses a Kubernetes-style format:
```yaml
kind: RecipeCriteria
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: gb200-eks-ubuntu-training
spec:
  service: eks
  os: ubuntu
  accelerator: gb200
  intent: training
  nodes: 8
```

Individual CLI flags can override criteria file values:
```shell
# Load criteria from file
aicr recipe --criteria criteria.yaml

# Override service from file
aicr recipe --criteria criteria.yaml --service gke

# Save output to file
aicr recipe -c criteria.yaml -o recipe.yaml
```

#### Query Mode
Generate recipes using direct system parameters:

**Flags:**
| Flag | Short | Type | Description |
|------|-------|------|-------------|
| `--service` | | string | K8s service: eks, gke, aks, oke |
| `--accelerator` | `--gpu` | string | Accelerator/GPU type: h100, gb200, a100, l40 |
| `--intent` | | string | Workload intent: training, inference |
| `--os` | | string | OS family: ubuntu, rhel, cos, amazonlinux |
| `--platform` | | string | Platform/framework type: kubeflow |
| `--nodes` | | int | Number of GPU nodes in the cluster |
| `--output` | `-o` | string | Output file (default: stdout) |
| `--format` | `-f` | string | Format: json, yaml (default: yaml) |
| `--data` | | string | External data directory to overlay on embedded data (see [External Data](#external-data-directory)) |

**Examples:**
```shell
# Basic recipe for Ubuntu on EKS with H100
aicr recipe --os ubuntu --service eks --accelerator h100

# Training workload with multiple GPU nodes
aicr recipe \
  --service eks \
  --accelerator gb200 \
  --intent training \
  --os ubuntu \
  --nodes 8 \
  --format yaml

# Kubeflow training workload
aicr recipe \
  --service eks \
  --accelerator h100 \
  --intent training \
  --os ubuntu \
  --platform kubeflow

# Save to file (--gpu is an alias for --accelerator)
aicr recipe --os ubuntu --gpu h100 --output recipe.yaml
```

#### Snapshot Mode
Generate recipes from captured snapshots:

**Flags:**
| Flag | Short | Type | Description |
|------|-------|------|-------------|
| `--snapshot` | `-s` | string | Path/URI to snapshot (file path, URL, or cm://namespace/name) |
| `--intent` | `-i` | string | Workload intent: training, inference |
| `--output` | `-o` | string | Output destination (file, ConfigMap URI, or stdout) |
| `--format` | | string | Format: json, yaml (default: yaml) |
| `--kubeconfig` | `-k` | string | Path to kubeconfig file (for ConfigMap URIs, overrides KUBECONFIG env) |

**Snapshot Sources:**
- **File**: Local file path (`./snapshot.yaml`)
- **URL**: HTTP/HTTPS URL (`https://example.com/snapshot.yaml`)
- **ConfigMap**: Kubernetes ConfigMap URI (`cm://namespace/configmap-name`)

**Examples:**
```shell
# Generate recipe from local snapshot file
aicr recipe --snapshot system.yaml --intent training

# From ConfigMap (requires cluster access)
aicr recipe --snapshot cm://gpu-operator/aicr-snapshot --intent training

# From ConfigMap with custom kubeconfig
aicr recipe \
  --snapshot cm://gpu-operator/aicr-snapshot \
  --kubeconfig ~/.kube/prod-cluster \
  --intent training

# Output to ConfigMap
aicr recipe -s system.yaml -o cm://gpu-operator/aicr-recipe

# Chain snapshot → recipe with ConfigMaps
aicr snapshot -o cm://default/snapshot
aicr recipe -s cm://default/snapshot -o cm://default/recipe

# With custom output
aicr recipe -s system.yaml -i inference -o recipe.yaml --format yaml
```

**Output structure:**
```yaml
apiVersion: aicr.nvidia.com/v1alpha1
kind: Recipe
metadata:
  version: v1.0.0
  created: "2025-12-31T10:30:00Z"
  appliedOverlays:
    - base
    - eks
    - eks-training
    - gb200-eks-training
criteria:
  service: eks
  accelerator: gb200
  intent: training
  os: any
componentRefs:
  - name: gpu-operator
    version: v25.3.3
    order: 1
    repository: https://helm.ngc.nvidia.com/nvidia
constraints:
  driver:
    version: "580.82.07"
    cudaVersion: "13.1"
```

---

### aicr query

Query a specific value from the fully hydrated recipe configuration. Resolves a recipe
from criteria (same as `aicr recipe`), merges all base, overlay, and inline value
overrides, then extracts the value at the given dot-path selector.

**Synopsis:**
```shell
aicr query --selector <path> [flags]
```

**Flags:**

All `aicr recipe` flags are supported, plus:

| Flag | Type | Description |
|------|------|-------------|
| `--selector` | string | **Required.** Dot-path to the configuration value to extract |

**Selector Syntax:**

Uses dot-delimited paths consistent with Helm `--set` and `yq`:

| Selector | Returns |
|----------|---------|
| `components.<name>.values.<path>` | Hydrated Helm value (scalar or subtree) |
| `components.<name>.chart` | Component metadata field |
| `components.<name>` | Entire hydrated component block |
| `criteria.<field>` | Recipe criteria field |
| `deploymentOrder` | Component deployment order list |
| `constraints` | Merged constraint list |
| `.` or empty | Entire hydrated recipe |

Leading dots are optional (yq-style): `.components.gpu-operator.chart` and
`components.gpu-operator.chart` are equivalent.

**Output:**

- **Scalar values** (string, number, bool) are printed as plain text — no YAML wrapper
- **Complex values** (maps, lists) are printed as YAML (default) or JSON (`--format json`)

**Examples:**
```shell
# Get a specific Helm value
aicr query --service eks --accelerator h100 --intent training \
  --selector components.gpu-operator.values.driver.version
# stdout: 570.86.16

# Get a value subtree
aicr query --service eks --accelerator h100 --intent training \
  --selector components.gpu-operator.values.driver
# stdout:
#   version: "570.86.16"
#   repository: nvcr.io/nvidia

# Get the full hydrated component
aicr query --service eks --accelerator h100 --intent training \
  --selector components.gpu-operator

# Get deployment order
aicr query --service eks --accelerator h100 --intent training \
  --selector deploymentOrder

# Use in shell scripts
VERSION=$(aicr query --service eks --accelerator h100 --intent training \
  --selector components.gpu-operator.values.driver.version)
echo "Driver version: $VERSION"

# JSON output for complex values
aicr query --service eks --accelerator h100 --intent training \
  --selector components.gpu-operator.values --format json

# Query from snapshot
aicr query --snapshot snapshot.yaml \
  --selector components.gpu-operator.values.driver.version

# Full hydrated recipe
aicr query --service eks --accelerator h100 --intent training --selector .
```

**Advanced Examples:**

```shell
# Cross-cloud comparison: how Prometheus storage differs between EKS and GKE
# EKS provisions a 50Gi persistent EBS volume (gp2)
aicr query --service eks --intent training \
  --selector components.kube-prometheus-stack.values.prometheus.prometheusSpec.storageSpec
# GKE uses a 10Gi ephemeral emptyDir (GMP handles long-term retention)
aicr query --service gke --intent training \
  --selector components.kube-prometheus-stack.values.prometheus.prometheusSpec.storageSpec

# Compare deployment order across clouds
# EKS deploys 12 components (includes aws-ebs-csi-driver, aws-efa, skyhook-customizations)
aicr query --service eks --accelerator h100 --intent training --selector deploymentOrder
# GKE deploys 9 components (storage and networking are platform-managed)
aicr query --service gke --accelerator h100 --intent training --selector deploymentOrder

# Pin the exact driver version into Terraform/Pulumi variables
DRIVER_VERSION=$(aicr query --service eks --accelerator h100 --intent training \
  --selector components.gpu-operator.values.driver.version)
echo "gpu_driver_version = \"${DRIVER_VERSION}\""

# Compare skyhook tuning parameters across accelerators
# H100: real tuning packages (kernel setup, nvidia-tuned, full setup)
aicr query --service eks --accelerator h100 --intent training \
  --selector components.skyhook-customizations.values
# GB200: same value structure, but manifest renders a no-op (ARM64 packages pending)
aicr query --service eks --accelerator gb200 --intent training \
  --selector components.skyhook-customizations.values

# Watch constraints tighten as you add specificity
# Just "EKS" → 1 constraint (K8s >= 1.28)
aicr query --service eks --selector constraints
# Add GPU + intent + OS → 4 constraints (K8s >= 1.32.4, Ubuntu 24.04, kernel >= 6.8)
aicr query --service eks --accelerator h100 --intent training --os ubuntu \
  --selector constraints
```

---

### aicr validate

Validate a system snapshot against the constraints defined in a recipe to verify cluster compatibility. Supports multi-phase validation with different validation stages.

**Synopsis:**
```shell
aicr validate [flags]
```

**Flags:**
| Flag | Short | Type | Description |
|------|-------|------|-------------|
| `--recipe` | `-r` | string | Path/URI to recipe file containing constraints (required) |
| `--snapshot` | `-s` | string | Path/URI to snapshot file containing measurements (omit to capture live) |
| `--phase` | | string | Validation phase to run: deployment, performance, conformance, all (default: all) |
| `--fail-on-error` | | bool | Exit with non-zero status if any constraint fails (default: true) |
| `--output` | `-o` | string | Output destination (file or stdout, default: stdout) |
| `--kubeconfig` | `-k` | string | Path to kubeconfig file (for ConfigMap URIs) |

**Input Sources:**
- **File**: Local file path (`./recipe.yaml`, `./snapshot.yaml`)
- **URL**: HTTP/HTTPS URL (`https://example.com/recipe.yaml`)
- **ConfigMap**: Kubernetes ConfigMap URI (`cm://namespace/configmap-name`)

**Validation Phases:**

Validation can be run in different phases to validate different aspects of the deployment:

| Phase | Description | When to Run |
|-------|-------------|-------------|
| `deployment` | Validates component deployment health and expected resources | After deploying components |
| `performance` | Validates system performance and network fabric health | After components are running |
| `conformance` | Validates workload-specific requirements and conformance | Before running production workloads |
| `all` | Runs all phases sequentially with dependency logic | Complete end-to-end validation |

> **Note:** Readiness constraints (K8s version, OS, kernel) are always evaluated implicitly before any phase runs. If readiness fails, validation stops before deploying any Jobs.

**Phase Dependencies:**
- Phases run sequentially when using `--phase all`
- If a phase fails, subsequent phases are skipped
- Use individual phases for targeted validation during specific deployment stages

**Constraint Format:**

Constraints use fully qualified measurement paths: `{Type}.{Subtype}.{Key}`

| Constraint Path | Description |
|-----------------|-------------|
| `K8s.server.version` | Kubernetes server version |
| `OS.release.ID` | Operating system identifier (ubuntu, rhel) |
| `OS.release.VERSION_ID` | OS version (24.04, 22.04) |
| `OS.sysctl./proc/sys/kernel/osrelease` | Kernel version |
| `GPU.info.type` | GPU hardware type |

**Supported Operators:**
| Operator | Example | Description |
|----------|---------|-------------|
| `>=` | `>= 1.30` | Greater than or equal (version comparison) |
| `<=` | `<= 1.33` | Less than or equal (version comparison) |
| `>` | `> 1.30` | Greater than (version comparison) |
| `<` | `< 2.0` | Less than (version comparison) |
| `==` | `== ubuntu` | Explicit equality |
| `!=` | `!= rhel` | Not equal |
| (none) | `ubuntu` | Exact string match |

**Examples:**

```shell
# Validate snapshot against recipe (readiness constraints run implicitly)
aicr validate --recipe recipe.yaml --snapshot snapshot.yaml

# Validate specific phase
aicr validate \
  --recipe recipe.yaml \
  --snapshot snapshot.yaml \
  --phase deployment

# Run all validation phases
aicr validate \
  --recipe recipe.yaml \
  --snapshot snapshot.yaml \
  --phase all

# Load snapshot from ConfigMap
aicr validate \
  --recipe recipe.yaml \
  --snapshot cm://gpu-operator/aicr-snapshot

# Save results to file
aicr validate \
  --recipe recipe.yaml \
  --snapshot cm://gpu-operator/aicr-snapshot \
  --output validation-results.json

# Validate deployment phase after components are installed
aicr validate \
  --recipe recipe.yaml \
  --snapshot snapshot.yaml \
  --phase deployment

# Run performance validation
aicr validate \
  --recipe recipe.yaml \
  --snapshot snapshot.yaml \
  --phase performance

# With custom kubeconfig
aicr validate \
  --recipe recipe.yaml \
  --snapshot cm://gpu-operator/aicr-snapshot \
  --kubeconfig ~/.kube/prod-cluster
```

**Output Structure ([CTRF](https://ctrf.io/) JSON):**

Results are output in CTRF (Common Test Report Format) — an industry-standard schema for test reporting.

```json
{
  "reportFormat": "CTRF",
  "specVersion": "0.0.1",
  "timestamp": "2026-03-10T20:10:44Z",
  "generatedBy": "aicr",
  "results": {
    "tool": {
      "name": "aicr",
      "version": "v0.10.3-next"
    },
    "summary": {
      "tests": 16,
      "passed": 13,
      "failed": 0,
      "skipped": 3,
      "pending": 0,
      "other": 0,
      "start": 1773173400872,
      "stop": 1773173799002
    },
    "tests": [
      {
        "name": "operator-health",
        "status": "passed",
        "duration": 0,
        "suite": ["deployment"],
        "stdout": ["Found 1 gpu-operator pod(s)", "Running: 1/1"]
      },
      {
        "name": "expected-resources",
        "status": "passed",
        "duration": 0,
        "suite": ["deployment"],
        "stdout": ["All expected resources are healthy"]
      },
      {
        "name": "nccl-all-reduce-bw",
        "status": "passed",
        "duration": 234000,
        "suite": ["performance"],
        "stdout": ["NCCL All Reduce bandwidth: 488.37 GB/s", "Constraint: >= 100 → true"]
      },
      {
        "name": "dra-support",
        "status": "passed",
        "duration": 8000,
        "suite": ["conformance"],
        "stdout": ["DRA GPU allocation successful"]
      },
      {
        "name": "cluster-autoscaling",
        "status": "skipped",
        "duration": 0,
        "suite": ["conformance"],
        "stdout": ["SKIP reason=\"Karpenter not found\""]
      }
    ]
  }
}
```

> **Note:** The `tests` array above is truncated for brevity. A full validation run produces one entry per check across all phases. Each entry includes `stdout` with detailed diagnostic output.

**Test Statuses:**
| Status | Description |
|--------|-------------|
| `passed` | Check or constraint passed |
| `failed` | Check or constraint failed |
| `skipped` | Check could not be evaluated (missing data, no-cluster mode) |
| `other` | Unexpected outcome (crash, OOM, timeout) |

**Exit Codes:**
| Code | Description |
|------|-------------|
| `0` | All checks passed |
| `2` | Invalid input (bad flags, missing recipe) |
| `8` | One or more checks failed (when `--fail-on-error` is set) |

---

### aicr bundle

Generate deployment-ready bundles from recipes containing Helm values, manifests, scripts, and documentation.

**Synopsis:**
```shell
aicr bundle [flags]
```

**Flags:**
| Flag | Short | Type | Description |
|---------------------------------|-------|------|-------------|
| `--recipe` | `-r` | string | Path to recipe file (required) |
| `--output` | `-o` | string | Output directory (default: current dir) |
| `--deployer` | | string | Deployment method: helm (default), argocd |
| `--repo` | | string | Git repository URL for ArgoCD applications (only used with `--deployer argocd`) |
| `--set` | | string[] | Override values in bundle files (repeatable). Use `enabled` key to include/exclude components (e.g., `--set awsebscsidriver:enabled=false`) |
| `--data` | | string | External data directory to overlay on embedded data (see [External Data](#external-data-directory)) |
| `--system-node-selector` | | string[] | Node selector for system components (format: key=value, repeatable) |
| `--system-node-toleration` | | string[] | Toleration for system components (format: key=value:effect, repeatable) |
| `--accelerated-node-selector` | | string[] | Node selector for accelerated/GPU nodes (format: key=value, repeatable) |
| `--accelerated-node-toleration` | | string[] | Toleration for accelerated/GPU nodes (format: key=value:effect, repeatable) |
| `--workload-gate` | | string | Taint for skyhook-operator runtime required (format: key=value:effect or key:effect). This is a day 2 option for cluster scaling operations. |
| `--workload-selector` | | string[] | Label selector for skyhook-customizations to prevent eviction of running training jobs (format: key=value, repeatable). Required when skyhook-customizations is enabled with training intent. |
| `--nodes` | | int | Estimated number of GPU nodes (default: 0 = unset). At bundle time, written to Helm value paths declared in the registry under `nodeScheduling.nodeCountPaths`. |
| `--attest` | | bool | Enable bundle attestation and binary provenance verification. Requires OIDC authentication. See [Bundle Attestation](#bundle-attestation). |
| `--certificate-identity-regexp` | | string | Override the certificate identity pattern for binary attestation verification. Must contain `"NVIDIA/aicr"`. For testing only. |

**Node Scheduling:**

The `--accelerated-node-selector` and `--accelerated-node-toleration` flags control scheduling for GPU-specific components:

| Flag | GPU Daemonsets | NFD Workers |
|------|---------------|-------------|
| `--accelerated-node-selector` | Applied (restricts to GPU nodes) | **Not applied** (NFD runs on all nodes) |
| `--accelerated-node-toleration` | Applied | Applied |
| `--system-node-selector` | Not applied | Not applied |
| `--system-node-toleration` | Not applied | Not applied |

NFD (Node Feature Discovery) workers must run on **all nodes** (GPU, CPU, and system) to detect hardware features. This matches the gpu-operator default behavior where NFD workers also run on control-plane nodes. The `--accelerated-node-selector` is intentionally not applied to NFD workers so they are not restricted to GPU nodes.

> **Note:** When no `--accelerated-node-toleration` is specified, a default toleration (`operator: Exists`) is applied to both GPU daemonsets and NFD workers, allowing them to run on nodes with any taint.

**Example:**

```bash
aicr bundle --recipe recipe.yaml \
  --accelerated-node-selector nodeGroup=gpu-worker \
  --accelerated-node-toleration dedicated=worker-workload:NoSchedule \
  --accelerated-node-toleration dedicated=worker-workload:NoExecute \
  --system-node-selector nodeGroup=system-worker \
  --system-node-toleration dedicated=system-workload:NoSchedule \
  --system-node-toleration dedicated=system-workload:NoExecute \
  --output bundle
```

> **Cluster node requirements:** This example assumes the cluster has nodes labeled `nodeGroup=system-worker` with taints `dedicated=system-workload:NoSchedule,NoExecute` for system infrastructure, and GPU nodes labeled `nodeGroup=gpu-worker` with taints `dedicated=worker-workload:NoSchedule,NoExecute`.

This results in:
- **GPU daemonsets** (driver, device-plugin, toolkit, dcgm): `nodeSelector=nodeGroup=gpu-worker` + tolerations for `dedicated=worker-workload` with both `NoSchedule` and `NoExecute`
- **NFD workers**: no nodeSelector (runs on all nodes) + tolerations for `dedicated=worker-workload` with both `NoSchedule` and `NoExecute`
- **System components** (gpu-operator controller, NFD gc/master, dynamo grove, kgateway proxy): `nodeSelector=nodeGroup=system-worker` + tolerations for `dedicated=system-workload` with both `NoSchedule` and `NoExecute`

**Behavior:**
- All components from the recipe are bundled automatically
- Each component creates a subdirectory in the output directory
- Components are deployed in the order specified by `deploymentOrder` in the recipe

**Deployment Methods (`--deployer`):**

The `--deployer` flag controls how deployment artifacts are generated:

| Method | Description |
|--------|-------------|
| `helm` | (Default) Generates Helm charts with values for deployment |
| `argocd` | Generates ArgoCD Application manifests for GitOps deployment |

**Deployment Order:**

All deployers respect the `deploymentOrder` field from the recipe, ensuring components are installed in the correct sequence:

- **Helm**: Components listed in README in deployment order
- **ArgoCD**: Uses `argocd.argoproj.io/sync-wave` annotation (0 = first, 1 = second, etc.)

**Value Overrides (`--set`):**

Override any value in the generated bundle files using dot notation:

```shell
--set bundler:path.to.field=value
```

**Format:** `bundler:path=value` where:
- `bundler` - Bundler name (e.g., `gpuoperator`, `networkoperator`, `certmanager`, `skyhook-operator`, `nvsentinel`)
- `path` - Dot-separated path to the field
- `value` - New value to set

**Behavior:**
- **Duplicate keys**: When the same `bundler:path` is specified multiple times, the **last value wins**
- **Array values**: Individual array elements cannot be overridden (no `[0]` index syntax). Arrays can only be replaced entirely via recipe overrides, not via `--set` flags. Use recipe-level overrides in `componentRefs[].overrides` if you need to replace an entire array.
- **Type conversion**: String values are automatically converted to appropriate types (`true`/`false` → bool, numeric strings → numbers)
- **Component enable/disable**: The special `enabled` key controls whether a component is included in the bundle. `--set <component>:enabled=false` excludes the component; `--set <component>:enabled=true` re-enables a recipe-disabled component. The `enabled` key is consumed by the bundler and not passed to Helm chart values.

**Examples:**
```shell
# Generate all bundles
aicr bundle --recipe recipe.yaml --output ./bundles

# Override values in GPU Operator bundle
aicr bundle -r recipe.yaml \
  --set gpuoperator:gds.enabled=true \
  --set gpuoperator:driver.version=570.86.16 \
  -o ./bundles

# Override multiple components
aicr bundle -r recipe.yaml \
  --set gpuoperator:mig.strategy=mixed \
  --set networkoperator:rdma.enabled=true \
  --set networkoperator:sriov.enabled=true \
  -o ./bundles

# Override cert-manager resources
aicr bundle -r recipe.yaml \
  --set certmanager:controller.resources.memory.limit=512Mi \
  --set certmanager:webhook.resources.cpu.limit=200m \
  -o ./bundles

# Override Skyhook manager resources
aicr bundle -r recipe.yaml \
  --set skyhook-operator:manager.resources.cpu.limit=500m \
  --set skyhook-operator:manager.resources.memory.limit=256Mi \
  -o ./bundles

# Disable a component at bundle time (e.g., EBS CSI already installed as EKS addon)
aicr bundle -r recipe.yaml \
  --set awsebscsidriver:enabled=false \
  -o ./bundles

# Schedule system components on specific node pool
aicr bundle -r recipe.yaml \
  --system-node-selector nodeGroup=system-pool \
  --system-node-toleration dedicated=system:NoSchedule \
  -o ./bundles

# Schedule GPU workloads on labeled GPU nodes
aicr bundle -r recipe.yaml \
  --accelerated-node-selector nvidia.com/gpu.present=true \
  --accelerated-node-toleration nvidia.com/gpu=present:NoSchedule \
  -o ./bundles

# Combined: separate system and GPU scheduling
aicr bundle -r recipe.yaml \
  --system-node-selector nodeGroup=system-pool \
  --system-node-toleration dedicated=system:NoSchedule \
  --accelerated-node-selector accelerator=nvidia-h100 \
  --accelerated-node-toleration nvidia.com/gpu=present:NoSchedule \
  -o ./bundles

# Set estimated GPU node count (writes to nodeCountPaths in registry)
aicr bundle -r recipe.yaml --nodes 8 -o ./bundles

# Day 2 options: workload-gate and workload-selector for skyhook
aicr bundle -r recipe.yaml \
  --workload-gate skyhook.io/runtime-required=true:NoSchedule \
  --workload-selector workload-type=training \
  -o ./bundles

# Generate an attested bundle (opens browser for OIDC auth)
aicr bundle -r recipe.yaml --attest -o ./bundles

# In GitHub Actions (OIDC token detected automatically)
aicr bundle -r recipe.yaml --attest -o ./bundles

# Generate ArgoCD Application manifests for GitOps
aicr bundle -r recipe.yaml --deployer argocd -o ./bundles

# ArgoCD with Git repository URL (avoids placeholder in app-of-apps.yaml)
aicr bundle -r recipe.yaml --deployer argocd \
  --repo https://github.com/my-org/my-gitops-repo.git \
  -o ./bundles

# Combine deployer with value overrides
aicr bundle -r recipe.yaml \
  --deployer argocd \
  -o ./bundles
```

**Bundle structure** (with default Helm deployer):
```
bundles/
├── README.md                      # Deployment guide with ordered steps
├── deploy.sh                      # One-command deployment script
├── recipe.yaml                    # Recipe used to generate bundle
├── checksums.txt                  # SHA256 checksums
├── attestation/                   # Present when --attest is used
│   ├── bundle-attestation.sigstore.json   # SLSA Build Provenance v1
│   └── aicr-attestation.sigstore.json     # Binary SLSA provenance chain
├── gpu-operator/
│   ├── values.yaml                # Component-specific Helm values
│   ├── README.md                  # Per-component install/upgrade/uninstall
│   └── manifests/                 # Additional manifests (if any)
│       └── dcgm-exporter.yaml
└── cert-manager/
    ├── values.yaml
    └── README.md
```

**ArgoCD bundle structure** (with `--deployer argocd`):
```
bundles/
├── app-of-apps.yaml               # Parent Application (bundle root)
├── recipe.yaml                    # Recipe used to generate bundle
├── gpu-operator/
│   ├── values.yaml                # Helm values for GPU Operator
│   ├── manifests/                 # Additional manifests (ClusterPolicy, etc.)
│   └── argocd/
│       └── application.yaml       # ArgoCD Application (sync-wave: 0)
├── network-operator/
│   ├── values.yaml                # Helm values for Network Operator
│   └── argocd/
│       └── application.yaml       # ArgoCD Application (sync-wave: 1)
└── README.md                      # ArgoCD deployment guide
```

**Day 2 Options:**

The `--workload-gate` and `--workload-selector` flags are day 2 operational options for cluster scaling operations:

- **`--workload-gate`**: Specifies a taint for skyhook-operator's runtime required feature. This ensures nodes are properly configured before workloads can schedule on them during cluster scaling. The taint is configured in the skyhook-operator Helm values file at `controllerManager.manager.env.runtimeRequiredTaint`. For more information about runtime required, see the [skyhook documentation](https://github.com/NVIDIA/skyhook/blob/main/docs/runtime_required.md).

- **`--workload-selector`**: Specifies a label selector for skyhook-customizations to prevent skyhook from evicting running training jobs. This is critical for training workloads where job eviction would cause significant disruption. The selector is set in the Skyhook CR manifest (tuning.yaml) in the `spec.workloadSelector.matchLabels` field.

**Estimated node count (`--nodes`):**

The `--nodes` flag is a **bundle-time** option: it is applied when you run `aicr bundle`, not when you run `aicr recipe`. The value is written to each component's Helm values at the paths declared in the registry under `nodeScheduling.nodeCountPaths`.

- **When to use**: Pass the expected or typical number of GPU nodes (e.g. size of your node pool). Use `0` (default) to leave the value unset.
- **Where it goes**: Components that define `nodeCountPaths` in the registry receive the value at those paths in their generated `values.yaml`.
- **Example**: `aicr bundle -r recipe.yaml --nodes 8 -o ./bundles` writes `8` to every path listed in each component's `nodeScheduling.nodeCountPaths`.

**Component Validation System:**

AICR includes a component-driven validation system that automatically checks bundle configuration and displays warnings or errors during bundle generation. Validations are defined in the component registry and run automatically when components are included in a recipe.

**How Validations Work:**

1. **Automatic Execution**: When generating a bundle, validations are automatically executed for each component in the recipe
2. **Condition-Based**: Validations can be configured to run only when specific conditions are met (e.g., intent, service, accelerator)
3. **Severity Levels**: Each validation can be configured as a "warning" (non-blocking) or "error" (blocking)
4. **Custom Messages**: Each validation can include an optional detail message that provides actionable guidance

**Validation Warnings:**

When generating bundles with skyhook-customizations enabled, validation warnings are displayed for missing configuration:

1. **Workload Selector Warning**: When skyhook-customizations is enabled with training intent, if `--workload-selector` is not set, a warning will be displayed:

```
Warning: skyhook-customizations is enabled but --workload-selector is not set. 
This may cause skyhook to evict running training jobs. Consider setting --workload-selector to prevent eviction.
```

2. **Accelerated Selector Warning**: When skyhook-customizations is enabled with training or inference intent, if `--accelerated-node-selector` is not set, a warning will be displayed:

```
Warning: skyhook-customizations is enabled but --accelerated-node-selector is not set. 
Without this selector, the customization will run on all nodes. Consider setting --accelerated-node-selector to target specific nodes.
```

**Viewing Validation Warnings:**

Validation warnings are displayed in the bundle output after successful generation:

```shell
Note:
  ⚠ Warning: skyhook-customizations is enabled but --workload-selector is not set. This may cause skyhook to evict running training jobs. Consider setting --workload-selector to prevent eviction.
  ⚠ Warning: skyhook-customizations is enabled but --accelerated-node-selector is not set. Without this selector, the customization will run on all nodes. Consider setting --accelerated-node-selector to target specific nodes.
```

**Resolving Validation Warnings:**

To resolve the warnings, include the appropriate flags when generating the bundle:

```shell
# Resolve workload selector warning
aicr bundle -r recipe.yaml \
  --workload-selector workload-type=training \
  -o ./bundle

# Resolve accelerated selector warning
aicr bundle -r recipe.yaml \
  --accelerated-node-selector nodeGroup=gpu-worker \
  -o ./bundle

# Resolve both warnings
aicr bundle -r recipe.yaml \
  --workload-selector workload-type=training \
  --accelerated-node-selector nodeGroup=gpu-worker \
  -o ./bundle
```

**Examples:**
```shell
# Generate bundle with day 2 options for training workloads
aicr bundle -r recipe.yaml \
  --workload-gate skyhook.io/runtime-required=true:NoSchedule \
  --workload-selector workload-type=training \
  --workload-selector intent=training \
  --accelerated-node-selector accelerator=nvidia-h100 \
  -o ./bundles

# Generate bundle for inference workloads with accelerated selector
aicr bundle -r recipe.yaml \
  --accelerated-node-selector accelerator=nvidia-h100 \
  -o ./bundles
```

ArgoCD Applications use multi-source to:
1. Pull Helm charts from upstream repositories
2. Apply values.yaml from your GitOps repository
3. Deploy additional manifests from component's manifests/ directory (if present)

#### Bundle Attestation

> **Prerequisite:** The `--attest` flag requires a binary installed using the install script, which includes a cryptographic attestation from NVIDIA. Binaries installed via `go install` or manual download do not include this file and cannot use `--attest`.

When `--attest` is passed, the bundle command performs four steps:

1. **Verifies the binary attestation file exists** — The running `aicr` binary must have a valid SLSA provenance file (`aicr-attestation.sigstore.json`) alongside it, included by the install script from a release archive. If missing, the command fails immediately with guidance on how to install correctly.
2. **Acquires an OIDC token** — In GitHub Actions the ambient OIDC token is used automatically. Locally, a browser window opens for Sigstore OIDC authentication.
3. **Verifies the binary's own attestation** — Cryptographically verifies the SLSA provenance binds to the running binary and was signed by NVIDIA CI. This ensures only NVIDIA-built binaries can produce attested bundles.
4. **Signs the bundle** — Creates a SLSA Build Provenance v1 in-toto statement binding the creator's identity to the bundle content (via `checksums.txt` digest) and the binary that produced it.
5. **Writes attestation files** — `attestation/bundle-attestation.sigstore.json` and `attestation/aicr-attestation.sigstore.json` are added to the bundle output.

Attestation is opt-in; bundles are unsigned by default. Signing uses Sigstore keyless signing (Fulcio CA + Rekor transparency log). For verification, see [`aicr verify`](#aicr-verify).

**Deploying a bundle:**
```shell
# Navigate to bundle
cd bundles/gpu-operator

# Review configuration
cat values.yaml
cat README.md

# Verify integrity
sha256sum -c checksums.txt

# Deploy to cluster
chmod +x deploy.sh && ./deploy.sh
```

> **Note:** `deploy.sh` and `undeploy.sh` are convenience scripts — not the only deployment path. Each component subdirectory contains a `README.md` with the exact `helm upgrade --install` command for manual or pipeline-driven deployment.

#### Deploy Script Behavior (`deploy.sh`)

The deploy script installs components in the order specified by `deploymentOrder` in the recipe.

**Flags:**

| Flag | Description |
|------|-------------|
| `--no-wait` | Skip `helm --wait` for each component (faster, no readiness check) |
| `--best-effort` | Continue past individual component failures instead of exiting |
| `--retries N` | Retry failed helm/kubectl operations N times with exponential backoff (default: 5) |

Unknown flags are rejected with an error to catch typos (e.g., `--best-effrot`).

**Retry behavior:**

The deploy script retries failed `helm upgrade --install` and `kubectl apply` operations with exponential backoff. By default, each operation is retried up to 5 times (6 total attempts). The backoff delay increases quadratically: 5s, 20s, 45s, 80s, 120s (capped) between retries.

Use `--retries 0` to disable retries (fail-fast behavior). When `--best-effort` is also set, retries are exhausted first before falling through to best-effort handling.

**Pre-install manifests and CRD ordering:**

Some components have pre-install manifests (CRDs, namespaces, ConfigMaps) that must exist before `helm install`. The script applies these with `kubectl apply` before the Helm install. On first deploy, CRD-dependent resources may produce `no matches for kind` warnings because the CRD hasn't been registered yet — these warnings are suppressed. All other `kubectl apply` errors (auth failures, webhook denials, bad manifests) fail the script immediately.

After `helm install`, the same manifests are re-applied as post-install to ensure CRD-dependent resources are created.

**Async components:**

Components that use operator patterns with custom resources that reconcile asynchronously (e.g., `kai-scheduler`) are installed without `--wait` to avoid Helm timing out on CR readiness.

**DRA kubelet plugin registration:**

After installing `nvidia-dra-driver-gpu`, the script automatically restarts the DRA kubelet plugin daemonset. This is a best-effort mitigation for a known issue: after uninstall/reinstall, the kubelet's plugin watcher (`fsnotify`) may not detect new registration sockets, causing `DRA driver gpu.nvidia.com is not registered` errors.

If DRA pods fail with this error after redeployment, the daemonset restart alone may not be sufficient — a **node reboot** is required to reset the kubelet's plugin registration state. To reboot GPU nodes:

```bash
# Cordon, drain, and reboot the affected node
kubectl cordon <node-name>
kubectl drain <node-name> --ignore-daemonsets --delete-emptydir-data
# Reboot via cloud provider (e.g., AWS EC2 console or CLI)
aws ec2 reboot-instances --instance-ids <instance-id>
# Uncordon after node returns
kubectl uncordon <node-name>
```

#### Undeploy Script Behavior (`undeploy.sh`)

The undeploy script removes components in reverse deployment order.

**Flags:**

| Flag | Description |
|------|-------------|
| `--keep-namespaces` | Skip namespace deletion after component removal |
| `--delete-pvcs` | Delete all PVCs in component namespaces (default: **off**) |
| `--timeout SECONDS` | Helm uninstall timeout per component (default: 120) |

**PVC preservation (default):**

PVCs are **not deleted** by default. This preserves historical data (Prometheus metrics, Alertmanager state, etcd data) across redeploys. If an EBS-backed PV has an AZ mismatch after redeployment, the PVC will stay Pending with a clear error — the operator can then decide to delete it manually.

Pass `--delete-pvcs` to delete all PVCs. Protected namespaces (`kube-system`, `kube-public`, `kube-node-lease`, `default`) are always excluded from PVC deletion to prevent accidental removal of non-bundle PVCs.

**Shared namespace ordering:**

When multiple components share a namespace (e.g., `monitoring` contains `kube-prometheus-stack`, `prometheus-adapter`, and `k8s-ephemeral-storage-metrics`), all components are uninstalled first, then PVC and namespace cleanup runs once. This prevents hangs caused by `kubernetes.io/pvc-protection` finalizers — if a StatefulSet owner is still running when PVC deletion is attempted, the delete blocks indefinitely.

**Stuck release handling:**

If a Helm release is in a `pending-install` or `pending-upgrade` state (from an interrupted deploy), the script retries with `--no-hooks` to force removal.

**Orphaned webhook cleanup:**

After uninstalling each component, the script checks for orphaned validating/mutating webhooks whose backing service no longer exists. Fail-closed webhooks with missing services block all pod creation, so these are deleted proactively.

---

### aicr verify

Verify the integrity and attestation chain of a bundle. Verification is fully offline — no network calls are made.

**Synopsis:**
```shell
aicr verify <bundle-dir> [flags]
```

**Flags:**
| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--min-trust-level` | string | `max` | Minimum required trust level. `max` auto-detects the highest achievable level and verifies against it. Explicit levels: `verified`, `attested`, `unverified`, `unknown`. |
| `--require-creator` | string | | Require a specific creator identity, matched against the bundle attestation signing certificate. |
| `--cli-version-constraint` | string | | Version constraint for the aicr CLI version in the attestation predicate. Supports `>=`, `>`, `<=`, `<`, `==`, `!=`. A bare version (e.g. `"0.8.0"`) defaults to `>=`. |
| `--certificate-identity-regexp` | string | | Override the certificate identity pattern for binary attestation verification. Must contain `"NVIDIA/aicr"`. For testing only. |
| `--format` | string | `text` | Output format: `text` or `json`. |

**Trust Levels:**

| Level | Name | Criteria |
|-------|------|----------|
| 4 | `verified` | Full chain: checksums + bundle attestation + binary attestation pinned to NVIDIA CI |
| 3 | `attested` | Chain verified but binary attestation missing or external data (`--data`) was used |
| 2 | `unverified` | Checksums valid, `--attest` was not used when creating the bundle |
| 1 | `unknown` | Missing or invalid checksums |

**Verification steps:**

1. **Checksums** — verifies all content files match `checksums.txt`
2. **Bundle attestation** — cryptographic signature verified against Sigstore trusted root
3. **Binary attestation** — provenance chain verified with identity pinned to NVIDIA CI (`on-tag.yaml` workflow)

**Examples:**
```shell
# Auto-detect maximum trust level
aicr verify ./my-bundle

# Enforce a minimum trust level
aicr verify ./my-bundle --min-trust-level verified

# Require a specific bundle creator
aicr verify ./my-bundle --require-creator jdoe@company.com

# Require minimum CLI version used to create the bundle
aicr verify ./my-bundle --cli-version-constraint ">= 0.8.0"

# JSON output for CI pipelines
aicr verify ./my-bundle --format json
```

> **Stale root:** If verification fails with certificate chain errors, run `aicr trust update` to refresh the Sigstore trusted root.

---

### aicr trust update

Fetch the latest Sigstore trusted root from the TUF CDN and update the local cache at `~/.sigstore/root/`. This is needed when Sigstore rotates signing keys (a few times per year).

**Synopsis:**
```shell
aicr trust update
```

**No flags.** This command contacts `tuf-repo-cdn.sigstore.dev`, verifies the update chain against the embedded TUF root, and writes the result to `~/.sigstore/root/`.

**When to run:**
- After initial installation (the install script runs this automatically)
- When `aicr verify` reports a stale or expired trusted root
- When Sigstore announces key rotation

**Example:**
```shell
aicr trust update
```

---

## Complete Workflow Examples

### File-Based Workflow

```shell
# Step 1: Capture system configuration
aicr snapshot --output snapshot.yaml

# Step 2: Generate optimized recipe for training workloads
aicr recipe \
  --snapshot snapshot.yaml \
  --intent training \
  --output recipe.yaml

# Step 3: Validate recipe constraints against snapshot
aicr validate \
  --recipe recipe.yaml \
  --snapshot snapshot.yaml

# Step 4: Create deployment bundle
aicr bundle \
  --recipe recipe.yaml \
  --output ./deployment

# Step 5: Deploy to cluster
cd deployment && chmod +x deploy.sh && ./deploy.sh

# Step 6: Verify deployment
kubectl get pods -n gpu-operator
kubectl logs -n gpu-operator -l app=nvidia-operator-validator
```

### ConfigMap-Based Workflow (Kubernetes-Native)

```shell
# Step 1: Agent captures snapshot to ConfigMap (using CLI deployment)
aicr snapshot --output cm://gpu-operator/aicr-snapshot

# The CLI handles agent deployment automatically
# No manual kubectl steps needed

# Step 2: Generate recipe from ConfigMap
aicr recipe \
  --snapshot cm://gpu-operator/aicr-snapshot \
  --intent training \
  --output recipe.yaml

# Alternative: Write recipe to ConfigMap as well
aicr recipe \
  --snapshot cm://gpu-operator/aicr-snapshot \
  --intent training \
  --output cm://gpu-operator/aicr-recipe

# With custom kubeconfig (if not using default)
aicr recipe \
  --snapshot cm://gpu-operator/aicr-snapshot \
  --kubeconfig ~/.kube/prod-cluster \
  --intent training \
  --output recipe.yaml

# Step 3: Validate recipe constraints against cluster snapshot
aicr validate \
  --recipe recipe.yaml \
  --snapshot cm://gpu-operator/aicr-snapshot

# For CI/CD pipelines: exit non-zero on validation failure
aicr validate \
  --recipe recipe.yaml \
  --snapshot cm://gpu-operator/aicr-snapshot \
  --fail-on-error

# Step 4: Create bundle from recipe
aicr bundle \
  --recipe recipe.yaml \
  --output ./deployment

# Step 5: Deploy to cluster
cd deployment && chmod +x deploy.sh && ./deploy.sh

# Step 6: Verify deployment
kubectl get pods -n gpu-operator
kubectl logs -n gpu-operator -l app=nvidia-operator-validator
```

### E2E Testing

Validate the complete workflow:

```shell
# Run all CLI integration tests (no cluster needed)
make e2e

# Run a single chainsaw test
AICR_BIN=$(find dist -maxdepth 2 -type f -name aicr | head -n 1)
chainsaw test --no-cluster --test-dir tests/chainsaw/cli/recipe-generation
```

## Shell Completion

Generate shell completion scripts:

```shell
# Bash
aicr completion bash

# Zsh
aicr completion zsh

# Fish
aicr completion fish

# PowerShell
aicr completion pwsh
```

**Installation:**

**Bash:**
```shell
source <(aicr completion bash)
# Or add to ~/.bashrc for persistence
echo 'source <(aicr completion bash)' >> ~/.bashrc
```

**Zsh:**
```shell
source <(aicr completion zsh)
# Or add to ~/.zshrc
echo 'source <(aicr completion zsh)' >> ~/.zshrc
```

## Environment Variables

AICR respects standard environment variables:

| Variable | Description | Default |
|----------|-------------|---------|
| `KUBECONFIG` | Path to Kubernetes config file | `~/.kube/config` |
| `LOG_LEVEL` | Logging level: debug, info, warn, error | info |
| `NO_COLOR` | Disable colored output | false |

## Exit Codes

| Code | Meaning |
|------|---------|
| 0 | Success |
| 1 | General error |
| 2 | Invalid arguments |
| 3 | File I/O error |
| 4 | Kubernetes connection error |
| 5 | Recipe generation error |

## Common Usage Patterns

### Quick Recipe Generation
```shell
aicr recipe --os ubuntu --accelerator h100 | jq '.componentRefs[]'
```

### Save All Steps
```shell
aicr snapshot -o snapshot.yaml
aicr recipe -s snapshot.yaml -i training -o recipe.yaml
aicr bundle -r recipe.yaml -o ./bundles
```

### JSON Processing
```shell
# Extract GPU Operator version from recipe
aicr recipe --os ubuntu --accelerator h100 --format json | \
  jq -r '.componentRefs[] | select(.name=="gpu-operator") | .version'

# Get all component versions
aicr recipe --os ubuntu --accelerator h100 --format json | \
  jq -r '.componentRefs[] | "\(.name): \(.version)"'
```

### Multiple Environments
```shell
# Generate recipes for different cloud providers
for service in eks gke aks; do
  aicr recipe --os ubuntu --service $service --gpu h100 \
    --output recipe-${service}.yaml
done
```

## Troubleshooting

### Snapshot Fails
```shell
# Check GPU drivers
nvidia-smi

# Check Kubernetes access
kubectl cluster-info

# Run with debug
aicr --debug snapshot
```

### Recipe Not Found
```shell
# Query parameters may not match any overlay
# Try broader query:
aicr recipe --os ubuntu --gpu h100
```

### Bundle Generation Fails
```shell
# Verify recipe file
cat recipe.yaml

# Check bundler is valid
aicr bundle --help  # Shows available bundlers

# Run with debug
aicr --debug bundle -r recipe.yaml
```

## External Data Directory

The `--data` flag enables extending or overriding the embedded recipe data with external files. This allows customization without rebuilding the CLI.

### Overview

AICR embeds recipe data (overlays, component values, registry) at compile time. The `--data` flag layers an external directory on top, enabling:

- **Custom components**: Add new components to the registry
- **Override values**: Replace default component values files
- **Custom overlays**: Add new recipe overlays for specific environments
- **Registry extensions**: Add custom components while preserving embedded ones

### Directory Structure

The external directory must mirror the embedded data structure:

```
my-data/
├── registry.yaml          # REQUIRED - merged with embedded registry
├── overlays/
│   └── base.yaml              # Optional - replaces embedded base.yaml
│   └── custom-overlay.yaml    # Optional - adds new overlay
└── components/
    └── gpu-operator/
        └── values.yaml        # Optional - replaces embedded values
```

### Requirements

1. **registry.yaml is required**: The external directory must contain a `registry.yaml` file
2. **Security validations**: Symlinks are rejected, file size is limited (10MB default)
3. **No path traversal**: Paths containing `..` are rejected

### Merge Behavior

| File Type | Behavior |
|-----------|----------|
| `registry.yaml` | **Merged** - External components are added to embedded; same-named components are replaced |
| All other files | **Replaced** - External file completely replaces embedded if path matches |

### Usage Examples

```shell
# Use external data directory for recipe generation
aicr recipe --service eks --accelerator h100 --data ./my-data

# Use external data directory for bundle generation
aicr bundle --recipe recipe.yaml --data ./my-data --output ./bundles

# Combine with other flags
aicr recipe --service eks --gpu gb200 --intent training \
  --data ./custom-recipes \
  --output recipe.yaml
```

### Example: Adding a Custom Component

1. **Create external data directory:**
```shell
mkdir -p my-data/components/my-operator
```

2. **Create registry.yaml with custom component:**
```yaml
# my-data/registry.yaml
apiVersion: aicr.nvidia.com/v1alpha1
kind: ComponentRegistry
components:
  - name: my-operator
    displayName: My Custom Operator
    helm:
      defaultRepository: https://my-charts.example.com
      defaultChart: my-operator
      defaultVersion: v1.0.0
```

3. **Create values file for the component:**
```yaml
# my-data/components/my-operator/values.yaml
replicaCount: 1
image:
  repository: my-registry/my-operator
  tag: v1.0.0
```

4. **Create overlay that includes the component:**
```yaml
# my-data/overlays/my-custom-overlay.yaml
kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: my-custom-overlay
spec:
  criteria:
    service: eks
    intent: training
  componentRefs:
    - name: my-operator
      type: Helm
      valuesFile: components/my-operator/values.yaml
```

5. **Generate recipe with external data:**
```shell
aicr recipe --service eks --intent training --data ./my-data
```

### Debugging External Data

Use `--debug` flag to see detailed logging about external data loading:

```shell
aicr --debug recipe --service eks --data ./my-data
```

Debug logs include:
- External files discovered and registered
- File source resolution (embedded vs external)
- Registry merge details (components added/overridden)

## Example Files

The `examples/` directory contains reference files for testing and learning:

### Recipes (`examples/recipes/`)

| File | Description |
|------|-------------|
| `kind.yaml` | Recipe for local Kind cluster with fake GPU |
| `eks-training.yaml` | EKS recipe optimized for training workloads |
| `eks-gb200-ubuntu-training-with-validation.yaml` | GB200 on EKS with Ubuntu and multi-phase validation |

**Usage:**
```shell
# Generate bundle from example recipe
aicr bundle --recipe examples/recipes/eks-training.yaml --output ./bundles
```

### Templates (`examples/templates/`)

| File | Description |
|------|-------------|
| `snapshot-template.md.tmpl` | Go template for custom snapshot report formatting |

**Usage:**
```shell
# Generate custom cluster report
aicr snapshot --template examples/templates/snapshot-template.md.tmpl --output report.md
```

## See Also

- [Installation Guide](installation.md) - Install aicr
- [Agent Deployment](agent-deployment.md) - Kubernetes agent setup
- [API Reference](api-reference.md) - Programmatic access
- [Architecture Docs](../contributor/) - Internal architecture
- [Data Architecture](../contributor/data.md) - Recipe data system details
