# ADR-006: Shared Component Check Definitions

## Status

**Proposed** — 2026-04-20
**Revised** — 2026-04-21 (contract clarifications, rollout sequencing, related design coordination, and shared-schema framing)

This PR records the design only; it does not include the implementation.
If accepted, the shared schema is planned for `pkg/recipe/`, with an initial
consumer in post-install validation and follow-up consumers in deploy /
cleanup flows. Any implementation changes are queued separately from this ADR
PR. The steady-state CI guard for newly added components remains a follow-up.

## Scope

This ADR covers the **shared component check-definition model** used by
deployment-related phases of AICR.

It builds on the `#622` validator contract (`readiness`, `customChecks`,
`crds`) and records the broader shared schema needed by both the initial
post-install validator consumer and deploy / cleanup consumers. In that shared
shape, `readiness` carries both the check definition used by the validator and
optional wait-oriented metadata used by deploy-flow consumers.

It defines:
- how component check metadata is expressed in `recipes/registry.yaml`
- how that metadata is transported in the resolved recipe for validator consumers
- how the initial post-install validator consumer interprets the shared check definitions
- how follow-up deploy / cleanup consumers are expected to consume the same
  schema without becoming a second source of truth
- why AICR wants one source of truth for component checks, while still using a
  Kubernetes-native, pure-Go validator path for post-install validation

It does **not** change:
- readiness constraint evaluation before deployment validation
- the exact runtime implementation of every future consumer
- the exact CLI surface or bridge-Job interface for deploy / cleanup consumers
- repo-side Chainsaw health-check workflows used in tests and component health assets
- the long-term deprecation or retention plan for `expectedResources`

## Problem

