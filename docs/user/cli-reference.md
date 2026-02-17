# CLI Reference

Complete reference for the `eidos` command-line interface.

## Overview

Eidos provides a four-step workflow for optimizing GPU infrastructure:

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

Eidos supports three logging modes:

1. **CLI Mode (default)**: Minimal user-friendly output
   - Just message text without timestamps or metadata
   - Error messages display in red (ANSI color)
   - Example: `Snapshot captured successfully`

2. **Text Mode (`--debug`)**: Debug output with full metadata
   - Key=value format with time, level, source location
   - Example: `time=2025-01-06T10:30:00.123Z level=INFO module=eidos version=v1.0.0 msg="snapshot started"`

3. **JSON Mode (`--log-json`)**: Structured JSON for automation
   - Machine-readable format for log aggregation
   - Example: `{"time":"2025-01-06T10:30:00.123Z","level":"INFO","msg":"snapshot started"}`

**Examples:**
```shell
# Default: Clean CLI output
eidos snapshot

# Debug mode: Full metadata
eidos --debug snapshot

# JSON mode: Structured logs
eidos --log-json snapshot

# Combine with other flags
eidos --debug --output system.yaml snapshot
```

## Commands

### eidos snapshot

Capture comprehensive system configuration including OS, GPU, Kubernetes, and SystemD settings.

**Synopsis:**
```shell
eidos snapshot [flags]
```

**Flags:**
| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--output` | `-o` | string | stdout | Output destination: file path, ConfigMap URI (cm://namespace/name), or stdout |
| `--format` | `-f` | string | yaml | Output format: json, yaml, table |
| `--kubeconfig` | `-k` | string | ~/.kube/config | Path to kubeconfig file (overrides KUBECONFIG env) |
| `--deploy-agent` | | bool | false | Deploy Kubernetes Job to capture snapshot on cluster nodes |
| `--namespace` | `-n` | string | gpu-operator | Kubernetes namespace for agent deployment |
| `--image` | | string | ghcr.io/nvidia/eidos-validator:latest | Container image for agent Job |
| `--job-name` | | string | eidos | Name for the agent Job |
| `--service-account-name` | | string | eidos | ServiceAccount name for agent Job |
| `--node-selector` | | string[] | | Node selector for agent scheduling (key=value, repeatable) |
| `--toleration` | | string[] | all taints | Tolerations for agent scheduling (key=value:effect, repeatable). **Default: all taints tolerated** (uses `operator: Exists`). Only specify to restrict which taints are tolerated. |
| `--timeout` | | duration | 5m | Timeout for agent Job completion |
| `--cleanup` | | bool | true | Delete Job and RBAC resources on completion. Use `--cleanup=false` to keep resources for debugging. |
| `--privileged` | | bool | true | Run agent in privileged mode (required for GPU/SystemD collectors). Set to false for PSS-restricted namespaces. |
| `--template` | | string | | Path to Go template file for custom output formatting (requires YAML format) |

**Output Destinations:**
- **stdout**: Default when no `-o` flag specified
- **File**: Local file path (`/path/to/snapshot.yaml`)
- **ConfigMap**: Kubernetes ConfigMap URI (`cm://namespace/configmap-name`)

**What it captures:**
- **SystemD Services**: containerd, docker, kubelet configurations
- **OS Configuration**: grub, kmod, sysctl, release info
- **Kubernetes**: server version, images, ClusterPolicy
- **GPU**: driver version, CUDA, MIG settings, hardware info

**Examples:**

