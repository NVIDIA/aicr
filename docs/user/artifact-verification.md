# Artifact Verification

A consumer-facing guide to verifying the artifacts `aicr` produces (deployment
bundles and recipe-evidence bundles) across public-trust, KMS-key, and
offline deployment shapes. For the exhaustive per-flag reference, see the
[CLI reference](cli-reference.md): [`aicr verify`](cli-reference.md#aicr-verify),
[`aicr evidence verify`](cli-reference.md#aicr-evidence-verify), and
[`aicr trust update`](cli-reference.md#aicr-trust-update).

## What Can Be Verified

| Artifact | Produced by | Verified by |
|----------|-------------|-------------|
| Deployment bundle | `aicr bundle --attest` | [`aicr verify`](cli-reference.md#aicr-verify) |
| Recipe-evidence bundle | `aicr validate --emit-attestation` | [`aicr evidence verify`](cli-reference.md#aicr-evidence-verify) |
| Embedded recipe catalog | shipped in the `aicr` binary | [`aicr recipe verify-catalog`](cli-reference.md#aicr-recipe-verify-catalog) |

This guide focuses on the first two. Catalog verification is a single
self-contained command; see its [CLI reference entry](cli-reference.md#aicr-recipe-verify-catalog).

Bundles are unsigned by default; attestation is opt-in via `aicr bundle
--attest`. An unsigned bundle can still be checksum-verified, but it cannot
reach the higher trust levels described below.

## Trust Levels

`aicr verify` computes a trust level for a bundle. The levels are ordered;
each one subsumes the guarantees of the levels beneath it.

| Level | Name | What it guarantees |
|-------|------|--------------------|
| 4 | `verified` | Checksums valid, bundle attestation verified, binary attestation verified with identity pinned to NVIDIA CI, and no external data |
| 3 | `attested` | Bundle attestation cryptographically verified, but the chain is incomplete — the binary attestation is missing or external `--data` was used |
| 2 | `unverified` | Checksums valid, but no attestation files exist (the bundle was created without `--attest`) |
| 1 | `unknown` | Checksums are missing/invalid, or an attestation (bundle or binary) is present but *fails* verification — a present binary attestation that does not verify is a hard failure, not a degraded `attested` |

The ordering matters for enforcement: `verified` > `attested` > `unverified` >
`unknown`. A bundle that uses external data can never exceed `attested`, and a
bundle created without `--attest` can never exceed `unverified`, regardless of
anything else. This is why `--min-trust-level max` (the default) auto-detects
the *highest level a given bundle could achieve* and verifies against that,
rather than failing a deliberately unsigned bundle.

## Public-Trust Bundle Verification

The default path. Verification is offline and makes no network calls (the one
exception, a KMS URI passed to `--key`, is covered below).

```shell
# Verify a bundle, auto-detecting and enforcing its maximum achievable trust level.
aicr verify ./my-bundle
```

Under the hood this runs three checks:

1. **Checksums**: every generated payload file listed in `checksums.txt`, including `recipe.yaml` in Helm bundles, is hashed and matched.
2. **Bundle attestation**: the bundle's signature is verified against the Sigstore trusted root.
3. **Binary attestation**: the provenance chain is verified with identity pinned to NVIDIA CI.

You can also pin the bundle's creator identity or the CLI version that produced it:

```shell
# Require that a specific identity created the bundle.
aicr verify ./my-bundle --require-creator jdoe@company.com

# Require a minimum aicr CLI version (recorded in the attestation predicate).
aicr verify ./my-bundle --cli-version-constraint ">= 0.8.0"
```

## Minimum Trust Level Enforcement

By default `aicr verify` uses `--min-trust-level max`, which resolves to the
highest level the bundle could achieve and fails if verification falls short.
To require an explicit floor regardless of the bundle's contents, name a level:

```shell
# Fail unless the bundle reaches full `verified` trust.
aicr verify ./my-bundle --min-trust-level verified

# Accept `attested` or higher (e.g. when external --data is expected).
aicr verify ./my-bundle --min-trust-level attested
```

Valid values are `verified`, `attested`, `unverified`, `unknown`, and `max`.
A bundle whose computed level is below the requested floor exits non-zero.

## KMS-Key Verification

Some environments cannot use keyless OIDC signing, so bundles are signed with a
cloud-KMS key via `aicr bundle --attest --signing-key <kms-uri>`. Verify those
bundles with `--key`, supplying the same KMS URI used to sign. Supported
schemes are `awskms://`, `gcpkms://`, and `azurekms://`.

```shell
# Sign with a KMS key, then verify it with the same key.
aicr bundle -r recipe.yaml --attest \
  --signing-key gcpkms://projects/p/locations/l/keyRings/r/cryptoKeys/k \
  -o ./bundles
aicr verify ./bundles/<bundle-dir> \
  --key gcpkms://projects/p/locations/l/keyRings/r/cryptoKeys/k
```

Resolving a KMS URI makes network calls to the KMS provider to fetch the public
key, so credentials for that provider must be available in the environment.

## Local PEM Key Verification

To verify without granting KMS access, export the public key once and verify
against the local PEM file; this part makes no provider calls:

```shell
# Export the public key once (requires KMS access at export time).
cosign public-key --key gcpkms://projects/p/locations/l/keyRings/r/cryptoKeys/k > bundle-signer.pub

# Verify anywhere afterward, with no KMS access needed.
aicr verify ./bundles/<bundle-dir> --key ./bundle-signer.pub
```

A local PEM key is read from disk only. The bundle's Rekor transparency-log
entry is also checked by default, but its inclusion proof is embedded in the
bundle and the trusted root falls back to the embedded copy on a cache miss —
so verification runs fully offline without `aicr trust update`.

## Privately-Signed Bundle Verification

Air-gapped and private-deployment sites often run their own Sigstore stack (a
self-hosted Fulcio CA and Rekor log, for example from sigstore/scaffolding)
rather than the public-good infrastructure. `aicr bundle --attest` signs
against that stack with `--fulcio-url` / `--rekor-url`; `aicr verify
--trust-root` is the verify counterpart. Point it at the `trusted_root.json`
your private Sigstore stack emits:

```shell
# Verify a bundle signed against a private Fulcio/Rekor.
aicr verify ./my-bundle --trust-root ./trusted_root.json
```

`--trust-root` is **additive**: the supplied root is unioned with AICR's
built-in public-good root, not substituted for it. So a single command verifies
both org-signed bundles (against the private root) and NVIDIA-signed bundles
(against the public-good root); you do not need to switch flags per artifact.

The flag supplies trust anchors only; it does not by itself require a bundle to
be privately signed. To pin a specific signer, keep using `--require-creator`
(and, for the binary attestation's identity, `--certificate-identity-regexp`):

```shell
# Verify against the org root and require a specific signer identity.
aicr verify ./my-bundle \
  --trust-root ./trusted_root.json \
  --require-creator ci@myorg.example.com
```

`--trust-root` composes with `--key`, so a KMS- or PEM-key-signed bundle whose
signature was logged to a private Rekor verifies with both flags together:

```shell
aicr verify ./my-bundle \
  --trust-root ./trusted_root.json \
  --key ./bundle-signer.pub
```

Only the **bundle attestation** consults the private root. The **binary
attestation** is always produced by NVIDIA's public CI and continues to verify
against the public-good root, regardless of `--trust-root`. Errors with the
supplied `trusted_root.json` (missing, unreadable, oversized, or malformed) are
reported as invalid-request failures.

## Offline and Air-Gapped Considerations

`aicr verify` is offline by default and does not call out to Sigstore at verify
time; the Rekor inclusion proof is embedded in the bundle, so no live Rekor
call is made. The check needs the Sigstore *trusted root*, but verification forces the local
cache (`WithForceCache`) and falls back to the **embedded** trusted root on a
cache miss — it does not fetch on the verify path, so offline verification works
out of the box. `aicr trust update` refreshes the cached trust material (it
contacts the TUF CDN once) but is **not** required for offline verification.

Two scope limits to be aware of, both reflected in the current CLI reference:

- **Fully transparency-log-free verification**: dropping the Rekor
  transparency-log check entirely, for true air-gapped use, is **not yet
  supported**. It is tracked in
  [#1154](https://github.com/NVIDIA/aicr/issues/1154).
- **Private Sigstore verification**: `aicr bundle --attest` can redirect
  *signing* to a private Fulcio/Rekor with `--fulcio-url` / `--rekor-url`, and
  `aicr verify --trust-root` verifies the resulting bundles against that
  infrastructure's `trusted_root.json`. See
  [Privately-Signed Bundle Verification](#privately-signed-bundle-verification).

## Recipe Evidence Verification

Recipe-evidence bundles, produced by `aicr validate --emit-attestation`, are
verified with `aicr evidence verify`. When the bundle carries a signature, the
command verifies it against the Sigstore trusted root, recomputes every
manifest-listed payload file's sha256 against `manifest.json`, and surfaces the predicate's fingerprint, phase
counts, and BOM info.

Bundles are **minimized by default**: the published `snapshot.yaml` keeps only
an allowlisted set of fields and the CTRF reports omit per-test stdout/message,
keeping sensitive operational detail (node names, provider instance IDs, the
node label/taint set, OS tuning, raw container logs) out of the published
artifact. The predicate records the applied policy in a `redaction` block, which
`aicr evidence verify` surfaces. Minimal bundles self-verify exactly like full
ones — the digests cover whatever bytes shipped. Pass `--full` to
`aicr validate --emit-attestation` to publish the raw payloads instead.

The positional argument is auto-detected. Prefer the committed **pointer file**,
which pins the bundle by digest:

```shell
# Verify a pointer a contributor committed alongside a recipe change (preferred).
aicr evidence verify recipes/evidence/<recipe>/<src>/<digest>.yaml

# Verify a pushed OCI bundle directly, pinned by digest.
aicr evidence verify ghcr.io/myorg/aicr-evidence@sha256:abc...

# Verify a local bundle directory (self-debug before push).
aicr evidence verify ./out/summary-bundle
```

To require a specific signer, pin the OIDC issuer and identity:

```shell
aicr evidence verify recipes/evidence/<recipe>/<src>/<digest>.yaml \
  --expected-issuer https://token.actions.githubusercontent.com \
  --expected-identity-regexp '^https://github\.com/myorg/myrepo/\.github/workflows/release\.yaml@refs/tags/.+$'
```

## Per-Source Pointer Layout and the Signer Allowlist

Committed evidence pointers are **per-source** so two parties can attest to the
same recipe without overwriting each other. The on-disk layout is add-only and
immutable (ADR-007; issue [#1347](https://github.com/NVIDIA/aicr/issues/1347)):

```
recipes/evidence/<recipe>/<src>/<bundle-digest>.yaml   # one immutable pointer per run
recipes/evidence/allowlist.yaml                            # maintained signer allowlist
```

Each file is a single-attestation V1 pointer — the same schema that
`aicr evidence verify` already consumes. The `<src>` segment is a stable slug
derived from the signer OIDC identity recorded in the pointer — the first 32 hex
characters (128 bits) of `sha256(issuer + "\n" + identity)`. Because the slug is
computed from the signer fields rather than chosen freely, the
`Evidence Pointer Contract` CI job recomputes the slug from each pointer's own
signer block and rejects any file that does not live under the directory its
signer hashes to.

> **Where cryptographic trust enters (#1535).** The on-disk
> pointer-contract merge gate is *structural*: it derives the slug from the
> signer fields the pointer declares and checks path ownership + allowlist
> membership, but does not itself verify a signature. Cryptographic trust is
> enforced **after merge, at ingest** (`evidence-ingest.yaml`), which verifies
> the signature pinned to the claimed signer and cross-checks the certificate
> before any result is counted — so a pointer that lied about its signer passes
> the gate but fails ingest. See
> [#1535](https://github.com/NVIDIA/aicr/issues/1535) and ADR-007. (This ingest verification is implemented but **currently fails closed** — the GP2 loader cannot yet parse the canonical `identityPattern`/`source` allowlist; tracked in [#1505](https://github.com/NVIDIA/aicr/issues/1505).)

Consumers discover a recipe's evidence by glob —
`recipes/evidence/<recipe>/*/*.yaml` — and aggregate across sources; nothing is
ever modified in place.

The **allowlist** (`recipes/evidence/allowlist.yaml`) is the trust root. It
pins, per class — `first-party`, `community`, `partner` — the signers that may
contribute corroborating evidence. Community and partner entries are keyed by
the one-way `source` slug only — the cleartext identity (e.g. a personal
email) is **never** stored in the *allowlist*; an optional non-PII `label`
(e.g. a GitHub handle) is for display and may be omitted. (The committed
pointer and Rekor still carry `signer.identity` — see the note below — so a
contributor is not anonymous.)
First-party entries pin a tightly-bounded anchored `identityPattern` whose
issuer/org/repo/workflow segment is literal (a wildcard is permitted only in
the ref) — these are CI workflow URLs, not personal identities. The classes
are disjoint and no two entries overlap, so a verified signer classifies
exactly one way. A verified signer that is **not** listed is admitted as
*reported* only — it never counts toward corroboration.

(Only the **allowlist** is slug-only — its loader rejects a stray `identity:`
field. Committed **pointers** (`recipes/evidence/<recipe>/<src>/*.yaml`) do
carry `signer.identity`, and keyless Sigstore also records it in the public
Rekor log, so the signer identity is public regardless. Sign with a
non-personal identity — a GitHub `users.noreply.github.com` address or CI's
ambient OIDC — not a personal email.)

First-party AICR CI (the UAT workflows) signs with the GitHub Actions OIDC
issuer and **ingests evidence directly** — it does not commit a per-run pointer,
which would churn `main` nightly. Committed per-source pointers are therefore
the community/partner channel.

### Contributing evidence (community / partner)

1. Run `aicr validate --emit-attestation` and push the signed bundle to a
   registry you control. The command prints a `copyTo` hint with the exact
   per-source destination path.
2. Open a PR that adds **your own** pointer file under
   `recipes/evidence/<recipe>/<src>/` — alongside, never replacing, any
   other party's — plus a slug entry (`source:` + `issuer:`, optional
   `label:`) for your signer in the correct class of
   `recipes/evidence/allowlist.yaml`. The `<src>` slug is printed in the
   `copyTo` hint; use that same value as `source:`.
3. Maintainer review (enforced by `CODEOWNERS`) is the trust gate; the
   `Evidence Pointer Contract` job verifies the path-ownership and allowlist
   invariants before merge.

A tag-only OCI reference is refused by default because tags are
registry-rewritable; pass the pointer file (which carries the digest) or a
digest-pinned ref instead. See the
[`aicr evidence verify`](cli-reference.md#aicr-evidence-verify) reference for the
full input-detection rules and the `--allow-unpinned-tag` escape hatch.

## JSON Output for CI

Both verifiers emit machine-readable JSON for pipeline gating.

```shell
# Bundle verification as JSON.
aicr verify ./my-bundle --format json

# Evidence verification as JSON to a file.
aicr evidence verify recipes/evidence/<recipe>/<src>/<digest>.yaml --format json -o result.json
```

`aicr evidence verify` exits `0` when every check passes and `2` when the
bundle is invalid or recorded validator results show failures. The JSON
output's `exit` field further distinguishes recorded phase failures (`1`) from
an invalid bundle (`2`), so a shell consumer can branch on it. Write the JSON to
a file and read the `exit` field from it rather than piping `aicr evidence
verify` straight into `jq`: under `set -o pipefail` (common in CI) the verifier's
non-zero exit would otherwise propagate through the pipeline and abort the script
before the `case` runs. The `|| true` keeps that exit from tripping `set -e`:

```shell
aicr evidence verify recipes/evidence/<recipe>/<src>/<digest>.yaml --format json -o result.json || true
case "$(jq '.exit' result.json)" in
  0) echo "evidence valid" ;;
  1) echo "validator phases failed" ;;
  2) echo "bundle invalid" ;;
esac
```

## Troubleshooting Common Failures

**Certificate chain errors / stale trusted root.** Sigstore rotates signing
keys a few times per year. If verification fails with certificate-chain errors,
refresh the local trusted root:

```shell
aicr trust update
```

**`--key` cannot reach the KMS provider.** A KMS URI requires provider
credentials in the environment. If you only need to verify (not sign), export
the public key once with `cosign public-key --key <kms-uri>` and pass the local
PEM file to `--key` instead; see [Local PEM Key Verification](#local-pem-key-verification).

**Tag-only OCI reference refused.** `aicr evidence verify` rejects tag-only
references because tags are registry-rewritable. Pass the committed pointer file
(which carries the `sha256:` digest) or a digest-pinned ref (`...@sha256:<hex>`).

**Trust level lower than expected.** A bundle created without `--attest` caps at
`unverified`; a bundle built with external `--data` caps at `attested`. If you
require a stricter level, re-create the bundle with attestation and without
external data, then verify with `--min-trust-level verified`.

**Private-Sigstore-signed bundle won't verify.** A bundle signed against a
private Fulcio/Rekor needs that infrastructure's trusted root. Pass it with
`aicr verify --trust-root ./trusted_root.json`. See
[Privately-Signed Bundle Verification](#privately-signed-bundle-verification).
