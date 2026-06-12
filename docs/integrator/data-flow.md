# Data Flow Architecture

Data transformations in the four-stage workflow.

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
│ Snapshot (aicr.nvidia.com/v1alpha1)                      │
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
│   ├─ GPU                                                │
│   │   └─ subtypes: [smi, driver, device]                │
│   │       └─ data: map[string]Reading                   │
│   │                                                     │
│   └─ NodeTopology                                       │
│       └─ subtypes: [summary, taint, label]              │
│           └─ data: map[string]Reading                   │
└─────────────────────────────────────────────────────────┘
```

**Output Destinations:**
- **File**: `aicr snapshot --output system.yaml`
- **Stdout**: `aicr snapshot` (default, pipe to other commands)
- **ConfigMap**: `aicr snapshot --output cm://namespace/name` (Kubernetes-native)

**ConfigMap Storage Pattern:**
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: aicr-snapshot
  namespace: gpu-operator
data:
  snapshot.yaml: |
    # Complete snapshot YAML stored as ConfigMap data
    apiVersion: aicr.nvidia.com/v1alpha1
    kind: Snapshot
    measurements: [...]
```

**Agent Deployment:**  
Kubernetes Job writes snapshots directly to ConfigMap without volumes:
```bash
aicr snapshot --output cm://gpu-operator/aicr-snapshot
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

#### Parallel Collection

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
aicr recipe --os ubuntu --accelerator h100 --service eks --intent training --platform kubeflow
```

**Snapshot Mode (File)** - Analyze captured snapshot:
```bash
aicr snapshot --output system.yaml
aicr recipe --snapshot system.yaml --intent training --platform kubeflow
```

**Snapshot Mode (ConfigMap)** - Read from Kubernetes:
```bash
# Agent or CLI writes snapshot to ConfigMap
aicr snapshot --output cm://gpu-operator/aicr-snapshot

# CLI reads from ConfigMap to generate recipe
aicr recipe --snapshot cm://gpu-operator/aicr-snapshot --intent training --platform kubeflow

# Recipe can also be written to ConfigMap
aicr recipe --snapshot cm://gpu-operator/aicr-snapshot \
            --intent training \
            --platform kubeflow \
            --output cm://gpu-operator/aicr-recipe
```

### Query Extraction (Snapshot Mode)

When a snapshot is provided, the recipe builder extracts query parameters:

```
Snapshot → Query Extractor → Recipe Query
```

#### Extraction mapping

```
K8s/server/version          → k8s (version)
K8s/image/gpu-operator      → service (eks/gke/aks detection)
K8s/config/*                → intent hints
OS/release/ID               → os (family)
OS/release/VERSION_ID       → osv (version)
OS/grub/BOOT_IMAGE          → kernel (version)
GPU/smi/model               → accelerator (type)
```

### Recipe Generation

#### Inheritance and Overlay Merging

When a query matches a leaf recipe with a `spec.base` reference, the builder:

1. **Matches overlays by criteria.** An overlay matches when every field it
   specifies equals the query; omitted fields act as wildcards (e.g. an overlay
   that omits `os` matches any OS). The `any` sentinel is its own value — a
   query `any` only matches a recipe `any`.
2. **Resolves the inheritance chain** for each match by following `spec.base`
   to the implicit `base`, producing a root-to-leaf ordering such as
   `base → eks → eks-training → gb200-eks-training → gb200-eks-ubuntu-training`.
3. **Merges in order**, later overlays overriding earlier ones.
4. **Applies mixins** (`spec.mixins`): appends their constraints and
   `componentRefs`, evaluating mixin constraints when a snapshot is provided.
5. **Strips context** maps from subtypes unless context output is requested.

For the resolver internals (specificity scoring, deep-merge semantics) see
[Recipe architecture](../contributor/recipe.md).

### Recipe Data Structure

```
┌─────────────────────────────────────────────────────────┐
│ Recipe (aicr.nvidia.com/v1alpha1)                        │
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

Constraints use fully qualified paths (`{Type}.{Subtype}.{Key}`) and a set of
comparison operators. The full path and operator reference — including the
per-validator operator narrowings and the `inference-perf` input entries
(model, concurrency, routing mode) — lives in the CLI reference; see
[Constraint paths and operators](../user/cli-reference.md#constraint-paths-and-operators).

### Input Sources

**File-based:**
```bash
aicr validate --recipe recipe.yaml --snapshot snapshot.yaml
```

**ConfigMap-based:**
```bash
aicr validate \
    --recipe recipe.yaml \
    --snapshot cm://gpu-operator/aicr-snapshot
```

**HTTP/HTTPS:**
```bash
aicr validate \
    --recipe https://example.com/recipe.yaml \
    --snapshot https://example.com/snapshot.yaml
```

### Validation Output

Results are emitted in [CTRF](https://ctrf.io/) (Common Test Report Format)
JSON: a top-level `summary` (test counts and start/stop timestamps) plus a
`tests` array where each entry carries a `name`, `status`
(passed/failed/skipped), `suite` (the phase — readiness, deployment,
performance, conformance), and `stdout` lines with the per-check evidence. For
a worked example of the full report and how performance checks such as
`inference-perf` surface their measured values, see
[Emitting recipe evidence for a PR](../user/validation.md#emitting-recipe-evidence).

### CI/CD Integration

By default, the command exits with non-zero status on validation failures (ideal for CI/CD):

```bash
aicr validate \
    --recipe recipe.yaml \
    --snapshot cm://gpu-operator/aicr-snapshot

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
│    └─ Nodewright                                       │
│       ├─ values map → values.yaml                      │
│       └─ values map → nodewright-cr.yaml               │
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

#### RecipeResult Pattern

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

#### ScriptData for Metadata

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
│  │     └─ Argo CD Application + sync-wave   │           │
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
│  │    Helm    │                    │  Argo CD    │       │
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
│   └── README.md          # Component install/upgrade/uninstall
└── network-operator/
    ├── values.yaml
    ├── README.md
    └── manifests/         # Optional manifest files
        └── nfd-network-rule.yaml
```

**Argo CD Deployer**:
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

Argo CD Application with multi-source:
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
      targetRevision: v26.3.2
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
│  aicr bundle -r recipe.yaml --deployer argocd \            │
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
- Location: `recipes/overlays/*.yaml` (including `base.yaml`), `recipes/mixins/*.yaml`
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

## See Also

- [Data Architecture](../contributor/recipe.md) - Recipe data architecture
- [API Reference](../user/api-reference.md) - API endpoint details
- [Automation](automation.md) - CI/CD integration patterns
- [CONTRIBUTING.md](https://github.com/NVIDIA/aicr/blob/main/CONTRIBUTING.md) - Developer guide