```shell
# Output to stdout (YAML)
eidos snapshot

# Save to file (JSON)
eidos snapshot --output system.json --format json

# Save to Kubernetes ConfigMap (requires cluster access)
eidos snapshot --output cm://gpu-operator/eidos-snapshot

# Debug mode
eidos --debug snapshot

# Table format (human-readable)
eidos snapshot --format table

# Agent deployment mode: Deploy Job to capture snapshot on cluster node
eidos snapshot --deploy-agent

# Agent deployment with custom kubeconfig
eidos snapshot --deploy-agent --kubeconfig ~/.kube/prod-cluster

# Agent deployment targeting specific nodes
eidos snapshot --deploy-agent \
  --namespace gpu-operator \
  --node-selector accelerator=nvidia-h100 \
  --node-selector zone=us-west1-a

# Agent deployment with tolerations for tainted nodes
# (By default all taints are tolerated - only needed to restrict tolerations)
eidos snapshot --deploy-agent \
  --toleration nvidia.com/gpu=present:NoSchedule

# Agent deployment: Full example with all options
eidos snapshot --deploy-agent \
  --kubeconfig ~/.kube/config \
  --namespace gpu-operator \
  --image ghcr.io/nvidia/eidos:v0.8.0 \
  --job-name snapshot-gpu-nodes \
  --service-account-name eidos \
  --node-selector accelerator=nvidia-h100 \
  --toleration nvidia.com/gpu:NoSchedule \
  --timeout 10m \
  --output cm://gpu-operator/eidos-snapshot \
  --cleanup

# Custom template formatting
eidos snapshot --template examples/templates/snapshot-template.md.tmpl

# Template with file output
eidos snapshot --template examples/templates/snapshot-template.md.tmpl --output report.md

# Agent deployment with custom template
eidos snapshot --deploy-agent \
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
  .Type         # Measurement type (K8s, GPU, OS, SystemD)
  .Subtypes     # Array of Subtype objects
    .Name       # Subtype name (e.g., "server", "smi", "grub")
    .Data       # Map of readings (key -> Reading with .String method)
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

When `--deploy-agent` is specified, Eidos deploys a Kubernetes Job to capture the snapshot instead of running locally:

1. **Deploys RBAC**: ServiceAccount, Role, RoleBinding, ClusterRole, ClusterRoleBinding
2. **Creates Job**: Runs `eidos snapshot` as a container on the target node
3. **Waits for completion**: Monitors Job status with configurable timeout
4. **Retrieves snapshot**: Reads snapshot from ConfigMap after Job completes
5. **Writes output**: Saves snapshot to specified output destination
6. **Cleanup**: Deletes Job and RBAC resources (use `--cleanup=false` to keep for debugging)

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
  name: eidos-snapshot
  namespace: gpu-operator
  labels:
    app.kubernetes.io/name: eidos
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
apiVersion: eidos.nvidia.com/v1alpha1
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

### eidos recipe

Generate optimized configuration recipes from query parameters or captured snapshots.

**Synopsis:**
```shell
eidos recipe [flags]
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
apiVersion: eidos.nvidia.com/v1alpha1
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
eidos recipe --criteria criteria.yaml

# Override service from file
eidos recipe --criteria criteria.yaml --service gke

# Save output to file
eidos recipe -c criteria.yaml -o recipe.yaml
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
eidos recipe --os ubuntu --service eks --accelerator h100

# Training workload with multiple GPU nodes
eidos recipe \
  --service eks \
  --accelerator gb200 \
  --intent training \
  --os ubuntu \
  --nodes 8 \
  --format yaml

# Kubeflow training workload
eidos recipe \
  --service eks \
  --accelerator h100 \
  --intent training \
  --os ubuntu \
  --platform kubeflow

# Save to file (--gpu is an alias for --accelerator)
eidos recipe --os ubuntu --gpu h100 --output recipe.yaml
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
eidos recipe --snapshot system.yaml --intent training

# From ConfigMap (requires cluster access)
eidos recipe --snapshot cm://gpu-operator/eidos-snapshot --intent training

# From ConfigMap with custom kubeconfig
eidos recipe \
  --snapshot cm://gpu-operator/eidos-snapshot \
  --kubeconfig ~/.kube/prod-cluster \
  --intent training

# Output to ConfigMap
eidos recipe -s system.yaml -o cm://gpu-operator/eidos-recipe

# Chain snapshot → recipe with ConfigMaps
eidos snapshot -o cm://default/snapshot
eidos recipe -s cm://default/snapshot -o cm://default/recipe

# With custom output
eidos recipe -s system.yaml -i inference -o recipe.yaml --format yaml
```

**Output structure:**
```yaml
apiVersion: eidos.nvidia.com/v1alpha1
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

### eidos validate

Validate a system snapshot against the constraints defined in a recipe to verify cluster compatibility. Supports multi-phase validation with different validation stages.

