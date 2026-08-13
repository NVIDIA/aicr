# ADR-019: k8s-aibom Runtime AI Inventory Component

## Status

**Proposed** — 2026-08-12.

## Decision Summary

AICR will admit
[`GoogleCloudPlatform/k8s-aibom`](https://github.com/GoogleCloudPlatform/k8s-aibom)
as an optional, provider-neutral Helm component after one exact upstream release
passes the packaging, security, lifecycle, and qualification gates in this ADR.

The first implementation is **registry-only**:

- `k8s-aibom` is added to the component registry but no stock recipe references
  it;
- custom and external recipes may opt in explicitly;
- recipes that declare the component also declare its deployment-health
  expectation, so an absent or unhealthy controller fails deployment validation;
- recipes that do not declare it, including every stock recipe, remain unchanged;
  and
- no runtime-observation evidence predicate or model-verification integration is
  introduced.

This ADR does not authorize a stock-recipe pilot or default installation. Stock
adoption requires a separate decision with recipe-recorded selection semantics,
current user demand, API maturity, and provider qualification. A bundle-only
`--set k8s-aibom:enabled=false` override is not an acceptable selection contract
for stock adoption because it does not change the recipe or its health checks.

## Context

### AICR

AICR generates and validates GPU-accelerated Kubernetes configurations using
the workflow:

```text
Snapshot -> Recipe -> Validate -> Bundle
```

An AICR recipe records desired platform configuration: selected components,
chart or source pins, values, constraints, deployment order, and validation
expectations. AICR evidence can bind a resolved recipe, snapshot, validation
results, and component BOM at one point in time. It does not continuously
observe workloads after deployment or prove that later live state still matches
the validated recipe.

AICR components are declarative metadata. A normal Helm component consists of a
registry entry, minimal AICR values, a pinned chart version, and a read-only
health check. Recipes reference it explicitly through `componentRefs`; the
registry entry itself carries no back-reference to any recipe. Existing
deployers project that definition into Helm, Helmfile, Argo CD, Argo CD Helm,
Flux, and local-format outputs. Components do not add custom installation or
controller logic to the bundler.

Relevant contracts:

- [Component contributor guide](../contributor/component.md)
- [ADR-006: Container Image Pinning Policy](006-image-pinning-policy.md)
- [ADR-007: Verifiable Recipe Test Evidence](007-recipe-evidence.md)

### k8s-aibom

k8s-aibom is a Kubernetes controller that observes AI workloads and emits
CycloneDX 1.6 ML-BOM documents. It watches workload and pod APIs, applies
category-specific scrapers, records `declared`, `inferred`, or `unresolved`
confidence with evidence locators, and creates one namespace-scoped `AIBOM`
resource per tracked top-level workload.

The controller does not mutate the observed workload or execute inside its
containers. It is not read-only, however: it creates, updates, and deletes
controller-owned `AIBOM` resources, updates `AIBOM` and
`AIBOMControllerConfig` status, and creates or patches Kubernetes Events.
Namespace selection limits which workloads produce AIBOMs; it does not by
itself remove the controller's cluster-wide informer visibility.

Small BOM documents are stored inline in `AIBOM.status.bomDocument`. Larger
documents are written to a configured external sink or reported as truncated
when no sink can retain them. The default configuration is namespace opt-in and
has no external sink.

This proposal was reviewed on 2026-08-12 against upstream commit
[`e752beb15c8eb0179bba4f3066c7b989c84da33e`](https://github.com/GoogleCloudPlatform/k8s-aibom/commit/e752beb15c8eb0179bba4f3066c7b989c84da33e).
At that commit:

- the public API is `aibom.k8saibom.dev/v1alpha1` and explicitly experimental;
- the repository has no source tag or GitHub release;
- Google publishes no controller image or Helm repository for the project;
- the chart accepts an image repository and tag, but not a first-class digest;
- chart, app, and binary-reported versions are inconsistent;
- the chart sets a `metadata.namespace` on the cluster-scoped default
  configuration and creates it via a `pre-install` hook; and
- rendered RBAC contains duplicate rules and broader write permissions than the
  minimum intended ownership boundary.

These are adoption blockers, not defects AICR will mask with a private chart or
manifest copy.

### Existing integration discussion

The projects' relationship was discussed in
[`GoogleCloudPlatform/k8s-aibom#8`](https://github.com/GoogleCloudPlatform/k8s-aibom/issues/8).
The complementary boundary is:

- AICR records qualified deployment intent and point-in-time validation;
- k8s-aibom records current AI-workload observations; and
- a future evidence design may bind those records while preserving their
  different subjects, freshness, and confidence semantics.

## Problem

AICR users currently have no AICR-native way to select and bundle a qualified
k8s-aibom release with the same pinning, secure-default, health, deployer, and
air-gap policies applied to other components.

Without an explicit boundary, the integration could drift into unsafe or
duplicative shapes:

- AICR could publish or fork an unofficial distribution;
- a mutable image or unreleased chart could enter the registry;
- the controller could become an implicit dependency of normal validation;
- a bundle-time disable could diverge deployment from the recipe's health
  contract;
- AIBOM data could leave the cluster without an explicit operator decision;
- an AICR recipe signature could be misrepresented as model verification; or
- workload-layer inventory could be presented as full infrastructure-drift
  coverage.

## Decision

### 1. Model k8s-aibom as a standard Helm component

The component name is `k8s-aibom`. It uses the existing component registry,
values, health-check, scheduling, bundle, mirror, and documentation paths. It
does not add a deployer, AICR controller adapter, component-specific Go package,
or installation script.

The final registry entry will use the exact repository, chart name, version,
namespace, and value paths published by the qualified upstream release. The
intended namespace is `k8s-aibom-system`, subject to verification against that
release.

### 2. Keep adoption registry-only

The implementation adds the registry entry, values, health check, tests, and
documentation without adding `k8s-aibom` to `recipes/overlays/base.yaml`, a leaf
overlay, or a mixin used by stock recipes.

Registry presence does not enable a component. A custom or external recipe must
declare a `ComponentRef` explicitly.

Opt-in scope follows the declaring overlay's `criteria`, not a single recipe.
AICR resolution is criteria-match across every overlay, so a broad-criteria
overlay attaches its components — and their health checks — to every matching
recipe; `recipes/overlays/monitoring-hpa.yaml` (`criteria: intent: any`) is the
in-tree example. An adopter declaring `k8s-aibom` must scope the declaring
overlay narrowly and avoid `intent: any` unless universal injection is intended.

Once declared and hydrated, the component's health assertions are part of the
recipe's deployment-validation contract:

- absence of the CRD, controller, or required configuration is a failed
  deployment check;
- controller/configuration failure is not flattened into a skip or successful
  empty observation; and
- zero `AIBOM` resources is healthy when no namespace is opted in.

Recipes that do not declare the component do not acquire its CRDs, RBAC, runtime
cost, health check, or validation dependency.

### 3. Preserve the upstream ownership boundary

k8s-aibom owns:

- workload watches, reconciliation, and controller lifecycle;
- scraper selection and heuristics;
- confidence and evidence-locator semantics;
- CycloneDX document construction and canonical output bytes;
- `AIBOM` and `AIBOMControllerConfig` APIs;
- external-sink behavior; and
- any future model-signature verifier.

AICR owns:

- selecting the qualified upstream release;
- chart and image pins recorded in AICR;
- secure AICR values and recipe placement;
- system-node scheduling injection;
- component health assertions;
- bundle rendering across every supported deployer;
- air-gap image discovery and relocation;
- AICR-side component qualification; and
- any future AICR evidence artifact that consumes public AIBOM output.

AICR consumes only public, versioned contracts. It will not import upstream
`internal/` packages, copy scraper code, carry a private chart, or republish an
unofficial upstream release under an AICR identity. This restriction does not
prevent normal user-controlled `aicr mirror` workflows from copying the
qualified artifact into a target air-gap registry.

### 4. Require immutable, verifiable upstream artifacts

Before implementation, upstream must provide one coherent release containing:

1. a tagged source release with release notes;
2. a published, immutable, multi-architecture controller image;
3. a published Helm chart in a stable Helm or OCI repository;
4. coherent source, chart, app, image, and binary versions;
5. an image reference selectable by digest without patching the chart;
6. a machine-readable SBOM bound to that image digest;
7. machine-verifiable build provenance bound to the same digest and source
   commit, and attributable to the expected upstream builder identity; and
8. documented compatibility for the chart, image, CRDs, and supported
   Kubernetes versions.

AICR qualifies the chart, image, CRDs, and public status contract as one
versioned set. A chart, image, CRD, or incompatible status change requires
requalification.

### 5. Apply secure, inactive-by-default values

The qualified AICR values must preserve these defaults:

- namespace discovery requires the explicit
  `aibom.k8saibom.dev/enabled=true` label;
- AICR does not label customer namespaces;
- no GCS, webhook, GUAC, or other external sink is configured;
- no cloud identity, credential Secret, token, certificate, or endpoint is
  created;
- `inlineThresholdBytes` defaults to 262144 bytes (256 KiB); BOMs at or
  below the configured threshold are stored in
  `AIBOM.status.bomDocument.inline`;
- when a BOM exceeds the threshold and no external sink succeeds, status omits
  the full BOM, sets `AIBOM.status.bomDocument.truncated=true`, and retains the
  summary, SHA-256 hash, byte size, and a truncation reason that distinguishes
  no configured sink from failed configured sinks;
- the controller runs non-root with a read-only root filesystem, default
  seccomp, no privilege escalation, and all Linux capabilities dropped;
- CPU and memory requests and limits are based on measured qualified scale;
- logging defaults to structured, non-debug output; and
- controller scheduling uses `nodeScheduling.system`, never accelerated-node
  paths.

The selected chart must expose the required digest, namespace selector, sink,
resource, security, node-selector, and toleration settings through public
values. AICR will not invent unsupported keys or patch chart templates.

### 6. Minimize and verify RBAC and lifecycle behavior

Qualification reviews rendered manifests, not only source annotations. The
release must prove:

- workload and pod APIs are read-only;
- AIBOM writes are limited to the controller-owned API group and required
  status paths;
- configuration writes are limited to required status behavior;
- Event writes are justified and bounded;
- Secret access is absent when sinks are disabled, or restricted to the
  controller namespace and minimum verbs when enabled;
- no privileged pod, host mount, host namespace, node-agent, workload exec, or
  Secret-value discovery permission exists;
- no ClusterRole `aggregationRule` or aggregation label pulls the ServiceAccount
  into `admin`, `edit`, or `view`, and no `impersonate`, `escalate`, or `bind`
  verb and no wildcard (`*`) apiGroup, resource, or verb grant appears. The
  cluster-wide reads the controller does require stay bounded by the positive
  limits above, which are the governing allowlist;
- the cluster-scoped default configuration has no namespace;
- configuration is managed as a normal release resource unless a hook is
  demonstrably necessary and its lifecycle is tested; and
- install, upgrade, rollback, uninstall, CRD retention, and AIBOM retention are
  deterministic and documented.

Duplicate, obsolete, unexpectedly broad, or lifecycle-ambiguous rendered
resources block admission.

### 7. Use a non-vacuous, read-only health check

The registry declares a Chainsaw health assertion that requires:

1. the controller Deployment exists in the expected namespace;
2. it has at least one desired replica and available replicas equal desired
   replicas;
3. `AIBOMControllerConfig/default` exists;
4. its `Ready` condition is `True`; and
5. status and conditions have observed the current generation when those fields
   are part of the qualified API contract.

The check must not pass because an object is absent, a selector is empty, or an
API error was treated as not-found. It does not require an AIBOM: with namespace
opt-in, a healthy new installation can legitimately observe zero workloads.

### 8. Define, but do not implement, the evidence boundary

The stable future consumer boundary is the public `AIBOM` resource and its
referenced CycloneDX document. No upstream internal package is required.

This ADR does not add that consumer. A follow-up design must define predicate
type, subject identity, freshness, correlation, exact-byte hashing, truncation,
external retrieval, redaction, storage, verification, partial failure, and
compatibility before AICR signs runtime observations.

Any future capture must preserve these trust limits:

- signing observed bytes proves what the signer captured, not that every
  upstream heuristic or workload declaration was true;
- missing, stale, truncated, malformed, inaccessible, or hash-mismatched BOMs
  are explicit outcomes, never successful empty observations;
- image digests are preferred correlation keys; workload names and declared
  model names are not cryptographic identities;
- AICR recipe evidence is not a model signature; and
- k8s-aibom covers the AI workload layer, not the GPU operator, network
  operator, drivers, node tuning, monitoring, or complete cluster drift.

ADR-007 recipe evidence remains unchanged.

## Adoption Gates

Registry implementation begins only after one exact upstream release passes all
of the following gates.

### Release and supply chain

- Tagged source, published chart, multi-architecture image, digest, SBOM, and
  provenance form one coherent release.
- Provenance and SBOM subjects bind the selected image digest.
- Provenance verifies against the expected upstream builder identity — OIDC
  issuer, source repository, and workflow ref — and meets a stated minimum SLSA
  build level. Subject-digest binding alone is insufficient: it detects a
  provenance/image mismatch, not a well-formed attacker-issued attestation whose
  subject correctly names a malicious digest. This requirement is specific to
  ADR-019 and deliberately exceeds ADR-006, which defers signature verification
  to admission time.
- No floating tag, branch, `latest`, or locally built artifact appears in the
  component definition.
- The chart renders only expected images, and every rendered image — including
  any sidecar or init container the qualified release introduces — is selectable
  by digest through public values. A chart-default sub-image left tag-only would
  fall into ADR-006 Layer 3, outside this ADR's pinning guarantee. At the
  reviewed commit the chart renders a single `manager` container.

### Helm and Kubernetes lifecycle

- `helm lint` and render succeed for the selected release.
- CRDs are established before custom resources use them.
- Cluster-scoped objects have valid metadata.
- Default configuration has deterministic install, update, rollback, and
  uninstall ownership.
- CRD conversion, migration, and retention behavior is documented for the
  selected API.

### Security and privacy

- Rendered RBAC and pod security satisfy Decision 5 and Decision 6.
- No AIBOM is produced for a namespace without an explicit selector match. The
  controller's informer cache stays cluster-wide regardless of opt-in, so pod
  and workload specs from every namespace — including inline environment values,
  args, and image references — are read into controller memory even for
  namespaces never opted in. No data leaves the cluster by default because no
  sink is configured, so this is in-memory read scope rather than egress. It is
  an accepted residual of this decision, not an oversight, and must be
  documented so adopters price it in. A namespace- or label-scoped informer is a
  qualification preference, not a gate.
- No external delivery or credential dependency exists by default.
- AIBOM content, readers, size bounds, truncation, retention, and deletion are
  documented.
- Secret references are not dereferenced into BOM output.

### Operational safety

- Every outbound Kubernetes API call and external-sink attempt has a finite
  numeric deadline and honors cancellation. Timeout or cancellation stops
  in-flight work and is surfaced rather than flattened into success.
- Retry policy is limited to transient failures and defines a finite maximum
  attempt count, maximum elapsed time, and bounded backoff schedule or cap.
- Any asynchronous delivery queue has a fixed numeric capacity and documented
  overflow policy. Overflow cannot block reconciliation indefinitely or
  silently discard an observation.
- The implementation PR records the qualified release's deadline, retry,
  backoff, queue-capacity, and overflow values and proves their observable
  behavior.
- After a sink deadline, retry exhaustion, or delivery-queue overflow, the
  controller still attempts the AIBOM status update. A successfully generated
  BOM remains `Ready=True`, while `Synced=False` and `SinkFailed=True` identify
  the failed sink and reason. A BOM-generation failure sets `Ready=False` when
  status is writable. A status-persistence failure requeues and emits a metric
  or Event; consumers cannot treat an unadvanced `observedGeneration` as fresh
  success.
- Invalid configuration and last-known-good behavior are visible.
- Inline status remains bounded by the configured threshold.
- Readiness detects controller/configuration failure rather than only a live
  process.
- Resource use is measured at representative workload counts.

### AICR qualification

- The component renders consistently through `helm`, `helmfile`, `argocd`,
  `argocd-helm`, `flux`, and `localformat`. Only the first five are
  `--deployer` values; `localformat` is the shared writer those paths build on,
  so exercise it at the writer and vendored-chart level rather than by passing
  `--deployer localformat`, which fails to parse.
- Vendored-chart output is tested wherever the deployer supports it.
- Scheduling values land on the controller Deployment.
- `aicr mirror list` discovers the exact image and mirror/copy qualification
  preserves its digest identity.
- `make component-test COMPONENT=k8s-aibom` proves installation and the generic
  health contract.
- A separate, named Kind integration test opts in a fixture namespace, creates
  a lightweight digest-pinned workload, and proves AIBOM generation,
  reconciliation after a relevant workload change, CycloneDX 1.6 validity, and
  owner-reference cleanup. Its hash assertions are that
  `status.bomDocument.sha256` matches the canonical BOM bytes, and that a
  reconcile of an unchanged workload preserves `status.inputHash`. It must not
  assert that `status.bomHash` is stable: that field covers the output bytes,
  which carry the BOM's generation timestamp, so it changes on every
  regeneration by design. The implementation PR must add and document this test
  command because the generic component harness stops after the read-only
  health check.
- Stock recipe resolution and bytes remain unchanged.
- `make bom-docs`, focused component/bundler/mirror tests, applicable render
  tests, documentation link validation, and `make qualify` pass.

## Implementation Deliverables

After the gates pass, the component PR includes:

- one registry entry with an immutable chart version;
- minimal AICR values with the qualified image digest and secure defaults;
- system scheduling paths verified against rendered templates;
- a read-only health check;
- the dedicated Kind integration test described above;
- deployer, vendoring, mirror, lifecycle, and negative-path tests;
- component-catalog, security, opt-in, troubleshooting, upgrade, uninstall,
  and data-retention documentation; and
- regenerated `docs/user/container-images.md`.

The component PR records the exact source tag, chart version, image digest,
SBOM, provenance verification, rendered RBAC review, and commands executed.

## Non-Goals

- Add k8s-aibom to a stock recipe, shared base, or platform mixin.
- Define a platform pilot or automatic progression from registry preview.
- Add a new AICR evidence predicate or change ADR-007.
- Make k8s-aibom required for recipe generation, bundling, or validation when a
  recipe does not declare it.
- Treat a bundle-time component toggle as recipe selection.
- Implement k8s-aibom as an AICR validator Job.
- Fork its chart or reimplement its scrapers.
- Publish upstream artifacts under an AICR identity.
- Provision sinks, buckets, webhook receivers, identities, credentials, or
  retention policies.
- Enable namespace observation or external sinks by default.
- Treat declared model names as verified identities.
- Present runtime workload inventory as full cluster-drift proof.
- Certify compliance with the EU AI Act, NIST AI RMF, ISO/IEC 42001, or another
  framework.

## Consequences

### Positive

- Users gain a reproducible, explicit path to deploy runtime AI inventory.
- Existing component, deployer, mirror, health, and documentation architecture
  is reused without new AICR business logic.
- Stock recipes and normal validation remain unchanged.
- Upstream remains the sole owner of discovery and ML-BOM semantics.
- Supply-chain, RBAC, privacy, and lifecycle blockers are resolved before
  admission rather than patched in AICR.
- A future evidence consumer has a public, loosely coupled boundary.

### Negative

- The integration cannot begin until upstream publishes qualified artifacts and
  fixes chart lifecycle and RBAC blockers.
- Users must author or adopt an explicit custom recipe to enable the component.
- Each adopted release adds chart, CRD, RBAC, health, mirror, BOM, and upgrade
  maintenance.
- AIBOM resources consume etcd storage and can expose deployment metadata.
- Registry-only adoption does not provide an out-of-box runtime inventory for
  stock recipes.

### Neutral

- k8s-aibom remains independently installable.
- AICR remains useful without k8s-aibom.
- Registry-only may remain the final adoption state.
- Stock adoption, runtime-observation evidence, and model verification remain
  separate future decisions.

## Alternatives Considered

### Add the component to stock recipes now

**Rejected.** Upstream artifacts are not publishable inputs today, the API is
experimental, and default installation would add CRDs, cluster-wide visibility,
write RBAC, and an always-on controller. Bundle-time disablement would also
diverge the deployment from the recipe's health contract.

### Build or republish upstream artifacts from AICR

**Rejected.** This would make AICR the owner of an unofficial distribution,
release cadence, vulnerabilities, provenance, and support.

### Vendor or patch the upstream chart

**Rejected.** A private chart would drift from upstream CRDs, RBAC,
configuration, and releases. Packaging defects must be fixed upstream.

### Pin a Git or Kustomize component to a source commit

**Rejected.** A source pin does not provide the released image, SBOM,
provenance, public values, or Helm lifecycle required by this decision.

### Implement k8s-aibom as an AICR validator

**Rejected.** It is a continuously reconciling controller with durable custom
resources, not a bounded validation Job.

### Put AIBOM documents directly into ADR-007 evidence

**Rejected for this ADR.** Recipe-test evidence and continuously refreshed
runtime observations have different subjects, freshness, storage, and failure
semantics.

## Follow-Up Decisions

Stock adoption requires a separate ADR or explicit amendment that defines:

- the exact recipe families in scope;
- generation-time, recipe-recorded selection and opt-out semantics;
- a non-alpha storage API and documented migration policy;
- a concrete user-demand case;
- managed-cluster qualification and measured controller/API-server cost; and
- upgrade, rollback, uninstall, and support evidence.

Runtime-observation signing requires a separate evidence design as described in
Decision 8. Neither follow-up is implied by registry qualification or elapsed
time.

## References

- [k8s-aibom repository](https://github.com/GoogleCloudPlatform/k8s-aibom)
- [k8s-aibom and AICR integration issue #8](https://github.com/GoogleCloudPlatform/k8s-aibom/issues/8)
- [README at the reviewed commit](https://github.com/GoogleCloudPlatform/k8s-aibom/blob/e752beb15c8eb0179bba4f3066c7b989c84da33e/README.md)
- [Helm chart at the reviewed commit](https://github.com/GoogleCloudPlatform/k8s-aibom/tree/e752beb15c8eb0179bba4f3066c7b989c84da33e/charts/k8s-aibom)
- [AIBOM API at the reviewed commit](https://github.com/GoogleCloudPlatform/k8s-aibom/blob/e752beb15c8eb0179bba4f3066c7b989c84da33e/api/v1alpha1/aibom_types.go)
- [AIBOMControllerConfig API at the reviewed commit](https://github.com/GoogleCloudPlatform/k8s-aibom/blob/e752beb15c8eb0179bba4f3066c7b989c84da33e/api/v1alpha1/aibomcontrollerconfig_types.go)
- [Security model at the reviewed commit](https://github.com/GoogleCloudPlatform/k8s-aibom/blob/e752beb15c8eb0179bba4f3066c7b989c84da33e/docs/security-model.md)
- [ADR-006: Container Image Pinning Policy](006-image-pinning-policy.md)
- [ADR-007: Verifiable Recipe Test Evidence](007-recipe-evidence.md)
