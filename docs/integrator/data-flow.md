# Data Flow Architecture

This document describes data transformations in the four-stage workflow.

## Overview

Data flows through four stages:

```
System Config → Snapshot → Recipe → Validate → Bundle → Deployment
     (Raw)      (Capture)  (Optimize) (Check)  (Package)  (Deploy)
```

Each stage transforms input data into a different format:

- **Snapshot**: Captures raw system state (OS, GPU, Kubernetes, SystemD)
- **Recipe**: Generates configuration recommendations by matching query parameters against overlay rules
- **Validate**: Checks recipe constraints against actual system measurements
- **Bundle**: Produces deployment artifacts (Helm values, manifests, scripts)

## Stage 1: Snapshot (Data Capture)

### Input Sources

**SystemD Services:**
- Source: `systemctl show containerd.service`
- Data: Service configuration, resource limits, cgroup delegates
- Format: Key-value pairs from SystemD properties

**OS Configuration:**
- **grub**: `/proc/cmdline` - Boot parameters
- **kmod**: `/proc/modules` - Loaded kernel modules
- **sysctl**: `/proc/sys/**/*` - Kernel runtime parameters
- **release**: `/etc/os-release` - OS identification

**Kubernetes Cluster:**
- Source: Kubernetes API via `client-go`
- **server**: Version info from `/version` endpoint
- **image**: Container images from all pods across namespaces
- **policy**: GPU Operator ClusterPolicy custom resource

**GPU Hardware:**
- Source: `nvidia-smi` command-line tool
- Data: Driver version, CUDA version, MIG settings, device info
- Format: Parsed XML/text output

### Snapshot Data Structure

```
┌─────────────────────────────────────────────────────────┐
│ Snapshot (eidos.nvidia.com/v1alpha1)                      │
├─────────────────────────────────────────────────────────┤
│ metadata:                                               │
│   created: timestamp                                    │
│   hostname: string                                      │
│                                                         │
│ measurements: []Measurement                             │
│   ├─ SystemD                                            │
│   │   └─ subtypes: [containerd.service, ...]            │
│   │       └─ data: map[string]Reading                   │
│   │                                                     │
│   ├─ OS                                                 │
│   │   └─ subtypes: [grub, kmod, sysctl, release]        │
│   │       └─ data: map[string]Reading                   │
│   │                                                     │
│   ├─ K8s                                                │
│   │   └─ subtypes: [server, image, policy]              │
│   │       └─ data: map[string]Reading                   │
│   │                                                     │
│   └─ GPU                                                │
│       └─ subtypes: [smi, driver, device]                │
│           └─ data: map[string]Reading                   │
└─────────────────────────────────────────────────────────┘
```

**Output Destinations:**
- **File**: `eidos snapshot --output system.yaml`
- **Stdout**: `eidos snapshot` (default, pipe to other commands)
- **ConfigMap**: `eidos snapshot --output cm://namespace/name` (Kubernetes-native)

**ConfigMap Storage Pattern:**
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: eidos-snapshot
  namespace: gpu-operator
data:
  snapshot.yaml: |
    # Complete snapshot YAML stored as ConfigMap data
    apiVersion: eidos.nvidia.com/v1alpha1
    kind: Snapshot
    measurements: [...]
```

**Agent Deployment:**  
Kubernetes Job writes snapshots directly to ConfigMap without volumes:
```bash
eidos snapshot --output cm://gpu-operator/eidos-snapshot
```

**Reading Interface:**
```go
type Reading interface {
    Any() interface{}      // Type-safe value extraction
    String() string        // String representation
    // Supports: int, string, bool, float64
}
```

### Collection Process

**Parallel Collection:**
```
┌──────────────┐
│ Snapshotter  │
└──────┬───────┘
       │ errgroup.WithContext()
       ├────────────┬─────────────┬─────────────┐
       │            │             │             │
  ┌────▼────┐   ┌───▼───┐     ┌───▼───┐     ┌───▼───┐
  │ SystemD │   │  OS   │     │  K8s  │     │  GPU  │
  │Collector│   │Collect│     │Collect│     │Collect│
  └────┬────┘   └───┬───┘     └───┬───┘     └───┬───┘
       │            │             │             │
       └────────────┴─────────────┴─────────────┘
                    │
              ┌─────▼──────┐
              │  Snapshot  │
              │   (YAML)   │
              └────────────┘