**Synopsis:**
```shell
eidos validate [flags]
```

**Flags:**
| Flag | Short | Type | Description |
|------|-------|------|-------------|
| `--recipe` | `-r` | string | Path/URI to recipe file containing constraints (required) |
| `--snapshot` | `-s` | string | Path/URI to snapshot file containing measurements (required) |
| `--phase` | | string | Validation phase to run: readiness (default), deployment, performance, conformance, all |
| `--fail-on-error` | | bool | Exit with non-zero status if any constraint fails (default: true) |
| `--output` | `-o` | string | Output destination (file or stdout, default: stdout) |
| `--format` | `-t` | string | Output format: json, yaml, table (default: yaml) |
| `--kubeconfig` | `-k` | string | Path to kubeconfig file (for ConfigMap URIs) |

**Input Sources:**
- **File**: Local file path (`./recipe.yaml`, `./snapshot.yaml`)
- **URL**: HTTP/HTTPS URL (`https://example.com/recipe.yaml`)
- **ConfigMap**: Kubernetes ConfigMap URI (`cm://namespace/configmap-name`)

**Validation Phases:**

Validation can be run in different phases to validate different aspects of the deployment:

| Phase | Description | When to Run |
|-------|-------------|-------------|
| `readiness` | Validates infrastructure prerequisites (K8s version, OS, kernel) and runs readiness checks | Before deploying any components |
| `deployment` | Validates component deployment health and expected resources | After deploying components |
| `performance` | Validates system performance and network fabric health | After components are running |
| `conformance` | Validates workload-specific requirements and conformance | Before running production workloads |
| `all` | Runs all phases sequentially with dependency logic | Complete end-to-end validation |

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
# Validate snapshot against recipe (default: readiness phase)
eidos validate --recipe recipe.yaml --snapshot snapshot.yaml

# Validate specific phase
eidos validate \
  --recipe recipe.yaml \
  --snapshot snapshot.yaml \
  --phase deployment

# Run all validation phases
eidos validate \
  --recipe recipe.yaml \
  --snapshot snapshot.yaml \
  --phase all

# Load snapshot from ConfigMap
eidos validate \
  --recipe recipe.yaml \
  --snapshot cm://gpu-operator/eidos-snapshot

# Save results to file
eidos validate \
  --recipe recipe.yaml \
  --snapshot cm://gpu-operator/eidos-snapshot \
  --output validation-results.yaml

# Validate readiness phase before installing components
eidos validate \
  --recipe recipe.yaml \
  --snapshot snapshot.yaml \
  --phase readiness \
  --fail-on-error

# Validate deployment phase after components are installed
eidos validate \
  --recipe recipe.yaml \
  --snapshot snapshot.yaml \
  --phase deployment

# Run performance validation
eidos validate \
  --recipe recipe.yaml \
  --snapshot snapshot.yaml \
  --phase performance

# JSON output format
eidos validate \
  --recipe recipe.yaml \
  --snapshot snapshot.yaml \
  --format json

# With custom kubeconfig
eidos validate \
  --recipe recipe.yaml \
  --snapshot cm://gpu-operator/eidos-snapshot \
  --kubeconfig ~/.kube/prod-cluster
```

**Output Structure (Pre-Deployment Phase):**
```yaml
apiVersion: eidos.nvidia.com/v1alpha1
kind: ValidationResult
metadata:
  timestamp: "2025-12-31T10:30:00Z"
  version: v0.14.0
recipeSource: recipe.yaml
snapshotSource: cm://gpu-operator/eidos-snapshot
summary:
  passed: 5
  failed: 0
  skipped: 0
  total: 5
  status: pass
  duration: 20.5µs
phases:
  readiness:
    status: pass
    constraints:
      - name: K8s.server.version
        expected: '>= 1.30'
        actual: v1.30.14-eks-3025e55
        status: passed
      - name: OS.release.ID
        expected: ubuntu
        actual: ubuntu
        status: passed
    checks:
      - name: gpu-hardware-detection
        status: pass
      - name: kernel-parameters
        status: pass
      - name: os-prerequisites
        status: pass
    duration: 20.5µs
