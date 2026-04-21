# ADR-006: Registry-Driven Component Check Definitions

## Status

**Proposed** — 2026-04-20
**Revised** — 2026-04-21 (contract clarifications, rollout sequencing, related design coordination, shared check-definition framing, and validator convergence)

This PR records the design only; it does not include the implementation.
If accepted, the shared schema is planned for `pkg/recipe/`, with initial
consumers in post-install validation and deploy / cleanup flows. The
steady-state CI guard for newly added components remains a follow-up.

## Scope

This ADR covers the **shared component check-definition model** used by
deployment-related phases of AICR.

It defines:
- how component check metadata is expressed in `recipes/registry.yaml`
- how that metadata is transported in the resolved recipe for validator consumers
- how initial phase consumers interpret the shared check definitions
- the intended convergence point for those consumers around a shared validator
  capability
- why AICR wants one source of truth for component checks, while still using a
  Kubernetes-native, pure-Go validator path for post-install validation

It does **not** change:
- readiness constraint evaluation before deployment validation
- the exact runtime implementation of every future phase consumer
- repo-side Chainsaw health-check workflows used in tests and component health assets
- the long-term deprecation or retention plan for `expectedResources`

## Problem

After the targeted GPU readiness work in [`#611`](https://github.com/NVIDIA/aicr/pull/611),
deployment-related checking was stronger but still asymmetric:

- baseline namespace and `expectedResources` checks ran for enabled components
- bespoke deep checks existed only for a few GPU-specific gaps
- many components still had only shallow validation coverage
- deploy / cleanup orchestration and post-install validation still risked
  carrying overlapping but separate readiness stories
- adding a new component did not naturally force a deep, shared check story

This created three problems:

1. **Coverage drift**: deep component checks depended partly on Go code,
   partly on ad hoc `expectedResources`, and only partially on registry metadata.
2. **Phase drift**: deploy / cleanup orchestration and post-install validation
   could end up maintaining overlapping readiness intent in different places.
3. **Weak extensibility**: the project needed a typed contract that could scale
   across the current component inventory, future additions, and multiple
   validation or lifecycle phases.

## Context

AICR already has two relevant validation models:

1. **`aicr validate --phase conformance` is Kubernetes-native and pure Go.**
   Conformance checks are dispatched as Go check functions in
   `validators/conformance/main.go` and operate through Kubernetes APIs and
   evidence capture, not through Chainsaw execution.

2. **Chainsaw exists in the repository, but as a separate asset/runtime.**
   Repo-side health checks live under `recipes/checks/*`, and the project has a
   reusable Chainsaw runtime in `validators/chainsaw/`. That runtime supports
   both raw resource assertions and full `kind: Test` execution, including
   shelling out to a `chainsaw` binary when required.

The Chainsaw discussion also has an important nuance: now that Chainsaw can
build cleanly with the current Go toolchain, AICR could choose to consume more
of the Chainsaw assertion engine as a Go library. That makes a library-backed
option more realistic than when Chainsaw first entered the project. It does not
eliminate the main design question here, though, because the current
repo-side health-check inventory is still authored as full `kind: Test` assets,
not just raw assertions. Direct reuse of those assets at validator runtime
would still require either binary support for full test execution or a
non-trivial rewrite into the narrower library-consumable form.

This ADR had to decide whether AICR should:
- establish one registry-resident source of truth for component checks that
  different phases can consume, or
- continue letting deployment-related phases grow partially overlapping
  readiness logic independently

It also had to decide whether those phases should converge on one validator
entrypoint, so that deploy-time waits and post-install validation do not
silently drift apart as the check definitions evolve.

For the post-install validator consumer specifically, the ADR also had to
decide whether to:
- adopt the same Kubernetes-native pure-Go model already used by conformance, or
- reintroduce Chainsaw as a runtime dependency inside
  `aicr validate --phase deployment`

Two additional constraints shaped the decision:

- **Single source of truth**: component checks should be defined once, in the
  registry contract, even if different phases consume different subsets.
- **Deployer-neutrality for validator consumers**: post-install validation must
  not depend on Helm APIs, release metadata, or release-scoped labels such as
  `app.kubernetes.io/instance`.
- **Resolved-recipe transport for validator consumers**: validator Jobs
  consume mounted `recipe.yaml`, not `registry.yaml`, so any post-install
  contract has to travel through the resolved recipe.

There is intentional overlap with the deployment half of
[`#610`](https://github.com/NVIDIA/aicr/issues/610): both discussions touch the
same per-component check intent. The scopes remain different:

- `#610` is about **deploy-time flow control and undeploy-time cleanup**
  through deployer-specific hooks, sync-wave ordering, bridge Jobs, images,
  RBAC, and cleanup behavior
- this ADR is about **post-install validation** in
  `aicr validate --phase deployment` and the broader shared check-definition
  contract those flows can consume

The goal is to keep a consistent definition of component checks and one source
of truth for them, while still letting different phase consumers implement the
runtime details they need. The project should coordinate semantics and
component mappings where that stays simple, without forcing the validator
contract to absorb hook-job details that belong to deploy-time orchestration.

## Options Summary

| Option | Summary | Benefits | Costs |
| --- | --- | --- | --- |
| **Registry fields + shared validator consumers** | Add three component-level fields (`readiness`, `customChecks`, `crds`) and make them the shared source of truth that deployment-related consumers can read, with a pure-Go validator consumer for post-install validation and a common validator entrypoint for wait-based flows | Typed contract, one source of truth, reduced drift between phases, deployer-neutral validator path, easy to unit test, aligns with current `aicr validate` architecture, keeps the validator extensible | Narrower than full Chainsaw expressiveness; requires coordination as more consumers adopt the contract |
| **Reuse Chainsaw at runtime** | Hydrate `healthCheck.assertFile` into validator jobs and run Chainsaw semantics during deployment validation, either through the Go assertion library where possible or full test execution where needed | Best parity with existing `recipes/checks/*`; strongest reuse of current assertions | Second runtime/model inside `aicr validate`, harder testing, upstream coupling, and for the current `kind: Test` inventory still either binary packaging or asset rewrites |
| **Keep bespoke per-component Go checks** | Continue adding code paths per component as gaps are found | No schema work | Does not scale; keeps coverage asymmetric and harder to enforce |

## Decision

Adopt **registry-driven component check definitions, with initial validator
consumers implemented in pure Go**.

Component checks are declared in `recipes/registry.yaml` and become the shared
source of truth for deployment-related phases. Post-install validator
consumers hydrate those definitions onto each resolved `ComponentRef` and
interpret them using Kubernetes API state only. Other consumers, such as
deploy / cleanup flow assets, can read the same logical definitions without
requiring a second per-component check inventory.

The intended convergence point is a shared validator capability, exposed
through `aicr validate`, that different phases can delegate to. In that model,
operators, CI, and bridge Jobs all rely on the same underlying readiness
implementation, including a wait-capable mode such as `--wait --timeout`,
rather than re-implementing readiness logic in parallel.

The schema adds three component-level fields — `readiness`, `customChecks`, and
`crds` — so each component can declare its own component check contract. The
initial validator consumer then runs those declared checks across all enabled
components in the resolved recipe, on a pure-Go path that can run in any
Kubernetes cluster and extend naturally toward broader CNCF AI Conformance
validation.

This option was chosen because it creates a typed, enforceable
component-check contract in `recipes/registry.yaml`, gives AICR one source of
truth for deployment-related checks, keeps validator jobs aligned with the
resolved recipe as the single runtime input, applies declared checks across all
enabled components, and preserves one pure-Go validator model that is easy to
unit test and extend toward broader **CNCF AI Conformance** validation.

### Contract

The shared component check contract is:

```yaml
components:
  - name: kube-prometheus-stack
    readiness:
      namespace: monitoring
      selector:
        app.kubernetes.io/name: kube-prometheus-stack
      workloads:
        - kind: Deployment
          name: kube-prometheus-operator
    customChecks: []
    crds: []
```

This is intentionally not a general-purpose DSL or arbitrary assertion
language. The shared contract is a small typed schema over common Kubernetes
resource-check primitives, with explicit override assets reserved for cases the
structured schema cannot express cleanly.

The supported coverage shapes are:

- `readiness`
  - defines **workload rollout checks** for `Deployment`, `DaemonSet`, and
    `StatefulSet` resources selected within a component namespace
  - despite the broader name, v1 uses `readiness` as a narrow workload-focused
    term: it does not mean arbitrary application health, pod-phase parity, or
    full Chainsaw semantics
  - the field name remains `readiness` to stay aligned with the chosen
    registry contract and implementation vocabulary, but the ADR fixes its
    meaning to workload readiness in this deployment-validation phase
- `crds`
  - defines **declared CRD establishment checks**
  - used for CRD-only or CRD-heavy components where the deployment signal is
    that specific `CustomResourceDefinition` objects are present and
    `Established=True`
  - remains a first-class structured field because CRD establishment is a
    common declarative signal, not merely a bespoke per-component edge case
- `customChecks`
  - defines **fixed component-specific Go checks**
  - used only when the desired readiness signal cannot be expressed cleanly by
    generic workload rollout or CRD establishment checks
  - remains intentionally narrow and registry-declared rather than arbitrary
    script or assertion execution

The `readiness` object carries:

- required `namespace`
- optional deployer-neutral `selector` using stable labels only;
  release-identity labels such as `app.kubernetes.io/instance` remain out of
  bounds
- optional exact `workloads` using `{kind, name}`
- `workloads.kind` accepts only the exact-case enum values `Deployment`,
  `DaemonSet`, and `StatefulSet`
- `workloads` items do not carry their own namespace; `readiness.namespace`
  is authoritative in v1

Under this design, validator consumers hydrate these fields onto each resolved
`ComponentRef` in `RecipeResult`. Post-install validator Jobs read mounted
`recipe.yaml`; they do not read `registry.yaml` directly at runtime. Other
phase consumers can derive from the same registry definitions without creating
an independent per-component check inventory.

Examples in `recipes/registry.yaml` look like:

```yaml
components:
  - name: cert-manager
    readiness:
      namespace: cert-manager
      selector:
        app.kubernetes.io/name: cert-manager

  - name: kube-prometheus-stack
    readiness:
      namespace: monitoring
      selector:
        app.kubernetes.io/name: kube-prometheus-stack
      workloads:
        - kind: Deployment
          name: kube-prometheus-operator

  - name: dynamo-crds
    crds:
      - backends.dynamo.nvidia.com
      - functions.dynamo.nvidia.com
      - models.dynamo.nvidia.com

  - name: skyhook-customizations
    customChecks:
      - skyhookReady
```

These examples show the intended split:

- use `readiness` when the component signal is workload rollout health
- use `crds` when the component signal is declared CRD establishment
- use `customChecks` only for the few cases where the signal is a
  component-specific status check that the generic model does not express well

Structured fields are the default path. When a component genuinely needs logic
that the structured contract cannot express cleanly, a phase-specific override
asset such as `readiness.yaml` may remain as an explicit escape hatch. This ADR
intentionally leaves the exact precedence and override mechanics to follow-up
design work rather than locking them here.

Overlay and external-data merge policy is replace-if-set for all three fields:

- `readiness`: replace the whole object if set; otherwise inherit
- `customChecks`: replace the whole list if set; otherwise inherit
- `crds`: replace the whole list if set; otherwise inherit

### Initial validator primitives

The initial post-install validator consumer uses a narrow, stable v1 contract:

- `Deployment`: `availableReplicas >= desiredReplicas`, with nil replicas treated
  as Kubernetes default `1`; explicit `0` is allowed
- `StatefulSet`: `readyReplicas >= desiredReplicas`, with nil replicas treated
  as Kubernetes default `1`; explicit `0` is allowed
- `DaemonSet`: `desiredNumberScheduled > 0` and `numberReady == desiredNumberScheduled`
- `CustomResourceDefinition`: `Established=True`

A selector plus a generic Kubernetes `Ready` condition alone was considered too
narrow for the current inventory. The initial validator subset also needs exact
named workload identities, tightened `DaemonSet` semantics, declarative CRD
establishment checks, and a small escape hatch for component-specific status
logic.

When both `selector` and `workloads` are present:
- selector matches and named workloads are unioned
- the set is deduplicated by `{kind, namespace, name}`
- every named workload is required; if any named workload is absent, the
  component fails even when selector-discovered workloads exist
- zero effective matches fail closed

`customChecks` are intentionally narrow:
- fixed registration map in Go
- fixed v1 key set:
  - `clusterPolicyReady`
  - `skyhookReady`
  - `draKubeletPluginReady`
- component-scoped execution only

`expectedResources` continues to coexist during migration, but the registry
contract becomes the standard component-coverage path for validator consumers.
Its long-term retention or deprecation remains out of scope for this ADR; see
Scope above.

Because `expectedResources` and generic readiness share the same typed workload
health primitive, the tightened `DaemonSet` rule above applies to both paths.

The broader validator capability should treat waiting as a general feature, not
as a deployment-hook-only special case. Wait-capable checks should honor
`--wait --timeout`, while one-shot checks should resolve immediately without
special casing at the CLI level.

## Why Not Runtime Chainsaw

Reusing Chainsaw at deployment-validator runtime was considered seriously because
it offers the best semantic reuse of the existing `recipes/checks/*` assets.

It was not chosen for the post-install validator consumer because it would make
`aicr validate` harder to operate and extend:

- it introduces a **second validator runtime** inside `aicr validate`
  - pure Go / Kubernetes-client checks for conformance
  - Chainsaw-driven execution for deployment
- distroless packaging is not the blocker by itself: AICR could ship an
  additional binary in a distroless image if it chose to. The cost is the
  broader packaging, signing, SBOM / CVE scanning, upgrade, and operational
  surface of making Chainsaw part of the validator runtime
- even with greater library-backed reuse, the current repo-side health-check
  inventory is still authored as full `kind: Test` assets, so direct reuse
  would still require either binary support for full test execution or
  rewriting those assets into a narrower form
- it couples validator behavior more tightly to **upstream Chainsaw semantics and upgrade cadence**
- it expands the maintenance and review surface because full Chainsaw `Test`
  execution brings scripts, waits, catches, and binary execution into runtime validation
- it is harder to unit test than typed Go readiness checks using fake clients
- it still needs explicit transport into validator Jobs because those jobs consume
  mounted `recipe.yaml`, not `registry.yaml`

Even if the internal evaluator changes later, the stable surface for consumers
should remain the validator entrypoint and shared check contract rather than
exposing phase-specific executor differences.

Chainsaw remains useful as:
- a repo-side health-check asset format
- a UAT / test workflow tool
- a possible override or escape-hatch executor when the structured schema is
  not sufficient for a specific component
- a source of parity reference when deciding whether the Go-based contract is too narrow

## Consequences

### Positive

- **Single source of truth**: deployment-related phases can converge on one
  component check definition instead of maintaining parallel readiness stories.
- **Shared validator behavior**: if deploy / cleanup flows and direct users
  delegate to the same validator entrypoint, readiness semantics stay aligned
  by default rather than through documentation discipline alone.
- **Symmetric validator coverage**: every current component can be mapped to
  `readiness`, `customChecks`, or `crds`.
- **Typed, reviewable metadata**: component-check behavior becomes visible in
  the registry rather than hidden in code-only paths.
- **Architecture consistency**: post-install validation and conformance both
  stay on a Kubernetes-native pure-Go validator model.
- **Extension path**: future CNCF AI Conformance validation can build on the same
  validator model instead of mixing Go checks with a second runtime engine.
- **Good testability**: workload discovery, zero-match behavior, CRD checks, and
  custom-check execution can all be exercised with fake clients.

### Negative

- **Narrower than Chainsaw**: the initial validator contract does not try to
  reproduce full Chainsaw expressiveness or pod-phase parity.
- **Cross-consumer coordination**: a shared schema means deploy / cleanup and
  validator consumers must stay aligned on the meaning of the contract.
- **More validator surface area**: general wait semantics introduce additional
  CLI and runtime behavior such as polling, backoff, timeout handling, and
  clean termination behavior for bridge-job-style consumers.
- **Fixed special-case surface**: new bespoke behavior requires coordinating Go
  and registry changes rather than arbitrary registry-defined assertions,
  unless an explicit override path is used.
- **Migration overlap**: `expectedResources` can temporarily duplicate reporting
  while the registry contract becomes the standard path.

## Non-Goals

This ADR intentionally does **not** attempt:

- full parity with existing repo-side Chainsaw health checks
- pod-phase checks such as `Pending`, `Failed`, or `Unknown`
- generic `Job` readiness in v1
- validator-side registry lookups at runtime
- Helm-aware or release-aware readiness logic
- a complete runtime specification for every future consumer of the shared
  schema

## Implementation Notes

The target implementation associated with this ADR will include:

- component check metadata on `ComponentConfig` and `ComponentRef`
- hydration of component check metadata into the resolved recipe
- overlay/external-data merge rules for `readiness`, `customChecks`, and `crds`
- post-install validator support for:
  - selector-based discovery
  - exact workload identities
  - union + dedup semantics
  - fixed custom checks
  - CRD `Established=True` readiness
  - fail-closed zero-match behavior
- shared validator support for:
  - a wait-capable mode such as `--wait --timeout`
  - reuse by bridge-job-style deploy / cleanup flows and direct CLI callers
- registry mappings for the current component inventory
- follow-up consumers that can read the same definitions in other phases rather
  than introducing a second per-component check inventory

At the time of writing this ADR, these details describe design intent rather
than current `main`. In particular, `ComponentConfig` / `ComponentRef`
component check metadata and the associated hydration path are planned follow-up code
changes, not already-exposed fields in `pkg/recipe/metadata.go`.

The broader direction is a generic, phase-aware validation model that can
consume the same registry-defined checks across deploy / cleanup,
post-deployment, and conformance-related paths. Resource types beyond the
initial validator subset, such as Pods, Jobs, and specific custom resources,
can be layered onto the same schema over time without introducing separate
phase-specific check inventories.

One follow-up design point remains intentionally open: if structured fields are
not sufficient for a component, the project may use an explicit override asset
such as `readiness.yaml`. The existence of that escape hatch is compatible with
this ADR; the precise precedence between structured fields and override assets
should be specified by the follow-up implementation that introduces it.

The rollout is intentionally sequenced in two phases:

- **Phase 1: migrate the current inventory**
  - add the component contract fields
  - hydrate them into the resolved recipe
  - populate the current **21-component** registry inventory
  - allow temporary overlap with `expectedResources` during migration
- **Phase 2: enforce the steady-state rule for newly added components**
  - once the current migration inventory is complete, `pkg/recipe` validation
    surfaced through existing repo CI or lint should reject newly added
    components that declare neither:
    - `readiness`
    - `customChecks`
    - `crds`
    nor an explicit approved override asset such as `readiness.yaml`, if that
    escape hatch is adopted by the corresponding follow-up implementation
    and should not be implemented as additional runtime deployment-validator
    behavior or as a separate standalone validator workflow

## Alternatives Considered

### 1. Keep adding bespoke Go checks per component

Rejected because it keeps deep readiness asymmetric across the registry and does
not produce an enforceable component contract.

### 2. Reuse Chainsaw health checks directly in deployment validation

Rejected for the reasons captured in [Why Not Runtime Chainsaw](#why-not-runtime-chainsaw).

### 3. Namespace-wide heuristics with no registry contract

Rejected because shared namespaces such as `kube-system` and `monitoring` need
explicit scoping or exact identities to avoid false attribution and brittle behavior.

## References

- [ADR-002: Replace Go Test Validation Engine with Container-Per-Validator Model](002-validatorv2-adr.md)
- [Issue #610](https://github.com/NVIDIA/aicr/issues/610) — bridge jobs for deploy/undeploy lifecycle flow control
- [Issue #607](https://github.com/NVIDIA/aicr/issues/607) — narrow GPU-scoped deployment readiness gap
- [PR #611](https://github.com/NVIDIA/aicr/pull/611) — targeted GPU readiness groundwork
- [Issue #622](https://github.com/NVIDIA/aicr/issues/622) — broader registry-driven deployment readiness follow-up
