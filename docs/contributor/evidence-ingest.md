# Evidence Ingest (GP2)

The evidence-ingest pipeline turns a **published, signed** recipe-evidence
bundle into the **source-keyed tree** that the corroboration dashboard
generator consumes. It is the bridge between the per-run attestation
bundles produced by `aicr validate --emit-attestation --push` (see
[ADR-007](../design/007-recipe-evidence.md)) and the aggregated,
consensus view.

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
   taken and classification matches the GP4 consumer exactly.
3. Otherwise `community`, `allowlisted=false` — the fail-closed default.
   Reported, but never counted toward consensus.

The allowlist schema is **shared with the GP4 consumer** (`pkg/corroborate`)
so both read the identical `recipes/evidence/allowlist.yaml` and classify
a signer identically:

```yaml
schemaVersion: "1.0.0"
firstParty:
  - issuer: https://token.actions.githubusercontent.com
    identity: '^https://github\.com/NVIDIA/aicr/\.github/workflows/uat-(aws|gcp)\.yaml@refs/heads/main$'
community:
  - issuer: https://token.actions.githubusercontent.com
    identity: https://github.com/acme-gpu/aicr-attest/.github/workflows/attest.yaml@refs/heads/main
partner:
  - issuer: https://oidc.coreweave-lab.example
    identity: https://oidc.coreweave-lab.example/attest
```

`identity` is an exact string, or a `^…$`-anchored regex (full-string
match). The loader rejects an over-broad regex (unbounded `*`/`+`/`{n,}`
that could span an org/repo segment) and overlapping entries, so the
allowlist cannot itself be used to manufacture consensus.

## Forward limitations

- Today's `pkg/evidence/verifier` populates a verified signer only from
  an in-artifact `attestation.intoto.jsonl` (DSSE + Fulcio cert). The
  "statement-only bundle whose signer is carried in the pointer and
  verified against Rekor by digest" shape is not yet implemented, so such
  community bundles cannot be ingested — the producer fails closed rather
  than record an unverified signer.
- Discovery currently walks the flat `recipes/evidence/<recipe>.yaml`
  pointers. The per-source nested layout is forward-compatible.
- The publish job writes to `gs://aicr-testgrid-staging/results` using the
  shared eidosx WIF service account from `uat-gcp.yaml`. GP3 will replace
  it with a dedicated `objectCreator`-only identity scoped to that prefix.