```

**Output Structure (All Phases):**
```yaml
apiVersion: eidos.nvidia.com/v1alpha1
kind: ValidationResult
metadata:
  timestamp: "2025-12-31T10:30:00Z"
  version: v0.14.0
recipeSource: recipe.yaml
snapshotSource: snapshot.yaml
summary:
  passed: 10
  failed: 0
  skipped: 0
  total: 10
  status: pass
  duration: 58.4µs
phases:
  readiness:
    status: pass
    constraints:
      - name: K8s.server.version
        expected: '>= 1.32.4'
        actual: v1.35.0
        status: passed
      - name: OS.release.ID
        expected: ubuntu
        actual: ubuntu
        status: passed
    checks:
      - name: gpu-hardware-detection
        status: pass
      - name: kernel-parameters
        status: pass
      - name: os-prerequisites
        status: pass
    duration: 20.7µs
  deployment:
    status: pass
    checks:
      - name: gpu-operator.version
        status: pass
      - name: operator-health
        status: pass
      - name: expected-resources
        status: pass
    duration: 1.2µs
  performance:
    status: pass
    checks:
      - name: nccl-bandwidth-test
        status: pass
      - name: fabric-health-check
        status: pass
    duration: 1.2µs
  conformance:
    status: skipped
    reason: conformance phase not configured in recipe
    duration: 0.8µs
```

**Validation Statuses:**
| Status | Description |
|--------|-------------|
| `passed` | Constraint satisfied |
| `failed` | Constraint not satisfied |
| `skipped` | Constraint could not be evaluated (missing data, invalid path) |

**Summary Status:**
| Status | Description |
|--------|-------------|
| `pass` | All constraints passed |
| `fail` | One or more constraints failed |
| `partial` | Some constraints skipped, none failed |

---

### eidos bundle

Generate deployment-ready bundles from recipes containing Helm values, manifests, scripts, and documentation.

**Synopsis:**
```shell
eidos bundle [flags]
```

**Flags:**
| Flag | Short | Type | Description |
|---------------------------------|-------|------|-------------|
| `--recipe` | `-r` | string | Path to recipe file (required) |
| `--output` | `-o` | string | Output directory (default: current dir) |
| `--deployer` | | string | Deployment method: helm (default), argocd |
| `--repo` | | string | Git repository URL for ArgoCD applications (only used with `--deployer argocd`) |
| `--set` | | string[] | Override values in bundle files (repeatable) |
| `--data` | | string | External data directory to overlay on embedded data (see [External Data](#external-data-directory)) |
| `--system-node-selector` | | string[] | Node selector for system components (format: key=value, repeatable) |
| `--system-node-toleration` | | string[] | Toleration for system components (format: key=value:effect, repeatable) |
| `--accelerated-node-selector` | | string[] | Node selector for accelerated/GPU nodes (format: key=value, repeatable) |
| `--accelerated-node-toleration` | | string[] | Toleration for accelerated/GPU nodes (format: key=value:effect, repeatable) |

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

**Examples:**
```shell
# Generate all bundles
eidos bundle --recipe recipe.yaml --output ./bundles

# Override values in GPU Operator bundle
eidos bundle -r recipe.yaml \
  --set gpuoperator:gds.enabled=true \
  --set gpuoperator:driver.version=570.86.16 \
  -o ./bundles

# Override multiple components
eidos bundle -r recipe.yaml \
  --set gpuoperator:mig.strategy=mixed \
  --set networkoperator:rdma.enabled=true \
  --set networkoperator:sriov.enabled=true \
  -o ./bundles

# Override cert-manager resources
eidos bundle -r recipe.yaml \
  --set certmanager:controller.resources.memory.limit=512Mi \
  --set certmanager:webhook.resources.cpu.limit=200m \
  -o ./bundles

# Override Skyhook manager resources
eidos bundle -r recipe.yaml \
  --set skyhook-operator:manager.resources.cpu.limit=500m \
  --set skyhook-operator:manager.resources.memory.limit=256Mi \
  -o ./bundles

# Schedule system components on specific node pool
eidos bundle -r recipe.yaml \
  --system-node-selector nodeGroup=system-pool \
  --system-node-toleration dedicated=system:NoSchedule \
  -o ./bundles

