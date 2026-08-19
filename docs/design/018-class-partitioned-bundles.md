# ADR-018: Class-Partitioned Bundles

## Status

**Proposed** — 2026-08-11.

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
whose absence prevents workloads from being scheduled or run) and the ops
stack (components whose absence mainly reduces observability, administration,
or operational convenience) have different owners, different change-approval
processes, and different release cadences. The need for co-existing bundles
has surfaced in several contexts; the first concrete consumer needs a
runtime bundle and an ops bundle that co-exist in the same cluster and are
deployed and operated by different owners.

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

- Classify every registry component into a named class, with `core` and
  `ops` as the initial classes.
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
  *published*. A standalone class artifact with an externally-satisfied
  profile-lock contract is follow-up work.
- Cross-version compatibility between sub-bundles. AICR emits the set and
  its ordering contract; verifying that an ops bundle from one generation
  is compatible with a core bundle deployed from another is
  operator-managed, and a deploy-time compatibility protocol is explicit
  follow-up work.
- OCI output for split generation. Multi-artifact naming and
  partial-publish recovery are undefined; `--split` with OCI output is
  rejected in v1.
- Split generation for OCP recipes. Supported OCP recipes declare the
  initial `ops` components but disable them all, so a split has nothing
  to offer OCP until the `-ocp` monitoring variants are classified;
  `--split` rejects all-disabled classes (see Partition validation).
- Reassigning a component's class per recipe shape. Class is registry-only
  and stable; moving a component between classes is a coordinated
  migration, not a recipe-authoring decision.
- Changing recipe resolution, overlay matching, or mixin composition. The
  class is read at bundle time only.
