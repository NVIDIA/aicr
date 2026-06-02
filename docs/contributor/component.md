# Components

A **component** in AICR is a registry entry pointing to a Helm chart or
Kustomize source that recipes can pull. The catalog lives in
[`recipes/registry.yaml`](https://github.com/NVIDIA/aicr/blob/main/recipes/registry.yaml);
per-component default values live under
[`recipes/components/<name>/`](https://github.com/NVIDIA/aicr/tree/main/recipes/components).
Overlays bind a component to a cluster shape; bundlers turn that
binding into a deployer-specific artifact.

Most components need **no Go code**. The declarative path is one
registry entry plus a `values.yaml`. The legacy
[`pkg/component/generic.go::ComponentConfig`](https://github.com/NVIDIA/aicr/blob/main/pkg/component/generic.go)
is marked `Deprecated` — it is unused in production. The live schema
is [`pkg/recipe/components.go::ComponentConfig`](https://github.com/NVIDIA/aicr/blob/main/pkg/recipe/components.go).

For the recipe data model — overlays, mixins, criteria, merge order
— see [recipe.md](recipe.md). This page is the contributor view for
adding or changing components.

## Where Does My Change Go?

| I want to... | Edit | Guide |
|---|---|---|
| Make an existing chart or kustomization available to recipes | `recipes/registry.yaml` entry | this page |
| Set default values for the chart | `recipes/components/<name>/values.yaml` | this page |
| Pin a chart version for a specific cluster shape | Recipe overlay in `recipes/overlays/` | [recipe.md](recipe.md) |
| Add a bundle-time validation warning | `registry.yaml` `validations:` block | [validator.md](validator.md#component-validations-bundle-time) |
| Add a chainsaw health check | `registry.yaml` `healthCheck.assertFile` + `recipes/checks/<name>/health-check.yaml` | [validator.md](validator.md) |
| Adjust where node selectors land in chart values | `registry.yaml` `nodeScheduling` paths | this page |

## Helm vs Kustomize

A component declares **either** `helm:` **or** `kustomize:` — never
both. `ComponentRegistry.Validate` rejects the mixed shape at load.

| Use `helm:` when | Use `kustomize:` when |
|---|---|
| Upstream ships a published Helm chart | Upstream ships only a Git source with `kustomization.yaml` |
| You need `--set` value overrides | You can accept no `--set` support |
| You want `nodeScheduling` injection | You will configure scheduling out-of-band (Kustomize ignores Helm value paths) |

Kustomize limitations to know up front:

- `--set <key>:<path>=<value>` flows through Helm value rendering only; Kustomize components silently ignore overrides.
- `nodeScheduling.system` / `accelerated` paths target Helm values; they do not apply to Kustomize sources.
- The bundler runs `kustomize build` at bundle time and wraps the output as `templates/manifest.yaml` inside the standard local-format folder (see [index.md](index.md) for the classification rule).

## Adding a Helm Component

**1. Add the registry entry** to `recipes/registry.yaml`:

```yaml
- name: my-operator
  displayName: My Operator
  valueOverrideKeys:
    - myoperator
  helm:
    defaultRepository: https://charts.example.com
    defaultChart: example/my-operator
    defaultVersion: v1.0.0
    defaultNamespace: my-operator
  nodeScheduling:
    system:
      nodeSelectorPaths: [operator.nodeSelector]
      tolerationPaths:   [operator.tolerations]
```

**2. Create `recipes/components/my-operator/values.yaml`** with the
chart defaults you want every recipe to start from. Keep this file
minimal and widely applicable — cluster-specific tweaks belong in
`values-<context>.yaml` siblings referenced from an overlay.

```yaml
# fullnameOverride avoids the aicr-stack- prefix on resource names.
fullnameOverride: my-operator

operator:
  replicas: 1
```

**3. Optional blocks** on the registry entry:

- `validations:` — bundle-time misconfiguration warnings ([validator.md](validator.md#component-validations-bundle-time))
- `healthCheck.assertFile:` — chainsaw conformance assertions ([validator.md](validator.md))
- `storageClassPaths:` — where `--storage-class` is injected
- `podScheduling.workload.workloadSelectorPaths` — for workload-pod placement
- `gkeCriticalPriority`, `hasSelfRefCRDs` — narrow service-specific quirks (see godoc on `ComponentConfig` for when these apply)

**4. Run `make bom-docs`** and commit the regenerated
`docs/user/container-images.md` in the same PR. CLAUDE.md treats this
as a hard rule whenever you change `registry.yaml`, a component's
`values.yaml`, or any chart version pin. See [BOM regeneration](#bom-regeneration).

**5. Run `make qualify`** — covers tests, lint, and the recipe-resolution
suite that parses every registry entry.

## Adding a Kustomize Component

```yaml
- name: my-kustomize-app
  displayName: My Kustomize App
  valueOverrideKeys:
    - mykustomize
  kustomize:
    defaultSource: https://github.com/example/my-app
    defaultPath: deploy/production
    defaultTag: v1.0.0
```

No `recipes/components/<name>/values.yaml` is required — Kustomize
reads its inputs from the upstream source. Reminder: no `--set`
overrides, and `nodeScheduling` paths do not apply.

## Schema Reference

Authoritative definitions live in
[`pkg/recipe/components.go`](https://github.com/NVIDIA/aicr/blob/main/pkg/recipe/components.go).
One-liner per field:

| Field | Purpose |
|---|---|
| `name` | Component identifier; must match `componentRefs[].name` in overlays |
| `displayName` | Human-readable label used in CLI output and bundle templates |
| `valueOverrideKeys` | Alt prefixes for `--set <key>:path=value` matching |
| `helm.defaultRepository` | Helm repo URL injected when an overlay leaves it empty |
| `helm.defaultChart` | Chart name (e.g. `nvidia/gpu-operator`) |
| `helm.defaultVersion` | Default chart version |
| `helm.defaultNamespace` | Install namespace |
| `kustomize.defaultSource` | Git or OCI source URL |
| `kustomize.defaultPath` | Subpath within the source |
| `kustomize.defaultTag` | Git ref / OCI tag |
| `nodeScheduling.system` | Helm value paths that receive the **control-plane** node selector / tolerations / taints |
| `nodeScheduling.accelerated` | Helm value paths that receive the **GPU node** selector / tolerations / taints |
| `nodeScheduling.nodeCountPaths` | Where `--nodes` is written |
| `podScheduling.workload.workloadSelectorPaths` | Workload-pod placement |
| `storageClassPaths` | Where `--storage-class` is written |
| `validations` | Bundle-time component check list ([validator.md](validator.md#component-validations-bundle-time)) |
| `healthCheck.assertFile` | Chainsaw assert YAML path (relative to data dir) |
| `gkeCriticalPriority`, `hasSelfRefCRDs` | Narrow service-specific flags (see godoc) |

## `nodeScheduling.system` vs `accelerated`

This is the field most contributors get wrong on first PR.

- `system` — paths into chart values for workloads that must land on
  **management / control-plane nodes** (e.g., operators, controllers,
  webhooks). The bundler writes the `--system-node-selector` and
  `--system-node-toleration` values here.
- `accelerated` — paths into chart values for workloads that must
  land on **GPU nodes** (e.g., device-plugin DaemonSets,
  driver-validation, DCGM exporters). The bundler writes the
  `--accelerated-node-selector` and `--accelerated-node-toleration`
  values here.

Concrete example from `gpu-operator`:

```yaml
nodeScheduling:
  system:
    nodeSelectorPaths:
      - operator.nodeSelector
      - node-feature-discovery.master.nodeSelector
    tolerationPaths:
      - operator.tolerations
  accelerated:
    nodeSelectorPaths:
      - daemonsets.nodeSelector
      - node-feature-discovery.worker.nodeSelector
    tolerationPaths:
      - daemonsets.tolerations
```

Wrong column = workloads land on the wrong node class. A DaemonSet
placed under `system` will miss GPU nodes; an operator under
`accelerated` will refuse to schedule on a cluster with tainted GPU
nodes only.

## `valueOverrideKeys`

`--set <key>:<path>=<value>` matches via `GetByOverrideKey`:

1. The component `name` is checked first.
2. Each entry in `valueOverrideKeys` is then checked.

For `gpu-operator` with `valueOverrideKeys: [gpuoperator]`, both
`--set gpu-operator:driver.version=...` and
`--set gpuoperator:driver.version=...` resolve to the same component.
Pick a key that is easier to type (no hyphens) and document it in the
displayName-adjacent comments if non-obvious. Override keys are
globally unique — `ComponentRegistry.Validate` rejects duplicates.

## `deploymentOrder`

`RecipeResult.DeploymentOrder` is **derived**, not authored.
`TopologicalSort` in `pkg/recipe/metadata_store.go` orders components
by `componentRefs[].dependencyRefs` declared in the overlay. When no
dependencies are declared, the order falls back to the order in
which components are listed in the overlay's `componentRefs`. Express
ordering by declaring `dependencyRefs` on the dependent component, not
by writing a separate `deploymentOrder` block.

## Local Format and Bundle Classification

The bundler emits a uniform `NNN-<component>/` layout via
[`pkg/bundler/deployer/localformat`](https://github.com/NVIDIA/aicr/tree/main/pkg/bundler/deployer/localformat).
Classification (single source of truth in `localformat.classify`):

| Recipe shape | Folder kind |
|---|---|
| `helm.defaultRepository` set, no `manifestFiles` | `KindUpstreamHelm` |
| `helm.defaultRepository` set + `manifestFiles` | `KindUpstreamHelm` + `KindLocalHelm` (`-post` injected) |
| `helm.defaultRepository == ""` + `manifestFiles` | `KindLocalHelm` |
| `kustomize.*Tag` or `*Path` set | `KindLocalHelm` (`kustomize build` → `templates/manifest.yaml`) |

If both `helm` and `kustomize` fields are populated, `Validate`
rejects the registry entry — there is no precedence rule because the
shape is invalid. `manifestFiles` are added post-chart; `preManifestFiles`
ship at sync-wave N-1 (e.g., a Namespace with PSS labels the chart
pods depend on).

## Deployers

AICR ships five output adapters in
[`pkg/bundler/deployer/`](https://github.com/NVIDIA/aicr/tree/main/pkg/bundler/deployer):
`helm`, `helmfile`, `argocd`, `argocdhelm`, `flux`. Each calls
`localformat.Write()` and then layers its own orchestration files
(`deploy.sh`, `helmfile.yaml`, Argo `Application` CRs, Flux
`HelmRelease`s). **Components do not need to be deployer-aware** —
the bundler renders per-deployer from one component definition.

See [index.md](index.md#community-standard-deployment-targets) for
the deployer matrix.

## BOM Regeneration

`docs/user/container-images.md` is rendered fresh from each Helm
chart's actual templates by `make bom-docs`. Run it and commit the
regenerated file in the **same PR** whenever you:

- Add or remove a component
- Bump a chart version (in `registry.yaml`, an overlay, or a mixin)
- Change a `values.yaml` in a way that affects which images render
  (image-repo override, subchart enable/disable, etc.)

`make bom-check` verifies the committed BOM matches a fresh regen
but is **opt-in only** — not wired into `make qualify`, `make lint`,
or the merge gate. Do not rely on CI to catch a missed regen.

## Boundary: Components Are Metadata

A component entry describes *what* to deploy and where its values
land. Components do **not** carry apply, wait, uninstall, rollback,
or readiness-polling logic — those concerns belong to the deployer
that consumes the bundle. If you find yourself writing custom apply
code inside the bundler or under `pkg/component/`, you are on the
wrong side of the boundary — see
[index.md "What AICR Is Not"](index.md#what-aicr-is-not).

## See Also

- [recipe.md](recipe.md) — overlays, mixins, criteria, the recipe data model
- [validator.md](validator.md#component-validations-bundle-time) — bundle-time component validation checks
- [validator.md](validator.md) — chainsaw health checks and validator runner
- [index.md](index.md) — contributor index and architectural boundary
- [integrator/recipe-development.md](../integrator/recipe-development.md) — end-user recipe authoring
- [user/component-catalog.md](../user/component-catalog.md) — end-user component catalog
- [`pkg/recipe/components.go`](https://github.com/NVIDIA/aicr/blob/main/pkg/recipe/components.go) — `ComponentConfig` source of truth
- [`recipes/registry.yaml`](https://github.com/NVIDIA/aicr/blob/main/recipes/registry.yaml) — live component catalog