# Schedule GPU workloads on labeled GPU nodes
eidos bundle -r recipe.yaml \
  --accelerated-node-selector nvidia.com/gpu.present=true \
  --accelerated-node-toleration nvidia.com/gpu=present:NoSchedule \
  -o ./bundles

# Combined: separate system and GPU scheduling
eidos bundle -r recipe.yaml \
  --system-node-selector nodeGroup=system-pool \
  --system-node-toleration dedicated=system:NoSchedule \
  --accelerated-node-selector accelerator=nvidia-h100 \
  --accelerated-node-toleration nvidia.com/gpu=present:NoSchedule \
  -o ./bundles

# Generate ArgoCD Application manifests for GitOps
eidos bundle -r recipe.yaml --deployer argocd -o ./bundles

# ArgoCD with Git repository URL (avoids placeholder in app-of-apps.yaml)
eidos bundle -r recipe.yaml --deployer argocd \
  --repo https://github.com/my-org/my-gitops-repo.git \
  -o ./bundles

# Combine deployer with value overrides
eidos bundle -r recipe.yaml \
  --deployer argocd \
  -o ./bundles
```

**Bundle structure** (with default Helm deployer):
```
bundles/
├── README.md                      # Deployment guide with ordered steps
├── deploy.sh                      # One-command deployment script
├── recipe.yaml                    # Recipe used to generate bundle
├── checksums.txt                  # SHA256 checksums (optional)
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

ArgoCD Applications use multi-source to:
1. Pull Helm charts from upstream repositories
2. Apply values.yaml from your GitOps repository
3. Deploy additional manifests from component's manifests/ directory (if present)

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

---

## Complete Workflow Examples

### File-Based Workflow

```shell
# Step 1: Capture system configuration
eidos snapshot --output snapshot.yaml

# Step 2: Generate optimized recipe for training workloads
eidos recipe \
  --snapshot snapshot.yaml \
  --intent training \
  --output recipe.yaml

# Step 3: Validate recipe constraints against snapshot
eidos validate \
  --recipe recipe.yaml \
  --snapshot snapshot.yaml

# Step 4: Create deployment bundle
eidos bundle \
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
eidos snapshot --deploy-agent --output cm://gpu-operator/eidos-snapshot

# Alternative: Manual kubectl deployment
kubectl apply -f deployments/eidos-agent/1-deps.yaml
kubectl apply -f deployments/eidos-agent/2-job.yaml
kubectl wait --for=condition=complete job/eidos -n gpu-operator --timeout=5m

# Step 2: Generate recipe from ConfigMap
eidos recipe \
  --snapshot cm://gpu-operator/eidos-snapshot \
  --intent training \
  --output recipe.yaml

# Alternative: Write recipe to ConfigMap as well
eidos recipe \
  --snapshot cm://gpu-operator/eidos-snapshot \
  --intent training \
  --output cm://gpu-operator/eidos-recipe

# With custom kubeconfig (if not using default)
eidos recipe \
  --snapshot cm://gpu-operator/eidos-snapshot \
  --kubeconfig ~/.kube/prod-cluster \
  --intent training \
  --output recipe.yaml

# Step 3: Validate recipe constraints against cluster snapshot
eidos validate \
  --recipe recipe.yaml \
  --snapshot cm://gpu-operator/eidos-snapshot

# For CI/CD pipelines: exit non-zero on validation failure
eidos validate \
  --recipe recipe.yaml \
  --snapshot cm://gpu-operator/eidos-snapshot \
  --fail-on-error

# Step 4: Create bundle from recipe
eidos bundle \
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
EIDOS_BIN=$(find dist -maxdepth 2 -type f -name eidos | head -n 1)
chainsaw test --no-cluster --test-dir tests/chainsaw/cli/recipe-generation
```

## Shell Completion

Generate shell completion scripts:

```shell
# Bash
eidos completion bash

# Zsh
eidos completion zsh

# Fish
eidos completion fish

# PowerShell
eidos completion pwsh
```

**Installation:**

**Bash:**
```shell
source <(eidos completion bash)
# Or add to ~/.bashrc for persistence
echo 'source <(eidos completion bash)' >> ~/.bashrc
```

