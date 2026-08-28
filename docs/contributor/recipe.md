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
| **Purpose** | Make a chart/kustomization available to recipes; set component-wide defaults (incl. the component version/tag) | Set values, scope by criteria | Share constraints or componentRefs across overlays |
| **Authority** | Component-wide (one entry per component) | Criteria-matched (selected by query) | Opt-in (referenced via `spec.mixins`) |
| **File** | `recipes/registry.yaml` (one entry) | `recipes/overlays/<name>.yaml` (one file per shape) | `recipes/mixins/<name>.yaml` (one file per fragment) |
| **Lifecycle** | Add once; bump the registry default (`defaultVersion` / `defaultTag`) on upgrade | Add per cluster shape; cull when shape retires | Stable; new mixin only when ≥ 2 leaves duplicate the same block |
| **Kind** | `ComponentRegistry` | `RecipeMetadata` | `RecipeMixin` |
| **Carries criteria?** | No | Yes (`spec.criteria`) | No (rejected at load) |
| **Carries `base`?** | No | Yes (single-parent chain) | No |
| **Example** | "make `gpu-operator` available, default to chart v25.10.1" | "for `eks` + `gb200` + `training` + `ubuntu`, pin K8s ≥ 1.34" | "for any overlay opting in via `mixins: [os-ubuntu]`, require Ubuntu 24.04" |

Rule of thumb: a change targeting *all* recipes goes in registry; a
change targeting *one* cluster shape goes in an overlay; a change
shared by ≥ 2 overlays as an opt-in fragment goes in a mixin.

## Registry (`recipes/registry.yaml`)

The registry is the component catalog. Each entry declares a chart or
kustomization the resolver can reference and supplies defaults the
resolver injects into any `ComponentRef` that leaves the field unset.

Top-level schema (`ComponentRegistry`):

