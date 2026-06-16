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
| 3 | `attested` | Full chain cryptographically verified, but binary attestation is missing or external `--data` was used, which caps trust because that data's own provenance is unknown |
| 2 | `unverified` | Checksums valid, but no attestation files exist (the bundle was created without `--attest`) |
| 1 | `unknown` | Checksums are missing or invalid |

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

1. **Checksums**: every content file is hashed and matched against `checksums.txt`.
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

A local PEM key is read from disk only. Note, however, that resolving the key
is only part of verification: by default the bundle's Rekor transparency-log
entry is also checked (see the next section), so a PEM key makes verification
fully offline only when the Sigstore trusted-root cache is already warm.

## Offline and Air-Gapped Considerations

`aicr verify` is offline by default and does not call out to Sigstore at verify
time; the Rekor inclusion proof is embedded in the bundle, so no live Rekor
call is made. The check does, however, need the Sigstore *trusted root*, which
is loaded from the local cache at `~/.sigstore/root/` when present and otherwise
fetched over the network. Pre-populate that cache before going offline:

```shell
# Warm the Sigstore trusted-root cache (contacts the TUF CDN once).
aicr trust update
```

Once the cache is warm, KMS-signed bundles verified with a local PEM key
(above) verify with no further network access.

Two scope limits to be aware of, both reflected in the current CLI reference:

- **Fully transparency-log-free verification**: dropping the Rekor
  transparency-log check entirely, for true air-gapped use, is **not yet
  supported**. It is tracked in
  [#1154](https://github.com/NVIDIA/aicr/issues/1154).
- **Private Sigstore verification**: `aicr bundle --attest` can redirect
  *signing* to a private Fulcio/Rekor with `--fulcio-url` / `--rekor-url`, but
  `aicr verify` does **not yet** support a custom Sigstore trust root, so
  bundles signed against private Sigstore infrastructure **cannot be verified
  with `aicr verify` today**. Public Sigstore is currently the only supported
  verification root. Verifier support is tracked in
  [#1149](https://github.com/NVIDIA/aicr/issues/1149) and
  [#1153](https://github.com/NVIDIA/aicr/issues/1153).

## Recipe Evidence Verification

Recipe-evidence bundles, produced by `aicr validate --emit-attestation`, are
verified with `aicr evidence verify`. When the bundle carries a signature, the
command verifies it against the Sigstore trusted root, recomputes every file's
sha256 against `manifest.json`, and surfaces the predicate's fingerprint, phase
counts, and BOM info.

The positional argument is auto-detected. Prefer the committed **pointer file**,
which pins the bundle by digest:

```shell
# Verify the pointer a contributor committed alongside a recipe change (preferred).
aicr evidence verify recipes/evidence/h100-eks-ubuntu-training.yaml

# Verify a pushed OCI bundle directly, pinned by digest.
aicr evidence verify ghcr.io/myorg/aicr-evidence@sha256:abc...

# Verify a local bundle directory (self-debug before push).
aicr evidence verify ./out/summary-bundle
```

To require a specific signer, pin the OIDC issuer and identity:

```shell
aicr evidence verify recipes/evidence/<recipe>.yaml \
  --expected-issuer https://token.actions.githubusercontent.com \
  --expected-identity-regexp '^https://github\.com/myorg/.*$'
```

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
aicr evidence verify recipes/evidence/<recipe>.yaml -o result.json -t json
```

`aicr evidence verify` exits `0` when every check passes and `2` when the
bundle is invalid or recorded validator results show failures. The JSON
output's `exit` field further distinguishes recorded phase failures (`1`) from
an invalid bundle (`2`), so a shell consumer can branch on it:

```shell
case "$(aicr evidence verify recipes/evidence/<recipe>.yaml --format json | jq '.exit')" in
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

**Private-Sigstore-signed bundle won't verify.** This is expected. `aicr
verify` does not yet support a custom Sigstore trust root. See
[Offline and Air-Gapped Considerations](#offline-and-air-gapped-considerations).
