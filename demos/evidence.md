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
OIDC if `ACTIONS_ID_TOKEN_REQUEST_URL` is set). The OCI digest is always
the canonical address, so the tag is just a human-readable label — tag
choice never affects verification. We omit the tag above, so aicr derives a
unique per-recipe one, `<recipe-slug>-<short-fingerprint>` (e.g.
`h100-eks-ubuntu-training-3f9a1c2b4d5e`). The fingerprint is the first 12 hex
of the bundle's manifest digest — deterministic, not random, so re-emitting
the same bundle yields the same tag while a different attestation yields a
different one. This keeps distinct attestations on distinct tags instead of
piling onto a shared one. Pass an explicit tag to override. The pointer file
(below) records both the tag and the digest. After it finishes:

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

### Alternative: split validate and publish across networks

Validation needs the cluster (often behind a corporate VPN); keyless signing
needs `fulcio.sigstore.dev` + `rekor.sigstore.dev`, which corporate networks
frequently block. Drop `--push` to emit an *unsigned* bundle on the VPN, then
sign + push + write the pointer from a host with Sigstore egress:

```shell
# On the VPN — validates and emits an unsigned bundle (no signing network).
aicr validate \
  --recipe recipe.yaml \
  --snapshot snapshot.yaml \
  --emit-attestation ./out

# Off the VPN (CI runner, jump box, hotspot) — sign, push, write pointer.
# Tag optional; aicr derives <recipe-slug>-<fingerprint> when omitted.
aicr evidence publish ./out --push ghcr.io/<owner>/aicr-evidence
```

The bundle is content-addressable and `evidence publish` signs the predicate
(with its emit-time `attestedAt`) verbatim from disk, so `./out` ends up
identical to the one-shot output above — including the signed
`attestation.intoto.jsonl` and a populated `pointer.yaml`.

## 2. Commit the pointer

A committed pointer lives at the per-source path
`recipes/evidence/<recipe>/<src>/<bundle-digest>.yaml`, where `<src>` is
the 32-hex slug derived from the signer's OIDC identity and the filename is the
bundle digest (`:` → `-`). The verifier only discovers pointers two levels deep
under a `<recipe>/<src>/` directory — a flat `recipes/evidence/<recipe>.yaml`
is rejected by the anti-squat gate. `aicr evidence publish` prints this exact
destination as `copyTo` in its output, so copy the pointer there verbatim:

```shell
# Path published by `aicr evidence publish` (its copyTo= log line), e.g.:
DEST=recipes/evidence/h100-eks-ubuntu-training/81724194d94a1e926f68c78ae51e8720/sha256-33d4cf36.yaml
mkdir -p "$(dirname "$DEST")"
cp ./out/pointer.yaml "$DEST"
git add "$DEST"
# Every commit from every contributor must use both -s (DCO sign-off) and
# -S (cryptographic signature); a branch ruleset rejects unsigned commits.
# See CONTRIBUTING.md.
git commit -s -S -m "evidence: attest h100-eks-ubuntu-training"
```

The `<src>` slug is `SourceSlug(issuer, identity)` — the first 32 hex of
`sha256(issuer + "\n" + identity)` for the signer that signed this bundle.
The example slug `81724194d94a1e926f68c78ae51e8720` is the hash of the
GitHub Actions issuer and identity shown in the pointer below; substitute
your own signer's values. The path-ownership gate recomputes this from the
signer fields **the pointer claims** and rejects any file committed under a
slug that does not match.

> **What the blocking gate does and does not check.** The *Evidence Pointer
> Contract* gate hashes the signer fields carried in the pointer and checks
> path ownership + the allowlist — it does **not** cryptographically verify
> that the bundle was actually signed by that identity (the OCI signature
> verification is a separate, currently warning-only check). So treat the
> pointer's signer as a *claimed* identity at gate time, not a cryptographically
> proven one. See [#1535](https://github.com/NVIDIA/aicr/issues/1535).

### Add the signer to the allowlist

The contract gate also rejects any committed pointer whose signer is not in
the allowlist (`signer ... is not in the allowlist`). Add a `community` (or
`partner`) entry for the signing identity to `recipes/evidence/allowlist.yaml`
in the same PR — first-party signers ingest directly and must not commit
per-run pointers. Community entries are keyed by the one-way `source` slug
(the same `<src>` value used in the pointer path above), never the cleartext
identity (the loader rejects an `identity:` field); an optional `label`
carries a non-PII display string:

```yaml
# recipes/evidence/allowlist.yaml
community:
  - issuer: https://token.actions.githubusercontent.com
    source: 81724194d94a1e926f68c78ae51e8720   # SourceSlug(issuer, identity)
    label: my-fork-validate-ci                  # optional, non-PII
```

`git log recipes/evidence/<recipe>/<src>/` is the audit trail of who signed
what, when. The pointer is small:

```yaml
schemaVersion: 1.0.0
recipe: h100-eks-ubuntu-training
attestations:
- bundle:
    oci: ghcr.io/<owner>/aicr-evidence:h100-eks-ubuntu-training-3f9a1c2b4d5e  # human-readable locator
    digest: sha256:f0c1...                                                    # canonical pin — verify uses this
    predicateType: https://aicr.run/recipe-evidence/v1
  signer:
    identity: https://github.com/<owner>/<repo>/.github/workflows/validate.yaml@refs/heads/main
    issuer: https://token.actions.githubusercontent.com
    rekorLogIndex: 91234567
  attestedAt: 2026-05-14T10:23:11Z
```

It's a locator, not a cache — every other field (fingerprint, phase counts,
BOM info) lives in the predicate inside the pulled artifact.