After the targeted GPU readiness work in [`#611`](https://github.com/NVIDIA/aicr/pull/611),
deployment validation was stronger but still asymmetric:

- baseline namespace and `expectedResources` checks ran for enabled components
- bespoke deep checks existed only for a few GPU-specific gaps
- many components still had only shallow validation coverage
- adding a new component did not naturally force a deep, reusable check story

This created three problems:

1. **Coverage drift**: deep component checks depended partly on Go code,
   partly on ad hoc `expectedResources`, and only partially on registry metadata.
2. **Consumer drift**: other deployment-flow consumers could end up
   maintaining overlapping readiness intent in different places unless they
   consume the same schema.
3. **Weak extensibility**: the project needed a typed contract that could scale
   across the current component inventory, future additions, and multiple
   deployment-flow consumers.

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

That distinction matters even more for non-validator consumers such as
[`#610`](https://github.com/NVIDIA/aicr/issues/610). A deploy-time readiness
Job that wants to render deployer-native behavior such as `kubectl wait`
commands does not just need to execute a check; it needs a canonical,
structured description of what to check. Full Chainsaw `kind: Test` YAML is
strong as an execution and testing format, but weak as canonical semantics for
those consumers because they would otherwise need to either execute Chainsaw or
parse executable test logic to recover structured intent.

This ADR had to decide whether AICR should:
- establish one registry-resident source of truth for component checks that
  different deployment-flow consumers can consume, or
- continue letting deployment validation and deploy / cleanup flows grow partially overlapping
  readiness logic independently

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
  `aicr validate --phase deployment` plus the shared check schema those flows
  should consume

The goal is to keep a consistent definition of component checks and one source
of truth for them, while still letting different phase consumers implement the
runtime details they need. The project should coordinate semantics and
component mappings where that stays simple, without forcing the schema to
absorb hook-job details that belong to deploy-time orchestration.

## Options Summary

| Option | Summary | Benefits | Costs |
| --- | --- | --- | --- |
| **Registry fields + shared consumers** | Add three component-level fields (`readiness`, `customChecks`, `crds`) and make them the shared source of truth that deployment-related consumers can read, with a pure-Go validator consumer for post-install validation | Typed contract, one source of truth, reduced drift between consumers, deployer-neutral validator path, easy to unit test, aligns with current `aicr validate` architecture, keeps the validator extensible | Narrower than full Chainsaw expressiveness; requires coordination as more consumers adopt the contract |
| **Reuse Chainsaw at runtime** | Hydrate `healthCheck.assertFile` into validator jobs and run Chainsaw semantics during deployment validation, either through the Go assertion library where possible or full test execution where needed | Best parity with existing `recipes/checks/*`; strongest reuse of current assertions; familiar third-party tool with existing docs and examples | Additional runtime/model complexity inside `aicr validate`; for the current `kind: Test` inventory still either binary packaging or asset rewrites; weak interchange format for consumers that need structured check semantics rather than direct execution |
| **Keep bespoke per-component Go checks** | Continue adding code paths per component as gaps are found | No schema work | Does not scale; keeps coverage asymmetric and harder to enforce |

## Decision

Adopt **shared component check definitions, with initial validator consumers
implemented in pure Go**.

Component checks are declared in `recipes/registry.yaml` and become the shared
source of truth for deployment-related consumers. The initial post-install
validator consumer hydrates those definitions onto each resolved `ComponentRef`
and interprets them using Kubernetes API state only. Follow-up deploy /
cleanup consumers should consume the same schema rather than maintaining a
second per-component check inventory.

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
      wait:
        timeout: 30m
    customChecks: []
    crds: []
```

This is intentionally not a general-purpose DSL or arbitrary assertion
language. The shared contract is a small typed schema over common Kubernetes
resource-check primitives.

The supported coverage shapes are:

- `readiness`
  - defines **structured readiness metadata** for deployment-flow consumers
  - for the initial post-install validator consumer, this means workload
    rollout checks for `Deployment`, `DaemonSet`, and `StatefulSet` resources
    selected within a component namespace
  - may also carry follow-up wait-oriented metadata for other consumers, such
    as deploy / cleanup flows that render `kubectl wait` commands from the same
    schema
  - intentionally carries both the check definition (`namespace`, `selector`,
    `workloads`) and consumer hints (`wait`) in one object
  - the field name remains `readiness` to stay aligned with the chosen
    registry contract and implementation vocabulary
- `crds`
  - defines **declared CRD establishment checks**
  - used for CRD-only or CRD-heavy components where the deployment signal is
    that specific `CustomResourceDefinition` objects are present and
    `Established=True`
  - remains a first-class structured field because CRD establishment is a
    common declarative signal, not merely a bespoke per-component edge case
- `customChecks`
  - defines **named component-specific checks**
  - used only when the desired readiness signal cannot be expressed cleanly by
    generic workload rollout or CRD establishment checks
  - for the initial post-install validator consumer, supported names are
    implemented through a fixed Go registration map
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
- optional `wait` metadata for follow-up consumers that generate wait-oriented
  commands from the shared schema
- `wait.timeout` carries duration-style timeout metadata for those consumers so
  they can render the correct wait command or equivalent wait behavior from the
  same shared definition; the initial post-install validator consumer does not
  depend on it in v1
- `wait` is supplemental metadata, not a standalone check target; a component
  still needs an actual declared check primitive such as a selector, named
  workloads, `customChecks`, or `crds`

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

These examples show the intended split for the initial validator subset:

- use `readiness` when the component signal is primarily workload rollout
  health, while allowing the same object to carry additional consumer-specific
  readiness metadata over time
- use `crds` when the component signal is declared CRD establishment
- use `customChecks` only for the few cases where the signal is a
  component-specific status check that the generic model does not express well

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

The initial validator subset needs more than a selector plus a generic
Kubernetes `Ready` condition. It also needs exact named workload identities,
tightened `DaemonSet` semantics, declarative CRD establishment checks, and a
small set of component-specific status checks.

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

## Why Not Runtime Chainsaw As The Initial Default

Reusing Chainsaw at deployment-validator runtime was considered seriously because
it offers the best semantic reuse of the existing `recipes/checks/*` assets.
This ADR does **not** remove Chainsaw from the repository, deprecate the
existing repo-side Chainsaw health assets, or require rewriting current
Chainsaw-based tests and checks.

The ADR does not reject Chainsaw outright. It only stops short of making
Chainsaw the primary runtime for the **initial** post-install validator
consumer, because that would make `aicr validate` harder to operate and extend:

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
- Chainsaw is a strong testing and execution tool, but a weak canonical check
  language for consumers such as `#610` readiness Jobs that need to derive
  deployer-native behavior from structured intent rather than simply execute a
  test; those consumers would otherwise need to either run Chainsaw directly or
  parse executable `kind: Test` logic to recover semantics
- it couples validator behavior more tightly to **upstream Chainsaw semantics and upgrade cadence**
- it expands the maintenance and review surface because full Chainsaw `Test`
  execution brings scripts, waits, catches, and binary execution into runtime validation
- it is harder to unit test than typed Go readiness checks using fake clients
- it still needs explicit transport into validator Jobs because those jobs consume
  mounted `recipe.yaml`, not `registry.yaml`

Even if the internal evaluator changes later, the stable surface for consumers
should remain the shared check contract rather than exposing phase-specific
executor differences.

Chainsaw remains useful as:
- a repo-side health-check asset format
- a UAT / test workflow tool
- a source of parity reference when deciding whether the Go-based contract is too narrow
- a possible source of greater library-backed reuse in future follow-up work,
  even though it is not the primary validator runtime in the initial shape of
  this ADR

## Consequences

### Positive

- **Single source of truth**: deployment-related phases can converge on one
  component check definition instead of maintaining parallel readiness stories.
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
- **Fixed special-case surface**: new bespoke behavior requires coordinating Go
  and registry changes rather than arbitrary registry-defined assertions.
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
- registry mappings for the current component inventory
- follow-up deploy / cleanup consumers that read the same definitions rather
  than introducing a second per-component check inventory

At the time of writing this ADR, these details describe design intent rather
than current `main`. In particular, `ComponentConfig` / `ComponentRef`
component check metadata and the associated hydration path are planned follow-up code
changes, not already-exposed fields in `pkg/recipe/metadata.go`.

The broader direction is a generic, phase-aware deployment-check schema that
can be consumed by post-install validation and by deploy / cleanup flows.
Resource types or metadata beyond the initial validator subset can be layered
onto the same schema over time without introducing separate per-component
inventories.

The rollout is intentionally sequenced in two phases:

- **Phase 1: migrate the current inventory**
  - add the component contract fields
  - hydrate them into the resolved recipe
  - populate the current **21-component** registry inventory
  - allow temporary overlap with `expectedResources` during migration
- **Phase 2: enforce the steady-state rule for newly added components**
  - once the current migration inventory is complete, `pkg/recipe` validation
    surfaced through existing repo CI or lint should reject newly added
    components that declare none of:
    - `readiness` that declares namespace-scoped workload coverage, optionally
      refined by a selector or named workloads; `wait.timeout` metadata alone
      does not satisfy this rule
    - `customChecks`
    - `crds`
    and should not be implemented as additional runtime deployment-validator
    behavior or as a separate standalone validator workflow

## Alternatives Considered

### 1. Keep adding bespoke Go checks per component

Rejected because it keeps deep readiness asymmetric across the registry and does
not produce an enforceable component contract.

### 2. Reuse Chainsaw health checks directly in deployment validation

Not chosen as the initial default because it reintroduces Chainsaw as a second
runtime inside `aicr validate`, even though the current deployment validator
can start with a typed Go model over registry-defined checks. Chainsaw remains
valuable as a repo-side asset format and parity reference, and future
implementations may choose greater library-backed or runtime reuse if that
reduces duplication without increasing long-term maintenance cost too much.

### 3. Namespace-wide heuristics with no registry contract

Rejected because shared namespaces such as `kube-system` and `monitoring` need
explicit scoping or exact identities to avoid false attribution and brittle behavior.

## References

- [ADR-002: Replace Go Test Validation Engine with Container-Per-Validator Model](002-validatorv2-adr.md)
- [Issue #610](https://github.com/NVIDIA/aicr/issues/610) — bridge jobs for deploy/undeploy lifecycle flow control
- [Issue #607](https://github.com/NVIDIA/aicr/issues/607) — narrow GPU-scoped deployment readiness gap
- [PR #611](https://github.com/NVIDIA/aicr/pull/611) — targeted GPU readiness groundwork
- [Issue #622](https://github.com/NVIDIA/aicr/issues/622) — broader registry-driven deployment readiness follow-up