```

**Context Propagation:**
- All collectors respect context cancellation
- First error cancels remaining operations
- Timeout: 30 seconds per collector

## Stage 2: Recipe (Data Optimization)

### Recipe Input Options

**Query Mode** - Direct generation from parameters:
```bash
eidos recipe --os ubuntu --gpu h100 --service eks --intent training --platform kubeflow
```

**Snapshot Mode (File)** - Analyze captured snapshot:
```bash
eidos snapshot --output system.yaml
eidos recipe --snapshot system.yaml --intent training --platform kubeflow
```

**Snapshot Mode (ConfigMap)** - Read from Kubernetes:
```bash
# Agent or CLI writes snapshot to ConfigMap
eidos snapshot --output cm://gpu-operator/eidos-snapshot

# CLI reads from ConfigMap to generate recipe
eidos recipe --snapshot cm://gpu-operator/eidos-snapshot --intent training --platform kubeflow

# Recipe can also be written to ConfigMap
eidos recipe --snapshot cm://gpu-operator/eidos-snapshot \
            --intent training \
            --platform kubeflow \
            --output cm://gpu-operator/eidos-recipe
```

### Query Extraction (Snapshot Mode)

When a snapshot is provided, the recipe builder extracts query parameters:

```
Snapshot → Query Extractor → Recipe Query
```

**Extraction mapping:**
```
K8s/server/version          → k8s (version)
K8s/image/gpu-operator      → service (eks/gke/aks detection)
K8s/config/*                → intent hints
OS/release/ID               → os (family)
OS/release/VERSION_ID       → osv (version)
OS/grub/BOOT_IMAGE          → kernel (version)
GPU/smi/model               → gpu (type)
```

### Recipe Generation

**Inheritance Chain Resolution:**

When a query matches a leaf recipe that has a `spec.base` reference, the system resolves the full inheritance chain before merging:

```
┌─────────────────────────────────────────────────────────────┐
│ Inheritance Resolution                                      │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  Query: {service: eks, accelerator: gb200, os: ubuntu,      │
│          intent: training}                                  │
│                                                             │
│  1. Find matching recipes (by specificity):                 │
│     - eks (specificity: 1)                                  │
│     - eks-training (specificity: 2)                         │
│     - gb200-eks-training (specificity: 3)                   │
│     - gb200-eks-ubuntu-training (specificity: 4)            │
│                                                             │
│  2. Resolve inheritance chain for each:                     │
│     gb200-eks-ubuntu-training.spec.base = "gb200-eks-training"
│     gb200-eks-training.spec.base = "eks-training"           │
│     eks-training.spec.base = "eks"                          │
│     eks.spec.base = "" (implicit base)                      │
│                                                             │
│  3. Build chain (root to leaf):                             │
│     [base] → [eks] → [eks-training] → [gb200-eks-training]  │
│           → [gb200-eks-ubuntu-training]                     │
│                                                             │
│  4. Merge in order (later overrides earlier):               │
│     result = base                                           │
│     result = merge(result, eks)                             │
│     result = merge(result, eks-training)                    │
│     result = merge(result, gb200-eks-training)              │
│     result = merge(result, gb200-eks-ubuntu-training)       │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

**Base + Overlay Merging:**
```
┌────────────────────────────────────────────────────────┐
│ Recipe Builder                                         │
├────────────────────────────────────────────────────────┤
│                                                        │
│  1. Load base measurements (universal config)          │
│     └─ From embedded overlays/base.yaml                │
│                                                        │
│  2. Match query to overlays (by criteria)              │
│     ├─ Query matches recipes where:                    │
│     │   - Recipe "any" field = wildcard (matches any)  │
│     │   - Query "any" field = only matches recipe "any"│
│     └─ Resolve inheritance chain for each match        │
│                                                        │
│  3. Merge inheritance chain in order                   │
│     ├─ Base values (from overlays/base.yaml)           │
│     ├─ + eks (EKS-specific settings)                   │
│     ├─ + eks-training (training optimizations)         │
│     ├─ + gb200-eks-training (GB200 overrides)          │
│     └─ + gb200-eks-ubuntu-training (Ubuntu specifics)  │
│                                                        │
│  4. Strip context (if !context)                        │
│     └─ Remove context maps from all subtypes           │
│                                                        │
│  5. Return recipe                                      │
│                                                        │
└────────────────────────────────────────────────────────┘
```

**Overlay Matching Algorithm:**
```go
// Overlay matches if all specified fields match query
// Omitted fields act as wildcards

overlay.key {
    service: "eks"   // Must match
    gpu: "gb200"     // Must match
    os: <omitted>    // Wildcard (any OS)
}

query {
    service: "eks"
    gpu: "gb200"
    os: "ubuntu"
}

Result: MATCH (os wildcarded)
```

### Recipe Data Structure

```
┌─────────────────────────────────────────────────────────┐
│ Recipe (eidos.nvidia.com/v1alpha1)                        │
├─────────────────────────────────────────────────────────┤
│ metadata:                                               │
│   version: recipe format version                        │
│   created: timestamp                                    │
│   appliedOverlays: inheritance chain (root to leaf)     │
│                                                         │
│ criteria: Criteria (service, accelerator, intent, os)   │
│                                                         │
│ componentRefs: []ComponentRef                           │
│   ├─ name: component name                               │
│   ├─ version: component version                         │
│   ├─ order: deployment order                            │
│   └─ repository: Helm repository URL                    │
│                                                         │
│ constraints:                                            │
│   └─ driver: version, cudaVersion                       │
└─────────────────────────────────────────────────────────┘
```

**Applied Overlays Example (with inheritance):**
```yaml
metadata:
  appliedOverlays:
    - base
    - eks
    - eks-training
    - gb200-eks-training
    - gb200-eks-ubuntu-training
```

## Stage 3: Validate (Constraint Checking)

### Validation Process

The validate stage compares recipe constraints against actual measurements from a cluster snapshot.

```
┌────────────────────────────────────────────────────────┐
│ Validator                                              │
├────────────────────────────────────────────────────────┤
│                                                        │
│  Recipe Constraints + Snapshot → Validation Results    │
│                                                        │
│  ┌─────────────────┐    ┌─────────────────┐            │
│  │ Recipe          │    │ Snapshot        │            │
│  │ constraints:    │    │ measurements:   │            │
│  │   - K8s.version │    │   - K8s/server  │            │
│  │   - OS.release  │    │   - OS/release  │            │
│  └────────┬────────┘    └────────┬────────┘            │
│           │                      │                     │
│           └───────────┬──────────┘                     │
│                       │                                │
│              ┌────────▼────────┐                       │
│              │ Constraint      │                       │
│              │ Evaluation      │                       │
│              │ ├─ Version cmp  │                       │
│              │ ├─ Equality     │                       │
│              │ └─ Exact match  │                       │
│              └────────┬────────┘                       │
│                       │                                │
│              ┌────────▼────────┐                       │
│              │ Results         │                       │
│              │ ├─ Passed       │                       │
│              │ ├─ Failed       │                       │
│              │ └─ Skipped      │                       │
│              └─────────────────┘                       │
│                                                        │
└────────────────────────────────────────────────────────┘
```

### Constraint Path Format

Constraints use fully qualified paths: `{Type}.{Subtype}.{Key}`

| Path | Description |
|------|-------------|
| `K8s.server.version` | Kubernetes server version |
| `OS.release.ID` | Operating system family (ubuntu, rhel) |
| `OS.release.VERSION_ID` | OS version (22.04, 24.04) |
| `OS.sysctl./proc/sys/kernel/osrelease` | Kernel version |
| `GPU.driver.version` | NVIDIA driver version |

### Supported Operators

| Operator | Description | Example |
|----------|-------------|---------|
| `>=` | Greater than or equal | `K8s.server.version>=1.28` |
| `<=` | Less than or equal | `K8s.server.version<=1.30` |
| `>` | Greater than | `OS.release.VERSION_ID>22.04` |
| `<` | Less than | `OS.release.VERSION_ID<25.00` |
| `==` | Exactly equal | `OS.release.ID==ubuntu` |
| `!=` | Not equal | `OS.release.ID!=rhel` |
| (none) | Exact match | `GPU.driver.version` |

### Input Sources

**File-based:**
```bash
eidos validate --recipe recipe.yaml --snapshot snapshot.yaml
```

**ConfigMap-based:**
```bash
eidos validate \
    --recipe recipe.yaml \
    --snapshot cm://gpu-operator/eidos-snapshot
```

**HTTP/HTTPS:**
```bash
eidos validate \
    --recipe https://example.com/recipe.yaml \
    --snapshot https://example.com/snapshot.yaml
```

### Validation Output

```yaml
apiVersion: eidos.nvidia.com/v1alpha1
kind: ValidationResult
metadata:
  created: "2025-01-15T10:30:00Z"
summary:
  total: 5
  passed: 4
  failed: 1
  skipped: 0
results:
  - constraint: "K8s.server.version>=1.28"
    status: passed
    expected: ">=1.28"
    actual: "1.33.5"
  - constraint: "OS.release.ID==ubuntu"
    status: passed
    expected: "ubuntu"
    actual: "ubuntu"
  - constraint: "GPU.driver.version>=570.00"
    status: failed
    expected: ">=570.00"
    actual: "560.28.03"
    message: "version 560.28.03 does not satisfy >=570.00"
```

### CI/CD Integration

By default, the command exits with non-zero status on validation failures (ideal for CI/CD):

```bash
eidos validate \
    --recipe recipe.yaml \
    --snapshot cm://gpu-operator/eidos-snapshot

# Exit code: 0 = all passed, 1 = failures detected
# Use --fail-on-error=false for informational mode without failing
```

## Stage 4: Bundle (Data Packaging)

### Bundler Framework

```
┌────────────────────────────────────────────────────────┐
│ Bundle Generator                                       │
├────────────────────────────────────────────────────────┤
│                                                        │
│  RecipeResult → Bundler Registry → Parallel Execution  │
│                                                        │
│  ┌─────────────────┐                                   │
│  │ RecipeResult    │                                   │
│  └────────┬────────┘                                   │
│           │                                            │
│  ┌────────▼────────┐                                   │
│  │ Get Component   │ (GetComponentRef)                 │
│  │ ├─ Name         │                                   │
│  │ ├─ Version      │                                   │
│  │ └─ Values map   │ (GetValuesForComponent)           │
│  └────────┬────────┘                                   │
│           │                                            │
│    ┌──────┴──────┐                                     │
│    │   Parallel  │                                     │
│    ├─────────────┤                                     │
│    ├─ GPU Operator                                     │
│    │  ├─ values map → values.yaml                      │
│    │  ├─ values map → clusterpolicy.yaml               │
│    │  └─ ScriptData → install.sh, README.md            │
│    │                                                   │
│    ├─ Network Operator                                 │
│    │  ├─ values map → values.yaml                      │
│    │  └─ ScriptData → install.sh, README.md            │
│    │                                                   │
│    ├─ Cert-Manager                                     │
│    │  └─ values map → values.yaml                      │
│    │                                                   │
│    ├─ NVSentinel                                       │
│    │  └─ values map → values.yaml                      │
│    │                                                   │
│    └─ Skyhook                                          │
│       ├─ values map → values.yaml                      │
│       └─ values map → skyhook-cr.yaml                  │
│                                                        │
│  ┌────────▼────────┐                                   │
│  │ Template Engine │ (go:embed templates)              │
│  │ ├─ values.yaml  │                                   │
│  │ ├─ manifests/   │                                   │
│  │ └─ checksums.txt│                                   │
│  └────────┬────────┘                                   │
│           │                                            │
│  ┌────────▼────────┐                                   │
│  │ Generate Files  │                                   │
│  │ └─ checksums    │                                   │
│  └─────────────────┘                                   │
│                                                        │
└────────────────────────────────────────────────────────┘
```

### Configuration Extraction

**RecipeResult Pattern:**
Bundlers receive `RecipeResult` with component references and values maps:

```go
// Get component reference and values from RecipeResult
component := input.GetComponentRef("gpu-operator")
values := input.GetValuesForComponent("gpu-operator")

// Values map contains nested configuration
// {
//   "driver": {"enabled": true, "version": "580.82.07"},
//   "mig": {"strategy": "single"},
//   "gds": {"enabled": false}
// }
```

**Template Usage:**
```yaml
# Helm values.yaml - receives values map
driver:
  version: {{ index .Values "driver.version" }}
  
# README.md - receives combined map with Values + Script
Driver Version: {{ index .Values "driver.version" }}
Namespace: {{ .Script.Namespace }}
```

**ScriptData for Metadata:**
```go
// ScriptData struct for scripts and README metadata
type ScriptData struct {
    Timestamp        string
    Version          string
    Namespace        string
    HelmRepository   string
    HelmChartVersion string
}
```

### Bundle Structure

The deployer generates the final output structure. See [Deployer-Specific Output](#deployer-specific-output) for details per deployer type.

## Stage 5: Deployment (GitOps Integration)

### Deployer Framework

After bundlers generate artifacts, the deployer framework transforms them into deployment-specific formats based on the `--deployer` flag.

```
┌────────────────────────────────────────────────────────┐
│ Deployer Selection                                     │
├────────────────────────────────────────────────────────┤
│                                                        │
│  Bundle Artifacts + Recipe → Deployer → Output         │
│                                                        │
│  ┌─────────────────┐    ┌─────────────────┐            │
│  │ Bundle Output   │    │ Recipe          │            │
│  │ ├─ values.yaml  │    │ deploymentOrder │            │
│  │ ├─ manifests/   │    │ componentRefs   │            │
│  │ └─ scripts/     │    └────────┬────────┘            │
│  └────────┬────────┘             │                     │
│           │                      │                     │
│           └───────────┬──────────┘                     │
│                       │                                │
│  ┌────────────────────▼────────────────────┐           │
│  │ Deployer Selection (--deployer flag)    │           │
│  │                                         │           │
│  │ ├─ helm (default)                       │           │
│  │ │   └─ Helm charts + README             │           │
│  │ │                                       │           │
│  │ └─ argocd                               │           │
│  │     └─ ArgoCD Application + sync-wave   │           │
│  └─────────────────────────────────────────┘           │
│                                                        │
└────────────────────────────────────────────────────────┘
```

### Deployment Order Flow

The `deploymentOrder` field in recipes specifies component deployment sequence. Each deployer implements ordering differently:

```
┌─────────────────────────────────────────────────────────┐
│ Deployment Order Processing                             │
├─────────────────────────────────────────────────────────┤
│                                                         │
│  Recipe deploymentOrder:                                │
│    1. cert-manager                                      │
│    2. gpu-operator                                      │
│    3. network-operator                                  │
│                                                         │
│         │                                               │
│         ▼                                               │
│  ┌──────────────────────────────────────────────────┐   │
│  │ orderComponentsByDeployment()                    │   │
│  │   Sorts components based on deploymentOrder      │   │
│  │   Returns: []orderedComponent{Name, Order}       │   │
│  └───────────────────────┬──────────────────────────┘   │
│                          │                              │
│         ┌────────────────┴────────────────┐             │
│         ▼                                 ▼             │
│  ┌────────────┐                    ┌────────────┐       │
│  │    Helm    │                    │  ArgoCD    │       │
│  │  Deployer  │                    │  Deployer  │       │
│  │ (default)  │                    │            │       │
│  └──────┬─────┘                    └──────┬─────┘       │
│         │                                 │             │
│         ▼                                 ▼             │
│  Per-component dirs                sync-wave:           │
│  + deploy.sh script                - cert-manager:0     │
│                                    - gpu-operator:1     │
│                                    - network-op:2       │
│                                                         │
└─────────────────────────────────────────────────────────┘
```

### Deployer-Specific Output

**Helm Deployer** (default):
```
bundle-output/
├── README.md              # Root deployment guide with ordered steps
├── deploy.sh              # Automation script (chmod +x)
├── recipe.yaml            # Copy of the input recipe
├── checksums.txt          # SHA256 checksums of all files
├── cert-manager/
│   ├── values.yaml        # Component Helm values
│   └── README.md          # Component install/upgrade/uninstall
├── gpu-operator/
│   ├── values.yaml        # Component Helm values
│   ├── README.md          # Component install/upgrade/uninstall
│   └── manifests/         # Optional manifest files
│       └── dcgm-exporter.yaml
└── network-operator/
    ├── values.yaml
    └── README.md
```

**ArgoCD Deployer**:
```
bundle-output/
├── app-of-apps.yaml       # Parent Application (bundle root)
├── gpu-operator/
│   ├── values.yaml
│   ├── manifests/
│   └── argocd/
│       └── application.yaml   # With sync-wave annotation
├── network-operator/
│   ├── values.yaml
│   └── argocd/
│       └── application.yaml   # With sync-wave annotation
└── README.md
```

ArgoCD Application with multi-source:
```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: gpu-operator
  annotations:
    argocd.argoproj.io/sync-wave: "1"  # After cert-manager (0)
spec:
  sources:
    # Helm chart from upstream
    - repoURL: https://helm.ngc.nvidia.com/nvidia
      targetRevision: v25.3.3
      chart: gpu-operator
      helm:
        valueFiles:
          - $values/gpu-operator/values.yaml
    # Values from GitOps repo
    - repoURL: <YOUR_GIT_REPO>
      targetRevision: main
      ref: values
    # Additional manifests (if present)
    - repoURL: <YOUR_GIT_REPO>
      targetRevision: main
      path: gpu-operator/manifests
```

### Deployer Data Flow

```
┌──────────────────────────────────────────────────────────────┐
│ Complete Bundle + Deploy Flow                                │
├──────────────────────────────────────────────────────────────┤
│                                                              │
│  eidos bundle -r recipe.yaml --deployer argocd \            │
│    --repo https://github.com/my-org/my-repo.git -o ./out     │
│                                                              │
│  1. Parse recipe                                             │
│     └─ Extract componentRefs + deploymentOrder               │
│                                                              │
│  2. Order components                                         │
│     └─ orderComponentsByDeployment()                         │
│                                                              │
│  3. Run bundlers (parallel)                                  │
│     ├─ cert-manager   → values.yaml, manifests/              │
│     ├─ gpu-operator   → values.yaml, manifests/              │
│     └─ network-operator → values.yaml, manifests/            │
│                                                              │
│  4. Run deployer (argocd) → per-component argocd/ dirs       │
│     ├─ cert-manager/argocd/application.yaml (wave: 0)        │
│     ├─ gpu-operator/argocd/application.yaml (wave: 1)        │
│     └─ network-operator/argocd/application.yaml (wave: 2)    │
│     └─ app-of-apps.yaml (bundle root, uses --repo URL)       │
│                                                              │
│  5. Generate checksums                                       │
│     └─ checksums.txt for each component                      │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

## Data Serialization

### Formats Supported

**JSON:**
```json
{
  "apiVersion": "v1",
  "kind": "Recipe",
  "measurements": [...]
}
```

**YAML:**
```yaml
apiVersion: v1
kind: Recipe
measurements:
  - type: K8s
    subtypes: [...]
```

**Table (Human-readable):**
```
TYPE    SUBTYPE      KEY                    VALUE
K8s     image        gpu-operator           v25.3.3
K8s     image        driver                 580.82.07
GPU     driver       version                580.82.07
```

### Serialization Pipeline

```
Go Struct → Interface → Marshaler → Output Format

Measurement{
  Type: "K8s"
  Subtypes: []Subtype{...}
}
    │
    ▼
json.Marshal() / yaml.Marshal()
    │
    ▼
{"type":"K8s","subtypes":[...]}
```

## API Server Data Flow

### Request Processing

```
HTTP Request → Middleware Chain → Handler → Response

1. Metrics Middleware (record request)
2. Version Middleware (check API version)
3. RequestID Middleware (add/echo request ID)
4. Panic Recovery (catch panics)
5. Rate Limit (100 req/s)
6. Logging (structured logs)
7. Handler:
   ├─ Parse query parameters
   ├─ Build Query
   ├─ recipe.Builder.Build(ctx, query)
   ├─ Serialize response
   └─ Return JSON
```

### Response Headers

```
HTTP/1.1 200 OK
Content-Type: application/json
X-Request-Id: 550e8400-e29b-41d4-a716-446655440000
Cache-Control: public, max-age=300
X-RateLimit-Limit: 100
X-RateLimit-Remaining: 95
X-RateLimit-Reset: 1735650000

{recipe JSON}
```

## Data Storage

### Embedded Data

**Recipe Data:**
- Location: `recipes/overlays/*.yaml` (including `base.yaml`)
- Embedded at compile time via `//go:embed` directives
- Loaded once per process, cached in memory
- TTL: 5 minutes (in-memory cache)

**Bundle Templates:**
- Location: `pkg/bundler/*/templates/*.tmpl`
- Embedded at compile time: `//go:embed templates/*.tmpl`
- Parsed once per bundler initialization

**No External Dependencies:**
- No database
- No configuration files
- No network calls (except Kubernetes API for snapshots)
- Fully self-contained binaries

## Performance Characteristics

### Snapshot Collection

- **Parallel**: All collectors run concurrently
- **Timeout**: 30 seconds per collector
- **Memory**: ~10-50MB depending on cluster size
- **Duration**: 1-5 seconds typical

### Recipe Generation

- **Cached**: Recipe data cached in memory (5min TTL)
- **Overlay Matching**: O(n) where n = number of overlays
- **Memory**: <1MB per request
- **Duration**: <100ms typical (in-memory only)

### Bundle Generation

- **Parallel**: All bundlers run concurrently
- **Template Rendering**: Minimal overhead (<10ms per template)
- **File I/O**: ~10-20 files per bundler
- **Duration**: <1 second typical

### API Server

- **Concurrency**: 100 req/s sustained, 200 burst
- **Latency**: p50: 50ms, p95: 150ms, p99: 300ms
- **Memory**: ~100MB baseline + 1MB per concurrent request
- **CPU**: Minimal (<5% single core at 100 req/s)

## Data Validation

### Input Validation

**Query Parameters:**
- Type checking (string, int, bool)
- Enum validation (eks, gke, aks, etc.)
- Version format validation (regex)
- Range validation (if applicable)

**Snapshot Files:**
- YAML/JSON schema validation
- Required fields presence
- Type consistency
- Measurement structure validation

### Output Validation

**Recipes:**
- Valid apiVersion and kind
- Metadata with version and timestamp
- Criteria properly populated
- ComponentRefs have required fields (name, version)

**Bundles:**
- All required files generated
- Templates rendered successfully
- Checksums computed
- File permissions correct (scripts executable)

## See Also

- [Data Architecture](../contributor/data.md) - Recipe data architecture
- [API Reference](../user/api-reference.md) - API endpoint details
- [Automation](automation.md) - CI/CD integration patterns
- [CONTRIBUTING.md](../../CONTRIBUTING.md) - Developer guide
