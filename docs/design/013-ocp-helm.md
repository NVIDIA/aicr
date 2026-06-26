# ADR-013: OpenShift Support via In-Tree Helm Charts

## Status

+**Accepted** — 2026-06-16 (implemented in OCP support PR stack).

## Problem

AICR does not support OpenShift Container Platform (OCP). OCP operators are
installed through the Operator Lifecycle Manager (OLM) — Subscriptions,
CatalogSources, and OperatorGroups — not Helm charts. The existing bundler
emits Helm-only artifacts (upstream-helm or local-helm folders). There is no
mechanism to express an OLM install followed by a Custom Resource
configuration step within the current bundle structure.


## Rationale: OLM Lifecycle via In-Tree Helm Templates

OLM resources (Subscriptions, OperatorGroups, CatalogSources) and the
Custom Resources they manage are plain Kubernetes objects. This design
models them as in-tree Helm chart templates — the same mechanism AICR
already uses for every other component. No OLM-specific deployer,
adapter, or shell-based polling logic is required.

This means the full OLM install lifecycle — creating a Subscription,
waiting for the CSV to reach `Succeeded`, then applying the operator's
CR — is expressed entirely through the existing bundler and deployer
pipeline.

### How in-tree Helm charts work

The bundler already supports manifest-only components via `KindLocalHelm`.
When a component has `defaultRepository: ""` in the registry and the overlay
provides `manifestFiles`, the bundler:

1. Reads the raw Helm-templated YAML files listed in `manifestFiles`
2. Generates a wrapper `Chart.yaml`
3. Places the templates into `templates/`
4. Writes `values.yaml` (from the component's `valuesFile`) and `install.sh`
5. Emits a fully valid local Helm chart deployable with `helm upgrade --install ./`

Existing components like `gke-nccl-tcpxo` and `nodewright-customizations`
already use this pattern. OCP components follow the same mechanism.

## Decision

Model each OCP operator as **two in-tree local Helm charts**, each a
separate registry component:

1. **Phase 1 — OLM chart (`*-ocp-olm`):** A local Helm chart whose
   templates contain the OLM resources (OperatorGroup, Subscription,
   optionally CatalogSource). Values control channel,
   version, approval strategy, and source catalog. A `readiness.yaml`
   gates on the operator CSV reaching `Succeeded` before subsequent
   components deploy.

2. **Phase 2 — CR chart (`*-ocp`):** A local Helm chart whose templates
   contain the operator's Custom Resource(s) (e.g., `ClusterPolicy` for
   GPU Operator, `NodeFeatureDiscovery` for NFD). Values control
   the CR spec. Applied after Phase 1 completes via `dependencyRefs`.

The OCP overlay also disables base components that are replaced by OCP
equivalents or not needed on the platform (monitoring, scheduling, DRA,
etc.) using `overrides: { enabled: false }`.

### Bundle output

The bundler emits the standard numbered-folder structure. For a component
`gpu-operator-ocp`:

```text
001-gpu-operator-ocp-olm/              # KindLocalHelm — OLM resources
    Chart.yaml
    templates/
        operatorgroup.yaml
        subscription.yaml
    values.yaml
    install.sh                         # includes --create-namespace
002-gpu-operator-ocp-olm-readiness/    # Readiness gate — waits for CSV Succeeded
    Chart.yaml
    templates/
        check-job.yaml
    install.sh                         # helm install --wait --wait-for-jobs
003-gpu-operator-ocp/                  # KindLocalHelm — Custom Resource
    Chart.yaml
    templates/
        clusterpolicy.yaml
    values.yaml
    install.sh
```

The readiness folder (`002-*-readiness`) is emitted automatically by the
localformat writer when `--readiness-hooks` is enabled and the component
has a `readiness.yaml`. No custom wait logic is needed.

All deployers (Helm, Argo CD, Helmfile) consume this structure unchanged —
each folder is a standard local Helm chart.

## Architecture

### Registry representation

Each OCP operator is registered as **two components** with an explicit
dependency:

```yaml
# recipes/registry.yaml
- name: gpu-operator-ocp-olm
  displayName: GPU Operator OCP (OLM)
  valueOverrideKeys: [gpuoperatorocpolm]
  helm:
    defaultRepository: ""           # in-tree local chart
    defaultNamespace: gpu-operator

- name: gpu-operator-ocp
  displayName: GPU Operator OCP (CR)
  valueOverrideKeys: [gpuoperatorocp]
  helm:
    defaultRepository: ""           # in-tree local chart
    defaultNamespace: gpu-operator
```

