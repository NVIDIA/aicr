# Recipe Development Guide

This guide covers how to create, modify, and validate recipe metadata.

## Quick Start: Contributing a Recipe

New to recipe development? Follow these minimal steps to contribute:

**1. Copy an existing overlay** ([details](#working-with-recipes))
```bash
cp recipes/overlays/h100-eks-ubuntu-training.yaml recipes/overlays/gb200-eks-ubuntu-training.yaml
```

**2. Edit criteria and components** ([criteria](#recipe-structure), [components](#component-configuration))
```yaml
# recipes/overlays/gb200-eks-ubuntu-training.yaml
spec:
  base: eks-training  # Inherit from intermediate recipe
  criteria:
    service: eks
    accelerator: gb200  # Changed from h100
    os: ubuntu
    intent: training
  componentRefs:
    - name: gpu-operator
      valuesFile: components/gpu-operator/eks-gb200-training.yaml
      overrides:
        driver:
          version: "580.82.07"  # GB200-specific driver
```

**3. Run tests** ([details](#testing-and-validation))
```bash
make test  # Validates schema, criteria, references, constraints
make qualify  # Includes end-to-end tests before submitting
```

**4. Open PR** ([best practices](#best-practices))
- Include test output showing recipe generation works
- Explain why the recipe is needed (new hardware, workload, platform)

---

## Overview

Recipe metadata files define component configurations for GPU-accelerated Kubernetes deployments using a **base-plus-overlay architecture** with three composition mechanisms — single-parent inheritance, explicit mixin composition, and criteria-wildcard matching:

- **Base values** (`overlays/base.yaml`) - universal defaults
- **Intermediate recipes** (`eks.yaml`, `eks-training.yaml`) - shared configurations for categories
- **Leaf recipes** (`gb200-eks-ubuntu-training.yaml`) - hardware/workload-specific overrides
- **Mixins** (`mixins/*.yaml`) - composable fragments (OS constraints, platform components) that leaf overlays reference via `spec.mixins` instead of duplicating content
- **Criteria-wildcard overlays** (`gb200-any.yaml`) - cross-cutting overlays picked up automatically by the resolver when their wildcard criteria match the query, without being referenced via `spec.base` or `spec.mixins`
- **Inline overrides** - per-recipe customization without new files

Recipe files in `recipes/` are embedded at compile time. Integrators can extend or override using the `--data` flag (see [Advanced Topics](#advanced-topics)).

For query matching and overlay merging internals, see [Data Architecture](../contributor/recipe.md).

## Recipe Structure

### Multi-Level Inheritance

Recipes use `spec.base` to inherit configurations. Chains progress from general (base) to specific (leaf):

```
base.yaml → eks.yaml → eks-training.yaml → gb200-eks-ubuntu-training.yaml
```

**Intermediate recipes** (partial criteria) capture shared configs:
```yaml
# eks-training.yaml
spec:
  base: eks
  criteria:
    service: eks
    intent: training  # Partial - no accelerator/OS
  componentRefs:
    - name: gpu-operator
      valuesFile: components/gpu-operator/values-eks-training.yaml
```

**Leaf recipes** (complete criteria) match user queries:
```yaml
# gb200-eks-ubuntu-training.yaml
spec:
  base: eks-training  # Inherits from intermediate
  criteria:
    service: eks
    accelerator: gb200
    os: ubuntu
    intent: training  # Complete
  componentRefs:
    - name: gpu-operator
      overrides:
        driver:
          version: "580.82.07"  # Hardware-specific override
```

**Leaf recipes with mixins** compose shared fragments:
```yaml
# h100-eks-ubuntu-training-kubeflow.yaml
spec:
  base: h100-eks-ubuntu-training
  mixins:
    - os-ubuntu          # Shared Ubuntu constraints (from recipes/mixins/)
    - platform-kubeflow  # Kubeflow trainer component (from recipes/mixins/)
  criteria:
    service: eks
    accelerator: h100
    os: ubuntu
    intent: training
    platform: kubeflow
```

Mixins use `kind: RecipeMixin` and carry only `constraints` and `componentRefs`. They live in `recipes/mixins/` and are applied after inheritance chain merging. See [Data Architecture](../contributor/recipe.md#mixin-composition) for details.

Some platforms declare their full component stack inline per leaf overlay rather than via a platform mixin. This is the case for `--platform slurm` and `--platform dynamo`, where each leaf carries hardware-specific tuning (GPU GRES strings, accelerator resource limits) that the mixin merge path cannot represent cleanly. Other shapes like `--platform kubeflow` and `--intent inference` still use the `platform-kubeflow` / `platform-inference` mixins shown above, since their leaf-specific tuning is minimal.

For example, `--platform slurm` leaves inline three `componentRefs`:

- `slinky-slurm-operator-crds` — SchedMD Slinky CRDs
- `slinky-slurm-operator` — the operator and admission webhook
- `slinky-slurm` — the Slinky-managed Slurm cluster instance (Controller / LoginSet / NodeSet / RestApi), with leaf-specific `overrides` (e.g. GPU GRES wiring on `nodesets.slinky` and `controller.extraConfMap`)

This is the same shape `dynamo-platform` uses across the `*-inference-dynamo` leaves. See `recipes/overlays/h100-eks-ubuntu-training-slurm.yaml` for the full example.

When authoring a recipe targeting Talos (`criteria.os: talos`), append the `os-talos` mixin to your overlay's `spec.mixins` list (e.g. `spec.mixins: [os-talos]`, or `[platform-kubeflow, os-talos]` if you already mix in a non-OS fragment). OS-scoped mixins are mutually exclusive — combining `os-ubuntu` and `os-talos` in one overlay is a recipe authoring error, not a supported composition. The mixin overrides namespaces for affected components and supplies PSA-privileged Namespace manifests via `componentRefs[].preManifestFiles`, which are applied before each chart — see [Talos integration](talos-integration.md) for the component list and labels.

**Cross-cutting overlays with wildcard criteria** apply across one criteria dimension without being referenced via `spec.base` or listed in `spec.mixins`. The resolver can return multiple independent maximal-leaf overlays for a single query, so a `service: any` overlay is picked up alongside the service-specific maximal leaf and its inheritance chain:

```yaml
# gb200-any.yaml — applies to every GB200 query (any service, any intent)
spec:
  base: base
  criteria:
    service: any         # Wildcard — matches eks, oke, gke, etc.
    accelerator: gb200
  validation:
    deployment:
      checks:
        - operator-health
        - expected-resources
        - gpu-operator-version
        - check-nvidia-smi
      constraints:
        - name: Deployment.gpu-operator.version
          value: ">= v25.10.0"
```

Only use this pattern when the content is truly uniform across the wildcard dimension — if values diverge per service, keep them inline in each service-specific overlay. NCCL performance thresholds, for example, are explicitly **not** a good fit for this pattern: each service has a different network fabric (EFA, TCPXO, RoCE, etc.) and the same bandwidth number is rarely correct across two fabrics. The intent-scoped `gb200-any-training.yaml` and `b200-any-training.yaml` shapes that previously carried cross-service NCCL thresholds were retired (`gb200-any-training` in #1052, `b200-any-training` in #1053) in favor of per-leaf performance blocks. See [Data Architecture](../contributor/recipe.md#criteria-wildcard-overlays) for when to use wildcard overlays vs mixins.

**Merge order:** `base.yaml` (lowest) → intermediate → leaf → mixins (highest)

**Merge rules:**
- Constraints: same-named overridden, new added
- ComponentRefs: same-named merged field-by-field, new added
- `validation.<phase>` blocks merge per-field: `checks` and `constraints` union and deduplicate when non-empty (`constraints` by name, overlay wins on same-name); an explicit empty list (`checks: []` / `constraints: []`) **clears** the inherited list, while an omitted/null field **inherits** it; `nodeSelection` replaced wholesale when set; `timeout`/`infrastructure` overlay-wins-if-non-empty
- Criteria: not inherited (each recipe defines its own)
- Mixin constraints/components must not conflict with the inheritance chain or other mixins

### Slinky Slurm Inline Components

Leaves that need topology-aware scheduling can optionally add `slinky-topograph` as a fourth `componentRef`. Its `dependencyRefs` include `slinky-slurm`, so it deploys **after** the Slurm cluster chart: `slinky-slurm` renders and owns the `slinky-slurm-config-extra` ConfigMap (from its `configFiles`, mounted into slurmctld via the Controller CR's `configFileRefs`), and Topograph patches only that ConfigMap's `topology.conf` key on each sync, preserving the chart-owned `cgroup.conf`/`gres.conf` keys — Helm has to own the ConfigMap first, or a later `helm upgrade` on `slinky-slurm` would fight Topograph for ownership. The `overrides` block supplies the provider and engine that are specific to each leaf:

```yaml
- name: slinky-topograph
  type: Helm
  valuesFile: components/slinky-topograph/values.yaml
  dependencyRefs:
    - slinky-slurm-operator
    - slinky-slurm-operator-crds
    - slinky-slurm   # deploy after Slurm so Helm already owns config-extra
  overrides:
    global:
      provider:
        name: gcp      # cloud provider: gcp | aws | oci | nebius | …
      engine:
        name: slinky   # scheduler consumer: slinky | slurm | k8s | graph

- name: slinky-slurm
  type: Helm
  valuesFile: components/slinky-slurm/values.yaml
  dependencyRefs:
    - slinky-slurm-operator
    - slinky-slurm-operator-crds
  overrides:
    configFiles:
      # Seed so topology.conf exists in slinky-slurm-config-extra from first
      # boot; Topograph overwrites this key after each successful sync.
      # NOTE: helm upgrade resets the key to this seed until the next
      # Topograph sync.
      topology.conf: |
        # Managed by NVIDIA Topograph (engine: slinky). Pre-sync placeholder.
        SwitchName=aicr-preseed Nodes=aicr-preseed-node
    controller:
      extraConfMap:
        TopologyPlugin: "topology/tree"
      # Reload path: the reconfigure sidecar loads Topograph's topology.conf
      # updates into the running slurmctld (rollout hash excludes configFileRefs).
      inplaceReconfigure: true
    # ... GPU GRES and other leaf-specific tuning ...
```

For cloud providers (gcp, aws, oci, nebius, …), `slinky-topograph` requires IAM access to call the cloud's topology API. For GKE, bind a GCP service account that has `roles/compute.viewer` on the project via Workload Identity:

```yaml
overrides:
  global:
    provider:
      name: gcp
  serviceAccount:
    annotations:
      iam.gke.io/gcp-service-account: <sa-name>@<project-id>.iam.gserviceaccount.com
```

If you prefer not to bake the GCP service account into the recipe, supply it at bundle time instead:

```bash
aicr bundle --recipe recipe.yaml \
  --set-json 'slinkytopograph:serviceAccount.annotations={"iam.gke.io/gcp-service-account":"<sa-name>@<project-id>.iam.gserviceaccount.com"}' \
  -o ./bundle
```

For `provider.name: dra` (Kubernetes Dynamic Resource Allocation, GA in K8s 1.34), topology is sourced from the DRA API — no cloud provider IAM or ServiceAccount annotations are needed. Use `dra` for clusters actually running DRA drivers with GPU resource claims. Kind-based CI clusters should use the `test` provider with a model fixture instead (as this repo's `h100-kind-training-slurm` overlay does) — a CPU-only Kind cluster has no DRA resources for the `dra` provider to read.

### Inference performance constraints

The `inference-perf` performance check reads named entries from
`validation.performance.constraints`. Two are pass/fail **thresholds**
(comparator values, 10% tolerance applied by the evaluator) and the rest are
optional **inputs** that tune the benchmark per accelerator (bare values, no
comparator):

```yaml
validation:
  performance:
    checks: [inference-perf]
    constraints:
      - name: inference-throughput          # >= only; output tokens/sec
        value: ">= 50000"
      - name: inference-ttft-p99            # <= only; TTFT p99 in ms
        value: "<= 2000"
      - name: inference-model               # optional; HF model ID
        value: Qwen/Qwen3-8B
      - name: inference-concurrency-per-gpu # optional; positive integer
        value: "256"
      - name: inference-routing-mode        # optional; dynamo-router or gateway-epp
        value: dynamo-router
```

`inference-model` and `inference-concurrency-per-gpu` resolve with precedence
**recipe constraint > `AICR_INFERENCE_PERF_*` catalog env > compiled default**
(Qwen3-8B at 256/GPU). Set them per overlay to pick the right model and load for
each accelerator — exactly as the throughput/TTFT thresholds already vary per
overlay — while the compiled defaults cover overlays that omit them. Because the
thresholds are only meaningful at a specific model + concurrency, pin all four
together in an overlay rather than relying on the global defaults for the inputs.
`inference-routing-mode` resolves from the recipe only, defaulting to
`dynamo-router`; set `gateway-epp` to validate the GAIE/EPP path through the
AICR-managed inference gateway.

### NCCL benchmark profile constraint

The NCCL checks (`nccl-all-reduce-bw`, `-net`, `-nvls`) decide applicability
from the recipe's `criteria` against a service + accelerator matrix compiled
into the validator, so a recipe for a service registered only through
external `--data` (or an embedded service on a new accelerator) skips them by
default. The optional `nccl-benchmark-profile` entry in
`validation.performance.constraints` (a bare `{accelerator}/{service}` value,
no comparator) opts such a recipe into one of the embedded benchmarks:

```yaml
validation:
  performance:
    checks: [nccl-all-reduce-bw-net, nccl-all-reduce-bw-nvls]
    constraints:
      - name: nccl-benchmark-profile     # optional; embedded profile to run as
        value: gb200/eks
      - name: nccl-all-reduce-bw-net     # thresholds stay same-named as the checks
        value: ">= 40"
      - name: nccl-all-reduce-bw-nvls
        value: ">= 500"
```

The profile resolves from the recipe only (no env tier) and fails closed on a
malformed or unknown value. Pick the pair whose fabric matches the target
hardware — the profile drives the benchmark's transport template, fabric
discovery, and preflights. See
[Opting external recipes into a benchmark profile](../user/validation.md#opting-external-recipes-into-a-benchmark-profile)
for the valid pairs and skip/fail semantics.

When no embedded pair matches — a genuinely private service+accelerator with a
fabric none of the shipped templates cover — supply the benchmark yourself:
ship a Kubeflow `TrainingRuntime` in your `--data` tree at
`validators/performance/testdata/{accelerator}/{service}/runtime.yaml` and
reference it with the `nccl-benchmark-runtime-ref` constraint (a bare
`{accelerator}/{service}` value). Run `aicr validate --data <dir> ...` so the
referenced file is resolvable; it is read and rendered in place of a baked-in
template, keyed on the recipe's own criteria with no compiled applicability
entry. The runtime owns its fabric wiring (the validator skips service-specific
fabric setup — discovery, preflights, and NVLS/IMEX provisioning — but still
asserts transport for the `-net`/`-nvls` variants), must
declare a `node` replicatedJob, and is mutually exclusive with
`nccl-benchmark-profile`. Laying the file at the embedded testdata path makes it
a drop-in for upstreaming. See
[Supplying a benchmark runtime for a private service](../user/validation.md#supplying-a-benchmark-runtime-for-a-private-service).

### Component Types

**Helm components** (most common):
```yaml
componentRefs:
  - name: gpu-operator
    type: Helm
    valuesFile: components/gpu-operator/values.yaml
    overrides:
      driver:
        version: "580.82.07"
```

The chart version comes from the registry default — see
[Chart Version Pinning](#chart-version-pinning).

#### Kustomize components

```yaml
componentRefs:
  - name: my-app
    type: Kustomize
    source: https://github.com/example/my-app
    tag: v1.0.0
    path: deploy/production
```

A component must have either `helm` OR `kustomize` configuration, not both.

> **`patches` is not supported.** The `componentRefs[].patches` field is not
> applied by any deployer. An enabled ref that sets `patches` is rejected at
> recipe resolution (rather than silently producing an unpatched bundle), so do
> not use it. See [#1588](https://github.com/NVIDIA/aicr/issues/1588).

## Component Configuration

### Chart Version Pinning

Do not set `version:` (Helm) or `tag:` (Kustomize) on a `componentRef` that
installs the component's registry default. Resolution falls back to the
registry entry's `helm.defaultVersion` / `kustomize.defaultTag` in
`recipes/registry.yaml`, which is the single source of truth for component
versions — bumping a component means bumping the registry default, in one
place.

Pin a version only when the overlay must intentionally diverge from the
registry default. For recipes contributed to this repo (the embedded
catalog), additionally declare that divergence in `versionPinExemptions`
(`pkg/recipe/version_pin_guard_test.go`) with a justification. CI rejects a
non-exempted embedded pin whenever the component has a matching registry
default: a pin that differs from it is undeclared drift, and a pin that
merely repeats it is redundant — it doubles bump churn and shields the
overlay from external registry overrides. Only Helm `version` divergences
can be exempted today: a Kustomize `tag` exemption is rejected because the
BOM variants pipeline cannot yet represent it (extend `tools/bom/variants.go`
first).

External `--data` overlays are not scanned by this guard: at resolution an
explicit pin always wins over the registry default, so external trees may
pin without declaring anything or rebuilding AICR — see
[data extension](data-extension.md#registryyaml-is-required).

This split keeps external data trees composable: an external `--data`
registry that overrides a component's registry default (`defaultVersion` /
`defaultTag`) takes effect for every overlay that does not pin, while an
explicit pin still wins. See
issue [#1616](https://github.com/NVIDIA/aicr/issues/1616).

### Configuration Patterns

**Pattern 1: ValuesFile only** (large, reusable configs)
```yaml
componentRefs:
  - name: cert-manager
    valuesFile: components/cert-manager/eks-values.yaml
```

**Pattern 2: Overrides only** (small, recipe-specific configs)
```yaml
componentRefs:
  - name: nvsentinel
    overrides:
      namespace: nvsentinel
      sentinel:
        enabled: true
```

**Pattern 3: Hybrid** (shared base + recipe tweaks)
```yaml
componentRefs:
  - name: gpu-operator
    valuesFile: components/gpu-operator/eks-gb200-training.yaml
    overrides:
      driver:
        version: "580.82.07"  # Override just this field
```

### Value Merge Precedence

Values merge from lowest to highest precedence:

```
Base → ValuesFile → Overrides → CLI --set flags
```

**Deep merge:** only specified fields replaced, unspecified preserved. Arrays replaced entirely (not element-by-element).

**Example:**
```yaml
# Base: driver.version="550.54.15", driver.repository="nvcr.io/nvidia"
# ValuesFile: driver.version="570.86.16"
# Override: driver.version="580.13.01"
# Result: driver.version="580.13.01", driver.repository="nvcr.io/nvidia" (preserved)
```

### Configuration Profiles

A service or OS overlay may declare one configuration profile when the same
criteria combination has multiple qualified ownership modes. The first
embedded adopter is the AKS family: `recipes/overlays/aks.yaml` declares
`gpuStack` (`azure-managed` default, `operator-managed` alternative) over the GPU
driver/toolkit ownership paths.

A declaring overlay uses recipe apiVersion `aicr.run/v1alpha3`:

```yaml
kind: RecipeMetadata
apiVersion: aicr.run/v1alpha3
metadata:
  name: example-service
spec:
  criteria:
    service: example
  profile:
    name: gpuStack
    description: Who installs the GPU driver.
    default: driver-installed
    values:
      # SHAPE ONLY — not a shippable declaration. Neither value carries
      # the distinguishing constraint ADR-015 requires (see below).
      driver-installed:
        componentRefs:
          - name: gpu-operator
            overrides:
              driver:
                enabled: false
      operator-managed:
        componentRefs:
          - name: gpu-operator
            overrides:
              driver:
                enabled: true
```

**Every value must carry a distinguishing constraint.** ADR-015 requires each
value to declare a validation signal that distinguishes its configuration from
every sibling value's. A value with no constraints at all — or with constraints
identical to a sibling's — does not support the "validated against deployed
config" claim and must not be declared. The snippet above shows the declaration
shape only; it is not a declaration you should copy into an overlay.

**Constraint names must be measurement paths a supported snapshot producer
actually emits** — a collector, or a provider projection attached at the
snapshot orchestration layer (e.g. `K8s.aks-gpu-pools.gpu-driver` from
`aicr snapshot --aks-gpu-pools`) — in
`{Type}.{Subtype}.{Key}` form. The type must be one the snapshot carries —
`K8s`, `GPU`, `OS`, `SystemD`, `NodeTopology`, or `NetworkTopology` —
so `K8s.server.version` resolves while an unknown type is rejected outright as
an invalid measurement type. Constraints are evaluated against the snapshot
when one is supplied, and a name whose subtype nothing produces fails closed
with `subtype not found`. Either way a fabricated path makes the whole value
unselectable rather than unconstrained.

Do not borrow paths from `validation.deployment.constraints`, such as
`Deployment.gpu-operator.version`. Those are deployment-phase validator keys
evaluated against a live cluster, not snapshot readings, and `Deployment` is
not a measurement type.

**One name is a node-set form, not a reading path:**
`NodeTopology.gpu-nodes.label`
([#1755](https://github.com/NVIDIA/aicr/issues/1755)). No snapshot producer
emits a `gpu-nodes` subtype; the evaluator synthesizes the GPU-node set from
the snapshot's `NodeTopology.label` readings (nodes carrying
`cloud.google.com/gke-accelerator`) and quantifies a label predicate over it.
Its value grammar is also not the operator grammar:
`<label-key>=<value>` asserts every GPU node carries the label with exactly
that value, and `!<label-key>` asserts no GPU node carries the key. Both
directions fail closed on a truncated node list (a snapshot captured with
`--max-nodes-per-entry` whose cap actually truncated a participating
reading), on an empty GPU-node universe, and on malformed or ambiguous
label readings (an encoding collision between a disambiguated entry and a
distinct dotted label name — see #2003). It is consumed by the GKE
`gpuStack` profile values (the positive form qualifies `driver-installer`, the
negated form `gke-default`), where each selected value's constraint is
verified at generation when generating from a snapshot (criteria-only
generation has no snapshot evaluator and defers entirely to the
pre-flight) and re-evaluated by the validate readiness pre-flight. Outside a profile declaration, declare it under
`validation.readiness.constraints`, not `spec.constraints` — as a top-level
constraint it would exclude the overlay during snapshot-based generation on
the very cluster the diagnostic exists to fix.

**Which signal qualifies a driver-ownership profile depends on the service.**
The example above names none, which is why it is shape only. `GPU.hardware`
readings do not settle it: `driver-loaded` proves a driver is *present*, not
which mode installed it.

**AKS** projects each GPU agent pool's durable `gpuProfile.driver` property
into a snapshot reading (`K8s.aks-gpu-pools.gpu-driver`). The reading
**qualifies** a selection, it does not make one: the selected value comes
from `--profile` (or the declaration's default), and its recorded constraint
is then checked against the reading — `azure-managed` requires `Install`,
`operator-managed` requires `None`. A `None`-pool snapshot resolved without
`--profile gpuStack=operator-managed` therefore fails closed on the azure-managed
default rather than silently switching values. Unavailable, unknown, or
mixed pool values fail closed against either selection. ADR-015 resolves
this signal. The AKS family above was the first embedded adopter; the GKE
family's `gpuStack` (device-plugin ownership over the #1755 node-set form,
with `advertiser: external` on `gke-default`) is the second.

No equivalent reading exists for other services yet. Declare a
driver-ownership profile only once the signal for that service exists, and
give both values symmetric constraints over it. Do not substitute a signal
that reports something adjacent: GKE's
`gke-no-default-nvidia-gpu-device-plugin` label, for instance, governs
device-plugin advertisement rather than driver provisioning
([#1755](https://github.com/NVIDIA/aicr/issues/1755) keeps the two
deliberately separate), so selecting driver modes with it can pick the wrong
configuration on a supported cluster.

Nothing in core admission blocks a constraint-free declaration — the
distinguishability rule is enforced during catalog review. A value without
constraints still resolves, still returns `metadata.selectedProfile`, and still
locks its `ownedPaths`, so shipping one produces a recipe that attests an
unqualified configuration.

Profile declarations are intentionally narrow:

- One declaration may influence a resolved composition. Put it on a shared
  overlay ancestor; mixins cannot declare profiles.
- `default` is required. Profile and value identifiers use
  `[A-Za-z0-9._-]+`, and value names must additionally be unique
  **case-insensitively** (rejected at catalog load): evidence and
  corroboration derive lowercase path segments from the selected value,
  so `Operator-Managed` and `operator-managed` would collapse onto one evidence
  directory.
- A value permits only `constraints` and
  `componentRefs{name,overrides}` in the core mechanism. `valuesFile`,
  component identity/deployment fields, literal dotted keys, nested empty
  maps, and root `overrides.enabled` are rejected.
- Override leaves must be JSON/YAML scalar values. Finite numbers and YAML
  timestamps are supported; non-finite `.nan`/`.inf` values are rejected
  because every resolved recipe must remain JSON-serializable. Integers must
  remain within the JSON round-trip-safe range, from -9007199254740991 through
  9007199254740991.
- Every value must assign the same flattened override paths. A profile may
  configure only components already enabled in the surviving composition.
- Profile values may assign overrides only to Helm components. Kustomize
  components do not consume values overrides, so they may be referenced only
  without overrides for presence locking.
- Profile constraints must distinguish sibling modes. They are evaluated
  fail closed during snapshot resolution and remain in the hydrated recipe
  for later validation. This qualification rule is enforced during catalog
  review; core admission does not infer whether arbitrary readings
  semantically distinguish two modes.
- A profile value may declare `advertiser: external` (the GKE `gke-default`
  shape) to record a provider-managed plugin outside the recipe as THE
  `nvidia.com/gpu` advertiser; the vocabulary is closed (empty or
  `external`), and the declaration extends the #1327 dual-advertisement
  gates and closure-locks the allocation-policy selector paths.

Select with `aicr recipe --profile name=value`; omission uses the declared
default. A profiled result uses `aicr.run/v1alpha3` and records
`metadata.selectedProfile`, including declaration-wide `ownedPaths`. The
lock on owned paths is enforced per surface:

- **`aicr bundle` static overrides** (any static source — `--set`,
  `--set-json`, `--set-file`, config-file): identical values are
  accepted, divergent values fail closed. Typed sources are always
  rejected for the `enabled` presence key, even when identical.
- **`aicr mirror list --set` overrides**: same identical-accepted /
  divergent-rejected rule; mirror exposes only the repeatable scalar
  `--set` (no typed flags) and does not apply a config file's
  `spec.bundle.deployment.set` overrides.
- **`--dynamic` exports**: fail closed on intersection with an owned
  path, regardless of value.
- **argocd-helm install-time values**: any owned-key presence fails
  closed at Helm render time, even when the value is identical.
- **Component presence** (the synthetic `enabled` owned path): removal of
  an owned component fails closed, and profile fragments cannot assign
  `enabled` — so reselecting a profile changes owned **value** paths
  only, never which components are present. Presence changes are
  catalog/composition changes.

**Snapshot-driven override — `gpu-operator.driver.enabled`.** When a recipe is
resolved from a snapshot (via `aicr recipe --snapshot` or
`ResolveRecipeFromSnapshot`), AICR reads the sampled GPU node's `driver-loaded`
measurement. When the NVIDIA kernel module is already loaded, AICR injects
`gpu-operator.overrides.driver.enabled=false` as an Overrides entry, so it wins
over base and provider values files.

Explicit `--set` flags at bundle generation, such as
`aicr bundle --set gpuoperator:driver.enabled=true`, retain higher precedence
and can supersede the injection unless the path is profile-owned. Divergent
overrides of profile-owned paths fail closed. `--set` is a bundle-time flag,
not an `aicr recipe` flag.

**Profile ownership.** When a selected profile owns
`gpu-operator.driver.enabled`, auto-detection does not mutate that path. The
selected profile remains authoritative.

**Static activation gate.** Otherwise, injection fires only when the resolved
component values already set `gpu-operator.driver.enabled=false` and carry the
coordinated preinstalled-driver configuration. This prerequisite is independent
of whether the recipe declares an ADR-015 profile. It scopes auto-detection to
statically prepared overlays such as AKS, GKE-COS, and OKE. Bare EKS is skipped
with a warning instead of leaving the Operator half-configured. The policy is
only-false and never forces `true`, so recipes resolved without a snapshot use
their static defaults.

**Snapshot timing.** Capture the snapshot **before** deploying the GPU
Operator. A snapshot taken after an earlier AICR-managed driver install still
reports `driver-loaded=true` and could flip a re-deployment toward driverless
nodes. AICR warns when both `driver-loaded=true` and a gpu-operator
ClusterPolicy are present in the snapshot. See
[Component Catalog › GPU Operator Driver Auto-Detect](../user/component-catalog.md#gpu-operator-driver-auto-detect).

### Typed Slurm accounting configuration

Slurm accounting ownership is resolved after catalog matching; it is not a
criteria dimension and overlays must not author
`configuration.slurm.accounting`. Every Slurm leaf declares stable refs for
`mariadb-operator-crds`, `mariadb-operator`, and
`slurm-accounting-mariadb`. The resolver derives their root `install` gates and
`slinky-slurm.accounting.enabled` from one typed mode:

| Mode | Slinky accounting | MariaDB components |
| --- | --- | --- |
| `disabled` | false | install false |
| `customer-managed` | true | install false |
| `aicr-provided` | true | install true |

The AICR install gate is consumed by `ComponentRef.IsEnabled` before deployer,
mirror, BOM, and health paths. It is not an upstream chart value. Mode-owned
paths are immutable at bundle time. See
[ADR-016](../design/016-slurm-accounting-enablement.md) and the
[Slurm Accounting guide](../user/slinky-slurm-accounting.md).

## Disable a Component in an Overlay

Set `overrides.enabled: false` on a `componentRef` to drop a component a base
recipe would otherwise install. Use this when the target platform already
provides that component — for example a CSP-managed cert-manager on OKE, where
installing a second copy would conflict.

```yaml
# Leaf overlay: the platform supplies cert-manager, so don't install ours.
componentRefs:
  - name: cert-manager
    overrides:
      enabled: false
```

A disabled component is excluded from the recipe's `deploymentOrder` and from
the generated bundle. A dependency edge pointing at it is treated as **already
satisfied** (the component is assumed provided externally), so components that
declare it in `dependencyRefs` — such as `gpu-operator` — still resolve and
order correctly instead of failing with a circular-dependency error. A
`dependencyRefs` entry that names a component which does not exist in the recipe
at all is still an error.

The disabled `componentRef` remains in the resolved recipe's `componentRefs`
(with `overrides.enabled: false`) for transparency, but it cannot be re-enabled at
bundle time — `--set <component>:enabled=true` on a recipe-disabled component
is rejected, because re-enabling a platform-provided component would install a
conflicting second copy. Disabling is therefore an authoring decision: to ship
the component, remove the `enabled: false` override from the recipe/overlay.
See [Enable or disable components](../user/bundling.md#enable-or-disable-components).

## File Naming Conventions

File names are for human readability—matching uses `spec.criteria`, not file names.

**Overlay naming:** `{accelerator}-{service}-{os}-{intent}-{platform}.yaml` (platform always last)

| File Type | Pattern | Example |
|-----------|---------|---------|
| Service | `{service}.yaml` | `eks.yaml` |
| Service + intent | `{service}-{intent}.yaml` | `eks-training.yaml` |
| Full criteria | `{accel}-{service}-{os}-{intent}.yaml` | `gb200-eks-ubuntu-training.yaml` |
| + platform | `{accel}-{service}-{os}-{intent}-{platform}.yaml` | `gb200-eks-ubuntu-training-kubeflow.yaml` |
| Mixin (OS) | `os-{os}.yaml` | `os-ubuntu.yaml` |
| Mixin (platform) | `platform-{platform}.yaml` | `platform-kubeflow.yaml` |
| Component values | `values-{service}-{intent}.yaml` | `values-eks-training.yaml` |

## Constraints and Validation

### Constraints

Constraints validate deployment requirements against cluster snapshots:

```yaml
constraints:
  - name: K8s.server.version
    value: ">= 1.32.4"
  - name: OS.release.ID
    value: ubuntu
  - name: OS.release.VERSION_ID
    value: "24.04"
```

#### Common measurement paths

| Path | Example |
|------|---------|
| `K8s.server.version` | `1.32.4` |
| `OS.release.ID` | `ubuntu`, `rhel` |
| `OS.release.VERSION_ID` | `24.04` |
| `GPU.hardware.model` | `h100`, `l40s` |

**Operators:** `>=`, `<=`, `>`, `<`, `==`, `!=`, or exact match (no operator)

**Add constraints when:** recipe needs specific K8s features, driver versions, OS capabilities, or hardware. Skip when universal or redundant with component self-checks.

### Validation Phases

Optional multi-phase validation beyond basic constraints:

```yaml
# expectedResources are declared on componentRefs, not under validation
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
  # Readiness phase has no checks — constraints are evaluated inline from snapshot.
  deployment:
    checks: [expected-resources]
  performance:
    infrastructure: nccl-doctor
    checks: [nccl-bandwidth-test]
```

**Phases:** `deployment`, `performance`, `conformance` (readiness constraints are evaluated implicitly)

### Testing

```bash
# Validate constraints
aicr validate --recipe recipe.yaml --snapshot snapshot.yaml

# Phase-specific
aicr validate --recipe recipe.yaml --snapshot snapshot.yaml --phase deployment

# Run validation tests
go test -v ./pkg/recipe/... -run TestConstraintPathsUseValidMeasurementTypes
```

## Working with Recipes

### Adding a New Recipe

**When:** new platform, hardware, workload type, or combined criteria

**Steps:**
1. Create overlay in `recipes/overlays/` with criteria and componentRefs
2. If the recipe shares OS constraints or platform components with other overlays, reference existing mixins via `spec.mixins` instead of duplicating (or create new mixins in `recipes/mixins/`)
3. Create component values files if using `valuesFile`
4. Run tests: `make test`
5. Test generation: `aicr recipe --service eks --accelerator gb200 --format yaml`

**Example:**
```yaml
# recipes/overlays/gb200-eks-ubuntu-training.yaml
apiVersion: aicr.run/v1alpha2
kind: RecipeMetadata
metadata:
  name: gb200-eks-ubuntu-training
spec:
  base: eks-training
  criteria:
    service: eks
    accelerator: gb200
    os: ubuntu
    intent: training
  componentRefs:
    - name: gpu-operator
      valuesFile: components/gpu-operator/eks-gb200-training.yaml
```

### Updating Recipes

**Updating versions:** bump the component's registry default in
`recipes/registry.yaml` — overlays inherit it, so no overlay edit is needed
(see [Chart Version Pinning](#chart-version-pinning)):
```yaml
# recipes/registry.yaml
- name: gpu-operator
  helm:
    defaultVersion: v26.3.3  # Changed from v26.3.2
```

**Adding components:**
```yaml
componentRefs:
  - name: new-component
    valuesFile: components/new-component/values.yaml
    dependencyRefs: [existing-component]  # Optional
```

**Test changes:** `aicr recipe --service eks --accelerator gb200 --format yaml`

### Adding a Component Readiness Gate

A component can declare a **readiness gate** so that, when a bundle is built with `aicr bundle --readiness-hooks`, the deploy blocks until a component-specific signal is actually healthy — not just until the chart's own resources report Ready. This matters for operators whose true readiness lives in a custom resource the deployer can't assess natively (e.g. `gpu-operator`'s `ClusterPolicy` reaching `status.state: ready`).

**Convention:** drop a [Chainsaw](https://kyverno.github.io/chainsaw/) `Test` at `recipes/components/<name>/readiness.yaml`. There is no registry field to set — the bundler discovers the file by path. Components without one are simply not gated.

```yaml
# recipes/components/gpu-operator/readiness.yaml
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: gpu-operator-readiness
spec:
  # The gate CLI owns the outer retry/poll loop, so a single assert only needs
  # a short window to confirm the current state.
  timeouts:
    assert: 30s
  steps:
    - name: clusterpolicy-ready
      try:
        - assert:
            resource:
              apiVersion: nvidia.com/v1
              kind: ClusterPolicy
              status:
                state: ready
```

When `--readiness-hooks` is set, the bundler wraps this test into a `NNN-<name>-readiness/` folder containing a `Job` that runs the `gate` CLI (`ghcr.io/nvidia/aicr-gate`). The deploy blocks on that Job — via `helm --wait` for the helm deployer (the gate Job is a `post-install,post-upgrade` hook, and `--wait` blocks on hook completion regardless of `--wait-for-jobs`), or via Argo CD's built-in `batch/Job` health on the next sync-wave for the `argocd`/`argocd-helm` deployers. Keep `spec.timeouts.assert` shorter than the gate's per-test timeout so a single poll can't outlast one gate iteration. This is now enforced rather than advisory: the effective budget is the **smaller** of the authored `spec.timeouts.assert` and the caller's per-component budget, so an authored value larger than the caller allows is capped rather than honored. A shorter authored value still shortens the budget as before. That caller budget is the gate's `--timeout` (`defaults.ReadinessGateExecTimeout`, 2m) for a readiness Job, and `defaults.ChainsawAssertTimeout` for `aicr validate --phase deployment` — not the `expected-resources` catalog timeout, which is the outer Job envelope rather than a per-component assertion budget. See [Readiness Gates](../user/cli-reference.md#readiness-gates) for the deploy-time behavior.

**Supported operations.** The gate evaluates the Test in-process against its own read-only ServiceAccount — it ships no Chainsaw binary and shells out to nothing. Only `assert` and `error` are honored; every other operation (`apply`, `create`, `delete`, `patch`, `update`, `script`, `command`, `wait`, `sleep`, `get`, `describe`, `events`, `podLogs`, `proxy`) is rejected before evaluation, as are `catch`, `finally`, and `cleanup` blocks. A readiness test that declares one fails the gate with an invalid-request error naming the offending step.

One action per operation: a `try` entry that sets **both** `assert` and `error` is rejected. Chainsaw evaluates a single action per operation and the executor reaches `assert` first, so the `error` half — the one that forbids a shape — would never run. Split them into separate `try` entries.

A Test declaring no `assert`/`error` operation at all is rejected rather than passing vacuously — a check that evaluates nothing must never report healthy. If the no-op is deliberate, because readiness for that component is enforced some other way, declare it with an annotation on the Test:

```yaml
metadata:
  name: my-component-readiness
  annotations:
    aicr/no-op-check: "true"
```

The rule applies per document, so in a multi-document (`---`) stream each Test that carries no operations needs its own annotation.

An **empty** readiness file — blank, whitespace-only, or nothing but comments and `---` separators — is rejected outright. There is no Test to carry the annotation, so the only honest verdict is a failure; a check whose content was lost to a truncated ConfigMap value or a bad template render must not report healthy.

A multi-document stream may hold **only** Test documents. Once any document in the file is a chainsaw Test, the whole stream is evaluated by the in-process executor, and nothing else reads it — so a raw Kubernetes manifest sitting alongside a Test would be silently ignored while the component still reported ready. Such a stream is rejected by naming the offending document's kind. Ordinary punctuation (a trailing `---`, a comment-only or `null` document) is not content and is skipped.

Resource blocks that omit `metadata.namespace` are scoped to the release namespace (the Job passes it via `--namespace`); cluster-scoped kinds, like the `ClusterPolicy` above, ignore it.

## Best Practices

**Do:**
- Use minimum criteria fields needed for matching
- Keep base recipe universal and conservative
- Use mixins for shared OS constraints or platform components instead of duplicating across leaf overlays
- Always explain why settings exist (1-2 sentences)
- Follow naming conventions (`{accel}-{service}-{os}-{intent}-{platform}`)
- Run `make test` before committing
- Test recipe generation after changes

**Don't:**
- Add environment-specific settings to base
- Over-specify criteria (too narrow = fewer matches)
- Create duplicate criteria combinations
- Duplicate OS or platform content across leaf overlays (use mixins instead)
- Skip validation tests
- Forget to update context when values change

## Testing and Validation

### Automated Tests

Tests in [`pkg/recipe/yaml_test.go`](https://github.com/NVIDIA/aicr/blob/main/pkg/recipe/yaml_test.go) validate:
- Schema conformance (YAML structure)
- Criteria enum values (service, accelerator, intent, OS, platform)
- File references (valuesFile, dependencyRefs)
- Constraint syntax (measurement paths, operators)
- No duplicate criteria
- Merge consistency
- No dependency cycles

### Running Tests

```bash
make test  # All tests
go test -v ./pkg/recipe/...  # Recipe tests only
go test -v ./pkg/recipe/... -run TestAllMetadataFilesConformToSchema  # Specific test
```

### Test Workflow

1. Create recipe file in `recipes/`
2. Run `make test` to validate
3. Test generation: `aicr recipe --service eks --accelerator gb200 --format yaml`
4. Inspect bundle: `aicr bundle -r recipe.yaml -o ./test-bundles`

Tests run automatically on PRs, main pushes, and release builds.

## Advanced Topics

### External Data Sources

Integrators can extend or override embedded recipe data using the `--data` flag without modifying the OSS codebase. This enables:
- Custom recipes for proprietary hardware
- Private component values with organization-specific settings
- Extended registries with internal Helm charts
- Rapid iteration without rebuilding binaries
- New criteria values (service / accelerator / OS / intent / platform) admitted at runtime via the catalog-driven [criteria registry](data-extension.md#adding-a-criteria-value) — no rebuild required

See [Data Extension](data-extension.md) for the full walkthrough (folder layout, registry rules, strict mode, debugging). The summary below is for quick reference.

#### Directory structure

```
./my-data/
├── registry.yaml              # Extends/overrides component registry
├── overlays/
│   └── custom-recipe.yaml     # New or override existing recipe
├── mixins/
│   └── os-custom.yaml         # Custom mixin fragments
└── components/
    └── my-operator/
        └── values.yaml        # Component values
```

**Usage:**
```bash
# Recipe generation
aicr recipe --service eks --accelerator gb200 --data ./my-data --output recipe.yaml

# Bundle generation
aicr bundle --recipe recipe.yaml --data ./my-data --deployer argocd --output ./bundle

# Debug loading
aicr --debug recipe --service eks --data ./my-data
```

**Precedence:** Embedded data (lowest) → External data (highest)

**Behavior:**
- Overlays: Same `metadata.name` replaces embedded
- Registry: Merged; same-named components replaced
- Values: External valuesFile references take precedence
- Criteria values: External overlays' `spec.criteria` values become valid CLI / API inputs at runtime via the criteria registry; `--criteria-strict` (or `AICR_CRITERIA_STRICT=1`) rejects external-only values for OSS CI gates

**Validation:**
```bash
aicr --debug recipe --service eks --data ./my-data --output /dev/stdout
aicr recipe --service eks --data ./my-data --format json | jq '.metadata.appliedOverlays'
```

### Regional registry overrides

A handful of components ship images from regional, account-scoped container registries rather than a single public URI. The clearest example today is the AWS EFA device plugin, whose canonical home is `<account>.dkr.ecr.<region>.amazonaws.com/eks/aws-efa-k8s-device-plugin` — a per-region private ECR that every EKS node is auto-authorized to pull from. AWS publishes these add-ons regionally for three reasons: pulls go over the AWS internal backbone (no NAT egress), no Docker Hub / public-registry rate limits, and the image stays available even when the public internet or another region is degraded.

AICR ships a sensible default for each such image (e.g., us-west-2 for `aws-efa`), but customers deploying in a different region need to override the registry's region segment. Two override paths cover the common cases:

**Bundle-time override (single region per bundle).** Use `--set` to bake a specific region into the bundle:

```bash
aicr bundle --recipe recipe.yaml \
  --set awsefa:image.repository=602401143452.dkr.ecr.us-east-1.amazonaws.com/eks/aws-efa-k8s-device-plugin \
  -o ./bundle
```

**Install-time override (one bundle, many regions).** Use `--dynamic` to declare the path as install-time-fillable, then provide the value via `helm install --set` (or your GitOps tool):

```bash
aicr bundle --recipe recipe.yaml \
  --dynamic awsefa:image.repository \
  --deployer helm \
  -o ./bundle

# Per-cluster install
helm install ... --set image.repository=602401143452.dkr.ecr.eu-west-1.amazonaws.com/eks/aws-efa-k8s-device-plugin
```

`--dynamic` is supported with `helm`, `argocd-helm`, and `flux` deployers; `argocd` does not support it (use `argocd-helm` instead). See [Dynamic Install-Time Values](../user/cli-reference.md#dynamic-install-time-values) for the broader pattern.

**Partition-aware variants.** Standard AWS uses account ID `602401143452`. GovCloud and China use different accounts and URI suffixes:

| Partition | Account ID | URI shape |
|-----------|------------|-----------|
| `aws` (standard) | `602401143452` | `<account>.dkr.ecr.<region>.amazonaws.com` |
| `aws-us-gov` (GovCloud) | `013241004608` | `<account>.dkr.ecr.<region>.amazonaws.com` |
| `aws-cn` (China) | `961992271922` | `<account>.dkr.ecr.<region>.amazonaws.com.cn` |

Substitute the appropriate account and suffix in the `--set` / install-time value.

## Troubleshooting

**Debug overlay matching:**
```bash
aicr recipe --service eks --accelerator gb200 --format json | jq '.metadata.appliedOverlays'
aicr recipe --service eks --accelerator gb200 --format json | jq '.componentRefs[].version'
```

**Common issues:**
| Issue | Solution |
|-------|----------|
| Test: "duplicate criteria" | Combine overlays or differentiate criteria |
| Test: "valuesFile not found" | Create file or fix path in recipe |
| Test: "unknown component" | Use registered bundler name |
| Recipe returns empty | Check criteria fields match query |
| Wrong values in bundle | Verify merge precedence (base → valuesFile → overrides) |

**Validation:**
```bash
make qualify  # Full qualification
make test     # All tests
aicr recipe --service eks --accelerator gb200 --format yaml  # Test generation
```

## Submitting Your Recipe

Recipes that target hardware AICR maintainers cannot independently
re-run require an **evidence bundle** so a reviewer can verify the
recipe without owning the hardware. The bundle is a signed,
OCI-distributed artifact that captures the resolved recipe, the
cluster snapshot, the validator phase results, a CycloneDX BOM, and
a manifest of per-file hashes. It is produced by adding two flags to
the same `aicr validate` invocation you already use to check the
recipe — no separate build step.

### When You Need Evidence

You need an evidence bundle when your PR adds or changes a recipe
whose `criteria` reach hardware or a service that AICR maintainers
cannot independently re-run — most non-H100 GPUs, non-EKS services,
and specialty fabrics fall into this bucket. The recipe-evidence CI
gate posts a sticky Markdown comment on every PR touching
`recipes/**` and flags (warning-only — it does not block merge) any
touched recipe that has no matching per-source pointer under
`recipes/evidence/<recipe>/<src>/`.

The proposed material-slice canonicalization aims to let non-material
edits (comments, formatting, `displayName`, `description`, key-order)
reuse an existing pointer without a fresh bundle. That semantic slice
is **not yet implemented** — today's verifier hashes the normalized
full recipe, so the collapse-to-same-digest behavior is target state,
not current. See
[ADR-007 § Material-slice canonicalization](https://github.com/NVIDIA/aicr/blob/main/docs/design/007-recipe-evidence.md#material-slice-canonicalization-proposed)
for the proposed slice definition.

### Producing the Bundle

Run `aicr validate` against the cluster that exercises your recipe
and add `--emit-attestation` (writes the bundle to disk) and
`--push` (signs and uploads the OCI artifact):

```bash
# 1. Capture snapshot and resolve the recipe you're contributing.
aicr snapshot --output snapshot.yaml
aicr recipe --service eks --accelerator gb200 --os ubuntu \
            --intent training --output recipe.yaml

# 2. Validate with attestation emission. Replace the OCI ref with a
#    registry you control (GHCR, GitLab Container Registry, Harbor,
#    AWS ECR, Google Artifact Registry, Azure Container Registry,
#    or JFrog Artifactory — any OCI 1.1 registry with Referrers API
#    support).
aicr validate \
  --recipe recipe.yaml \
  --snapshot snapshot.yaml \
  --emit-attestation ./out \
  --push ghcr.io/<owner>/aicr-evidence

# 3. Commit the SIGNED pointer. The bundle bytes live in OCI; the repo
#    only stores the locator. The blocking Evidence Pointer Contract gate
#    requires a signed pointer committed under the per-source tree
#    recipes/evidence/<recipe>/<src>/<digest>.yaml — NOT a flat
#    recipes/evidence/<recipe>.yaml. <src> is the signer slug
#    (SourceSlug = first 32 hex of sha256(issuer\nidentity)); <digest> is
#    the bundle digest with ':' rewritten to '-'. Don't construct it by
#    hand: step 2 already logged the exact destination as the `copyTo`
#    field of its "evidence pointer written" line, e.g.
#      copyTo=recipes/evidence/<recipe>/<src>/sha256-<digest>.yaml
DEST=recipes/evidence/<recipe>/<src>/sha256-<digest>.yaml   # from the copyTo log line
mkdir -p "$(dirname "$DEST")"
cp ./out/pointer.yaml "$DEST"
git add "$DEST"
```

> **The signer must be allowlisted.** The blocking *Evidence Pointer Contract*
> gate rejects a committed pointer whose signer is not listed in
> `recipes/evidence/allowlist.yaml` ("signer … is not in the allowlist; add a
> community/partner entry"). A maintainer adds your verified signer (keyed by
> its one-way `source` slug, or an anchored `identityPattern` for CI) as a
> `community`/`partner` entry — coordinate this in your PR; the pointer cannot
> merge until the entry exists.

`--push` signs the bundle (cosign keyless via Sigstore) and attaches it to the
OCI artifact as a Sigstore Bundle referrer. The tag is just a label — the
bundle is pinned by its `sha256:` digest — so omitting it lets aicr derive a
unique per-recipe tag (`<recipe-slug>-<short-fingerprint>`, e.g.
`gb200-eks-ubuntu-training-3f9a1c2b4d5e`).

For the full bundle layout, flag reference, tag derivation, OIDC token
precedence, and registry compatibility notes, see
[Emitting recipe evidence](../user/validation.md#emitting-recipe-evidence).
For the end-to-end producer-and-consumer walkthrough, see the
[Recipe Evidence Demo](https://github.com/NVIDIA/aicr/blob/main/demos/evidence.md).

### Self-Verifying Before You Open the PR

Run the verifier locally — it is the same code the warning-only
recipe-evidence verify gate runs against the committed pointer. (A
second, **blocking** *Evidence Pointer Contract* gate also checks that
the committed pointer is signed and correctly placed under
`recipes/evidence/<recipe>/<src>/<digest>.yaml`; signing locally and
committing the nested pointer, as above, satisfies it.) A clean local
run keeps the sticky comment green:

```bash
# Verify the emitted pointer before committing it...
aicr evidence verify ./out/pointer.yaml
# ...or the committed nested pointer after copying it into place:
aicr evidence verify recipes/evidence/<recipe>/<src>/<digest>.yaml
```

Exit 0 means the signature verified, the predicate parsed, every
manifest/file hash matched, and the per-phase CTRF report digests
matched the predicate. The fingerprint and BOM are only *surfaced*
in the report (signer identity, fingerprint dimensions, phase counts,
BOM info) — they are not cross-checked against the recipe criteria or
registry. A non-zero exit writes a structured
Markdown report describing the specific check that failed. See
[`aicr evidence verify`](../user/cli-reference.md#aicr-evidence-verify)
for the full check list and exit-code semantics.

### What to Include in the PR

The recipe-evidence CI gate posts a Markdown summary as a sticky
comment, so you do not need to inline the verifier output. The
[PR template](https://github.com/NVIDIA/aicr/blob/main/.github/PULL_REQUEST_TEMPLATE.md)
has no dedicated evidence section, so add the following three pieces of
context the verifier cannot infer to the PR description (the Summary or
Implementation Notes section is fine):

- **The OCI ref** of the pushed bundle, digest-pinned, so a
  maintainer can audit it directly:
  `ghcr.io/<owner>/aicr-evidence@sha256:<digest>`.
- **The cluster you attested from** — cloud, accelerator SKU, OS,
  Kubernetes version, node count. The fingerprint dimensions are in
  the predicate, but the human description is what the maintainer
  reads first.
- **Evidence disposition.** If `aicr evidence verify` reported a
  non-zero exit with a `1` in the JSON output's `exit` field
  (signature valid, recorded phase results show failures), include a
  short justification in the PR description. The maintainer either applies the `evidence/known-failure`
  label (not yet created — future state) and merges, or requests changes. See
  [Exit-1 Review Process](../contributor/maintaining.md#exit-1-review-process)
  for what counts as an acceptable reason — broadly: optional check
  not applicable to your hardware, performance ceiling limited by
  your test bed, or a validator under known active rework.

### If You Cannot Push to a Registry

You can still produce a bundle locally without `--push`. The
resulting `./out/summary-bundle/` directory is unsigned but
otherwise complete:

```bash
aicr validate --recipe recipe.yaml --snapshot snapshot.yaml \
  --emit-attestation ./out
aicr evidence verify ./out/summary-bundle
```

The verifier records the signature step as "skipped (unsigned)" and
the manifest-hash chain becomes self-consistency only — useful for
catching accidental corruption during development, but **not
acceptable for the CI gate**, which requires a signed bundle bound
to a committed pointer.

- For mechanical changes that touch `recipes/**` but carry no
  recipe semantics (file renames, comment-only changes, license
  header sweeps, self-bootstrapping evidence-pipeline changes), ask
  a maintainer to apply `evidence/exempt` (not yet created — future state) per the
  [bypass policy](../contributor/maintaining.md#evidenceexempt-bypass-policy).
  Self-applying that label is not appropriate.

"I don't have the hardware right now, please merge" is **not** a
valid exempt path — see the bypass policy's "Inappropriate uses."

### Reference

- [Emitting recipe evidence](../user/validation.md#emitting-recipe-evidence) — user-facing flag reference and bundle layout
- [Recipe Evidence Demo](https://github.com/NVIDIA/aicr/blob/main/demos/evidence.md) — full producer-and-consumer walkthrough
- [Maintaining Recipe Contributions](../contributor/maintaining.md) — maintainer-side review checklist
- [ADR-007](https://github.com/NVIDIA/aicr/blob/main/docs/design/007-recipe-evidence.md) — bundle format and verifier semantics

---

## See Also

- [Data Architecture](../contributor/recipe.md) - Recipe generation process, overlay system, query matching algorithm
- [Components](../contributor/component.md) - Creating new bundlers
- [Maintaining Recipe Contributions](../contributor/maintaining.md) - Maintainer runbook for evidence-backed recipe PRs
- [CLI Reference](../user/cli-reference.md) - CLI commands for recipe and bundle generation
- [API Reference](../user/api-reference.md) - Programmatic recipe access
