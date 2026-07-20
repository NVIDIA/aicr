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
- Source: NFD/PCI enumeration via sysfs (driver-free; no `nvidia-smi`)
- Data: GPU presence, count, kernel-module state, and the accelerator SKU resolved from the PCI device ID

### Snapshot Data Structure

```
┌─────────────────────────────────────────────────────────┐
│ Snapshot (aicr.run/v1alpha2)                      │
├─────────────────────────────────────────────────────────┤
│ metadata:                                               │
│   timestamp: RFC3339 string                             │
│   version: CLI version that captured the snapshot       │
│   source-node: node name the snapshot was taken on      │
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
│   │   └─ subtypes: [hardware]                           │
│   │       └─ data: map[string]Reading                   │
│   │                                                     │
│   ├─ NodeTopology                                       │
│   │   └─ subtypes: [summary, taint, label]              │
│   │       └─ data: map[string]Reading                   │
│   │                                                     │
│   └─ NetworkTopology (only with --cluster-config /      │
│       │                --discover-network)              │
│       └─ subtypes: [identity, capabilities, pfs,        │
│           │          kernel-modules]                    │
│           ├─ identity/capabilities/kernel-modules:      │
│           │     data: map[string]Reading                │
│           └─ pfs: items: []ItemEntry                    │
│                 (per item: context + data)              │
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
    apiVersion: aicr.run/v1alpha2
    kind: Snapshot
    measurements: [...]
```

**Agent Deployment:**  
Kubernetes Job writes snapshots directly to ConfigMap without volumes:
```bash
aicr snapshot --namespace gpu-operator --output cm://gpu-operator/aicr-snapshot
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
- Collectors degrade gracefully: a collector that errors is logged and
  skipped, and its measurement is omitted — a partial snapshot is the
  intended outcome, not a hard failure of the whole snapshot. (The
  orchestrator runs under `errgroup.WithContext` so a future
  cancel-on-error collector is supported, but today per-collector errors
  are swallowed.)
- Each collector sets its own timeout rather than sharing a universal
  one — e.g. 10s (OS, systemd), 60s (Kubernetes), 90s (node topology),
  5s (NFD GPU detection), up to 10m (network discovery)

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
aicr snapshot --namespace gpu-operator --output cm://gpu-operator/aicr-snapshot

# CLI reads from ConfigMap to generate recipe
# (the cm:// URI carries the namespace; `aicr recipe` has no --namespace flag)
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
K8s/node/provider     → service     (provider ID → eks/gke/aks/…)
topology gpu.product  → accelerator (label primary; PCI id fallback)
OS/release/ID         → os          (family)
topology node count   → nodes       (count)
```

`Fingerprint.ToCriteria` projects only **service, accelerator, os, and node
count**. Intent and platform are recipe-author choices the cluster cannot
reveal, so they always resolve to `any` and must be supplied via CLI flags.
Other snapshot fields (K8s server version, OS version, kernel) are captured as
measurements and become constraint *targets*, not recipe criteria.

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
│ RecipeResult (aicr.run/v1alpha2)                          │
├─────────────────────────────────────────────────────────┤
│ metadata:                                               │
│   version: CLI version that generated the recipe        │
│   appliedOverlays: inheritance chain (root to leaf)     │
│   excludedOverlays: matched-but-excluded overlays       │
│                                                         │
│ criteria: Criteria (6 dimensions — see mapping above)   │
│                                                         │
│ constraints: []Constraint                               │
│   ├─ name: constraint identifier (e.g. K8s.server.ver…) │
│   ├─ value: expression (e.g. ">= 1.32.4")               │
│   └─ severity: error | warning                          │
│                                                         │
│ componentRefs: []ComponentRef                           │
│   ├─ name: component name                               │
│   ├─ chart: Helm chart name                             │
│   ├─ source: repository URL or OCI reference            │
│   ├─ version: chart/component version                   │
│   └─ namespace: deploy namespace                        │
│                                                         │
│ deploymentOrder: []string                               │
│   └─ topologically sorted component names               │
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
    --namespace gpu-operator \
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
    --namespace gpu-operator \
    --recipe recipe.yaml \
    --snapshot cm://gpu-operator/aicr-snapshot

# Exit code: 0 = all passed; 8 (ExitInternal, from ErrCodeInternal) when
#   one or more phases did not pass
# Use --fail-on-error=false for informational mode without failing
```

## Stage 4: Bundle (Data Packaging)

### Bundler Framework

```
RecipeResult
  -> DefaultBundler (one invocation): for each component,
       GetComponentRef (name, version) + GetValuesForComponent (values map)
  -> selected deployer writes its own layout (Helm deployer shown below;
       argocd/argocd-helm/flux/helmfile differ):
         - static values       -> <NNN-component>/values.yaml
         - dynamic/per-cluster -> <NNN-component>/cluster-values.yaml
         - component manifests -> <NNN-component>/   (e.g. ClusterPolicy or a
                                   CR, for components that ship one)
         - go:embed templates  -> per-component install.sh, and the root
                                   README.md + deploy.sh
  -> write canonical recipe.yaml (Helm deployer only)
  -> finalize root checksums.txt over every regular payload file