Dependency ordering (`dependencyRefs`) is declared in the overlay, not the
registry, because it is specific to the OCP deployment topology.

### In-tree chart layout

```text
recipes/components/
├── gpu-operator-ocp-olm/
│   ├── values.yaml                # channel, source, approval defaults
│   ├── readiness.yaml             # gates on CSV reaching Succeeded
│   └── manifests/
│       ├── operatorgroup.yaml     # Helm template: {{ $v.operatorGroup.* }}
│       └── subscription.yaml      # Helm template: {{ $v.subscription.* }}
├── gpu-operator-ocp/
│   ├── values.yaml                # ClusterPolicy spec defaults
│   └── manifests/
│       └── clusterpolicy.yaml     # Helm template: {{ .Values.spec.* }}
```

Manifests are **Helm templates** rendered by the bundle rendering engine
(`pkg/manifest/render.go`). The localformat writer places them into the
generated chart's `templates/` directory.

**Important:** Templates must use the `$v := index .Values "<component-name>"`
pattern — not bare `.Values.xxx` references. This is required because the
bundle rendering engine nests each component's values under the component
name as a top-level key in `.Values`:

```go
// pkg/manifest/render.go — the renderer wraps values per component
data := templateData{
    Values: map[string]any{input.ComponentName: input.Values},
    ...
}
```

Because `.Values` is `{"gpu-operator-ocp-olm": {…actual config…}}` rather
than a flat dictionary, templates must extract the component subtree first:

```yaml
{{- $v := index .Values "gpu-operator-ocp-olm" }}
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: {{ $v.subscription.name }}          # ✓ correct — accesses nested values
  name: {{ .Values.subscription.name }}     # ✗ wrong  — subscription is not a top-level key
```

This convention is consistent with all existing in-tree manifest templates
(e.g., `nodewright-customizations`, `gke-nccl-tcpxo`).

Overlays reference these manifests via `manifestFiles` in `componentRefs`.
This is the existing mechanism used by `gke-nccl-tcpxo`,
`nodewright-customizations`, and others.

This enables the full overlay system: overlays reference overlay-specific
values files via `valuesFile` in `componentRefs`, and users can customize
at bundle time via `--set gpuoperatorocp:spec.driver.version=570.86.16`.

### Overlay structure

OCP overlays follow the existing pattern. A new `service: ocp` criteria
value is added. This is a new enum value that must be propagated to
`CriteriaServiceType` in `pkg/recipe/criteria.go` and to the `service`
enum in the OpenAPI contract (`api/aicr/v1/server.yaml`). The overlay
disables base components that are replaced by OCP equivalents or not
applicable to the platform:

```yaml
# recipes/overlays/ocp.yaml
kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: ocp
spec:
  criteria:
    service: ocp
  componentRefs:
    # OLM + CR pairs with manifestFiles
    - name: nfd-ocp-olm
      type: Helm
      valuesFile: components/nfd-ocp-olm/values.yaml
      manifestFiles:
        - components/nfd-ocp-olm/manifests/operatorgroup.yaml
        - components/nfd-ocp-olm/manifests/subscription.yaml
    - name: nfd-ocp
      type: Helm
      valuesFile: components/nfd-ocp/values.yaml
      manifestFiles:
        - components/nfd-ocp/manifests/nodefeaturediscovery.yaml
      dependencyRefs: [nfd-ocp-olm]
    
    # ... additional disabled components omitted for brevity

```

### Deployer behavior

No deployer changes are required. Both phases emit `KindLocalHelm` folders,
which all existing deployers that support `--readiness-hooks` already
handle:

| Deployer | Phase 1 (OLM) | Readiness gate | Phase 2 (CR) |
|----------|---------------|----------------|--------------|
| Helm | `helm upgrade --install` via `deploy.sh` | `--wait --wait-for-jobs` (readiness Job) | `helm upgrade --install` |
| Argo CD | `Application` CR, sync-wave N | `Application` CR, sync-wave N+1 | `Application` CR, sync-wave N+2 |

Helmfile and Flux are **not supported** with `--readiness-hooks` — the
bundler rejects the combination at runtime. Without readiness hooks the
OLM and CR charts still deploy, but there is no gate between them.