**Zsh:**
```shell
source <(eidos completion zsh)
# Or add to ~/.zshrc
echo 'source <(eidos completion zsh)' >> ~/.zshrc
```

## Environment Variables

Eidos respects standard environment variables:

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
eidos recipe --os ubuntu --accelerator h100 | jq '.componentRefs[]'
```

### Save All Steps
```shell
eidos snapshot -o snapshot.yaml
eidos recipe -s snapshot.yaml -i training -o recipe.yaml
eidos bundle -r recipe.yaml -o ./bundles
```

### JSON Processing
```shell
# Extract GPU Operator version from recipe
eidos recipe --os ubuntu --accelerator h100 --format json | \
  jq -r '.componentRefs[] | select(.name=="gpu-operator") | .version'

# Get all component versions
eidos recipe --os ubuntu --accelerator h100 --format json | \
  jq -r '.componentRefs[] | "\(.name): \(.version)"'
```

### Multiple Environments
```shell
# Generate recipes for different cloud providers
for service in eks gke aks; do
  eidos recipe --os ubuntu --service $service --gpu h100 \
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
eidos --debug snapshot
```

### Recipe Not Found
```shell
# Query parameters may not match any overlay
# Try broader query:
eidos recipe --os ubuntu --gpu h100
```

### Bundle Generation Fails
```shell
# Verify recipe file
cat recipe.yaml

# Check bundler is valid
eidos bundle --help  # Shows available bundlers

# Run with debug
eidos --debug bundle -r recipe.yaml
```

## External Data Directory

The `--data` flag enables extending or overriding the embedded recipe data with external files. This allows customization without rebuilding the CLI.

### Overview

Eidos embeds recipe data (overlays, component values, registry) at compile time. The `--data` flag layers an external directory on top, enabling:

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
eidos recipe --service eks --accelerator h100 --data ./my-data

# Use external data directory for bundle generation
eidos bundle --recipe recipe.yaml --data ./my-data --output ./bundles

# Combine with other flags
eidos recipe --service eks --gpu gb200 --intent training \
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
apiVersion: eidos.nvidia.com/v1alpha1
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
apiVersion: eidos.nvidia.com/v1alpha1
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
eidos recipe --service eks --intent training --data ./my-data
```

### Debugging External Data

Use `--debug` flag to see detailed logging about external data loading:

```shell
eidos --debug recipe --service eks --data ./my-data
```

Debug logs include:
- External files discovered and registered
- File source resolution (embedded vs external)
- Registry merge details (components added/overridden)

## Example Files

The `examples/` directory contains reference files for testing and learning:

### Snapshots (`examples/snapshots/`)

| File | Description |
|------|-------------|
| `gb200.yaml` | GB200 NVL72 system snapshot (Ubuntu 24.04, EKS 1.33, NVLink) |
| `h100.yaml` | H100 GPU cluster snapshot (Ubuntu 22.04, GKE 1.32) |
| `gb200-h100-comp.md` | Configuration comparison between GB200 and H100 |

**Usage:**
```shell
# Generate recipe from example snapshot
eidos recipe --snapshot examples/snapshots/gb200.yaml --intent training --platform kubeflow
```

### Recipes (`examples/recipes/`)

| File | Description |
|------|-------------|
| `eks-gb200-training.yaml` | GB200 training workload recipe for EKS |
| `eks-h100-training.yaml` | H100 training workload recipe for EKS |

**Usage:**
```shell
# Generate bundle from example recipe
eidos bundle --recipe examples/recipes/eks-gb200-training.yaml --output ./bundles
```

### Templates (`examples/templates/`)

| File | Description |
|------|-------------|
| `snapshot-template.md.tmpl` | Go template for custom snapshot report formatting |

**Usage:**
```shell
# Generate custom cluster report
eidos snapshot --template examples/templates/snapshot-template.md.tmpl --output report.md
```

## See Also

- [Installation Guide](installation.md) - Install eidos
- [Agent Deployment](agent-deployment.md) - Kubernetes agent setup
- [API Reference](api-reference.md) - Programmatic access
- [Architecture Docs](../contributor/README.md) - Internal architecture
- [Data Architecture](../contributor/data.md) - Recipe data system details