```

`pkg/bundler/registry` exists but is **not** used by the production path: the
default flow constructs a single `DefaultBundler`, extracts values for every
component in one `DefaultBundler` invocation, builds one deployer
(helm/argocd/argocd-helm/flux/helmfile), and invokes it once. Static values land in `values.yaml`; dynamic, per-cluster values land in
`cluster-values.yaml`. Every component is handled in that single invocation, not by separate
per-component bundlers. The per-component file layout above is the **Helm**
deployer's; argocd/argocd-helm/flux/helmfile emit their own layouts.
Finalization treats each deployer output as a closed-world inventory:
`checksums.txt` lists every regular payload file, including `recipe.yaml` when
present, and verification rejects additional files, directories, symlinks, or
other non-regular objects outside the exact allowed metadata paths.

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

**Template usage:** per-component `values.yaml` is the component's values map
marshaled to YAML directly (not a Go-templated file). `README.md` and
`deploy.sh` are rendered from `readmeTemplateData` / `deployTemplateData`,
which expose fields like `RecipeVersion`, `Components`, and `Constraints`:

```gotemplate
# README.md (readmeTemplateData)
Recipe version: {{ .RecipeVersion }}
{{- range .Components }}
- {{ .Name }} {{ .Version }}
{{- end }}
```

#### Template data

Scripts and READMEs are rendered from embedded templates using per-output
structs defined in `pkg/bundler/deployer/helm/helm.go` — `readmeTemplateData`
(recipe/bundler version, components, constraints) for `README.md`, and
`deployTemplateData` (bundler version, components, readiness timeout) for
`deploy.sh`. There is no `ScriptData` type.

### Bundle Structure

The deployer generates the final output structure. See [Deployer-Specific Output](#deployer-specific-output) for details per deployer type.

## Stage 5: Deployment (GitOps Integration)

### Deployer Framework

After bundlers generate artifacts, the deployer framework transforms them into deployment-specific formats based on the `--deployer` flag.

```
Bundle artifacts + recipe (deploymentOrder, componentRefs)
  → deployer selected by --deployer:
      helm (default) — per-component Helm charts + root deploy.sh/README
      argocd         — Argo CD App-of-Apps + sync-waves
      argocd-helm    — Helm-chart app-of-apps (values overridable at install)
      flux           — Flux HelmRelease manifests
      helmfile       — helmfile.yaml release graph
  → numbered NNN-<component>/ output + closed-world root checksums.txt

Each component folder holds install.sh, values.yaml, and cluster-values.yaml
(there is no scripts/ subdirectory).
```

### Deployment Order Flow

Ordering follows each component's declared `dependencyRefs`, not its linear position. The flat `deploymentOrder` (one topological serialization) numbers the `NNN-<name>/` folders and drives the serial helm `deploy.sh`. The concurrent deployers derive ordering from the dependency graph so independent components roll out together, but differ in how faithfully: **argocd**, **argocd-helm**, and **helmfile** approximate it as depth **tiers** (a component waits for its whole prior tier), while **flux** projects the exact `dependencyRefs` DAG onto `dependsOn` (a component waits for only its actual dependencies). Each deployer expresses this differently:

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
│  Per-component dirs                sync-wave (by level):│
│  + deploy.sh script                - cert-manager:1     │
│                                    - gpu-operator:5     │
│                                    - network-op:9       │
│                                                         │
└─────────────────────────────────────────────────────────┘
```

