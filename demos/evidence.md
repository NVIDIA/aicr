# Recipe Evidence Demo

Recipe evidence is a signed, OCI-distributed bundle that proves a particular
recipe passed `aicr validate` against a specific cluster. Contributors emit
the bundle with `aicr validate --emit-attestation`; maintainers verify it
offline with `aicr evidence verify`. This is the trust handoff for recipes
on hardware AICR maintainers can't reach — see
[ADR-007](../docs/design/007-recipe-evidence.md) for the design.

This demo walks through the full producer-and-consumer loop:

1. Run `aicr validate --emit-attestation` to produce a bundle and pointer.
2. Push the bundle to an OCI registry (signs via cosign keyless OIDC).
3. Commit the pointer file to the repo.
4. Verify from the pointer (the maintainer's path).
5. Verify directly from the OCI artifact.
6. Verify locally without push (contributor self-debug, no signature).
7. Tamper a file and re-verify.

## Prerequisites

* `aicr` with `aicr evidence verify` available.
* A Kubernetes cluster to validate against.
* OCI registry write access for the producer. The registry must
  support storing image manifests AND referrers — either via the
  OCI 1.1 Referrers API (`/v2/<name>/referrers/<digest>`) or via
  fallback tag-schema referrers. ORAS handles either transparently.
  Known-good: GHCR, GitLab Container Registry, Harbor (≥ 2.8),
  AWS ECR, Google Artifact Registry, Azure Container Registry,
  JFrog Artifactory. Registries without referrer support cannot
  carry the Sigstore Bundle attached to the artifact; without that
  the verifier records signature-verify as "skipped (unsigned)"
  even though the bundle was signed at push-time.
* For signing: a working OIDC source. GitHub Actions OIDC is detected
  automatically; otherwise the CLI opens a browser for keyless signing.
* Bootstrap the Sigstore trusted root once on the verifier's machine:

  ```shell
  aicr trust update
  ```

## 1. Validate and emit evidence

```shell
aicr recipe \
  --service eks \
  --accelerator h100 \
  --os ubuntu \
  --intent training \
  --output recipe.yaml

aicr snapshot --output snapshot.yaml

aicr validate \
  --recipe recipe.yaml \
  --snapshot snapshot.yaml \
  --emit-attestation ./out \
  --push ghcr.io/<owner>/aicr-evidence
```

`--push` opens a browser for OIDC sign-in (or uses ambient GitHub Actions
OIDC if `ACTIONS_ID_TOKEN_REQUEST_URL` is set). The tag may be omitted as
shown — the emitter applies `:v1` as a placeholder since the OCI digest
is the canonical address; the pointer file (below) records both. After
it finishes:

```text
./out
├── pointer.yaml                 # copy this into the repo
└── summary-bundle/
    ├── recipe.yaml              # canonical post-resolution recipe
    ├── snapshot.yaml            # cluster snapshot at validate-time
    ├── bom.cdx.json             # CycloneDX SBOM
    ├── ctrf/                    # per-phase test results
    ├── manifest.json            # per-file sha256 inventory
    ├── statement.intoto.json    # unsigned in-toto Statement (recipe-subject)
    └── attestation.intoto.jsonl # SIGNED Sigstore Bundle (DSSE + Fulcio + Rekor)
```

## 2. Commit the pointer

```shell
mkdir -p recipes/evidence
cp ./out/pointer.yaml recipes/evidence/h100-eks-ubuntu-training.yaml
git add recipes/evidence/h100-eks-ubuntu-training.yaml
git commit -S -m "evidence: attest h100-eks-ubuntu-training"
```

`git log recipes/evidence/<recipe>.yaml` is the audit trail of who signed
what, when. The pointer is small:

```yaml
schemaVersion: 1.0.0
recipe: h100-eks-ubuntu-training
attestations:
- bundle:
    oci: ghcr.io/<owner>/aicr-evidence:v1
    digest: sha256:f0c1...
    predicateType: https://aicr.nvidia.com/recipe-evidence/v1
  signer:
    identity: https://github.com/<owner>/<repo>/.github/workflows/validate.yaml@refs/heads/main
    issuer: https://token.actions.githubusercontent.com
    rekorLogIndex: 91234567
  attestedAt: 2026-05-14T10:23:11Z
```

It's a locator, not a cache — every other field (fingerprint, phase counts,
BOM info) lives in the predicate inside the pulled artifact.

## 3. Verify from the pointer (maintainer path)

```shell
aicr evidence verify recipes/evidence/h100-eks-ubuntu-training.yaml
```