- A generic dependency solver or per-class release tooling.
- A registry-level mutual-exclusion (`conflictsWith`) declaration. Today's
  scattered checks (for example gpu-operator vs gpu-operator-ocp,
  [#1685](https://github.com/NVIDIA/aicr/issues/1685))
  would benefit from one, but that is a separate follow-up.
- More than two shipped classes. The mechanism is N-ary by design (an
  `apps` class for workload platform operators such as Kueue or Dynamo is
  a plausible future), but this ADR ships `core` and `ops` only.

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
the three ops components in `monitoring`.

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
  ...
```

- Allowed values are the known class names; the initial set is `core` and
  `ops`. The class set is a **closed enumeration in Go**, not
  registry-defined strings: an unenumerated class cannot be pre-validated
  against the catalog, so new classes are added only in code, at a
  release — cheap to extend, but the constraints are kept.
- Omitted means `core`. This default is **availability-safe, not
  classification-safe**: an unclassified component lands in the bundle
  that must always be installed, but a component that *should* be `ops`
  and omits the field is misclassified into `core` with no mechanical
  signal beyond the external-replacement warning below.
- The field is named `class`, not `type`, because `componentRefs[].type`
  already means Helm-vs-Kustomize.
- Class is **registry-only and stable**. Overlays cannot reassign it, and
  it is not recorded in the resolved `RecipeResult` — so `recipe.yaml`,
  checksums, and attestations of ordinary bundles are unchanged. Moving a
  component between classes is a registry change treated as a coordinated
  migration for clusters with a deployed split.
- The default applies uniformly to **external registry entries** too: an
  external `registry.yaml` entry replaces the embedded entry wholesale, so
  a replacement for an `ops` component that omits `class` lands in `core`.
  A replacement that intends `ops` must restate it; registry merging emits
  a merge-time warning when an external entry replaces an embedded `ops`
  component without declaring `class`.
- External entries carry `class` like embedded ones, so an internal or
  third-party catalog can classify its own components into the existing
  classes **without any public change**. Only a brand-new class *name*
  requires extending the Go enum — a one-value, release-time addition
  made when a concrete consumer needs it.

Initial classification: `kube-prometheus-stack`, `prometheus-adapter`, and
`k8s-ephemeral-storage-metrics` are `ops`; everything else, including
`nvsentinel` and `prometheus-operator-crds`, is `core`. The working
rubric, sharpened by DGXC operational experience: **`ops` must be GPU-free
and verifiable on CPU-only clusters**. `nvsentinel` is `core` under that
test — its node remediation is part of the GPU runtime and must be tested
with GPUs, and its dependencies (`cert-manager`, `gpu-operator`,
`prometheus-operator-crds`) are already `core`.

**`class` is not a criteria axis.** Criteria (`service`, `accelerator`,
`intent`, `os`, `platform`) drive overlay matching, and every new axis
multiplies the recipe-shape and test matrix. `class` deliberately does not
participate in matching: it is a label read at bundle time, after
resolution. The recipe count, the overlay files, and the resolved
`RecipeResult` are identical whether or not `--split` is ever used, so the
split adds surface linearly per class (bundle output and partition checks),
not multiplicatively per recipe × class. The blast radius of introducing
the field is correspondingly small: with `core` as the default, existing
registry entries need no edits beyond the handful classified as `ops`, and
generation without `--split` remains byte-identical relative to the
post-prerequisite catalog baseline.

### Class dependency direction

Cross-class dependency edges must point one way: **`ops` may depend on
`core`; `core` must not depend on `ops`.** The class graph is built from
**cross-class edges only** — same-class dependencies are ignored at class
level (they would otherwise collapse into self-loops) and remain governed
by the existing component-level cycle validation. That cross-class graph
must be acyclic. This keeps the deployment contract trivial (install core
first, then ops) and gives partition validation a crisp rule to enforce.

Direction is the floor, not the goal: classification should also
**minimize cross-class edges** in the first place, because operating
models exist where no ordering between bundles can be enforced at all. In
the DGXC Runtime, for example, the runtime and operations stacks deploy as
separate Argo CD app-of-apps with no enforceable ordering between them.
The install-core-first contract therefore binds only the sequential helm
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
operators consume the helm-local bundle set with their own per-class
Application/Kustomization wrappers; AICR-generated Argo/Flux split output
is the deferred follow-up.

Retry also has limits: it covers prerequisites whose absence makes
reconciliation *fail* (such as missing CRDs); **fail-open prerequisites**
— ops resources that apply successfully despite a missing non-CRD
dependency — are not verified in v1, nor is cross-bundle version
compatibility; a live set-identity or preflight protocol remains
deferred.

The existing catalog violates the rule, and fixing it is a prerequisite.
Three core components declare dependencyRefs on `kube-prometheus-stack`
(declared to guarantee the ServiceMonitor CRDs exist before their monitors
are applied; the prerequisite PR verifies each edge's actual purpose
before repointing it): `gpu-operator` in 26 declarations (including
base), `dynamo-platform` in 8, and `kubeflow-trainer` in 4 (3 overlays
plus 1 mixin, `recipes/mixins/platform-kubeflow.yaml`) — 38 declarations
in total, **across overlays and mixins**. Because `dependencyRefs` merge
additively across the composition chain, repointing only
`recipes/overlays/base.yaml` fixes nothing; every declaration must be
repointed to `prometheus-operator-crds`, which stays in `core`. CRDs are
cluster API surface needed by both sides; keeping shared CRD packages in
`core` is the general pattern.

Dependency edges are **install-time requirements only**; runtime data
flows need no edge and naturally run in the allowed direction. The
Prometheus split is the canonical case: core components (such as
`nvsentinel`) install monitor CRs, which requires only the
`monitoring.coreos.com` CRDs — `prometheus-operator-crds`, a `core`
component (`nvsentinel` already declares that direct edge; see
[#928](https://github.com/NVIDIA/aicr/issues/928)) —
while the ops Prometheus *scrapes* those monitors at runtime, an
ops-consumes-core flow with no install edge at all. If ops is absent,
core runs fully; its metrics are simply not collected yet, which is the
definition of `ops`.

The repoint prerequisite changes ordinary bundles **once**, independent of
`--split`: `dependencyRefs` serialize into each affected recipe's emitted
`recipe.yaml`, so the repoint is a one-time `recipe.yaml` and checksum
change for every recipe that carried the old edge. All byte-identity
claims in this ADR are relative to that post-prerequisite catalog
baseline; the class field and split plumbing themselves change nothing in
ordinary output.

To keep the boundary fixed, a standing guard test validates the
**effective dependency graph of every resolved catalog recipe** — not the
raw overlay/mixin YAML — and fails on any `core` → `ops` edge or
class-level cycle, in the spirit of the existing deployment-order guard
tests.

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
  directory) — distinct from the rejected all-disabled case below — so
  the flag is not hardcoded to two classes: adding a future class (for
  example `apps` for workload platform operators) changes the output
  layout of recipes that declare such components, and nothing else. A
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
- a class-level dependency cycle, or any `core` → `ops` edge in the
  resolved recipe;
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
enabled component into the empty class's directory. It also sets v1 scope
honestly: supported OCP recipes declare the initial `ops` components but
disable them all (`recipes/overlays/ocp.yaml`), so `--split` on OCP
recipes — and any bundle-time override combination that disables a whole
class — is rejected in v1. OCP split support is follow-up work that
starts with classifying the `-ocp` monitoring variants.

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
class of every component, and the class ordering contract (core before
ops). `recipeDigest` plus the class map identify the **resolved recipe and
its partition**, not the rendered payload: bundle-time inputs (`--set`,
typed overrides, scheduling settings) land in the extracted component
values, not in `recipe.yaml`, so two invocations can share a
`recipeDigest` yet render different class directories. The rendered payload is
identified by the digest of the root `checksums.txt` — which remains the
bundle attestation's in-toto subject, exactly as today; the split changes
neither contract. The `split.yaml` schema, defined with the
implementation, restates this scope note alongside the `recipeDigest`
field.

Because class lives only in the registry, the same `recipeDigest` can
partition differently after a registry classification change. The
per-component class map in `split.yaml` is deliberately the record that
disambiguates, and a set generated before a class migration stays
auditable through its recorded map.

`split.yaml` lets an operator answer "which recipe and partition produced
what is deployed here" — it is not a deploy-time enforcement mechanism,
and deployment does not reject on digest comparison. Cross-version
compatibility between a deployed core and a newer ops set remains an
operator-managed concern (see Non-Goals).

### Deployer identities

Two sub-bundles deployed to one cluster must not collide on deployer-level
identities. The rule: **sub-bundle root identities derive from the class** —
for example, the Argo CD parent Application `<app-name>-core` /
`<app-name>-ops`.

Component-level identities need one guard beyond unique component names:
*synthetic* release names. The Helm writer injects `<name>-pre` /
`<name>-post` wrapper releases and today detects collisions against the
complete component list, so per-class rendering could miss a collision
split across classes (a core component's injected wrapper vs an ops
component's name — reachable with external component registries). Emitted
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
| `helm` (local) | Supported | Set directory layout (`core/`, `ops/`); full-set emitted-identity validation before rendering |
| `helmfile` (local) | Rejected | Follow-up: per-class root helmfile |
| `argocd` (local) | Rejected | Follow-up: parent Application `<app-name>-core` / `<app-name>-ops` |
| `argocd-helm` (local) | Rejected | Follow-up: same parent-Application rule |
| `flux` (local) | Rejected | Follow-up: per-class root Kustomization |
| Any deployer + OCI output | Rejected | Follow-up: multi-artifact naming + partial-publish recovery undefined |

Enabling a rejected deployer is incremental follow-up work: wire its
class-derived root identity, add it to the matrix, and lift the rejection.

## Example

An EKS H100 training recipe resolves, as today, to one `RecipeResult` with
components including `nfd`, `gpu-operator`, `network-operator`,
`cert-manager`, `prometheus-operator-crds`, `nvsentinel`, `kueue`,
`kubeflow-trainer` (class `core`) and `kube-prometheus-stack`,
`prometheus-adapter` (class `ops`).

```shell
aicr bundle --recipe recipe.yaml --split --output ./bundles
```

validates the complete recipe once, then renders:

```text
bundles/
├── recipe.yaml                 # the full resolved recipe (set-level)
├── split.yaml                  # audit: recipeDigest + class of every
│                               # component + ordering contract (core → ops)
├── checksums.txt               # covers the whole set
├── core/
│   ├── deploy.sh               # cert-manager, nfd, prometheus-operator-crds,
│   │                           # gpu-operator, network-operator, nvsentinel,
│   │                           # kueue, ...
│   └── <component folders>
└── ops/
    ├── deploy.sh               # deploy after core; installs
    │                           # kube-prometheus-stack, prometheus-adapter, ...
    └── <component folders>
```

`kube-prometheus-stack`'s dependency on `prometheus-operator-crds` crosses
the boundary in the allowed direction (`ops` → `core`) and is treated as
satisfied externally while rendering `ops/`; the complete edge remains
auditable in the set-root `recipe.yaml`. The runtime owner deploys
`core/`; only after `core/` is installed does the observability owner
invoke `ops/deploy.sh`.
The v1 script does not query the cluster to preflight cross-class
prerequisites; it assumes the operator followed the `core` before `ops`
ordering contract. Taking a *newer* set's `ops/` against an older deployed
`core/` is likewise an operator decision that `split.yaml` makes auditable but
v1 does not mechanically verify.

## Open Questions

**How do we classify components into classes, and how many classes?** The
initial two-way split is a working hypothesis, not a settled taxonomy:

- `core` — the GPU runtime and cluster plumbing: `gpu-operator`,
  `network-operator` (fabric), `cert-manager`, storage drivers
  (`aws-ebs-csi-driver`), `nfd`, DRA, schedulers, `nvsentinel`, and shared
  CRD packages.
- `ops` — the GPU-free, Prometheus-centered observability stack:
  `kube-prometheus-stack`, `prometheus-adapter`,
  `k8s-ephemeral-storage-metrics`.

Classification is not a set of independent per-component labels: the
direction rule makes it **cascade along dependency edges**. `core` must be
dependency-closed — everything a `core` component depends on must itself
be `core` — so classifying one component can force its whole dependency
closure. `nvsentinel` was the live example: as GPU health monitoring it
reads as `ops`, but its node remediation is runtime-critical and it must
be tested on GPU clusters, so the GPU-free rubric settles it as `core` —
cleanly, since its dependencies are already `core`. The resolved-graph
guard catches a violating assignment mechanically; deciding which closure
is *right* for future boundary components remains the taxonomy question.

Should there be a third, **workload-specific** class for platform operators
such as `kubeflow-trainer`, `kueue`, `dynamo-platform`, `grove`,
`agentgateway`, `k8s-nim-operator`, and the Slinky Slurm set? These are
neither node-level runtime (a cluster schedules and runs pods without them)
nor observability; they track upstream platform releases on their own
cadence and are already selected by `intent`/`platform` criteria. Folding
them into `core` keeps the MVP at two classes but makes the runtime bundle
larger and more churn-prone than "runtime core" suggests. The mechanism in
this ADR is N-ary either way; the open question is the shipped taxonomy —
the working rubric ("absence prevents scheduling/running" for `core`,
"GPU-free and verifiable on CPU-only clusters" for `ops`, "absence removes
a workload platform" for a potential third class) to be confirmed with the
first consumer before the registry classification lands.

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
proof obligation,
not a registry property, is the thing to design against if standalone class
artifacts are ever pursued.

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
- **Per-class emission and overlay-level class reassignment.** Considered
  and dropped: emitting a single class, or letting overlays reassign class
  per shape, lets two generation calls disagree about which bundle owns a
  component — reintroducing across calls the collision that single
  resolution prevents within one. Atomic complete generation and a
  registry-only stable class close that hole; per-shape needs, if they
  materialize, are follow-up work with an explicit migration story.

## Consequences

- Adding a component now includes deciding its class; omission safely
  defaults to `core`.
- The `core`/`ops` boundary becomes a checked contract: the resolved-graph
  guard fails CI on a new `core` → `ops` dependency instead of letting it
  break split deployments.
- All existing `kube-prometheus-stack` dependency declarations from core
  components (see Class dependency direction for the per-component
  counts) must be repointed to `prometheus-operator-crds` before the
  split can ship — a mechanical but catalog-wide prerequisite PR that
  changes affected recipes' emitted `recipe.yaml` and checksums once.
- Single-bundle users see no artifact or output change; `--split` is
  strictly opt-in, and non-split output stays byte-identical from the
  post-prerequisite baseline onward. (One new diagnostic applies to all
  users: the merge-time warning for external registry entries that
  replace an embedded `ops` component without declaring `class` — registry
  merging is independent of `--split`.)
- Docs gain the class concept: component catalog (per-component class),
  bundling guide (`--split`, set layout, ordering contract, deployer
  support matrix), and contributor recipe docs (class field, direction
  rule, migration note for class moves).

## Implementation Plan

1. Catalog prerequisite: repoint all core-component
   `kube-prometheus-stack` dependency declarations (overlays and mixins)
   to `prometheus-operator-crds`; add the resolved-graph
   dependency-direction guard test. After this lands, a targeted
   non-split parity regression test pins that the class field and split
   plumbing leave ordinary output byte-identical.
2. Registry: `class` field, validation of known class names, initial `ops`
   classification of the three GPU-free monitoring components.
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
   cohesion sets (ADR-016 accounting), all-disabled-class rejection,
   rejection of caller-supplied `bundlers` filters and of disabled
   checksums in split mode, and full-set emitted Helm identity validation
   before subset rendering.
5. Docs and examples as listed above.

Deferred (explicitly out of the first version): additional classes,
per-shape class reassignment, standalone verified per-class artifacts
(with a class-subset selector such as `--classes`, once the
externally-satisfied profile-lock contract exists), OCI split output, a
deploy-time compatibility protocol, multi-instance components, and a
registry-level `conflictsWith` declaration.