### Why OCP components use separate names

OCP operators cannot reuse the base component names (`nfd`, `gpu-operator`)
because the overlay merge treats `source: ""` as "not set" rather than
"explicitly empty." When the OCP overlay references `nfd` with
`source: ""`, the merge preserves the base's upstream Helm repository URL,
producing a mixed component (upstream chart + manifestFiles) that emits
an unwanted `-post` folder.

Instead, OCP components use dedicated names (`nfd-ocp-olm`, `nfd-ocp`,
`gpu-operator-ocp-olm`, `gpu-operator-ocp`) and the OCP overlay disables
the base components via `overrides: { enabled: false }`. The bundler
skips disabled components during bundle generation.

### OLM readiness gate

The OLM component carries a `readiness.yaml` using the same Chainsaw
assertion pattern as the existing `gpu-operator` readiness gate. The
localformat writer emits this as a `-readiness` folder between the OLM and
CR folders when `--readiness-hooks` is enabled:

```yaml
# recipes/components/gpu-operator-ocp-olm/readiness.yaml
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: gpu-operator-ocp-olm-readiness
spec:
  timeouts:
    assert: 30s
  steps:
    - name: csv-succeeded
      try:
        - assert:
            resource:
              apiVersion: operators.coreos.com/v1alpha1
              kind: ClusterServiceVersion
              metadata:
                namespace: gpu-operator
              status:
                phase: Succeeded
```

This reuses the existing readiness hooks infrastructure — no new wait
mechanisms or deployer changes are needed.

### Values and template examples

**OLM values** (`recipes/components/gpu-operator-ocp-olm/values.yaml`):

```yaml
namespace: nvidia-gpu-operator
subscription:
  name: gpu-operator-certified
  channel: v24.9
  source: certified-operators
  sourceNamespace: openshift-marketplace
  installPlanApproval: Automatic
operatorGroup:
  name: gpu-operator-group
  targetNamespaces: []             # empty = AllNamespaces
```

**OLM Subscription template** (`recipes/components/gpu-operator-ocp-olm/manifests/subscription.yaml`):

```yaml
{{- $v := index .Values "gpu-operator-ocp-olm" }}
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: {{ $v.subscription.name }}
  namespace: {{ $v.namespace }}
  annotations:
    helm.sh/hook: post-install,post-upgrade
    helm.sh/hook-weight: "10"
    helm.sh/hook-delete-policy: before-hook-creation
  labels:
    app.kubernetes.io/managed-by: {{ .Release.Service }}
spec:
  channel: {{ $v.subscription.channel }}
  name: {{ $v.subscription.name }}
  source: {{ $v.subscription.source }}
  sourceNamespace: {{ $v.subscription.sourceNamespace }}
  installPlanApproval: {{ $v.subscription.installPlanApproval }}
```

**CR values** (`recipes/components/gpu-operator-ocp/values.yaml`):

CR values files should mirror the same keys and structure used in the
upstream Helm chart values for other services (e.g., EKS `values.yaml`).
This ensures consistency across services: the same knobs (`driver`,
`toolkit`, `dcgm`, `dcgmExporter`, `cdi`, `hostPaths`, etc.) appear in
OCP CR values as in EKS/AKS/GKE Helm values, even though the underlying
mechanism differs (ClusterPolicy CR fields vs. Helm chart values). This
makes overlay-specific values files (e.g., `values-training.yaml`)
portable across services where possible and keeps the `--set` override
keys familiar to users.

```yaml
name: gpu-cluster-policy
spec:
  operator:
    defaultRuntime: crio
  driver:
    enabled: true
    version: "570.86.16"
  toolkit:
    enabled: true
  devicePlugin:
    enabled: true
  dcgm:
    enabled: true
  dcgmExporter:
    enabled: true
  migManager:
    enabled: true
  gdrcopy:
    enabled: true
  nodeStatusExporter:
    enabled: true
  gfd:
    enabled: true
```

**CR template** (`recipes/components/gpu-operator-ocp/manifests/clusterpolicy.yaml`):

