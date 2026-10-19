# AICR Roadmap

AICR v1.0.0 made the day-0 path stable: snapshot, recipe, validate, and bundle
run behind compatibility-gated public surfaces, against a validated recipe
portfolio, with verifiable supply-chain artifacts. Work after v1 extends that
path in two directions: more GPU systems with proven configurations, and the
day-2 lifecycle of deploying, validating, recovering, and changing a running
cluster.

## How to read this roadmap

Each focus area states an outcome, the initiative or epic that tracks it, and
the groups of work that lead to it. Priority and sequencing change as work
lands and capacity shifts, so they live in the tracking issues rather than
here. This file changes when an outcome is added, met, or retired.

Listing work here is not a support claim. A recipe coordinate is Supported only
under the [maturity](#maturity) rules below.

## Focus areas

| Focus area | Outcome | Tracking |
|---|---|---|
| [Run more GPU systems with proven configurations](#run-more-gpu-systems-with-proven-configurations) | More GPU environments have reusable configurations with a clearly stated qualification depth | [#3153](https://github.com/NVIDIA/aicr/issues/3153) |
| [Let teams deploy their parts of the stack](#let-teams-deploy-their-parts-of-the-stack) | Runtime and operations teams deploy into one cluster from the same verified configuration | [#3156](https://github.com/NVIDIA/aicr/issues/3156) |
| [Know whether a cluster is ready for workloads](#know-whether-a-cluster-is-ready-for-workloads) | Users can tell whether a cluster is ready for a workload and see the evidence behind that answer | [#1041](https://github.com/NVIDIA/aicr/issues/1041) |
| [Recover unhealthy nodes safely](#recover-unhealthy-nodes-safely) | Operators respond to faults with retained evidence and a clear owner for every action | [#2623](https://github.com/NVIDIA/aicr/issues/2623) |
| [Change components without disrupting workloads](#change-components-without-disrupting-workloads) | Users change components with qualified transition guidance and trustworthy artifacts | [#3154](https://github.com/NVIDIA/aicr/issues/3154) |

### Run more GPU systems with proven configurations

Expand recipe coverage across exact GPU, service, and OS combinations, turn
topology into usable network configuration, and connect every support claim to
qualification evidence.

**Tracking:** initiative [#3153](https://github.com/NVIDIA/aicr/issues/3153),
with epic [#827](https://github.com/NVIDIA/aicr/issues/827) and initiative
[#3076](https://github.com/NVIDIA/aicr/issues/3076).
**Guides:** [recipe development](docs/contributor/recipe.md),
[coverage matrix](docs/user/coverage-matrix.md).

- **Platform and OS breadth.** Add and deepen service, OS, and managed-stack
  combinations, stating each combination's workload and profile scope.
- **Fabric configuration.** Generate network-operator configuration from
  declared or observed NIC topology
  ([#827](https://github.com/NVIDIA/aicr/issues/827)), and give EFA, TCPX, and
  NVLink/IMEX wiring clear ownership and dependency order.
- **Hardware qualification on reserved capacity.** Stand up qualification lanes
  on reserved GPU capacity across cloud providers, so new coordinates move from
  structural integration to hardware evidence
  ([#3076](https://github.com/NVIDIA/aicr/issues/3076)).
- **Per-accelerator driver configuration.** Let GPU driver versions and driver
  floors vary by accelerator instead of following one global pin.
- **VR200 tuning and validation.** Continue tuning and validating the VR200
  recipes on RKE2 beyond their Preview baseline.

**Boundaries:** A recognized criteria value does not imply qualified support.
Structural integration and hardware qualification are separate acceptance
steps, and each added combination names its owner, test access, and continuing
test cadence.

### Let teams deploy their parts of the stack

Produce coordinated core and operations bundles from one resolved recipe. Keep
ownership unique, preserve dependency order, and verify the complete set before
either team deploys its part.

**Tracking:** epic [#3156](https://github.com/NVIDIA/aicr/issues/3156).
**Guides:** [ADR-018](docs/design/018-class-partitioned-bundles.md),
[bundling](docs/user/bundling.md).

- **Core and operations boundary.** Define a dependency-closed split, agreed
  with the first consumer, and guard it so it stays closed.
- **Verified bundle set.** Validate the complete recipe before partitioning it,
  give every component and release exactly one owner, and publish the set
  atomically under checksums that cover all of it.
- **Qualified co-deployment.** Prove that core deploys before operations,
  including negative ownership, ordering, and tamper cases, and that non-split
  bundles behave as before.

**Boundaries:** This outcome covers Helm-local output and a same-generation
pair only. Additional classes, GitOps split output, OCI publication, independent
class artifacts, and mixed-generation compatibility are outside it.

### Know whether a cluster is ready for workloads

Run checks against the configuration and workload actually deployed, and show
the evidence behind every verdict. Bound NVIDIA Cluster Readiness Engine
(NVCRE) execution, keep runtime artifacts and results reproducible, and qualify
selected workload and scale combinations with explicit evidence.

**Tracking:** initiative [#1041](https://github.com/NVIDIA/aicr/issues/1041),
with epics [#3155](https://github.com/NVIDIA/aicr/issues/3155),
[#2683](https://github.com/NVIDIA/aicr/issues/2683),
[#2427](https://github.com/NVIDIA/aicr/issues/2427), and
[#1044](https://github.com/NVIDIA/aicr/issues/1044).
**Guides:** [validation](docs/user/validation.md).

- **Trustworthy execution and evidence.** Execution errors, cleanup failures,
  incomplete results, and missing prerequisites never read as a pass. Evidence
  identities describe the recipe and workload actually checked, and
  qualification and contributor test paths stay bounded and reproducible
  ([#3155](https://github.com/NVIDIA/aicr/issues/3155)).
- **Performance and goodput validation.** Opt-in NVCRE-backed NCCL and training
  checks, with AICR bounding node footprint, run duration, and cleanup. The
  workload runtime is pinned and reproducible, thresholds are calibrated per
  service, accelerator, and topology, and results are published to the
  validation dashboard ([#2683](https://github.com/NVIDIA/aicr/issues/2683)).
- **Scale qualification.** A scale-qualified configuration contract, component
  defaults requalified for clusters beyond 2,000 nodes, scale preflight checks,
  and a dedicated qualification lane
  ([#2427](https://github.com/NVIDIA/aicr/issues/2427)).
- **Workload coverage.** Performance and conformance checks that cover every
  shipped workload variant, including NIM, Slurm, and Dynamo
  ([#1044](https://github.com/NVIDIA/aicr/issues/1044)).

**Boundaries:** Installing and running NVCRE is never implicit; a recipe opts
in explicitly. A qualified lane covers the combinations it
tested; it does not retire existing checks or stand in for full-scale
qualification.

### Recover unhealthy nodes safely

Preserve monitoring-only defaults and make each state-changing action explicit.
Give every action one operational owner, collect diagnostics before destructive
steps, and qualify platform-specific detection and recovery behavior.

**Tracking:** epic [#2623](https://github.com/NVIDIA/aicr/issues/2623).
**Guides:** [component catalog](docs/user/component-catalog.md).

- **Detection and prevention.** Detect GPU, NIC, fabric, operator, and
  scheduler faults, keep unhealthy nodes from starting workloads, and prove the
  health pipeline with fault injection.
- **Controlled response.** Advance through observe, quarantine, drain, and
  remediate only by explicit opt-in, with drain scope and timing owned
  explicitly and scheduler-managed nodes drained through their scheduler.
- **Diagnosis before action.** Capture diagnostics before destructive
  remediation, correlate stored health events, and detect repeated remediation.
- **Node admission and return to service.** Validate new nodes before they take
  work, and remediated nodes before they return to service.
- **Integration with external systems.** Hand nodes and health events to
  external systems under explicit ownership contracts, and act on provider
  maintenance and node-repair signals.
- **Platform parity.** Make each detection and recovery path work, or disable
  cleanly, on every supported platform.

**Boundaries:** Existing recipes stay monitoring-only unless an operator opts
in. Destructive or provider-specific paths are documented as supported only
after real-cluster qualification.

### Change components without disrupting workloads

Compare proposed changes with installed components, preserve resource identity,
verify artifacts, and test upgrade and rollback, so guidance and evidence
describe the versions actually deployed.

**Tracking:** initiative [#3154](https://github.com/NVIDIA/aicr/issues/3154),
with epic [#2424](https://github.com/NVIDIA/aicr/issues/2424).
**Guides:** [ADR-021](docs/design/021-component-upgrade-safety.md),
[upgrade records](docs/contributor/upgrade-records.md).

- **Resource continuity.** Upgrades and removals keep the CRDs, node labels, and
  in-place-managed resources that running workloads depend on.
- **Accurate transition guidance.** Every pinned version carries a transition
  record, upgrade guidance names only reachable paths, and version
  recommendations exclude stale or retracted releases.
- **Qualified upgrade and rollback.** Synthetic and release-to-release upgrade
  and rollback lanes, pre-migration releases for transitions that need hooks,
  and tested Kubernetes compatibility boundaries for every stack component.
- **Deployed-state awareness.** Report drift between deployed and pinned
  versions, and pin deployment identity at first deploy so registry defaults
  can move safely.
- **Artifact trust.** Tie signatures, digests, vulnerability data, and evidence
  to the exact artifact or transition assessed.

**Boundaries:** A tested transition supports only that pair; broader version
ranges stay unqualified until tested. Hook-based migration is added only for a
concrete component transition that needs it.

## Established in v1

These foundations shipped in v1.0.0. They stay in force and keep improving, but
they are no longer open roadmap outcomes.

### API stability

Each public surface has a committed compatibility baseline and a gate that
fails on unintended breakage. The gates run in `make qualify` and the merge
gate.

| Surface | Baseline and gate |
|---|---|
| CLI flags and subcommands | `pkg/cli/testdata/cli-surface.golden`, gated by `pkg/cli/surface_test.go` |
| REST API | `api/aicr/v1/server.baseline.yaml`, gated by `make openapi-diff` |
| Go SDK facade | `pkg/client/v1` exported surface, gated by `make api-diff` against the latest stable release |
| Bundle layout and artifact schemas | `pkg/bundler/testdata/layout/manifests/` and `api/aicr/v1/schemas/baseline/`, gated by `make test` |

An integrator can implement the snapshot, recipe, bundle, validate, and
evidence workflows through `github.com/NVIDIA/aicr/pkg/client/v1` without
importing another AICR `pkg/*` package. [RELEASE.md](RELEASE.md#deprecation-policy)
defines breaking changes and the deprecation policy for every surface; breaking
changes after v1 require a major version bump. Artifact maturity is a separate
axis, governed by [ADR-022](docs/design/022-artifact-maturity-and-deprecation.md).

### Maturity

Coverage is expressed as recipe coordinates across service, accelerator, OS,
intent, and platform. A file's presence alone is not evidence that a coordinate
works.

- **Supported:** the upstream recipe resolves and bundles, deploys on real
  hardware, passes its declared validation phases, and has published,
  verifiable evidence.
- **Preview:** the recipe path and evidence are useful for early adoption, but
  AICR does not yet make the complete production support and lifecycle
  commitment required for Supported status. Current Preview coordinates are
  listed in [recipe development](docs/integrator/recipe-development.md#current-preview-coordinates).
- **Planned:** tracked work that is not yet part of AICR's support contract.

[AICR Recipe Validation](https://validation.aicr.run/) is the canonical public
evidence channel. It publishes recipe coordinates, AICR and Kubernetes
versions, per-source and per-test results, evidence identities, historical
runs, and reproducible `aicr evidence verify` commands. Validation measures the
artifact a recipe actually ships, not a test-only fixture.

Evidence trust class is not part of the Supported definition. Supported
requires that the evidence signer is recorded, allowlisted in
`recipes/evidence/allowlist.yaml`, and verifiable, not that it is first-party.

### Supply chain

The supply-chain baseline is complete under
[#1149](https://github.com/NVIDIA/aicr/issues/1149): build provenance, bundle
and evidence signing and verification, offline and private trust modes,
KMS-backed verification, signed recipe data, verify-on-deploy guidance, and
end-user verification documentation. Binding artifacts and evidence to exactly
what was assessed continues under
[Change components without disrupting workloads](#change-components-without-disrupting-workloads).

### Contribution path

AICR keeps its user, integrator, and contributor documentation, recipe quality
checks, KWOK coverage, and community contribution path. Self-service
contributor checks, such as static recipe validation before a pull request,
continue under epic [#3155](https://github.com/NVIDIA/aicr/issues/3155).
