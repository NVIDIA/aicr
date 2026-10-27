# ADR-025: NVIDIA Cluster Readiness Engine Component

## Status

**Proposed** — 2026-09-02. Amended 2026-10-26: AICR's own run limits satisfy
the node-cap, run-deadline, and cleanup bullets of the execution-safety gate, so
upstream bounds no longer gate stock adoption;
see [Amendment: AICR's limits](#amendment-aicrs-limits).

Reviews upstream
[`NVIDIA/cluster-readiness-engine`](https://github.com/NVIDIA/cluster-readiness-engine)
v0.1.0, published 2026-09-01 at source commit `45726bfe`. Implementation
follows acceptance as a separate change; see
[PR #2524](https://github.com/NVIDIA/aicr/pull/2524).

## Decision Summary

AICR admits `nvcre` as an optional, opt-in Helm component. The first
implementation is **registry-only**: the entry will exist, but no stock recipe
will reference it, and custom or external recipes must declare a
`ComponentRef` explicitly. Recipes that do not declare it — including every
stock recipe — are unchanged and acquire none of its CRDs, RBAC, or runtime
cost. No registry entry is added by this ADR; it is proposed here and
implemented separately.

This ADR does not authorize stock-recipe adoption. That is a separate decision,
recorded as an amendment.

## Context

NVIDIA Cluster Readiness Engine (NVCRE, formerly Excalibur) is a
Kubernetes-native GPU cluster burn-in certification controller. It runs real
training and NCCL communication workloads across topology-aware node groups,
measures goodput and bandwidth, and bisects failing groups to isolate
individual bad nodes. v0.1.0 is its first public release.

The chart is `cluster-readiness-engine` at
`oci://ghcr.io/nvidia/cluster-readiness-engine`. It ships seven CRDs under the
`nvcre.nvidia.com` group and four cluster-scoped `LogProfile` CRs.

The named first consumer is the internal DGX Cloud recipes repository, which
already runs NVCRE against AICR bundles through its own external overlay. A
proof of concept there produced one finding this ADR carries forward: the tuned
per-fabric configuration — image, `mpirun` path, fabric environment, and
per-platform runtime patches — lives only in the Certification workload
catalog. A generic `WorkloadRun` resolving through its own override set
measured ~3 GB/s over TCP on the same AWS H100 nodes where Certification
measured 489 GB/s over EFA; on AWS H100 that path did not use EFA at all.

`Certification` is therefore the integration surface. `WorkloadRun` is a
lower-level API for advanced cases and is not a certification path, so AICR
drives `Certification` and reads its report.

NVCRE reported a well-formed number for a run silently on the wrong transport,
so any AICR check built on its output must assert the expected network plugin
loaded, not merely that a number was produced.

## Decision

### 1. Registry-only adoption

The implementation adds a registry entry, component values, and a health check.
It does not touch `recipes/overlays/base.yaml`, any leaf overlay, or any mixin
used by stock recipes.

Opt-in scope follows the declaring overlay's `criteria`, not a single recipe,
so an adopter must scope that overlay narrowly and avoid `intent: any`.

### 2. Upstream ownership boundary

NVCRE owns the certification catalog and its per-platform tuning, fault
isolation, measurement and log parse rules, its seven APIs, and the Kubeflow
Trainer integration.

AICR owns release selection, the chart and image pins recorded in AICR, AICR
values and placement, health assertions, bundle rendering across deployers,
mirror and BOM coverage, and AICR-side qualification.

AICR consumes only public, versioned contracts — no upstream `internal/`
imports, no private chart, no republished upstream release under an AICR
identity.

### 3. Ordering contract

Two constraints are properties of the chart, not of any recipe.

A registry entry **records** these constraints; it cannot enforce them.
`ComponentConfig` has no dependency or ordering field, and `hasSelfRefCRDs`
does not help here — that flag covers a chart referencing CRDs it ships itself,
whereas both constraints below are cross-chart. Ordering is enforced only by
`ComponentRef.DependencyRefs`, declared on an overlay's `componentRefs` entry
and read by the helmfile bundler's DAG-stratified sub-helmfile layout.

**Any overlay referencing `nvcre` must therefore declare
`dependencyRefs: [kubeflow-trainer, prometheus-operator-crds]` on that
`componentRefs` entry**, dropping `prometheus-operator-crds` only where
`metrics.serviceMonitor.enabled` is left at AICR's `false`. An overlay that
omits them can render the `ServiceMonitor` before its CRDs exist, or submit a
`TrainJob` before Trainer is installed.

- **Kubeflow Trainer.** NVCRE creates `TrainJob`s against a `TrainingRuntime`
  and the chart does not install Trainer. AICR already supplies it via
  `recipes/mixins/platform-kubeflow.yaml`.
- **ServiceMonitor.** The chart renders one under
  `metrics.serviceMonitor.enabled`, whose chart default is `true` and requires
  the prometheus-operator CRDs. AICR values pin it to `false`, so a bare
  install needs no monitoring CRDs.

### 4. The registry entry does not enforce manager placement

The chart's Deployment template renders `affinity` and `tolerations` but has no
`nodeSelector` block, so a system node selector would be silently dropped. The
registry entry therefore declares `nodeScheduling.system.tolerationPaths` only.

**Tolerations are taint compatibility, not placement.** They permit scheduling
onto matching tainted nodes; they never select a node. The chart default is a
blanket `tolerations: [{operator: Exists}]`, which tolerates every taint in the
cluster — so by default nothing keeps the manager off GPU or general nodes.
Replacing that default with the recipe's system tolerations narrows which
taints are tolerated, but still does not confine the manager to system nodes.

An adopter that requires system-node isolation must set `manager.affinity`
explicitly, through component values or `--set-json` — it is object-valued and
is not settable through scalar `--set`. AICR injects no such affinity, so
placement isolation is the adopter's responsibility, not a property of the
registry entry.

Only the manager is in scope either way. NVCRE schedules its own benchmarks
through Kubeflow Trainer onto GPU nodes, and those must not inherit system
placement.

### 5. Non-vacuous health check

The check asserts the manager Deployment is ready, the `certifications`,
`workloadruns`, and `bandwidthmeasurements` CRDs are `Established`, and the
cluster-scoped `nccl-bandwidth` `LogProfile` is present — without it the
BandwidthMeasurement controller has no parse rules and `status.results[]` stays
empty.

Readiness asserts both `readyReplicas > 0` and `readyReplicas == replicas`, so
a partially rolled-out Deployment does not pass. The first conjunct is what
fails closed: `readyReplicas` is `omitempty` and therefore absent on a
Deployment that has never had a ready pod, where `unavailableReplicas == 0`
would pass vacuously.

The check is read-only and creates no benchmark workload.

### 6. AICR bounds Certification execution

NVCRE's API bounds neither the node footprint of a run nor its total duration,
and deleting a `Certification` does not establish that its GPU workloads have
stopped. Until upstream closes these, an AICR caller supplies the bounds and
verifies the outcome itself.

The rules below are stated against the **API**, because the opt-in validators
this ADR places in scope create the CR directly. `nvcrectl` flags are a
convenience wrapper over the same fields, not the contract.

**Cap the footprint with `target.nodeNames`.** Setting `nodesPerJob` is not
sufficient and does not bound anything at the run level: partitioning chunks
the entire matched node list and adds an overflow group that borrows from the
last full group, so `nodesPerJob` sizes each group while the run still spans
every targeted node. `execution.maxConcurrent` defaults to `0`, meaning
unlimited, so those groups can all run at once. On a 64-node target,
`nodesPerJob: 2` yields 32 groups covering the fleet. An explicit
`nodesPerJob` is clamped to `min(nodesPerJob, matchingNodes)`, which is a
per-job ceiling, not a total-footprint bound. `target.nodeNames` is the only
field that caps the total, so an AICR caller sets it to an explicit node list.

**Bound the wait, and treat expiry as failure.** A timeout yields a partial
report and is reported as a failure, never as a pass.

**Verify the workloads actually stopped.** Deleting the `Certification` is not
proof. The delete carries no propagation policy; the controller issues its
child deletes and removes its own finalizer in the same reconcile without
observing that they completed, so the parent can disappear while `TrainJob`s
and pods still hold GPUs. The controller's drain barrier gives up by design
after a five-minute grace period and proceeds with cleanup regardless. On
success, failure, cancellation, or timeout the caller must therefore confirm
directly that the run's `TrainJob`s and GPU pods are gone within a bounded
wait, and report cleanup failure — not a warning, and never a pass — when they
are not.

An AICR check that omits any of these can strand GPU capacity indefinitely,
which is why they are stated here rather than left to each caller.

## Adoption Gates

These gates bound **stock-overlay adoption**, not registry-only admission.

Registry-only admission proceeds on the current pin: the entry is inert until
an overlay declares a `ComponentRef`, so an unmet gate cannot reach a stock
recipe. Gates not yet met on the reviewed pin are recorded in
[Status](#status-against-v010) and tracked in
[Follow-Up Decisions](#follow-up-decisions); none of them blocks the
registry-only implementation. The full set must pass before the stock-adoption
amendment, except the node-cap, run-deadline, and cleanup bullets that AICR's own
limits meet; see [Amendment: AICR's limits](#amendment-aicrs-limits).

The category structure follows
[ADR-019](019-k8s-aibom-runtime-inventory.md), which set the
registry-admission precedent. *Release and supply chain* and *AICR
qualification* are relied on as ADR-019 defines them. *Chart and CRD
lifecycle* and *Security* are narrowed to NVCRE's surface, dropping the
AIBOM-specific data-handling clauses. *Benchmark execution safety* has no
ADR-019 analogue — k8s-aibom observes workloads, while NVCRE creates them.

**Release and supply chain**

- Source tag, chart, and image are one coherent release at the same version.
- The image is immutable, multi-architecture, and selectable by digest through
  public values without patching the chart.
- Signature, SBOM, and build provenance cover both the image and the chart.
  Image attestations bind the qualified image digest; chart attestations bind
  the qualified chart digest — not a tag, and not the image digest. Each names
  the source release it was built from and verifies against the upstream
  release workflow's signer identity.
- No floating tag, branch, `latest`, or locally built artifact appears in the
  component definition.

**Chart and CRD lifecycle**

- `helm lint` and render succeed for the selected release.
- The seven CRDs are established before the four `LogProfile` CRs apply. The
  chart ships both, so `hasSelfRefCRDs: true` is required on the registry entry
  to bypass the helm-diff live-mapper check on a fresh cluster (issue #914).
- Ownership of the cluster-scoped `LogProfile` CRs on install, upgrade, and
  uninstall is documented — they are not namespaced by release.
- The Decision 3 ordering constraints are exercised, not merely documented.

**Benchmark execution safety**

- A stated maximum node count and run deadline per certification. Met by AICR's
  own limits, not by an upstream bound; see
  [Amendment: AICR's limits](#amendment-aicrs-limits). The deadline is the
  validator's in-process wait, so it holds while the validator pod lives: a
  killed pod is bounded only by CRE's `spec.timeoutPerJob`, whose clock restarts
  when a workload is replaced (gap 4), and by the orphan reclaim below.
- Documented `TrainJob` cleanup on completion, failure, and cancellation. Met
  the same way while the validator pod lives: the caller confirms the workloads
  stopped. A killed pod is covered only by the orphan reclaim, which needs a
  later CRE check in the same namespace and an orphan at least 45 minutes old.
- A bounded termination condition for adaptive fault isolation's bisection,
  in both total runs and wall-clock.
- The controller creates no workload without an explicit `Certification`,
  `WorkloadRun`, or `Workflow` resource.
- **The workload runtime closure is inventoried, digest-qualified, and
  relocatable.** The catalog is `go:embed`-compiled into the manager, so its
  contents never appear in rendered chart output and AICR's mirror discovery —
  which extracts images from rendered YAML — cannot see them. Catalog entries
  carry tag-only images, including a `:latest` pin in one platform-specific
  runtime patch, and the training entry clones Megatron-LM at pod start. Every
  image, fetched source, and runtime download reachable from a supported
  `Certification` category and platform path must be discoverable, pinned by
  digest, and mirrorable. Scope is the supported paths, not every dormant
  catalog entry. This does not bind registry-only admission — the controller
  creates nothing without an explicit CR — but it binds before any opt-in
  validator ships, which is why it sits in the gate set.

**Security**

- Rendered RBAC is minimal for the controller's function. The chart ships a
  manager `ClusterRole` plus admin, editor, and viewer `ClusterRole`s for each
  of the seven CRDs.
- Pod security is non-root with a read-only root filesystem and all
  capabilities dropped.
- No credential dependency by default.

**AICR qualification**

- The component renders correctly across every supported deployer.
- Mirror and BOM coverage include the controller image.
- The health check is non-vacuous and read-only, per Decision 5.
- A representative certification runs to completion on real GPU hardware and
  reports the expected transport.

### Status against v0.1.0

| Artifact | Digest |
|---|---|
| Chart `v0.1.0` | `sha256:a9c7f23753dc4fafccba8b600644af265fa26a13bf95748392f37d010635d02c` |
| Image index `manager:v0.1.0` | `sha256:ed1e5928d9658988a18fe253b2dbaee729cf4ce14da368511b113f96a8bf07a0` |

Verified: a keyless cosign signature on the image against OIDC issuer
`https://token.actions.githubusercontent.com` with a certificate identity under
`https://github.com/NVIDIA/cluster-readiness-engine/`; a CycloneDX SBOM
attestation binding the image digest; `linux/amd64` and `linux/arm64`; coherent
chart `version` and `appVersion`; digest pinning through public values by
setting `manager.image.tag` to `v0.1.0@sha256:ed1e5928…`; and anonymous public
pull for both chart and image.

Four gates are unmet on this pin. None blocks registry-only admission; all are
tracked in [Follow-Up Decisions](#follow-up-decisions). Gaps 1 and 2 must close
before the stock-adoption amendment. Gaps 3 and 4 are met by AICR's own limits
(see [Amendment: AICR's limits](#amendment-aicrs-limits)) and do not block
stock adoption.

Supply chain — upstream release-workflow changes, not AICR work:

1. **No build provenance on the image.** The only attestation predicates
   present are `cyclonedx.org/bom` and `sigstore.dev/cosign/sign/v1`; a
   `slsaprovenance` query returns no match.
2. **No supply-chain artifacts on the chart.** `cosign tree` against the chart
   reports none — no signature, SBOM, or provenance. The chart is published by
   a `helm push` with no signing step.

#### Gap 1 closes at the shipped pin and gap 2 partly

The findings above are v0.1.0. Rechecked at the pin AICR ships:

| Artifact | Digest |
|---|---|
| Chart `v0.8.2` | `sha256:75458557b0ff2610c7eea01e0b315ae0355cc6b2baa816756a2dba0c0c2387a4` |
| Image index `manager:v0.8.2` | `sha256:c5de16ddb95bd923c6b04d04d7ff9754a7d7b83d18fa400baa33e7ae72fc593d` |

Both carry a cosign signature and a SLSA v1.0 provenance attestation, and both
verify against certificate identity
`https://github.com/NVIDIA/cluster-readiness-engine/.github/workflows/attest.yml@refs/tags/v0.8.2`
at the GitHub Actions OIDC issuer. The chart attestations bind the chart digest
— confirmed by running `cosign tree` against the digest rather than the tag.
Anonymous `helm pull` succeeds with an empty registry config, so no pull secret
is required.

Two details make the recheck non-obvious:

- `cosign verify-attestation --type slsaprovenance` **fails** on this pin. That
  shorthand resolves to SLSA v0.2 and the predicate here is
  `https://slsa.dev/provenance/v1`; use `--type slsaprovenance1`. A query by the
  shorthand reports no provenance on an image that has it.
- SBOMs bind the per-platform children, not the index. `cosign tree` on the
  index shows only the signature and provenance; the CycloneDX attestation is on
  `linux/amd64` and `linux/arm64` individually.

Gap 2 therefore closes only in part. The release-and-supply-chain gate above
asks for signature, SBOM, **and** provenance on both artifacts; the chart has
the first and third and no SBOM at all, so that gate still does not pass and
follow-up 2 stays open.

Execution safety — these live in the `Certification` API, so driving
`Certification` rather than `WorkloadRun` does not close them:

3. **No bound on a run's node footprint.** `CategoryOptions.NodesPerJob`
   carries `Minimum=1` and no maximum, but it sizes each group rather than the
   run: partitioning covers the whole matched node list, and
   `execution.maxConcurrent` defaults to `0` (unlimited). Only
   `target.nodeNames` caps the total.
4. **No total run deadline, and no cleanup guarantee.** `CertificationSpec`
   exposes only `timeoutPerJob` and `measurementTimeout`; `ExecutionSpec` adds
   `maxConcurrent` but no aggregate bound. Deleting a `Certification` does not
   confirm its workloads stopped — the delete carries no propagation policy,
   the controller drops its finalizer without observing child deletion, and
   the pod-drain barrier proceeds after a five-minute grace period regardless.

AICR bounds items 3 and 4 from the caller side per
[Decision 6](#6-aicr-bounds-certification-execution), which is what makes them
non-blocking here rather than merely deferred. The
[amendment below](#amendment-aicrs-limits) makes that permanent for the gate:
they stay upstream asks, and neither holds up stock adoption.

This list is not a full adjudication. The remaining gate categories —
Security, and the parts of Chart and CRD lifecycle and AICR qualification not
named above — are neither verified nor listed as gaps; they are adjudicated at
amendment time. Unassessed is not the same as passed. Nothing turns on this
for registry-only admission, since the entry stays inert until an overlay
references it.

## Amendment: AICR's limits

Gaps 3 and 4 above ask NVCRE for a run-level node cap, a
total run deadline, and a cleanup guarantee. AICR does not wait for them. Every
opt-in check that drives a `Certification` carries the limits below, each
enforced in the validator and none depending on a future NVCRE release:

| Limit | Value | Where |
|---|---|---|
| Node footprint | An explicit `target.nodeNames` list, capped at the node count each combination was qualified at (two on `eks` × `h100`); an empty list is refused | `creQualifiedEntries`, `buildCRECertification` |
| Wait for a terminal state | 30 minutes from creation, expiry reported as a failure; the same value is written to `spec.timeoutPerJob` as a backstop | `defaults.CRECertificationTimeout` |
| Measurement finalization | 5 minutes after the `Certification`'s terminal state is observed; expiry fails the check rather than falling back to provisional results | `defaults.CREMeasurementFinalizeTimeout` |
| Teardown | 10 minutes to delete the `Certification` and confirm its `TrainJob`s and GPU pods are gone; an unconfirmed teardown fails the check | `defaults.CRECertificationTeardownTimeout` |
| Orphans | A `Certification` labeled `app.kubernetes.io/managed-by=aicr-validator` and older than the sum of the three bounds above (45 minutes) is reclaimed by the next CRE check in the same namespace that reaches the reclaim step: one with a constraint, a qualified combination, and enough nodes | `defaults.CREOrphanReclaimAge` |

These satisfy the first two *Benchmark execution safety* bullets for a check
that observes them, while its validator pod lives. A pod that is killed never
runs its wait or its teardown, so for that case the run deadline falls back to
CRE's `spec.timeoutPerJob` (whose clock restarts when a workload is replaced)
and the cleanup bullet rests on the orphan reclaim alone, which needs a later
CRE check in the same namespace that gets as far as the reclaim step.

The gate attaches to the check, not to the pin: a new opt-in validator that
omits a limit does not meet it. Upstream bounds remain
welcome and stay tracked as follow-ups 3 and 4, but they are no longer
conditions for the stock-adoption amendment.

This amendment does not touch the other gate categories. The bisection
termination bullet is neither asserted nor claimed here, the chart SBOM (follow-up
2) is still absent, and *Security* and the unlisted parts of *Chart and CRD
lifecycle* and *AICR qualification* are still adjudicated at amendment time.

## Non-Goals

- Adding `nvcre` to any stock recipe.
- Introducing certification results into [ADR-007](007-recipe-evidence.md)
  recipe evidence.
- Vendoring, patching, or republishing the upstream chart.
- Replacing the existing NCCL bandwidth checks on the current TrainJob path.

Opt-in AICR validators that drive `Certification` are **in scope** and are not
a non-goal. They stay off stock overlays, assert the expected transport per
[Context](#context), and observe Decision 6. Flipping any overlay to them is
the separate stock-adoption amendment.

## Consequences

**Positive.** Stock recipes are unchanged, so no existing user acquires
NVCRE's CRDs, cluster-scoped RBAC, or runtime cost. Registry-only admission
does not wait on upstream, so opt-in adopters get the component now. All four
open gaps are named concretely with verifiable close conditions.

**Negative.** AICR carries a component whose pin does not yet pass every gate.
Two of the four gaps are mitigated only by caller discipline (Decision 6), so
an opt-in validator that ignores it can strand GPU capacity — a risk that
exists until upstream adds a run-level node-footprint cap, a run deadline, and a
deletion that confirms child workloads stopped. A validator that observes
Decision 6 can still strand it when its pod is killed, until a later CRE check
in the same namespace reclaims the orphan. Since the amendment, the node cap,
run deadline, and cleanup bullets no longer block stock adoption for a
validator that observes them, because AICR's own limits satisfy the gate; the
chart SBOM, the runtime closure (follow-up 5), and the gates still unassessed
do, so the PoC's measured value is not yet available to stock recipes.

## Follow-Up Decisions

1. ~~Upstream: add build provenance to the controller image, attributable to
   the release workflow and meeting a stated SLSA build level.~~ Closed at
   v0.2.0 and still verifying at the current v0.8.2 pin — SLSA v1.0 provenance
   verifies against the release workflow identity.
2. Upstream: publish a chart SBOM. ~~Sign the Helm chart and publish chart
   provenance.~~ Signature and SLSA v1.0 provenance have been present since
   v0.2.0 and bind the chart digest; the **SBOM** is still absent at v0.8.2,
   because the CycloneDX attestation covers the image's per-platform children
   only. Open, so item 6 is not yet reached.
3. Upstream: add a run-level cap on the node footprint, so a `Certification`
   cannot span every matched node without an explicit `target.nodeNames`. Not
   a stock-adoption gate; AICR's own cap applies.
4. Upstream: add a total run deadline to `CertificationSpec`, and make
   deletion establish that child workloads stopped — foreground propagation,
   a finalizer held until children are gone, or an equivalent guarantee. Not
   a stock-adoption gate; AICR's own teardown confirmation applies.
5. Upstream: publish the workload runtime closure as digest-pinned,
   discoverable artifacts, so the catalog's images and fetched source can be
   inventoried and mirrored. AICR now derives the closure for the supported
   paths itself (`recipes/components/nvcre/workload-images.yaml`), which makes
   the images mirrorable but does not remove the upstream ask: the inventory
   has to be re-derived on every chart bump because the catalog ships inside
   the manager image.
6. AICR: record the qualified artifact set in this ADR's Status once items 1, 2,
   and 5 close, and re-run the gates against that pin. Items 3 and 4 are not
   conditions.
7. AICR: a separate amendment for any stock-recipe adoption, which requires
   the full gate set to pass.