```yaml
{{- $v := index .Values "gpu-operator-ocp" }}
apiVersion: nvidia.com/v1
kind: ClusterPolicy
metadata:
  name: {{ $v.name }}
  annotations:
    helm.sh/hook: post-install,post-upgrade
    helm.sh/hook-weight: "10"
    helm.sh/hook-delete-policy: before-hook-creation
  labels:
    app.kubernetes.io/managed-by: {{ .Release.Service }}
spec: {{ $v.spec | toYaml | nindent 2 }}
```

**Training overlay values** (`recipes/components/gpu-operator-ocp/values-training.yaml`):

```yaml
spec:
  migManager:
    enabled: true
  gdrcopy:
    enabled: true
```

Overlays reference values files via `valuesFile` in `componentRefs`. Users
can further customize at bundle time:
`--set gpuoperatorocp:spec.driver.version=570.86.16`.

**Note:** Users who prefer raw manifests over Helm-based deployment can run
`helm template <release> <bundle-folder>` on any emitted `KindLocalHelm`
folder to produce plain Kubernetes YAML with all values resolved. This
works outside of AICR with a standard Helm installation.

## Component Matrix (Initial Scope)

| Operator | OLM Component | CR Component | CR Kind |
|----------|--------------|--------------|---------|
| Node Feature Discovery | `nfd-ocp-olm` | `nfd-ocp` | `NodeFeatureDiscovery` |
| GPU Operator | `gpu-operator-ocp-olm` | `gpu-operator-ocp` | `ClusterPolicy` |

Additional operators use the same two-phase pattern. Each operator pair
is self-contained — the OLM chart installs the operator, the CR chart
configures it.



## Testing Strategy

| Layer | Coverage |
|-------|----------|
| Unit (Go) | `pkg/recipe`: overlay resolution for `service: ocp` criteria. Table-driven tests for OCP overlay chain. |
| Chainsaw (CLI) | New `tests/chainsaw/cli/bundle-ocp/`: generate OCP recipe, bundle, verify folder structure + manifest content. |
| KWOK | Add `ocp-training` to recipe set once overlays exist. OLM CRDs are not present in KWOK — test validates bundle structure, not OLM reconciliation. |

## Acceptance Criteria

1. `aicr recipe --service ocp --intent training`
   produces a recipe with OLM + CR component pairs and disabled base
   components.
2. `aicr bundle -r recipe.yaml --readiness-hooks -o ./bundles` emits
   numbered folders with valid Helm charts for OLM, readiness gate, and
   CR phases (disabled components skipped).
3. `aicr bundle -r recipe.yaml --readiness-hooks --deployer argocd -o ./bundles`
   emits Argo CD Applications with correct sync-wave ordering (OLM →
   readiness → CR).
4. `make qualify` passes.
5. Chainsaw tests verify folder structure and manifest content for OCP
   bundles.

## Consequences

### Positive

- No new deployer code — OLM resources flow through the existing
  `KindLocalHelm` pipeline unchanged.
- Full overlay and `--set` customization works out of the box because
  OLM manifests are standard Helm templates with values.
- Readiness gating reuses the Chainsaw infrastructure already shipping
  for non-OCP components — no custom wait/polling logic.
- CR values files can share structure with EKS/AKS/GKE values, keeping
  overlay-specific values files portable across services.

### Negative

- The Helm bundle serves as a reference and validation artifact, but many
  OpenShift users apply plain Kubernetes manifests directly. They can
  run `helm template` on the emitted charts to produce static YAML, but
  this is an extra step compared to a raw-manifest output mode.
- Each OCP operator requires two registry entries (OLM + CR) instead of
  one, increasing registry surface area.
- In-tree Helm templates for OLM resources must be maintained in sync
  with upstream operator channel/version changes.
- OCP components cannot reuse base component names (`nfd`,
  `gpu-operator`) because the overlay merge treats `source: ""` as "not
  set" and preserves the base's upstream repository URL, producing a
  mixed component with an unwanted `-post` folder. Separate names
  (`*-ocp-olm`, `*-ocp`) with `enabled: false` on the base components
  are the workaround.

### Neutral

- Bundle folder count grows (three folders per operator: OLM, readiness,
  CR) but each folder is a standard local Helm chart — no new kinds.

## Alternatives Considered

### OLM-specific deployer with direct `kubectl apply` (rejected)

A custom deployer that applies OLM manifests via `kubectl apply` and
polls for CSV status via shell scripts (`install-direct.sh`).

Rejected because it introduces a new deployer surface (`direct`)
that the project is actively shrinking (see #899, #904).