The verifier pulls the OCI artifact and runs five checks:

1. **Materialize** the bundle (OCI pull).
2. **Signature verify** — cosign keyless via sigstore-go; predicate extracted from the verified DSSE payload.
3. **Predicate parse** — uses the signature-anchored predicate.
4. **Manifest hash check** — every bundled file recomputed against `manifest.json`, which is bound to `predicate.Manifest.Digest` (now cryptographically anchored).
5. **Render** a Markdown summary with signer, fingerprint table, phase counts, and BOM info.

Exit codes:

| Surface | `0` | `2` |
|---|---|---|
| OS exit code | Bundle valid; every check passed. | Anything else — bundle invalid OR recorded validator results show failures. |

The structured output's `exit` field (`VerifyResult.Exit` in the
library, `.exit` in JSON output) carries a three-valued code so JSON
consumers can distinguish the two non-zero cases:

* `0` — bundle valid; every check passed.
* `1` — bundle valid; recorded validator results show failures
  (cryptographic integrity intact, informational).
* `2` — bundle invalid (signature, integrity, or predicate failure).

Today the CLI collapses `1` and `2` to OS exit `2` because
`pkg/errors/exitcode.go` maps both `ErrCodeConflict` and
`ErrCodeInvalidRequest` to the same OS code. Shell scripts that want
to branch on the informational case should consume `--format json`
and read `.exit` via `jq`:

```shell
aicr evidence verify recipes/evidence/<recipe>.yaml --format json | jq '.exit'
```

Pin the expected signer when only one identity should be accepted:

```shell
aicr evidence verify recipes/evidence/h100-eks-ubuntu-training.yaml \
  --expected-issuer https://token.actions.githubusercontent.com \
  --expected-identity-regexp '^https://github\.com/<owner>/.*$'
```

## 4. Verify directly from OCI

```shell
aicr evidence verify ghcr.io/<owner>/aicr-evidence@sha256:f0c1...
```

Same five checks, no repo checkout required. Useful for auditing a
contribution before merge.

## 5. Verify locally without push (contributor self-debug, no signature)

Skip `--push` and the verifier runs every check except signature:

```shell
aicr validate --recipe recipe.yaml --snapshot snapshot.yaml \
  --emit-attestation ./out

aicr evidence verify ./out/summary-bundle
```

The Signer line in the rendered report reads `_unsigned bundle_`. The
predicate comes from `statement.intoto.json` rather than the verified
signed payload, so the manifest-hash chain becomes self-consistency
only — useful for catching accidental corruption, not deliberate
tampering. See ADR-007 §"Trust model" for details.

## 6. Tamper demo

The signed manifest hash pins every file. One example:

```shell
# Pull a signed bundle locally and mutate a CTRF result.
mkdir tmp && cd tmp
oras pull ghcr.io/<owner>/aicr-evidence@sha256:f0c1...
sed -i 's/"passed"/"failed"/' summary-bundle/ctrf/deployment.json

aicr evidence verify ./summary-bundle
# Expected: manifest-hash-check status = failed; exit 2.
# The CTRF file's sha256 no longer matches manifest.json, and
# manifest.json's digest is anchored to the verified predicate.
```

## 7. PR-comment Markdown

```shell
aicr evidence verify recipes/evidence/h100-eks-ubuntu-training.yaml \
  -o ./evidence-summary.md
```

Paste the rendered Markdown into the PR comment for maintainer review.

## 8. JSON output (CI path)

```shell
aicr evidence verify recipes/evidence/h100-eks-ubuntu-training.yaml \
  -o evidence-result.json -t json

jq '.exit' evidence-result.json          # 0 / 1 / 2 (library code)
jq '.signer' evidence-result.json        # signer claims
jq '.predicate.phases' evidence-result.json
```

## Troubleshooting

**"sigstore verification failed — trusted root may be stale"** — Sigstore
rotates its TUF roots periodically. Run `aicr trust update`.

**"pointer digest does not match pulled digest"** — the OCI artifact at the
pointer's reference is not the one the pointer was attested against. Either
the registry rewrote the tag (use a digest-bound reference) or the bundle
was re-pushed. Re-emit and re-commit the pointer.

**"signed subject digest does not match pulled artifact digest"** — the
Sigstore signature was made for a different OCI artifact than the one we
pulled. Someone substituted the bundle and re-pointed at a stale signature.

**"OCI pull failed"** — registry auth. The verifier uses ambient Docker
credentials (`docker login` / `DOCKER_CONFIG`); confirm `docker pull
<oci-ref>` works from the same shell.
