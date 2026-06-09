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
| `--debug` | | bool | false | Enable debug logging (text mode with full metadata) |
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
| `--format` | | string | yaml | Output format: json, yaml, table |
| `--config` | | string | | Path or HTTP/HTTPS URL to an AICRConfig file (YAML/JSON) that populates `spec.snapshot.*`. CLI flags below always win over the corresponding config field. |
| `--kubeconfig` | `-k` | string | ~/.kube/config | Path to kubeconfig file (overrides KUBECONFIG env). Also used when `--output` is a ConfigMap URI so reads and writes target the same cluster. |
| `--namespace` | `-n` | string | default | Kubernetes namespace for agent deployment |
| `--image` | | string | ghcr.io/nvidia/aicr:latest | Container image for agent Job |
| `--job-name` | | string | aicr | Name for the agent Job |
| `--service-account-name` | | string | aicr | ServiceAccount name for agent Job |
| `--node-selector` | | string[] | auto | Node selector for agent scheduling (key=value, repeatable). When omitted (and neither `--require-gpu` nor `--runtime-class` is set), the agent auto-targets GPU nodes labeled `nvidia.com/gpu.present=true` if the cluster has any — see [GPU Node Auto-Targeting](#gpu-node-auto-targeting). Pass an explicit selector to override. |
| `--toleration` | | string[] | all taints | Tolerations for agent scheduling (key=value:effect, repeatable). **Default: all taints tolerated** (uses `operator: Exists`). Only specify to restrict which taints are tolerated. |
| `--timeout` | | duration | 5m | Timeout for agent Job completion |
| `--no-cleanup` | | bool | false | Skip removal of Job and RBAC resources on completion. **Warning:** leaves a cluster-admin ClusterRoleBinding active. |
| `--privileged` | | bool | true | Run agent in privileged mode (required for GPU/SystemD collectors). Set to false for PSS-restricted namespaces. |
| `--image-pull-secret` | | string[] | | Image pull secrets for private registries (repeatable) |
| `--require-gpu` | | bool | false | Require GPU resources on the agent pod (mutually exclusive with `--runtime-class`) |
| `--runtime-class` | | string | | Runtime class for GPU access without consuming a GPU allocation (e.g., `nvidia`). Mutually exclusive with `--require-gpu`. |
| `--template` | | string | | Path to Go template file for custom output formatting (requires YAML format) |
| `--max-nodes-per-entry` | | int | 0 | Maximum node names per taint/label entry in topology collection (0 = unlimited) |
| `--os` | | string | | Node OS family (`ubuntu`, `rhel`, `cos`, `amazonlinux`, `talos`). Selects the per-OS pod configuration and in-pod service collector backend. `talos` skips the `/run/systemd` and `/etc/os-release` hostPath mounts and uses the Kubernetes-API service backend. Reads `AICR_OS` env when unset. |
| `--requests` | | string | | Override agent container resource requests as a comma-separated list of `name=quantity` pairs (e.g. `cpu=500m,memory=1Gi,ephemeral-storage=1Gi`). Unspecified resources keep the built-in privileged or restricted defaults. Reads `AICR_REQUESTS` env when unset. |
| `--limits` | | string | | Override agent container resource limits as a comma-separated list of `name=quantity` pairs (e.g. `cpu=1,memory=2Gi,ephemeral-storage=2Gi`). Unspecified resources keep the built-in defaults. With `--require-gpu`, the default `nvidia.com/gpu=1` is applied only when `--limits` does not already contain that key — an explicit `--limits nvidia.com/gpu=N` wins. Reads `AICR_LIMITS` env when unset. |

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

# Auto-target GPU nodes (default when no placement flag is set)
# On a cluster with nvidia.com/gpu.present=true nodes, the agent is
# steered onto a GPU node automatically — see GPU Node Auto-Targeting.
aicr snapshot --namespace gpu-operator

# Targeting specific nodes (explicit selector disables auto-targeting)
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

#### Snapshot Config File Mode

Drive `aicr snapshot` from an `AICRConfig` document so the snapshot inputs version-control alongside the recipe, bundle, and validate steps in an end-to-end workflow.

```yaml
kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: gke-h100-training
spec:
  snapshot:
    output:
      path: snapshot.yaml          # written to disk; same shape as -o
      format: yaml                 # yaml | json | table
      template: ""                 # optional Go template path
    agent:
      namespace: aicr-validation
      image: ""                    # default: ghcr.io/nvidia/aicr:latest
      imagePullSecrets: []
      jobName: aicr
      serviceAccountName: aicr
      nodeSelector:
        nodeGroup: gpu-worker
      tolerations:
        - dedicated=gpu-workload:NoSchedule
        - nvidia.com/gpu=present:NoSchedule
      requireGpu: false
      runtimeClassName: ""         # mutually exclusive with requireGpu
      os: ""                       # ubuntu | rhel | cos | amazonlinux | talos
      requests: ""                 # "cpu=500m,memory=1Gi"
      limits: ""                   # "cpu=1,memory=2Gi"
    execution:
      timeout: 5m
      noCleanup: false
      privileged: true             # set false for PSS-restricted namespaces
      maxNodesPerEntry: 0          # 0 = unlimited topology entries
```

Precedence: a CLI flag always wins over the matching config field. Selectors and tolerations omitted entirely inherit the snapshotter's compiled-in defaults (`tolerations` defaults to *tolerate all taints*); an explicit empty list (`tolerations: []`) clears the tolerate-all default — the same nil-vs-empty semantics used by `spec.validate.agent`.

```shell
# Run snapshot driven entirely by config
aicr snapshot --config aicr-config.yaml

# Reuse the same config but write to a one-off path
aicr snapshot --config aicr-config.yaml -o /tmp/snapshot.yaml
```

#### Custom Templates

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

#### Agent Deployment Mode

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

#### GPU Node Auto-Targeting

The agent Job defaults to an empty node selector that tolerates all taints, so on a
mixed CPU+GPU cluster the scheduler can place it on a non-GPU node where `nvidia-smi`
is absent — the snapshot then completes successfully but contains no GPU data. To close
that silent failure mode, `aicr snapshot` applies two safeguards:

**Proactive auto-targeting.** Before deploying the Job, if no placement constraint is
set (`--node-selector`, `--require-gpu`, and `--runtime-class` are all unset) and the
cluster has nodes labeled `nvidia.com/gpu.present=true`, the agent auto-injects that
selector so the Job lands on a GPU node:

```
auto-targeting GPU nodes: selector=nvidia.com/gpu.present=true (pass --node-selector to override)
```

- Passing any of `--node-selector`, `--require-gpu`, or `--runtime-class` disables
  auto-targeting — your explicit intent is always preserved.
- Detection relies on Node Feature Discovery / GPU Feature Discovery (NFD/GFD) labels.
  Clusters without those labels (e.g., before the GPU Operator is installed) are not
  auto-detected; target nodes explicitly with `--node-selector kubernetes.io/hostname=<gpu-node>`.
- The pre-flight node check is presence-only (`limit=1`), bounded by a 5s timeout, and
  **fails open**: an API error logs a warning and the snapshot proceeds without
  injection rather than blocking.
- A GPU node can be cordoned between detection and pod scheduling. When that happens the
  Job pends until `--timeout`; the timeout error names the auto-injected selector so the
  cause is clear.

**Reactive placement-mismatch warning.** After collection, if the snapshot has no GPU
data but the cluster topology shows GPU-capable nodes (via NFD labels), the agent emits
a loud warning so the result is not silently trusted:

```
snapshot has no GPU data but cluster topology shows GPU-capable nodes — agent likely ran on a non-GPU node
```

The warning lists remediation options:
- `--node-selector nvidia.com/gpu.present=true` (after GPU Operator/NFD)
- `--node-selector kubernetes.io/hostname=<gpu-node>` (before GPU Operator)
- `--require-gpu` (requests the `nvidia.com/gpu` resource; needs the Device Plugin)
- `--runtime-class nvidia` (nvidia-smi access without consuming a GPU slot)

#### ConfigMap Output

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
    app.kubernetes.io/version: <aicr-version>
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

#### Config File Mode (Recommended)

Generate recipes using an `AICRConfig` document. The same file format also drives the `bundle` command, so a single file can describe an end-to-end recipe-to-bundle workflow.

**Flags:**

| Flag | Short | Type | Description |
|------|-------|------|-------------|
| `--config` | | string | Path or HTTP/HTTPS URL to an AICRConfig file (YAML/JSON) |
| `--output` | `-o` | string | Output file (default: stdout) |
| `--format` | `-f` | string | Format: json, yaml (default: yaml) |
| `--data` | | string | External data directory to overlay on embedded data (see [External Data](#external-data-directory)) |

The config file uses a Kubernetes-style envelope:

```yaml
kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: gb200-eks-ubuntu-training
spec:
  recipe:
    criteria:
      service: eks
      os: ubuntu
      accelerator: gb200
      intent: training
      nodes: 8
    output:
      path: recipe.yaml
      format: yaml
```

Individual CLI flags always override config file values. For slice/map flags, presence on the CLI replaces the file's value (no append).

```shell
# Load criteria from config file
aicr recipe --config config.yaml

# Override service from file
aicr recipe --config config.yaml --service gke

# Save output to file
aicr recipe --config config.yaml -o recipe.yaml

# Load config from a URL (e.g. CI shared template)
aicr recipe --config https://team.example.com/configs/eks-h100-training.yaml
```

`--config` accepts a local file path or an HTTP/HTTPS URL. ConfigMap (`cm://`) sources are not supported; export the data with `kubectl get cm <name> -o yaml` and pass the resulting file.

#### Query Mode

Generate recipes using direct system parameters:

**Flags:**
| Flag | Short | Type | Description |
|------|-------|------|-------------|
| `--service` | | string | K8s service: eks, gke, aks, oke, kind, lke, bcm |
| `--accelerator` | `--gpu` | string | Accelerator/GPU type: h100, h200, gb200, b200, a100, l40, rtx-pro-6000 |
| `--intent` | | string | Workload intent: training, inference |
| `--os` | | string | OS family: ubuntu, rhel, cos, amazonlinux, talos |
| `--platform` | | string | Platform/framework type: dynamo, kubeflow, nim, runai, slurm |
| `--nodes` | | int | Number of GPU nodes in the cluster |
| `--output` | `-o` | string | Output file (default: stdout) |
| `--format` | `-f` | string | Format: json, yaml (default: yaml) |
| `--data` | | string | External data directory to overlay on embedded data (see [External Data](#external-data-directory)) |
| `--criteria-strict` | | bool | Reject criteria values not in the embedded OSS catalog; ignores values registered from `--data`. Also honored via `AICR_CRITERIA_STRICT=1` or `spec.recipe.criteriaStrict: true` in `--config`. Intended for OSS CI gates. |

> **Service / Accelerator / OS / Intent / Platform value listings above are the OSS-embedded set.** When `--data` registers additional values (e.g., undisclosed providers, proprietary platforms), the CLI admits them at runtime through the criteria registry — see [Data Extension](../integrator/data-extension.md). `--criteria-strict` restores the OSS-only set regardless of what `--data` contributes.

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
| `--kubeconfig` | `-k` | string | Path to kubeconfig file (used when `--snapshot` or `--output` is a ConfigMap URI; overrides KUBECONFIG env) |

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

### aicr recipe list

Enumerate overlay recipes in the catalog. Useful for discovering which criteria
combinations have a dedicated leaf overlay versus an intermediate shared recipe.

**Synopsis:**

```shell
aicr recipe list [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--service` | | string | | Filter by Kubernetes service type (e.g. eks, gke, aks) |
| `--accelerator` | `--gpu` | string | | Filter by accelerator/GPU type (e.g. h100, gb200) |
| `--intent` | | string | | Filter by workload intent (e.g. training, inference) |
| `--os` | | string | | Filter by worker-node OS (e.g. ubuntu, rhel, cos) |
| `--platform` | | string | | Filter by platform/framework type (e.g. dynamo, kubeflow, nim) |
| `--format` | `-t` | string | table | Output format: json, yaml, table |
| `--data` | | string | | External data directory to include alongside embedded overlays |

Filter flags narrow the output to overlays whose criteria carry that exact value.
Unspecified flags match all overlays for that dimension. Multiple filters are combined
with AND.

**Output fields:**

| Field | Description |
|-------|-------------|
| `name` | Overlay name (e.g. `h100-eks-ubuntu-training`) |
| `criteria` | The full criteria dimensions the overlay targets |
| `is_leaf` | `true` when the overlay is a leaf — no other overlay inherits from it |
| `source` | Data provenance: `embedded` (built-in) or `external` (from `--data`) |

**Examples:**

```shell
# List all overlays as a table (default)
aicr recipe list

# List all overlays as JSON
aicr recipe list --format json

# Filter to EKS training overlays
aicr recipe list --service eks --intent training

# Filter to H100 overlays and emit JSON
aicr recipe list --accelerator h100 --format json

# Include external overlays from a custom data directory
aicr recipe list --data /etc/aicr/custom-recipes --format yaml
```

**Example JSON output:**

```json
[
  {
    "name": "gb200-eks-ubuntu-training",
    "criteria": {"service": "eks", "accelerator": "gb200", "os": "ubuntu", "intent": "training"},
    "is_leaf": true,
    "source": "embedded"
  },
  {
    "name": "h100-eks-ubuntu-training",
    "criteria": {"service": "eks", "accelerator": "h100", "os": "ubuntu", "intent": "training"},
    "is_leaf": true,
    "source": "embedded"
  }
]
```

---

### aicr recipe verify-catalog

Verify the embedded recipe catalog (`registry.yaml` + `validators/catalog.yaml`)
against its Sigstore bundle. The bundle is distributed as `recipe-catalog.sigstore.json`
release asset alongside each tagged `aicr` binary.

`aicr recipe verify-catalog` recomputes a deterministic SHA-256 over the embedded
catalog content using a length-prefixed encoding of the two raw files, then
verifies the digest against the Sigstore bundle using NVIDIA CI identity pinning.
Exit code is 0 on success, non-zero on any verification failure.

**Synopsis:**

```shell
aicr recipe verify-catalog <bundle-path> [flags]
```

**Flags:**

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--identity-pattern` | string | | Override the NVIDIA CI certificate identity regexp. Must contain `NVIDIA/aicr` — overrides that drop the repo prefix are rejected. Also reads `AICR_CATALOG_IDENTITY_PATTERN`. |

**Examples:**

```shell
# Download the catalog signature for a tagged release and verify it.
curl -Lo recipe-catalog.sigstore.json \
  https://github.com/NVIDIA/aicr/releases/download/vX.Y.Z/recipe-catalog.sigstore.json

aicr recipe verify-catalog recipe-catalog.sigstore.json
```

**Output:**

On success, prints the verified content digest and the Fulcio certificate
identity (the GitHub Actions workflow that signed the release):

```text
catalog verified
  digest:   sha256:<hex>
  identity: https://github.com/NVIDIA/aicr/.github/workflows/on-tag.yaml@refs/tags/vX.Y.Z
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

#### Selector Syntax

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
# EKS deploys 12 components (includes aws-ebs-csi-driver, aws-efa, nodewright-customizations)
aicr query --service eks --accelerator h100 --intent training --selector deploymentOrder
# GKE deploys 9 components (storage and networking are platform-managed)
aicr query --service gke --accelerator h100 --intent training --selector deploymentOrder

# Pin the exact driver version into Terraform/Pulumi variables
DRIVER_VERSION=$(aicr query --service eks --accelerator h100 --intent training \
  --selector components.gpu-operator.values.driver.version)
echo "gpu_driver_version = \"${DRIVER_VERSION}\""

# Compare nodewright tuning parameters across accelerators
# H100: real tuning packages (kernel setup, nvidia-tuned, full setup)
aicr query --service eks --accelerator h100 --intent training \
  --selector components.nodewright-customizations.values
# GB200: same value structure, but manifest renders a no-op (ARM64 packages pending)
aicr query --service eks --accelerator gb200 --intent training \
  --selector components.nodewright-customizations.values

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

For a task-oriented walkthrough (capture snapshot → generate recipe → run each
phase, with worked training and inference examples), see [Validation](validation.md).

**Synopsis:**
```shell
aicr validate [flags]
```

**Flags:**
| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--recipe` | `-r` | string | (required) | Path/URI to recipe file containing constraints (or via `spec.validate.input.recipe` in `--config`) |
| `--snapshot` | `-s` | string | | Path/URI to snapshot file containing measurements (omit to capture live) |
| `--config` | | string | | Path or HTTP/HTTPS URL to an AICRConfig file (YAML/JSON). CLI flags override values from this file. See [Validate Config File Mode](#validate-config-file-mode). |
| `--phase` | | string[] | all | Validation phase to run: deployment, performance, conformance, all (repeatable) |
| `--fail-on-error` | | bool | true | Exit with non-zero status if any constraint fails |
| `--fail-fast` | | bool | false | Stop after the first phase that fails. By default all phases run and produce results. |
| `--output` | `-o` | string | stdout | Output destination: file path, ConfigMap URI (`cm://namespace/name`), or stdout |
| `--kubeconfig` | `-k` | string | ~/.kube/config | Path to kubeconfig file (used when `--recipe`, `--snapshot`, or `--output` is a ConfigMap URI) |
| `--namespace` | `-n` | string | aicr-validation | Kubernetes namespace for validation Job deployment |
| `--image` | | string | ghcr.io/nvidia/aicr:latest | Container image for validation Job |
| `--image-pull-secret` | | string[] | | Image pull secrets for private registries (repeatable) |
| `--job-name` | | string | aicr-validate | Name for the validation Job |
| `--service-account-name` | | string | aicr | ServiceAccount name for validation Job |
| `--node-selector` | | string[] | | Override GPU node selection for validation workloads. Replaces platform-specific selectors (e.g., `cloud.google.com/gke-accelerator`, `node.kubernetes.io/instance-type`) on inner workloads like NCCL benchmark pods. Use when GPU nodes have non-standard labels. Does not affect the validator orchestrator Job. (format: key=value, repeatable) |
| `--toleration` | | string[] | | Override tolerations for validation workloads. Replaces the default tolerate-all policy on inner workloads like NCCL benchmark pods and conformance test pods. Does not affect the validator orchestrator Job. (format: key=value:effect, repeatable) |
| `--timeout` | | duration | 5m | Timeout for validation Job completion |
| `--no-cleanup` | | bool | false | Skip removal of Job and RBAC resources on completion |
| `--require-gpu` | | bool | false | Require GPU resources on the validation pod |
| `--no-cluster` | | bool | false | Skip cluster access (test mode): skips RBAC and Job deployment, reports checks as skipped |
| `--evidence-dir` | | string | | Directory to write conformance evidence artifacts |
| `--cncf-submission` | | bool | false | Generate CNCF conformance submission artifacts |
| `--feature` | `-f` | string[] | | CNCF evidence-collection feature(s) to scope (repeatable). Valid names: `dra-support`, `gang-scheduling`, `secure-access`, `accelerator-metrics`, `ai-service-metrics`, `inference-gateway`, `robust-operator`, `pod-autoscaling`, `cluster-autoscaling`. Empty selects all features. |
| `--emit-attestation` | | string | | Directory to write a recipe-evidence v1 attestation bundle (signed when `--push` is set). See [ADR-007](../design/007-recipe-evidence.md). |
| `--bom` | | string | | Path to a CycloneDX BOM (`bom.cdx.json`) to embed. Optional with `--emit-attestation`; when omitted, aicr synthesizes a recipe-bound BOM from the recipe's component refs + validator catalog images. Pass `make bom`'s output for an exhaustive BOM. |
| `--push` | | string | | OCI registry reference to push the signed summary bundle to. Triggers Sigstore keyless signing via the precedence chain documented under `--identity-token`. The `sha256:` digest is the canonical address, so the tag is only a human-readable label — tag choice never affects verification. Omit the tag and aicr derives a unique per-recipe one, `<recipe-slug>-<short-fingerprint>` (e.g. `ghcr.io/myorg/aicr-evidence:h100-eks-ubuntu-training-3f9a1c2b4d5e`), so distinct attestations never collide on a shared tag. Pass an explicit tag to override. |
| `--plain-http` | | bool | false | Use HTTP instead of HTTPS for evidence push (local registry tests). |
| `--insecure-tls` | | bool | false | Skip TLS verification for evidence push (self-signed registries). |
| `--identity-token` | | string | | Pre-fetched OIDC identity token for `--push` keyless signing. Skips ambient/browser/device-code flows. Reads `COSIGN_IDENTITY_TOKEN` from env. Same precedence chain as `aicr bundle --attest`. |
| `--oidc-device-flow` | | bool | false | Use the OAuth 2.0 device authorization grant for `--push` OIDC instead of opening a browser callback. Reads `AICR_OIDC_DEVICE_FLOW`. |
| `--data` | | string | | External data directory to overlay on embedded data |

**Input Sources:**
- **File**: Local file path (`./recipe.yaml`, `./snapshot.yaml`)
- **URL**: HTTP/HTTPS URL (`https://example.com/recipe.yaml`)
- **ConfigMap**: Kubernetes ConfigMap URI (`cm://namespace/configmap-name`)

#### Validation Phases

Validation can be run in different phases to validate different aspects of the deployment:

| Phase | Description | When to Run |
|-------|-------------|-------------|
| `deployment` | Validates component deployment completeness plus post-install GPU readiness signals (see below) | After deploying components |
| `performance` | Validates system performance and network fabric health | After components are running |
| `conformance` | Validates workload-specific requirements and conformance | Before running production workloads |
| `all` | Runs all phases sequentially; results collected regardless of failures | Complete end-to-end validation |

> **Note:** Readiness constraints (K8s version, OS, kernel) are always evaluated implicitly before any phase runs. If readiness fails, validation stops before deploying any Jobs.

**Deployment phase checks:**

The `deployment` phase verifies that the cluster is actually ready for GPU workloads — not just that install commands returned successfully. It covers:

- Enabled component namespaces are `Active`.
- Declared `expectedResources` (Deployments, DaemonSets, etc.) exist and are healthy.
- When `nodewright-customizations` is enabled: every Skyhook CR the recipe declares reports `status.status == complete`. The set of expected CR names is extracted from the recipe's own `ComponentRef.ManifestFiles` for this component, so unrelated Skyhook CRs on the cluster (stale from prior deploys, or owned by another tenant) are ignored. If no Skyhook names can be extracted from those `ManifestFiles`, deployment validation fails closed as a recipe/configuration error instead of skipping.
- When `gpu-operator` is enabled: `ClusterPolicy` reports `status.state == ready`.
- When `nvidia-dra-driver-gpu` is enabled: the kubelet-plugin DaemonSet is ready. Discovery is by the upstream chart's role-suffix convention — the validator finds the single DaemonSet in the component namespace whose name ends in `-kubelet-plugin`, so custom `fullnameOverride` values are handled automatically.

**Graceful skip:** If a component is declared in the recipe but its CRD is not yet registered on the cluster (e.g., fresh cluster, operator chart not installed), the corresponding readiness check is skipped rather than failing. Once the CRD is present, the check runs and a missing CR is treated as a real failure — for example, if the `gpu-operator` CRD is registered but no `ClusterPolicy` CR exists, deployment validation fails with a "CR missing" diagnostic rather than silently passing. Other errors still fail closed: an RBAC denial on `skyhooks.skyhook.nvidia.com` returns HTTP 403 (not a `NoMatch`), so the validator surfaces it as a failure instead of silently skipping the Skyhook check.

**Day-N re-verification:** Because this is a read-only check against live cluster state, re-running `aicr validate --phase deployment` after scale-up, upgrade, or other runtime changes is safe and answers the same "is this cluster ready for GPU workloads now?" question.

**Phase Dependencies:**
- Phases run sequentially when using `--phase all`
- All phases run by default and produce results regardless of earlier failures. Use `--fail-fast` to stop after the first phase that fails (e.g., to skip a 65-minute inference-perf run when deployment already failed).
- Use individual phases for targeted validation during specific deployment stages

#### Constraint Format

Constraints use fully qualified measurement paths: `{Type}.{Subtype}.{Key}`

| Constraint Path | Description |
|-----------------|-------------|
| `K8s.server.version` | Kubernetes server version |
| `OS.release.ID` | Operating system identifier (ubuntu, rhel) |
| `OS.release.VERSION_ID` | OS version (24.04, 22.04) |
| `OS.sysctl./proc/sys/kernel/osrelease` | Kernel version |
| `GPU.info.type` | GPU hardware type |

#### Supported Operators

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

# Write a recipe-evidence v1 attestation bundle (unsigned, on disk).
# --bom is optional: when omitted, aicr synthesizes a recipe-bound BOM from
# the recipe's component refs and validator catalog images.
aicr validate \
  --recipe recipe.yaml --snapshot snapshot.yaml \
  --emit-attestation ./out
# Writes ./out/summary-bundle/ and ./out/pointer.yaml.

# Use an exhaustive BOM (e.g., `make bom`-produced) instead of the auto-generated one
aicr validate \
  --recipe recipe.yaml --snapshot snapshot.yaml \
  --emit-attestation ./out --bom dist/bom/bom.cdx.json

# Sign and push a recipe-evidence bundle to OCI (cosign keyless via Sigstore public-good).
# Token acquisition follows the same precedence chain as `aicr bundle --attest`:
# pre-fetched COSIGN_IDENTITY_TOKEN > ambient GitHub Actions OIDC > --oidc-device-flow > interactive browser.
aicr validate \
  --recipe recipe.yaml --snapshot snapshot.yaml \
  --emit-attestation ./out \
  --push ghcr.io/myorg/aicr-evidence  # tag optional; aicr derives :<recipe-slug>-<fingerprint>
# After this, copy ./out/pointer.yaml to recipes/evidence/<recipe>.yaml

# Validate on a cluster with custom GPU node labels (non-standard labels that AICR doesn't
# recognize by default, e.g., using a custom node pool label instead of cloud-provider defaults)
aicr validate \
  --recipe recipe.yaml \
  --node-selector my-org/gpu-pool=true \
  --phase performance

# Override both node selector and tolerations for a non-standard taint setup
aicr validate \
  --recipe recipe.yaml \
  --node-selector gpu-type=h100 \
  --toleration gpu-type=h100:NoSchedule
```

#### Validate Config File Mode

`aicr validate --config <path>` reads inputs from an AICRConfig YAML/JSON file
under `spec.validate`. CLI flags always override values loaded from `--config`;
override events are logged at INFO so users can see which input won. The OIDC
identity token used for `--push` signing stays out of the schema by design
(short-lived tokens must not be committed); the CLI resolves it at sign time
through the precedence chain described on `--identity-token`.

**Supported schema:**

```yaml
kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: prod-validate
spec:
  validate:
    input:
      recipe: ./recipe.yaml
      snapshot: ./snapshot.yaml          # optional; omit to capture live
    agent:                               # only used when input.snapshot is empty
      namespace: aicr-validation
      image: ghcr.io/nvidia/aicr:v0.1.0
      imagePullSecrets: [registry-secret]
      jobName: aicr-validate
      serviceAccountName: aicr
      nodeSelector:
        my-org/gpu-pool: "true"
      tolerations:
        - "gpu-type=h100:NoSchedule"
      requireGpu: true
    execution:
      phases: [deployment, conformance]
      failOnError: true                  # default true; set false to report only
      noCluster: false
      noCleanup: false
      timeout: 10m
    evidence:
      cncf:                              # --evidence-dir / --cncf-submission / --feature
        dir: ./out/cncf
        cncfSubmission: false
        features: []                     # empty = all features
      attestation:                       # --emit-attestation / --bom / --push / ...
        out: ./out/attestation
        bom: dist/bom/bom.cdx.json       # optional; auto-generated from recipe + validators when absent
        push: ghcr.io/myorg/aicr-evidence  # tag optional; aicr derives :<recipe-slug>-<fingerprint>
        plainHTTP: false
        insecureTLS: false
```

**Examples:**

```shell
# Use a config file
aicr validate --config validate.yaml

# Override a single config value from the CLI
aicr validate --config validate.yaml --phase deployment

# Validate the same recipe across two clusters using two different agent
# configs (config-bound) without retyping flags
aicr validate --config validate-cluster-a.yaml
aicr validate --config validate-cluster-b.yaml
```

#### Workload Scheduling

The `--node-selector` and `--toleration` flags control scheduling for **validation
workloads** — the inner pods that validators create to test cluster functionality
(e.g., NCCL benchmark workers, conformance test pods). They do **not** affect the
validator orchestrator Job, which runs lightweight check logic and is placed on
CPU-preferred nodes automatically.

When `--node-selector` is provided, it replaces the platform-specific selectors
that validators use by default:

| Platform | Default Selector (replaced) | Use Case |
|----------|-----------------------------|----------|
| GKE | `cloud.google.com/gke-accelerator: nvidia-h100-mega-80gb` | Non-standard GPU node pool labels |
| EKS | `node.kubernetes.io/instance-type: <discovered>` | Custom node pool labels |
| OKE | `nvidia.com/gpu.product: <discovered>` (when GFD labels are present) | Pin NCCL workers to the accelerator cohort sized for the test; non-GFD clusters get no default selector |

When `--toleration` is provided, it replaces the default tolerate-all policy
(`operator: Exists`) on workloads that need to land on tainted GPU nodes.

Validators that use `nodeName` pinning (nvidia-smi, DRA isolation test) or
DRA ResourceClaims for placement (gang scheduling) are not affected by these flags.

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
        "stdout": ["All deployment resources and required readiness signals are healthy"]
      },
      {
        "name": "nccl-all-reduce-bw",
        "status": "passed",
        "duration": 234000,
        "suite": ["performance"],
        "stdout": ["NCCL All Reduce bandwidth: 488.37 GB/s", "Constraint: >= 100 → true"]
      },
      {
        "name": "inference-perf",
        "status": "passed",
        "duration": 612000,
        "suite": ["performance"],
        "stdout": [
          "RESULT: Inference throughput: 108789.87 tokens/sec",
          "RESULT: Inference TTFT p99: 687.50 ms",
          "Throughput constraint: >= 50000 → PASS",
          "TTFT p99 constraint: <= 2000 → PASS"
        ]
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
| `5` | Timeout (validator section or context deadline exceeded) |
| `8` | One or more checks failed (when `--fail-on-error` is set) |

---

### aicr diff

Compare two snapshots field-by-field to surface configuration drift between cluster states. Reports added, removed, and modified readings across every measurement type (K8s, GPU, OS, SystemD, NodeTopology).

**Synopsis:**
```shell
aicr diff --baseline <path|cm://...> --target <path|cm://...> [flags]
```

**Flags:**
| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--baseline` | `-b` | string | | Baseline snapshot (file path or ConfigMap URI). **Required.** |
| `--target` | | string | | Target snapshot (file path or ConfigMap URI). **Required.** |
| `--fail-on-drift` | | bool | false | Exit with non-zero status (`ErrCodeConflict`) if any drift is detected. Useful for CI/CD gating. |
| `--output` | `-o` | string | stdout | Output destination: file path, ConfigMap URI (`cm://namespace/name`, JSON/YAML only), or stdout. **Note:** ConfigMap destinations are rejected for `--format table` (a structured format is required for ConfigMap storage). |
| `--format` | `-t` | string | yaml | Output format: `json`, `yaml`, or `table`. |
| `--kubeconfig` | `-k` | string | ~/.kube/config | Path to kubeconfig (used when `--baseline`, `--target`, or `--output` is a ConfigMap URI). |

**Inputs:**
- File paths (`./baseline.yaml`, `/tmp/snap.json`)
- ConfigMap URIs (`cm://gpu-operator/aicr-snapshot`)
- Both inputs may mix freely; e.g., a local baseline file vs. a live ConfigMap target.

**Output Semantics:**
- A nil reading is rendered as the literal `<nil>` so it cannot be confused with an empty-string value (`""`). Both forms surface as drift when one side is nil and the other is a concrete value.
- Changes are emitted in deterministic order (sorted by `Path`) so the diff is reproducible across runs and machines.
- The `Result` envelope includes `baselineSource` and `targetSource` (the supplied paths), a `changes` array, and a `summary` with `added`, `removed`, `modified`, and `total` counts.

**Examples:**

```shell
# Local-file diff in default YAML
aicr diff --baseline before.yaml --target after.yaml

# Human-readable table to stdout
aicr diff -b before.yaml --target after.yaml --format table

# CI/CD gate: non-zero exit on drift, JSON to a file
aicr diff -b before.yaml --target after.yaml \
  --format json --output drift.json --fail-on-drift

# Compare two ConfigMaps in the cluster
aicr diff \
  --baseline cm://gpu-operator/aicr-snapshot-baseline \
  --target   cm://gpu-operator/aicr-snapshot

# Mix file + ConfigMap (golden baseline vs live cluster)
aicr diff --baseline ./golden.yaml --target cm://default/aicr-snapshot
```

**Exit Codes:**

| Code | Description |
|------|-------------|
| `0` | Diff completed; no drift, or `--fail-on-drift` not set |
| `2` | Invalid input (missing flags, bad format, ConfigMap output for `--format table`) **or** drift detected with `--fail-on-drift` (mapped from `ErrCodeConflict`) |

> **Note on CI gating:** A non-zero exit identifies *that* drift was detected, but doesn't by itself distinguish drift from malformed input — both map to exit `2`. To differentiate without relying on stderr format (text by default; JSON only with `--log-json`), inspect the diff payload directly: write the result with `--output drift.json --format json` and branch on the presence of the file plus its `summary.total` field. That signal is format-stable regardless of logging mode.

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
| `--recipe` | `-r` | string | Path to recipe file (required, or via `spec.bundle.input.recipe` in `--config`) |
| `--config` | | string | Path or HTTP/HTTPS URL to an AICRConfig file (YAML/JSON). CLI flags override values from this file. See [Bundle Config File Mode](#bundle-config-file-mode). |
| `--output` | `-o` | string | Output directory (default: current dir) |
| `--deployer` | `-d` | string | Deployment method: `helm` (default), `argocd`, `argocd-helm`, `flux`, or `helmfile` |
| `--repo` | | string | Git/OCI repository URL baked into Argo CD Application sources. Used with `--deployer argocd`. Ignored with `--deployer argocd-helm` (that bundle is URL-portable — the URL is supplied at `helm install` time via `--set repoURL=...`); a warning is logged if passed. |
| `--set` | | string[] | Override **scalar** values in bundle files (repeatable, format: `component:path=value`). Use `enabled` key to include/exclude components (e.g., `--set awsebscsidriver:enabled=false`). Scalar-only — for list/object values use `--set-json` / `--set-file`. |
| `--set-json` | | string[] | Override values with a JSON-encoded **list or object** (repeatable, format: `component:path=<json>`, e.g. `--set-json agentgateway:allowedSourceRanges='["216.228.127.128/30"]'`). Object values deep-merge into existing maps; lists and scalars replace. Takes precedence over `--set` on the same path. See [List and Object Value Overrides](#list-and-object-value-overrides). |
| `--set-file` | | string[] | Override a value by reading JSON/YAML from a file (repeatable, format: `component:path=<filepath>`). For larger structures than `--set-json`; same merge semantics. |
| `--dynamic` | | string[] | Declare value paths as install-time parameters (repeatable, format: `component:path`). Supported with `helm`, `argocd-helm`, `flux`, and `helmfile` deployers. See [Dynamic Install-Time Values](#dynamic-install-time-values). |
| `--data` | | string | External data directory to overlay on embedded data (see [External Data](#external-data-directory)) |
| `--system-node-selector` | | string[] | Node selector for system components (format: key=value, repeatable) |
| `--system-node-toleration` | | string[] | Toleration for system components (format: key=value:effect, repeatable) |
| `--accelerated-node-selector` | | string[] | Node selector for accelerated/GPU nodes (format: key=value, repeatable) |
| `--accelerated-node-toleration` | | string[] | Toleration for accelerated/GPU nodes (format: key=value:effect, repeatable) |
| `--workload-gate` | | string | Taint for nodewright-operator runtime required (format: key=value:effect or key:effect). This is a day 2 option for cluster scaling operations. |
| `--workload-selector` | | string[] | Label selector for nodewright-customizations to prevent eviction of running training jobs (format: key=value, repeatable). Required when nodewright-customizations is enabled with training intent. |
| `--nodes` | | int | Estimated number of GPU nodes (default: 0 = unset). At bundle time, written to Helm value paths declared in the registry under `nodeScheduling.nodeCountPaths`. |
| `--storage-class` | | string | Kubernetes StorageClass name to inject at bundle time. Written to registry-declared `storageClassPaths` for each component. Overrides any `storageClassName` set in recipe overlays. |
| `--vendor-charts` | | bool | Pull upstream Helm chart bytes into the bundle at bundle time so the artifact is fully self-contained and air-gap deployable. Requires `helm` on `$PATH`. See [Vendoring Charts for Air-Gap](#vendoring-charts-for-air-gap). |
| `--readiness-hooks` | | bool | Emit a per-component readiness gate (`NNN-<name>-readiness/`) for each component that ships a `recipes/components/<name>/readiness.yaml` Chainsaw test. The gate runs as a post-component Job so the deploy blocks on component-specific readiness signals (e.g. `ClusterPolicy` state). Supported with `--deployer helm`, `argocd`, and `argocd-helm`. Off by default. See [Readiness Gates](#readiness-gates). |
| `--flux-oci-source-name` | | string | Name of the OCIRepository CR that Flux uses to pull the bundle (default: `aicr-bundle`). Used with `--deployer flux` and OCI output. Must match the OCIRepository deployed in the target cluster. See [Flux OCI Mode](#flux-oci-mode). |
| `--flux-namespace` | | string | Kubernetes namespace where Flux CRs (HelmRelease, sources, ArtifactGenerator) are deployed (default: `flux-system`). Must match the namespace of the Flux installation in the target cluster. |
| `--app-name` | | string | Parent Argo Application name (default: `aicr-stack` for `--deployer argocd-helm`, `nvidia-stack` for `--deployer argocd`). Must be a DNS-1123 subdomain. Required when deploying multiple non-overlapping AICR bundles to the same Argo CD namespace so the parent Applications do not collide. For `--deployer argocd-helm`, the value is the chart default and can still be overridden at install time via `helm install --set appName=...`. Rejected on other deployers (`helm`, `flux`, `helmfile`). |
| `--kubeconfig` | `-k` | string | Path to kubeconfig file |
| `--insecure-tls` | | bool | Skip TLS verification for OCI registry connections |
| `--plain-http` | | bool | Use plain HTTP for OCI registry connections |
| `--image-refs` | | string | Path to image references file for OCI registry |
| `--attest` | | bool | Enable bundle attestation and binary provenance verification. Requires OIDC authentication, or a KMS key via `--signing-key` for environments without OIDC. See [Bundle Attestation](#bundle-attestation). |
| `--certificate-identity-regexp` | | string | Override the certificate identity pattern for binary attestation verification. Must contain `"NVIDIA/aicr"`. For testing only. |
| `--identity-token` | | string | Pre-fetched OIDC identity token for `--attest` keyless signing. Skips ambient/browser/device-code flows. Prefer `COSIGN_IDENTITY_TOKEN` on shared hosts — flag values are visible in `ps` and `/proc/<pid>/cmdline`. |
| `--oidc-device-flow` | | bool | Use the OAuth 2.0 device authorization grant for `--attest` instead of opening a browser callback. Useful on headless hosts that can still reach Sigstore (`--identity-token` and CI ambient OIDC are alternatives). Also reads `AICR_OIDC_DEVICE_FLOW`. |
| `--fulcio-url` | | string | Override the Fulcio CA URL for `--attest` keyless signing, pointing at a private Sigstore instance. Must be an absolute `https://` URL with no embedded credentials. Defaults to the public-good Fulcio when omitted. Also reads `AICR_FULCIO_URL`. |
| `--rekor-url` | | string | Override the Rekor transparency-log URL for `--attest` keyless signing, pointing at a private Sigstore instance. Must be an absolute `https://` URL with no embedded credentials. Defaults to the public-good Rekor when omitted. Also reads `AICR_REKOR_URL`. The two URLs are independent — a private Fulcio can pair with the public Rekor or vice versa. |
| `--signing-key` | | string | Sign the `--attest` bundle with a KMS-backed key instead of keyless OIDC, for CI/CD environments without OIDC (Jenkins, internal pipelines). Takes a cloud KMS URI; supported schemes are `awskms://`, `gcpkms://`, and `azurekms://`. Mutually exclusive with `--identity-token`, `--oidc-device-flow`, and `--fulcio-url` (the keyless-only flags); passing both is a validation error. `--rekor-url` may still be combined to log to a private Rekor. See [KMS-Backed Signing](#kms-backed-signing). |

#### Bundle Config File Mode

The bundle command accepts the same `AICRConfig` format used by `aicr recipe`. A single file can populate both `spec.recipe` and `spec.bundle`, capturing an end-to-end workflow that can be committed to git, fetched from CI, or shared across environments.

When both `spec.recipe.output.path` and `spec.bundle.input.recipe` are set, they must reference the same path; otherwise loading fails fast.

```yaml
kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  bundle:
    input:
      recipe: ./recipe.yaml
    output:
      target: oci://ghcr.io/example/bundle:v1.0.0
    deployment:
      deployer: argocd
      repo: https://example.git/charts
      set:
        - gpuoperator:driver.version=570.86.16
    scheduling:
      systemNodeSelector:
        role: system
      acceleratedNodeTolerations:
        - "nvidia.com/gpu=present:NoSchedule"
      nodes: 8
      storageClass: gp3
    attestation:
      enabled: false
      # Optional: target a private Sigstore instead of the public-good
      # endpoints. Each defaults to public Sigstore when omitted; both must
      # be absolute https:// URLs with no embedded credentials.
      fulcioURL: https://fulcio.internal.example.com
      rekorURL: https://rekor.internal.example.com
    registry:
      insecureTLS: false
      plainHTTP: false
```

```shell
# Drive the bundle entirely from a config file
aicr bundle --config bundle.yaml

# Override the deployer for a one-off run
aicr bundle --config bundle.yaml --deployer helm
```

CLI flags always override values loaded from `--config`. For slice/map flags (`--set`, `--dynamic`, `--system-node-selector`, etc.), CLI presence replaces the config's value rather than appending. Override events are logged at INFO so users can see which input won.

**Secrets:** the cosign identity token is never read from a config file; supply it via `--identity-token` or `COSIGN_IDENTITY_TOKEN`.

#### Node Scheduling

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
- **System components** (gpu-operator controller, NFD gc/master, dynamo grove, agentgateway proxy): `nodeSelector=nodeGroup=system-worker` + tolerations for `dedicated=system-workload` with both `NoSchedule` and `NoExecute`

**Behavior:**
- All components from the recipe are bundled automatically
- Each component creates a subdirectory in the output directory
- Components are deployed in the order specified by `deploymentOrder` in the recipe

#### Storage Class

The `--storage-class` flag injects a Kubernetes StorageClass name into components at bundle time. StorageClass is a cluster infrastructure detail — the right value depends on what the target cluster has provisioned, not on the recipe.

When provided, the value is written to all Helm value paths declared in the component registry under `storageClassPaths`, overriding any `storageClassName` set in recipe overlays. If a per-component `--set <component>:<path>=<value>` explicitly targets the same path, that value takes precedence over `--storage-class`.

**Example:**

```bash
# Use EBS gp3 instead of the overlay default gp2 on EKS
aicr bundle --recipe recipe.yaml \
  --storage-class gp3 \
  --output bundle

# Use a custom storage class on an on-prem cluster
aicr bundle --recipe recipe.yaml \
  --storage-class local-path \
  --output bundle
```

When `--storage-class` is not set, any `storageClassName` values already defined in the recipe overlays are preserved as defaults. When it is set, `--set <component>:<path>=<value>` on the same path still wins — `--storage-class` only fills in paths that were not explicitly overridden.

If a rendered component creates a PVC at a registry-declared `storageClassPaths` entry and no usable `storageClassName` is set after overlay, `--storage-class`, and `--set` precedence is resolved, `aicr bundle` emits a non-blocking warning. The bundle still relies on the target cluster's default StorageClass in that case.

Similarly, when a bundle includes the `agentgateway` component with an unscoped `allowedSourceRanges` — empty, or including an any-source CIDR such as `0.0.0.0/0` or `::/0` — `aicr bundle` emits a non-blocking warning that the inference-gateway will be provisioned as an internet-facing `LoadBalancer` open to `0.0.0.0/0`. Scope it to trusted CIDRs via a recipe `componentRef` override or the list-aware [`--set-json`](#list-and-object-value-overrides) flag (`agentgateway:allowedSourceRanges='["<cidr>"]'`). See [Inference Gateway Network Exposure](component-catalog.md#inference-gateway-network-exposure).

#### Deployment Methods

The `--deployer` flag controls how deployment artifacts are generated:

| Method | Description |
|--------|-------------|
| `helm` | (Default) Generates Helm charts with values for deployment. Supports `--dynamic`. |
| `argocd` | Generates Argo CD Application manifests for GitOps deployment. Does **not** support `--dynamic`. |
| `argocd-helm` | Generates a Helm chart app-of-apps for Argo CD. All values overridable at install time via `helm --set`. Use `--dynamic` to pre-populate specific paths. |
| `flux` | Generates Flux HelmRelease manifests for GitOps deployment. Supports `--dynamic` via ConfigMap `valuesFrom`. |
| `helmfile` | Generates a `helmfile.yaml` release graph driven by the upstream [helmfile](https://helmfile.readthedocs.io/) CLI (`helmfile apply` / `diff` / `destroy`). Supports `--dynamic` via per-release `cluster-values.yaml`. Requires the `helmfile` binary at deploy time. |

> **Note:** `--dynamic` is not supported with `--deployer argocd`. Use `--deployer argocd-helm` instead, which produces a Helm chart where all values are overridable at install time.

**Deployment Order:**

All deployers respect the `deploymentOrder` field from the recipe, ensuring components are installed in the correct sequence:

- **Helm**: Components listed in README in deployment order
- **Argo CD**: Uses `argocd.argoproj.io/sync-wave` annotation (0 = first, 1 = second, etc.)
- **Flux**: Uses `dependsOn` references in HelmRelease CRs (each component depends on the previous component's terminal release — its `<prev>-post` release when post-manifests are present, otherwise `<prev>`). Components with pre-manifests insert a `<name>-pre` release that the primary HelmRelease depends on, so the chain becomes `previous → <name>-pre → <name> → <name>-post → next`. The bundle's root `kustomization.yaml` is a plain Kustomize file (not a Flux Kustomization CR).
- **Helmfile**: Uses `needs:` references in each release (each component depends on its predecessor)

#### Value Overrides

Override any value in the generated bundle files using dot notation:

```shell
--set bundler:path.to.field=value
```

**Format:** `bundler:path=value` where:
- `bundler` - Bundler name (e.g., `gpuoperator`, `networkoperator`, `certmanager`, `nodewright-operator`, `nvsentinel`)
- `path` - Dot-separated path to the field
- `value` - New value to set

**Behavior:**
- **Duplicate keys**: When the same `bundler:path` is specified multiple times, the **last value wins**
- **Array values**: Individual array elements cannot be overridden (no `[0]` index syntax). `--set` is **scalar-only** — pointing it at a list/object field writes a bare string and produces type-invalid output. To replace an entire array or object from the CLI, use [`--set-json` / `--set-file`](#list-and-object-value-overrides); recipe-level overrides in `componentRefs[].overrides` are the alternative.
- **Type conversion**: String values are automatically converted to appropriate types (`true`/`false` → bool, numeric strings → numbers)
- **Component enable/disable**: The special `enabled` key controls whether a component is included in the bundle. `--set <component>:enabled=false` excludes the component; `--set <component>:enabled=true` re-enables a recipe-disabled component. The `enabled` key is consumed by the bundler and not passed to Helm chart values.
- **Aliases merge**: overrides supplied under both a component's canonical name and a registered alias (e.g. `gpu-operator` and `gpuoperator`) are **combined, not dropped**; the canonical name wins on any shared path. (Same alias-merge behavior as [`--set-json` / `--set-file`](#list-and-object-value-overrides).)
- **Repeat to add; commas are literal**: To supply multiple overrides, repeat the flag (`--set a:x=1 --set b:y=2`). On the `bundle` command, commas inside a single slice-flag value are taken **literally** (not treated as a value separator), so a value containing a comma — and the comma-heavy JSON passed to `--set-json` — is preserved intact. This applies to all repeatable `bundle` flags (`--set`, `--set-json`, `--set-file`, `--dynamic`, `--*-node-selector`, `--*-node-toleration`, `--workload-selector`).

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

# Override Nodewright manager resources
aicr bundle -r recipe.yaml \
  --set nodewright-operator:manager.resources.cpu.limit=500m \
  --set nodewright-operator:manager.resources.memory.limit=256Mi \
  -o ./bundles

# Disable a component at bundle time (e.g., EBS CSI already installed as EKS addon)
aicr bundle -r recipe.yaml \
  --set awsebscsidriver:enabled=false \
  -o ./bundles
```

#### List and Object Value Overrides

`--set` is scalar-only: it cannot express a list or object value. Pointing it
at a list field such as `agentgateway.allowedSourceRanges` exits 0 but writes a
**bare string** at that path, producing a type-invalid manifest — the value may
be dropped or rejected at apply time. Use `--set-json` (inline) or `--set-file`
(from a file) for any list or object override.

**Format:** `component:path=<value>` where:
- `component` / `path` — same component name and dot-separated path as `--set`
- `<value>` — for `--set-json`, a JSON-encoded value; for `--set-file`, a path
  to a **regular file** containing a **single** JSON or YAML value (one
  document — in a multi-document YAML file only the first document is read). A
  non-regular path (directory, FIFO/named pipe, device, socket) is rejected
  with a fast validation error rather than hanging or failing later.

**Behavior:**
- **Object values deep-merge** into any existing map at the path (partial
  object overrides compose with recipe/base values), matching how inline recipe
  `componentRefs[].overrides` merge. Within a merged object, a JSON `null`
  **deletes** that key from the result (same explicit-null semantics as recipe
  overrides).
- **Lists and scalars replace** the value at the path.
- **Precedence**: applied after `--set`, so a `--set-json` / `--set-file` entry
  wins over a scalar `--set` on the same path. Between the two typed flags, an
  inline `--set-json` wins over a `--set-file` on the same `component:path`
  (mirroring Helm's `--set` taking precedence over `-f` value files). Within a
  single flag, the last entry for a given `component:path` wins.
- **Overlapping nested paths**: when one override targets a parent object and
  another targets a key beneath it (e.g. `comp:driver.env=<object>` plus
  `comp:driver.env.HTTPS_PROXY=<value>`), the deeper, more-specific path wins
  on the keys they share — regardless of the order the flags are given. The
  parent object's other keys are preserved.
- **Aliases merge**: overrides supplied under both a component's canonical name
  and a registered alias (e.g. `gpu-operator` and `gpuoperator`) are combined,
  not dropped; the canonical name wins on any shared path.
- **Node-scheduling paths deep-merge with CLI injection (asymmetric with
  `--set`)**: a typed override on a node-scheduling path
  (e.g. `--set-json gpu-operator:nodeSelector=<object>`) **deep-merges into**
  the selectors and tolerations injected by `--accelerated-node-selector` /
  `--system-node-selector` / `--*-node-toleration`, rather than suppressing that
  injection. This is intentional — deep-merge is the point of the typed path —
  but it differs from scalar `--set`: `--set comp:nodeSelector.x=y` marks that
  path as user-populated and suppresses CLI injection on it, whereas a typed
  override composes with the injected keys (so system-injected selector keys
  remain present alongside it). Use scalar `--set`, or omit the node-scheduling
  flags, when you need to fully replace an injected selector instead of merging
  into it.
- **CLI-only**: `--set-json` / `--set-file` have no `AICRConfig`
  (`spec.bundle.deployment.set`) or HTTP API (`?set=`) equivalent — those
  surfaces remain scalar-only. To set a list or object value outside the CLI,
  use a recipe overlay or `componentRefs[].overrides`.
- **Not for `enabled`**: the special `enabled` component toggle is honored only
  via scalar `--set`; passing it through `--set-json` / `--set-file` is rejected
  with an error (it would not toggle the component and would leak a stray
  `enabled:` value into the chart).

**Examples:**

```shell
# Scope the agentgateway inference-gateway to trusted CIDRs (inline JSON list)
aicr bundle -r recipe.yaml \
  --set-json agentgateway:allowedSourceRanges='["216.228.127.128/30","10.0.0.0/8"]' \
  -o ./bundles

# Same override, read from a file (JSON or YAML)
cat > ranges.yaml <<'EOF'
- 216.228.127.128/30
- 10.0.0.0/8
EOF
aicr bundle -r recipe.yaml \
  --set-file agentgateway:allowedSourceRanges=ranges.yaml \
  -o ./bundles

# Override object keys (deep-merges into the existing map)
aicr bundle -r recipe.yaml \
  --set-json gpuoperator:driver.env='{"HTTPS_PROXY":"http://proxy:3128"}' \
  -o ./bundles
```

```shell
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

# Day 2 options: workload-gate and workload-selector for nodewright
aicr bundle -r recipe.yaml \
  --workload-gate skyhook.nvidia.com/runtime-required=true:NoSchedule \
  --workload-selector workload-type=training \
  -o ./bundles

# Generate an attested bundle (opens browser for OIDC auth)
aicr bundle -r recipe.yaml --attest -o ./bundles

# In GitHub Actions (OIDC token detected automatically)
aicr bundle -r recipe.yaml --attest -o ./bundles

# Sign with a KMS-backed key in CI/CD without OIDC (Jenkins, internal pipelines)
aicr bundle --recipe recipe.yaml --attest \
  --signing-key awskms://arn:aws:kms:us-east-1:123456789012:key/abcd-1234 \
  --output ./bundles

# Generate Argo CD Application manifests for GitOps
aicr bundle -r recipe.yaml --deployer argocd -o ./bundles

# Argo CD with Git repository URL (avoids placeholder in app-of-apps.yaml)
aicr bundle -r recipe.yaml --deployer argocd \
  --repo https://github.com/my-org/my-gitops-repo.git \
  -o ./bundles

# Combine deployer with value overrides
aicr bundle -r recipe.yaml \
  --deployer argocd \
  -o ./bundles
```

#### Vendoring Charts for Air-Gap

The `--vendor-charts` flag pulls upstream Helm chart bytes into the bundle at bundle time. With the flag set, every Helm-typed component becomes a local chart inside the generated bundle and the resulting artifact deploys end-to-end with zero registry egress. Without the flag, deploy-time `helm upgrade --install` calls fetch from the upstream repository — which works for connected clusters but breaks in air-gapped environments.

**Bundle-time requirement:** the `helm` binary must be on `$PATH` when `aicr bundle --vendor-charts` runs. Authentication for private chart registries flows through Helm's own conventions:

- **HTTP(S) repositories** — `HELM_REPOSITORY_USERNAME` / `HELM_REPOSITORY_PASSWORD` environment variables.
- **OCI registries** — standard docker config (`~/.docker/config.json` or `$DOCKER_CONFIG`); run `docker login <registry>` ahead of time.

**Tradeoff: CVE-yank fail-loud signal is lost.** Non-vendored bundles fail loudly when an upstream chart version is yanked at registry time, which prompts a rebundle with a fixed recipe. Vendored bundles freeze the chart bytes at bundle creation and silently install the frozen version even after upstream yank. Treat `provenance.yaml` (below) as the audit surface for cross-referencing yank lists.

**Bundle-time costs.** Vendoring adds bundle-time network egress (the chart pull), bundle-time auth surface (private registries need credentials at the bundle host), and bundle size (typically 0.5–5 MB unpacked per chart). Users who don't need air-gap shouldn't set `--vendor-charts` and shouldn't pay these costs.

**Bundle layout with `--vendor-charts`** — every Helm component emits a single wrapper folder (mixed components no longer split into a primary + `-post` pair):

```text
my-bundle/
  001-gpu-operator/
    Chart.yaml                     # wrapper, declares the vendored subchart
    charts/gpu-operator-v25.3.0.tgz # vendored upstream tarball
    values.yaml                    # values nested under the subchart name
    cluster-values.yaml            # dynamic values, also nested
    install.sh                     # helm upgrade --install <name> ./<dir> ...
  002-alloy/
    Chart.yaml
    charts/alloy-1.2.3.tgz
    templates/                     # for mixed components: raw manifests
      clusterrole.yaml             #   with helm.sh/hook: post-install
    values.yaml
    cluster-values.yaml
    install.sh
  provenance.yaml                  # bundle-time audit log
  ...
```

**`provenance.yaml`** sits at the bundle root and lists one entry per vendored chart, using the same K8s-style `apiVersion`/`kind` shape as the rest of AICR's persisted formats:

```yaml
apiVersion: aicr.nvidia.com/v1alpha1
kind: BundleProvenance
vendoredCharts:
  - name: gpu-operator
    chart: gpu-operator
    version: v25.3.0
    repository: https://helm.ngc.nvidia.com/nvidia
    sha256: abc123...
    tarballName: gpu-operator-v25.3.0.tgz
    pullerVersion: helm-cli v3.20.2
```

The `sha256` field is the digest of the bytes copied into `charts/`, suitable for yank-list lookups and cross-bundle drift comparisons. Pipe through `yq -o=json provenance.yaml` if your scanner expects JSON.

**Examples:**

```bash
# Vendor everything for an air-gap deployment
aicr bundle --recipe recipe.yaml --vendor-charts -o ./bundle

# Vendor with private OCI registry credentials
docker login nvcr.io
aicr bundle --recipe recipe.yaml --vendor-charts -o ./bundle

# Vendor with private HTTP(S) chart repo credentials
HELM_REPOSITORY_USERNAME=robot \
HELM_REPOSITORY_PASSWORD=secret \
  aicr bundle --recipe recipe.yaml --vendor-charts -o ./bundle
```

#### Readiness Gates

The `--readiness-hooks` flag makes a deploy block on **component-specific readiness signals** rather than just the chart's own resources reporting Ready. A component opts in by shipping a `recipes/components/<name>/readiness.yaml` Chainsaw test that asserts the signal that actually means "ready" — for example, `gpu-operator` waits for its `ClusterPolicy` to reach `status.state: ready`, which Helm and Argo CD cannot assess natively.

With the flag set, the bundler emits an extra folder, `NNN-<name>-readiness/`, immediately after each opted-in component. The folder is a small chart containing a Kubernetes `Job` (plus the ServiceAccount/RBAC and a ConfigMap holding the Chainsaw test). The Job runs the `gate` CLI (`ghcr.io/nvidia/aicr-gate`, which embeds Chainsaw), which polls the test until it passes continuously for a stability window or a `--max-wait` ceiling elapses. The deploy blocks on that Job:

- **`helm`** — `deploy.sh` runs the readiness folder with `helm upgrade --install --wait`. The gate Job is a `post-install,post-upgrade` hook, and `--wait` blocks on hook completion regardless of `--wait-for-jobs`, so the latter is not needed. Helm's own `--timeout` is derived by the bundler from the gate's `--max-wait` plus a buffer, so the gate owns the deadline (Helm never preempts it).
- **`argocd` / `argocd-helm`** — the readiness folder inherits the next sync-wave after its component, and Argo CD blocks that wave on the gate Job via its built-in `batch/Job` health (Progressing → Healthy on success, Degraded on failure). No custom health Lua and no direct `ClusterPolicy` watch — the readiness logic stays encapsulated in the Chainsaw test the Job runs.

`flux` and `helmfile` are not yet supported and `--readiness-hooks` is rejected for them. Components without a `readiness.yaml` are unaffected. The gate image tracks the Chainsaw version pinned in `.settings.yaml`, so the in-cluster gate runs the same Chainsaw AICR validates with.

```bash
# Deploy and block on each component's readiness gate (helm)
aicr bundle --recipe recipe.yaml --readiness-hooks -o ./bundle

# Same, generating an Argo CD app-of-apps that gates per sync-wave
aicr bundle --recipe recipe.yaml --readiness-hooks --deployer argocd -o ./bundle
```

#### Dynamic Install-Time Values

The `--dynamic` flag declares value paths that are cluster-specific and should be provided at install time rather than baked into the bundle at build time. This enables building a single bundle that can be deployed to multiple clusters with different configurations.

Use `--dynamic` for values that genuinely vary per cluster — cluster names, subnet IDs, endpoint URLs, region-specific settings. For values that are static per bundle but differ from the recipe default (e.g., a specific driver version), use `--set` instead.

| Use case | Flag | Example |
|----------|------|---------|
| Cluster-specific value (varies per deployment) | `--dynamic` | `--dynamic alloy:clusterName` |
| Static override (same for all deployments of this bundle) | `--set` | `--set gpuoperator:driver.version=580.105.08` |

> **Attestation scope:** Dynamic values are supplied at install time and are **not covered by `--attest`**. Attestation binds the shipped bundle (defaults and stubs), not operator-provided overrides. If you need to constrain dynamic values at deploy time, use admission control or Argo sync hooks — see [Attestation Scope](#attestation-scope).

```shell
--dynamic component:path.to.field
```

**Format:** `component:path` where:
- `component` - Component name or override key (same keys as `--set`, e.g., `gpuoperator`, `alloy`)
- `path` - Dot-separated path to the value that varies per cluster

**Helm deployer behavior:**

Dynamic paths are removed from `values.yaml` and written to a separate `cluster-values.yaml` per component. The generated `deploy.sh` passes both files to Helm:

```shell
helm upgrade --install gpu-operator ... \
  -f values.yaml \
  -f cluster-values.yaml
```

Before deploying, fill in `cluster-values.yaml` with cluster-specific values.

**Argo CD deployer behavior:**

The `--deployer argocd-helm` generates a Helm chart app-of-apps where all values are overridable at install time. Static values are baked into the chart as files; dynamic overrides are merged on top at render time. Use `--dynamic` to pre-populate specific paths in the root `values.yaml`:

```shell
helm install aicr-bundle ./bundle \
  --set alloy.clusterName=prod-east \
  --set alloy.subnetName=subnet-abc123
```

**Examples:**
```shell
# Helm: declare cluster name as install-time parameter
aicr bundle -r recipe.yaml \
  --dynamic alloy:clusterName \
  -o ./bundles

# Helm: multiple dynamic paths across components
aicr bundle -r recipe.yaml \
  --dynamic alloy:clusterName \
  --dynamic alloy:subnetName \
  -o ./bundles

# Helm: combine with --set (static overrides + dynamic cluster-specific values)
aicr bundle -r recipe.yaml \
  --set gpuoperator:driver.version=580.105.08 \
  --dynamic alloy:clusterName \
  -o ./bundles

# Argo CD Helm chart: all values overridable, --dynamic pre-populates specific paths
aicr bundle -r recipe.yaml \
  --deployer argocd-helm \
  --dynamic alloy:clusterName \
  -o ./bundles

# Argo CD Helm chart: without --dynamic, still fully overridable via helm --set
aicr bundle -r recipe.yaml \
  --deployer argocd-helm \
  -o ./bundles
```

**Bundle structure with `--dynamic`** (Helm deployer):
```
bundles/
├── alloy/
│   ├── values.yaml                # Static values (clusterName removed)
│   └── cluster-values.yaml        # Dynamic values (override before deploying)
├── gpu-operator/
│   └── values.yaml                # No dynamic values, no cluster-values.yaml
├── deploy.sh                      # Passes -f cluster-values.yaml when present
└── ...
```

**Bundle structure with `--dynamic`** (Flux deployer):

The `--deployer flux` bundle uses Flux's native `spec.valuesFrom` to reference ConfigMaps containing dynamic values. Dynamic paths are removed from the inline `spec.values` and placed into a ConfigMap per component. Flux merges `valuesFrom` first, then inline values on top — since dynamic paths are stripped from inline values, the ConfigMap values take effect without conflicts.

```text
bundles/
├── gpu-operator/
│   ├── helmrelease.yaml            # HelmRelease with valuesFrom + inline values
│   └── configmap-values.yaml       # Dynamic values ConfigMap (edit before applying)
├── cert-manager/
│   └── helmrelease.yaml            # No dynamic values, no ConfigMap
├── sources/
│   └── ...
├── kustomization.yaml
└── README.md
```

Before applying the bundle to your cluster, edit each `configmap-values.yaml` with the correct per-cluster values:

```shell
# 1. Generate the bundle
aicr bundle -r recipe.yaml --deployer flux \
  --dynamic gpuoperator:driver.version \
  --repo https://github.com/my-org/gitops.git \
  -o ./bundles

# 2. Edit dynamic ConfigMaps
vim bundles/gpu-operator/configmap-values.yaml

# 3. Push to your Git repository and let Flux reconcile
git add bundles/ && git commit -m "Add AICR bundle" && git push
```

**Bundle structure with `--dynamic`** (Helmfile deployer):

The `--deployer helmfile` bundle references both `values.yaml` (static) and `cluster-values.yaml` (dynamic stubs) per release. `helmfile` merges value files in declaration order, so `cluster-values.yaml` overrides on top of the generated `values.yaml`. Edit `cluster-values.yaml` per component before `helmfile apply`:

```text
bundles/
├── helmfile.yaml                    # Release graph; per-release values: [./NNN-<component>/values.yaml, ./NNN-<component>/cluster-values.yaml]
├── 001-cert-manager/
│   ├── values.yaml                  # Generated static values
│   └── cluster-values.yaml          # Dynamic stubs (edit before apply)
├── 002-gpu-operator/
│   ├── values.yaml
│   └── cluster-values.yaml
└── README.md
```

```shell
# 1. Generate the bundle
aicr bundle -r recipe.yaml --deployer helmfile \
  --dynamic gpuoperator:driver.version \
  -o ./bundles

# 2. Edit per-cluster overrides
vim bundles/002-gpu-operator/cluster-values.yaml

# 3. Preview and apply
cd ./bundles && helmfile diff && helmfile apply
```

**Argo CD Helm chart structure with `--dynamic`:**

The `--deployer argocd-helm` bundle is itself a Helm chart whose `templates/` create per-component Argo Applications. Each application's `helm.values` block merges static values (loaded via `.Files.Get` for upstream-helm components, or read from the wrapped chart's own `values.yaml` for local-chart components) with dynamic overrides from the parent chart's `.Values`.

The same uniform `NNN-<component>/` folder layout used by `--deployer argocd` is included at the bundle root so that path-based Argo Applications (manifest-only, kustomize-wrapped, mixed `-post`) can resolve their `path:` references against the OCI-published bundle.

```text
bundles/
├── Chart.yaml                          # Parent chart metadata
├── values.yaml                         # Dynamic stubs only (per-cluster surface)
├── templates/
│   ├── aicr-stack.yaml                 # Parent Argo Application (renders all children)
│   ├── cert-manager.yaml               # Argo App, multi-source (upstream-helm)
│   ├── gpu-operator.yaml               # Argo App, multi-source
│   ├── gpu-operator-post.yaml          # Argo App, path-based (mixed -post)
│   └── nodewright-customizations.yaml  # Argo App, path-based (manifest-only)
├── static/
│   ├── cert-manager.yaml               # Static values for upstream-helm Applications
│   └── gpu-operator.yaml
├── 001-cert-manager/                   # NNN-folder content (KindUpstreamHelm)
│   └── values.yaml
├── 002-gpu-operator/                   # KindUpstreamHelm (mixed primary)
│   └── values.yaml
├── 003-gpu-operator-post/              # KindLocalHelm (mixed -post)
│   ├── Chart.yaml
│   ├── templates/
│   └── values.yaml
├── 004-nodewright-customizations/      # KindLocalHelm (manifest-only)
│   ├── Chart.yaml
│   ├── templates/
│   └── values.yaml
└── README.md
```

Manifest-only components and mixed-component raw manifests are supported by `--deployer argocd-helm` via the path-based Application shape.

**The bundle is URL-portable.** No `--repo` flag is needed (and is ignored if passed with `--deployer argocd-helm`). The same generated bundle bytes can be pushed to any chart-source backend the user chooses — Argo CD pulls from whichever URL the user supplies at install time via `helm install --set repoURL=...`. The publish location is *not* baked into the bundle artifact.

**Recommended deploy flow:**

```shell
# 1. Generate the bundle (URL-agnostic)
aicr bundle -r recipe.yaml --deployer argocd-helm --dynamic gpuoperator:driver.version -o ./bundle

# 2. Publish to your chart registry (any HTTPS-capable OCI / Helm chart repo)
helm package ./bundle -d /tmp/
helm push /tmp/aicr-bundle-*.tgz oci://<your-registry>/<path>

# 3. Install — the URL is supplied here, not at bundle time
#    `--set repoURL` is the PARENT NAMESPACE (no trailing chart name).
#    The parent Application appends `.Chart.Name` into its OCI
#    `source.repoURL`, and path-based children append it directly into
#    their rendered `source.repoURL`. For non-OCI Helm repositories, the
#    parent uses `source.chart` instead. Including the chart name in
#    --set repoURL double-appends it and the children fail to resolve.
helm install aicr-bundle oci://<your-registry>/<path>/aicr-bundle --version <chart-version> \
  -n argocd \
  --set repoURL=oci://<your-registry>/<path> \
  --set targetRevision=<chart-version>
```

The chart's `templates/aicr-stack.yaml` renders the parent Argo Application with `.Values.repoURL` and `.Values.targetRevision` substituted in. The parent Application then triggers Argo to render the chart again from the OCI source, creating the per-component child Applications with sync-wave ordering preserved. Child Applications whose source is path-based (manifest-only and mixed-component `-pre` / `-post` folders) inherit `.Values.repoURL` and append `.Chart.Name` so they pull from the same published artifact as the parent.

**Argo CD OCI prerequisites.** Path-based child Applications use Argo CD's generic OCI artifact source type (introduced in Argo CD v2.13). The argocd-helm bundle therefore requires:
- Argo CD **≥ v2.13** on the target cluster.
- A registry that serves Helm-pushed OCI artifacts through the generic OCI manifest fetch path (most modern registries — ECR, GHCR, GAR, Harbor, Artifactory, plain `oras`-compatible registries — support this).

If the recipe is pure-Helm (no manifest-only / mixed components), path-based children are not exercised and the bundle can work on Argo CD versions older than v2.13. If path-based children are present, Argo CD v2.13+ is required. See the troubleshooting section below if `Failed to load target state` appears on `aicr-stack` or any `<component>-pre` / `<component>-post` Application.

**`helm install ./bundle` from a local directory** *also* works, but with a caveat: child Applications whose source is path-based require Argo's repo-server to fetch the bundle from a remote (git or OCI) — there is no local-filesystem source type for an Argo Application. Local `helm install` is therefore end-to-end only when the recipe contains pure-Helm components. For everything else, publish first.

**Bundle structure** (with default Helm deployer):
```
bundles/
├── README.md                      # Deployment guide with ordered steps
├── deploy.sh                      # Generic install loop + name-matched blocks
├── recipe.yaml                    # Recipe used to generate bundle
├── checksums.txt                  # SHA256 checksums
├── attestation/                   # Present when --attest is used
│   ├── bundle-attestation.sigstore.json   # SLSA Build Provenance v1
│   └── aicr-attestation.sigstore.json     # Binary SLSA provenance chain
├── 001-cert-manager/              # Upstream-helm folder: no Chart.yaml
│   ├── install.sh                 # Rendered: helm upgrade --install ... --repo ${REPO}
│   ├── values.yaml
│   ├── cluster-values.yaml        # Dynamic-path overrides (operator-edited)
│   └── upstream.env               # CHART, REPO, VERSION (sourced by install.sh)
├── 002-network-operator/          # Mixed component primary (upstream-helm)
│   ├── install.sh
│   ├── values.yaml
│   ├── cluster-values.yaml
│   └── upstream.env
└── 003-network-operator-post/     # Injected -post wrapped chart (mixed component's raw manifests)
    ├── Chart.yaml                 # Local-helm folder: Chart.yaml + templates/ present
    ├── install.sh                 # Rendered: helm upgrade --install ... ./
    ├── values.yaml
    ├── cluster-values.yaml
    └── templates/
        └── nfd-network-rule.yaml
```

**Folder layout rules:**

- Folders are numbered `NNN-<component>/` (1-based, zero-padded). Numbering is regenerated on every bundle.
- Each folder is one of two **kinds**, distinguished by the presence of `Chart.yaml`:
  - **upstream-helm** — no `Chart.yaml`; `upstream.env` carries `CHART`/`REPO`/`VERSION`; `install.sh` installs the upstream chart.
  - **local-helm** — `Chart.yaml` + `templates/`; `install.sh` installs the local chart (`helm upgrade --install <name> ./`).
- **Mixed components** (Helm chart + raw manifests) emit **two adjacent folders**: a primary upstream-helm `NNN-<name>/` and an injected `(NNN+1)-<name>-post/` local-helm wrapper carrying the raw manifests. Subsequent components shift by one.
- Manifest-only components (no upstream Helm chart, just raw manifests) become a single local-helm wrapped chart.
- Kustomize-typed components run `kustomize build` at bundle time; the output becomes a single `templates/manifest.yaml` inside a local-helm folder.

**Breaking change vs. earlier releases:**

Previous releases used a flat `<component>/` layout with `manifests/` siblings and a `--deployer helm` script that branched on component kind. The new format is uniform:

- All folders carry a rendered `install.sh`. The top-level `deploy.sh` is a generic loop with no per-component branching — name-matched special-case blocks (nodewright-operator taint cleanup, kai-scheduler async timeout, orphan-CRD scan, DRA kubelet-plugin restart) live around the loop, not inside it.
- Raw manifests for mixed components now apply **post-install only**, via the injected `-post` wrapped chart. The earlier pre-apply mechanism with a CRD-race retry wrapper is gone — Helm now owns CRD ordering for mixed components natively.
- Tooling that parsed bundle paths by bare component name must account for the `NNN-` prefix.

**Argo CD bundle structure** (with `--deployer argocd`):

The argocd deployer uses the same uniform `NNN-<component>/` folder layout as `--deployer helm`. Each folder carries an `application.yaml` whose Application shape is decided by the folder kind:

- **`Chart.yaml` absent** (KindUpstreamHelm — pure Helm components): today's multi-source Application pointing at the upstream Helm repository plus a values $ref to the user's git repo. Unchanged for current users.
- **`Chart.yaml` present** (KindLocalHelm — manifest-only, kustomize-wrapped, mixed `-post`): single-source path-based Application with `source.path: NNN-<name>` against the user's repo.

The argocd deployer emits only what Argo CD's repo-server consumes: `application.yaml`, `values.yaml` (multi-source `helm.valueFiles` for upstream-helm, or local-chart Helm rendering for KindLocalHelm), and `Chart.yaml`/`templates/` for KindLocalHelm. The helm-deployer orchestration files (`install.sh`, `upstream.env`, `cluster-values.yaml`) are stripped — Argo doesn't run shell scripts or source shell env, and `--dynamic` is rejected with `--deployer argocd` (use `--deployer argocd-helm` for install-time values).

```text
bundles/
├── app-of-apps.yaml               # Parent Application (recurses *.application.yaml)
├── 001-cert-manager/              # KindUpstreamHelm — no Chart.yaml
│   ├── values.yaml                # Static Helm values (consumed via multi-source)
│   └── application.yaml           # Multi-source Application (sync-wave 0)
├── 002-gpu-operator/              # KindUpstreamHelm — primary of mixed
│   ├── values.yaml
│   └── application.yaml
├── 003-gpu-operator-post/         # KindLocalHelm — injected mixed -post
│   ├── Chart.yaml                 # Synthesized wrapper for raw manifests
│   ├── templates/                 # Rendered manifests
│   ├── values.yaml
│   └── application.yaml           # Path-based Application (sync-wave 2)
├── 004-nodewright-customizations/ # KindLocalHelm — manifest-only
│   ├── Chart.yaml
│   ├── templates/
│   ├── values.yaml
│   └── application.yaml
└── README.md
```

Manifest-only components (e.g., `nodewright-customizations`) and mixed-component raw manifests (the `-post` injection) are now deployed by `--deployer argocd`. Previously they were silently dropped. Set `--repo <user-git-or-oci>` to populate the `repoURL` on path-based Applications so Argo can resolve them.

**Day 2 Options:**

The `--workload-gate` and `--workload-selector` flags are day 2 operational options for cluster scaling operations:

- **`--workload-gate`**: Specifies a taint for nodewright-operator's runtime required feature. This ensures nodes are properly configured before workloads can schedule on them during cluster scaling. The taint is configured in the nodewright-operator Helm values file at `controllerManager.manager.env.runtimeRequiredTaint`. For more information about runtime required, see the [Nodewright documentation](https://github.com/NVIDIA/nodewright/blob/main/docs/runtime_required.md).

- **`--workload-selector`**: Specifies a label selector for nodewright-customizations to prevent nodewright from evicting running training jobs. This is critical for training workloads where job eviction would cause significant disruption. The selector is set in the Skyhook CR manifest (tuning.yaml) in the `spec.workloadSelector.matchLabels` field.

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

When generating bundles with nodewright-customizations enabled, validation warnings are displayed for missing configuration:

1. **Workload Selector Warning**: When nodewright-customizations is enabled with training intent, if `--workload-selector` is not set, a warning will be displayed:

```
Warning: nodewright-customizations is enabled but --workload-selector is not set.
This may cause nodewright to evict running training jobs. Consider setting --workload-selector to prevent eviction.
```

2. **Accelerated Selector Warning**: When nodewright-customizations is enabled with training or inference intent, if `--accelerated-node-selector` is not set, a warning will be displayed:

```
Warning: nodewright-customizations is enabled but --accelerated-node-selector is not set.
Without this selector, the customization will run on all nodes. Consider setting --accelerated-node-selector to target specific nodes.
```

**Viewing Validation Warnings:**

Validation warnings are displayed in the bundle output after successful generation:

```shell
Note:
  ⚠ Warning: nodewright-customizations is enabled but --workload-selector is not set. This may cause nodewright to evict running training jobs. Consider setting --workload-selector to prevent eviction.
  ⚠ Warning: nodewright-customizations is enabled but --accelerated-node-selector is not set. Without this selector, the customization will run on all nodes. Consider setting --accelerated-node-selector to target specific nodes.
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
  --workload-gate skyhook.nvidia.com/runtime-required=true:NoSchedule \
  --workload-selector workload-type=training \
  --workload-selector intent=training \
  --accelerated-node-selector accelerator=nvidia-h100 \
  -o ./bundles

# Generate bundle for inference workloads with accelerated selector
aicr bundle -r recipe.yaml \
  --accelerated-node-selector accelerator=nvidia-h100 \
  -o ./bundles
```

Argo CD Applications use multi-source to:
1. Pull Helm charts from upstream repositories
2. Apply values.yaml from your GitOps repository
3. Deploy additional manifests from component's manifests/ directory (if present)

#### Flux OCI Mode

When using `--deployer flux` with OCI output (`--output oci://...`), AICR generates ArtifactGenerator and ExternalArtifact CRs instead of GitRepository sources for local-chart components. This allows Flux to reconcile HelmReleases directly from OCI artifacts without a Git repository.

**Prerequisites (Flux v2.7+):**

- **source-watcher controller** must be deployed (`source.extensions.fluxcd.io`). This controller watches ArtifactGenerator CRs and creates ExternalArtifact objects.
- **ExternalArtifact=true feature gate** must be enabled on helm-controller. This allows HelmRelease CRs to reference ExternalArtifact objects via `spec.chartRef`.

Without both prerequisites, bundles generate successfully but HelmReleases will not reconcile at deploy time.

**Configuration flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--flux-oci-source-name` | `aicr-bundle` | Name of the OCIRepository CR in the target cluster. Every generated ArtifactGenerator references this name in `spec.sources[0].name`. |
| `--flux-namespace` | `flux-system` | Namespace where all Flux CRs (HelmRelease, sources, ArtifactGenerator) are placed. |

```shell
# Generate an OCI bundle with a custom OCIRepository name and namespace
aicr bundle -r recipe.yaml --deployer flux \
  --output oci://ghcr.io/my-org/aicr-bundle:v1.0.0 \
  --flux-oci-source-name my-oci-repo \
  --flux-namespace gitops
```

The generated ArtifactGenerator CRs extract per-component chart directories from the outer OCIRepository into ExternalArtifact objects. Each HelmRelease then references the ExternalArtifact via `spec.chartRef` instead of the traditional `spec.chart.spec.sourceRef` pointing at a GitRepository.

#### Bundle Attestation

> **Prerequisite:** The `--attest` flag requires a binary installed using the install script, which includes a cryptographic attestation from NVIDIA. Binaries installed via `go install` or manual download do not include this file and cannot use `--attest`.

When `--attest` is passed, the bundle command performs five steps:

1. **Verifies the binary attestation file exists** — The running `aicr` binary must have a valid SLSA provenance file (`aicr-attestation.sigstore.json`) alongside it, included by the install script from a release archive. If missing, the command fails immediately with guidance on how to install correctly.
2. **Acquires a signing credential** — in the default keyless mode this is an OIDC token (see [OIDC Token Sources](#oidc-token-sources) below); with `--signing-key` this step instead resolves the KMS key and no OIDC token is acquired (see [KMS-Backed Signing](#kms-backed-signing)).
3. **Verifies the binary's own attestation** — Cryptographically verifies the SLSA provenance binds to the running binary and was signed by NVIDIA CI. This ensures only NVIDIA-built binaries can produce attested bundles.
4. **Signs the bundle** — Creates a SLSA Build Provenance v1 in-toto statement binding the creator's identity to the bundle content (via `checksums.txt` digest) and the binary that produced it.
5. **Writes attestation files** — `attestation/bundle-attestation.sigstore.json` and `attestation/aicr-attestation.sigstore.json` are added to the bundle output.

Attestation is opt-in; bundles are unsigned by default. By default, signing uses Sigstore keyless signing (Fulcio CA + Rekor transparency log). For CI/CD environments without OIDC, pass `--signing-key` to sign with a cloud KMS key instead; see [KMS-Backed Signing](#kms-backed-signing) below. For verification, see [`aicr verify`](#aicr-verify).

**Private Sigstore infrastructure:** organizations running their own Fulcio CA or Rekor log can redirect signing with `--fulcio-url` and `--rekor-url` (both must be absolute `https://` URLs with no embedded credentials). The two are independent, so a private Fulcio can pair with the public Rekor or vice versa. Public Sigstore remains the default when the flags are omitted.

> **Verification caveat:** these flags redirect **signing** only. `aicr verify` does not yet support a custom Sigstore trust root, so bundles signed against private Fulcio/Rekor **cannot be verified with `aicr verify` today** — public Sigstore is currently the only supported verification root. Verifier support for private trust roots is tracked under [#1149](https://github.com/NVIDIA/aicr/issues/1149) / [#1153](https://github.com/NVIDIA/aicr/issues/1153).

##### OIDC Token Sources

`--attest` resolves an OIDC identity token from the first matching source, in
order:

1. `--identity-token` flag (or `COSIGN_IDENTITY_TOKEN` env) — a pre-fetched
   token. Use this when a token is obtained out of band (e.g., from a cloud
   workload-identity exchange or another `cosign` invocation). On shared
   hosts prefer the env var: a flag value is visible in `ps` and
   `/proc/<pid>/cmdline` to any user on the same machine.
2. `ACTIONS_ID_TOKEN_REQUEST_URL` + `ACTIONS_ID_TOKEN_REQUEST_TOKEN` — the
   ambient GitHub Actions OIDC credential. Used automatically in CI.
3. `--oidc-device-flow` flag (or `AICR_OIDC_DEVICE_FLOW` env) — OAuth 2.0
   Device Authorization Grant (RFC 8628). The CLI prints a verification URL
   and short code; the user enters the code in a browser **on a separate
   device**. Use on headless hosts (bastions, remote build boxes) where the
   default browser callback cannot reach the machine running `aicr`. The
   host still needs outbound network access to Sigstore's OIDC and signing
   endpoints.
4. Interactive browser flow — opens the default browser and listens on a
   random `localhost` port for the redirect. Default on workstations.

Both interactive flows time out after 5 minutes.

Attestation works with all deployers (`helm`, `argocd`, `argocd-helm`, `flux`). External `--data` files are included in `checksums.txt` and listed as resolved dependencies in the attestation.

##### KMS-Backed Signing

Keyless signing depends on an OIDC identity provider. Some CI/CD environments
(Jenkins, internal pipelines, air-gapped build hosts) have no OIDC issuer that
Sigstore trusts. For those, `--signing-key` signs the `--attest` bundle with a
cloud-KMS-backed key instead of a short-lived Fulcio certificate. The flag takes
a KMS URI; the supported schemes are:

- `awskms://`: AWS Key Management Service
- `gcpkms://`: Google Cloud KMS
- `azurekms://`: Azure Key Vault

```shell
aicr bundle --recipe recipe.yaml --attest \
  --signing-key awskms://arn:aws:kms:us-east-1:123456789012:key/abcd-1234 \
  --output ./bundles
```

`--signing-key` is mutually exclusive with the keyless-only flags
`--identity-token`, `--oidc-device-flow`, and `--fulcio-url`. Passing
`--signing-key` together with any of them is a validation error, since they
select incompatible signing modes (KMS key versus Fulcio-issued certificate).

`--rekor-url` is **not** mutually exclusive: KMS-signed bundles upload to the
public-good Rekor transparency log by default, mirroring keyless signing. Pass
`--rekor-url` to log to a private Rekor instead.

The resulting bundle uses the same Sigstore bundle format as keyless signing,
but its verification material is the signing key's public key rather than a
Fulcio certificate.

> **Verification:** verify a KMS-signed bundle with `aicr verify --key <uri>`,
> supplying the same KMS URI used to sign (or a local PEM public-key file). See
> the [`aicr verify`](#aicr-verify) flags below. cosign's public-key path
> (`cosign verify-blob-attestation --key <same-kms-uri> ...`) also works, since
> the bundle uses the standard Sigstore bundle format.

HashiCorp Vault (`hashivault://`) is not supported: its client libraries are MPL-2.0 licensed, which is incompatible with this project's license policy. This is a deliberate, ongoing exclusion rather than a not-yet-implemented feature at this time.

##### Attestation Scope

Attestation binds the **shipped bundle** — defaults, dynamic-value stubs, and any external `--data` files copied into the bundle. It does **not** bind install-time values supplied via `helm --set`, a user-provided `-f extra.yaml`, or Argo `Application.spec.source.helm.parameters`. That boundary is intentional: dynamic values are the operator's domain by design.

If you need to enforce specific install-time values (e.g., pinning `driver.version`), that is a **policy concern**, not an attestation one. Use admission control (Kyverno, Gatekeeper) or Argo sync hooks to reject deployments that violate the policy. `aicr verify` checks bundle integrity and provenance; it does not evaluate install-time value constraints.

#### Deploying a bundle

```shell
# Navigate to bundle
cd bundles

# Review root README and a component's values
cat README.md
cat 001-gpu-operator/values.yaml

# Verify integrity
sha256sum -c checksums.txt

# Deploy to cluster
chmod +x deploy.sh && ./deploy.sh
```

> **Note:** `deploy.sh` is a convenience script — not the only deployment path. Each `NNN-<component>/` folder contains a rendered `install.sh` that runs the exact `helm upgrade --install` command for manual or pipeline-driven deployment. For teardown, bundles delegate to the deployer-native uninstall path (see [Bundle Uninstall](#bundle-uninstall) below).

#### Deploy Script Behavior (`deploy.sh`)

The deploy script installs components in the order specified by `deploymentOrder` in the recipe.

**Flags:**

| Flag | Description |
|------|-------------|
| `--no-wait` | Skip Helm chart-level wait (`helm --wait`) where AICR uses it. Keeps `--timeout` for hooks. |
| `--best-effort` | Continue past individual component failures instead of exiting |
| `--retries N` | Retry failed helm/kubectl operations N times with exponential backoff (default: 5) |

Unknown flags are rejected with an error to catch typos (e.g., `--bes-effort` or `--retires N`).

> **Note on install completion vs. workload readiness.** By default, `deploy.sh` waits on Helm chart readiness where AICR uses `helm --wait`. Some components are intentionally installed without Helm chart-level waiting, and the script does not wait for bundle-level workload readiness such as Nodewright node tuning, GPU operator operand rollout (driver, toolkit, device-plugin DaemonSets), or NVIDIA DRA kubelet plugin registration. Those continue asynchronously after the script exits. When `--best-effort` is used, the script may also finish with non-fatal component failures; check warning lines and logs before treating the install/apply pass as fully successful. `--no-wait` only skips the Helm chart-level wait where AICR uses it; it does not affect bundle-level convergence.

**Retry behavior:**

The deploy script retries failed `helm upgrade --install` and `kubectl apply` operations with exponential backoff. By default, each operation is retried up to 5 times (6 total attempts). The backoff delay increases quadratically: 5s, 20s, 45s, 80s, 120s (capped) between retries.

Use `--retries 0` to disable retries (fail-fast behavior). When `--best-effort` is also set, retries are exhausted first before falling through to best-effort handling.

**Pre-install manifests and CRD ordering:**

Some components have pre-install manifests (CRDs, namespaces, ConfigMaps) that must exist before `helm install`. The script applies these with `kubectl apply` before the Helm install. On first deploy, CRD-dependent resources may produce `no matches for kind` warnings because the CRD hasn't been registered yet — these warnings are suppressed. All other `kubectl apply` errors (auth failures, webhook denials, bad manifests) fail the script immediately.

After `helm install`, the same manifests are re-applied as post-install to ensure CRD-dependent resources are created.

**Async components:**

Components that use operator patterns with custom resources that reconcile asynchronously (e.g., `kai-scheduler`) are installed without `--wait` to avoid Helm timing out on CR readiness.

##### DRA kubelet plugin registration

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

#### Bundle Uninstall

AICR bundles do **not** ship a generated `undeploy.sh`. Teardown is delegated
to the deployer-native uninstall path; AICR's role ends at design-time
generation. Pick the walkthrough that matches the deployer used to generate
your bundle.

##### helm

Uninstall releases in **reverse** deployment order — the same order the
generated `README.md` lists under `## Uninstall`:

```bash
# For each NNN-<component>/ folder in descending order:
helm uninstall <release> -n <namespace>
```

Helm intentionally does not delete CRDs (charts that declare them under
`crds/` are left in place) or PVCs (StatefulSet-managed volumes are
preserved). Remove them only when you are sure no other release depends on
them:

```bash
# CRDs — review first; deletion cascades to every custom resource cluster-wide
kubectl get crd -o name | grep -E '<component-prefix>'
kubectl delete crd <name>

# PVCs in a single namespace
kubectl -n <namespace> delete pvc --all

# Namespace
kubectl delete namespace <namespace>
```

If a release is stuck in `pending-install` or `pending-upgrade` (interrupted
deploy), retry with `--no-hooks`:

```bash
helm uninstall <release> -n <namespace> --no-hooks
```

See [Helm 3 uninstall docs](https://helm.sh/docs/helm/helm_uninstall/) for
the full flag reference.

##### argocd

Delete the parent `Application` that owns the bundle's child Applications
(app-of-apps). AICR does **not** set the
`resources-finalizer.argocd.argoproj.io` finalizer on generated
Applications, so a plain `kubectl delete` removes only the Application CR
and leaves the managed resources running. Use one of the cascade-aware
flows instead:

```bash
# Argo CD CLI — cascade is the default; foreground waits for resources
argocd app delete <bundle-parent-app> --cascade --propagation-policy foreground
```

If you can only use `kubectl`, add the finalizer first so the controller
performs the cascade for you:

```bash
kubectl -n argocd patch application <bundle-parent-app> --type=merge \
  -p '{"metadata":{"finalizers":["resources-finalizer.argocd.argoproj.io"]}}'
kubectl -n argocd delete application <bundle-parent-app>
```

The CRD and PVC notes from the **helm** walkthrough above still apply:
Argo CD does not run `helm uninstall` for Helm-templated children — it
renders manifests with `helm template` and prunes the rendered resources
directly — so CRDs declared under `crds/` and PVCs from StatefulSets are
not deleted by the cascade. Remove them by hand if needed.

See [ArgoCD app deletion docs](https://argo-cd.readthedocs.io/en/stable/user-guide/app_deletion/)
for finalizer behavior, cascade modes, and selective deletion.

##### argocd-helm

Same path as plain `argocd`: Argo CD uses Helm only to render charts into
Kubernetes manifests (via `helm template`) and then manages those resources
itself. Deleting the Application with cascade enabled prunes the resources
Argo CD tracks; it does **not** run `helm uninstall`, and `helm ls` will
not show the bundle's releases.

```bash
argocd app delete <bundle-parent-app> --cascade --propagation-policy foreground
```

The kubectl + finalizer-patch fallback from the **argocd** walkthrough
applies here too, and CRD / PVC cleanup follows the **helm** notes above.

See the [Argo CD Helm user guide](https://argo-cd.readthedocs.io/en/stable/user-guide/helm/)
and the [Argo CD FAQ entry on `helm ls`](https://argo-cd.readthedocs.io/en/stable/faq/#after-deploying-my-helm-application-with-argo-cd-i-cannot-see-it-with-helm-ls-and-other-helm-commands)
for why Helm CLI tools don't see Argo-deployed releases.

##### flux

AICR's `flux` bundle emits one `HelmRelease` per component (plus the
`HelmRepository` / `OCIRepository` source objects). Deleting each
`HelmRelease` from the cluster triggers `helm-controller` to run
`helm uninstall` for the underlying release, honoring the chart's
`spec.uninstall` settings (`disableHooks`, `keepHistory`, etc.):

```bash
kubectl -n <namespace> delete helmrelease <release>
```

Delete the bundle's source objects (`HelmRepository` / `OCIRepository`)
after the releases are gone. The CRD / PVC notes from the **helm**
walkthrough above still apply — `helm-controller` follows the same
non-destructive defaults.

See the [Flux helm-controller uninstall reference](https://fluxcd.io/flux/components/helm/helmreleases/#uninstall-configuration)
for `spec.uninstall` field semantics.

##### helmfile

AICR's `helmfile` bundle emits a single `helmfile.yaml` release graph.
The upstream `helmfile` CLI handles teardown:

```bash
helmfile -f helmfile.yaml destroy
```

CRD / PVC cleanup follows the **helm** walkthrough above. See the
[Helmfile `destroy` documentation](https://github.com/helmfile/helmfile/blob/main/docs/index.md)
for flags and behavior.

---

### aicr mirror list

Discover container images and Helm charts referenced by a recipe for air-gapped
mirroring. Renders each component's Helm chart with recipe-resolved values and
scans referenced manifests to produce a deduplicated image and chart list. When
the recipe was resolved with `--data <dir>`, both values and manifests are read
through the overlay so overlay-shadowed paths take precedence over embedded.

For an end-to-end walkthrough covering Hauler and Zarf workflows, see
[Air-Gapped Mirroring](air-gap-mirror.md).

**Synopsis:**
```shell
aicr mirror list [flags]
```

**Flags:**
| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--recipe` | `-r` | string | | Path/URI to a previously generated recipe. Supports: file paths, HTTP/HTTPS URLs, or ConfigMap URIs (`cm://namespace/name`). |
| `--service` | | string | | Cloud service (e.g., `eks`, `gke`, `aks`). Alternative to `--recipe`. |
| `--accelerator` | | string | | GPU accelerator (e.g., `h100`, `gb200`). Alternative to `--recipe`. |
| `--intent` | | string | | Workload intent (`training` or `inference`). Alternative to `--recipe`. |
| `--os` | | string | | Operating system (e.g., `ubuntu`). Alternative to `--recipe`. |
| `--platform` | | string | | Optional platform specialization (e.g., `kubeflow`). |
| `--set` | | string[] | | Override values that affect image discovery (format: `component:path.to.field=value`). Repeatable. |
| `--data` | | string | | External data directory to overlay on embedded data. Overlay-provided component values and manifests both feed image discovery (see [External Data](#external-data-directory)). |
| `--format` | `-f` | string | `yaml` | Output format: `yaml`, `json`, `hauler`, `zarf` |
| `--output` | `-o` | string | stdout | Output file path |

**Examples:**

```shell
# List images from a recipe file (YAML to stdout)
aicr mirror list --recipe recipe.yaml

# Resolve recipe from query parameters
aicr mirror list --service eks --accelerator h100 --intent training --os ubuntu

# Generate Hauler manifest
aicr mirror list --recipe recipe.yaml --format hauler --output hauler-manifest.yaml

# Generate Zarf package config
aicr mirror list --recipe recipe.yaml --format zarf --output zarf.yaml

# Override a value that affects image discovery
aicr mirror list --recipe recipe.yaml --set gpuoperator:driver.enabled=false
```

---

### aicr verify

Verify the integrity and attestation chain of a bundle. By default verification is offline and makes no network calls. The one exception is `--key` with a KMS URI, which reaches the KMS provider to fetch the public key (see the `--key` network behavior note below).

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
| `--key` | string | | Verify a key-signed bundle attestation against a KMS key URI (`awskms://` \| `gcpkms://` \| `azurekms://`) or a local PEM public-key file. This is the counterpart to `bundle --signing-key`. It coexists with `--certificate-identity-regexp`, which pins the binary attestation; the two verify different attestations. |
| `--format` | string | `text` | Output format: `text` or `json`. |

#### Trust Levels

| Level | Name | Criteria |
|-------|------|----------|
| 4 | `verified` | Full chain: checksums + bundle attestation + binary attestation pinned to NVIDIA CI |
| 3 | `attested` | Chain verified but binary attestation missing or external data (`--data`) was used |
| 2 | `unverified` | Checksums valid, `--attest` was not used when creating the bundle |
| 1 | `unknown` | Missing or invalid checksums |

#### Verification steps

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

# Sign a bundle with a KMS key, then verify it with the same key
aicr bundle -r recipe.yaml --attest --signing-key gcpkms://projects/p/locations/l/keyRings/r/cryptoKeys/k -o ./bundles
aicr verify ./bundles/<bundle-dir> --key gcpkms://projects/p/locations/l/keyRings/r/cryptoKeys/k

# Or verify against an exported PEM public key (no KMS access needed)
aicr verify ./bundles/<bundle-dir> --key ./bundle-signer.pub
```

> **`--key` network behavior:** Resolving a **KMS URI** (`awskms://`, `gcpkms://`, `azurekms://`) makes network calls to the KMS provider to fetch the public key, so credentials for that provider must be available in the environment. A **local PEM** public-key file is read from disk with no provider calls; export it once with `cosign public-key --key <kms-uri>` (or your provider's console) and verify anywhere.
>
> Resolving the key is only part of verification: by default the bundle's Rekor transparency-log entry is also checked. Its inclusion proof is embedded in the bundle, so no live Rekor call is made, but the check needs the Sigstore trusted root. That root is loaded from the local cache when present and otherwise fetched over the network, so run `aicr trust update` once to pre-populate it. A local PEM key therefore makes verification fully offline only when the trusted-root cache is already warm. Verification that drops the transparency-log requirement entirely, for true air-gapped use, is tracked in [#1154](https://github.com/NVIDIA/aicr/issues/1154).
>
> **Stale root:** If verification fails with certificate chain errors, run `aicr trust update` to refresh the Sigstore trusted root.

---

### aicr evidence digest

Print the canonical sha256 of a resolved recipe — byte-for-byte the same value recorded in `predicate.recipe.digest` by `aicr validate --emit-attestation`. The input is resolved through the same recipe builder path as `aicr validate -r`, so overlays and mixins are hydrated before hashing.

Use this to detect drift between a signed evidence pointer and the current recipe on a PR branch without pulling the OCI artifact.

**Synopsis:**

```shell
aicr evidence digest -r <recipe-or-overlay> [flags]
```

**Flags:**

| Flag | Alias | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--recipe` | `-r` | string | | Path/URI to a recipe or overlay file (file, HTTP/HTTPS, or `cm://namespace/name`). Required. |
| `--kubeconfig` | | string | | Kubeconfig path; consulted only when the input is a `cm://` URI. |

**Exit codes:**

| Code | Meaning |
|------|---------|
| 0 | Digest printed to stdout. |
| non-zero | Input could not be loaded, hydrated, or canonicalized. |

**Examples:**

```shell
# Print the digest of a hydrated overlay.
aicr evidence digest -r recipes/overlays/h100-eks-ubuntu-training.yaml

# CI drift gate: compare the digest pinned in a signed evidence bundle
# against the recipe currently on the PR branch.
signed=$(aicr evidence verify recipes/evidence/<slug>.yaml --format json \
         | jq -r .predicate.recipe.digest)
current=$(aicr evidence digest -r recipes/overlays/<file>.yaml)
[[ "$signed" == "$current" ]] || echo "evidence is stale"
```

---

### aicr evidence publish

Sign, push, and write the pointer for a recipe-evidence v1 bundle that was produced earlier by `aicr validate --emit-attestation` **without** `--push` (which leaves an unsigned bundle on disk).

This decouples the cluster-bound validate step from the Fulcio/Rekor-bound signing step so they can run on different networks: validation must run where the cluster is reachable (often a corporate VPN), but keyless signing must reach `fulcio.sigstore.dev` + `rekor.sigstore.dev`, which corporate networks frequently block. Run `validate --emit-attestation` on the VPN, then `evidence publish` from a host with Sigstore egress (CI runner, jump box, hotspot).

The signed artifact is content-addressable, so the result is byte-for-byte identical to the one-shot `validate --emit-attestation --push` output regardless of which host ran which leg — the predicate (including its baked-in `attestedAt` timestamp) is signed verbatim from the bundle on disk.

**Synopsis:**

```shell
aicr evidence publish <bundle-dir> --push <ref> [flags]
```

The positional `<bundle-dir>` is either the directory `--emit-attestation` wrote (holds `summary-bundle/` and receives `pointer.yaml`) or the `summary-bundle/` directory itself.

**Flags:**

| Flag | Alias | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--push` | | string | | OCI registry reference to push the signed summary bundle to. Required. Triggers Sigstore keyless signing via the precedence chain documented under `--identity-token`. Omit the tag and aicr derives a unique per-recipe one (`<recipe-slug>-<short-fingerprint>`); pass an explicit tag to override. See [`aicr validate --push`](#aicr-validate). |
| `--identity-token` | | string | | Pre-fetched OIDC identity token for keyless signing. Skips ambient/browser/device-code flows. Reads `COSIGN_IDENTITY_TOKEN` from env. Same precedence chain as `aicr validate --push`. |
| `--oidc-device-flow` | | bool | `false` | Use the OAuth 2.0 device authorization grant for OIDC instead of opening a browser callback. Reads `AICR_OIDC_DEVICE_FLOW`. Useful on headless hosts. |
| `--plain-http` | | bool | `false` | Use HTTP instead of HTTPS when pushing the OCI artifact (local-registry tests). |
| `--insecure-tls` | | bool | `false` | Skip TLS verification when pushing the OCI artifact (self-signed registries). |

**Exit codes:**

| Code | Meaning |
|------|---------|
| 0 | Bundle signed, pushed, and `pointer.yaml` written. |
| non-zero | Bundle could not be loaded, signed, or pushed. |

**Examples:**

```shell
# On VPN: produce an unsigned bundle from a passing validation.
aicr validate -r recipe.yaml -s snapshot.yaml --emit-attestation ./out

# Off VPN: sign, push, and write the pointer. Omit the tag and aicr derives
# a unique per-recipe one (<recipe-slug>-<fingerprint>).
aicr evidence publish ./out --push ghcr.io/myorg/aicr-evidence
```

---

### aicr evidence verify

Verify a recipe-evidence v1 bundle produced by `aicr validate --emit-attestation`. When the bundle carries a signature, verifies it against the Sigstore trusted root and extracts the cryptographically anchored predicate. Recomputes every file's sha256 against `manifest.json` (which the predicate's `manifest.digest` field anchors), and surfaces the predicate's fingerprint, phase counts, and BOM info.

Inline constraint replay is reserved for a follow-up PR.

**Synopsis:**
```shell
aicr evidence verify <input> [flags]
```

The positional argument is auto-detected as one of:

* `recipes/evidence/<recipe>.yaml` — **pointer file (preferred)**. The verifier pulls **by digest** — `registry/repo@<bundle.digest>`, with the registry/repo taken from `bundle.oci` and the digest as the pin — so it fetches the exact attested bytes even if the `bundle.oci` tag has since been moved to a different artifact. This is the input to use in nearly all cases.
* `ghcr.io/<owner>/aicr-evidence@sha256:...` or `oci://...@sha256:...` — a **digest-pinned** OCI reference. A tag-only ref (such as the `bundle.oci` value copied from a pointer, e.g. `...aicr-evidence:h100-eks-ubuntu-training-3f9a1c2b4d5e`) is refused by default because tags are registry-rewritable; see `--allow-unpinned-tag`.
* `./out/summary-bundle/` (or a parent containing it) — unpacked directory.

> **Do not extract `bundle.oci` from a pointer and pass it to `verify` as a raw OCI argument.** As a raw ref it carries no companion `bundle.digest`, so a tag-only ref is refused (tags are registry-rewritable). Pass the pointer file itself — the verifier reads `bundle.digest` from it and pulls `registry/repo@<digest>`, ignoring the tag. If you must verify a raw OCI ref, use the digest form (`...@sha256:<hex>`), not the tag.

**Flags:**

| Flag | Alias | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--output` | `-o` | string | | Write output to this file. When empty, output goes to stdout. |
| `--format` | `-t` | string | `text` | Output format: `text` (Markdown) or `json`. Applies regardless of destination. |
| `--expected-issuer` | | string | | Pin the OIDC issuer URL on the signing certificate. Empty allows any issuer. |
| `--expected-identity-regexp` | | string | | Pin the signer's `SubjectAlternativeName` via regex. Empty allows any identity. |
| `--bundle` | | string | | OCI reference override for a local-only pointer that carries no `bundle.oci`. Use a digest-pinned ref (`...@sha256:<hex>`); a tag-only ref is refused unless `--allow-unpinned-tag` is set. |
| `--registry-plain-http` | | bool | `false` | Use HTTP for registry traffic (local-registry tests only). |
| `--registry-insecure-tls` | | bool | `false` | Skip TLS verification for the registry (self-signed certificates). |
| `--allow-unpinned-tag` | | bool | `false` | Accept tag-only OCI references. By default the verifier refuses unpinned refs because tags are registry-rewritable; opt in only for one-off debugging. Pointer-driven flows ignore this flag when the pointer carries a `sha256:` digest. |

**Exit codes:**

| Code | Meaning |
|------|---------|
| 0 | Bundle valid; every check passed. |
| 2 | Bundle invalid (signature, integrity, or predicate failure), OR recorded validator results show failures. |

The JSON/Markdown output's `exit` field (and `VerifyResult.Exit` from the library API) still distinguishes the two non-zero cases as `1` (recorded phase failures) vs `2` (bundle invalid). Shell consumers can branch via `jq '.exit'` on `--format json` output.

**Examples:**
```shell
# Verify the pointer that a contributor committed alongside their recipe change.
aicr evidence verify recipes/evidence/h100-eks-ubuntu-training.yaml

# Verify a pushed OCI bundle directly (no repo checkout required).
aicr evidence verify ghcr.io/myorg/aicr-evidence@sha256:abc...

# Verify a local bundle directory (contributor self-debug before push).
aicr evidence verify ./out/summary-bundle

# Pin the expected OIDC signer.
aicr evidence verify recipes/evidence/<recipe>.yaml \
  --expected-issuer https://token.actions.githubusercontent.com \
  --expected-identity-regexp '^https://github\.com/myorg/.*$'

# CI pipelines: JSON output.
aicr evidence verify recipes/evidence/<recipe>.yaml -o result.json -t json
```

See [`demos/evidence.md`](https://github.com/NVIDIA/aicr/blob/main/demos/evidence.md) for a full producer-and-consumer walkthrough.

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

### aicr skill

Generate an AI agent skill file that teaches a coding agent how to use the AICR CLI. The generated file is written to the agent's standard configuration directory.

**Synopsis:**

```shell
aicr skill --agent <agent> [flags]
```

**Flags:**

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--agent` | string | (required) | Target coding agent: `claude-code`, `codex` |
| `--stdout` | bool | false | Print to stdout instead of writing to disk |
| `--force` | bool | false | Overwrite an existing skill file without prompting |

**Install Locations:**

| Agent | Path |
|-------|------|
| `claude-code` | `~/.claude/skills/aicr/SKILL.md` |
| `codex` | `~/.codex/skills/aicr/SKILL.md` |

**Behavior:**
- Without `--stdout`: writes the file to disk and prints the path
- With `--stdout`: prints the generated content to stdout
- If the target file already exists: prompts `overwrite? [y/N]` when stdin is a terminal; aborts on non-interactive stdin unless `--force` is set
- Creates parent directories as needed

**Examples:**

```shell
# Install Claude Code skill file
aicr skill --agent claude-code

# Install Codex skill file
aicr skill --agent codex

# Overwrite an existing skill file without prompting (e.g., in CI)
aicr skill --agent claude-code --force

# Print to stdout (e.g., for review before installing)
aicr skill --agent claude-code --stdout
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
| `AICR_LOG_LEVEL` | Logging level: debug, info, warn, error | info |
| `AICR_LOG_PREFIX` | Override the CLI logger prefix | `cli` |
| `AICR_REQUESTS` | Default for `aicr snapshot --requests`. Comma-separated `name=quantity` pairs (e.g. `cpu=500m,memory=1Gi,ephemeral-storage=1Gi`). Unspecified resources keep the built-in privileged or restricted defaults. | unset |
| `AICR_LIMITS` | Default for `aicr snapshot --limits`. Comma-separated `name=quantity` pairs (e.g. `cpu=1,memory=2Gi,ephemeral-storage=2Gi`). Unspecified resources keep the built-in defaults. With `--require-gpu`, the default `nvidia.com/gpu=1` is applied only when this list does not already contain that key — explicit `nvidia.com/gpu=N` wins. | unset |
| `AICR_CRITERIA_STRICT` | When set to `1` / `true` / `yes` / `on`, equivalent to `--criteria-strict` on every `aicr recipe` invocation: rejects criteria values not in the embedded OSS catalog regardless of `--data` contributions. Intended for OSS CI gates; `make qualify` exports it automatically for the unit-test step. | unset |
| `NO_COLOR` | Suppress ANSI color codes in CLI logger output (de-facto standard, see [no-color.org](https://no-color.org/)) | unset |

## Exit Codes

| Code | Meaning |
|------|---------|
| 0 | Success |
| 1 | General error (unclassified) |
| 2 | Invalid input (bad arguments, validation failure) |
| 3 | Not found (requested resource does not exist) |
| 4 | Unauthorized (authentication or authorization failure) |
| 5 | Timeout (operation exceeded time limit) |
| 6 | Unavailable (service temporarily unavailable) |
| 7 | Rate limited (client exceeded rate limit) |
| 8 | Internal error (unexpected failure) |

## Common Usage Patterns

### Quick Recipe Generation

```shell
aicr recipe --os ubuntu --accelerator h100 | jq '.componentRefs[]'
```

### Save All Steps

```shell
aicr snapshot -o snapshot.yaml
aicr recipe -s snapshot.yaml --intent training -o recipe.yaml
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

# List available flags
aicr bundle --help

# Run with debug
aicr --debug bundle -r recipe.yaml
```

**"helm CLI not found on PATH" with `--vendor-charts`** — the bundle-time vendoring path shells out to `helm pull`. Install Helm v3 or later (`brew install helm` / package manager) and re-run, or drop `--vendor-charts` for a registry-referencing bundle. See [Vendoring Charts for Air-Gap](#vendoring-charts-for-air-gap).

**"failed to load manifest \<path\> for component \<name\>"** — the recipe references a manifest path that does not exist in the current AICR binary's embedded data. This usually means the recipe was generated by an older binary and a referenced manifest has since been removed or relocated. Regenerate the recipe with the current binary (`aicr recipe ...`) and re-bundle. AICR recipes are a point-in-time artifact of the binary that produced them; bundling a stale recipe against a newer binary is not supported.

**`--deployer argocd-helm`: `aicr-stack` or `<component>-pre` / `<component>-post` Application stuck at `Unknown` sync status / "Failed to load target state: ... `<registry>/<path>:<tag>: not found`"** — Argo CD cannot resolve the OCI artifact the parent or path-based child Application points at. Common causes:

1. **Chart name doubled in `--set repoURL`.** Under the current contract, `--set repoURL` carries the **parent namespace only** (e.g., `oci://ghcr.io/myorg`). The parent Application appends `.Chart.Name` into its OCI `source.repoURL`, and path-based children append it directly into their rendered `source.repoURL`. For non-OCI Helm repositories, the parent uses `source.chart` instead. Passing `--set repoURL=oci://ghcr.io/myorg/aicr-bundle` produces a double-suffixed reference (`.../aicr-bundle/aicr-bundle:<tag>`) that does not exist. Drop the trailing chart segment.
2. **Argo CD older than v2.13.** Path-based children rely on Argo CD's generic OCI artifact source type, added in v2.13. Older Argo treats the source as Git and fails to resolve. Check with `kubectl -n argocd get deploy argocd-repo-server -o jsonpath='{.spec.template.spec.containers[0].image}'`. Upgrade Argo, or use `--deployer helm` if Argo upgrade is not an option.
3. **Tag missing from the registry.** Verify the published artifact exists at the exact tag the parent expects: `oras manifest fetch <registry>/<path>/<chart>:<tag>`. If `aicr bundle` is invoked without a tag (`oci://<registry>/<path>/<chart>` with no `:<tag>` suffix), the CLI version is used as the default — make sure `--set targetRevision=<chart-version>` at install time matches.
4. **Private registry credentials keyed to a different source URL.** Problem: Argo CD matches repository credentials against the source URL it dereferences.

   Failure case: For this deployer, path-based OCI Applications render full `oci://<registry>/<path>/<chart>` source URLs even though `--set repoURL` is the parent namespace. A Secret keyed only to `<registry>/<path>` or to a scheme-less Helm-OCI URL may let local `helm install` succeed while Argo's repo-server still returns 401.

   Solution: Key the Argo CD repository credential to the rendered `oci://.../<chart>` prefix, or to a broader matching prefix allowed by your cluster's credential policy, such as `oci://<registry>/` or `oci://<registry>/<path>/`.

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
- [Data Architecture](../contributor/recipe.md) - Recipe data system details
