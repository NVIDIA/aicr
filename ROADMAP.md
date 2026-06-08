# AICR Roadmap

This roadmap tracks remaining work toward AICR v1 (GA) and post-v1 enhancements.

## Objectives

v1 will be the first release we declare stable and production-ready. The bar:

| Objective | Definition |
|-----------|------------|
| **Sufficient Coverage** | Enough recipe coverage to establish gravity across the key axes (service / accelerator / platform), backed by sustained validation |
| **API/CLI Stability** | Both surfaces — including bundler output — stable enough to GA; subsequent breaking changes require a major version bump |
| **Easy to Contribute** | No-code recipe contribution path validated end-to-end; new recipes don't require deep AICR internals knowledge, and the review pipeline keeps up with community flow |
| **Closed Supply Chain** | Every bundle has verifiable provenance from build through deploy, including air-gapped and enterprise-controlled signing modes |

## Scope

### 1. Coverage

Recipe portfolio is the ongoing focus of the project: expanding the matrix of
optimized GPU-accelerated Kubernetes configurations. Coverage is judged along
four axes plus the validation that backs it.

**Service axis.** Validate end-to-end AICR on managed Kubernetes services — both Cloud Service Provider (CSP) and partner-cloud offerings — alongside vanilla Kubernetes.

**Accelerator axis.** H100 is mature; v1 needs L40 and B200 in addition to the
GB200 work already in flight, with GB300 and Vera Rubin (VR) framed as
near-horizon targets.

**Service axis (bare metal).** Bare-metal service classes — including BCM and
equivalent operator-managed Kubernetes distributions — are represented in the
v1 matrix alongside CSP and vanilla Kubernetes.

**Intent axis.** Both training and inference per (service × accelerator) cell once
that cell is in scope.

**Automated snapshot intake.** Continue closing the loop from `--from-snapshot`
through NFD-based discovery so recipes are derivable from real cluster state, not
hand-authored criteria.

**Sustained validation.** KWOK-tiered tests exercise the recipe matrix today —
advisory on PRs (Tier 1 generic plus diff-aware Tier 2) with the full matrix
running on merges to `main` and nightly. v1 closes the gap by adding a nightly
UAT cadence on live managed Kubernetes services and tightening these signals
into the merge path so coverage stays validated between releases. Results are
published through a Testgrid-style dashboard so the matrix has a single,
durable evidence channel anyone can read.

**CNCF AI Conformance.** Certify AICR-on-EKS as conformant once the WG finalizes
requirements. Treat conformance evidence as a first-class output of the validator.

**Acceptance:**

*Shippable artifacts:*
- v1 matrix of (service × accelerator × intent) cells declared in this document, with bare-metal classes (BCM) and forward-looking accelerators (GB200, GB300, VR) represented as in-flight slots.
- CNCF AI Conformance evidence emitted by the validator and regenerated per release.
- Nightly UAT cadence on live managed Kubernetes wired into the merge signal.
- Testgrid (or equivalent dashboard) brought up as the canonical evidence channel for recipe validation.

*Demonstrable signals:*
- Sustained green-rate across the matrix in Testgrid across multiple release cycles.
- Recipe portfolio carries enough gravity that the dominant service / accelerator / platform needs of most users land inside the supported matrix.

### 2. Stability

Lock every product surface — CLI, REST API, and bundle output — before declaring
v1, so breaking changes after GA require a major version bump.

**CLI and REST API freeze.** Audit every flag, subcommand, endpoint, and schema
field in `api/aicr/v1/server.yaml`; remove deprecated paths; tag a baseline that
CI can diff against to catch unintended breakage. Establish a deprecation channel
so future changes can warn before they break.

**Bundle output as a stable surface.** The bundle is what downstream consumers
integrate against; v1 hardens it as a first-class contract. This includes a generic
Helm bundle format that is deployer-neutral, deferring teardown to the deployer's
native uninstall path (helm, ArgoCD, Flux) rather than shipping bundle-side
scripts, and decoupling environment specifics (e.g., StorageClass) from recipe
content so bundles are portable.

**Versioning policy.** Document the major-bump policy for each surface in
`RELEASING.md` and wire compatibility tests into CI.

**Acceptance:**

