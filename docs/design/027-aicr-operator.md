# ADR-027: AICR Operator

## Status

**Proposed** — 2026-10-28. Answers [#3285](https://github.com/NVIDIA/aicr/issues/3285).
Implementation follows acceptance as a separate change.

## Decision Summary

AICR adds the AICR Operator: an opt-in, in-cluster component that keeps a
running cluster on a validated recipe through day-2 change. It adds
coordination, not configuration. It takes the same inputs as the AICR CLI,
runs the same AICR commands that teams now run and sequence by hand, from a
pinned AICR image, and hands every change to the deployment engine (Argo CD
first, then Flux), in the dependency order AICR's bundle already encodes. What
it adds is what neither AICR nor the engines provide: readiness at the revision
just written, approval for every transition AICR does not assess `safe`, and
evidence about what is actually deployed.

The CLI, REST API, SDK and artifact formats do not change, and the CLI still
never applies or reconciles. The operator ships in AICR's release with an alpha
API.

## Context

AICR answers what a cluster should run: it snapshots the cluster, selects a
recipe, renders a bundle, assesses upgrades and validates the result. The
[contributor guide](../contributor/index.md#what-aicr-is-not) leaves everything
after that (readiness, upgrade, rollback, uninstall, ordering, reconciliation)
to the tool that applies the bundle, and keeps in-cluster controllers out of
scope ([#464](https://github.com/NVIDIA/aicr/issues/464)). Day-2 is now on the
[roadmap](../../ROADMAP.md), and the work keeps falling between AICR and those
tools:

- **Verdicts nothing enforces.** `upgrade-check`
  ([ADR-021](021-component-upgrade-safety.md)) rates a transition `safe`,
  `manual`, `unknown`, `unversioned` or `blocked`; the deploy path applies it
  either way.
- **Evidence about a run, not a cluster.** Validation can pass while the
  cluster runs versions other than the recipe's
  ([#2977](https://github.com/NVIDIA/aicr/issues/2977)).
- **One-shot discovery.** Nodes that join later are never checked against the
  recipe ([#3088](https://github.com/NVIDIA/aicr/issues/3088)).
- **Engine hazards.** An Argo CD `Application` can report `Synced` and
  `Healthy` for the revision before the one just written, and deleting or
  changing a release can strip node labels or recreate resources a running GPU
  stack depends on
  ([#3117](https://github.com/NVIDIA/aicr/issues/3117),
  [#2585](https://github.com/NVIDIA/aicr/issues/2585)).
- **Every consumer rebuilds the same controller**
  ([#1723](https://github.com/NVIDIA/aicr/issues/1723)), each with its own
  answer to ordering, approval and evidence.

None of these can be fixed in the CLI, which runs once and exits, or in the
engines, which do not know AICR's recipes, verdicts or evidence.

## Decision

### Scope

The operator lives in this repository, under `pkg/operator` with its own
binary and chart, so it is built, tested and released with the recipes and
validators it runs. It consumes AICR only through the pinned image and its
published artifacts: it imports no AICR package that computes recipes,
bundles, verdicts or evidence (foundation utilities such as structured errors
are allowed), and nothing outside `pkg/operator` depends on it. On acceptance,
the contributor guide keeps its boundary for the CLI, API server, SDK and
bundler, and adds one exception:

> The AICR Operator ([ADR-027](../design/027-aicr-operator.md)) is an opt-in
> in-cluster component under `pkg/operator`. It orders the engine objects it
> writes, gates them on readiness and approval, and repairs drift only in
> those objects; installing charts and reconciling what they deploy stay with
> the engine.

### Behavior

- **Same configuration as the CLI.** The runtime takes the CLI's inputs
  (criteria, intent, platform, overrides) and adds none. Each AICR command
  (`snapshot`, `recipe`, `bundle`, `upgrade-check`, `validate`) runs as a Job
  from the pinned image, so every bundle the operator deploys is identical to
  what `aicr bundle` renders from the same recipe and flags. Those flags
  always include the per-component readiness gates (`--readiness-hooks`),
  which the readiness rule below depends on. Each Job's output is stored
  content-addressed and handed to the next by digest, and the resulting plan
  is immutable and records the snapshot, recipe and bundle it came from; a
  retry reuses it rather than deriving a new one. The operator only decides
  when each command runs and what happens with its result.
- **Discovery is continuous.** A new node profile, AICR image or GPU stack
  re-runs `snapshot` and `recipe`; a cluster that no longer matches its
  criteria stops changing rather than drifting.
- **AICR computes, the engine installs, the operator coordinates.** Before it
  writes an engine object, the operator publishes the plan's charts to an
  immutable source the engine can fetch, such as the OCI registry it is
  configured with, pinned by digest. It writes one engine object per bundle
  release, in the bundle's dependency order, and moves on only when the engine
  reports that exact revision applied and healthy and the release's readiness
  gate passes. A status that is missing or reports any other revision is not
  ready; how each adapter reads its engine's status is an implementation
  detail.
- **Verdicts are enforced.** A transition runs automatically only when every
  changed component is assessed `safe` and no changed chart renders a CRD.
  The operator establishes the latter itself from the two bundles it holds:
  CRDs rendered from templates count as well as those in `crds/`, and a chart
  it cannot read in full counts as changing CRDs. A `blocked` verdict stops
  the transition before any change, naming the component, and no approval
  overrides it; the stored assessment keeps AICR's record with any migration
  steps it gives. Every other verdict needs approval.
- **Nothing is removed by accident.** Deleting the operator or its runtime
  leaves the stack running. Removal is an approved rollout, and a reinstalled
  operator re-adopts the stack with approval.
- **Evidence covers what is deployed.** Once every step is ready, the operator
  takes a fresh snapshot, and AICR's deployment validation runs against the new
  recipe as a whole and that snapshot, never one taken before the rollout. The
  rollout succeeds, and its plan becomes active, only if validation passes; the
  operator binds the evidence to the deployed plan, the snapshot's digest, the
  images and the approval. A failed validation fails the rollout: its plan
  never becomes active, nothing rolls back, and the runtime stalls until a
  person retries with a fresh approval or selects another target, which is
  assessed against what is deployed.

### Approval

An approval never claims a transition is safe. A named approver accepts one
transition as assessed, bound to its starting state (the live engine objects),
target plan and steps; if the live state drifts from what was approved, the
rollout stops. The approval expires and can be revoked. It is a field set with
plain `kubectl` under a role separate from editing the runtime.

For `manual`, a record's steps can fall on both sides of the apply, the only
step the operator performs. The approval acknowledges the steps before it; the
rollout then pauses after the apply until the approver acknowledges the steps
after it, and only then does the plan become active. A step that changes the
bundle's inputs, such as regenerating it at a new pin, does not complete the
transition: it yields a new target, assessed in turn. For `unknown` and
`unversioned`, the approver accepts what AICR has not assessed.

### Release

The operator image and chart ship in AICR's release train at AICR's version.
The API (`operator.aicr.run/v1alpha1`) is alpha and outside AICR's v1 stability
gates; promotion amends this ADR. Its unit and envtest tests join `make
qualify`; a kind lane with the real AICR image runs in CI only, like the GPU
lanes.

### Qualification

AICR documents the operator as supported for a recipe coordinate only after
that coordinate passes on real hardware with the release's images: install; an
AICR upgrade that needs approval; rollback; revocation; removal; orphan and
re-adoption; an operator upgrade and a crash mid-rollout; deployment validation
with bound evidence; and AICR's conformance and performance phases. kind and
envtest prove controller behavior only.

A prototype of this design has passed that list on H100 training with Kubeflow
on GKE (2 × `a3-megagpu-8g`, GPUDirect-TCPXO), against AICR v1.0.0 and v1.1.2,
with a training job running throughout.

## Non-Goals

The operator coordinates AICR and the engine; everything else stays with its
current owner. It does not:

- replace GPU Operator or other component operators, which keep their device
  lifecycles;
- allocate GPUs to tenants or workloads;
- infer training performance from healthy pods; performance comes only from
  AICR's validation phases;
- roll back automatically; a rollback is a new, approved rollout;
- run its own node drains; drains stay with the component that needs them and
  the workload owner;
- support heterogeneous accelerators or several intents in one cluster, adopt
  installations made outside the operator, or drive engines other than Argo CD
  and Flux.

## Alternatives

- **A separate project.** Two release trains and two boundary owners for one
  contract, and consumers keep building their own.
- **Engine-native coordination** (sync waves, progressive syncs). AICR's Argo
  CD waves and Flux `dependsOn` already order releases, but the engines still
  report health for the revision before the one just written, and nothing
  assesses, approves or binds evidence.
- **Embedding the Go SDK or calling the REST API.** Couples the controller to
  AICR's process and dependencies; the API lacks snapshot capture and
  validation.

## Consequences

- AICR gains a long-running component and its first chart, and a failing
  operator gate blocks AICR's release.
- The operator writes engine objects itself, so a cluster that manages its GPU
  stack through Git keeps using the CLI.
- Most upgrades need approval until more components carry transition records.
  That is intended: unknown is not safe.
- Deployment validation needs `cluster-admin`, through a per-run binding or an
  administrator-provisioned privileged ServiceAccount. With the operator, a
  permanent in-cluster component holds the authority to launch it. Narrowing
  the validators' permissions is separate work.
