# ADR-007: Verifiable Recipe Test Evidence

> **Status:** Proposed (design-only; not implemented). This ADR specifies
> the V1 contract. Implementation lands as five follow-on PRs tracked
> under [#750](https://github.com/NVIDIA/aicr/issues/750) and its children
> ([#751](https://github.com/NVIDIA/aicr/issues/751)–[#754](https://github.com/NVIDIA/aicr/issues/754)).
> Bundle formats, CLI flags, schema fields, and verifier behavior
> described below are future intent, not current behavior.

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
- **Re-running validators in `verify-evidence`.** Verification is an
  offline cryptographic + schema operation; re-running validators
  defeats the purpose and would require maintainers to have the
  hardware they don't have.
- **Pre-building features without demand.** Tier policies, multi-instance
  pointers, signed layered predicate types, material-slice canonicalization,
  re-cert automation, and an advisory feed are reasonable extensions of
  the V1 design. None ship now. Each is sketched in "Future direction"
  below with the pull-trigger that should bring it in.

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
| Re-cert on non-material edits | Material-slice canonicalizer (RFC 8785-derived, append-only versioned) suppresses re-cert on cosmetic changes | Any recipe edit triggers re-cert; the cost is acceptable until a real attested-recipe corpus exists |
| Multi-cluster attestation per recipe | Multi-instance pointer schema with primary / supplementary / negative roles | Pointer carries a list with one entry; second cluster attests via an additive PR. Schema bump (1.0 → 2.0) when roles arrive |
| Logs handling | Three signed layered predicate types (logs / redaction / augmentation) attached to the same OCI digest | Optional unsigned logs bundle as separate OCI artifact; per-file content hashes pre-committed in summary's manifest binds them to the signature |

Two surfaces from the broader design **do** ship in V1 because their
cost-to-defer is high:

- **OCI transport + in-tree pointer file.** Bundle bytes live in OCI
  (`ghcr.io/<owner>/aicr-evidence:<digest>`); the pointer file at
  `recipes/evidence/<recipe>.yaml` binds the repo to the bundle by
  content hash. Discoverability and audit trail (`git log` on the
  pointer) are worth the small added complexity over PR-attached
  tarballs.
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
lies harder is deferred (see `Future direction`, "Fingerprint per-signal
provenance").

Concretely, this relocates the maintainer's trust judgment from "did this PR
really run?" to "do I trust this signer's claim that it did?" — a richer
artifact than today's "trust me, it passed" review surface, but the same
underlying maintainer-judgment surface. The eventual closer for the
cluster-physicality gap is the deferred Tier model (signed policy file
labeling first-party / partner / community identities) plus per-signal
fingerprint provenance, not a cryptographic primitive V1 can bolt on. V1
delivers the artifact and the verifier; tier classification arrives when
contribution volume or partner relationships pull it in.

**Ship V1 as five PRs, deferring the rest until pulled by demand.**

### V1 surface (proposed)

1. **Bundle format + pointer + `aicr validate --emit-evidence`.** Single
   in-toto Statement per recipe run, predicate type
   `https://aicr.nvidia.com/recipe-evidence/v1`, DSSE-wrapped, signed
   with cosign keyless OIDC. Summary bundle is an OCI artifact;
   optional logs bundle as a separate OCI artifact,
   contributor-controlled. The pointer file (schema 1.0, in-tree at
   `recipes/evidence/<recipe>.yaml`) is a *side effect* of
   `--emit-evidence`, not a separate command — generation, OCI push,
   signing, and pointer population happen in one invocation:

   ```bash
   aicr validate --recipe r.yaml --snapshot s.yaml --emit-evidence ./out
   # writes:
   #   ./out/summary-bundle/   (recipe, snapshot, BOM, CTRF, manifest, attestation)
   #   ./out/logs-bundle/      (optional; when --include-logs)
   #   ./out/pointer.yaml      (ready to copy to recipes/evidence/<recipe>.yaml)
   ```

   With an optional `--push <oci-registry>` that closes the loop:

   ```bash
   aicr validate --recipe r.yaml --snapshot s.yaml \
     --emit-evidence ./out \
     --push ghcr.io/myorg/aicr-evidence \
     [--push-logs]
   # pushes summary OCI artifact, runs cosign attest, populates
   # pointer.yaml's bundle.oci and bundle.digest fields
   # pushes logs OCI if --push-logs (else logs stay local)
   ```

   The pointer is bundle-derived; mismatches between pointer and
   bundle are integrity-chain failures. Contributors copy
   `./out/pointer.yaml` to `recipes/evidence/<recipe>.yaml` and commit.

2. **`aicr verify-evidence` CLI.** Full verification: signature, schema,
   inventory (manifest hashes), recipe ↔ snapshot fingerprint match
   (per-dimension diff), inline constraint replay, phase results, BOM
   cross-reference. Markdown + JSON output. Single positional argument
   accepts any of four input forms:

   ```bash
   aicr verify-evidence <input>

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

3. **CI gate workflow + PR template.** Required check on PRs touching
   `recipes/**`. Reads pointer, runs verify, posts Markdown comment.
   PR without a pointer file for a touched recipe fails the gate.

4. **`maintainers:` block on `RecipeMetadataSpec`.** Required field on
   every recipe in `recipes/overlays/`, listing GitHub handle, org, and
   a durable escalation contact (DL or shared mailbox). One-time backfill
   PR populates existing recipes via `git log` heuristics.

### Bundle anatomy (proposed)

Summary bundle (always published):

```text
oci://ghcr.io/<owner>/aicr-evidence:<digest>
└── (OCI artifact whose layers contain:)
    ├── attestation.intoto.jsonl    # DSSE-wrapped, cosign keyless signed
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
```

`subject.digest` is `sha256(canonicalize(post-resolution recipe YAML))`
where canonicalize is "sort map keys, strip comments, normalize line
endings, UTF-8 encode" — a small helper, not the RFC 8785-derived
canonicalizer in the full design. Any recipe edit triggers re-cert;
the canonicalizer that would suppress re-cert on non-material edits
is deferred (see "Future direction").

The `manifest.digest` field binds the manifest to the signature, which
in turn binds every supporting file (snapshot, BOM, CTRF) by the
hashes the manifest enumerates. Without this field, only `recipe.yaml`
would be cryptographically bound — adversaries could swap any other
file undetected. The verifier's inventory check is what closes the
chain.

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

The pointer is bundle-derived; `aicr validate --emit-evidence`
regenerates it from the OCI artifact (or the locally-emitted bundle
directory, before push). Mismatches between pointer and bundle are
**integrity-chain failures**, not clerical errors — the bundle is
authoritative; the pointer is a denormalized cache.

### Verifier steps (proposed)

`aicr verify-evidence recipes/evidence/<recipe>.yaml` (or any
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
6. **Subject digest check.** Recompute
   `sha256(canonicalize(post-resolution recipe in repo at HEAD))`;
   confirm match against the in-toto Statement's `subject.digest`.
   Any recipe drift since attest-time is a hard fail.
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
  mismatch, subject digest mismatch, fingerprint not matched, BOM
  mismatch, OR no pointer file present for a touched recipe)

The CI gate explicitly checks for pointer file presence: a PR that
touches `recipes/overlays/<recipe>.yaml` without producing a fresh
`recipes/evidence/<recipe>.yaml` (whether new or updated to the
new recipe state) fails with a clear "no evidence bundle present"
message, satisfying #751 acceptance criterion 1.

### Forward-compatibility hooks

Three V1 choices preserve future evolution at near-zero cost:

1. **Predicate type is `recipe-evidence/v1`.** When the material
   classifier or other breaking-change V2 work ships, becomes a clean
   `/v2` — no migration shim, no schema-version negotiation. Verifiers
   carry both parsers (append-only).
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
| Material-slice canonicalization (RFC 8785-derived, append-only versioned) | Renovate-driven re-cert flood becomes a real complaint (≥ 5 attested recipes + weekly chart-version bumps). Until then, V1's "any edit triggers re-cert" is honest and cheap. |
| Multi-instance pointer (schema 2.0) with primary / supplementary / negative roles | Two contributors attest the same recipe from different clusters, OR a "this didn't work for me" negative attestation needs to coexist with a passing primary. |
| Signed layered predicate types (logs / redaction / augmentation) | Contributor asks to publish redacted logs, OR third party wants to add an independent re-run with its own signer. V1's manifest-pre-commit binding handles "publish logs later" already. |
| Re-cert age cutoffs (24mo hard, 23mo bot) | First bundle ages past 12 months. Document the policy in CONTRIBUTING; defer the bot. |
| Catalog-MAJOR re-cert trigger | Catalog SemVer contract filed (follow-on to [#660](https://github.com/NVIDIA/aicr/issues/660)). Trigger is dormant until the contract exists. |
| Advisory feed (`NVIDIA/aicr-advisories` OSV) | First post-merge incident requires revocation of an attestation. |
| Reusable workflow (`workflow_call`) | Multiple contributors ask for a turn-key path AND have hardware they can register against a public fork (corporate policy commonly disallows). Local `cosign attest` is documented in CONTRIBUTING. |
| Mirror bot + archive registry | First contributor's OCI registry goes dark on an accepted bundle. |
| Fingerprint per-signal provenance | First Tier-C-equivalent contribution gets pushback for "is this cluster real?" V1 records resolved `{value}` only; per-signal sources (`signals: [kubelet, dcgm, imds]`, `confidence: high`) are additive predicate fields. |

Each row's pull-trigger is the demand signal that should bring the
feature in; "Future direction" below sketches the shape each one
takes when its trigger fires.

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
  documented pull-trigger and a sketch in "Future direction." No
  surprise rewrites; expansion is additive.

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
- **Any recipe edit triggers re-cert.** Without the material-slice
  canonicalizer, a comment-only edit invalidates the existing
  bundle. Until the project has multiple attested recipes under
  active Renovate maintenance, this is a non-issue.
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

## Future direction

The features below are deferred from V1 by deliberate choice. Each
carries its pull-trigger, the shape it takes when implemented, and
compatibility notes. Don't pre-build — let demand decide which lands
first.

### Material-slice canonicalization (the highest-risk piece)

A canonicalizer that distinguishes *material* recipe edits (chart
version, criteria, constraints, manifest content, override values)
from *non-material* ones (comments, whitespace, displayName,
key-order). V1's `subject.digest` is "sort keys, strip comments,
sha256 the bytes" — any edit invalidates the bundle. A real
canonicalizer suppresses re-cert on cosmetic changes.

The shape: an RFC 8785-derived canonical form (with NFC string
normalization and type-preserving YAML load), a structured-diff
classifier that walks overlays → mixins → registry → component
values → manifest files transitively, and an append-only
`materialSliceVersion` integer in the predicate so old bundles
verify under their original algorithm even after bug fixes ship a
v2. The classifier has to model the full recipe-resolution surface,
including `componentRefs[].{type, source, version, valuesFile,
manifestFiles, overrides}` and the registry's `nodeScheduling`
paths.

**Pull-trigger:** Renovate-driven re-cert flood becomes a real
maintenance complaint (≥ 5 attested recipes under weekly bumps,
contributors asking why every Renovate PR invalidates their
attestation). **Compatibility:** the next predicate type is
`recipe-evidence/v2` since subject digest semantics change; verifiers
carry both parsers.

### Tier model and trust policy file

Tier A (first-party reusable workflow), Tier B (allowlisted
partner), Tier C (community workload OIDC), Tier D (rejected).
Identities live in a signed, freshness-bounded policy file at
`.sigstore/recipe-evidence-policy.yaml`. The verifier resolves the
cosign cert claims against the policy and labels the output.

V1 records the cosign cert claims faithfully but does not classify.
Maintainers eyeball the claims to tell first-party from community.
This works at low contribution volume; tier classification matters
when allowlists, partner relationships, or signer-identity-based
exit codes show up.

**Pull-trigger:** first partner relationship requests a non-community
trust label, OR community volume creates review fatigue tier
filtering would relieve. **Compatibility:** purely additive — the
verifier consumes a new flag (`--policy <path>`) and extends Markdown
output with a tier label.

### Multi-instance pointer schema (2.0)

V1's pointer carries `attestations: [...]` (a list of one). Schema 2.0
adds:

- A `role:` field per entry: `primary | supplementary | negative`
- A `staleAfter:` annotation for entries superseded by material change
- Pointer rotation (`<recipe>.archive.yaml`) for recipes with > 10
  attestations or 24-month history

This pays off when (a) a second contributor attests the same recipe
from a different cluster (multi-instance), (b) a contributor wants
to record "this didn't work for me on $configuration" without
auto-failing the gate (negative role), or (c) Renovate-driven
material change needs to mark old entries stale rather than delete
them. None of those apply at zero attested recipes.

**Pull-trigger:** any of (a)/(b)/(c). **Compatibility:** the V1
schema 1.0 pointer's `attestations[]` field stays; schema 2.0 adds
fields. V1 readers treat absent `role:` as `primary`. No structural
break.

### Signed layered predicate types

Three additional predicate types
(`recipe-evidence-logs/v1`, `recipe-evidence-redaction/v1`,
`recipe-evidence-augmentation/v1`) carrying a back-reference field
`evidenceFor: <summary-subject-digest>`. V1's logs bundle is
unsigned but bound by manifest pre-commit hashes; signed layers
enable:

- **Logs republished after redaction.** Redaction predicate documents
  scope (which fields scrubbed) and signer.
- **Third-party augmentation.** Independent re-run by a different
  signer, attached to the same OCI digest as the original summary.
- **Signed logs with own attestation event.** Logs predicate signs
  the logs bundle as a whole, in addition to the manifest pre-commit.

**Pull-trigger:** first contributor asks to publish redacted logs,
OR third party wants to add an independent re-run.
**Compatibility:** purely additive predicate types attached to the
same OCI digest as the V1 summary. V1 bundles never need rewriting.

### Re-cert automation

The full design defines four re-cert triggers: recipe content changed
(handled by the material-slice classifier), validator catalog MAJOR
bump, time decay (soft >12mo, hard >24mo with a scheduled bot opening
re-cert PRs at month 23), and critical advisory match against a
separate `NVIDIA/aicr-advisories` OSV-format repository.

V1 has no aged bundles, no catalog SemVer contract (a follow-on to
[#660](https://github.com/NVIDIA/aicr/issues/660)), and no merged
bundles to revoke. All four mechanisms are documented as policy in
CONTRIBUTING; none ship as automation.

**Pull-triggers, in likely order:** (1) catalog SemVer contract filed
+ #660 lands → catalog-MAJOR detection becomes meaningful; (2) first
bundle ages past 12 months → age-decay bot becomes useful; (3) first
post-merge incident requires revocation → OSV advisory feed gets
stood up. **Compatibility:** the verifier reserves `--check-advisories`
as a recognized but no-op flag in V1 so V2 integration is purely
additive.

### Reusable workflow and turnkey signing

A `.github/workflows/recipe-evidence-reusable.yaml` consumed via
`workflow_call` from contributor forks. The reusable workflow runs
`aicr validate --emit-evidence`, signs with cosign keyless, pushes
to OCI, and emits a pointer entry as a workflow output. The real
obstacle: most operators run GB200 / H100 hardware under corporate
GitHub Enterprise, where corporate policy commonly disallows
public-fork runner registration outright. Local `cosign attest`
against ambient OIDC remains the supported fallback.

V1 documents only the local-signing path. **Pull-trigger:** multiple
contributors ask for a turn-key path AND have hardware they can
register against a public fork. **Compatibility:** purely additive;
the verifier doesn't distinguish transport — the cert claims are what
matter.

### Mirror bot and archive registry

A post-merge bot (GitHub Action triggered on `push` to main) that
pulls each accepted bundle from the contributor's OCI registry and
re-pushes to `ghcr.io/nvidia/aicr-evidence-archive:<digest>`,
preserving content-addressed digests. The pointer carries an optional
`mirror:` field so verifiers can fall back to the mirror when the
contributor's registry becomes unavailable.

V1 stores the bundle in the contributor's OCI registry; long-term
durability is whatever their hosting provides plus the Rekor signing
record. **Pull-trigger:** first contributor's OCI registry goes dark
on an accepted bundle. **Compatibility:** additive — the verifier
reads `mirror:` if present and ignores it otherwise.

### Fingerprint per-signal provenance

V1's `fingerprint` records resolved values
(`accelerator: { value: h100 }`). The full design records, per
dimension, which collector signal contributed
(`accelerator: { value: h100, signals: [kubelet, dcgm, imds, dra],
confidence: high }`) plus a "what the fingerprint cannot prove"
disclosure surfaced in the verifier output. Multi-signal corroboration
makes forged-collector attacks harder to mount unnoticed.

**Pull-trigger:** first time a community contribution gets pushback
for "is this cluster real?" Until then, the resolved values plus
maintainer review are sufficient. **Compatibility:** the predicate's
`fingerprint.<dim>` field grows from `{value}` to
`{value, signals[], confidence}`; verifiers that parse the V1 shape
continue to read the resolved value from `value`.

## Adoption plan

1. **This ADR lands.** Sets policy, no code changes.
2. **PR-A: bundle format + pointer + `aicr validate --emit-evidence`.**
   Ships the `/v1` predicate; the OCI summary bundle layout (recipe +
   snapshot + BOM + CTRF + manifest); the optional logs bundle; the
   manifest pre-commit binding; the cosign keyless signing path; the
   pointer schema 1.0 (`docs/spec/recipe-evidence-pointer-v1.md` with
   JSON Schema); and `--emit-evidence` writing the pointer file
   alongside the bundle directories. Optional `--push <oci-registry>`
   handles the OCI upload, cosign attest, and pointer population in one
   command. Updates `pkg/bundler/attestation` with the new predicate
   type. Pulls the BOM from the existing #739 pipeline.
3. **PR-B: `aicr verify-evidence` CLI.** Single positional input
   (pointer / OCI / tarball / directory, auto-detected), twelve
   verification steps, three exit codes, Markdown + JSON output.
   Depends on PR-A (predicate parsing + pointer parsing).
4. **PR-C: CI gate workflow + PR template.** Required check on PRs
   touching `recipes/**`. Depends on PR-B.
5. **PR-D: `maintainers:` block schema + CI presence gate + backfill
   PR.** Independent of A/B/C; can land at any time.

PR-A is the foundation. PR-B depends on PR-A. PR-C depends on PR-B.
PR-D is fully independent and can land first if convenient.

When V1 ships and feedback lands, consult the deferred-features table
and "Future direction" above. **Each deferred feature has a documented
pull-trigger; let demand decide what V2 brings in.** Don't pre-build.