*Shippable artifacts:*
- Frozen baseline tag for CLI flags, REST endpoints (`api/aicr/v1/server.yaml`), Go SDK exported surface (`pkg/client/v1`), and bundle layout + schemas (recipe, validation, snapshot kinds).
- CI diff-gate that fails on unintended breakage of any of the four surfaces.
- Deprecation channel documented in `RELEASING.md`.

*Demonstrable signals:*
- Sustained external use of the Go SDK, REST API, and schemas by consumers outside the AICR team, exercised against the frozen surface across release cycles without breaking-change requests.
- Deprecation channel exercised at least once in practice before tagging `v1.0.0`.

### 3. UX

Friction in the contribution path is the primary bottleneck to community growth.
Two sub-themes:

**Authoring path.** Validate the no-code recipe contribution path end-to-end:
static recipe validation (syntax, cross-reference, semantic) usable pre-PR via CLI,
pre-commit, and CI; an `aicr recipe validate` verb; a scaffolding generator that
produces a working overlay skeleton from `(service, accelerator, intent)`; a
reusable overlay template; KWOK e2e auto-generation for new leaves; and a public
walkthrough in `docs/contributor/`. Recipe quality discipline — mixin-based
composition with no duplication across overlays — is part of this theme so new
contributors aren't copy-pasting drift into the tree.

**Review pipeline.** A merge queue and a unified merge-gating CI definition keep
the review pipeline fast and predictable; a local registry for nvkind keeps the
inner-loop tight for contributors without cloud access.

**Acceptance:** the bar is *self-service without filing a support issue or
reading source code* for three personas — **user**, **integrator**, and
**contributor**.

*Per-persona self-service thresholds:*
- **User** — install through bundle generation, deployment validation, and evidence collection completable using only `docs/user/`. The same path eventually submits that evidence to the Testgrid pipeline described in §1.
- **Integrator** — Go SDK or REST API embeddable into a downstream pipeline using only godoc and `docs/user/api-reference.md`.
- **Contributor** — new or modified leaf recipes authorable and mergeable using only `docs/contributor/` plus CLI tooling, and every recipe change carries validation evidence that the CI gate verifies before merge.

*Shippable artifacts behind the thresholds:* `aicr recipe validate` verb, scaffolding generator, reusable overlay template, KWOK e2e auto-generation for new leaves, mixin-based composition discipline, merge queue + unified merge-gate, recipe-change CI gate that requires verifiable validation evidence.

*Demonstrable signals:*
- A community-authored leaf recipe merged end-to-end without maintainer hand-holding.
- An independent user deploys a bundle and submits evidence through the documented pipeline.
- An external integrator reference exists (in-tree example, downstream repo link, or recorded user story).

### 4. Security

GPU clusters are high-value targets. A fully configured GPU training environment
can carry hundreds of configuration values across many components; verifiable
provenance is foundational for production trust.

**Build-time provenance.** SLSA build attestation, cosign signing, trust-level
evaluation in `aicr bundle verify`, and bundle attestation for the `argocd-helm`
output are shipped.

**Enterprise signing modes.** v1 closes the gap for production users who can't
rely on the public Sigstore: air-gapped signing flows, private Sigstore deployment,
and KMS-backed key custody.

**Verify-on-deploy.** Document and test the path that gates deploys on signature
and provenance verification, so the chain isn't broken at the last hop.

**Recipe data provenance.** Sign and verify the embedded validator catalog and
component registry — the recipe data layer must carry the same trust guarantees
as the binaries that read it.

**End-user verification.** A top-level guide that walks a consumer from a
downloaded bundle to a fully-verified deploy, covering all signing modes above.

**Acceptance:**

*Shippable artifacts:*
- SLSA attestation on every artifact (binary, image, recipe data, bundle) with cosign signing.
- `aicr bundle verify` honoring trust-level evaluation across signing modes.
- Air-gapped signing flow, private Sigstore deployment, and KMS-backed key custody documented and tested.
- Verify-on-deploy path documented and tested.
- Signed validator catalog and component registry.
- End-user verification guide published in `docs/user/`.

*Demonstrable signals:*
- Consecutive releases pass the full provenance chain in CI.
- An independent party verifies a release end-to-end across the supported signing modes.

*Epic:* #1149.