The `orderComponentsByDeployment()` sort shown above produces the flat order used for folder numbering and the helm `deploy.sh`. The Argo CD sync-waves are **not** taken from that linear order: they are assigned by dependency tier (`wave = tier*4 + phase`), so components that share a tier share a wave and sync together. The `1, 5, 9` values above reflect the specific `cert-manager → gpu-operator → network-operator` dependency chain (one component per tier); a recipe with independent components would place them at the same wave. See [Deployment ordering](../contributor/component.md#deployment-ordering) for the full model.

### Deployer-Specific Output

**Helm Deployer** (default):
```
bundle-output/
├── README.md                    # Root deployment guide with ordered steps
├── deploy.sh                    # Automation script (0755)
├── recipe.yaml                  # Canonical post-resolution recipe
├── checksums.txt                # SHA256 of every regular payload file
├── 001-cert-manager/
│   ├── install.sh               # Per-folder install script (0755)
│   ├── values.yaml              # Static Helm values
│   ├── cluster-values.yaml      # Per-cluster dynamic values
│   └── upstream.env             # CHART/REPO/VERSION (upstream-helm folder)
├── 002-gpu-operator/
│   ├── install.sh
│   ├── values.yaml
│   ├── cluster-values.yaml
│   └── upstream.env
└── 003-network-operator/
    ├── install.sh
    ├── values.yaml
    ├── cluster-values.yaml
    └── upstream.env
```

Folder names carry the `NNN-<component>/` prefix (the number encodes
deployment order). Local-chart components instead ship `Chart.yaml` +
`templates/` in place of `upstream.env`.

**Argo CD Deployer**:
```
bundle-output/
├── app-of-apps.yaml          # Parent Application (bundle root)
├── checksums.txt              # SHA256 of every regular payload file
├── 001-cert-manager/
│   ├── values.yaml
│   └── application.yaml      # With sync-wave annotation
├── 002-gpu-operator/
│   ├── values.yaml
│   ├── manifests/
│   └── application.yaml      # With sync-wave annotation
├── 003-network-operator/
│   ├── values.yaml
│   └── application.yaml      # With sync-wave annotation
└── README.md
```

Component folders use the same numbered `NNN-<name>/` prefix as the Helm
deployer (the number encodes deployment order), and each folder holds its
`application.yaml` directly — there is no nested `argocd/` subdirectory.

Argo CD Application with multi-source:
```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: gpu-operator
  annotations:
    argocd.argoproj.io/sync-wave: "5"  # tier 1 (depends on cert-manager in tier 0)
spec:
  sources:
    # Helm chart from upstream
    - repoURL: https://helm.ngc.nvidia.com/nvidia
      targetRevision: v26.3.2
      chart: gpu-operator
      helm:
        valueFiles:
          - $values/002-gpu-operator/values.yaml
    # Values from GitOps repo
    - repoURL: <YOUR_GIT_REPO>
      targetRevision: main
      ref: values
    # Additional manifests (if present)
    - repoURL: <YOUR_GIT_REPO>
      targetRevision: main
      path: 002-gpu-operator/manifests
```

### Deployer Data Flow

```
┌──────────────────────────────────────────────────────────────┐
│ Complete Bundle + Deploy Flow                                │
├──────────────────────────────────────────────────────────────┤
│                                                              │
│  aicr bundle -r recipe.yaml --deployer argocd \              │
│    --repo https://github.com/my-org/my-repo.git -o ./out     │
│                                                              │
│  1. Parse recipe                                             │
│     └─ Extract componentRefs + deploymentOrder               │
│                                                              │
│  2. Order components                                         │
│     └─ orderComponentsByDeployment()                         │
│                                                              │
│  3. Bundle (single DefaultBundler, all components)           │
│     ├─ cert-manager   → values.yaml, manifests/              │
│     ├─ gpu-operator   → values.yaml, manifests/              │
│     └─ network-operator → values.yaml, manifests/            │
│                                                              │
│  4. Run deployer (argocd) → numbered NNN-<name>/ folders     │
│     ├─ 001-cert-manager/application.yaml (wave: 1)          │
│     ├─ 002-gpu-operator/application.yaml (wave: 5)          │
│     └─ 003-network-operator/application.yaml (wave: 9)      │
│     └─ app-of-apps.yaml (bundle root, uses --repo URL)       │
│                                                              │
│  5. Finalize closed-world inventory                          │
│     └─ checksums.txt (root; every regular payload file)      │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

## Data Serialization

### Formats Supported

**JSON:**
```json
{
  "apiVersion": "aicr.run/v1alpha2",
  "kind": "Snapshot",
  "measurements": [...]
}
```

**YAML:**
```yaml
apiVersion: aicr.run/v1alpha2
kind: Snapshot
measurements:
  - type: K8s
    subtypes: [...]
```

**Table (Human-readable):**
```
TYPE    SUBTYPE      KEY                    VALUE
K8s     image        gpu-operator           v25.3.3
K8s     image        driver                 580.82.07
GPU     hardware     gpu-present            true
GPU     hardware     gpu-count              8
GPU     hardware     model                  H100
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
   ├─ Recipe/query routes
   │  ├─ Parse query parameters
   │  ├─ Build Query
   │  ├─ recipe.Builder.Build(ctx, query)
   │  ├─ Serialize response
   │  └─ Return JSON
   └─ POST /v1/bundle
      ├─ Decode a fully hydrated RecipeResult
      ├─ Generate and finalize the bundle inventory
      ├─ Stage one private revalidated inventory snapshot
      ├─ Stream only its required directories and regular files to ZIP
      └─ Derive X-Bundle-Files and X-Bundle-Size from that inventory
```

The bundle response includes `recipe.yaml` when the deployer emits it and
excludes every unverified entry. `X-Bundle-Files` is the number of verified
regular files, and `X-Bundle-Size` is their aggregate uncompressed byte size.
The server rejects an additional file or directory, symlink, or other
non-regular object before the ZIP response is published.

### Response Headers

```
HTTP/1.1 200 OK
Content-Type: application/json
X-Request-Id: 550e8400-e29b-41d4-a716-446655440000
Cache-Control: public, max-age=600
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
- The per-`Client` metadata-store and component-registry caches persist for
  the lifetime of the `Client` (keyed on `DataProvider` identity) and are
  released only by `Client.Close()` — they do not expire on a timer
- The HTTP recipe/query response `Cache-Control: max-age` is `600` seconds
  (`defaults.RecipeCacheTTL`, 10 minutes) — a downstream/browser caching hint,
  distinct from the in-process caches above

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
