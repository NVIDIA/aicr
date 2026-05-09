# Recipe Evidence Bundle v1

> **Status:** Stable. This is the V1 contract for recipe evidence bundles
> as specified by [ADR-007](../design/007-recipe-evidence.md) and
> implemented under [#754](https://github.com/NVIDIA/aicr/issues/754).
> The schema is a public stability boundary — additive changes only at
> the `v1` predicate type. Breaking changes ship as a `v2` predicate.

## Purpose

A recipe evidence bundle is a signed, content-addressed artifact that
ties a specific AICR recipe to a successful (or failed) `aicr validate`
run on real hardware. It exists so a maintainer can review and approve
a recipe contribution for hardware they cannot reach themselves.

## Scope of this document

- **Predicate type** `https://aicr.nvidia.com/recipe-evidence/v1` — the
  body of the in-toto Statement signed via cosign keyless OIDC.
- **Pointer schema 1.0** — the in-tree file at
  `recipes/evidence/<recipe>.yaml` that binds the repo to the OCI bundle
  by content hash.
- **Bundle layout** — the OCI artifact contents: recipe, snapshot, BOM,
  CTRF reports, manifest, attestation file.

## Bundle layout

A summary bundle is published as an OCI artifact:

```text
oci://<registry>/<owner>/aicr-evidence:<digest>
└── (OCI artifact whose layers contain:)
    ├── attestation.intoto.jsonl    # DSSE-wrapped, cosign keyless signed (present when --push)
    ├── statement.intoto.json       # unsigned in-toto Statement (always written)
    ├── recipe.yaml                 # post-resolution canonical YAML
    ├── snapshot.yaml               # cluster snapshot at validate-time
    ├── bom.cdx.json                # CycloneDX BOM
    ├── ctrf/
    │   ├── deployment.json
    │   ├── performance.json
    │   └── conformance.json
    └── manifest.json               # file inventory + per-file sha256
```

The unsigned `statement.intoto.json` lets a contributor sign the
bundle after the fact (with `cosign attest`, sigstore-go, or any
DSSE-aware signer) without re-running `aicr validate`. It carries
the same payload that gets DSSE-wrapped and signed when `--push` is
set. Neither `statement.intoto.json` nor `attestation.intoto.jsonl`
is enumerated in `manifest.json` — both derive from
`manifest.digest`, so the manifest binds the rest of the bundle and
the statement (signed or unsigned) binds the manifest.

When `--include-logs --push-logs` is set, an optional logs bundle is
published as a separate OCI artifact:

```text
oci://<registry>/<owner>/aicr-evidence-logs:<digest>
└── phases/
    ├── deployment/logs/*
    ├── performance/logs/*
    └── conformance/logs/*
```

The logs bundle is not signed as a whole. The summary's `manifest.json`
pre-commits per-file sha256 hashes for any log file that *would* be in
the logs bundle, so tampering is detectable file-by-file against the
manifest pre-commit without a separate signature event.

## Manifest schema

`manifest.json` is the integrity inventory for every file in the
bundle. The manifest's own sha256 is recorded in the predicate
(`manifest.digest`); since the predicate is signed, the manifest binds
the entire bundle's contents to the signature.

```json
{
  "schemaVersion": "1.0.0",
  "files": [
    {
      "path": "recipe.yaml",
      "size": 4321,
      "sha256": "sha256:abc...",
      "mediaType": "application/yaml"
    },
    {
      "path": "snapshot.yaml",
      "size": 87654,
      "sha256": "sha256:def...",
      "mediaType": "application/yaml"
    }
  ]
}
```

`files[].path` is bundle-relative, slash-separated. Implementations
must walk the bundle in a deterministic order (sorted by path) so
identical inputs produce identical manifest bytes.

## Predicate v1

Predicate type: `https://aicr.nvidia.com/recipe-evidence/v1`

```yaml
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
criteriaMatch:
  matched: true
  perDimension:
    service: { recipeRequires: eks, fingerprintProvides: eks, match: true }
    accelerator: { recipeRequires: h100, fingerprintProvides: h100, match: true }
    os: { recipeRequires: ubuntu, fingerprintProvides: ubuntu, match: true }
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

### Field reference

| Field | Type | Required | Notes |
|---|---|---|---|
| `schemaVersion` | string | yes | SemVer string. Always `"1.0.0"` for v1; bump goes to v2 predicate type. |
| `attestedAt` | string (RFC 3339) | yes | UTC timestamp of statement construction. |
| `aicrVersion` | string | yes | The `aicr` binary version that emitted the bundle. |
| `validatorCatalogVersion` | string | yes | Catalog version (per validator catalog SemVer). May be empty until the catalog SemVer contract lands (#660). |
| `validatorImages[]` | array | yes | List of `{image, digest}` for every validator container that ran. Order is sorted by `image`. |
| `fingerprint` | object | yes | Per-dimension cluster fingerprint. V1 records resolved values only (`{value: <v>}`); per-signal provenance (`signals[]`, `confidence`) is reserved for V2. |
| `criteriaMatch.matched` | bool | yes | Whether the recipe's `criteria` block is satisfied by the fingerprint. |
| `criteriaMatch.perDimension` | object | yes | Map of dimension → `{recipeRequires, fingerprintProvides, match}`. Empty map allowed when the recipe has no `criteria`. |
| `phases.<phase>` | object | yes when phase ran | `{passed, failed, skipped, ctrfDigest}`. `ctrfDigest` is the sha256 of the corresponding `ctrf/<phase>.json` file. |
| `bom.format` | string | yes | Always `"CycloneDX"` in v1. |
| `bom.version` | string | yes | CycloneDX spec version (e.g., `"1.5"`). |
| `bom.digest` | string | yes | sha256 of `bom.cdx.json` in the bundle. |
| `bom.imageCount` | int | yes | Count of `components[]` in the BOM. |
| `manifest.digest` | string | yes | sha256 of `manifest.json` in the bundle. **This is the field that binds every other file in the bundle to the signature.** |
| `manifest.fileCount` | int | yes | Count of entries in `manifest.json`'s `files[]` array. |

### Subject

The in-toto Statement's `subject[0]` carries the recipe identity:

```json
{
  "name": "recipe:<recipe-name>",
  "digest": { "sha256": "<recipe-canonical-digest>" }
}
```

The subject digest is `sha256(canonicalize(post-resolution recipe YAML))`
where canonicalize is:

1. Sort YAML map keys recursively.
2. Strip comments.
3. Normalize line endings to `\n`.
4. UTF-8 encode.

This is the V1 canonicalizer; it is intentionally simple. Any recipe
edit (including comment-only edits) invalidates the bundle and triggers
re-cert. A material-slice canonicalizer that suppresses re-cert on
non-material edits is reserved for V2.

## Pointer schema 1.0

The in-tree pointer file lives at `recipes/evidence/<recipe>.yaml` and
binds the repo to the OCI bundle by content hash:

```yaml
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
    logsBundle:
      oci: ghcr.io/<owner>/aicr-evidence-logs:<digest>
      digest: sha256:def456...
```

### Pointer field reference

| Field | Type | Required | Notes |
|---|---|---|---|
| `schemaVersion` | string | yes | Always `"1.0.0"` for the pointer schema. |
| `recipe` | string | yes | Recipe name; matches `recipe:<name>` subject suffix. |
| `attestations[]` | array | yes | List of attestations. **Always a list, length 1 in V1.** Schema 2.0 adds `role:` (primary / supplementary / negative) and supports multi-instance attestation. V1 readers treat absent `role:` as `primary`. |
| `attestations[].bundle.oci` | string | yes (after push) | OCI reference. Empty string when the bundle has been emitted locally but not yet pushed. |
| `attestations[].bundle.digest` | string | yes (after push) | sha256 of the OCI artifact's content. Empty string before push. |
| `attestations[].bundle.predicateType` | string | yes | Always `https://aicr.nvidia.com/recipe-evidence/v1` in V1. |
| `attestations[].signer.identity` | string | yes | OIDC cert subject (e.g., GitHub Actions workflow URI, contributor email). |
| `attestations[].signer.issuer` | string | yes | OIDC issuer URL. |
| `attestations[].signer.rekorLogIndex` | integer | yes | Rekor inclusion proof index. `0` is invalid; absence means no Rekor entry (e.g., signed offline). |
| `attestations[].attestedAt` | string (RFC 3339) | yes | Mirrors predicate's `attestedAt`. |
| `attestations[].fingerprint` | object | yes | Denormalized fingerprint for quick reading; the bundle's predicate is authoritative. |
| `attestations[].criteriaMatch.matched` | bool | yes | Quick yes/no for sidebar display in PR comments. |
| `attestations[].phaseSummary.<phase>` | object | yes when phase ran | `{passed, failed}`. Skipped/error counts are in the bundle's CTRF. |
| `attestations[].logsBundle` | object | no | Present when contributor pushed the optional logs bundle. |

### Pointer is a denormalized cache

The pointer is **bundle-derived**: `aicr validate --emit-attestation`
regenerates it from the OCI artifact (or the locally-emitted bundle
directory, before push). Mismatches between pointer and bundle are
**integrity-chain failures**, not clerical errors. The bundle is
authoritative; the pointer exists for fast reads, audit trail
(`git log` on the pointer), and CI gating.

## Verification model

A separate verifier (`aicr verify-evidence`, see #753) consumes the
bundle. The contract this spec establishes for that verifier:

1. The bundle's `attestation.intoto.jsonl` is a DSSE-wrapped in-toto
   Statement signed via cosign keyless.
2. The signed Statement carries `predicateType =
   https://aicr.nvidia.com/recipe-evidence/v1` and a body matching the
   schema above.
3. Every file referenced in `manifest.json` is present in the bundle
   and matches its recorded sha256.
4. `manifest.json`'s own sha256 matches `predicate.manifest.digest`.
5. The recipe in the bundle, when canonicalized, matches the
   Statement's `subject[0].digest`.

These are the cryptographic invariants. The verifier additionally
performs schema validation, fingerprint/criteria matching, and
inline-constraint replay; those are out of scope for this spec.

## Forward compatibility

Three V1 choices preserve future evolution at near-zero cost:

- **Predicate type is `recipe-evidence/v1`.** A breaking schema change
  goes to `recipe-evidence/v2`; verifiers carry both parsers.
- **`pointer.attestations` is a list from day one.** Multi-instance in
  schema 2.0 is additive: more entries plus a `role:` field defaulting
  to `primary`. No structural break for V1 pointers.
- **`fingerprint.<dim>` is an object.** V1 only populates `value`; V2
  may add `signals[]` and `confidence` fields. V1 readers that read
  `fingerprint.<dim>.value` continue to work.

## Stability guarantee

V1 fields listed above are stable. New optional fields may be added at
the V1 predicate type without bumping the schema version, provided
existing V1 verifiers ignore unknown fields gracefully (which is the
expected behavior). Removing or repurposing a V1 field is a breaking
change and must ship as `recipe-evidence/v2`.
