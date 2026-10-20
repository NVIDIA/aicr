# ADR-018: Class-Partitioned Bundles

## Status

**Proposed** — 2026-08-11; classification revised 2026-10-20 (three
classes: `core`, `ops`, `workload`).

Builds on the resolved-recipe invariants from
[ADR-015](015-recipe-configuration-profiles.md) (profile locks, strict
boundary) and the component subset filter from
[#1531](https://github.com/NVIDIA/aicr/issues/1531). It does not change the
recipe resolution algorithm.

## Problem

AICR today emits exactly one bundle per resolved recipe. Every component —
GPU runtime, networking, scheduling, and observability — deploys as a single
unit with a single deployment order.

Operators increasingly want to split that unit. The runtime core (components
whose absence prevents GPU workloads from being scheduled or run), the
operational services (scheduling, observability, administration), and the
workload platforms (training, inference, and batch frameworks) have
different owners, different change-approval processes, and different
release cadences. The need for co-existing bundles has surfaced in several
contexts; the first concrete consumer needs a runtime bundle and an ops
bundle that co-exist in the same cluster and are deployed and operated by
different owners.

The naive approach — author two recipes, or resolve the same recipe twice
with different filters — reintroduces the exact failure the split must
prevent: the two bundles can carry the *same component with different
arguments*, and the collision surfaces only at deployment time, in the
cluster, as a Helm release conflict or a silently diverged configuration.

Two gaps block a safe split today:

- Components have no classification. `recipes/registry.yaml` entries carry
  identity, sourcing, scheduling, and validation fields, but nothing that
  says which bundle a component belongs to.
- The only subsetting primitive, the `bundlers` name filter (#1531), is an
  ad hoc list on the SDK/REST surface. It has no CLI flag, no notion of a
  named group, and no check that a set of subsets forms a complete,
  non-overlapping partition of the recipe.

## Goals

- Classify every registry component into a named class, with `core`,
  `ops`, and `workload` as the initial classes.
- Generate one atomic **bundle set** — one separately deployable
  sub-bundle (rendered as a class directory) per class, subject to
  cross-class ordering prerequisites — from a **single resolved recipe**,
  via a new `aicr bundle --split` flag.
- Make same-component-different-arguments collisions impossible by
  construction, not detected after the fact.
- Preserve deployment-order correctness across sub-bundles: the per-class
  order is the induced subsequence of the recipe's global topological order.
- Make the bundle set auditable: it records the resolved recipe it came
  from and the class of every component.
- Keep ordinary generation unchanged: no `--split` still produces one
  ordinary bundle, byte-identical relative to the post-prerequisite
  catalog baseline (see Class dependency direction).

## Non-Goals

- Deploying more than one instance of the same component (for example two
  Prometheus stacks). Component identity remains unique across the bundle
  set; multi-instance support is future work.
- Standalone per-class bundle artifacts that are independently verified,
  attested, or published. In v1 the bundle set is the provenance and
  verification unit; class directories can be invoked separately after
  predecessor-class requirements are satisfied, but are not independently
  *published*. A standalone class artifact with an externally satisfied
  profile-lock contract is follow-up work.
- Cross-version compatibility between sub-bundles. AICR emits the set and
  its ordering contract; verifying that an ops bundle from one generation
  is compatible with a core bundle deployed from another is
  operator-managed, and a deploy-time compatibility protocol is explicit
  follow-up work.
- In-place migration of installed components between bundles. When a
  class change moves a component on a cluster with a deployed split, the
  supported path is **redeployment** from the newly generated set, not a
  live handoff of the release from one owner to the other. Redeploying
  core components can disrupt running workloads, so the operator
  schedules it. `split.yaml` records the class map so the change stays
  auditable. Keeping a deployed split across an AICR upgrade that
  reclassifies a component — `aicr recipe --inherit-from` reading the
  prior set's class map, as it already keeps namespaces — is follow-up
  work, gated as described under the registry class rules.
- OCI output for split generation. Multi-artifact naming and
  partial-publish recovery are undefined; `--split` with OCI output is
  rejected in v1.
- Split generation for OCP recipes. OCP recipes replace the base
  monitoring stack with `-ocp` variants and disable the base components,
  and OCP split has not been designed or qualified. `--split` rejects OCP
  recipes explicitly (see Partition validation).
- Reassigning a component's class per recipe shape in v1. Class is
  registry-only and stable; per-shape reassignment is possible follow-up
  work (see Configurable classes).
- Changing recipe resolution, overlay matching, or mixin composition. The
  class is read at bundle time only.
- A generic dependency solver or per-class release tooling.
- A registry-level mutual-exclusion (`conflictsWith`) declaration. Today's
  scattered checks (for example gpu-operator vs gpu-operator-ocp,
  [#1685](https://github.com/NVIDIA/aicr/issues/1685))
  would benefit from one, but that is a separate follow-up.
- More than three shipped classes. The mechanism is N-ary by design, but
  this ADR ships `core`, `ops`, and `workload` only.

## Decision

### Resolve once, partition atomically

The load-bearing invariant: a recipe is resolved exactly once — one criteria
match, one overlay/mixin merge, one `RecipeResult` — and the split is a
**partition of the resolved componentRefs**, applied at bundle time. Each
enabled component belongs to exactly one class, and one sub-bundle
therefore owns **everything emitted for that component** — its single
instance, its RBAC objects, and all of its Helm releases (including
synthetic `-pre`/`-post`/`-readiness` wrapper releases) in the
component's target namespace — with the one merged value set the
resolution produced. Target namespaces are shared surfaces, not owned:
the initial taxonomy itself puts core's `prometheus-operator-crds` and
the ops Prometheus components in `monitoring`.

This is why collision detection is not needed for the case that motivated
the design: with a single resolution there is no representation in which two
bundles hold the same component with different arguments. If two classes
both need a component, one class owns it and the other depends on it across
the bundle boundary.

The partition is **atomic**: `--split` always generates the complete bundle
set, never a single class in isolation. Partial generation would let two
invocations — at different catalog versions, or before and after a
classification change — disagree about ownership of a component, recreating
the collision across calls that the single resolution prevents within one.
An operator who wants only the ops components still deploys only the `ops/`
directory of a complete, verified set, after its `core/` prerequisites are
installed.

### Component class in the registry

Each entry in `recipes/registry.yaml` gains an optional `class` field:

```yaml
- name: kube-prometheus-stack
  displayName: Kube Prometheus Stack
  class: ops
  # Additional component fields omitted.
```

- Allowed values are the known class names; the initial set is `core`,
  `ops`, and `workload`, in that order. The class set is a **closed,
  ordered enumeration in Go**, not registry-defined strings: an
  unenumerated class cannot be pre-validated against the catalog, so new
  classes are added only in code, at a release — cheap to extend, without
  giving up pre-validation.
- Omitted means `core`. This default is **availability-safe, not
  classification-safe**: an unclassified component lands in the bundle
  that must always be installed, but a component that belongs in another
  class and omits the field is misclassified into `core`. The embedded
  registry therefore declares `class` on **every** entry, enforced by a
  CI check, so each built-in assignment is a reviewed decision. The
  default remains for external registries, so existing `--data`
  registries keep working unchanged.
- The field is named `class`, not `type`, because `componentRefs[].type`
  already means Helm-vs-Kustomize.
- Class is **registry-only and stable**. Overlays cannot reassign it in
  v1 (see Configurable classes), and it is not recorded in the resolved
  `RecipeResult` — so `recipe.yaml`, checksums, and attestations of
  ordinary bundles are unchanged. Moving a component between classes is a
  registry change; clusters with a deployed split pick it up by
  redeployment (see Non-Goals).
- v1 introduces the classes, so no deployed split predates them. The
  first release that changes a **built-in** component's class ships only
  once `aicr recipe --inherit-from` keeps the class recorded in the prior
  set's `split.yaml` and `aicr upgrade-check` reports the class move as an
  identity change. Until then the embedded classification stays fixed,
  so an AICR upgrade never moves a running component between bundles.
  Reclassification in an operator's own `--data` registry is the
  operator's change and is applied by redeployment.
- The default applies uniformly to **external registry entries** too: an
  external `registry.yaml` entry replaces the embedded entry wholesale, so
  a replacement for a non-`core` (`ops` or `workload`) component that
  omits `class` lands in `core`. A replacement that intends a non-`core`
  class must restate it; registry merging emits a merge-time warning when
  an external entry replaces an embedded non-`core` component without
  declaring `class`.
- External entries carry `class` like embedded ones, so an internal or
  third-party catalog can classify its own components into the existing
  classes **without any public change**. Only a brand-new class *name*
  requires extending the Go enum — a release-time addition, made when a
  concrete consumer needs it, that also defines the new class's place in
  the class ordering (see Configurable classes).

### Class definitions

Each class is defined by what its components are for, with a test that
decides borderline cases:

| Class | Definition | Test |
|---|---|---|
| `core` | **Platform-independent GPU execution:** a plain GPU pod can be scheduled and run correctly, including the accelerator fabric the hardware requires. Includes everything those components need to install and become ready, declared in `dependencyRefs` (including the storage driver for persistent volumes). | Without it, does a plain GPU pod (no workload platform) still run correctly? If not, it is `core`. |
| `ops` | **Operational services:** scheduling, node health, observability, policy, inventory, and certification. An `ops` service may use GPUs and may be a prerequisite of a workload platform. | Not required by a plain GPU pod, and not a workload platform. |
| `workload` | **Workload platforms and their supporting components:** the training, inference, and batch frameworks workloads are built on, and components that exist only to serve them. | Present because of what users run, typically selected through the `platform` criteria axis. |

"Run correctly" includes the fabric: without the accelerator's RDMA or
EFA fabric, multi-node NCCL falls back to TCP at a fraction of its
bandwidth, regardless of which platform runs on top.

One named exception places a component in `core` that the definition
alone would not:

- **`nvsentinel`.** A GPU pod runs without it, but its remediation
  cordons, drains, and reboots GPU nodes, which makes it part of keeping
  the GPU runtime healthy, and it must be tested on GPU clusters.

The rest of the NVSentinel set follows from that exception and the
prerequisite rule in the `core` definition:

- **Its datastore** (`nvsentinel-mongodb`, `psmdb-operator`), which the
  opt-in `nvsentinel-observe`, `nvsentinel-quarantine`, and
  `nvsentinel-remediation` mixins add.
- **The storage driver those volumes need,** such as
  `aws-ebs-csi-driver`. The datastore requests persistent volumes, which
  on EKS are provisioned by the bundled EBS driver unless the EKS-managed
  EBS addon is installed instead. Keeping the driver in `core` keeps
  `core` self-contained when those mixins are composed. The edge is
  declared like any other: the datastore mixins apply to every service, so
  an EKS-only mixin composed with them adds `nvsentinel-mongodb` →
  `aws-ebs-csi-driver`. With the managed addon, the driver stays declared
  with `enabled: false`, and the bundler treats the edge as satisfied
  outside the recipe. Partition validation checks the edge at bundle time.

Representative assignments under these definitions:

- `core`: `gpu-operator`, `gcp-driver-installer`, `nfd`, `cert-manager`,
  `nodewright-operator`, `nodewright-customizations`,
  `prometheus-operator-crds`, `nvidia-dra-driver-gpu`,
  `dra-node-labeler`, `network-operator`, `aws-efa`, the GKE RDMA and
  TCPXO fabric components, `dranet`, `aws-ebs-csi-driver`, and the
  NVSentinel set.
- `ops`: `kai-scheduler`, `kube-prometheus-stack`, `prometheus-adapter`,
  `k8s-ephemeral-storage-metrics`, `node-problem-detector`, `gatekeeper`,
  `k8s-aibom`, and `nvcre`.
- `workload`: `kubeflow-trainer`, `dynamo-platform`, `grove`,
  `k8s-nim-operator`, `agentgateway`, `kueue`, `topograph`, and the Slurm
  set (Slinky, MariaDB, and the ADR-016 accounting components).

**`kai-scheduler` is `ops`.** A plain GPU pod runs on the default
scheduler, and `kai-scheduler` is not a workload platform, so the
definitions place it in `ops`. Two consequences follow, and they are
accepted:

- The operations owner carries a GPU-aware scheduler, which must be
  tested on GPU clusters. `ops` is not limited to GPU-free components.
- `dynamo-platform` depends on `kai-scheduler`, so a Dynamo deployment
  requires `ops`.

Classification cascades along dependency edges: because a component may
depend only on its own or an earlier class, classifying one component can
force its dependents into later classes or its dependencies into earlier
ones. The resolved-graph guard catches a violating assignment
mechanically; placing future boundary components follows the class
definitions above.

`recipes/registry.yaml` is authoritative for every component's class, and
`docs/user/component-catalog.md` documents the full assignment. This ADR
defines the rules and exceptions; it is not the per-component record.

**`class` is not a criteria axis.** Criteria (`service`, `accelerator`,
`intent`, `os`, `platform`) drive overlay matching, and every new axis
multiplies the recipe-shape and test matrix. `class` deliberately does not
participate in matching: it is a label read at bundle time, after
resolution. The recipe count, the overlay files, and the resolved
`RecipeResult` are identical whether or not `--split` is ever used, so the
split adds surface linearly per class (bundle output and partition checks),
not multiplicatively per recipe × class. The blast radius of introducing
the field is correspondingly small. Authoring cost is one `class` line on
each embedded registry entry, which the explicit-class rule requires.
Compatibility is separate: external registries that omit `class` keep
working through the `core` default, and generation without `--split`
remains byte-identical relative to the post-prerequisite catalog
baseline.

### Class dependency direction

Classes are totally ordered: **`core` < `ops` < `workload`**. A component
may depend only on components of its own class or an earlier one: `ops`
may depend on `core`, `workload` may depend on `ops` and `core`, and
`core` depends on nothing outside `core`. Same-class dependencies are
governed by the existing component-level cycle validation. Because the
order is total, rejecting every edge to a later class also makes the
class-level graph acyclic; no separate cycle check is needed. This keeps
the deployment contract simple (install `core`, then `ops`, then
`workload`) and gives partition validation a crisp rule to enforce. `core`
alone is always a complete install.

Direction is the floor, not the goal: classification should also
**minimize cross-class edges** in the first place, because operating
models exist where no ordering between bundles can be enforced at all. In
the DGXC Runtime, for example, the runtime and operations stacks deploy as
separate Argo CD app-of-apps with no enforceable ordering between them.
The class install order therefore binds only the sequential Helm
`deploy.sh` path. In an externally managed, reconciliation-based model,
each class converges independently and cut edges are not enforced — but
convergence is not free: a sync that races a missing prerequisite (such
as CRDs) fails rather than waits, so the operating model must configure a
**failed-sync retry policy** with a retry window long enough for
prerequisites to converge (in Argo CD, `syncPolicy.retry` — automated
sync alone does not reattempt a failed sync of the same revision, and
self-heal only repairs live drift, so it is a complement, not a
substitute). Separate app-of-apps with no enforced ordering is a
supported operating model **given such a retry policy**; note
this describes externally managed deployments — v1's own generated
output rejects the Argo/Flux deployers until their class-derived
identities land (see the support matrix). The v1 bridge for such models:
operators consume the Helm-local bundle set with their own per-class
Application/Kustomization wrappers; AICR-generated Argo/Flux split output
is the deferred follow-up.

Retry also has limits: it covers prerequisites whose absence makes
reconciliation *fail* (such as missing CRDs); **fail-open prerequisites**
— ops resources that apply successfully despite a missing non-CRD
dependency — are not verified in v1, nor is cross-bundle version
compatibility; a live set-identity or preflight protocol remains
deferred.

The existing catalog violates the rule, and fixing it is a prerequisite.
`gpu-operator` (`core`) declares `dependencyRefs` on
`kube-prometheus-stack` (`ops`) in 35 declarations **across overlays**,
including base. The edge exists to guarantee the
ServiceMonitor CRDs exist before its monitors are applied, and
`kube-prometheus-stack` installs no CRDs itself (`crds.enabled: false`);
the CRDs come from `prometheus-operator-crds`. Because `dependencyRefs`
merge additively across the composition chain, repointing only
`recipes/overlays/base.yaml` fixes nothing; every declaration must be
repointed to `prometheus-operator-crds`, which stays in `core`. CRDs are
cluster API surface needed by every class; keeping shared CRD packages in
`core` is the general pattern.

`dynamo-platform` (12 declarations) and `kubeflow-trainer` (6, including
`recipes/mixins/platform-kubeflow.yaml`) declare the same edge. As
`workload` components they may depend on `ops`, but they too need only
the CRDs, so the prerequisite repoints all 53 declarations; it verifies
each edge's actual purpose before repointing it.

Dependency edges are **install-time requirements only**; runtime data
flows need no edge and naturally run in the allowed direction. The
Prometheus split is the canonical case: core components (such as
`nvsentinel`) install monitor CRs, which requires only the
`monitoring.coreos.com` CRDs — `prometheus-operator-crds`, a `core`
component (`nvsentinel` already declares that direct edge; see
[#928](https://github.com/NVIDIA/aicr/issues/928)) —
while the ops Prometheus *scrapes* those monitors at runtime, an
ops-consumes-core flow with no install edge at all. If ops is absent,
core runs fully; its metrics are simply not collected yet. A runtime flow
can also run toward a later class: `dynamo-platform`'s planner reads
metrics from the `ops` Prometheus, which needs no install edge but does
mean planner-driven autoscaling has no metrics source until `ops` is
installed.

The repoint prerequisite changes ordinary bundles **once**, independent of
`--split`: `dependencyRefs` serialize into each affected recipe's emitted
`recipe.yaml`, so the repoint is a one-time `recipe.yaml` and checksum
change for every recipe that carried the old edge. All byte-identity
claims in this ADR are relative to that post-prerequisite catalog
baseline; the class field and split plumbing themselves change nothing in
ordinary output.

To keep the boundary fixed, a standing guard test validates the
**effective dependency graph of every resolved catalog recipe** — not the
raw overlay/mixin YAML — and fails on any edge to a later class, in the
spirit of the existing deployment-order guard tests. Prerequisites are
declared as edges, including the storage a core component's volumes
need, so the direction rule never depends on review.

Two opt-in NVSentinel mixins declare edges from `core` to later classes:
`nvsentinel-preflight` (`nvsentinel` → `kai-scheduler`) and
`nvsentinel-slinky-drainer` (`nvsentinel` → `slinky-drainer` and its
CRDs). They are composed only onto operators' own leaf overlays, so no
catalog recipe carries them and the guard does not see them. Recipes that
compose either mixin cannot use `--split` in v1; partition validation
rejects the edge at bundle time. Pulling a scheduler or Slurm into
`core` to accommodate them would undo the narrow `core` definition.

### Configurable classes

Operators may disagree with the shipped classification. In v1 the
per-component assignment is configurable and everything else is fixed:

- **Assignment is configurable through the registry.** `class` is an
  ordinary registry field, so an external `--data` registry can classify
  its own components and reclassify an embedded one by replacing its
  entry (wholesale, per the external-entry rules above). This is the
  supported way to change the shipped defaults, and it needs no public
  change. A `--data` catalog never runs through AICR's CI, but partition
  validation runs on the resolved recipe at every `--split`, so a custom
  cut gets the same direction, cycle, cohesion, and all-disabled checks
  as the embedded one.
- **Per-invocation cuts are rejected.** A bundle-time flag or
  caller-supplied list that changes class would let two invocations
  against the same catalog place one component in different bundles —
  the cross-call collision this design exists to prevent (see
  Alternatives Considered). Fixing the assignment per catalog keeps
  ownership stable.
- **Overlay-level reassignment is deferred, not rejected.** It does not
  reintroduce the cross-call collision: the class would still be decided
  by the resolved recipe, so a given recipe always yields the same
  partition. Its cost is complexity instead (see Alternatives
  Considered), and no consumer needs it yet.
- **Class names are closed.** A new name requires extending the Go enum.
  User-defined names would also need a user-declared ordering between
  classes, because the only ordering contract today is `core`, then
  `ops`, then `workload`. That is follow-up work once a consumer needs a
  class AICR does not ship.

The dependency graph bounds how far any reassignment can go. Because a
component may depend only on its own or an earlier class, moving a
component to a later class forces every component in an earlier class
that depends on it, directly or transitively, to move too; moving one to
an earlier class likewise pulls in its dependencies. A component at the
edge of the graph, such as `kube-prometheus-stack`, can move without
forcing anything else. Moving `gpu-operator` out of `core` would drag
`nvsentinel` and the rest of the GPU stack with it, leaving a `core` too
small to mean "runtime". Reassignment is practical at narrow points in the
graph — the shipped cuts are such points, the `core`/`ops` cut made
narrow by the `prometheus-operator-crds` repoint — not at its foundation.

That narrowness suggests deriving classes instead of declaring them:
label one component at the cut and let every dependent inherit its class.
This ADR keeps the field explicit. Under derivation, adding a dependency
edge could silently move a component into another bundle and change its
owner; with an explicit field, the same edge fails the direction guard
and the ownership change goes through review. A follow-up can have the
guard's failure name the components that would have to move, which keeps
the convenience of derivation without its implicitness.

### Bundle generation with `--split`

`aicr bundle` gains a boolean `--split` flag:

```shell
aicr bundle --recipe recipe.yaml --split --output ./bundles
```

- No `--split` keeps today's behavior: one bundle, all enabled
  components.
- `--split` takes no class arguments: it renders every class with
  **declared components** in the resolved recipe, one directory per
  class. A known class with no declared components is **skipped** (no
  directory) — distinct from the rejected all-disabled case below. A
  recipe with no workload platform, for example, produces only `core/`
  and `ops/`. The flag is not hardcoded to a fixed set of classes: adding
  a future class changes the output layout of recipes that declare such
  components, and nothing else. A
  class-subset selector is deliberately absent: emitting less
  than the complete partition reintroduces the cross-call ownership
  collisions this design exists to prevent (see the atomicity rationale
  above), so per-class emission stays coupled to the deferred
  standalone-class-artifact follow-up.
- `--split` generates the complete bundle set in one invocation. The
  pipeline is **plan, validate once, render subsets**: the complete
  candidate is loaded, bundle-time overrides are applied, and all
  validation — coherence, component validations, and the ADR-015 profile
  lock — runs once against the full component set, exactly as today.
  Only after validation passes is the partition rendered, one directory
  per class, reusing the #1531 subset filter for rendering only.
- This ordering is required, not incidental: the current pipeline filters
  before `ValidateProfileLock`, so rendering a class through today's
  pipeline would fail whenever another class holds a profile-locked
  component. Validating the union first preserves ADR-015 for profiled
  recipes (all AKS and GKE recipes) without weakening the lock.
- Dependency edges cut by the partition use the existing
  satisfied-externally semantics (the same path used when a declared
  component is disabled).
- Per-class deployment order is the induced subsequence of the recipe's
  global `deploymentOrder`, so relative ordering is consistent across
  sub-bundles by construction.

### Partition validation

Before writing any output, `--split` rejects:

- a component assigned to an unknown class (assignment is total and
  single-valued by construction: one registry field with a default);
- any dependency edge to a later class in the resolved recipe (which also
  excludes class-level cycles), including the two opt-in NVSentinel
  mixins described under Class dependency direction;
- an OCP recipe (`criteria.service: ocp`). OCP split is out of v1 scope
  (see Non-Goals) and is rejected explicitly rather than relying on the
  all-disabled rule below;
- a partition that separates components required to travel together. In
  v1 the only such cohesion set is the AICR-provided Slurm accounting set
  ([ADR-016](016-slurm-accounting-enablement.md)), known directly to
  partition validation; a generic cohesion
  declaration in the registry is deferred;
- a class whose declared components are **all disabled** after recipe
  disables and bundle-time overrides are applied (scalar
  `--set <component>:enabled=false` is the supported bundle-time disable
  mechanism; a class with no declared components at all is skipped, not
  rejected — see above);
- a caller-supplied `bundlers` component filter (#1531, SDK/REST). The
  filter removes components before validation and would break the
  complete-partition invariant or fail profiled recipes; in split mode the
  class partition is the only supported subsetting, and the #1531 filter
  is reused internally for rendering only;
- disabled checksums — the set would be unverifiable before publication
  (see Bundle set layout and provenance); and
- an unsupported deployer or output combination (see below).

The all-disabled rule matters because the existing subset filter treats an
empty component list as "no filter" — naive reuse would render *every*
enabled component into the empty class's directory. It also rejects any
bundle-time override combination that disables a whole class.

OCP needs its own rejection because the all-disabled rule does not cover
it. `recipes/overlays/ocp.yaml` disables the base monitoring components
but enables `prometheus-adapter-ocp`, an `ops` component, so an OCP
recipe has an enabled `ops` class and would pass the all-disabled check.
OCP split support is follow-up work.

### Bundle set layout and provenance

The **bundle set is the v1 provenance and verification unit.** The set root
holds the full `recipe.yaml`, `split.yaml`, `checksums.txt`, and the
attestation when enabled — covering the whole set. Class directories can be
deployed separately after their predecessor-class requirements are satisfied,
but are not independently verified or published artifacts.

The set reuses the existing bundle checksum and attestation contract
unchanged, applied at the set root: the same file inventory and exclusion
rules as today's single bundle (with `split.yaml` and the class
directories as ordinary payload), and the same attestation subject. For
ordinary bundles, disabling checksums keeps its existing semantics; in
**split mode, disabling checksums is rejected** — the bundle set is the
provenance unit and pre-publication verification requires the root
manifest, so a checksum-less split set would be unverifiable by
construction.

Whole-set verification begins at the root `checksums.txt`; when present,
the attestation authenticates that manifest. `split.yaml` is never a
trust root. Verification is a whole-set operation: verify the set, keep
it intact, and run `<class>/deploy.sh` from within it. A class directory
copied out of the set carries **no standalone provenance** — the
exact-tree manifest cannot verify a detached subtree.

**Publication is atomic on disk, not just in semantics, and create-only
in v1.** The `--split` contract:

- An explicit output path is required (`--output`, config, or a
  non-empty SDK `OutputDir`) and the exact destination must be absent;
  its parent may exist. A pre-existing destination is rejected before
  any work — replacing a populated set is deliberately out of v1 scope.
- The entire set — every class directory, checksums, and attestation —
  renders and verifies in private staging on the **same filesystem** as
  the destination (expected shape: a private sibling under the
  destination's retained parent), then publishes with one atomic move to
  the absent destination.
- Concurrent attempts have at most one winner; losers fail without
  altering the published set. A pre-commit failure leaves the
  destination absent, with staging cleaned up; once publication has
  committed, cleanup diagnostics cannot un-publish the set.
- The guarantee holds at the functional API boundary (`pkg/bundler` and
  the `pkg/client/v1` facade), not only in the CLI. The REST client
  contract is unchanged — it already streams a complete ZIP or an error
  — but the handler must pass a non-existent child of its private temp
  directory to satisfy the create-only precondition, so the
  implementation touches `pkg/server` as well.
- Returned result paths reference the final published locations.

This is **net-new work**: the existing closed-world staging machinery
from [#1758](https://github.com/NVIDIA/aicr/issues/1758) is the pattern,
but it runs only in the CLI and only for OCI output (for local output,
`opts.ociRef == nil`, generation writes directly to the destination) —
the mode `--split` rejects. Relocating staged publication into the
functional API for local output is part of this design's scope, not an
extension of existing behavior. Failure-path tests are specified with
the implementation.

`split.yaml` is **audit metadata only**: it records the set's
`recipeDigest` — computed over the emitted set-root `recipe.yaml`, using
the same canonicalization as `evidence/attestation.SubjectDigest` — the
class of every component, and the class ordering contract (core, then
ops, then workload). `recipeDigest` plus the class map identify the **resolved recipe and
its partition**, not the rendered payload: bundle-time inputs (`--set`,
typed overrides, scheduling settings) land in the extracted component
values, not in `recipe.yaml`, so two invocations can share a
`recipeDigest` yet render different class directories. The rendered
payload is identified by the digest of the root `checksums.txt` — which remains the
bundle attestation's in-toto subject, exactly as today; the split changes
neither contract. The `split.yaml` schema, defined with the
implementation, restates this scope note alongside the `recipeDigest`
field.

Because class lives only in the registry, the same `recipeDigest` can
partition differently after a registry classification change. The
per-component class map in `split.yaml` is deliberately the record that
disambiguates, and a set generated before a class change stays
auditable through its recorded map. It is also the record that
class-aware `--inherit-from` (follow-up) reads, so its class map is a
stable, versioned part of the `split.yaml` schema.

`split.yaml` lets an operator answer "which recipe and partition produced
what is deployed here" — it is not a deploy-time enforcement mechanism,
and deployment does not reject on digest comparison. Cross-version
compatibility between a deployed class and a newer set's later class
(for example a deployed core and a newer ops set) remains an
operator-managed concern (see Non-Goals).

### Deployer identities

Two sub-bundles deployed to one cluster must not collide on deployer-level
identities. The rule: **sub-bundle root identities derive from the class** —
for example, the Argo CD parent Applications `<app-name>-core`,
`<app-name>-ops`, and `<app-name>-workload`.

Component-level identities need one guard beyond unique component names:
*synthetic* release names. The Helm writer injects `<name>-pre` /
`<name>-post` wrapper releases and today detects collisions against the
complete component list, so per-class rendering could miss a collision
split across classes (one class's injected wrapper vs another class's
component name — reachable with external component registries). Emitted
identity validation therefore runs against the **full set** before
subsets are rendered, consistent with the validate-once pipeline: Helm
identities are validated as (target namespace, release name) **pairs** —
namespaces may span classes and need not be unique — while generated
folder names and reserved synthetic suffixes stay globally checked. This
validation covers **deployer-level identities only**; collisions between
rendered Kubernetes objects are pre-existing single-bundle behavior,
unchanged by partitioning, and out of scope here.

v1 fails closed outside this support matrix — `--split` with a rejected
combination returns a clear error, never degraded output:

| Deployer / output | v1 `--split` | Class-derived root identity |
|---|---|---|
| `helm` (local) | Supported | Set directory layout (`core/`, `ops/`, `workload/`); full-set emitted-identity validation before rendering |
| `helmfile` (local) | Rejected | Follow-up: per-class root helmfile |
| `argocd` (local) | Rejected | Follow-up: parent Application `<app-name>-<class>` |
| `argocd-helm` (local) | Rejected | Follow-up: same parent-Application rule |
| `flux` (local) | Rejected | Follow-up: per-class root Kustomization |
| Any deployer + OCI output | Rejected | Follow-up: multi-artifact naming + partial-publish recovery undefined |

Enabling a rejected deployer is incremental follow-up work: wire its
class-derived root identity, add it to the matrix, and lift the rejection.

## Example

An EKS H100 training recipe with the Kubeflow platform resolves, as today,
to one `RecipeResult` with components including `cert-manager`, `nfd`,
`nodewright-operator`, `prometheus-operator-crds`, `gpu-operator`,
`aws-efa`, `aws-ebs-csi-driver`, `nvsentinel` (class `core`);
`kai-scheduler`, `kube-prometheus-stack`, `k8s-ephemeral-storage-metrics`
(class `ops`); and `kubeflow-trainer` (class `workload`).

```shell
aicr bundle --recipe recipe.yaml --split --output ./bundles
```

validates the complete recipe once, then renders:

```text
bundles/
├── recipe.yaml                 # the full resolved recipe (set-level)
├── split.yaml                  # audit: recipeDigest + class of every
│                               # component + ordering contract
│                               # (core → ops → workload)
├── checksums.txt               # covers the whole set
├── core/
│   ├── deploy.sh               # cert-manager, nfd, prometheus-operator-crds,
│   │                           # gpu-operator, aws-efa, nvsentinel, ...
│   └── <component folders>
├── ops/
│   ├── deploy.sh               # deploy after core; installs kai-scheduler,
│   │                           # kube-prometheus-stack, ...
│   └── <component folders>
└── workload/
    ├── deploy.sh               # deploy after ops; installs kubeflow-trainer
    └── <component folders>
```

Edges that cross a class boundary point to an earlier class and are
treated as satisfied externally while rendering the later class — for
example, `kube-prometheus-stack` → `prometheus-operator-crds` (`ops` →
`core`) and `kubeflow-trainer` → `gpu-operator` (`workload` → `core`). The complete
edges remain auditable in the set-root `recipe.yaml`. The runtime owner
deploys `core/`, then the operations owner deploys `ops/`, then the
platform owner deploys `workload/`.
The v1 scripts do not query the cluster to preflight cross-class
prerequisites; they assume the operator followed the class ordering
contract. Taking a *newer* set's later class against an older deployed
earlier class is likewise an operator decision that `split.yaml` makes
auditable but v1 does not mechanically verify.

## Open Questions

**Component required-ness under standalone class artifacts.**
[#2181](https://github.com/NVIDIA/aicr/issues/2181) as implemented adds no
required-ness property to `registry.yaml`: it configures NVSentinel from the
existing ADR-015 `gpuStack` profile, which pins that component's presence
through the profile's existing synthetic presence marker. Consistent with
the alternative rejected below, reselection still never adds or removes a
component: NVSentinel is referenced by every value of the declaration. There
is no `requirePresence` field and no registry-level skippability to
coordinate the `class` field against.

This ADR's v1 is unaffected, because it validates the complete resolved
recipe before partitioning and partitions only for rendering. A component
omitted from one class directory is still present in the verified bundle
set — absent from a directory, not absent from the deployment — so
profile-locked presence and the `class` cut do not interact.

The two axes would meet only if a future revision emitted a class artifact
as a **standalone** deployable rather than one member of a co-deployed set.
Such an artifact would have to prove that every profile-required component
it omits is supplied by a compatible sibling bundle — today's whole-union
validation gives that for free, and a standalone artifact would not. That
proof obligation, not a registry property, is the thing to design against
if standalone class artifacts are ever pursued.

## Alternatives Considered

- **Two recipes (or two resolutions) per configuration**, for example split
  leaf overlays or mixin-based `runtime.yaml`/`ops.yaml` pairs. Rejected:
  double resolution is exactly what allows the same component to resolve
  with different arguments in each bundle, and it doubles the overlay
  maintenance surface.
- **Reuse ADR-015 profiles as the grouping axis.** Rejected: profiles are
  deliberately barred from changing component presence ("component presence
  is not affected by reselection"), and a profile expresses one selected
  variant, not a partition into co-existing sets.
- **Caller-defined partitions (bare `bundlers` name lists or arbitrary
  per-invocation cuts), no class field.** Rejected: callers would
  hand-maintain component lists that drift as recipes evolve; nothing
  would check completeness, overlap, or dependency direction; and two
  invocations could cut the DAG differently, producing co-existing
  bundles that claim the same component. A registry-fixed cut also has a
  property no per-invocation cut can: CI validates the entire catalog
  against it (direction, cohesion, and recipe-shape all-disabled
  classes; override-induced cases are enforced at bundle time) before
  any user runs `bundle`. The class field makes the partition a
  reviewed, stable contract maintained once by component authors.
- **Two classes, with `ops` limited to GPU-free components.** Rejected.
  With only `core` and a CPU-only `ops`, every component that touches
  GPUs must be `core`, including schedulers and workload platforms that a
  plain GPU pod does not need. `core` becomes nearly the whole catalog,
  larger and more churn-prone than "runtime core" suggests. Defining
  `core` by necessity and adding `workload` keeps `core` small; dropping
  the CPU-only rule gives GPU-aware operational services a home other
  than `core`.
- **`apps` as the name of the third class.** Rejected in favor of
  `workload`: `apps` suggests the user's own applications and collides
  with Argo CD vocabulary (`<app-name>-apps` parent Applications inside
  app-of-apps).
- **Per-class emission.** Rejected: emitting a single class lets two
  generation calls disagree about which bundle owns a component —
  reintroducing across calls the collision that single resolution
  prevents within one. Atomic complete generation closes that hole.
- **Overlay-level class reassignment.** Deferred, not rejected. A given
  recipe would still always yield the same partition, so the cross-call
  collision does not arise. The reasons to wait are elsewhere:
  - class would become part of resolution — merged through overlays and
    mixins and recorded in `recipe.yaml` to stay auditable — undoing the
    "class is not a criteria axis" simplicity; and
  - no consumer needs it yet: the closest candidate, OCP's monitoring
    stack, already uses separate `-ocp` component variants, which can
    carry their own registry class.

  Per-shape needs, if they materialize, are follow-up work.

## Consequences

- Adding a component to the embedded registry now includes declaring its
  class, enforced by CI. In external registries, omission defaults to
  `core`.
- Class boundaries become a checked contract: the resolved-graph guard
  fails CI on a new dependency to a later class instead of letting it
  break split deployments.
- All existing `kube-prometheus-stack` dependency declarations from
  `core` and `workload` components (see Class dependency direction for
  the per-component counts) are repointed to `prometheus-operator-crds`
  before the split can ship — a mechanical but catalog-wide prerequisite
  PR that changes affected recipes' emitted `recipe.yaml` and checksums
  once.
- A split set installs in class order: `core`, then `ops`, then
  `workload`, for whichever classes are present. Workload platforms
  install after `ops`, and some require it; for example, Dynamo depends
  on `kai-scheduler`. Others, such as Kubeflow Trainer, depend only on
  `core`. `core` alone remains a complete install.
- Classes separate ownership, not runtime coupling: a component may still
  rely on another class at runtime, as Dynamo does on `kai-scheduler`.
  Compatibility is verified only within one generated set, so owners
  coordinate version changes through that set rather than upgrading
  their class independently.
- Single-bundle users see no artifact or output change; `--split` is
  strictly opt-in, and non-split output stays byte-identical from the
  post-prerequisite baseline onward. (One new diagnostic applies to all
  users: the merge-time warning for external registry entries that
  replace an embedded non-`core` component without declaring `class` —
  registry merging is independent of `--split`.)
- Docs gain the class concept: component catalog (per-component class),
  bundling guide (`--split`, set layout, ordering contract, deployer
  support matrix), and contributor recipe docs (class field, direction
  rule, and a note that a class change on a deployed cluster is applied
  by redeployment).

## Implementation Plan

1. Catalog prerequisite: repoint all `core` and `workload`
   `kube-prometheus-stack` dependency declarations (overlays and mixins)
   to `prometheus-operator-crds`; add the EKS-only mixin that declares
   the NVSentinel datastore's `aws-ebs-csi-driver` edge; add the
   resolved-graph dependency-direction guard test. After this lands, a targeted
   non-split parity regression test pins that the class field and split
   plumbing leave ordinary output byte-identical.
2. Registry: the ordered `class` field (`core`, `ops`, `workload`),
   validation of known class names, an explicit class on every embedded
   entry (enforced by CI), and the initial classification per the class
   definitions, documented in `docs/user/component-catalog.md`.
3. Bundler: `--split` flag (CLI, SDK, REST) with the
   plan/validate-once/render-subsets pipeline, per-class directories,
   set-level `recipe.yaml`/`split.yaml` (with `recipeDigest`)/
   `checksums.txt`/attestation, create-only staged atomic publication at
   the functional API boundary (net-new for local output; includes the
   `pkg/server` non-existent-child accommodation and the explicit
   output-path requirement), the merge-time external-registry `class`
   warning, and fail-closed rejection of everything outside the
   deployer/output support matrix (Helm-local only in v1).
4. Partition validation: known-class check, resolved-graph direction rule,
   explicit OCP rejection, cohesion sets (ADR-016 accounting),
   all-disabled-class rejection,
   rejection of caller-supplied `bundlers` filters and of disabled
   checksums in split mode, and full-set emitted Helm identity validation
   before subset rendering.
5. Docs and examples as listed above.

Deferred (explicitly out of the first version): additional classes,
user-defined class names, per-shape (overlay-level) class reassignment,
standalone verified per-class artifacts (with a class-subset selector
such as `--classes`, once the externally satisfied profile-lock contract
exists), OCI split output, a deploy-time compatibility protocol,
multi-instance components, a registry-level `conflictsWith`
declaration, class inheritance in `aicr recipe --inherit-from` with
class-move reporting in `aicr upgrade-check` (a prerequisite for any
built-in reclassification), and a direction-guard diagnostic that names
the components a reassignment would also move.