`bundle.oci` is the human-readable ref; `bundle.digest` is the
content-addressable pin. Verification (next section) pulls **by digest** —
`registry/repo@<bundle.digest>`, taking only the registry/repo from
`bundle.oci` — so the tag never affects what gets fetched, and the pointer
stays verifiable even if that tag is later moved to another artifact.
**Don't copy `bundle.oci` out of the pointer and feed it to `aicr evidence
verify`** — pass the pointer file itself (or a digest-pinned ref). As a raw
OCI argument a tag-only ref is refused because tags are registry-rewritable;
see §4 and `--allow-unpinned-tag`.

## 3. Verify from the pointer (maintainer path)

```shell
aicr evidence verify recipes/evidence/h100-eks-ubuntu-training/81724194d94a1e926f68c78ae51e8720/sha256-33d4cf36.yaml
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
aicr evidence verify recipes/evidence/<recipe>/<src>/<digest>.yaml --format json | jq '.exit'
```

Pin the expected signer when only one identity should be accepted:

```shell
aicr evidence verify recipes/evidence/h100-eks-ubuntu-training/81724194d94a1e926f68c78ae51e8720/sha256-33d4cf36.yaml \
  --expected-issuer https://token.actions.githubusercontent.com \
  --expected-identity-regexp '^https://github\.com/<owner>/.*$'
```

## 4. Verify directly from OCI

```shell
aicr evidence verify ghcr.io/<owner>/aicr-evidence@sha256:f0c1...
```

Same five checks, no repo checkout required. Useful for auditing a
contribution before merge. Note the `@sha256:...` digest form — take the
digest from the pointer's `bundle.digest` (or the push output), **not** the
`:tag` from `bundle.oci`. A tag-only ref is refused:

```text
OCI reference ghcr.io/<owner>/aicr-evidence:h100-eks-ubuntu-training-3f9a1c2b4d5e is
tag-only — refusing to pull an unpinned reference. Use a digest-bound
reference (registry/repo@sha256:<hex>), supply a pointer with bundle.digest
set, or pass --allow-unpinned-tag for one-off debugging.
```

In practice, prefer verifying from the pointer file (§3) — it carries the
digest, so you never handle the ref by hand.

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
# Pull the bundle locally and mutate a CTRF result.
# AICR pushes the contents of summary-bundle as the artifact root, so
# `-o summary-bundle` lands ctrf/, recipe.yaml, manifest.json … under it.
mkdir tmp && cd tmp
oras pull ghcr.io/<owner>/aicr-evidence@sha256:f0c1... -o summary-bundle
sed -i 's/"passed"/"failed"/' summary-bundle/ctrf/deployment.json

aicr evidence verify ./summary-bundle
# Expected: manifest-hash-check status = failed; exit 2.
# The CTRF file's sha256 no longer matches manifest.json, so the tamper is
# caught.
```

Note: `oras pull <subject>@sha256:…` fetches the bundle **subject only**, not
its Sigstore signature referrer — so a local-directory `verify` reports the
bundle as *unsigned* and the manifest-hash chain is checked as
**self-consistency** (it catches this tamper, but does not prove the signature
binding). To also exercise the signed chain, verify against the registry ref
(or a materialization path that fetches the signature referrer) so the
predicate is read from the verified signed payload. See §5 above and
ADR-007 §"Trust model".

## 7. PR-comment Markdown

```shell
aicr evidence verify recipes/evidence/h100-eks-ubuntu-training/81724194d94a1e926f68c78ae51e8720/sha256-33d4cf36.yaml \
  -o ./evidence-summary.md
```

Paste the rendered Markdown into the PR comment for maintainer review.

## 8. JSON output (CI path)

```shell
aicr evidence verify recipes/evidence/h100-eks-ubuntu-training/81724194d94a1e926f68c78ae51e8720/sha256-33d4cf36.yaml \
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