```yaml
apiVersion: aicr.run/v1alpha2
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
| `sharedStorageClassPaths` | []string | no | Helm value paths where `--shared-storage-class` is written for shared filesystem PVCs |
| `validations` | []`ComponentValidationConfig` | no | Bundle-time validation checks (function, severity, conditions, message) |
| `healthCheck.assertFile` | string | **yes** | Chainsaw assert YAML (relative to data dir) consumed by `aicr validate --phase deployment` (runtime — #1220) and by `make check-health` locally. Content is restricted to the read-only `assert` / `error` operation allowlist. Enforced at PR time by `pkg/recipe.TestComponentRegistry_RequiresHealthCheck` (every component must declare a path) and `pkg/chainsaw.TestValidateTestReadOnly_RegistryContent` (every declared path must pass the allowlist) — see #1223. |
| `gkeCriticalPriority` | bool | no | Synthesize ResourceQuota on GKE so `system-*-critical` pods admit |
| `hasSelfRefCRDs` | bool | no | Tells helmfile to emit `disableValidation: true` (chart ships CRD + CR in same release) |
| `manifestsUseChartCRDs` | bool | no | Tells helmfile to emit `disableValidation: true` on the release carrying the attached manifests — the injected `-post` wrapper under both vendored and non-vendored layouts (manifests create CRs of CRDs the chart installs) |

`HelmConfig`: `defaultRepository`, `defaultChart`, `defaultVersion`,
`defaultNamespace`. `KustomizeConfig`: `defaultSource`, `defaultPath`,
`defaultTag`. A component must have *either* `helm` or `kustomize`,
not both.

`pkg/component/generic.go` carries a `ComponentConfig` marked
`Deprecated:` — that is a separate, unused-in-production legacy type;
the live ComponentConfig is the one in `pkg/recipe/components.go`.

Defaults flow into a `ComponentRef` only when the field is empty —
see [applyRegistryDefaults](#merge-algorithm) below. The registry
default (`helm.defaultVersion`, or `kustomize.defaultTag` for Kustomize
components) is the single source of truth for a component's version;
`base`, overlay, and mixin `componentRefs` alike carry no version pins
except exemption-declared divergences — see
[Version pinning is single-source](#version-pinning-is-single-source).

## Overlay (`recipes/overlays/`)

An overlay is a `RecipeMetadata` document with a `spec.criteria` block
that selects it for matching queries. Overlays live in
`recipes/overlays/` and inherit single-parent via `spec.base`.

```yaml
kind: RecipeMetadata
apiVersion: aicr.run/v1alpha2
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
      value: ">= 1.34"
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
| `service` | `CriteriaServiceType` | `any` or empty | `eks`, `gke`, `aks`, `oke`, `ocp`, `kind`, `lke`, `bcm`, `metal3` |
| `accelerator` | `CriteriaAcceleratorType` | `any` or empty | `h100`, `h200`, `gb200`, `gb300`, `b200`, `a100`, `l40`, `l40s`, `rtx-pro-6000` |
| `intent` | `CriteriaIntentType` | `any` or empty | `training`, `inference` |
| `os` | `CriteriaOSType` | `any` or empty | `ubuntu`, `rhel`, `cos`, `amazonlinux`, `ol`, `talos` |
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
specificity ascending, so less-specific overlays merge first. Note:
`nodes` is included in `Specificity()` to allow nodes-only CLI queries
to pass the guard, but it does **not** participate in `Matches()` — no
overlay in the **embedded catalog** gates on node count (see #1781).
External `--data` catalogs that set `criteria.nodes` on an overlay are
rejected at load time (`ErrCodeInvalidRequest`); operators must remove
or zero that field before upgrading.

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

**Leaf.** A leaf is the most specific overlay in a chain — the
terminal node carrying fully-qualified `criteria` (every relevant
dimension set, e.g. `service` + `accelerator` + `os` + `intent` +
`platform`) that an end-user query actually resolves to. A leaf
usually adds little of its own (often `componentRefs: []`); its job is
to bind one inheritance chain plus its `mixins` under a single
criteria fingerprint. "Base → ... → leaf" throughout this page refers
to walking from the root spec down to this node. Leaf is a role, not a
distinct `kind` — every overlay is a `RecipeMetadata`; "leaf" just
names the ones at the end of a chain.

### Configuration Profile Contract

`RecipeMetadata.Spec.Profile` declares one overlay-scoped enum for qualified
configuration ownership modes. A declaration requires recipe apiVersion
`aicr.run/v1alpha3` or its ADR-022 target `aicr.run/v1beta2`; either profile
track without a declaration, or a declaration on a default-track version, is
rejected. Profile-version metadata and recipe artifacts are strictly decoded
so an unknown field cannot silently disappear.

The core `ProfileValue` contract is closed to `advertiser`, `constraints`,
`readinessConstraints`, and `componentRefs{name,overrides}`. It rejects `valuesFile`, component
identity/deployment fields, root `overrides.enabled`, literal dotted keys,
and nested empty maps. The `advertiser` field accepts exactly one non-empty
value, `external` (validated against `pkg/allocpolicy`, the canonical
append-only #1327 descriptor). A profile that owns advertisement — a
declared `advertiser: external`, or explicit ownership of a non-synthetic
policy-selector path (`devicePlugin.enabled`, DRA `resources.gpus.enabled`
/ `gpuResourcesEnabledOverride`)
— triggers the recomputed policy closure: every enabled descriptor
component's selector paths plus its synthetic `enabled` join the
**effective lock set** (`RecipeResult.EffectiveLockSet()`), which the
bundle/mirror lock, the argocd-helm guard, and the hydrating gate all
consume. The closure is recomputed at every artifact boundary and never
persisted in `ownedPaths`. The hydrating gate additionally runs the shared
tuple-coherence rules (external + `devicePlugin.enabled=true` and external
+ DRA `gpus.enabled=true` both reject). Profiles that do not own
advertisement (the AKS shape) trigger no closure and leave
allocation-policy keys on the bundle-time WARN semantics.
Value-bearing ownership is limited to Helm components;
Kustomize components do not consume values overrides and support only a
valueless reference for presence locking.

Resolution enforces these invariants:

1. Before snapshot filtering, collect declarations from every matching
   candidate chain. Deduplicate one declaring source reached through multiple
   chains; reject independently authored declarations.
2. Snapshot filtering may not remove the declaring source and fall back to an
   unprofiled composition.
3. Apply an explicit `name=value` selection or the required default after
   overlays and mixins. Every referenced component across all values must
   already be enabled in the surviving composition.
4. Every value has the same flattened override-path set. Apply the selected
   fragment at the highest recipe precedence and reject constraint-name
   collisions.
5. Evaluate selected profile constraints fail closed. A missing reading has a
   distinct invalid-request diagnostic; other evaluator failures propagate.
   A value's `readinessConstraints` are exempt from this step by design:
   they name post-deployment properties (ADR-015 DD5) and route into
   `spec.validation.readiness.constraints`, where the `aicr validate`
   readiness pre-flight evaluates them fail closed. Names deduplicate
   per phase — the same measurement path may carry a generation-time
   pre-condition and a readiness-time post-deployment state.
6. Stamp the result `aicr.run/v1alpha3` and persist
   `metadata.selectedProfile`. Its sorted `ownedPaths` is the
   declaration-wide path union plus synthetic `enabled` for each referenced
   component.

The shared raw-artifact gate checks version/profile coupling and hydrates
profile-owned values. Bundle and mirror compare final candidate state with the
hydrated recipe before creating output. Exact, ancestor, or descendant dynamic
paths reject unconditionally; static writes reject only when the effective
three-state observation (present bytes, absent, or blocked) diverges.
That bundle-time rejection is the whole enforcement for `helm`, `flux`, and
`helmfile`, whose install-time surface is closed — only the paths `--dynamic`
declares. The `argocd-helm` deployer additionally emits a structural
template-time guard, because it exposes component values through the parent
chart's `.Values`, an open-ended surface the bundle-time gate cannot
enumerate. Plain `argocd` rejects `--dynamic` and has no install-time value
surface.

Unprofiled compositions retain the legacy apiVersion and byte shape.
Generation-side driver auto-detection skips a path owned by the selected
profile. Evidence projection accepts profiled artifacts: the selected
value joins the evidence path name and the corroboration tab as a
lowercase `-<name>-<value>` segment (shared helper
`attestation.ProfileSegment`), pointers record the selection, and the
repo evidence gate recomputes each pointer's digest with its recorded
selection. The TestGrid coordinate is deliberately unsuffixed — its
digest-bound build ID already partitions per value.

## Mixin Composition

Inheritance is single-parent, which means cross-cutting concerns (OS
constraints, platform components) would otherwise duplicate across
every leaf. **Mixins** are composable fragments referenced via
`spec.mixins`. They live in `recipes/mixins/` and use kind
`RecipeMixin`.

```yaml
# recipes/mixins/os-ubuntu.yaml
kind: RecipeMixin
apiVersion: aicr.run/v1alpha2
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

Host-managed driver floors (GKE COS / A4X Max and similar platforms where
`driver.enabled: false`) use a separate deployment constraint,
`Deployment.gpu-driver.version` (e.g. `">= 580.95.05"`).
`check-nvidia-smi` evaluates it against the nvidia-smi banner on each
verified node; when the constraint is absent the check keeps its
banner-presence behavior and does not invent a floor (#1995).
When the constraint is set but the host driver cannot be measured —
unreadable nvidia-smi banner, no GPU nodes, all GPU nodes cordoned, or
GPU nodes busy with workloads — the check fails closed rather than
Skip. Skip on those paths is preserved only when no floor is
configured. The value must carry a comparison operator (`>=`, `>`,
`<=`, `<`) to behave as a floor; a bare version is exact string match,
so a newer driver would fail. The constraint name is an exact match;
a typo silently disables the floor (shared with
`Deployment.gpu-operator.version`).

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
proceeds in this temporal order:

```text
base chain (root → leaf) → mixins → selected profile → registry defaults → CLI/API --set
```

The base inheritance chain is merged first (root → leaf, later
ancestors override earlier ones on same-named entries). Mixins are
applied **after** the chain, not before it: `mergeMixins` appends each
referenced mixin's constraints and componentRefs. A mixin constraint
whose name already exists in the chain (or in another mixin) is
**rejected** — constraints have no merge semantic, so a name collision
is treated as an unambiguous conflict, not resolved by last-wins
precedence. Registry defaults then fill any still-empty componentRef
fields, and CLI/API `--set` overrides win last.

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
4. **Profile specialization.** When the pre-filter composition found one
   declaration and that source survived filtering, apply the selected value
   after the chain and mixins. The fragment supersedes earlier assignments
   only on its closed override surface. Unprofiled resolution skips this step.
5. **Registry defaults.** `applyRegistryDefaults(provider, refs)`
   fills in chart/version/namespace/source/tag/path defaults for any
   `ComponentRef` field still empty after the chain merge. Failure to
   load the registry is propagated, not swallowed — partial refs
   would fail downstream far from the root cause.
6. **Topological sort.** `TopologicalSort()` orders components by
   `dependencyRefs` for the final `DeploymentOrder`. Cycles produce
   `ErrCodeInvalidRequest`. Components disabled via
   `overrides.enabled: false` (`ComponentRef.IsEnabled()`) are excluded
   from the ordering, and an edge pointing at a declared-but-disabled
   component is treated as satisfied (assumed provided externally) so it
   does not trigger a false cycle; an edge to an *undeclared* component
   still surfaces as `ErrCodeInvalidRequest`. `TopologicalLevels()` /
   `ComponentRefsTopologicalLevels()` apply the same filter.

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

## Criteria Coverage Post-Condition

Resolution enforces a post-condition (issue #1542): every criteria dimension
the caller explicitly states — `service`, `accelerator`, `intent`, `os`,
`platform` — must be honored by at least one overlay in the final applied
set (`appliedOverlays`), or resolution fails with `ErrCodeInvalidRequest`
instead of silently returning a recipe that disregards a stated value.
`verifyCriteriaCoverage` (`pkg/recipe/coverage.go`) runs last in both build
paths, against the *final* applied set:

- In `BuildRecipeResult`, after `mergeOverlayChains` and `mergeMixins` —
  ancestor-supplied coverage counts, but mixins carry no `criteria` and
  cannot contribute coverage.
- In `BuildRecipeResultWithEvaluator`, after per-overlay constraint
  exclusion *and* the mixin-constraint-failure rebuild — a dimension whose
  only coverage came from a constraint-excluded overlay is still uncovered.
  The error's context carries `excludedOverlays` / `constraintWarnings` when
  constraint exclusion occurred during resolution, so the caller has that
  context even though the exclusion is not always what caused this specific
  dimension to go uncovered.

The error's `uncovered` context entries (`dimension`, `requestedValue`,
`validCompletions`) are computed from the maximal set of overlays that carry
the requested value without conflicting with any other stated dimension —
see `completionTuplesFor` / `minimalTuples`. `nodes` is deliberately excluded
from `coverageDimensions`: no overlay in the embedded catalog gates on
node count, so covering it would reject every `--nodes` query. It does
not participate in `Criteria.Matches()` (removed in #1781), but is
retained in `Criteria.Specificity()` so that nodes-only CLI queries pass
the minimum-specificity guard. It carries no coverage guarantee — it is
advisory metadata. External `--data` catalogs that set `criteria.nodes`
on any overlay are rejected at load time (`ErrCodeInvalidRequest`) to
prevent silent match-all behaviour; operators must remove or zero that
field before upgrading.

**Joint sufficiency.** Per-dimension coverage is necessary but not
sufficient. It is satisfied when `service` and `accelerator` are each honored
by *some* overlay independently, even when no single overlay carries the
combination and the combination's content lives only on an OS-gated leaf. The
caller then receives a recipe that silently omits that OS-gated content.
`verifyCriteriaCoverage` therefore also enforces a second condition
(issue #1782): resolution fails when **no applied overlay jointly carries
every stated dimension** *and* stating a strict dimension would reach an
overlay currently being skipped.

Both halves matter. The first is the escape hatch that keeps the generic tier
valid: `--service eks` resolves through `eks.yaml`, which carries the whole
stated combination, and is never asked for an OS. The second is what detects
the loss.

`os` is the only **strict** dimension, and `coverage.go` records why. Every
other dimension degrades to a smaller but coherent recipe when omitted: no
`--platform` yields no Slurm or Kubeflow layer, no `--intent` yields untuned
GPU Operator values. `os` decides whether the driver can be installed at all.
On Ubuntu the GPU Operator installs it, so an OS-agnostic recipe is a real
answer and `eks.yaml` carries no `os`; on COS the operator installs no driver
and the device-plugin owner differs, which is why every `gke` overlay is
OS-gated and no OS-agnostic GKE recipe exists to return. That is a property of
installing NVIDIA drivers on Linux rather than of this catalog's shape, so it
holds for external `--data` catalogs too.

This condition replaced the `requireOSIfNeeded` guard, which ran before the
merge and hardcoded three separate scopes: it only fired when `service` was
stated, only compared `service`+`accelerator` regardless of what the caller
asked for, and only ever demanded `os`. Only the last survives.
`coverage_subsumption_test.go` keeps the retired guard as a test-only oracle
and asserts over generated catalogs that every query it would have rejected is
still rejected.

A joint-sufficiency failure carries `details.strictDimensions`, **not**
`details.uncovered`. The distinction is load-bearing: `pkg/client/v1`
relaxation clears uncovered dimensions and retries, which here would discard
the check and return the partial recipe that #1542 fixed.

**Evaluator error classification is fail-closed.** During constraint
evaluation on the snapshot-driven path, `ErrCodeNotFound` (the evaluator's
designed signal for "measurement absent from snapshot") is the *only*
error code that degrades gracefully into overlay exclusion. Every other
error code returned by the evaluator propagates as a hard resolution
failure (see `isNotFoundEvalError` in `pkg/recipe/coverage.go`, consumed at
both call sites in `metadata_store.go`) — a malformed constraint expression
or an evaluator bug must not be swallowed as a quiet exclusion.
`ConstraintEvaluatorFunc` returns a plain `error`, so both fail-closed
branches (`evaluateOverlayConstraints`, `evaluateMixinConstraints`) pass a
non-`NotFound` error through `aicrerrors.PropagateOrWrap(..., ErrCodeInternal,
...)` before returning it — an evaluator that hasn't adopted `pkg/errors`
still surfaces a coded error instead of an uncoded 500 at the server layer.

**The engine stays strict; the SDK's snapshot path can relax derived-only
failures.** Everything above describes `pkg/recipe`'s behavior, which never
relaxes the post-condition — a coverage failure there is always terminal.
The relax-and-retry lives one layer up, in `pkg/client/v1`
(`relax.go`), behind an opt-in resolve option:

```go
result, err := client.ResolveRecipeFromSnapshotWithOptions(ctx, criteria, snap,
    aicr.WithSnapshotCriteriaRelaxation(aicr.DimensionIntent))
```

`service`, `accelerator`, and `os` can be derived from the snapshot
fingerprint rather than stated by the user (`intent` and `platform` are
always user-stated — the fingerprint never derives them). The option's
arguments name the dimensions the *caller* stated; everything else is
treated as derived. If a coverage error's uncovered dimensions are *all*
safely relaxable, the facade clears them to unstated and retries resolution
once, logging a warning per relaxed dimension and reporting them in
`pkg/client/v1.RecipeResult.RelaxedDimensions` — the facade's result type,
not the resolver's `RecipeResult` documented under
[Observable RecipeResult Surfaces](#observable-reciperesult-surfaces) below.

**Not every uncovered dimension is relaxable**, and the distinction is why
`verifyCriteriaCoverage` records `constraintExcluded` per entry. A dimension
no overlay states at all is safe to clear — nothing in the recipe
distinguishes the detected value. A dimension whose only provider was
removed by constraint evaluation is not: clearing it converts a real
incompatibility (the cluster failed that overlay's constraints) into a
broader recipe that resolves at exit 0. The facade refuses in that case, and
refuses again if clearing would leave no stated *coverage* dimension, which
would match every overlay and resolve the generic fallback — the same
fail-open as issue #1888. That check counts only the five coverage
dimensions, not `Specificity()`, because `nodes` scores a specificity point
while participating in no overlay match (#1781). A stated dimension is never
relaxed either way.

This lets an overlay tree that is deliberately agnostic to a dimension
(e.g. Kind's OS-agnostic overlays) tolerate a snapshot that still reports a
concrete value for it, without weakening the post-condition for anyone who
asked for that dimension explicitly.

Omitting the option keeps the strict behavior, so the coverage
post-condition is unchanged for every caller that does not opt in — the
REST recipe endpoint among them. `pkg/cli/query.go` passes the option and
supplies the stated set from its `touched` map; declaring which dimensions
a user typed is the one part of the policy that has to stay in the CLI,
since only that layer knows a flag was set (issue #2027).

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
| `Configuration` | Closed desired-state configuration that affects rendering without participating in overlay matching. Slurm accounting records exactly one ownership mode and derives its protected component gates. |
| `Validation` | Multi-phase config (`readiness`, `deployment`, `performance`, `conformance`) inherited from overlay metadata. |
| `owner` (unexported) | `*Builder` that produced this result. `AssertOwnedBy(b)` enforces — two builders bound to different `DataProvider`s must not cross-read each other's results. |
| `provider` (unexported) | `DataProvider` that produced this result; accessed via `(*RecipeResult).DataProvider()`. Lets `GetValuesForComponent` route file reads through the originating provider, preserving per-Client isolation. |

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
3. **Ship the chainsaw health check** (registry entries only). Every
   new component in `recipes/registry.yaml` MUST declare
   `healthCheck.assertFile` pointing at
   `recipes/checks/<name>/health-check.yaml`, and that file MUST use
   only the read-only `assert` / `error` operation allowlist (no
   `script`, `apply`, `wait`, `command`, etc. — see
   `pkg/chainsaw/allowlist.go`). The contract is enforced at
   PR time by `pkg/recipe.TestComponentRegistry_RequiresHealthCheck`
   and `pkg/chainsaw.TestValidateTestReadOnly_RegistryContent`
   — both gate `make qualify`. See #1223 and the
   [chainsaw health check section in validator.md](validator.md#chainsaw-health-checks)
   for the assertion patterns currently in use (DaemonSet
   `numberReady == desiredNumberScheduled`, Deployment
   `Available=True`, CRD `Established=True`).
4. **Run `make bom-docs` and commit `docs/user/container-images.md`**
   if your change touches `registry.yaml`, a component's `values.yaml`,
   or a chart version pin (see [BOM regeneration](#bom-regeneration)).
5. **Unit tests.** `make test` runs the recipe-resolution suite —
   `pkg/recipe/yaml_test.go` (static catalog: parse, refs, enum
   values, inheritance depth, no cycles) and
   `pkg/recipe/metadata_test.go` (runtime merge, topological sort).
   Both gate `make qualify`. If your change adds a registry entry, a
   new overlay file, or a mixin, the static suite typically picks it
   up without new test code.
6. **Integration validation.** For a new chart pin, run `make qualify`
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
- Bump a chart version — normally the registry default; overlay/mixin
  pins exist only as declared exemptions, and bumping one changes the
  BOM's variants table
- Modify a component's `values.yaml` in a way that changes which
  images render (image repo override, subchart enable/disable, etc.)

The regen can also surface drift from *upstream* chart updates —
when a chart bumps an image inside its own templates without a
registry pin change on our side. That drift will appear in the BOM
diff whether you expected it or not.

### Version freshness is gated; image drift is not

`TestCommittedBOMVersionsMatchRegistry` (`tools/bom/freshness_test.go`,
run by `make test` → `make qualify`) checks the committed doc's version
column against the registry pins with no Helm rendering, so a chart-pin
bump that forgets `make bom-docs` fails CI. The check treats the doc as
an *exact* registry projection: it is bidirectional (a component added
to the registry without a doc row, or a doc row left behind after a
component is removed, both fail), rejects duplicate rows, and compares
**every** row — pinned components by their effective type (Helm
`defaultVersion` or Kustomize `defaultTag`) and unpinned components
against the `—` sentinel, so a fabricated version on an otherwise
unpinned row cannot slip through. Because a docs-only PR that edits the committed
BOM skips the full `tests` job, the `bom-freshness` job in
`.github/workflows/merge-gate.yaml` runs this same test whenever
`docs/user/container-images.md` or `recipes/registry.yaml` change, so
the gate holds for docs-only edits too.

`TestCommittedBOMVariantsMatchRecipePins` (same file, same jobs) gates
the doc's **Version variants** table the same way: it derives every
explicit base/overlay/mixin Helm pin that differs from its registry
default and compares bidirectionally — a new divergent pin without a
variant row fails, and a stale row without a backing pin fails. Variant
discovery reads only the recipe data (the pins are the source facts);
it has no dependency on the version-pin guard's exemption policy, which
decides whether a divergence is *allowed*, not what deploys.

Neither gate catches *upstream image drift* — a chart bumping an image
inside its own templates without a pin change on our side. Full `make bom-check` verifies that too by
re-rendering, but it is **opt-in only** — not wired into `make qualify`,
`make lint`, or the PR gate. So you still must run `make bom-docs` after
a values change.

### Version pinning is single-source

The BOM renders each chart at its registry `defaultVersion`, but at
resolution the registry default is only a *fallback*: a `componentRef`
that sets `version` (Helm) or `tag` (Kustomize) in `base.yaml`, an
overlay, or a mixin overrides it. Declared divergent pins are
represented truthfully by the BOM's Version-variants table (#1611), so
a default row and a declared variant can coexist. The residual fiction
is the **sole-consumer** case: a divergence that leaves the registry
default with zero consumers makes the default row advertise a version
no recipe installs (issue #1424; #1418's `aws-efa` bug).

The registry default (`helm.defaultVersion` / `kustomize.defaultTag`)
is therefore the single source of truth for a component's version, and
`base`/overlay/mixin `componentRefs` carry **no version pins** apart
from declared divergences (#1616): resolution falls back to the
registry default, so a version bump edits `registry.yaml` in one place
and no overlay needs touching. `TestOverlayVersionPinsMatchRegistry`
(`pkg/recipe/version_pin_guard_test.go`, run by `make test` →
`make qualify`) fails on a non-exempted pin whenever the component has
a matching registry default: a pin that diverges from it is undeclared
drift, and a pin that equals it is redundant — it doubles bump churn
and shields the overlay from an external registry's default override.
(Refs outside the registry, or whose matching default is empty, are out
of the guard's scope; `make bom-pinning-check` separately requires
every embedded chart-bearing Helm component to declare `defaultVersion`
— manifest-only components have no chart version to pin.) If an
overlay must legitimately run a different chart version (e.g. a
platform validated against an older chart), pin it **and** add an entry
to `versionPinExemptions` with a justification — a declared divergence
is not a silent one. Only Helm `version` divergences are exemptable
today; a Kustomize `tag` exemption is rejected until the BOM variants
pipeline can represent it. Do **not** exempt a component whose only
consumer diverges; that reinstates the exact fiction the guard
prevents.

## Common Pitfalls

- **Skipping `make bom-docs`** after a values change that alters
  rendered images. A stale version *column* now fails CI
  (`TestCommittedBOMVersionsMatchRegistry`), but *image* drift from a
  values change without a pin bump does not surface in qualify — the
  BOM goes stale silently. See
  [Version freshness is gated; image drift is not](#version-freshness-is-gated-image-drift-is-not).
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
- **Setting identity fields in a mixin componentRef when overriding
  an inherited component.** A mixin may not override `chart`,
  `version`, `valuesFile`, etc. on a component the inheritance chain
  already carries — the resolver rejects with the offending field
  name. Move chart-changing logic to an overlay. (A mixin may still
  *introduce* a new component with these fields.)
- **Pinning a component version/tag in a recipe ref.** The registry
  default (`helm.defaultVersion` / `kustomize.defaultTag`) is the single
  source of truth and `base`/overlay/mixin refs carry no non-exempted
  version/tag pins; a bump edits `registry.yaml` only.
  `TestOverlayVersionPinsMatchRegistry` fails on a non-exempted pin with
  a matching registry default — divergent (undeclared drift; most
  dangerously, when the diverging overlay is the component's sole
  consumer the BOM's default row advertises a version no recipe
  installs) or default-equal (redundant) — see
  [Version pinning is single-source](#version-pinning-is-single-source).
- **Assuming the cluster fingerprint is trustworthy.** The
  fingerprint block persisted in `aicr snapshot` output is
  advisory; trust-bearing consumers recompute via
  `fingerprint.FromMeasurements(...)` before acting. See the
  collector docs and ADR-007 for details.
- **Adding a collector subtype or closed-space key without updating the
  constraint path catalog.** `pkg/measurement/catalog.go` enumerates
  every addressable path, and recipe loading rejects anything it cannot
  address (#1783). Each subtype declares *two* independent key spaces,
  because extraction reads them from different places:
  `{Type}.{Subtype}.{Key}` resolves against `Subtype.Data` (scalar), and
  `{Type}.{Subtype}[<selector>].{Key}` resolves against
  `ItemEntry.Data` then `ItemEntry.Context` (item). Register a new key
  in whichever space emits it — an item-only subtype needs its keys in
  the item space, or every selector path through it is rejected. An
  entry is required for a new subtype (unless the Type is open-subtype)
  and for a new key in a *closed* space; a new key in an *open* space
  needs nothing. A missing entry does not weaken a check — it makes a
  legitimate constraint path fail at load for whoever first writes it.
  Declare a key space open when it is not provably fixed (image names,
  sysctl paths, label keys). Note that `ItemEntry.Context` keys **are**
  addressable and belong in the item space; only `Subtype.Context` is
  never addressable, so do not list those.

## See Also

- [recipe-development.md](../integrator/recipe-development.md) — end-user recipe authoring guide
- [component.md](component.md) — adding a component to the registry
- [validator.md](validator.md#component-validations-bundle-time) — adding bundle-time component validation checks
- [validator.md](validator.md) — adding a validator check or health check
- [ADR-005](../design/005-overlay-refactoring.md) — overlay refactoring rationale (mixin composition, maximal-leaf resolver, wildcard overlays)
- [ADR-007](../design/007-recipe-evidence.md) — fingerprint, evidence bundle, verification
- [pkg/recipe godoc](https://github.com/NVIDIA/aicr/tree/main/pkg/recipe) — implementation
- [api/aicr/v1/server.yaml](https://github.com/NVIDIA/aicr/blob/main/api/aicr/v1/server.yaml) — recipe API contract and criteria enums
