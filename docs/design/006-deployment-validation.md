# ADR-006: Registry-Driven Deployment Validation

## Status

**Proposed** — 2026-04-20
**Revised** — 2026-04-21 (contract clarifications, rollout sequencing, and related design coordination)

This PR records the design only; it does not include the implementation.
If accepted, the primary implementation is planned for `pkg/recipe/` and
`validators/deployment/`. The steady-state CI guard for newly added components
remains a follow-up.

## Scope

This ADR covers the **deployment** phase of `aicr validate`.

It defines:
- how component deployment-readiness metadata is expressed
- where that metadata is transported in the resolved recipe
- how the deployment validator interprets it
- why AICR uses a Kubernetes-native, pure-Go runtime for this phase instead of
  reusing Chainsaw at validator runtime

It does **not** change:
- readiness constraint evaluation before deployment validation
- performance or conformance phase runtime design
- repo-side Chainsaw health-check workflows used in tests and component health assets
- the long-term deprecation or retention plan for `expectedResources`

## Problem

After the targeted GPU readiness work in [`#611`](https://github.com/NVIDIA/aicr/pull/611),
deployment validation was stronger but still asymmetric:

- baseline namespace and `expectedResources` checks ran for enabled components
- bespoke deep checks existed only for a few GPU-specific gaps
- many components still had only shallow validation coverage
- adding a new component did not naturally force a deep deployment-validation story

This created three problems:

1. **Coverage drift**: deep deployment validation depended partly on Go code,
   partly on ad hoc `expectedResources`, and only partially on registry metadata.
2. **Asymmetry across the registry**: shared-namespace components and
   CRD-owning components could not be expressed cleanly through the existing
   deployment validator contract.
3. **Weak extensibility**: the project needed a typed contract that could scale
   across the current component inventory and future additions.

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

This ADR had to decide whether deployment validation should:
- adopt the same Kubernetes-native pure-Go model already used by conformance, or
- reintroduce Chainsaw as a runtime dependency inside `aicr validate --phase deployment`

Two additional constraints shaped the decision:

- **Deployer-neutrality**: deployment validation must not depend on Helm APIs,
  release metadata, or release-scoped labels such as `app.kubernetes.io/instance`.
- **Resolved-recipe transport**: validator Jobs consume mounted `recipe.yaml`,
  not `registry.yaml`, so any deployment-readiness contract has to travel
  through the resolved recipe.

There is intentional overlap with the deployment half of
[`#610`](https://github.com/NVIDIA/aicr/issues/610): both discussions touch the
per-component readiness intent. The scopes remain different:

- `#610` is about **deploy-time flow control and undeploy-time cleanup**
  through deployer-specific hooks, sync-wave ordering, bridge Jobs, images,
  RBAC, and cleanup behavior
- this ADR is about **post-install validation** in
  `aicr validate --phase deployment`, using a deployer-neutral, pure-Go,
  Kubernetes-API-based contract

The goal is to coordinate semantics and component mappings where that stays
simple, without forcing the validator contract to absorb hook-job details that
belong to deploy-time orchestration.

## Options Summary

| Option | Summary | Benefits | Costs |
| --- | --- | --- | --- |
| **Registry fields + pure Go runtime** | Add three component-level fields (`readiness`, `customChecks`, `crds`) and have the deployment validator run the declared checks across all enabled components selected into the resolved recipe | Typed contract, deployer-neutral, easy to unit test, aligns with current `aicr validate` architecture, keeps the validator Kubernetes-native and extensible | Narrower than full Chainsaw expressiveness; some semantic overlap with repo-side health checks |
| **Reuse Chainsaw at runtime** | Hydrate `healthCheck.assertFile` into validator jobs and run Chainsaw semantics during deployment validation, either through the Go assertion library where possible or full test execution where needed | Best parity with existing `recipes/checks/*`; strongest reuse of current assertions | Second runtime/model inside `aicr validate`, harder testing, upstream coupling, and for the current `kind: Test` inventory still either binary packaging or asset rewrites |
| **Keep bespoke per-component Go checks** | Continue adding code paths per component as gaps are found | No schema work | Does not scale; keeps coverage asymmetric and harder to enforce |

## Decision

Adopt **registry-driven deployment validation implemented in pure Go**.

Deployment-phase deep checks are declared in `recipes/registry.yaml`, hydrated
onto each resolved `ComponentRef`, and interpreted by the deployment validator
using Kubernetes API state only.

The schema adds three component-level fields — `readiness`, `customChecks`, and
`crds` — so each component can declare its own deployment check contract. The
validator then runs those declared checks across all enabled components in the
resolved recipe, on a pure-Go path that can run in any Kubernetes cluster and
extend naturally toward broader CNCF AI Conformance validation.

This option was chosen because it creates a typed, enforceable
deployment-readiness contract in `recipes/registry.yaml`, keeps validator jobs
aligned with the resolved recipe as the single runtime input, applies declared
checks across all enabled components, and preserves one pure-Go validator model
that is easy to unit test and extend toward broader **CNCF AI Conformance**
validation.

### Contract

The component readiness contract is:

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

The supported coverage shapes are:

- `readiness`
  - required `namespace`
  - optional deployer-neutral `selector` using stable labels only;
    release-identity labels such as `app.kubernetes.io/instance` remain out of
    bounds
  - optional exact `workloads` using `{kind, name}`
  - `workloads.kind` accepts only the exact-case enum values `Deployment`,
    `DaemonSet`, and `StatefulSet`
  - `workloads` items do not carry their own namespace; `readiness.namespace`
    is authoritative in v1
- sibling `customChecks`
- top-level `crds`

Under this design, these fields are hydrated onto each resolved `ComponentRef`
in `RecipeResult`. The deployment validator reads only mounted `recipe.yaml`;
it does not read `registry.yaml` directly at runtime.

Overlay and external-data merge policy is replace-if-set for all three fields:

- `readiness`: replace the whole object if set; otherwise inherit
- `customChecks`: replace the whole list if set; otherwise inherit
- `crds`: replace the whole list if set; otherwise inherit

### Readiness primitives

The deployment validator uses a narrow, stable v1 contract:

- `Deployment`: `availableReplicas >= desiredReplicas`, with nil replicas treated
  as Kubernetes default `1`; explicit `0` is allowed
- `StatefulSet`: `readyReplicas >= desiredReplicas`, with nil replicas treated
  as Kubernetes default `1`; explicit `0` is allowed
- `DaemonSet`: `desiredNumberScheduled > 0` and `numberReady == desiredNumberScheduled`
- `CustomResourceDefinition`: `Established=True`

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
contract becomes the standard component-coverage path. Its long-term retention
or deprecation remains out of scope for this ADR; see Scope above.

Because `expectedResources` and generic readiness share the same typed workload
health primitive, the tightened `DaemonSet` rule above applies to both paths.

## Why Not Runtime Chainsaw

Reusing Chainsaw at deployment-validator runtime was considered seriously because
it offers the best semantic reuse of the existing `recipes/checks/*` assets.

It was not chosen for this ADR because it would make `aicr validate` harder to
operate and extend:

- it introduces a **second validator runtime** inside `aicr validate`
  - pure Go / Kubernetes-client checks for conformance
  - Chainsaw-driven execution for deployment
- even with greater library-backed reuse, the current repo-side health-check
  inventory is still authored as full `kind: Test` assets, so direct reuse
  would still require either binary support for full test execution, including
  a packaging change to the current distroless deployment-validator image, or
  rewriting those assets into a narrower form
- it couples validator behavior more tightly to **upstream Chainsaw semantics and upgrade cadence**
- it expands the maintenance and review surface because full Chainsaw `Test`
  execution brings scripts, waits, catches, and binary execution into runtime validation
- it is harder to unit test than typed Go readiness checks using fake clients
- it still needs explicit transport into validator Jobs because those jobs consume
  mounted `recipe.yaml`, not `registry.yaml`

Chainsaw remains useful as:
- a repo-side health-check asset format
- a UAT / test workflow tool
- a source of parity reference when deciding whether the Go-based contract is too narrow

## Consequences

### Positive

- **Symmetric deployment coverage**: every current component can be mapped to
  `readiness`, `customChecks`, or `crds`.
- **Typed, reviewable metadata**: deep deployment-validation behavior becomes
  visible in the component registry rather than hidden in code-only paths.
- **Architecture consistency**: deployment and conformance both stay on a
  Kubernetes-native pure-Go validator model.
- **Extension path**: future CNCF AI Conformance validation can build on the same
  validator model instead of mixing Go checks with a second runtime engine.
- **Good testability**: workload discovery, zero-match behavior, CRD checks, and
  custom-check execution can all be exercised with fake clients.

### Negative

- **Narrower than Chainsaw**: the deployment contract does not try to reproduce
  full Chainsaw expressiveness or pod-phase parity.
- **Potential semantic overlap**: some repo-side Chainsaw health checks and the
  deployment contract may cover similar ground separately.
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

## Implementation Notes

The target implementation associated with this ADR will include:

- readiness metadata on `ComponentConfig` and `ComponentRef`
- hydration of readiness metadata into the resolved recipe
- overlay/external-data merge rules for `readiness`, `customChecks`, and `crds`
- deployment-validator support for:
  - selector-based discovery
  - exact workload identities
  - union + dedup semantics
  - fixed custom checks
  - CRD `Established=True` readiness
  - fail-closed zero-match behavior
- registry mappings for the current component inventory

At the time of writing this ADR, these details describe design intent rather
than current `main`. In particular, `ComponentConfig` / `ComponentRef`
readiness metadata and the associated hydration path are planned follow-up code
changes, not already-exposed fields in `pkg/recipe/metadata.go`.

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
    - `readiness`
    - `customChecks`
    - `crds`
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
