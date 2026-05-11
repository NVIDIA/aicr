# ADR-007: Verifiable Recipe Test Evidence

## Status

**Proposed** — 2026-05-08 (design-only; not implemented).

This ADR specifies the V1 contract. Implementation lands as five
follow-on PRs tracked under
[#750](https://github.com/NVIDIA/aicr/issues/750) and its children
([#751](https://github.com/NVIDIA/aicr/issues/751)–[#754](https://github.com/NVIDIA/aicr/issues/754)).
Bundle formats, CLI flags, schema fields, and verifier behavior
described below are future intent, not current behavior.

## Problem

AICR ships recipes for AWS, OCI, GCP, Azure, CoreWeave, Forge, and on-prem
combinations spanning H100, GB200, B200, MI300, multiple OSes, and several
intent variants. **No single team has hands-on access to all of them.**

Today, recipe contributions for hardware AICR maintainers can't reach are
either blocked or accepted on faith. Three classes of friction follow:

- **Reachability gap.** A contributor running on AWS GB200 cannot get a
  maintainer to re-run their validation; the maintainer has neither the
  cluster nor the time to set one up. The PR stalls or merges on trust
  alone.
- **No artifact for review.** `aicr validate` produces console output and
  CTRF JSON locally, but nothing the maintainer can cryptographically
  tie back to the contributor who ran it. "Trust me, it passed" is the
  current contract.
- **No signal lineage.** Even when a contribution lands, there is no
  durable record tying the recipe-as-merged to the validation result the
  reviewer relied on. A future re-cert or audit has nothing to consult.

The recipe **already self-defines its tests** through `validation`,
`componentRefs`, `criteria`, and the existing `aicr validate` pipeline.
What's missing is a way to package the answer to a question the recipe
already poses, so a maintainer can verify it without re-running.

## Non-Goals

- **Auto-merging recipe PRs based on evidence.** Evidence is a
  verifiable input to maintainer judgment, not a replacement for it.
- **Extending the recipe schema with new "acceptance criteria" fields.**
  The recipe's existing `validation` and `componentRefs` define what
  passing means; this ADR packages the answer, not the question.
- **Per-component admission-time digest verification.** That's
  [ADR-006](006-image-pinning-policy.md) territory and #745. The bundle
  scopes to AICR's pinning surface (recipe + chart-pin + digest-pin),
  not chart-default sub-images.
- **KMS-backed signing.** V1 uses cosign keyless OIDC. KMS is a
  future extension under the same predicate type and bundle format —
  no design change required.
- **Re-running validators in `aicr evidence verify`.** Verification is
  an offline cryptographic + schema operation; re-running validators
  defeats the purpose and would require maintainers to have the
  hardware they don't have.
- **Pre-building features without demand.** Tier policies, multi-instance
  pointers, signed layered predicate types, re-cert automation, and an
  advisory feed are reasonable extensions of the V1 design. None ship
  now. Each is listed in the deferred-features table under
  `## Decision` → "What V1 does *not* ship," with the pull-trigger that
  should bring it in.

## Context

Epic [#750](https://github.com/NVIDIA/aicr/issues/750) and its four
children ([#751](https://github.com/NVIDIA/aicr/issues/751) contribution
workflow, [#752](https://github.com/NVIDIA/aicr/issues/752) fingerprint,
[#753](https://github.com/NVIDIA/aicr/issues/753) verifier,
[#754](https://github.com/NVIDIA/aicr/issues/754) bundle format) cover
the broad design. This ADR captures the V1 implementation contract that
delivers the trust handoff with cryptographic completeness, OCI-native
transport, and a CycloneDX BOM in every bundle. It defers the rest of
the broader design until demand pulls it in.

Two design tensions drive what's V1 vs deferred:

| Tension | Full design choice | V1 choice |
|---|---|---|
| Identity model | Tier A/B/C/D with signed policy file, freshness-bounded | One bundle class; verifier records the OIDC identity, maintainer review classifies |
| Re-cert on non-material edits | Material-slice canonicalizer (RFC 8785-derived, append-only versioned) suppresses re-cert on cosmetic changes | Material-slice canonicalizer ships in V1; the CI gate rolls out soft-fail first (warning-only), then hardens to required after a concrete corpus trigger fires |
| Multi-cluster attestation per recipe | Multi-instance pointer schema with primary / supplementary / negative roles | Pointer carries a list with one entry; second cluster attests via an additive PR. Schema bump (1.0 → 2.0) when roles arrive |
| Logs handling | Three signed layered predicate types (logs / redaction / augmentation) attached to the same OCI digest | Optional unsigned logs bundle as separate OCI artifact; per-file content hashes pre-committed in summary's manifest binds them to the signature |

Two surfaces from the broader design **do** ship in V1 because their
cost-to-defer is high:

- **OCI transport + in-tree pointer file.** Bundle bytes live in OCI
  (`ghcr.io/<owner>/aicr-evidence:<digest>`); the pointer file at
  `recipes/evidence/<recipe>.yaml` binds the repo to the bundle by
  content hash. Discoverability and audit trail (`git log` on the
  pointer) are worth the small added complexity over PR-attached
  tarballs. **GHCR is shown as the example throughout this ADR; any
  OCI-1.1-compliant registry is acceptable** (GHCR, GitLab Container
  Registry, Harbor, JFrog Artifactory, AWS ECR, Google Artifact
  Registry, Azure Container Registry). Contributors on corporate
  registries push to whatever their organization permits; the verifier
  reads the registry from the pointer file and uses standard ORAS
  client paths.
- **CycloneDX BOM in the summary bundle.** Per #754 and the hard
  dependency on [#739](https://github.com/NVIDIA/aicr/issues/739), the
  BOM ties the recipe to the exact image set deployed at validate
  time. Excluding it would force re-cert on every Renovate digest
  rotation and offer no audit baseline.

## Decision

### Trust model

V1's trust handoff is **signer-identity-bound, not cluster-physicality-bound**.
The bundle proves: an OIDC identity with the recorded cosign cert claims signed
this `(recipe, snapshot, validator results, BOM)` tuple at the recorded
`attestedAt` time, and every artifact in the bundle is cryptographically tied
to that signature. It does not prove the cluster the snapshot describes
physically existed — a contributor controlling their own cluster can lie to
the snapshot collectors, and per-signal corroboration that would make those
lies harder is deferred (see the deferred-features table under
`## Decision` → "Fingerprint per-signal provenance").

Concretely, this relocates the maintainer's trust judgment from "did this PR
really run?" to "do I trust this signer's claim that it did?" — a richer
artifact than today's "trust me, it passed" review surface, but the same
underlying maintainer-judgment surface. The eventual closer for the
cluster-physicality gap is the deferred Tier model (signed policy file
labeling first-party / partner / community identities) plus per-signal
fingerprint provenance, not a cryptographic primitive V1 can bolt on. V1
delivers the artifact and the verifier; tier classification arrives when
contribution volume or partner relationships pull it in.

### Evidence taxonomy

AICR carries multiple kinds of evidence with different purposes and
shapes; the V1 design treats *recipe-test attestation* as one kind in
that family, not the only one. Two kinds exist today:

- **`cncf-conformance`** (already shipped, in `pkg/evidence`).
  Per-requirement Markdown documents rendered from validator results,
  intended for CNCF conformance submission. Human-readable, not
  cryptographically signed; trust comes from the submission process,
  not the artifact itself.
- **`recipe-test-attestation`** (this ADR, V1). Signed, OCI-distributed
  in-toto Statement plus supporting bundle (recipe, snapshot, BOM,
  CTRF, manifest); intended for maintainer review of contributions
  from unreachable hardware. Cryptographically self-verifying; trust
  comes from the signer-identity claims and the bundle's content
  binding.

The two kinds share consumption surface (see V1 surface item 2 below):
`aicr evidence verify <input>` auto-detects and dispatches per kind.
They do *not* share production surface — each is emitted by the
command whose pipeline produced it. CNCF conformance evidence is
emitted alongside `aicr validate` today; recipe-test attestation is
emitted by `aicr validate --emit-attestation` (V1 surface item 1).
Production lives at the origin, where the data is; consumption unifies
under `aicr evidence` so future kinds (CycloneDX BOM-only attestation,
SLSA provenance, license attestation) plug in without further
top-level CLI growth.

**CLI flag layout — kind-scoped flag names disambiguate.** The
existing CNCF surface on `aicr validate` already uses
`Category: "Evidence"` for its flags (`--evidence-dir`,
`--cncf-submission`, `--feature`); adding the recipe-test-attestation
surface to the same category creates flag-name overlap unless each
kind is named for itself. V1 keeps the existing CNCF flags unchanged
and introduces kind-scoped flag names for the new attestation kind:

| Kind | Production flags on `aicr validate` |
|---|---|
| `cncf-conformance` | `--evidence-dir <path>` (existing), `--cncf-submission`, `--feature <name>` |
| `recipe-test-attestation` | `--emit-attestation <path>` (NEW), `--include-logs`, `--push <oci-ref>`, `--push-logs` |

Both kinds may run from a single `aicr validate` invocation (each
flag set produces its own output tree) — they are independent
pipelines that happen to consume the same recipe and snapshot inputs.
`aicr validate --help` groups both flag sets under `Evidence`; the
flag *names* tell the user which kind each one targets.

The AICRConfig structure mirrors this — `cncf` and `attestation` sit
as siblings under `spec.validate.evidence`, so a single config file
can drive either kind or both without flag-name ambiguity (see V1
surface item 1 below for the full schema).

**Ship V1 as five PRs, deferring the rest until pulled by demand.**

### V1 surface (proposed)

1. **Bundle format + pointer + `aicr validate --emit-attestation`.** Single
   in-toto Statement per recipe run, predicate type
   `https://aicr.nvidia.com/recipe-evidence/v1`, DSSE-wrapped, signed
   with cosign keyless OIDC. Summary bundle is an OCI artifact;
   optional logs bundle as a separate OCI artifact,
   contributor-controlled. The pointer file (schema 1.0, in-tree at
   `recipes/evidence/<recipe>.yaml`) is a *side effect* of
   `--emit-attestation`, not a separate command — generation, OCI push,
   signing, and pointer population happen in one invocation.

   **Canonical invocation: `--config aicr.yaml`.** PR-A extends
   `pkg/config.AICRConfig` with a `ValidateSpec` sibling of
   `RecipeSpec`/`BundleSpec`, reusing `AttestationSpec` and
   `RegistrySpec` so the cosign and OCI surfaces stay consistent
   between `bundle` and `validate`:

   ```yaml
   apiVersion: aicr.nvidia.com/v1alpha1
   kind: AICRConfig
   metadata:
     name: my-recipe
   spec:
     validate:
       input:
         recipe: r.yaml
         snapshot: s.yaml
       evidence:
         # Sibling sections per evidence kind; either or both may be
         # populated. Each block configures one kind end-to-end and is
         # independent of the others. Signing and OCI-transport
         # options nest inside the attestation block because only that
         # kind signs and pushes; the CNCF block has no need for them.
         cncf:                                 # cncf-conformance kind
           out: ./cncf-evidence                # equivalent to --evidence-dir
           cncfSubmission: false               # equivalent to --cncf-submission
           features: []                        # equivalent to --feature (repeatable)
         attestation:                          # recipe-test-attestation kind
           out: ./out                          # equivalent to --emit-attestation
           push: oci://ghcr.io/myorg/aicr-evidence  # optional; OCI push target
           includeLogs: true                   # equivalent to --include-logs
           pushLogs: false                     # equivalent to --push-logs
           signing:                            # reuses pkg/config.AttestationSpec
             enabled: true
             certificateIdentityRegexp: ^https://github\.com/myorg/.*$
             oidcDeviceFlow: false
           registry:                           # reuses pkg/config.RegistrySpec
             insecureTLS: false
             plainHTTP: false
   ```

   `signing` and `registry` reuse the existing Go types
   `pkg/config.AttestationSpec` and `pkg/config.RegistrySpec` (already
   exposed under `BundleSpec`), so cosign-identity inputs and OCI
   transport options stay consistent between `aicr bundle` and
   `aicr validate`. They nest under `evidence.attestation` rather than
   sitting at `validate` top-level because they are only meaningful
   when the attestation kind is being emitted.

   `aicr validate --config aicr.yaml` is the supported invocation;
   the CLI flag form (`--recipe`, `--snapshot`, `--emit-attestation`,
   `--push`, `--include-logs`, `--push-logs`, plus existing
   `--evidence-dir` / `--cncf-submission` / `--feature` for the CNCF
   kind) is the expanded equivalent and overrides config values.
   `aicr.yaml` flows the same contributor through `aicr recipe` →
   `aicr validate --emit-attestation` → `aicr bundle` without re-typing
   inputs.

   **Flag form (equivalent expansion):**

   ```bash
   aicr validate --recipe r.yaml --snapshot s.yaml --emit-attestation ./out
   # writes:
   #   ./out/summary-bundle/   (recipe, snapshot, BOM, CTRF, manifest, attestation)
   #   ./out/logs-bundle/      (optional; when --include-logs)
   #   ./out/pointer.yaml      (ready to copy to recipes/evidence/<recipe>.yaml)
   ```

   With an optional `--push <oci-registry>` that closes the loop:

   ```bash
   aicr validate --recipe r.yaml --snapshot s.yaml \
     --emit-attestation ./out \
     --push ghcr.io/myorg/aicr-evidence \
     [--push-logs]
   # pushes summary OCI artifact, runs cosign attest, populates
   # pointer.yaml's bundle.oci and bundle.digest fields
   # pushes logs OCI if --push-logs (else logs stay local)
   ```

   The pointer is bundle-derived; mismatches between pointer and
   bundle are integrity-chain failures. Contributors copy
   `./out/pointer.yaml` to `recipes/evidence/<recipe>.yaml` and commit.

2. **`aicr evidence` CLI family.** A typed verb group that consumes
   the evidence kinds enumerated above (see "Evidence taxonomy") with
   a single shared surface. V1 ships three subcommands:

   - **`aicr evidence verify <input>`** — full verification of a
     `recipe-test-attestation` bundle: signature, schema, inventory
     (manifest hashes), recipe ↔ snapshot fingerprint match
     (per-dimension diff), inline constraint replay, phase results,
     BOM cross-reference. Markdown + JSON output. Auto-detects kind
     from input shape and dispatches to the matching verifier (V1:
     attestation only; CNCF conformance verification slots in here
     when its trust posture is formalized).

     ```bash
     aicr evidence verify <input>

     # where <input> is auto-detected as:
     #   recipes/evidence/<recipe>.yaml      → pointer file
     #   ghcr.io/.../aicr-evidence:<digest>  → OCI reference
     #   ./bundle.tar.gz                     → tarball
     #   ./out/summary-bundle/               → unpacked directory
     ```

     Detection is by URL prefix, file extension, and directory
     existence. The four forms map to distinct workflows: pointer for
     CI, OCI for canonical artifact verification, tarball for
     air-gapped or email transport, unpacked directory for contributor
     self-debug without a packaging step. Same verification logic runs
     against any of the four.

   - **`aicr evidence list`** — enumerate evidence available locally
     or in a target OCI repository. Useful for "which recipes have
     attestations?" and "what bundles exist under
     `ghcr.io/myorg/aicr-evidence`?" without fetching their contents.
   - **`aicr evidence show <input>`** — read-only inspection of a
     bundle's predicate body, manifest, and signer claims. No
     verification; cheap to run during contributor debugging.

   Production stays at the origin (V1 surface item 1 — `aicr validate
   --emit-attestation` produces attestations; CNCF conformance evidence is
   produced by the existing `pkg/evidence` pipeline as part of validate
   today). `aicr evidence` is the *consumption* family; new evidence
   kinds plug into it without further top-level CLI growth.

3. **CI gate workflow + PR template.** Check on PRs touching
   `recipes/**` whose material slice (see "Material-slice
   canonicalization" below) differs from the slice the current pointer
   was attested against. Reads pointer, runs verify, posts Markdown
   comment. **Two-phase rollout**:

   - **Phase 1 — soft-fail (informational).** The check runs on every
     qualifying PR but emits warning-only annotations; merge is not
     blocked. Phase 1 starts when PR-C lands and stays in effect until
     the corpus trigger fires (≥ 3 distinct accelerator classes have
     at least one attested recipe with a valid pointer, OR 12 weeks
     after PR-A merges, whichever comes first).
   - **Phase 2 — hard-fail (required).** The check becomes a required
     status. Maintainers can apply an `evidence/exempt` label to bypass
     the gate for an individual PR; bypass requires a justification
     line in the PR description and is recorded for audit. Pure
     non-material edits (comments, whitespace, `displayName`,
     `description`, key-order) never reach Phase 2 because the
     canonicalizer collapses them to the same material-slice digest.

   Promotion from Phase 1 to Phase 2 is a one-line config change in
   the workflow, paired with a CONTRIBUTING update documenting the
   `evidence/exempt` policy.

4. **`maintainers:` block on `RecipeMetadataSpec`.** Required field on
   every recipe in `recipes/overlays/`, listing GitHub handle, org, and
   a durable escalation contact (DL or shared mailbox). One-time
   backfill PR populates existing recipes via `git log` heuristics.

   **Why this lives under ADR-007 rather than as a separate schema PR.**
   The `maintainers:` block is the durable contact surface this
   evidence design generates work for: re-cert prompts when a
   material-slice digest expires, advisory-revocation notifications
   when a deployed image is flagged post-merge (deferred — see the
   advisory-feed row in the deferred-features table), and
   signer-identity disputes ("who signed `recipes/evidence/<recipe>.yaml`?
   are they still the right routing target?"). Without a recipe-level
   contact, every such event has to be triaged through `git log`
   heuristics — which is exactly what the backfill PR does *once*, and
   then ages out of usefulness as authors leave teams. PR-D is
   schedule-independent of A/B/C (it can land first if convenient,
   since the field is additive metadata) but its motivation is the
   evidence lifecycle this ADR establishes.

### Material-slice canonicalization (proposed)

The material slice is the projection of the recipe-resolution surface
that affects runtime behavior. Two recipe states with the same material
slice produce the same deployed cluster; differences outside the slice
(comments, formatting, descriptive metadata) cannot. The bundle's
subject digest is computed over the canonical form of this slice, not
over the raw post-resolution YAML — so non-material edits do not
invalidate the bundle.

**What the slice includes** (any change re-certs):

- Leaf overlay's `criteria`, `componentRefs`, `constraints`,
  `validation`, `nodeScheduling` paths.
- Each `componentRef`'s resolved `{type, source, version, valuesFile,
  manifestFiles, overrides}`, recursively into included
  `valuesFile`/`manifestFiles` content.
- Mixin contents pulled in by `spec.mixins`, scoped to their
  `constraints` and `componentRefs` (mixins carry no other material
  fields by construction — see ADR-005).
- Registry entries (`recipes/registry.yaml`) referenced by the
  resolution chain: `helm.{defaultRepository, defaultChart}`,
  `kustomize.{defaultSource, defaultPath, defaultTag}`,
  `valueOverrideKeys`, `nodeScheduling`.
- `spec.maintainers` (PR-D) — a maintainer change is durable
  signal-routing metadata, not cosmetic.

**What the slice excludes** (changes pass without re-cert):

- All YAML comments, leading/trailing whitespace, blank lines,
  key-order, quoting style.
- `metadata.{displayName, description, labels, annotations}` and
  any registry `displayName`.
- Top-level `# yaml-language-server: $schema=...` directives and
  similar editor hints.

**Algorithm.** RFC 8785 (JSON Canonicalization Scheme) over the
filtered subtree, after a type-preserving YAML→JSON load and NFC
Unicode normalization on string scalars. The slice is built by
walking the resolution chain (overlay → mixins → registry →
component values → manifest files transitively) and emitting only
the included paths above. Output is the SHA-256 of the JCS bytes;
this is the value bound by the in-toto Statement's
`subject[0].digest.sha256` and the trigger for re-cert. The same walk
records each consumed input's file-bytes SHA-256 into the predicate's
`chainManifest` for forensic provenance (see "Predicate body" below
for the envelope shape and the rationale for keeping the re-cert
trigger and the forensic record separate).

**`materialSliceVersion`.** The predicate carries an integer
`materialSliceVersion` (V1 = `1`). Bundles attest under the algorithm
in effect at sign-time; the verifier loads the matching algorithm by
version. New versions append; old versions never get rewritten. This
isolates canonicalizer bug fixes (and future slice-set changes) from
historical bundles — a v2 algorithm does not invalidate v1
attestations, and the verifier carries both parsers.

**Verifier check.** The subject-digest step (verifier step 6) recomputes
the slice from the repo at HEAD using the bundle's
`materialSliceVersion` and confirms match against the predicate. A
mismatch is a hard fail; a match means the recipe is materially
unchanged since attest-time, regardless of cosmetic edits.

The CI gate (V1 surface item 3) leans on the same canonicalizer:
"PR touches `recipes/**`" is the broad trigger, but the gate only
*fails* when the material-slice digest computed from the PR head
differs from the slice the current pointer was attested against.
Comment-only PRs, Renovate-driven `displayName` rewrites, and
formatter changes pass without new evidence.

### Bundle anatomy (proposed)

Summary bundle (always published):

```text
oci://ghcr.io/<owner>/aicr-evidence:<digest>
└── (OCI artifact whose layers contain:)
    ├── attestation.intoto.json     # DSSE-wrapped, cosign keyless signed
    ├── recipe.yaml                 # post-resolution canonical YAML
    ├── snapshot.yaml               # cluster snapshot at validate-time
    ├── bom.cdx.json                # CycloneDX BOM (per #739)
    ├── ctrf/
    │   ├── deployment.json
    │   ├── performance.json
    │   └── conformance.json
    └── manifest.json               # file inventory + per-file sha256
```

Optional logs bundle (contributor-controlled; absent when not published):

```text
oci://ghcr.io/<owner>/aicr-evidence-logs:<digest>
└── phases/
    ├── deployment/logs/*
    ├── performance/logs/*
    └── conformance/logs/*
```

Logs are not signed-as-a-whole. The summary's `manifest.json`
pre-commits per-file hashes for any log file that *would* be in the
logs bundle. Anyone fetching the logs bundle later can verify
file-by-file against the manifest pre-commit; tampering is detectable
without a separate signature event. This is the V1 mechanism;
signed-logs predicates (with redaction and augmentation variants)
arrive when demand justifies — see "Future direction."

### Predicate body

```yaml
# https://aicr.nvidia.com/recipe-evidence/v1
schemaVersion: 1.0.0
materialSliceVersion: 1
attestedAt: 2026-05-08T10:23:11Z
aicrVersion: v0.13.0
validatorCatalogVersion: v2.4.0
validatorImages:
  - image: ghcr.io/nvidia/aicr/validator-deployment
    digest: sha256:...
  - image: ghcr.io/nvidia/aicr/validator-performance
    digest: sha256:...
fingerprint:
  service: { value: eks }
  accelerator: { value: h100 }
  os: { value: ubuntu }
  k8sVersion: { value: "1.33.4" }
  region: { value: us-west-2 }
  nodeCount: { value: 12 }
criteriaMatch:
  matched: true
  perDimension:
    service: { recipeRequires: eks, fingerprintProvides: eks, match: true }
    accelerator: { recipeRequires: h100, fingerprintProvides: h100, match: true }
    os: { recipeRequires: ubuntu, fingerprintProvides: ubuntu, match: true }
    intent: { recipeRequires: training, fingerprintProvides: training, match: true }
    platform: { recipeRequires: kubeflow, fingerprintProvides: kubeflow, match: true }
phases:
  deployment: { passed: 12, failed: 0, skipped: 0, ctrfDigest: sha256:... }
  performance: { passed: 3, failed: 0, skipped: 0, ctrfDigest: sha256:... }
  conformance: { passed: 9, failed: 0, skipped: 0, ctrfDigest: sha256:... }
bom:
  format: CycloneDX
  version: "1.5"
  digest: sha256:...
  imageCount: 24
manifest:
  digest: sha256:...
  fileCount: 9
chainManifest:
  leaf:
    path: recipes/overlays/h100-eks-ubuntu-training.yaml
    sha256: <hex>
  inputs:
    - { kind: mixin,    path: recipes/mixins/os-ubuntu.yaml,           sha256: <hex> }
    - { kind: mixin,    path: recipes/mixins/platform-kubeflow.yaml,   sha256: <hex> }
    - { kind: registry, path: recipes/registry.yaml,                   sha256: <hex> }
    - { kind: values,   path: recipes/components/gpu-operator/values.yaml, sha256: <hex> }
    - { kind: values,   path: recipes/components/network-operator/values.yaml, sha256: <hex> }
```

The in-toto Statement's `subject[0].digest.sha256` (outside the
predicate body, in the wrapping Statement) is the **material digest**:
`sha256(JCS(material-slice(post-resolution recipe)))` under the
algorithm identified by `materialSliceVersion`. This is the value the
verifier recomputes in step 6, and it is what determines whether a
recipe edit needs new evidence:

- Non-material edits (comments, whitespace, `displayName`,
  `description`, key-order) produce the same material digest and
  pass without new evidence.
- Material edits (chart version, criteria, constraints, override
  values, manifest content) produce a different material digest and
  require a fresh attestation.

The predicate body's `chainManifest` is the **forensic provenance**:
it records the unresolved leaf overlay plus every input the resolver
consumed (mixins, registry entries, component values, manifest files),
each with its own SHA-256 of the file bytes at attest-time. This is
*not* what the signature binds to — the material digest is — but it
lets the verifier (and post-incident investigators) tell *why* the
material digest changed when one does. A registry edit, a shared
mixin tweak, or a `recipes/components/<name>/values.yaml` change shows
up as a specific input hash mismatch; an edit isolated to the leaf
overlay shows up as a leaf hash mismatch with all inputs intact. This
ergonomic separation costs nothing at sign-time (the resolver already
walks every input) and pays off heavily at re-cert review.

`materialSliceVersion` continues to govern algorithm evolution: V1
ships version `1`; future canonicalizer revisions append
(`materialSliceVersion: 2`, …) without invalidating bundles signed
under the previous version. Verifiers carry both algorithm parsers.

The `manifest.digest` field binds the manifest to the signature, which
in turn binds every supporting file (snapshot, BOM, CTRF) by the
hashes the manifest enumerates. Without this field, only the material
digest would be bound — adversaries could swap any other file
undetected. The verifier's inventory check is what closes the chain.

### Pointer schema (1.0) (proposed)

```yaml
# recipes/evidence/<recipe>.yaml — schema 1.0, single-attestation list
schemaVersion: 1.0.0
recipe: h100-eks-ubuntu-training
attestations:
  - bundle:
      oci: ghcr.io/<owner>/aicr-evidence:<digest>
      digest: sha256:abc123...
      predicateType: https://aicr.nvidia.com/recipe-evidence/v1
    signer:
      identity: <oidc-subject>
      issuer: <oidc-issuer-url>
      rekorLogIndex: 91234567
    attestedAt: 2026-05-08T10:23:11Z
    fingerprint:
      service: eks
      accelerator: h100
      os: ubuntu
      k8sVersion: "1.33.4"
    criteriaMatch:
      matched: true
    phaseSummary:
      deployment: { passed: 12, failed: 0 }
      performance: { passed: 3, failed: 0 }
      conformance: { passed: 9, failed: 0 }
    logsBundle:           # optional; absent when contributor doesn't publish
      oci: ghcr.io/<owner>/aicr-evidence-logs:<digest>
      digest: sha256:def456...
```

`attestations` is a **list** from day one (length 1 in V1). When
multi-instance arrives, additional entries append; the schema 2.0
bump introduces a `role:` field and pointer rotation. V1 readers
treat absent `role:` as `primary`. This avoids a breaking schema
transition for multi-instance.

The pointer is bundle-derived; `aicr validate --emit-attestation`
regenerates it from the OCI artifact (or the locally-emitted bundle
directory, before push). Mismatches between pointer and bundle are
**integrity-chain failures**, not clerical errors — the bundle is
authoritative; the pointer is a denormalized cache.

### Verifier steps (proposed)

`aicr evidence verify recipes/evidence/<recipe>.yaml` (or any
auto-detected input form — OCI ref, tarball, unpacked directory):

1. **Schema-validate** the pointer file.
2. **Cosign signature verify** the in-toto Statement against Rekor
   (default; `--no-rekor` skips Rekor and uses bundled cert + sig).
3. **Schema-validate** the predicate body.
4. **Materialize the bundle.** From an OCI ref: `oras pull` and
   confirm the artifact's digest matches `pointer.attestations[*].bundle.digest`
   (or the user-supplied digest when `--bundle` is used without a
   pointer). From a tarball: extract to a temp dir; recompute the
   tarball's SHA-256 and confirm match against any digest claim in
   scope. From an unpacked directory: read in place; no digest claim
   to check at this step (the inventory check in step 5 is what binds
   files to the signature). In all four forms, downstream steps are
   identical because they operate on the materialized file tree.
5. **Inventory check.** Verify every file in `manifest.json` exists
   in the bundle; recompute SHA-256 per file; confirm match. Confirm
   `manifest.digest` matches predicate.
6. **Material digest + chain manifest check.** Two-part:

   a. **Material digest (re-cert trigger).** Load the algorithm
      identified by the predicate's `materialSliceVersion`; recompute
      `sha256(JCS(material-slice(post-resolution recipe in repo at HEAD)))`;
      confirm match against the in-toto Statement's
      `subject[0].digest.sha256`. Mismatch is a hard fail (re-cert
      required). Non-material edits (comments, formatting,
      `displayName`, `description`, key-order) produce the same digest
      and pass.

   b. **Chain manifest (forensic check).** Walk the resolution chain
      from the same HEAD; for each entry recorded in
      `predicate.chainManifest.{leaf, inputs[]}`, recompute the
      file's SHA-256 and compare. The check is *informational* in
      V1: any mismatch is reported in the verifier's Markdown output
      ("input X changed since attest-time") but does not by itself
      fail verification — the material-digest check above is the
      authoritative re-cert signal. The chain manifest's job is to
      tell the maintainer *why* a re-cert is needed (leaf vs. shared
      mixin vs. registry vs. component values), not to add a second
      hard-fail surface. (V2 may promote individual chain mismatches
      to hard-fail signals once the slice-set has stabilized.)
7. **Per-dimension fingerprint match.** Run `Fingerprint.Match(recipe.criteria)`
   from #752; confirm `criteriaMatch.matched: true`; render per-dimension
   diff in Markdown so reviewers see exactly which dimensions matched.
8. **Inline constraint replay.** Run the snapshot through the recipe's
   inline constraints (the `aicr validate --no-cluster` deterministic
   path) and confirm the recorded pass/fail matches what the bundle
   claims. This is what makes the verifier independent — it doesn't
   trust the constraint outcome the contributor recorded; it
   recomputes it.
9. **Phase results surface.** Read CTRF results from the bundle; verify
   per-phase `ctrfDigest` against the predicate.
10. **BOM cross-reference.** Confirm `bom.digest` matches predicate;
    count chart-default sub-images for the disclosure
    ("BOM contains N chart-default sub-images NOT covered by this
    attestation; admission-time policy required for full coverage").
11. **(Optional) Logs bundle verification.** If `pointer.attestations[*].logsBundle`
    is present, pull, recompute per-file hashes, confirm match against
    summary's manifest pre-commit. Logs bundle absence is **not** a
    failure.
12. **Render Markdown summary.** Includes signer identity, per-phase
    results, per-dimension fingerprint match, BOM disclosure, and
    sub-image count.

Exit codes (proposed):

- `0` — valid + passed (every check passed)
- `1` — valid + failed checks (informational; signature and integrity
  intact, but recorded validator results show failures — known-issue
  documentation, work-in-progress, hardware-specific limitations)
- `2` — invalid (signature mismatch, schema invalid, inventory
  mismatch, material-digest mismatch, fingerprint not matched, BOM
  mismatch, OR no pointer file present for a touched recipe).
  Chain-manifest mismatches alone do not produce exit `2` in V1 —
  they surface as informational rows in the Markdown summary so the
  maintainer can see which input drifted; only material-digest drift
  is authoritative.

The CI gate explicitly checks for pointer file presence: a PR that
touches `recipes/overlays/<recipe>.yaml` without producing a fresh
`recipes/evidence/<recipe>.yaml` (whether new or updated to the
new recipe state) fails with a clear "no evidence bundle present"
message, satisfying #751 acceptance criterion 1.

### Forward-compatibility hooks

Three V1 choices preserve future evolution at near-zero cost:

1. **Predicate type is `recipe-evidence/v1`; `materialSliceVersion`
   handles canonicalizer revs without a predicate bump.** A breaking
   change to the material-slice algorithm ships as
   `materialSliceVersion: 2` under the same `/v1` predicate type;
   verifiers carry both algorithm parsers (append-only). The predicate
   type bumps to `/v2` only when other parts of the predicate body
   (signer envelope, phase shape, fingerprint surface) change in a way
   the algorithm version cannot express.
2. **`pointer.attestations` is a list from day one.** Multi-instance
   in schema 2.0 is additive: more entries, plus a `role:` field
   defaulting to `primary`. No structural break for V1 pointers.
3. **The verifier takes a single positional input** that auto-detects
   the form (pointer / OCI / tarball / directory), so the CLI surface
   stays small even as transport options grow. New input forms in V2
   (e.g., bundles fetched via custom resolver, signed registry refs)
   slot in as additional auto-detect cases without breaking V1
   invocations.

### What V1 does *not* ship

| Deferred | Pulled by |
|---|---|
| Tier A/B/C/D identity policy file | First partner relationship requests a non-community trust label, OR community contribution volume creates review fatigue that tier filtering would relieve. |
| Multi-instance pointer (schema 2.0) with primary / supplementary / negative roles | Two contributors attest the same recipe from different clusters, OR a "this didn't work for me" negative attestation needs to coexist with a passing primary. |
| Signed layered predicate types (logs / redaction / augmentation) | Contributor asks to publish redacted logs, OR third party wants to add an independent re-run with its own signer. V1's manifest-pre-commit binding handles "publish logs later" already. |
| Re-cert age cutoffs (24mo hard, 23mo bot) | First bundle ages past 12 months. Document the policy in CONTRIBUTING; defer the bot. |
| Catalog-MAJOR re-cert trigger | Catalog SemVer contract filed (follow-on to [#660](https://github.com/NVIDIA/aicr/issues/660)). Trigger is dormant until the contract exists. |
| Advisory feed (`NVIDIA/aicr-advisories` OSV) | First post-merge incident requires revocation of an attestation. |
| Reusable workflow (`workflow_call`) | Multiple contributors ask for a turn-key path AND have hardware they can register against a public fork (corporate policy commonly disallows). Local `cosign attest` is documented in CONTRIBUTING. |
| Mirror bot + archive registry | First contributor's OCI registry goes dark on an accepted bundle. |
| Fingerprint per-signal provenance | First Tier-C-equivalent contribution gets pushback for "is this cluster real?" V1 records resolved `{value}` only; per-signal sources (`signals: [kubelet, dcgm, imds]`, `confidence: high`) are additive predicate fields. |

Each row's pull-trigger is the demand signal that should bring the
feature in. Per-row design happens at the demand event, not now —
see `## Future direction`.

## Consequences

### Positive

- **Trust handoff works for community contributions today.** A
  contributor on AWS GB200 produces a signed bundle with a few
  commands; a maintainer reads the verifier's Markdown summary and
  approves on judgment. The PR stops stalling.
- **Cryptographically complete.** Every file in the bundle is bound
  to the signature via the manifest digest. Tampering with snapshot,
  CTRF, or BOM after sign-time is detectable; the verifier's inventory
  check enforces it.
- **OCI-native transport with audit trail.** `git log
  recipes/evidence/<recipe>.yaml` shows every signing event;
  content-addressed pulls catch registry compromise.
- **BOM in every bundle.** Ties the recipe to the exact image set
  deployed, satisfying #739's audit requirement and giving downstream
  consumers a stable claim.
- **Per-dimension fingerprint match.** The verifier's Markdown summary
  shows exactly which `criteria` dimensions matched (and against what
  fingerprint values), not just a yes/no.
- **Inline constraint replay.** The verifier independently confirms
  constraint pass/fail rather than trusting the recorded outcome.
- **Standard tooling.** in-toto + cosign keyless + Rekor + CycloneDX
  + OCI artifacts. Third parties can verify any AICR bundle with
  vanilla `cosign verify-attestation` against public Rekor; the AICR
  predicate parser is needed only to render the human-readable
  summary.
- **Bounded scope.** Five PRs, ~2300 lines of code, no new external
  services. Implementation effort fits a sprint plus.
- **Forward-compat seams visible.** Each deferred feature has a
  documented pull-trigger in the deferred-features table. No surprise
  rewrites; expansion is additive.

### Negative

- **Trust is signer-identity-bound, not cluster-physicality-bound.**
  See `## Decision` → "Trust model." A contributor controlling their
  own cluster can lie to the snapshot collectors; V1 records the
  resolved fingerprint values, and per-signal provenance (which
  collector signal contributed, with what confidence) is deferred.
  Maintainer review of the cosign cert claims compensates — same
  judgment surface as today's PR-only review, with a richer artifact
  attached. The Tier policy file (deferred) is what eventually closes
  this gap; per-signal provenance alone does not.
- **Material-slice changes trigger re-cert; the slice definition is
  itself a maintenance surface.** Edits to the leaf overlay's
  `criteria`/`componentRefs`/`constraints`/`validation`/`nodeScheduling`,
  to `componentRefs`-resolved values/manifest files, to mixin
  `constraints` or `componentRefs`, or to registry entries pulled
  through the resolution chain re-cert every recipe whose slice
  touches them. Comment-only and metadata-only edits pass without
  re-cert. The slice-set (what's in vs. out of the material projection)
  is part of the algorithm version: tightening or relaxing it requires
  bumping `materialSliceVersion`. Initial slice-set is conservative
  (errs toward "include" for safety); empirical pull-trigger to relax
  is contributors flagging false positives.
- **Soft-fail window can hide real evidence drift.** During Phase 1,
  a PR that legitimately needs new evidence merges with a warning the
  reviewer may overlook. This is the cost of not blocking on hardware
  availability before the corpus exists; the trigger conditions
  (≥ 3 accelerator classes attested, OR 12 weeks) are calibrated to
  end Phase 1 before drift accumulates. If the trigger drags, the
  corpus is too thin to enforce a gate against in the first place.
- **No tier label distinguishes first-party from community evidence.**
  A bundle signed by NVIDIA's CI looks the same to the verifier as
  one signed by an unfamiliar fork. Maintainers eyeball the cosign
  cert claims to tell them apart. Acceptable until a real partner
  relationship requests Tier B.
- **No long-term archive.** If the contributor deletes the bundle
  from their OCI registry after merge, the verifiable record is the
  Rekor entry only — bytes may be unrecoverable. Acceptable until
  first incident; the mirror bot is the answer.
- **Single-instance pointer.** Until schema 2.0, only one attestation
  per recipe lives in the pointer at a time. A second contributor's
  attestation overwrites the first; multi-cluster diversity isn't
  captured.

## Alternatives Considered

The V1 design space is wider than the chosen surface. The alternatives
below were evaluated and rejected (or deferred) for the reasons given.

### PR-attached tarball vs. OCI artifact (rejected)

A bundle attached to the PR description as a release asset or a
`*.tar.gz` checked into `recipes/evidence/`. Cheaper to ship — no
registry dependency, no `oras` in the toolchain.

Rejected because (1) tarballs in git balloon repo size and break
content-addressed updates, (2) GitHub release assets have no audit
trail and can be silently replaced, (3) cosign attestations naturally
live next to OCI artifacts via Rekor — the in-tree-tarball path forces
a parallel signing surface, and (4) the in-tree pointer file gives the
same `git log` audit benefit a tarball would, without the size cost.

### Sigstore bundle format vs. in-toto Statement (rejected)

The sigstore "bundle" format (cert + sig + Rekor proof in one envelope)
is increasingly the default for `cosign sign-blob`. It would let the
verifier skip a Rekor round-trip.

Rejected because in-toto Statements are the established industry
standard for build/test attestations, third-party verifiers
(`cosign verify-attestation`) consume them natively, and the predicate
type discriminates V1 from future V2 cleanly. Sigstore-bundle remains
an option for the cosign signing path *underneath* the in-toto
envelope; this is not an either/or at the bundle layer.

### Single combined bundle vs. split summary/logs (rejected)

Keep summary and logs in one OCI artifact; sign once.

Rejected because logs are large, contributor-controlled (some clusters
generate GBs per validator phase), and frequently sensitive (cluster
identifiers, internal endpoints). A single bundle forces every
publication to ship logs, raises registry costs, and leaks operational
detail into every public attestation. The split design lets logs stay
local until the contributor opts in via `--push-logs`, while the
manifest pre-commit binding still gives forensic recoverability.

### KMS-backed signing vs. cosign keyless (deferred, not rejected)

KMS-backed signing (AWS KMS / GCP KMS / Azure Key Vault / HSM-backed
PKCS#11) gives a stable, organizationally-controlled signing identity
that does not depend on OIDC token issuers.

Deferred to V2 under the same predicate type — `predicateType` does
not change, only the `signer.issuer` claim shape. V1 ships keyless
because contributors targeting unreachable hardware do not have NVIDIA
KMS access, and corporate OIDC (GitHub Actions OIDC, Azure AD) is
already the friction-free path. KMS arrives when a partner relationship
or NVIDIA-internal CI path requires it.

### git-LFS vs. OCI for transport (rejected)

Store bundle bytes in git-LFS at `recipes/evidence-bundles/<recipe>/`.
Eliminates the OCI-registry dependency.

Rejected because (1) LFS bytes still inflate clone size for every
contributor who only needs the recipe text, (2) GitHub LFS has bandwidth
quotas that incidents can blow through, (3) LFS objects are mutable on
their backing store — content-addressed pulls don't transfer, and
(4) cosign tooling has no LFS integration, so the signing surface would
fragment.

### `aicr verify-evidence` top-level verb vs. `aicr evidence verify` family (rejected)

Earlier drafts of this ADR proposed a standalone `aicr verify-evidence`
top-level command, paired with `aicr validate --emit-attestation` for
production. Simpler to type; one-to-one mapping between produce and
verify.

Rejected because (1) AICR already groups verbs under nouns
(`aicr snapshot`, `aicr recipe`, `aicr validate`, `aicr bundle`,
`aicr query`); a standalone `verify-evidence` breaks that pattern,
(2) AICR carries multiple kinds of evidence with different shapes
(`pkg/evidence` ships CNCF conformance evidence today; this ADR adds
`recipe-test-attestation`), and a standalone verb fixes the surface
to one kind, and (3) future evidence kinds (CycloneDX BOM-only
attestation, SLSA provenance, license attestation) would each need
their own top-level verb. The `aicr evidence verify` / `list` /
`show` family handles all kinds under one surface and leaves room
for future read-only operations (`diff`, `push`, `archive`) without
top-level pollution.

Production stays at the origin (`aicr validate --emit-attestation`)
because evidence is a side-effect of the pipeline that produced its
inputs. Re-emitting via `aicr evidence emit` would either re-run
validate (defeating the purpose of an offline verifier) or reach into
validate's stored output (a hidden coupling). Keeping production at
the origin keeps the data flow obvious; consumption unifying under
`evidence` keeps the inspection surface coherent.

### Post-resolution-only subject digest vs. material slice + chain manifest (rejected)

Earlier drafts bound the subject digest to the *post-resolution recipe*
as a single value: `sha256(canonicalize(post-resolution recipe YAML))`.
Simpler — one hash to compute, one to verify.

Rejected because (1) it couples bundle validity to resolver determinism
across the entire overlay → mixin → registry → component-values chain
(a change to ADR-005's resolver semantics would silently invalidate
every prior bundle even when the leaf overlay is byte-identical), and
(2) it gives the verifier no way to tell *which* input changed when
the digest does change — registry-edit, mixin-tweak, and
component-values-update all surface as a single opaque mismatch. The
chosen design splits the two concerns: the in-toto subject is the
material-slice digest (re-cert trigger; what the signature is bound
to), and the predicate's `chainManifest` records the unresolved leaf
plus per-input hashes (forensic provenance; tells the maintainer why
re-cert is needed). This is additive in cost (the resolver already
walks every input) and materially better in re-cert ergonomics.

### Single attestation per recipe vs. list from day one (rejected)

V1's `pointer.attestations` could be a single object instead of a list
of one. Slightly simpler schema today.

Rejected because the multi-instance schema 2.0 transition (one
attestation per cluster) would then require a breaking schema-version
bump and a migration shim. Shipping a list of one in V1 makes schema
2.0 purely additive — new entries plus an optional `role:` field —
and verifier readers do not have to discriminate by schema version.

## Future direction

The deferred-features table in `## Decision` → "What V1 does *not* ship"
names every V2 candidate with its pull-trigger and compatibility note
— that is the canonical list. Each row is a *placeholder*, not a
design: when a row's trigger fires, the V2 work gets its own tracking
issue under [#750](https://github.com/NVIDIA/aicr/issues/750) and the
shape is decided then, against the demand event that pulled it in.
This ADR deliberately does not pre-design that work. Earlier drafts
sketched detailed V2 surfaces (tier-policy file layout, schema 2.0
fields, four predicate types, OSV advisory feed shape, mirror bot
trigger) — those sketches were removed because they implied
commitments the demand event has not yet justified.

## Adoption plan

1. **This ADR lands.** Sets policy, no code changes.
2. **PR-A: bundle format + pointer + `aicr validate --emit-attestation`.**
   Ships the `/v1` predicate; the OCI summary bundle layout (recipe +
   snapshot + BOM + CTRF + manifest); the optional logs bundle; the
   manifest pre-commit binding; the cosign keyless signing path; the
   pointer schema 1.0 (`docs/spec/recipe-evidence-pointer-v1.md` with
   JSON Schema); and `--emit-attestation` writing the pointer file
   alongside the bundle directories. Optional `--push <oci-registry>`
   handles the OCI upload, cosign attest, and pointer population in one
   command. Updates `pkg/bundler/attestation` with the new predicate
   type. Pulls the BOM from the existing #739 pipeline. Extends
   `pkg/config.AICRConfig` (apiVersion `aicr.nvidia.com/v1alpha1`,
   additive) with a `ValidateSpec` sibling of `RecipeSpec`/`BundleSpec`,
   reusing `AttestationSpec` and `RegistrySpec` so cosign and OCI
   surfaces stay consistent across `bundle` and `validate`.
3. **PR-B: `aicr evidence` CLI family.** Adds `aicr evidence verify`
   (single positional input — pointer / OCI / tarball / directory,
   auto-detected — twelve verification steps, three exit codes,
   Markdown + JSON output), `aicr evidence list`, and `aicr evidence
   show`. Depends on PR-A (predicate parsing + pointer parsing).
   Future evidence kinds (CNCF conformance, BOM-only, SLSA provenance)
   plug in as additional verifier dispatches under the same family
   without further top-level CLI growth.
4. **PR-C: CI gate workflow + PR template.** Required check on PRs
   touching `recipes/**`. Depends on PR-B.
5. **PR-D: `maintainers:` block schema + CI presence gate + backfill
   PR.** Schedule-independent of A/B/C — the field is additive
   metadata and can land first if convenient — but motivated by this
   ADR's evidence lifecycle (durable contact for re-cert prompts,
   advisory revocations, and signer-identity disputes).

PR-A is the foundation. PR-B depends on PR-A. PR-C depends on PR-B.
PR-D is schedule-independent and can land first if convenient; its
motivation, not its dependencies, is the evidence lifecycle.

When V1 ships and feedback lands, consult the deferred-features table
under `## Decision`. **Each deferred feature has a documented
pull-trigger; let demand decide what V2 brings in.** Don't pre-build.

## References

### Standards and specifications

- [in-toto Attestation Framework](https://github.com/in-toto/attestation) —
  predicate envelope and Statement shape used for the
  `recipe-evidence/v1` predicate type.
- [DSSE (Dead Simple Signing Envelope)](https://github.com/secure-systems-lab/dsse) —
  signature envelope wrapping the in-toto Statement.
- [Sigstore Cosign](https://docs.sigstore.dev/cosign/overview/) —
  keyless signing path used in V1; reference for `cosign attest` and
  `cosign verify-attestation`.
- [Fulcio](https://github.com/sigstore/fulcio) — short-lived cert
  authority that issues the OIDC-bound signing cert recorded in the
  bundle's `signer.identity` and `signer.issuer` claims.
- [Rekor](https://github.com/sigstore/rekor) — transparency log; the
  verifier's default cross-check for `signer.rekorLogIndex`.
- [CycloneDX 1.5](https://cyclonedx.org/specification/overview/) —
  BOM format used for the `bom.cdx.json` artifact in every summary
  bundle, per [#739](https://github.com/NVIDIA/aicr/issues/739).
- [OCI Distribution Spec 1.1](https://github.com/opencontainers/distribution-spec) —
  registry transport for the summary and (optional) logs bundles.
- [OCI Image Spec 1.1 — Artifacts](https://github.com/opencontainers/image-spec/blob/main/manifest.md) —
  artifact-type media field used for AICR evidence bundles.
- [ORAS](https://oras.land/) — OCI artifact transport library
  expected to back the `--push` and verifier `oras pull` paths.
- [RFC 8785 — JSON Canonicalization Scheme (JCS)](https://www.rfc-editor.org/rfc/rfc8785) —
  canonical form for the material-slice subject digest.
- [CTRF](https://ctrf.io/) — common test-result format consumed from
  the validator phases for `phaseSummary`.

### Related ADRs

- [ADR-002: Validator V2](002-validatorv2-adr.md) — the validator
  pipeline that produces phase results consumed by this bundle.
- [ADR-005: Overlay Refactoring](005-overlay-refactoring.md) — the
  resolver chain (overlays → mixins → registry → component values)
  enumerated in this ADR's `chainManifest` for forensic provenance.
  The bundle's material digest is computed over the *post-resolution
  material slice*, so resolver determinism still matters; the chain
  manifest lets the verifier diagnose which input caused a digest
  change without re-running the resolver.
- [ADR-006: Container Image Pinning Policy](006-image-pinning-policy.md) —
  pinning surface this ADR scopes to (recipe + chart-pin + digest-pin);
  per-component admission-time digest verification stays out of scope.

### Tracking issues

- [#739](https://github.com/NVIDIA/aicr/issues/739) — CycloneDX BOM
  pipeline (hard dependency for V1).
- [#745](https://github.com/NVIDIA/aicr/issues/745) — admission-time
  image policy (out of scope for this ADR).
- [#750](https://github.com/NVIDIA/aicr/issues/750) — epic; verifiable
  recipe test evidence.
- [#751](https://github.com/NVIDIA/aicr/issues/751) — contribution
  workflow.
- [#752](https://github.com/NVIDIA/aicr/issues/752) — fingerprint
  match.
- [#753](https://github.com/NVIDIA/aicr/issues/753) — verifier.
- [#754](https://github.com/NVIDIA/aicr/issues/754) — bundle format.
