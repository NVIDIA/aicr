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
GB200 work already in flight, with GB300 framed as a near-horizon target.

**Intent axis.** Both training and inference per (service × accelerator) cell once
that cell is in scope.

**Automated snapshot intake.** Continue closing the loop from `--from-snapshot`
through NFD-based discovery so recipes are derivable from real cluster state, not
hand-authored criteria.

**Sustained validation.** KWOK-tiered tests exercise the recipe matrix today —
advisory on PRs (Tier 1 generic plus diff-aware Tier 2) with the full matrix
running on merges to `main` and nightly. v1 closes the gap by adding a nightly
UAT cadence on live managed Kubernetes services and tightening these signals
into the merge path so coverage stays validated between releases.

**CNCF AI Conformance.** Certify AICR-on-EKS as conformant once the WG finalizes
requirements. Treat conformance evidence as a first-class output of the validator.

**Acceptance:** every (service × accelerator × intent) cell in the v1 matrix
validates against a snapshot, generates a bundle, deploys via UAT, and is exercised
by nightly automation.

### 2. Stability

Lock every product surface — CLI, REST API, and bundle output — before declaring
v1, so breaking changes after GA require a major version bump.

**CLI and REST API freeze.** Audit every flag, subcommand, endpoint, and schema
field in `api/aicr/v1/server.yaml`; remove deprecated paths; tag a baseline that
CI can diff against to catch unintended breakage. Establish a deprecation channel
so future changes can warn before they break.

**Bundle output as a stable surface.** The bundle is what downstream consumers
integrate against; v1 hardens it as a first-class contract. This includes a generic
Helm bundle format that is deployer-neutral, undeploy pre/post-flight checks for
safe teardown, and decoupling environment specifics (e.g., StorageClass) from
recipe content so bundles are portable.

**Versioning policy.** Document the major-bump policy for each surface in
`RELEASING.md` and wire compatibility tests into CI.

**Acceptance:** `v1.0.0` ships with a frozen, documented contract for the CLI,
REST API, and bundle layout; any subsequent breaking change is detected in CI and
gated behind a major-version bump.

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

**Acceptance:** an external contributor adds a new validated leaf recipe using
only public docs and the CLI, and the review pipeline merges community PRs at the
cadence the inbound flow demands.

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

**Acceptance:** every artifact (binary, image, recipe data, bundle) has retrievable
provenance and a documented verification path that works in air-gapped, private,
and public-trust deployments.

## Revision History

| Date | Change |
|------|--------|
| 2026-04-28 | Restructured around v1 objectives |
| 2026-02-17 | Expanded Recipe Creation Tooling with validation framework details, scaffolding, and workflow integration |
| 2026-02-14 | Moved implemented items to Completed: EKS H100 recipes, snapshot-to-recipe, monitoring, Nodewright Ubuntu |
| 2026-01-26 | Reorganized: removed completed items, streamlined structure |
| 2026-01-05 | Added Opens section based on architectural decisions |
| 2026-01-01 | Initial comprehensive roadmap |
