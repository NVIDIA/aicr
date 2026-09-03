# ADR-024: NVIDIA Cluster Readiness Engine Component

## Status

**Proposed** — 2026-09-02.

Reviews upstream
[`NVIDIA/cluster-readiness-engine`](https://github.com/NVIDIA/cluster-readiness-engine)
v0.1.0, published 2026-09-01 at source commit `45726bfe`. Implementation
follows acceptance as a separate change; see
[PR #2523](https://github.com/NVIDIA/aicr/pull/2523).

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

### 4. Placement is limited to tolerations

The chart's Deployment template renders `affinity` and `tolerations` but has no
`nodeSelector` block, so a system node selector would be silently dropped. The
registry entry declares `nodeScheduling.system.tolerationPaths` only.

This narrows placement rather than widening it: the chart default is a blanket
`tolerations: [{operator: Exists}]`, which tolerates every taint in the cluster
including tainted GPU nodes. Only the manager is covered — NVCRE's benchmarks
belong on GPU nodes and must not inherit system placement.

Hard placement stays available through component values or `--set-json`;
`manager.affinity` is object-valued and is not settable through scalar `--set`.

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

## Adoption Gates

Implementation is admitted once the selected release passes the following.

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

- A stated maximum node count and run deadline per certification.
- Documented `TrainJob` cleanup on completion, failure, and cancellation.
- A bounded termination condition for adaptive fault isolation's bisection,
  in both total runs and wall-clock.
- The controller creates no workload without an explicit `Certification`,
  `WorkloadRun`, or `Workflow` resource.

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

Two supply-chain items are open, both upstream release-workflow changes rather
than AICR work:

1. **No build provenance on the image.** The only attestation predicates
   present are `cyclonedx.org/bom` and `sigstore.dev/cosign/sign/v1`; a
   `slsaprovenance` query returns no match.
2. **No supply-chain artifacts on the chart.** `cosign tree` against the chart
   reports none — no signature, SBOM, or provenance. The chart is published by
   a `helm push` with no signing step.

Both are tracked in [Follow-Up Decisions](#follow-up-decisions).

## Non-Goals

- Adding `nvcre` to any stock recipe.
- Introducing certification results into [ADR-007](007-recipe-evidence.md)
  recipe evidence.
- Building an AICR-side validator that drives NVCRE.
- Vendoring, patching, or republishing the upstream chart.
- Replacing the existing NCCL bandwidth checks on the current TrainJob path.

## Consequences

**Positive.** Stock recipes are unchanged, so no existing user acquires
NVCRE's CRDs, cluster-scoped RBAC, or runtime cost. The internal DGX Cloud
consumer keeps its current external-overlay path and is not blocked. The two
supply-chain gaps are named concretely with a verifiable close condition.

**Negative.** Implementation is gated on upstream release-workflow changes AICR
does not control, so the PoC's measured value is not available through AICR
until they close.

## Follow-Up Decisions

1. Upstream: add build provenance to the controller image, attributable to the
   release workflow and meeting a stated SLSA build level.
2. Upstream: sign the Helm chart and publish chart SBOM and provenance.
3. AICR: on close, land the implementation in
   [PR #2523](https://github.com/NVIDIA/aicr/pull/2523) and record the
   qualified artifact set in this ADR's Status.
4. AICR: a separate amendment for any stock-recipe adoption.
