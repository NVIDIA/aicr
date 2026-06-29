# Evidence Ingest (GP2)

The evidence-ingest pipeline turns a **published, signed** recipe-evidence
bundle into the **source-keyed tree** that the corroboration dashboard
generator consumes. It is the bridge between the per-run attestation
bundles produced by `aicr validate --emit-attestation --push` (see
[ADR-007](../design/007-recipe-evidence.md)) and the aggregated,
consensus view. The tree it writes is rendered and served by
[dashboard-publish (GP5)](evidence-dashboard-publish.md).

The `GPn` labels name stages of the evidence-corroboration pipeline (epic #1400):
**GP1** signer-allowlist management (`recipes/evidence/allowlist.yaml`),
**GP2** ingest/verify (this doc), **GP3** dashboard infrastructure
(`infra/evidence-dashboard`), **GP4** consensus/corroboration
(`pkg/corroborate`), and **GP5** dashboard publish.

Its defining property is **verify-before-count**: a bundle's signature,
issuer, identity, and source registry are all checked in a step that
holds no bucket-write credentials, *before* any of its results are
recorded. Unverified evidence never reaches the tree, and contributors
never hold credentials to the publish bucket.

## Pipeline

```
pointer / bundle ref
        │
        ▼
  materialize (ORAS pull, digest-pinned)
        │
        ▼
  verify  ── issuer + identity pins, signature, registry allowlist
        │        (credential-free; fails closed)
        ▼
  classify (allowlist → class + allowlisted)
        │
        ▼
  synthesize  results/<group>/<dashboard>/<tab>/<idHash>/<runId>/
        │         meta.json + ctrf/<phase>.json
        ▼
  publish (separate, credentialed step) → GCS
```

The Go pieces:

- `pkg/evidence/project` — the synthesis library. Pure and offline: given
  an already-verified bundle plus its verified signer claims, it derives
  the coordinate and signer id-hash and writes the run directory. It
  performs no verification and no network I/O of its own.
- `tools/evidence-project` — the CLI that wires it together: resolve the
  OCI ref, enforce the trusted-registry allowlist, materialize once,
  verify on the unpacked directory with non-empty pins
  (`pkg/evidence/verifier`), classify, then synthesize.
- `.github/workflows/evidence-ingest.yaml` + `.github/scripts/evidence-ingest.sh`
  — the CI driver with two triggers (below) and a fork-safe split between
  the credential-free verify job and the credentialed publish job.

## Triggers

1. **Push to `recipes/evidence/**`** — community and partner pointers
   added or refreshed in-tree. Each changed pointer is verified with the
   signature pinned to the signer the pointer *claims*; the verifier also
   cross-checks the certificate against that claim, so a pointer cannot
   lie about who signed.
2. **`workflow_call` / `workflow_dispatch` with `bundle_ref`** — a
   first-party UAT run ingests its bundle directly, by ref, with no repo
   commit. The signature is pinned to the NVIDIA/aicr Actions identity.
   `uat-aws.yaml` and `uat-gcp.yaml` call this workflow as a dependent
   `ingest-evidence` job on a successful run, passing the digest-pinned
   ref read from the conformance step's `evidence/pointer.yaml`.

## meta.json

Each run directory carries one `meta.json` (schema
`aicr-corroboration-meta/v1`). It is the **authoritative** coordinate
source — the directory layout is organizational only. Every field traces
to the verified predicate or certificate; there is no clock and no
randomness, so output is byte-identical across runs from identical input.

| Field | Source |
|---|---|
| `schemaVersion` | constant `aicr-corroboration-meta/v1` |
| `coordinate.{group,dashboard,tab}` | `recipe.CoordinateFor` over the bundle recipe's criteria |
| `recipe` | predicate `recipe.name` |
| `signer.idHash` | `SignerIDHash(issuer, identity)` — the source dedup key |
| `signer.identity` / `signer.issuer` | the **verified** cert SAN + OIDC issuer |
| `signer.class` / `signer.allowlisted` | classification (below) |
| `runId` | `--run-id`, else `run-<attestedAt:YYYYMMDDThhmm>` |
| `aicrVersion` | predicate `aicrVersion` |
| `k8sVersion` | predicate `fingerprint.k8sVersion.value` |
| `k8sConstraint` | the recipe's `K8s.server.version` constraint |
| `bundleDigest` | predicate `manifest.digest` |
| `evidenceRef` | OCI ref of the signed bundle |
| `rekorLogIndex` | verified Rekor index (omitted when absent) |
| `attestedAt` | predicate `attestedAt` (the only timestamp) |

`ctrf/<phase>.json` reports are copied for each phase the run produced
(`deployment`, `performance`, `conformance`); a phase the run did not
produce is simply absent — never stubbed.

## idHash (producer ↔ consumer contract)

`idHash` is the stable per-signer dedup key the consensus model counts
distinct values of. It is

```
idHash = first 32 hex chars (128 bits) of sha256(issuer + "\n" + identity)
```

defined once in `project.SignerIDHash`. The same verified signer hashes
to the same value across every recipe and run; two different signers do
not collide. This is the GP2-producer/GP4-consumer contract — do not
change the algorithm without migrating both the bucket tree and the
consumer.

## Classification

`project.Allowlist.Classify` derives a signer's trust tier from the
**verified** `(issuer, identity)`, never a raw pointer string:

1. The first matching `--allowlist` entry wins, `allowlisted=true`.
2. When no allowlist file is loaded — the interim state before GP1 ships
   `recipes/evidence/allowlist.yaml` — a built-in heuristic admits AICR's
   own UAT identity (GitHub Actions OIDC + `NVIDIA/aicr`) as `first-party`
   so it is not mislabeled. Once the file exists this branch is never
   taken. (Note: GP2 and GP4 do not yet parse the canonical
   `identityPattern`/`source` schema identically — see the reconciliation note
   below — so this is the intended end-state, not current full parity.)
3. Otherwise `community`, `allowlisted=false` — the fail-closed default.
   Reported, but never counted toward consensus.

The canonical `recipes/evidence/allowlist.yaml` keys first-party CI signers
by an anchored `identityPattern` and external contributors/partners by a
one-way `source` slug — it deliberately does **not** store a cleartext
`identity` field (the loader rejects one, to keep personal emails out of the
repo):

```yaml
schemaVersion: 1.0.0
firstParty:
  - label: aicr-uat-aws
    issuer: https://token.actions.githubusercontent.com
    identityPattern: '^https://github\.com/NVIDIA/aicr/\.github/workflows/uat-aws\.yaml@refs/heads/.+$'
community:
  # Keyed by source slug only — first 32 hex of sha256(issuer + "\n" + identity).
  - issuer: https://github.com/login/oauth
    source: 7c4c0edc8c765a95a0f3afdb3bbb8e91
partner: []
```

`identityPattern` is a `^…$`-anchored regex (full-string match); the loader
rejects an over-broad regex (unbounded `*`/`+`/`{n,}` that could span an
org/repo segment) and overlapping entries, so the allowlist cannot itself be
used to manufacture consensus.

> **Loaders not yet reconciled with the canonical schema.** Neither
> loader parses the `identityPattern`/`source` allowlist above yet. Both
> the GP2 ingest loader (`pkg/evidence/project`) and the GP4 consumer
> (`pkg/corroborate`) still carry a single `Identity` field (and the GP1
> canonical loader deliberately rejects a cleartext `identity`), so neither
> can parse the canonical `recipes/evidence/allowlist.yaml`. Reconciling
> both loaders with the GP1 allowlist schema is still outstanding.

## Forward limitations

- Today's `pkg/evidence/verifier` populates a verified signer only from
  an in-artifact `attestation.intoto.jsonl` (DSSE + Fulcio cert). The
  "statement-only bundle whose signer is carried in the pointer and
  verified against Rekor by digest" shape is not yet implemented, so such
  community bundles cannot be ingested — the producer fails closed rather
  than record an unverified signer.
- Discovery walks the per-source nested layout
  `recipes/evidence/<recipe>/<src>/<digest>.yaml` (the
  `<recipe>/<src>/*.yaml` glob in `pkg/evidence/verifier/discover.go`); a
  flat `recipes/evidence/<recipe>.yaml` is rejected as an unexpected root file.
- The publish job writes to `gs://aicr-testgrid-staging/results` using the
  shared eidosx WIF service account from `uat-gcp.yaml`. GP3 will replace
  it with a dedicated `objectCreator`-only identity scoped to that prefix.
