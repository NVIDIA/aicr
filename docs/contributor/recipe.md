# Recipes, Overlays, and Mixins

The recipe data layer is the rule-based engine that turns a `Criteria`
query (`{service, accelerator, intent, os, platform, nodes}`) into a
resolved `RecipeResult` — the merged spec, component refs, deployment
order, and validation phases that `aicr bundle` consumes.

This page covers everything related to AICR recipes for contributors:
the three layers that contribute data (**registry**, **overlay**,
**mixin**), the on-disk schemas for each, the resolver's merge
algorithm, and the invariants the resolver enforces. End-user recipe
authoring lives in
[recipe-development.md](../integrator/recipe-development.md); this
page is for contributors changing recipe content or extending the
resolver in `pkg/recipe`.

> **Where does my change go?** Most changes hit exactly one of three
> files. Skim [Decision matrix](#decision-matrix) before editing —
> picking the wrong layer leaks defaults across recipes or duplicates
> content across overlays.

## Layered Model

```text
┌──────────────┐  ┌──────────────┐  ┌──────────────┐
│   Registry   │  │    Mixin     │  │   Overlay    │
│ recipes/     │  │ recipes/     │  │ recipes/     │
│ registry.yaml│  │ mixins/*.yaml│  │ overlays/    │
│              │  │              │  │              │
│ Component    │  │ Composable   │  │ Criteria-    │
│ catalog +    │  │ fragment     │  │ matched      │
│ defaults     │  │ (constraints │  │ recipe with  │
│ (chart, ns,  │  │ + componentRefs)│ │ spec.base   │
│ scheduling)  │  │              │  │ inheritance  │
└──────┬───────┘  └──────┬───────┘  └──────┬───────┘
       │                 │                 │
       └─────────────────┴─────────────────┘
                         │
                         ▼
              ┌────────────────────┐
              │ RecipeResult       │
              │ (merged, ordered,  │
              │  validated)        │
              └────────────────────┘
```

Resolution: the resolver loads the base spec (`overlays/base.yaml`) as
the merge seed, then merges each matching overlay's inheritance chain
on top (base → ... → leaf), then applies the leaf's mixins, then
finally injects registry defaults for any component field the chain
left unset. Per-component values files (`recipes/components/<name>/`)
are pulled in at bundle time, not at recipe resolution.

## Decision Matrix

| | **Registry entry** | **Overlay** | **Mixin** |
|---|---|---|---|
| **Purpose** | Make a chart/kustomization available to recipes; set component-wide defaults | Pin versions, set values, scope by criteria | Share constraints or componentRefs across overlays |
| **Authority** | Component-wide (one entry per component) | Criteria-matched (selected by query) | Opt-in (referenced via `spec.mixins`) |
| **File** | `recipes/registry.yaml` (one entry) | `recipes/overlays/<name>.yaml` (one file per shape) | `recipes/mixins/<name>.yaml` (one file per fragment) |
| **Lifecycle** | Add once; bump `defaultVersion` on chart upgrade | Add per cluster shape; cull when shape retires | Stable; new mixin only when ≥ 2 leaves duplicate the same block |
| **Kind** | `ComponentRegistry` | `RecipeMetadata` | `RecipeMixin` |
| **Carries criteria?** | No | Yes (`spec.criteria`) | No (rejected at load) |
| **Carries `base`?** | No | Yes (single-parent chain) | No |
| **Example** | "make `gpu-operator` available, default to chart v25.10.1" | "for `eks` + `gb200` + `training` + `ubuntu`, pin K8s ≥ 1.32.4" | "for any overlay opting in via `mixins: [os-ubuntu]`, require Ubuntu 24.04" |

Rule of thumb: a change targeting *all* recipes goes in registry; a
change targeting *one* cluster shape goes in an overlay; a change
shared by ≥ 2 overlays as an opt-in fragment goes in a mixin.

## Registry (`recipes/registry.yaml`)

The registry is the component catalog. Each entry declares a chart or
kustomization the resolver can reference and supplies defaults the
resolver injects into any `ComponentRef` that leaves the field unset.

Top-level schema (`ComponentRegistry`):

```yaml
apiVersion: aicr.nvidia.com/v1alpha1
kind: ComponentRegistry
components:
  - name: <component-id>
    ...
```

`ComponentConfig` fields (see `pkg/recipe/components.go`):

| Field | Type | Required | Purpose |
|---|---|---|---|
| `name` | string | yes | Component identifier (matches `componentRefs[].name` in overlays) |
| `displayName` | string | yes | Human label used in CLI output and bundle templates |
| `valueOverrideKeys` | []string | no | Alt keys for `--set <key>:path=value` (e.g., `gpuoperator`) |
| `helm` | `HelmConfig` | one of helm/kustomize | Chart defaults (see below) |
| `kustomize` | `KustomizeConfig` | one of helm/kustomize | Kustomization defaults (see below) |
| `nodeScheduling` | `NodeSchedulingConfig` | no | Helm value paths for injecting selectors/tolerations/taints (`system`, `accelerated`, plus `nodeCountPaths`) |
| `podScheduling` | `PodSchedulingConfig` | no | Helm value paths for workload pod scheduling injection |
| `storageClassPaths` | []string | no | Helm value paths where `--storage-class` is written |
| `validations` | []`ComponentValidationConfig` | no | Bundle-time validation checks (function, severity, conditions, message) |
| `healthCheck.assertFile` | string | no | Chainsaw assert YAML (relative to data dir) for conformance phase |
| `gkeCriticalPriority` | bool | no | Synthesize ResourceQuota on GKE so `system-*-critical` pods admit |
| `hasSelfRefCRDs` | bool | no | Tells helmfile to emit `disableValidation: true` (chart ships CRD + CR in same release) |

`HelmConfig`: `defaultRepository`, `defaultChart`, `defaultVersion`,
`defaultNamespace`. `KustomizeConfig`: `defaultSource`, `defaultPath`,
`defaultTag`. A component must have *either* `helm` or `kustomize`,
not both.

`pkg/component/generic.go` carries a `ComponentConfig` marked
`Deprecated:` — that is a separate, unused-in-production legacy type;
the live ComponentConfig is the one in `pkg/recipe/components.go`.

Defaults flow into a `ComponentRef` only when the field is empty —
see [applyRegistryDefaults](#merge-algorithm) below.

## Overlay (`recipes/overlays/`)

An overlay is a `RecipeMetadata` document with a `spec.criteria` block
that selects it for matching queries. Overlays live in
`recipes/overlays/` and inherit single-parent via `spec.base`.

```yaml
kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: gb200-eks-ubuntu-training
spec:
  base: gb200-eks-training       # inheritance chain root → leaf
  mixins:                        # composed AFTER inheritance merge
    - os-ubuntu
  criteria:
    service: eks                 # query → overlay selection
    accelerator: gb200
    os: ubuntu
    intent: training
    # platform: kubeflow         # optional 6th dimension
  constraints:                   # OS/K8s/GPU/SystemD constraints
    - name: K8s.server.version
      value: ">= 1.32.4"
  componentRefs: []              # overrides on inherited components
  validation:                    # per-phase validation config
    readiness:   { ... }
    deployment:  { ... }
    performance: { ... }
    conformance: { ... }
```

Criteria fields (see `pkg/recipe/criteria.go` `type Criteria`):

| Field | Type | Wildcard | Static OSS values |
|---|---|---|---|
| `service` | `CriteriaServiceType` | `any` or empty | `eks`, `gke`, `aks`, `oke`, `kind`, `lke`, `bcm` |
| `accelerator` | `CriteriaAcceleratorType` | `any` or empty | `h100`, `h200`, `gb200`, `b200`, `a100`, `l40`, `rtx-pro-6000` |
| `intent` | `CriteriaIntentType` | `any` or empty | `training`, `inference` |
| `os` | `CriteriaOSType` | `any` or empty | `ubuntu`, `rhel`, `cos`, `amazonlinux`, `talos` |
| `platform` | `CriteriaPlatformType` | `any` or empty | `dynamo`, `kubeflow`, `nim`, `runai`, `slurm` |
| `nodes` | int | `0` | any positive int |

`--data` overlays may contribute additional values via the criteria
registry — `Has(FieldX, ...)` is consulted when a value misses the
fast-path `switch` in `Parse<X>`. Adding a new value to a Go enum
(e.g., a new accelerator) is multi-file work; audit
`CriteriaAccelerator*` callers as listed in CLAUDE.md before merging.

**Specificity.** Each criteria carries a specificity score equal to
the count of non-`any`, non-empty fields. The current `Specificity()`
in `criteria.go` counts six fields: `service`, `accelerator`,
`intent`, `os`, `platform`, `nodes`. Overlays are sorted by
specificity ascending, so less-specific overlays merge first.

**Matching is asymmetric.** Recipe-side `any` is a wildcard (matches
anything in the query); query-side `any` is *not* a wildcard (matches
only recipe-side `any`). A generic query never resolves to a
hardware-specific recipe. See `MatchesCriteriaField` in
`criteria.go`.

**Inheritance.** `spec.base` walks a single-parent chain from leaf →
... → `base` (the root spec, held separately on the metadata store).
Cycles are detected at catalog load. Per-field merge: constraints
merge by name (later wins on same name; new appended); componentRefs
merge by name field-by-field; criteria are *not* inherited (each
recipe declares its own).

## Mixin Composition

Inheritance is single-parent, which means cross-cutting concerns (OS
constraints, platform components) would otherwise duplicate across
every leaf. **Mixins** are composable fragments referenced via
`spec.mixins`. They live in `recipes/mixins/` and use kind
`RecipeMixin`.

```yaml
# recipes/mixins/os-ubuntu.yaml
kind: RecipeMixin
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: os-ubuntu
spec:
  constraints:
    - name: OS.release.ID
      value: ubuntu
    - name: OS.release.VERSION_ID
      value: "24.04"
  componentRefs: []   # optional
```

Mixin files currently in the tree: `os-ubuntu`, `os-talos`,
`platform-inference`, `platform-kubeflow`.

**Mixin rules:**

- A mixin carries only `constraints` and `componentRefs`. Setting
  `criteria`, `base`, `mixins`, or `validation` is rejected at load.
- Resolution order: base chain merged first, then mixins applied to
  the merged result. A leaf adopts a mixin by listing its file
  basename in `spec.mixins`.
- Mixin componentRefs are restricted to additive merges via
  `mixinComponentRefSafeForMerge` (see
  `pkg/recipe/metadata_store.go`). A mixin componentRef may only set
  `name`, `namespace`, `manifestFiles`, `preManifestFiles`. Setting
  any of `chart`, `type`, `source`, `version`, `tag`, `path`,
  `valuesFile`, `overrides`, `patches`, `dependencyRefs`, `cleanup`,
  `expectedResources`, `healthCheckAsserts` is rejected at compose
  time — those fields silently override the chain's chosen chart, so
  the resolver names the offending field and refuses to merge (see
  ADR-005 "Silent constraint override" mitigation).
- When a snapshot evaluator is wired in, mixin constraints are
  evaluated against it after merging; failure invalidates the entire
  composed candidate. In plain query mode mixin constraints are
  merged but not evaluated.

## Criteria Wildcard Overlays

Some overlays apply across an entire criteria dimension without being
referenced via `spec.base` or `spec.mixins`. The resolver picks them
up automatically because `FindMatchingOverlays` returns *all* maximal
matches, not just the most specific one. Two wildcard patterns in
the tree today: `gb200-any.yaml` (matches `service: any`) and
`monitoring-hpa.yaml` (matches `intent: any`).

```yaml
# recipes/overlays/gb200-any.yaml
spec:
  base: base
  criteria:
    service: any        # wildcard — matches eks, oke, gke, ...
    accelerator: gb200
  validation:
    deployment:
      constraints:
        - name: Deployment.gpu-operator.version
          value: ">= v25.10.0"
```

For a query `{service: eks, accelerator: gb200, intent: training}`,
the resolver returns three independent maximal leaves —
`gb200-eks-training` (matched by explicit criteria), `gb200-any`
(matched by `service: any`), and `monitoring-hpa` (matched by
`intent: any`). Each leaf's inheritance chain is resolved separately
and merged onto the base spec in specificity order.

**Maximal-leaf filter.** `filterToMaximalLeaves` (in
`metadata_store.go`) drops any match that is a transitive
`spec.base` ancestor of another match — ancestors re-enter the
output via chain resolution, so keeping them as separate matches
would double-count their contributions. Independent leaves on
unrelated chains (wildcard + explicit) are kept; one is not an
ancestor of the other.

**When to use a wildcard overlay vs a mixin:**

| Use a criteria-wildcard overlay when... | Use a mixin when... |
|---|---|
| Content applies based on query criteria | Content applies based on explicit opt-in |
| Consumer set is determined by matching | Consumer set is an enumerated list of leaves |
| Adopt-by-default is desired for new matching overlays | Each consumer should reference it explicitly |
| You need a `validation` block (mixins can't carry one) | You only need `constraints` / `componentRefs` |

**Precedence.** Leaves merge in specificity-ascending order, so a
service-specific leaf overrides the wildcard on same-named
constraints. `spec.validation.<phase>` blocks merge per-field:
`checks` and `constraints` union (nil = inherit, `[]` = clear,
non-empty = union); `nodeSelection` and `infrastructure` are
wholesale-replace. Don't carry per-fabric values in a wildcard
(NCCL bandwidth thresholds differ per service); reserve wildcards
for content genuinely uniform across the wildcard dimension.

## Merge Algorithm

The resolver lives in `pkg/recipe/metadata_store.go`. The merge
proceeds in fixed precedence (low → high):

```text
registry defaults → mixin → base chain → overlay leaf → CLI/API --set
```

Implementation notes:

1. **Seed.** `initBaseMergedSpec()` clones `s.Base` (parsed from
   `overlays/base.yaml`) into the merge target. The base spec is held
   separately on the metadata store; it is *not* an overlay candidate
   in `FindMatchingOverlays`.
2. **Chain merge.** For each maximal leaf, the inheritance chain is
   walked root → leaf and `mergedSpec.Merge(&recipe.Spec)` is called
   for each. Same-named constraints/componentRefs override; new
   entries append.
3. **Mixin merge.** `mergeMixins(mergedSpec)` walks `spec.mixins` on
   the leaf, loads each from `recipes/mixins/`, and appends.
   `mixinComponentRefSafeForMerge` rejects mixin componentRefs that
   touch identity/sourcing fields.
4. **Registry defaults.** `applyRegistryDefaults(provider, refs)`
   fills in chart/version/namespace/source/tag/path defaults for any
   `ComponentRef` field still empty after the chain merge. Failure to
   load the registry is propagated, not swallowed — partial refs
   would fail downstream far from the root cause.
5. **Topological sort.** `TopologicalSort()` orders components by
   `dependencyRefs` for the final `DeploymentOrder`. Cycles produce
   `ErrCodeInvalidRequest`.

**Deep-copy semantics.** `deepMergeMap` (`metadata.go`) recurses into
nested `map[string]any`. Non-map values (scalars *and* `[]any`) are
deep-copied via `serializer.DeepCopyAny` so `dst` never aliases
`src`'s slice values. This matters: copying `[]any` by reference
during overlay merge would let a downstream mutation (e.g., bundler
appending a toleration) leak back into the cached source map and
corrupt subsequent queries. The
[CLAUDE.md](https://github.com/NVIDIA/aicr/blob/main/.claude/CLAUDE.md)
anti-patterns list calls this out — any new helper that touches
overlay-derived maps must follow the same rule.

## Determinism

Recipe output is reproducible: same inputs → same bytes. The data
layer enforces this via two rules.

**Use `serializer.MarshalYAMLDeterministic` for any output that feeds
a digest, signature, OCI manifest, or fingerprint.** `yaml.v3` walks
Go maps in randomized order, so two consecutive marshals of the same
`map[string]any` produce different byte sequences. Plain
`yaml.Marshal` is fine for human-readable scratch output but is a
correctness bug anywhere a downstream consumer hashes the bytes.

**Per-dimension ordered lists, not unordered maps.** `RecipeResult`
fields like `appliedOverlays`, `componentRefs`, `deploymentOrder`,
and the per-dimension fingerprint diff are ordered slices, not maps,
so iteration is deterministic.

## Recipe Store Immutability

The metadata store is read-only after init. `LoadMetadataStoreFor(dp)`
returns a `sync.Once`-cached `*MetadataStore` per `DataProvider`
identity, so concurrent recipe builds against the same provider share
the store without locks. Per-request mutations (chain resolution,
constraint evaluation, registry defaulting) happen on clones, never
on the cached spec.

**Deferred registration.** `pendingRegistryEntry` stages each
overlay's criteria for the per-provider criteria registry *before*
registration. The actual `Register(field, value, origin)` calls only
fire after every overlay parses cleanly, the base recipe is present,
and dependency validation passes. Partial catalog loads never leak
into the registry; a malformed overlay does not poison criteria
validation for the next process.

**Eviction.** `EvictCachedStore(provider)` and
`EvictCachedRegistry(provider)` drop a single provider's cache entry
without disturbing other providers. Use after rewriting a `--data`
overlay on disk.

## Observable RecipeResult Surfaces

`RecipeResult` (in `pkg/recipe/metadata.go`) is the resolver's
externally-visible product. Fields beyond `ComponentRefs` and
`DeploymentOrder` that contributors should be aware of:

| Field | Purpose |
|---|---|
| `Metadata.AppliedOverlays` | Ordered list of overlays merged into this result (base first, leaf last). |
| `Metadata.ExcludedOverlays` | Overlays that matched criteria but were dropped (e.g., a mixin constraint failed against the snapshot). Each carries a typed `Reason` (`constraint-failed`, `mixin-constraint-failed`). |
| `Metadata.ConstraintWarnings` | Per-constraint detail for excluded overlays (overlay, constraint name, expected vs actual, reason text). |
| `Validation` | Multi-phase config (`readiness`, `deployment`, `performance`, `conformance`) inherited from overlay metadata. |
| `owner` (unexported) | `*Builder` that produced this result. `AssertOwnedBy(b)` enforces — two builders bound to different `DataProvider`s must not cross-read each other's results. |
| `provider` (unexported) | `DataProvider` that produced this result; accessed via `(*RecipeResult).DataProvider()`. Lets `GetValuesForComponent` route file reads through the originating provider even after the package-global has rotated. |

`ComponentRef` extras beyond the chart-identity fields:

| Field | Purpose |
|---|---|
| `ManifestFiles` | Extra manifest files to bundle at sync-wave N+1 (after primary chart). Additive merge, dedup. |
| `PreManifestFiles` | Manifest files to bundle at sync-wave N-1 (before primary chart) — e.g., a Namespace with PSS labels the chart pods need. Additive merge, dedup; `..` segments rejected at load. |
| `ExpectedResources` | List of `{Kind, Name, Namespace}` the deployment phase validator asserts exist. Overlay wholesale-replaces. |
| `HealthCheckAsserts` | Raw Chainsaw assert YAML loaded from the registry's `healthCheck.assertFile`; overlay wins if set. |
| `Cleanup` | Bundler uninstalls this component after validation (used for ephemeral validators like `nccl-doctor`). |

## Adding a Recipe

1. **Decide registry vs overlay vs mixin** ([decision matrix](#decision-matrix)).
2. **Write the YAML** in the correct directory. For an overlay, set
   `spec.base` to the most specific shared ancestor and let the chain
   carry shared constraints; only declare what differs.
3. **Run `make bom-docs` and commit `docs/user/container-images.md`**
   if your change touches `registry.yaml`, a component's `values.yaml`,
   or a chart version pin (see [BOM regeneration](#bom-regeneration)).
4. **Unit tests.** `make test` runs the recipe-resolution suite —
   `pkg/recipe/yaml_test.go` (static catalog: parse, refs, enum
   values, inheritance depth, no cycles) and
   `pkg/recipe/metadata_test.go` (runtime merge, topological sort).
   Both gate `make qualify`. If your change adds a registry entry, a
   new overlay file, or a mixin, the static suite typically picks it
   up without new test code.
5. **Integration validation.** For a new chart pin, run `make qualify`
   and let the e2e pipeline render the bundle. KWOK simulated
   clusters (`make kwok-e2e RECIPE=<name>`) catch most resolution
   regressions without GPU hardware.

## BOM Regeneration

`docs/user/container-images.md` is auto-generated from the actual
rendered Helm templates of every chart referenced by the registry. It
is regenerated by `make bom-docs`.

**Run `make bom-docs` and commit the regenerated
`docs/user/container-images.md` in the same PR whenever you:**

- Add or remove a component in `recipes/registry.yaml`
- Bump a chart version (in `registry.yaml`, an overlay, or a mixin)
- Modify a component's `values.yaml` in a way that changes which
  images render (image repo override, subchart enable/disable, etc.)

The regen can also surface drift from *upstream* chart updates —
when a chart bumps an image inside its own templates without a
registry pin change on our side. That drift will appear in the BOM
diff whether you expected it or not.

**Freshness is not gated at merge time.** `make bom-check` verifies
the committed BOM matches a fresh regen, but it is **opt-in only** —
not wired into `make qualify`, `make lint`, or the PR gate. Do not
rely on local qualify or CI to catch a missed regen. Wiring
`bom-check` into the gate is a desirable follow-up.

## Common Pitfalls

- **Skipping `make bom-docs`** after a chart pin or values change.
  The diff doesn't surface in qualify; the BOM goes stale silently.
- **Mutating in place during merge.** Overlay-derived `map[string]any`
  and `[]any` must be deep-copied, not aliased. `deepMergeMap` does
  this for you; a bespoke helper that recurses into maps but copies
  `[]any` by reference will alias and corrupt the cached source map.
- **Plain `yaml.Marshal` on output that feeds a digest.** Use
  `serializer.MarshalYAMLDeterministic` for any byte sequence a
  downstream consumer hashes (evidence predicate body, OCI manifest,
  signature input, fingerprint).
- **Adding a new criteria value to the Go enum but missing call
  sites.** A new accelerator, OS, intent, or platform value is
  enumerated in many files — the criteria registry, OpenAPI spec,
  every docs page that lists current values, issue templates, the
  `Specificity()` helper. Start from the Go type in `criteria.go`
  and follow the audit list in CLAUDE.md.
- **Setting identity fields in a mixin componentRef.** A mixin may
  not set `chart`, `version`, `valuesFile`, etc. — the resolver
  rejects with the offending field name. Move chart-changing logic
  to an overlay.
- **Assuming the cluster fingerprint is trustworthy.** The
  fingerprint block persisted in `aicr snapshot` output is
  advisory; trust-bearing consumers recompute via
  `fingerprint.FromMeasurements(...)` before acting. See the
  collector docs and ADR-007 for details.

## See Also

- [recipe-development.md](../integrator/recipe-development.md) — end-user recipe authoring guide
- [component.md](component.md) — adding a component to the registry
- [validator.md](validator.md#component-validations-bundle-time) — adding bundle-time component validation checks
- [validator.md](validator.md) — adding a validator check or health check
- [ADR-005](../design/005-overlay-refactoring.md) — overlay refactoring rationale (mixin composition, maximal-leaf resolver, wildcard overlays)
- [ADR-007](../design/007-recipe-evidence.md) — fingerprint, evidence bundle, verification
- [pkg/recipe godoc](https://github.com/NVIDIA/aicr/tree/main/pkg/recipe) — implementation
- [api/aicr/v1/server.yaml](https://github.com/NVIDIA/aicr/blob/main/api/aicr/v1/server.yaml) — recipe API contract and criteria enums
