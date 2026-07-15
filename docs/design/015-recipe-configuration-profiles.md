# ADR-015: Recipe-Declared Configuration Profiles

## Status

**Accepted** — 2026-07-21 (proposed 2026-07-14).

Originated from an internal GKE device-plugin ownership discussion
(2026-07-14) and
[#1755](https://github.com/NVIDIA/aicr/issues/1755) (fail-closed check for
GKE device-plugin ownership conflict), but the mechanism is
**service-agnostic**: it applies wherever one criteria combination maps to
more than one qualified cluster configuration.

## Problem

A single criteria combination increasingly maps to more than one *valid*
cluster configuration. The recurring axis is **who owns a layer of the GPU
stack — the cloud service or the GPU Operator**:

- **AKS**: two documented *unmanaged*-pool ownership modes
  (`docs/integrator/aks-gpu-setup.md`): GPU Operator-managed pools created
  with `--gpu-driver none` (operator installs driver + toolkit) vs. the AKS
  **"Driver only"** install profile with driver and toolkit preinstalled
  (gpu-operator needs `driver.enabled=false`, `toolkit.enabled=false`,
  `operator.runtimeClass=nvidia-container-runtime`, all three together —
  mixing modes leaves containerd without a working `nvidia` runtime
  handler). Fully AKS-managed GPU node pools (`--enable-managed-gpu=true`,
  preview) additionally own the device plugin, DCGM exporter, and health
  tooling; that mode conflicts with the operands AICR deploys and stays
  **out of scope** here, as the AKS guide already states.
- **GKE**: device-plugin ownership. The stock profile runs the GPU
  Operator's device plugin and requires GPU node pools to carry
  `gke-no-default-nvidia-gpu-device-plugin=true`; a default-provisioned GKE
  cluster instead runs GKE's managed plugin. Both are coherent
  single-advertiser states with *opposite* validation constraints.
- **GKE (DGXC/NKX)**: driver preinstalled by provisioning vs. installed by a
  cos-gpu-installer component (internal recipes MR #27).

The same shape will recur for other services (OKE addons, OCP operators).

AICR yields **one resolved recipe per criteria combination**. Today the only two
outlets for a second configuration are:

1. `--set` overrides at bundle time — **invisible to validation**. Overrides
   are applied when the bundle is rendered
   (`pkg/bundler`, after recipe values) and are not recorded in
   `recipe.yaml`, so `aicr validate -r recipe.yaml -s snapshot` checks the
   *stock* profile's constraints against a cluster running a *different*
   configuration. The existing #1327 bundle-override policy
   (`pkg/bundler/allocation_policy.go`) rejects only `--dynamic` on
   allocation-policy keys; static `--set` on those keys merely warns.
2. A new overlay distinguished by a new criteria value — grows the recipe
   matrix and the criteria schema (see Alternative B).

Goal: support multiple qualified configurations per recipe **while keeping
the deployed configuration validated against the recipe** — the recipe stays
the single source of truth for what validation expects on the cluster
(the #1327 contract).

## Non-Goals

- Auto-detecting cluster state and silently selecting a configuration
  (e.g. flipping `devicePlugin.enabled` when the GKE opt-out label is
  absent). Ownership stays explicit in the recipe; recipe specialization
  requires explicit intent.

  The shipped snapshot-driven driver auto-override
  (`applyGPUDriverAutoOverride`) predates this ADR and is
  **value-level, never profile selection** — on a profile-bearing
  composition it is **subordinated** to the declaration (see Override
  locking).
- Replacing `--set` for paths the profile does not own. Bundle-time
  overrides remain the tool for validation-neutral tweaks (registry
  mirrors, resource limits, `allowedSourceRanges`).
- A generic parameter language. A resolved recipe carries at most one
  effective profile declaration, and exactly one value when a declaration
  exists; multiple independent parameters (and their Cartesian product of
  configurations) are explicitly rejected — see Alternative C.
- Changing today's overlay/mixin constraint-eligibility semantics
  (exclude-and-record). Fail-closed evaluation applies to
  profile-contributed constraints and — the profile-specific
  exception to exclude-and-record — to snapshot exclusions that would
  remove the composition's profile declaration (see the resolution
  algorithm).
- Free-form templating of recipes.

## Context

- Recipe selection: criteria (`service + accelerator + os + intent
  (+ platform)`) resolve to a set of **maximal leaf candidates** whose
  inheritance chains and mixins are merged into one result
  (`pkg/recipe/metadata_store.go`); independent
  co-matched overlays (e.g. `monitoring-hpa`) merge alongside the primary
  chain.
- Snapshot-filtered generation evaluates
  each candidate overlay's top-level `Spec.Constraints` **before** merging;
  failing candidates are excluded and recorded in
  `metadata.excludedOverlays` / `constraintWarnings`.
- The generated artifact is a flat `RecipeResult`
  (`pkg/recipe/metadata.go`): `kind`/`apiVersion`/`metadata` plus resolved
  content — no `spec` wrapper. Hydrated recipes are consumed **without
  overlay resolution**: `pkg/recipe/loader.go` loads them from disk, and
  `pkg/client/v1` `adoptRecipe` accepts them from `/v1/bundle` POST bodies.
  Enforcement at bundle time therefore cannot rely on consulting the
  overlay catalog (which may have changed since generation).
- Overlays already carry component fragments via
  `componentRefs[].overrides` / `valuesFile`; mixins carry `constraints` +
  `componentRefs` only.
- `aicr bundle -r recipe.yaml --set key:path=value` applies value overrides
  via registry `valueOverrideKeys` (alias-resolved) during bundle rendering.

## Decision

Introduce **recipe-declared configuration profiles**: an overlay may declare
a single named profile enum; exactly one value is selected at **recipe
generation** time (explicitly or by declared default) and is hydrated into
the generated `recipe.yaml` together with its effects and a compact
ownership record.

### Declaration

A profile value's effect is a **recipe fragment reusing existing syntax**
(`componentRefs[].overrides` and `constraints`) — exactly mixin-shaped,
kept as a distinct profile fragment so existing component merge, registry
defaulting, and dependency validation apply unchanged. The one
profile-specific field not inherited from existing fragment syntax is
`advertiser` (see the #1327 amendment below): a value that hands
`nvidia.com/gpu` advertisement to a provider-managed plugin declares
`advertiser: external`. It is optional and `external` is its only value —
operator ownership is never declared, it is read from the effective
values (`devicePlugin.enabled`) as today. Two consumers, same shape:

```yaml
# recipes/overlays/aks.yaml — driver/toolkit ownership (unmanaged pools only;
# fully AKS-managed GPU pools are out of scope, see aks-gpu-setup.md)
spec:
  profile:
    name: gpuStack
    description: Who installs the GPU driver and container toolkit.
    default: operator
    values:
      operator:            # node pools created with --gpu-driver none
        componentRefs:
          - name: gpu-operator
            overrides:
              driver:  {enabled: true}
              toolkit: {enabled: true}
              operator: {runtimeClass: null}   # authored absence — union
                                               # totality, see below
      driver-only:         # AKS "Driver only" install profile:
                           # driver + toolkit preinstalled on the node
        componentRefs:
          - name: gpu-operator
            overrides:
              driver:  {enabled: false}
              toolkit: {enabled: false}
              operator: {runtimeClass: nvidia-container-runtime}
        # constraints: TBD — see note below; no snapshot signal exists yet.
```

The `driver-only` value deliberately shows no constraint: AICR's GPU
collector is driver-free by design, and the only driver signal it
captures is a
`GPU.hardware.driver-loaded` boolean (`pkg/collector/gpu`), which proves
driver *presence*, not which mode owns it. No *node-level
installed-driver* or node-pool-mode reading exists in snapshots today —
the snapshot's `K8s.policy.driver.version` is the ClusterPolicy-
*configured* value, post-deployment intent rather than node state.

The shipped `gpuDriverState` heuristic is a legacy value-level
optimization on unprofiled compositions, never profile selection, and
not the authoritative signal this value needs — it samples a single
node and is ambiguous once any installer has run.

The *authoritative*
node-pool-mode signal (an AKS-set node label, the `gpuProfile` surface, or
a new collector reading) must be identified and documented before this
profile value ships. The rule, precisely: a profile value must carry a
validation signal that **distinguishes its configuration from every
sibling value's** — a value without one (no signal at all, or
constraints identical to a sibling's) does not satisfy this ADR's
"validated against deployed config" claim and must not be declared.
Values shown in this document without such a signal are target state,
gated by this rule.

```yaml
# recipes/overlays/gke-cos.yaml — device-plugin ownership
# Shown with the post-DD5 value set. DD5's durable marker, not yet
# identified, will additionally land as symmetric constraints on
# operator and operator-selfdriver at that event — the constraints
# drawn here are therefore not yet the declarable post-DD5 state.
spec:
  profile:
    name: gpuStack
    description: Who owns GPU device advertisement.
    default: operator
    values:
      # GKE-installed driver, GKE's default plugin suppressed by the node
      # label — the GPU Operator's plugin is the sole advertiser (today's
      # qualified state).
      operator:
        componentRefs:
          - name: gcp-driver-installer
            overrides:
              install: false      # every value assigns every union path
          - name: gpu-operator
            overrides:
              devicePlugin: {enabled: true}
        constraints:
          - name: NodeTopology.gpu-nodes.label   # requires #1755
            value: gke-no-default-nvidia-gpu-device-plugin=true
      # GKE-installed driver AND GKE's managed device plugin — a
      # default-provisioned GKE cluster (no node label required).
      csp-managed:
        advertiser: external      # GKE's managed plugin owns nvidia.com/gpu
        componentRefs:
          - name: gcp-driver-installer
            overrides:
              install: false
          - name: gpu-operator
            overrides:
              devicePlugin: {enabled: false}
        constraints:
          - name: NodeTopology.gpu-nodes.label
            value: "!gke-no-default-nvidia-gpu-device-plugin"
      # TARGET STATE — not declarable until its distinguishing signal
      # is identified (Deferred Decision 5). Driver NOT preinstalled
      # (DGXC/NKX-style): the values-gated gcp-driver-installer
      # component installs it; the operator's plugin advertises.
      operator-selfdriver:
        componentRefs:
          - name: gcp-driver-installer
            overrides:
              install: true       # the chart-level gate
          - name: gpu-operator
            overrides:
              devicePlugin: {enabled: true}
        constraints:
          - name: NodeTopology.gpu-nodes.label
            value: gke-no-default-nvidia-gpu-device-plugin=true
```

The GKE declaration lives once in `gke-cos`; accelerator/intent leaves
(`h100-gke-cos-*`, `b200-gke-cos-*`, `gke-cos-training`, …) inherit it
with **zero changes**. The initial declaration carries only the
`gpu-operator` componentRef; the `gcp-driver-installer` refs — and its
paths in the union — appear only at the DD5 landing event adoption
step 2 describes. Selection:

```bash
# operator (declared default) — no flag needed
aicr recipe --service gke --os cos --accelerator h100 --intent inference

# explicit alternative configuration
aicr recipe --service gke --os cos --accelerator h100 --intent inference \
  --profile gpuStack=csp-managed
```

A profile fragment may reference only components **enabled in the
pre-profile resolved composition** — checked at resolution time, when
the surviving composition is known (catalog load cannot see snapshot
exclusions). A profile changes how existing components are configured,
never the component set — in either direction.

A fragment override therefore must not set the root `enabled` key
(rejected at catalog load): the component model reads
`overrides.enabled: false` as component removal, which would mutate
the component set through the value map. Component presence is still
protected — the synthetic `enabled` path (see Recording) keeps
explicit toggles from changing the pre-existing presence state, with
subset-filter semantics defined under Override locking.

**Conditional installation is expressible in v1 through a values-gated
component** — the `operator-selfdriver` value above is the pattern.
The component sits unconditionally in the composition and renders
nothing unless its gate value is selected; the profile value flips a
plain values path — owned, locked, and validated like any other —
while the component *set* stays identical across values. Selection is
explicit in the recipe, never runtime detection. The pattern's one
standing cost: once declared, the gated component is locked for
**every** selection, even values where it renders nothing (see
Override locking) — that lock is exactly what keeps non-selected
values from smuggling the installation in. Packaging details
(chart gating, ordering, installer idempotence, zero-render health
semantics) are in
[#1761](https://github.com/NVIDIA/aicr/issues/1761).

True component-*set* mutation (a fragment adding or removing registry
components) remains a **future amendment**: no consumer requires it
once conditional installation is values-gated, and admitting it would
pull addition and ordering-declaration semantics into the fragment
schema ahead of any user.

**The fragment schema is closed: a profile value permits `advertiser`,
`constraints`, and `componentRefs` only, and each `componentRef`
permits only `name` and `overrides` — every other `ComponentRef` field
is rejected at catalog load** (full field inventory in #1761).

The operative criterion: a fragment field is permitted only when its
effect is either representable in `ownedPaths` (value paths plus the
synthetic `enabled`; defined under Recording and wire model below) or
persisted verbatim in the artifact with no bundle-time override
surface (`constraints` join the artifact's constraint set;
`advertiser` is recorded in `selectedProfile`). Every other
`ComponentRef` field fails both tests — an effect the artifact cannot
carry is an effect the lock cannot protect.

Enforcing a closed schema also requires seeing the keys: overlay
metadata is decoded non-strictly today, so **strictness keys on the
artifact version, not on finding a profile — any `RecipeMetadata`
document carrying the new recipe apiVersion is decoded strictly in
full, and a new-version document without a `spec.profile` declaration
is rejected**.

Three value-shape rules complete the schema, each keeping the
flattened `ownedPaths` record faithful to a fragment's effect:
**literal dotted map keys are rejected** (a flattened record cannot
distinguish a literal dotted key from nested segments), **nested
empty-map assignments are rejected** (an effective merge change the
ownership record cannot represent), and **whole-list replacement is
allowed** — a list-valued override replaces the earlier layer's list,
never merges. The rejections' amendment paths and worked cases are
pinned in #1761.

`valuesFile` is additionally rejected for a merge-semantics reason: a
later `valuesFile` **replaces** the earlier one, so a profile could
change effective values through paths no syntactic union could lock —
support is deferred until a consumer requires it.

**One profile per resolved composition.** A `RecipeResult` may be
influenced by at most one effective profile declaration across all
contributing sources — candidate chains and mixins alike (mixins
remain `constraints` + `componentRefs` only and **cannot declare
profiles**). A second declaration anywhere is an error, enforced by
the fail-closed pre-filter guard (resolution step 1); **static
catalog-time co-match analysis is deliberately omitted** —
criteria-subsumption analysis is complex and the runtime guard is
complete on its own. This lets `aks` or `gke-cos` declare the profile
once, inherited by accelerator/intent leaves.

### Optionality and naming

- The `profile` block is **optional**. The field is deliberately
  **singular**: a composition carries at most one declaration, and the
  plurality lives in its `values` map — a plural `profiles:` would
  misread as a list of independent declarations, which Alternative C
  rejects. An overlay family that does not declare one behaves
  **byte-identically to today**, digest included (see Consequences —
  non-breaking by construction).
- Selecting a profile against a composition with no declaration is
  `ErrCodeInvalidRequest` — fail closed, never silently ignored — so a
  user cannot believe they selected a configuration and receive the stock
  one.
- When a declaration exists, `default` is **required** and must name one
  of the declared `values` — both rules validated at catalog load.
  Explicit selection overrides the default. This forecloses the undefined
  "no `default`, no `--profile`" state: the no-flag workflow always
  resolves to exactly one value.
- Profile and value names (`gpuStack`, `operator`, `csp-managed`,
  `driver-only`) are **overlay-scoped identifiers, not reserved
  keywords**: validity is membership in the declaring overlay's enum.
  Both are lexically constrained at catalog load so the single
  `name=value` wire form carried by every selection surface is always
  unambiguous (grammar in #1761).
  Naming follows a cross-service vocabulary **to be documented in
  `docs/contributor/recipe.md`** (part of this ADR's docs cost): a value
  name must name the precise
  ownership split it encodes (see Consequences), and only qualified,
  validatable configurations may be declared.
- The single piece of **reserved vocabulary** is `advertiser`: an
  optional marker whose only value is `external`, feeding #1327 policy
  resolution and rejected at every boundary when unknown.

### Resolution algorithm

Existing candidate eligibility is preserved; profile specialization applies
to the surviving composition:

1. Resolve maximal-leaf candidates from criteria. **Before any snapshot
   filtering**, collect the profile declarations reachable from every
   candidate chain and enforce composition-wide uniqueness (fail closed
   on duplicates). Deduplication is by declaration *source*: the same
   declaring overlay reached through multiple chains counts once;
   independently authored declarations fail uniqueness even when
   structurally identical.
2. On the snapshot-filtered path, apply today's overlay/mixin constraint
   filtering unchanged (exclude-and-record in
   `metadata.excludedOverlays` / `constraintWarnings`) — with one
   profile-specific exception: **if step 1 found a declaration and no
   surviving candidate chain carries it, recipe generation fails** with
   the excluding constraints' diagnostics instead of falling back to the
   un-specialized composition. Today's silent base-configuration
   fallback would otherwise emit an unprofiled, lock-free artifact for
   criteria whose no-flag workflow promises exactly one profile value —
   the same divergence step 5 forecloses, one level up.
3. Validate the requested profile value against the declaration, or
   apply the declared default; any invalid selection is
   `ErrCodeInvalidRequest` — **fail closed, never ignore the
   selection** (rules in Optionality and naming).
4. Validate fragment membership against the **surviving** composition —
   every component referenced by **any declared value** (not only the
   selected one) must be enabled after step 2's filtering, because
   `ownedPaths` is declaration-wide: a selection must never record lock
   paths for a component absent from its own composition (a snapshot
   exclusion can disable a referenced component, failing generation
   here) — then apply the selected
   value's fragment at highest recipe precedence,
   after all surviving overlays and mixins. The fragment
   **authoritatively supersedes** earlier assignments of its owned
   paths — base and overlay values files routinely assign them, and
   overriding that baseline is the fragment's function, so a mechanical
   collision rejection would reject every declaring family. The
   supersession is a conversion-review obligation, not a runtime check:
   descendant or external specializations of now-owned paths must be
   found and resolved when a family converts (see Consequences).
5. Merge the selected value's constraints into the composition under
   the **mixin collision rule**: a profile constraint whose name
   collides with a chain or mixin constraint rejects at resolution —
   constraints don't compose, the same rule `mergeMixins` already
   enforces. Values of one declaration may reuse a constraint name
   across values (only one is ever selected). Then evaluate
   **profile-contributed constraints** fail-closed: under a
   provided snapshot, a failing profile constraint fails recipe generation
   with the constraint diagnostics — it does not exclude anything or fall
   back. The profile lives inside an already-matched composition, so no
   alternative resolution exists, and falling back to the un-specialized
   recipe would produce exactly the divergence this ADR exists to prevent.
   Chain/mixin constraint semantics are unchanged (Non-Goals).
   Profile-contributed constraints are merged into the hydrated
   artifact's constraint set like any others, so `aicr validate`
   re-evaluates them against the cluster post-deployment — the
   generation-time fail-closed evaluation is in addition to, not instead
   of, the validate phase.
6. Finalize the `RecipeResult` with the selection, the compact ownership
   record, and the new recipe artifact apiVersion (see the compatibility
   gate below).

### Recording and wire model

- The flat `RecipeResult` records the selection **and a compact
  ownership record** under `metadata.selectedProfile`:

  ```yaml
  metadata:
    selectedProfile:
      name: gpuStack
      value: csp-managed
      advertiser: external      # optional; present only for external
      ownedPaths:               # lexicographic per component — feeds the
                                # digest, so ordering must be byte-stable
        # Post-DD5 state shown; the initial recording is
        # gpu-operator: [devicePlugin.enabled, enabled] only.
        gcp-driver-installer: [enabled, install]
        gpu-operator: [devicePlugin.enabled, enabled]
  ```

  `ownedPaths` derives from fragment overrides by **leaf flattening**:
  every non-map assignment contributes its dotted path — a scalar leaf,
  or a list or `null` assignment owning the path to that value — and
  map values recurse.

  **Union totality (catalog-validated): every declared value must
  assign every path in the declaration's fragment-derived,
  leaf-flattened override-path union, evaluated before synthetic
  presence paths are added — equivalently, all declared values have
  identical leaf-flattened path sets.** The synthetic per-component
  `enabled` path is part of `ownedPaths` but exempt from totality:
  fragments may not assign it. An owned path is
  never inherited from the baseline — supersession applies only the
  *selected* fragment, so a value that omitted an owned path would let
  a pre-profile assignment (a values file, an external overlay) survive
  the selection and be locked-and-attested as if qualified. Totality
  makes each value a complete assignment of the declaration's
  override-path union.

  `ownedPaths` is the **union of profile-owned paths across all values
  of the effective declaration**, not only the selected fragment —
  otherwise selecting `operator` could still override a path introduced
  only by `csp-managed` and create an unqualified hybrid. Component
  *presence* is modeled as the synthetic `enabled` path, added for
  every component any value references — the same way the #1327 policy
  map models it; there is no separate `lockedComponents` list.

  The automatically added #1327 policy closure is **never
  persisted** — each boundary recomputes the effective lock set as
  `ownedPaths` plus the canonical closure (see the #1327 amendment),
  so artifacts pick up future policy keys automatically.

  `ownedPaths` is included in the recipe digest and changes only when
  the ownership *surface* changes — editing an unselected value's
  assigned values does not invalidate digests of recipes that selected
  the other value. The converse is a cost worth naming: *changing the
  ownership surface in either direction* — owning a path not already in
  the union (through a new value or a new path on an existing value),
  or removing a path's last owner, which also unlocks it — changes
  `ownedPaths` and with it the digest of every recipe generated from
  the family, including selections whose effective configuration did
  not change; each ownership-surface change is therefore a
  re-qualification and evidence re-signing event for the family. A new
  value that only re-assigns paths already in the union changes no
  digest (see Consequences). All digest-stability statements are modulo
  the digested CLI-version stamp: regenerating with a newer binary
  changes `metadata.version`, and with it the digest, independently of
  profiles.

  Declaration-owned paths never retro-strengthen issued artifacts — an
  artifact's `ownedPaths` is what it recorded at generation; the
  recomputed policy closure is the deliberate exception (see the #1327
  amendment). The declaration itself stays in overlay metadata
  (`RecipeMetadata.Spec`).
- Selection surfaces all carry the same single field: the CLI flag and
  config file, the SDK request option, the `/v2` resolving endpoints
  (GET parameter or POST envelope — see the compatibility gate), and
  criteria-based `aicr query` / `aicr mirror list`. **`/v2/bundle`
  carries no selection field — it transports an already-selected
  artifact** (the generate-first workflow). **Direct overlay hydration
  (`aicr bundle -r <overlay.yaml>`) deliberately exposes no selection
  surface and applies the declared default** — selecting a non-default
  value means generating the recipe first.
- Discovery surfaces the **effective declaration after inheritance and
  co-match resolution** — the declaration typically lives on an
  ancestor (`gke-cos`), so a criteria-filtered listing of leaves must
  still surface it, not merely copy each overlay's local block;
  `aicr query` exposes the resolved `metadata.selectedProfile` via
  `--selector` like any other hydrated field.
- The selected profile is part of recipe identity: evidence/attestation
  digests change with it, so each value is qualified and signed
  separately. Evidence stores must be able to *hold* per-value results
  separately too. The corroboration projection keys results by recipe
  coordinate, signer, and run — all identical across two values of one
  family — so the profile value must join a **path-forming key
  segment** there (metadata- or display-label-only placement leaves
  one value's results overwriting another's); the TestGrid publisher
  needs no path change — its digest-bound build ID already partitions
  per value, since selection changes the bundle digest — only
  acceptance of the new predicate type, with any profile display
  metadata a product decision. And the repo evidence gate must
  recompute each pointer's expected digest from that pointer's
  recorded selection — a single selection-less hydration matches only
  the declared default (wiring in #1761).

### Override locking

Profile-owned paths are locked. At bundle time the **effective lock set**
is `selectedProfile.ownedPaths` **plus the recomputed canonical #1327
closure** when the closure trigger applies (see the amendment below).
`aicr bundle` **rejects** (`ErrCodeInvalidRequest`) any override —
static `--set`/`--set-json`/`--set-file` or config-file — whose result
**diverges from the selected recipe at a locked path** (a redundant
override that restates the qualified value passes), and any `--dynamic`
declaration on a locked path **regardless of value** (the mutability
condition of the invariant below). Path intersection uses the existing
exact/parent/child and registry-alias matching from
`pkg/bundler/allocation_policy.go`.

**Locked-path identity is a new relation this ADR defines over a
three-valued observation, evaluated on the effective post-merge
candidate.** A locked path is **present** (with deterministic canonical
bytes, from the same serializer the recipe digest uses), **cleanly
absent**, or **blocked** — a non-map ancestor makes the leaf
untraversable. Identity requires the same observation on candidate and
recipe, and byte-identical values when present.

Two consequences are load-bearing: **a valid recipe is never
blocked** — a recipe-side blocked observation is an incoherent
ownership record, rejected at generation and at the hydration gate,
so in the identity evaluation blocked arises only candidate-side and
is always a divergence; and **equality is byte-level
and deterministic, never mathematical** — nulls follow the effective
post-merge candidate, not the override's tokens, and numeric
canonicalization is deliberately not attempted. Worked cases (null
spellings, numeric coercions, the serializer binding) are pinned as
acceptance criteria in
[#1761](https://github.com/NVIDIA/aicr/issues/1761).

Component *presence* is enforced through the synthetic `enabled` path,
which locks the recipe's enabled/disabled state against **explicit
change**:

- An `enabled`-state override (`--set <c>:enabled=...`) on a component
  with an owned `enabled` path is rejected when it **diverges from the
  recipe's presence state** — restating the recipe's own state passes.
- A `bundlers`/`WithBundlers` subset that omits a component
  contributing any path to the **effective lock set — declaration
  `ownedPaths` or the recomputed closure** (on GKE the closure locks
  `nvidia-dra-driver-gpu` paths, and every recipe inherits that
  component from base) — is **rejected**: the pre-output
  invariant cannot evaluate state at locked paths absent from the
  output; for declaration-owned components, the bundle's attested
  `recipe.yaml` would additionally record a `selectedProfile`
  referring to a component the bundle does not
  carry. The inability to evaluate an omitted locked component is
  sufficient on its own. Subsets omitting *unrelated*
  components keep #1531's satisfied-externally semantics unchanged.
  Restoring owned-component subset redeploys requires a contract for
  validating externally satisfied components — explicit follow-up work
  if demand appears.
- The synthetic `enabled` path does not block unrelated value paths on
  those components; value locking is governed by the explicit owned
  paths.

**The lock is a pre-output invariant, not a set of per-writer checks.**
Before any output is rendered or written, two conditions must hold,
enforced at the bundle and mirror boundaries (`ErrCodeInvalidRequest`
on violation):

1. **State** — the effective candidate configuration equals the
   hydrated selected recipe at every locked path and every locked
   component-presence state.
2. **Mutability** — no supported output surface exposes a locked path
   or presence state as an install-time parameter. A `--dynamic`
   export of a locked path fails even though it leaves the bundle-time
   value unchanged: the path would become operator-controlled at
   installation.

Component-presence state is the recipe's enabled/disabled value
(subset and toggle semantics in the presence bullets above).

Stating the rule as an outcome makes it writer-proof by construction.
User overrides, component filtering, registry-driven injections (node
scheduling, node count, storage class), bundler-derived defaulting,
and any mutation source added later are all caught without this ADR —
or a future contributor — having to enumerate them. A redundant write
that lands exactly on the qualified value passes instead of
false-rejecting. Presence verdicts are symmetric across writers: at
unrelated components, subset omission and explicit disables both pass;
at locked components, omission rejects (evaluability) and a divergent
disable rejects (state).

Per-surface checks (the override matcher above) remain as **early
diagnostics only**: they attribute a divergence to the flag that
caused it, which the outcome check cannot, and may fail fast when a
violation is conclusive — but they are not independently
authoritative.

There is deliberately **no catalog-time mirror of this rule**. A
registry path is only a *potential* mutation target (selectors inject
only when supplied, node count only when positive, storage class only
when set), so owning such a path is legal and bundles safely until a
conflicting write actually fires. The runtime invariant is the single
authoritative check, and no second catalog of mutation paths needs
maintaining.

**Generation-side mutators never write locked paths.** The pre-output
invariant guards bundle and mirror outputs against divergence from the
artifact — it cannot guard the artifact against its own generator. A
generation-time auto-override writing an owned path (the shipped
snapshot-driven driver auto-override injects `driver.enabled=false` —
exactly the path the AKS declaration owns) would bake divergence into
the artifact itself. On a profile-bearing composition, generation-side
auto-overrides therefore **skip paths in the effective lock set** and
log the skip — an explicit profile selection is the stronger intent
signal. Non-profile compositions keep today's auto-override behavior
unchanged.

Signing a bundle whose overrides or component filter contradict the
recorded profile would attest to an unqualified configuration;
rejection is the only disposition consistent with the evidence
contract.

The lock covers **every supported override surface, not only
`aicr bundle`**. `aicr mirror list` accepts the same `--set` overrides
and applies them at discovery time before rendering charts
(`pkg/mirror/discover.go`), and the air-gap guide today recommends
`--set gpuoperator:driver.enabled=false` for preinstalled drivers —
exactly a path an AKS profile owns. Unenforced, an operator-profile
recipe could be mirrored with driver images omitted while the actual
bundle (where the same override is rejected) requires them — an
incomplete air-gap inventory for the only deployable configuration.

Mirror therefore runs the shared gate against a defensive copy
(discovery must not mutate its input) and enforces the same pre-output
invariant against its discovery-time effective values — the **same
validator and canonicalization as the bundler**, or the same input
could produce different verdicts at the two boundaries (wiring in
#1761).

The argocd-helm deployer adds an install-time surface the bundle-time
check cannot see: its root chart deliberately makes every component
value overridable at `helm install` time, so an installer could change
an owned path without touching the attested bundle.

**Profile conversion must preserve all otherwise supported
deployers**, so argocd-helm remains supported via a **template-time
guard that fails rendering on any install-time input that structurally
intersects a locked path** — the install-time equivalent of the
invariant's mutability condition: no final candidate values exist at
install time to compare, so the guard **rejects structural presence
(exact/parent/child, mirroring the bundler's matching) and never
compares effective values**, and its failure message names the locked
path.

The guard is deliberately the **only** lock emission: encoding the
lock a second time into `values.schema.json` would duplicate the
matcher semantics for zero enforcement weight — **schema validation is
skippable, templates always render**; the chart's existing
`deployer.*` schema is unchanged (guard mechanics in #1761).

**Descriptor expansion and frozen outputs.** One boundary caveat: the
emitted guard **freezes the generation-time closure** — a rendered
chart is not a boundary where an AICR binary can recompute it. After a
canonical-descriptor expansion, an existing authentic argocd-helm
bundle still carries its old guard and accepts an install-time value
on the newly recognized path. The same frozen-output condition applies
to every rendered output: any write legal before the expansion — a
static override, registry injection, derived mutation, component
subset, or a `--dynamic` export persisted operator-editable
(per-deployer persistence mechanisms are enumerated in #1761) — may be
baked into output that, under the expanded closure, diverges at a
newly locked path or leaves it install-time-mutable.

A descriptor expansion therefore carries three obligations, recorded
alongside each expansion:

- **Outcome-based output remediation, not per-deployer**: regenerate
  (and re-attest) every previously rendered profiled output that, at a
  newly locked path or presence state, diverges from its selected
  recipe or exposes install-time mutability — or, when proving that
  per output is impractical, conservatively all outputs affected by
  the new descriptor entry.
- **Re-qualification and evidence re-signing for affected profile
  values**: the closure is absent from the artifact and its digest by
  design, so expansion changes no recipe digest and previously signed
  evidence still gate-matches — despite having been collected under an
  evaluator that did not know the new selector. Conservatively,
  re-sign all values affected by the descriptor entry when narrower
  proof is impractical.
- **Evidence currentness (an explicit, profile-scoped ADR-007
  amendment)**: re-signing alone cannot retire old evidence — ADR-007
  pointers are immutable and add-only, discovery aggregates every
  pointer, and the gate digest-matches each independently. For
  profile-bearing evidence, currentness therefore requires both the
  recipe digest and the canonical-descriptor identity in the signed
  predicate to match — the deterministic identity of the
  canonical-descriptor entries contributing to that recipe's effective
  closure: an empty contribution set has a deterministic empty-set
  identity, and a recipe whose closure an expansion does not touch
  keeps a matching identity. Missing or mismatched identity is
  historical-only. Unprofiled evidence retains ADR-007's digest-only
  currentness rule — existing predicates carry no descriptor identity,
  and a global rule would either strand all legacy evidence
  (contradicting the non-breaking guarantee) or, read as a wildcard,
  recreate the expansion bypass. That scoped identity is recorded in
  the evidence payload — never in `RecipeResult`, preserving digest
  stability — so pre-expansion evidence stays historically valid
  while — for recipes whose effective closure the expansion touches —
  ceasing to corroborate the post-expansion configuration.

  Profile-bearing evidence rides a **new predicate type**, and the
  contract is **bidirectional**: released verifiers hard-require the
  existing predicate type and decode predicates non-strictly, so an
  identity field added to the v1 predicate would be silently ignored —
  stale profile evidence reported valid, the exact fail-open class the
  artifact apiVersion exists to prevent. A new type fails closed on
  released verifiers for free. New verifiers **reject v1-predicate
  evidence for a profile-bearing recipe** (accepting both would let a
  new profiled artifact pair with freshly signed v1 evidence and
  bypass descriptor identity); unprofiled evidence keeps the v1
  predicate. ADR-007 receives a reciprocal "Amended by ADR-015" banner
  on acceptance.

Finally, expansion is a **minimum-binary-version cut-over** for the
affected profiled families: descriptor expansion strengthens artifacts
only when processed by binaries at or above the recorded minimum
version — an older profile-aware binary still computes the old
closure, keeps producing under-locked output, and honors its old
descriptor identity, and no artifact-side mechanism can stop it.
Binaries below the minimum are unsupported for those families after
the expansion. This is an operational rollout boundary, stated as
such, not an enforcement mechanism.

Scope of the guarantee: **within AICR's supported override surfaces
(bundle, mirror, and argocd-helm install-time values), no path or
presence state in the effective lock set can change — on authentic,
unmodified artifacts.** Overrides
on unrelated paths retain today's semantics.

Raw deployer-native changes
at install time — direct `helm --set` or extra `-f` values files against
a rendered bundle's charts, Argo CD Helm parameters — sit outside AICR's
override surfaces and are **unsupported on any path or presence state
in the effective lock set**, the
same disposition as manual artifact editing: they are the documented
operator-domain surface that attestations do not bind. (The argocd-helm
guard exists because that deployer's root chart *designs in*
install-time values as a supported surface; plain rendered charts do
not.)

Manual editing of a
generated recipe is not an override surface the lock defends against:
locking defends against flag misuse, not artifact forgery — the digest
makes an edit observable, and a signature or evidence record anchored to
the expected digest detects it.

### Amendment to the #1327 allocation-policy model

The #1327 resolver (`pkg/validator/v1/allocation_policy.go`) currently
treats externally managed advertisers as an explicit non-goal: with
`devicePlugin.enabled=false` and full-GPU DRA disabled, resolution fails
closed as "no whole-GPU advertiser." Under that contract the GKE
`csp-managed` value is unrepresentable — #1755's inverted label check
could pass and validation would still fail at policy resolution, before
any check runs. This ADR therefore **amends the #1327 model**:

- A profile value may declare `advertiser: external`, recorded in the
  artifact as `metadata.selectedProfile.advertiser`. This is the **only**
  way to express an external advertiser — it is never inferred from
  `devicePlugin.enabled=false`, preserving #1327's fail-closed posture for
  recipes without the declaration (the non-goal narrows from "externally
  managed advertisers" to "*inferring* externally managed advertisers").
- Resolution counts a declared external advertiser as **the** advertiser
  in the exactly-one invariant. The dual-advertisement gates extend
  accordingly, fail closed: `advertiser: external` +
  `devicePlugin.enabled=true` is rejected, as is `advertiser: external` +
  DRA `gpus.enabled=true`.
- The resolved policy value is **unchanged**: GKE's managed plugin still
  provides `nvidia.com/gpu` through a device plugin, so resolution yields
  the existing `device-plugin-extended-resource` — the #1327 enum names
  the *request mechanism*, not provider ownership, and a new policy string
  would fail closed as unknown in evidence dispatch
  (`pkg/evidence/cncf/scripts/collect-evidence.sh`). `advertiser:
  external` is a separate ownership source recorded alongside the policy:
  ordinary device-plugin policy validation still proves GPUs are actually
  advertised, while the profile's constraints prove the intended
  *ownership* configuration (for GKE `csp-managed`: the opt-out label
  absent, so GKE's managed plugin is the advertiser).
- **Advertisement ownership locks the complete policy tuple — recomputed
  at every `RecipeResult` boundary, never persisted in the artifact.** When a profile declares
  `advertiser: external` **or explicitly owns a non-synthetic
  policy-selector path** (`devicePlugin.enabled`, a DRA resource path),
  the canonical #1327 closure — every advertiser component's policy
  paths **as enumerated by the canonical descriptor** (the descriptor
  is authoritative; its current contents are pinned in #1761) — joins
  the effective lock set.

  The closure contributes entries only for components
  **enabled** in the selected recipe: a descriptor entry for an absent
  component (`gpu-operator-ocp` on a non-OCP recipe) or a
  declared-but-disabled one locks nothing — re-enabling a
  recipe-disabled component is already rejected by the bundler's
  pre-existing enabled-toggle rule (`--set <c>:enabled=true` on a
  recipe-disabled component fails `ErrCodeInvalidRequest` in
  `pkg/bundler`; this ADR's presence semantics cover only components
  with an owned `enabled` path, so that pre-existing rejection is
  load-bearing for this carve-out), and a disabled component can never
  appear in a `bundlers` subset.

  Without the closure, static `--set
  dradriver:resources.gpus.enabled=true` (today: warn only) would pass
  the recipe-time external-advertiser gate against DRA=false and then
  render a bundle with exactly the external+DRA dual advertisement the
  gates reject.

  The trigger condition is itself load-bearing, not an
  optimization: **locks follow ownership**. A profile that does not own
  advertisement (the AKS driver/toolkit values) leaves allocation-policy
  keys on today's WARN semantics — closing them anyway would reject
  overrides on paths unrelated to what that profile declares,
  contradicting "overrides on unrelated paths retain today's semantics"
  (Consequences) and pre-empting the separately-deferred WARN→REJECT
  graduation. The synthetic per-component `enabled` presence path never
  triggers the closure (referencing `gpu-operator` is not policy
  ownership; the descriptor distinguishes selector paths from the
  presence path), and a future profile that changes advertisement solely
  by enabling/disabling an advertiser component must own an explicit
  selector path or amend this rule.
- **Tuple coherence is validated at the hydration boundary, not only at
  resolution.** Disk-loaded and POSTed recipes bypass resolution, and
  bundle-only callers never reach the validation-time resolver — so the
  shared gate gains a **context-aware, hydrating form**: it hydrates
  the effective values of every component contributing to the
  effective lock set (the conflicting toggle typically lives in a
  component values file, not the artifact) and runs the shared policy
  evaluator, **rejecting** (`ErrCodeInvalidRequest`) an incoherent
  artifact:

  - an unknown `advertiser` string;
  - `advertiser: external` with `devicePlugin.enabled=true` or DRA
    `gpus.enabled=true`;
  - a recipe-side `blocked` observation at any locked path (see
    Override locking).

  Hydration and tuple evaluation are **gated on the artifact carrying
  `selectedProfile`** — skipped entirely otherwise, adding no I/O and
  no new failure modes to legacy paths. All four raw-artifact
  boundaries (file load, adoption, direct bundler, mirror) invoke the
  hydrating form; the existing non-hydrating gate keeps the shape +
  version/profile checks. Method-level wiring is in #1761. There is no
  separate closure-completeness check — the closure is recomputed per
  the Recording rule, so it cannot be stale or incomplete.
- **The policy path map and tuple evaluator get one dependency-neutral
  owner**: one canonical descriptor **and one shared policy evaluator**
  (advertiser vocabulary + tuple-coherence rules), consumed at every
  boundary — so generated, disk-loaded, POSTed, and direct-bundler
  recipes all fail on an incoherent tuple before any output is
  written. A copied map or a second evaluator would let a future
  policy key or advertiser value silently reopen the bypass this
  amendment closes (package placement, the duplicated-vocabulary
  inventory, and the consumer list in #1761).

  The descriptor is **append-only while any
  supported artifact may reference an entry**: because the closure is
  recomputed rather than persisted, removing or renaming a selector path
  would silently unlock it on older authentic recipes — which pin chart
  versions that still honor the old path — the moment a newer binary
  recomputes the closure. Removing an entry requires a deprecated
  tombstone retained for the support window, or ending support for the
  affected artifacts — an apiVersion bump alone does not permit
  removal, because ADR-011 transition windows keep the prior version
  accepted, so the older artifact still loads against the shrunken
  descriptor.

### Artifact compatibility gate

`selectedProfile` cannot ship as an additive field: recipe
deserialization is non-strict (`pkg/serializer/reader.go` ignores unknown
fields unless strict mode is explicitly enabled) and the loader gates
only on `apiVersion`/`kind` — so a **released binary would load a
profile-bearing artifact, silently drop the profile, and permit every
override the profile forbids**. Profiles therefore ride a **new recipe
artifact apiVersion**, with the following contract:

- A profile-declaring `RecipeMetadata` overlay and a profile-bearing
  `RecipeResult` are stamped with the new recipe artifact version:
  **`aicr.run/v1alpha3`**. Snapshots and configs remain on
  `aicr.run/v1alpha2`, as does `RecipeCriteria` (below). **Every
  full-artifact byte-decoding boundary decodes a v1alpha3
  `RecipeResult` strictly — files and `/v2/bundle` POST bodies
  alike**: unknown fields, duplicate
  or trailing JSON documents, and a malformed `selectedProfile` subtree
  fail closed ("machine-generated" is not a trust boundary on a public
  HTTP endpoint). Decode strictness cannot
  protect typed Go callers, so the shared gate additionally validates
  the `selectedProfile` subtree structurally. Every recipe-byte
  consumer first gates on kind and apiVersion; lightweight projections
  of v1alpha3 recipe bytes project only after that shared strict
  full-artifact decode and must not silently discard profile identity.
- New binaries accept the legacy version for non-profile recipes and the
  new version for profiled recipes. Snapshots and configs remain on their
  current version.
- `RecipeCriteria` stays on the legacy version — the version-constant
  de-aliasing this requires is pinned in #1761 — and **profile
  selection rides the v2 request envelope, never the criteria
  document**.
- Cross-checks fail closed in both directions and for both kinds: a
  legacy-version result carrying `selectedProfile`, or a new-version
  result without one, is rejected; symmetrically, new-version
  `RecipeMetadata` without a `spec.profile` declaration is rejected at
  catalog load (see Declaration — strict decoding).
  An empty apiVersion is treated as legacy for this check (older
  artifacts may omit it), so an empty-version result carrying
  `selectedProfile` is likewise rejected. Any apiVersion that is
  neither the legacy nor the new recipe version is **rejected
  outright** — an unknown version never degrades to legacy handling.
  (Enforced at the `RecipeResult` raw-artifact boundaries. At catalog
  load the version/declaration cross-check is bidirectional — a
  declaring overlay not stamped with the new version and a new-version
  overlay without a declaration are both rejected; only a
  non-declaring overlay with a legacy, empty, or unknown apiVersion
  keeps today's kind-only loading, pending the Deferred Decision 4
  follow-up.)
- The 1:1 coupling of the new version to profile presence is deliberate
  for this bump: a future recipe schema change unrelated to profiles
  takes its own version rather than reusing this one.
- Released file loaders already reject the new version through the
  existing apiVersion gate — no retrofit needed there; the REST, SDK,
  and catalog boundaries cannot be retrofitted and are addressed
  individually below.
- This **explicitly amends ADR-011 in one respect: kind-scoped version
  evolution.** `pkg/header` today defines one `GroupVersion` shared by
  recipes, snapshots, and configs, and the current code accepts exactly
  one version with no transition window (`IsSupportedAPIVersion`).
  ADR-011 itself already anticipates dual-accept transition windows for
  future bumps; what it does not contemplate is one artifact kind
  evolving independently — recipes accepting two versions while
  snapshots and configs stay pinned. That kind-scoped divergence is the
  amendment; ADR-011 receives a reciprocal "Amended by ADR-015" banner
  when this ADR is accepted.

One path the result gate does **not** protect: the metadata-store
catalog loader checks only `kind`, never `apiVersion`, so a released
binary pointed at a newer catalog would silently resolve an
unspecialized recipe. **Profile-declaring catalogs are declared
incompatible with older binaries — including a binary rolled back
after the catalog was updated**; a general catalog compatibility
contract is explicit follow-up work (Deferred Decision 4,
[#1812](https://github.com/NVIDIA/aicr/issues/1812)).

Released servers fail open in both directions — released `/v1/bundle`
adopts a POSTed result with no apiVersion or kind check, and released
`/v1/recipe` silently drops an unknown `profile` parameter
(handler-level detail in #1761). Contract: profile-aware operations
are served only on **`/v2/recipe`, `/v2/query`, and `/v2/bundle`** —
released servers return 404 for them (fail closed).
Legacy `/v1` endpoints remain for non-profile use, and new servers' `/v1`
handlers reject profile-bearing **results, not merely profile-bearing
input**: with `default` mandatory, an ordinary criteria request against a
profile-declaring composition resolves to a profiled result with no
`profile` parameter in sight, so `/v1/recipe` must reject based on the
*resolved composition*, and `/v1/bundle` based on the POSTed artifact's
version and `selectedProfile`. The same resolved-composition rule applies
to `/v1/query`, which resolves a recipe through the same client path
(GET and POST).

New servers reject rather than serve these `/v1` requests — fail
closed on every instance that understands profiles. A mixed old/new
fleet is **inherently nondeterministic** for such requests (a released
server ignores the declaration and serves the unprofiled result; a new
server rejects), so a family conversion carries a **rollout
requirement**: homogeneous server and catalog versions behind an
endpoint, an atomic or blue-green traffic switch, or version-segregated
routing — old or unconverted instances must not share the endpoint
during cut-over.

On the `/v2` **resolving endpoints** (`/v2/recipe`,
`/v2/query`), selection is carried in **both**
GET parameters and POST request bodies — via a v2 request *envelope*
that keeps the profile outside `RecipeCriteria`, so it never becomes a
criteria dimension. The envelope is minimal and normative — the
`profile` field is a single `name=value` string mirroring the CLI flag
(one declaration per composition makes a richer object unnecessary):

```yaml
# POST /v2/recipe; /v2/query adds its query fields alongside
criteria: {service: gke, os: cos, accelerator: h100, intent: inference}
profile: gpuStack=csp-managed   # optional
```

GET carries the same string in the `profile` parameter.
`/v2` requests are **strict**: an unknown query parameter or envelope
field is rejected (`ErrCodeInvalidRequest`) — a typo (`?profie=…`)
must not silently select the default. `/v1` keeps its lenient parsing
for unknown inputs, with one reserved exception: new servers **reject
explicit profile input on `/v1`** — a `profile` GET parameter or
top-level POST field on `/v1/recipe` and `/v1/query`, regardless of
the resolved composition (`ErrCodeInvalidRequest`) — rather than
silently dropping it, so selection intent fails closed on every
surface (see Optionality and naming). Released servers still drop it
silently — the same released-server fail-open surface documented
above, closed only by moving to `/v2`.
When more than one surface supplies a selection,
precedence is **explicit-over-ambient** (a CLI flag overrides a
config-file selection); GET parameter and POST envelope are *equally*
request-explicit, so neither outranks the other — a disagreeing pair
is ambiguity and is rejected, while an agreeing duplicate is accepted.
The endpoint contract:

| Endpoint | Accepted input |
|---|---|
| `/v1/recipe` | Criteria without explicit profile input, whose resolved composition declares no profile |
| `/v2/recipe` | Profile and non-profile criteria (GET or POST); default or explicit selection |
| `/v1/query` | Queries without explicit profile input, whose resolved composition declares no profile |
| `/v2/query` | Profile and non-profile queries (GET or POST); default or explicit selection |
| `/v1/bundle` | Legacy-version, non-profile recipes only |
| `/v2/bundle` | Legacy and profile-bearing recipe versions |

The CLI config-file boundary needs no gate work: `AICRConfig`
decoding is strict, so released binaries already reject an unknown
profile field rather than silently ignoring it.

Released raw-`RecipeResult` consumers (`AdoptRecipe`,
`DefaultBundler.Make`, a released `mirror.Lister`, a released server's
`/v1/bundle`) perform no apiVersion check, so passing newer artifacts
to them is **unsupported — a documented limitation, not a gate**.
Going forward, the apiVersion/`selectedProfile` cross-check lives at
the **single choke point three of the four raw-artifact boundaries
already traverse**; the mirror path is newly wired to it (see Override
locking) — one gate, not four implementations that can drift (wiring
in #1761).

## Alternatives Considered

### A. Bundle-time `--set` / install-time `--dynamic` overrides (status quo)

Document per-service defaults and the `--set` (or `--dynamic`)
incantations for the other configuration.

- **Pros:** zero schema change; mechanism already exists; maximally flexible.
- **Cons (disqualifying):** overrides are applied after the recipe exists
  and are recorded nowhere validation looks; validation silently checks the
  stock constraints against a differently-configured cluster. Values are
  free-form, so unqualified states are expressible. Evidence/attestations
  describe a recipe that is not what was deployed. Today's #1327 policy
  enforcement only warns on static overrides of allocation-policy keys.

`--dynamic` (dynamic install-time values) shares every disqualifier,
just later in the pipeline — the hydrated recipe still records the
stock configuration. The motivating cases then defeat it in two
different ways: the GKE values need **opposite label constraints**,
which no value mechanism can express (values cannot swap a constraint
set); and AKS "Driver only" needs three values flipped **together** —
expressible by free-form values, but not enumerable or atomic, so
exactly the unqualified hybrids the AKS guide warns against remain
expressible too. This is a correctness gap, not a UX gap.

The chosen mechanism nevertheless **builds on `--dynamic`'s semantics**
rather than replacing it: `--dynamic` remains the tool for
validation-neutral install-time paths, and the lock's mutability
condition binds to it directly — a profile-owned path can never be
exported `--dynamic`, which is also what lets the argocd-helm guard
reject structural presence without comparing values (see Override
locking).

### B. New criteria axis (e.g. `driver_type` / `gpu_stack`)

Add a multi-valued dimension to recipe selection; each value maps to its
own overlay.

- **Pros:** selection-time visibility; each configuration is a first-class
  recipe; clean multi-valued modeling.
- **Cons:** large blast radius per axis — criteria structs
  (`pkg/recipe/criteria.go`), the OpenAPI contract, CLI flags, catalog
  matching, issue templates, and every doc page that enumerates criteria
  values must change; one-recipe-per-combo multiplies overlay files within
  participating families for every value (unrelated services can omit the
  axis — an omitted recipe field wildcard-matches); criteria matching has
  no *declared*-default semantics — an axis-less ancestor overlay can
  stay the implicit default (an omitted query matches it; a specific
  query wildcard-matches it unless a more specific descendant wins), but
  nothing names which value that default is, and an omitted request
  records `any` while an explicit default query records the concrete
  value — the same configuration yields artifacts with different
  recorded criteria, and the default is never enumerable or fail-closed;
  each future ownership question compounds the matrix — duplicating what
  the overlay/mixin refactor (ADR-005) removed.

### C. Generic per-overlay parameters

Multiple independently-selectable enumerated params per overlay.

- **Pros:** more expressive than a single profile.
- **Cons (disqualifying):** two 2-value params already yield four
  configurations inside one recipe — the criteria-matrix problem recreated
  at a less visible layer. Overlapping effects need conflict and precedence
  rules; the qualified unit becomes the full parameter vector, not the
  value. A single coherent profile enum serves every in-scope motivating
  case; if a genuine second independent axis appears, that is an explicit
  amendment to this ADR with combination rules, not an implicit capability.

### Comparison

| | A. `--set` / `--dynamic` | B. criteria axis | C. generic params | **Profiles (chosen)** |
|---|---|---|---|---|
| Validated against deployed config | **No** | Selection only — overrides still bypass | Selection only — overrides still bypass | **Yes (override locking)** |
| Recorded in recipe artifact | No | Yes (implicit) | Yes | **Yes (explicit + ownership record)** |
| Enumerated / fail-closed | No | Yes | Per param | **Yes (whole config)** |
| Qualified unit | — | Recipe | Param vector (combinatorial) | **Profile value** |
| Recipe matrix growth | None | Multiplicative | Hidden (in-recipe) | **None** |
| Global schema blast radius | None | High (per axis) | Medium | **Low (one-time)** |
| Per-service opt-in | N/A | Opt-in (no default semantics) | Natural | **Natural** |

In short — B and profiles both add a selection surface (`--profile` is
still a new flag); the difference is where the dimension lives and
what each *new* ownership question costs. Each new criteria axis
changes the global schema surface (criteria structs, the OpenAPI
contract, CLI flags, catalog matching), and each new *value* still
touches the global enum surface (OpenAPI enums, docs, issue templates)
and multiplies overlays within participating families; unrelated
services simply omit the axis. The default stays implicit — an
axis-less ancestor serves it, but nothing declares which value it is,
and the same configuration is recorded as `any` or as the concrete
value depending on how it was requested. A profile value is an
overlay-scoped identifier resolved *after* matching, with a mandatory
declared default that is always recorded concretely, so each
subsequent question is a per-overlay edit. Profiles pay a larger one-time mechanism cost
(locking, ownership record, apiVersion gate) and win under
recurrence — the shape the Problem section expects.

| | B. criteria axis | Profiles |
|---|---|---|
| First ownership question | new axis: schema + matching + docs; overlays multiply | one-time mechanism |
| Each subsequent question | new value: global enum surface + overlays; new axis: full repeat | one overlay edit |
| Default semantics | implicit, unnamed — recorded as `any` or the concrete value | mandatory declared default, always recorded |
| Unrelated services | omit the axis (wildcard) | untouched |

## Consequences

- **Landing the mechanism is non-breaking by construction.** A composition
  without a profile declaration produces today's artifact byte-for-byte —
  no `selectedProfile`, no ownership record, no locks, the legacy artifact
  apiVersion, and an unchanged digest — so existing overlays, generated
  recipes, committed evidence, `/v1` endpoints, and released binaries are
  unaffected when the mechanism ships.

  Six behavior changes land with the mechanism itself, all
  fail-closed tightenings on inputs that are invalid under this ADR:

  - `mirror.Lister.Discover` gains the shared context-aware validation
    gate (see Override locking) — no rejection for well-formed recipes;
    the gate validates a defensive copy, so `Discover` output and the
    caller's artifact are unchanged;
  - the raw-artifact boundaries reject artifacts carrying an **unknown
    apiVersion** that today pass unchecked;
  - a legacy- or empty-version artifact carrying `selectedProfile` is
    newly rejected — today's non-strict decode silently drops the
    field;
  - a legacy- or empty-version overlay carrying a `spec.profile`
    declaration is newly rejected at catalog load — today the
    kind-only, non-strict catalog loader silently yields an unprofiled
    catalog (see the compatibility gate);
  - a new-version overlay without a `spec.profile` declaration is
    newly rejected at catalog load — the same cross-check's other
    direction (today's kind-only loader accepts it unchecked);
  - a `/v1` request carrying explicit profile input is newly rejected
    (today the parameter is silently ignored — see the compatibility
    gate).

  Well-formed legacy artifacts are unaffected. Every breaking effect
  on well-formed inputs — new apiVersion, digest changes, the
  family-wide `/v1` rejection of ordinary criteria requests, evidence
  re-signing — is deferred to a family's explicit conversion (below),
  never triggered by the mechanism landing.
- One-time feature cost: declaration/resolution in `pkg/recipe`, the
  ownership record and new artifact apiVersion, selection plumbing and
  the `/v2` endpoints, override locking across bundle/mirror/argocd-helm,
  and docs. After this lands, new configurations are per-overlay edits.
  The full implementation inventory and acceptance criteria live in
  [#1761](https://github.com/NVIDIA/aicr/issues/1761).
- Validation needs the constraint forms the fragments reference: the
  GKE consumer depends on the node-set label check from
  [#1755](https://github.com/NVIDIA/aicr/issues/1755) — a new
  reading/evaluator capability, scope confirmation is Deferred
  Decision 2 — **and** on the #1327 amendment above; part of the
  mechanism cost, not an optional follow-up. The AKS
  consumer's authoritative node-pool-mode signal must be identified and
  documented first; whether it needs new collector or evaluator machinery
  depends on that signal (see Declaration).
- Every declared profile value is a supported configuration and must be
  qualified (KWOK lanes / UAT coverage where feasible); values we cannot
  test — or cannot validate against the cluster — do not get declared
  (enforced in review today; a catalog-load lint is possible follow-up).
- **Converting an existing family to a profile declaration is a
  qualification event, not a refactor.** Byte-identity holds only for
  overlays without a declaration: the moment a family (e.g. gke-cos)
  declares a profile, every generated recipe gains `selectedProfile`,
  `ownedPaths`, and the new apiVersion — digests change, committed
  evidence pointers (`recipes/evidence/<recipe>/...`) stop matching
  freshly generated artifacts, and released binaries reject the new
  artifacts. Each conversion therefore ships with regenerated,
  re-signed evidence for **each declared value** — every declared value
  is a supported, separately-signed configuration (see Recording), and
  a value that cannot be qualified is not declared — and a documented
  cut-over; older binaries keep working against previously published
  artifacts but cannot consume newly generated ones.

  Conversion also carries four further effects:

  - **Supersession review (step 4)**: the fragment authoritatively
    supersedes earlier assignments of its owned paths, so the
    converting author must find descendant or co-matched
    specializations of now-owned paths and fold each into the
    declaration or drop it — external `--data` overlays inheriting the
    converted base need the same review by their owners. The same
    review covers constraint names a value reuses (the mixin collision
    rule rejects those loudly at resolution).
  - **Declaration survival (step 2)**: a snapshot that excludes every
    declaring chain now fails generation instead of silently emitting
    the base configuration.
  - **Narrowed `bundlers` subsets**: subsets omitting any component
    contributing to the effective lock set are rejected on profiled
    recipes (see Override locking); restoring them requires an
    externally-satisfied validation contract — explicit follow-up
    work.
  - **Client migration**: new servers reject the family on `/v1` (see
    the compatibility gate), so **every `/v1` REST consumer of the
    family must move to `/v2` at cut-over**, and the cut-over
    documentation must say so.
- **Reversion is conversion's mirror image.** Removing a declaration is
  itself a qualification and client-migration event: newly generated
  artifacts return to the legacy apiVersion, digests change again,
  `/v1` resumes serving the family, and previously published profiled
  artifacts stay valid for binaries that accept them. Reverting is not
  guaranteed to restore pre-conversion digests — an exact byte-level
  restoration does, and the prior evidence then attests identical
  bytes; otherwise evidence is regenerated.
- Relation to the #1327 allocation-policy override policy: the current
  policy keys are GPU *advertisement* keys (`devicePlugin.enabled`, DRA
  `resources.gpus.enabled` / `gpuResourcesEnabledOverride`, component
  `enabled`) — not
  `driver.enabled`/`toolkit.enabled`/`operator.runtimeClass`. Profile
  locking therefore changes AKS driver paths from allowed/silent to
  rejected; it does not interact with the allocation-policy WARN. Where a
  profile triggers the policy closure, all #1327 policy paths join the
  lock (see the amendment). Graduating the
  allocation-policy static-override WARN to REJECT globally remains a
  **separate decision** outside this ADR.
- Rule of thumb, to keep profiles from sprawling: a profile value encodes
  *ownership of a stack layer on an existing recipe*; new workload/product
  surfaces remain criteria. Value names must name the precise ownership
  split (`driver-only`, `csp-managed` for GKE's device-plugin handoff) —
  a name that overstates the delegation (calling AKS "Driver only" mode
  `csp-managed`) misdescribes what is qualified.

## Adoption plan

1. Land the profile mechanism (declaration, composition-wide uniqueness,
   resolution ordering, ownership-record + apiVersion stamping, override
   locking) + docs.
2. First consumer: GKE `gpuStack: [operator (default), csp-managed,
   operator-selfdriver]` — device-plugin and driver-provisioning
   ownership, gated on the #1755 label check and the #1327
   external-advertiser amendment. The `operator-selfdriver` value
   additionally requires the `gcp-driver-installer` component
   (values-gated chart, new public registry entry) and is declared only
   once its durable ownership-mode distinguishing signal is identified
   (Deferred Decision 5) — the other two values do not wait on it. The
   dormant component and the third value land **together**, in one
   event: declaring the value later is an ownership-surface expansion
   (`install` joins the union and the installer's synthetic `enabled`
   joins `ownedPaths`), which is a family-wide
   re-qualification and evidence re-signing event by the
   Recording rules — budget for it when DD5 resolves. Any dcgm-exporter GPU-ID-mapping
   adjustment for `csp-managed` is an external GKE behavior not
   verifiable from this repository — it is verified and added during
   this step if required, with the upstream citations (NVIDIA GPU
   Operator GKE guidance; GKE device-plugin documentation) recorded in
   that step's PR. GKE goes first because it exercises the full
   mechanism end-to-end: effects, opposite constraints, policy amendment,
   locking, and evidence. Because conversion rejects the family's
   ordinary criteria requests on `/v1` (see the compatibility gate),
   this step also checks the family's `/v1` usage — or announces a
   deprecation window — **before** converting, so clients do not
   discover the rejection at cut-over.
3. Second consumer: AKS `gpuStack: [operator (default), driver-only]` —
   driver/toolkit ownership on unmanaged pools, replacing the documented
   `--set` guidance **including the air-gap mirror guidance**
   (`docs/user/air-gap-mirror.md` recommends
   `--set gpuoperator:driver.enabled=false`, which migrates to profile
   selection); gated on identifying and documenting the authoritative
   node-pool-mode validation signal. Fully AKS-managed pools remain out
   of scope.
4. Internal recipes (DGXC/NKX): once `operator-selfdriver` is declared,
   the internal cos-gpu-installer arrangement (internal MR #27)
   migrates to the public value — the values-gated
   `gcp-driver-installer` component makes the case expressible without
   an internal-only component or a component-addition amendment.

## Deferred Adoption Decisions

None of the following changes the mechanism above, and none gates this
ADR's acceptance. Each lists the options and a proposed default, and is
routed to the implementation issue
([#1761](https://github.com/NVIDIA/aicr/issues/1761)) or the consumer
work that resolves it.

1. **Diagnostics when a profile constraint cannot be evaluated** (the
   snapshot lacks the reading form): same "constraint violated"
   diagnostic, or a distinguishable "reading unavailable — regenerate
   the snapshot"? Both fail closed; only the second is actionable.
   **Proposed: distinguish.**
2. **#1755 scope confirmation.** This ADR reads #1755 as delivering the
   node-set constraint *form* (every GPU node has label X, including
   the negated form) — a new reading/evaluator capability. **Proposed:
   confirm on the issue before implementation; the GKE consumer is
   gated on it.**
3. **AKS node-pool-mode signal** — an AKS-set node label, the
   `gpuProfile` surface, or a new collector reading? Gates declaring
   the `driver-only` value at all. Like DD5, the signal must be a
   **durable ownership-mode marker** — profile constraints are
   re-evaluated post-deployment. When identified, it lands as
   symmetric constraints on both values — `operator` asserts the
   opposite of `driver-only` — not on `driver-only` alone. **Proposed:
   resolve during adoption step 3; nothing else waits on it.**
4. **Catalog compatibility follow-up.** A general catalog compatibility
   contract (the `--data` path has no apiVersion gate) is deferred.
   **Filed as
   [#1812](https://github.com/NVIDIA/aicr/issues/1812); it can land
   alongside #1761's first stage.**
5. **`operator-selfdriver` distinguishing signal.** The value's
   declared constraints are identical to `operator`'s, and its "driver
   not preinstalled" pre-condition is destroyed by the installer
   running — so the signal must be a **durable post-deployment
   property** (a node label or image marker, not a pre-condition) for
   `aicr validate` to distinguish the two values on a running cluster.
   It lands symmetrically, with `operator` asserting the marker's
   absence, so the two values stay mutually distinguishable.
   **Proposed: identify a durable signal during the value's adoption;
   the `operator` and `csp-managed` values do not wait on it.**
